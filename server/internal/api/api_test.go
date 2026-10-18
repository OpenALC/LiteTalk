package api

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"

	"github.com/OpenALC/LiteTalk/server/internal/auth"
	"github.com/OpenALC/LiteTalk/server/internal/store"
)

// newTestServer 构造内存态 API Server（无 MySQL/Redis 依赖）。
// 注入 SessionIndex：登录/刷新/鉴权中间件的会话校验是 v1.1.0 的核心路径，
// 不注入等于把 PR #2 审查 P0-1（sid 恒空）那类 bug 全部放行。
func newTestServer(t *testing.T) (*Server, *store.Mem) {
	t.Helper()
	return newTestServerMode(t, AuthModeFull)
}

// newTestServerNoSession 无 SessionIndex 的 simple 模式变体：仅用于验证 sess==nil 时
// 会话校验整段跳过的降级路径（生产不会出现，main.go 恒注入）。
func newTestServerNoSessionMode(t *testing.T) (*Server, *store.Mem) {
	t.Helper()
	mem := store.NewMem()
	return New(Config{
		Store:    mem,
		JWT:      auth.NewJWT("test-secret"),
		Refresh:  auth.NewRefreshStore(mem),
		Codes:    auth.NewEmailCodes(mem),
		Limits:   auth.NewRateLimit(mem),
		AuthMode: AuthModeSimple,
		Presence: mem,
	}), mem
}

// postJSON 发请求并解码响应。
func postJSON(t *testing.T, h http.Handler, path, token string, body any, wantStatus int) map[string]any {
	t.Helper()
	var buf bytes.Buffer
	if body != nil {
		if err := json.NewEncoder(&buf).Encode(body); err != nil {
			t.Fatal(err)
		}
	}
	req := httptest.NewRequest(http.MethodPost, path, &buf)
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != wantStatus {
		t.Fatalf("%s: 期望 %d 实际 %d body=%s", path, wantStatus, rec.Code, rec.Body.String())
	}
	out := map[string]any{}
	if rec.Body.Len() > 0 {
		_ = json.Unmarshal(rec.Body.Bytes(), &out)
	}
	return out
}

func getJSON(t *testing.T, h http.Handler, path, token string, wantStatus int) any {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != wantStatus {
		t.Fatalf("%s: 期望 %d 实际 %d body=%s", path, wantStatus, rec.Code, rec.Body.String())
	}
	var out any
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	return out
}

// patchJSON / delJSON PATCH 与 DELETE 变体（v0.1.2 好友备注/分组等管理端点测试用）。
func patchJSON(t *testing.T, h http.Handler, path, token string, body any, wantStatus int) map[string]any {
	t.Helper()
	var buf bytes.Buffer
	if body != nil {
		if err := json.NewEncoder(&buf).Encode(body); err != nil {
			t.Fatal(err)
		}
	}
	req := httptest.NewRequest(http.MethodPatch, path, &buf)
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != wantStatus {
		t.Fatalf("%s: 期望 %d 实际 %d body=%s", path, wantStatus, rec.Code, rec.Body.String())
	}
	out := map[string]any{}
	if rec.Body.Len() > 0 {
		_ = json.Unmarshal(rec.Body.Bytes(), &out)
	}
	return out
}

func delJSON(t *testing.T, h http.Handler, path, token string, wantStatus int) map[string]any {
	t.Helper()
	req := httptest.NewRequest(http.MethodDelete, path, nil)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != wantStatus {
		t.Fatalf("%s: 期望 %d 实际 %d body=%s", path, wantStatus, rec.Code, rec.Body.String())
	}
	out := map[string]any{}
	if rec.Body.Len() > 0 {
		_ = json.Unmarshal(rec.Body.Bytes(), &out)
	}
	return out
}

// asID 从解码结果取 ID：Snowflake 后 wire 为字符串（#31），兼容旧 JSON 数字。
// 测试解码进 map[string]any，大数字走 float64 会掉精度——只认字符串/整数形态。
func asID(v any) int64 {
	switch x := v.(type) {
	case string:
		n, err := strconv.ParseInt(x, 10, 64)
		if err != nil {
			return 0
		}
		return n
	case json.Number:
		n, _ := x.Int64()
		return n
	case float64:
		return int64(x)
	}
	return 0
}

// registerUser 注册一个测试用户并返回 (access, refresh)。
func registerUser(t *testing.T, h http.Handler, mem *store.Mem, email, nickname, password string) (string, string) {
	t.Helper()
	// 发验证码（控制台 mailer，验证码经 KV 直读）。
	postJSON(t, h, "/api/v1/auth/email-code", "", map[string]string{
		"email": email, "purpose": "register",
	}, http.StatusOK)
	code, err := mem.Get(context.Background(), "code:register:"+email)
	if err != nil {
		t.Fatalf("取验证码失败: %v", err)
	}
	resp := postJSON(t, h, "/api/v1/auth/register", "", map[string]string{
		"nickname": nickname, "email": email, "password": password, "code": code,
	}, http.StatusCreated)
	return resp["access_token"].(string), resp["refresh_token"].(string)
}

// TestAuthFlow 注册→登录→refresh 旋转→重置密码 全链路。
func TestAuthFlow(t *testing.T) {
	srv, mem := newTestServer(t)
	h := srv.Handler()

	access, refresh := registerUser(t, h, mem, "a@ex.com", "alice", "password123")
	if access == "" || refresh == "" {
		t.Fatal("令牌为空")
	}

	// access 令牌可用。
	getJSON(t, h, "/api/v1/users/me", access, http.StatusOK)

	// 登录。
	login := postJSON(t, h, "/api/v1/auth/login", "", map[string]string{
		"email": "a@ex.com", "password": "password123",
	}, http.StatusOK)
	refresh2 := login["refresh_token"].(string)

	// refresh 旋转：旧令牌吊销。
	rot := postJSON(t, h, "/api/v1/auth/refresh", "", map[string]string{
		"refresh_token": refresh2,
	}, http.StatusOK)
	_ = rot
	postJSON(t, h, "/api/v1/auth/refresh", "", map[string]string{
		"refresh_token": refresh2,
	}, http.StatusUnauthorized)

	// 重置密码。
	postJSON(t, h, "/api/v1/auth/email-code", "", map[string]string{
		"email": "a@ex.com", "purpose": "reset",
	}, http.StatusOK)
	code, _ := mem.Get(context.Background(), "code:reset:a@ex.com")
	postJSON(t, h, "/api/v1/auth/password/reset", "", map[string]string{
		"email": "a@ex.com", "code": code, "password": "newpassword456",
	}, http.StatusOK)
	// 旧密码失效。
	postJSON(t, h, "/api/v1/auth/login", "", map[string]string{
		"email": "a@ex.com", "password": "password123",
	}, http.StatusUnauthorized)
	postJSON(t, h, "/api/v1/auth/login", "", map[string]string{
		"email": "a@ex.com", "password": "newpassword456",
	}, http.StatusOK)
}

// TestEmailCodeRateLimit 验证码 60s 间隔 + 每日 5 次。
func TestEmailCodeRateLimit(t *testing.T) {
	srv, _ := newTestServer(t)
	h := srv.Handler()
	postJSON(t, h, "/api/v1/auth/email-code", "", map[string]string{
		"email": "r@ex.com", "purpose": "register",
	}, http.StatusOK)
	postJSON(t, h, "/api/v1/auth/email-code", "", map[string]string{
		"email": "r@ex.com", "purpose": "register",
	}, http.StatusTooManyRequests)
}

// TestFriendsFlow 加好友→接受→列表→删除。
func TestFriendsFlow(t *testing.T) {
	srv, mem := newTestServer(t)
	h := srv.Handler()
	aAccess, _ := registerUser(t, h, mem, "fa@ex.com", "friendA", "password123")
	bAccess, _ := registerUser(t, h, mem, "fb@ex.com", "friendB", "password123")

	// A 按 昵称#tag 请求 B。
	var me map[string]any
	req := httptest.NewRequest(http.MethodGet, "/api/v1/users/me", nil)
	req.Header.Set("Authorization", "Bearer "+bAccess)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	_ = json.Unmarshal(rec.Body.Bytes(), &me)
	bTag := me["tag"].(string)

	fr := postJSON(t, h, "/api/v1/friends/requests", aAccess, map[string]string{
		"nickname": "friendB#" + bTag,
	}, http.StatusCreated)

	// B 看到 incoming 请求并接受。
	list := getJSON(t, h, "/api/v1/friends", bAccess, http.StatusOK).([]any)
	if len(list) != 1 {
		t.Fatalf("B 好友列表应有 1 条，实际 %d", len(list))
	}
	fid := asID(fr["id"])
	postJSON(t, h, fmt.Sprintf("/api/v1/friends/requests/%d/accept", fid), bAccess, nil, http.StatusOK)

	// 双方列表都是 accepted。
	for _, tok := range []string{aAccess, bAccess} {
		items := getJSON(t, h, "/api/v1/friends", tok, http.StatusOK).([]any)
		if len(items) != 1 {
			t.Fatalf("好友列表应有 1 条，实际 %d", len(items))
		}
		item := items[0].(map[string]any)
		if item["status"] != "accepted" {
			t.Fatalf("状态应为 accepted，实际 %v", item["status"])
		}
	}

	// 删除。
	req2 := httptest.NewRequest(http.MethodDelete, fmt.Sprintf("/api/v1/friends/%d", fid), nil)
	req2.Header.Set("Authorization", "Bearer "+aAccess)
	rec2 := httptest.NewRecorder()
	h.ServeHTTP(rec2, req2)
	if rec2.Code != http.StatusOK {
		t.Fatalf("删除好友期望 200 实际 %d", rec2.Code)
	}
}

// TestServerChannelMessageFlow 建服→频道→邀请→入服→发消息→分页。
func TestServerChannelMessageFlow(t *testing.T) {
	srv, mem := newTestServer(t)
	h := srv.Handler()
	aAccess, _ := registerUser(t, h, mem, "sa@ex.com", "srvA", "password123")
	bAccess, _ := registerUser(t, h, mem, "sb@ex.com", "srvB", "password123")

	// 建服。
	serverResp := postJSON(t, h, "/api/v1/servers", aAccess, map[string]string{
		"name": "测试服务器", "icon_name": "music", "icon_color": "#4FA4F3",
	}, http.StatusCreated)
	serverID := asID(serverResp["id"])

	// 建频道。
	chResp := postJSON(t, h, fmt.Sprintf("/api/v1/servers/%d/channels", serverID), aAccess,
		map[string]string{"name": "闲聊广场", "type": "text"}, http.StatusCreated)
	chID := asID(chResp["id"])

	// 邀请码。
	invResp := postJSON(t, h, fmt.Sprintf("/api/v1/servers/%d/invites", serverID), aAccess,
		map[string]any{"max_uses": 5}, http.StatusCreated)
	code := invResp["code"].(string)

	// B 入服。
	postJSON(t, h, "/api/v1/servers/join", bAccess, map[string]string{"code": code}, http.StatusOK)

	// B 发消息。
	msgResp := postJSON(t, h, fmt.Sprintf("/api/v1/channels/%d/messages", chID), bAccess,
		map[string]string{"content": "你好世界"}, http.StatusCreated)
	_ = msgResp

	// A 拉取消息。
	msgs := getJSON(t, h, fmt.Sprintf("/api/v1/channels/%d/messages", chID), aAccess, http.StatusOK).([]any)
	if len(msgs) != 1 {
		t.Fatalf("消息数应为 1，实际 %d", len(msgs))
	}

	// 超长消息拒绝。
	postJSON(t, h, fmt.Sprintf("/api/v1/channels/%d/messages", chID), bAccess,
		map[string]string{"content": string(make([]byte, 2001))}, http.StatusBadRequest)

	// 非成员不能发消息。
	cAccess, _ := registerUser(t, h, mem, "sc@ex.com", "srvC", "password123")
	postJSON(t, h, fmt.Sprintf("/api/v1/channels/%d/messages", chID), cAccess,
		map[string]string{"content": "闯入者"}, http.StatusForbidden)
}

// TestUnauthorized 未携带令牌 → 401。
func TestUnauthorized(t *testing.T) {
	srv, _ := newTestServer(t)
	h := srv.Handler()
	getJSON(t, h, "/api/v1/users/me", "", http.StatusUnauthorized)
	getJSON(t, h, "/api/v1/users/me", "bad-token", http.StatusUnauthorized)
}

// TestLoginRateLimit 登录 5 次/min/IP。
func TestLoginRateLimit(t *testing.T) {
	srv, mem := newTestServer(t)
	h := srv.Handler()
	registerUser(t, h, mem, "lr@ex.com", "limiter", "password123")
	for i := 0; i < 5; i++ {
		postJSON(t, h, "/api/v1/auth/login", "", map[string]string{
			"email": "lr@ex.com", "password": "wrong",
		}, http.StatusUnauthorized)
	}
	postJSON(t, h, "/api/v1/auth/login", "", map[string]string{
		"email": "lr@ex.com", "password": "wrong",
	}, http.StatusTooManyRequests)
}
