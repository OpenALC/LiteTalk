package api

// tokens_test.go 个人访问令牌 REST 全场景（#54 必覆盖清单）：
//   - 创建响应含一次性明文，列表只含元数据（无 token 字段、无 secret_hash）；
//   - 吊销立即 401（无缓存）；
//   - 过期 401；
//   - scope 不足 403，且 scope 只是上限（角色有的权限 scope 没有也 403）；
//   - 跨用户访问他人令牌 404（与不存在不可区分）；
//   - last_used_at 节流（60s 内不重复写库）；
//   - 创建校验（空/未知/重复 scope、空名、过期时刻）与频控。

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/OpenALC/LiteTalk/server/internal/store"
	"github.com/OpenALC/LiteTalk/server/internal/token"
)

// TestTokenLifecycle 创建→明文仅一次→列表无明文→吊销立即 401→跨用户 404。
func TestTokenLifecycle(t *testing.T) {
	srv, mem := newTestServer(t)
	h := srv.Handler()
	aAccess, _ := registerUser(t, h, mem, "ta@ex.com", "tokA", "password123")
	bAccess, _ := registerUser(t, h, mem, "tb@ex.com", "tokB", "password123")

	// 创建：201，响应带一次性明文。
	created := postJSON(t, h, "/api/v1/tokens", aAccess, map[string]any{
		"name":   "CI 机器人",
		"scopes": []string{"messages.read", "messages.write"},
	}, http.StatusCreated)
	plain, _ := created["token"].(string)
	if !strings.HasPrefix(plain, "ltk_pat_") {
		t.Fatalf("应返回 ltk_pat_ 明文，实际 %v", created["token"])
	}
	if created["secret_hash"] != nil || len(created) > 9 { // 9 字段 = CreatedToken 全集，多一个就是泄露
		t.Fatalf("响应不应含 secret_hash：keys=%v", created)
	}
	prefix, _ := created["prefix"].(string)
	if len(prefix) != 8 || prefix != strings.ToLower(prefix) {
		t.Fatalf("prefix 应为 8 位小写 hex，实际 %q", prefix)
	}
	if !strings.Contains(plain, prefix) {
		t.Fatalf("明文应含 prefix %q：%q", prefix, plain)
	}
	tokenID := asID(created["id"])

	// 明文可直接调 REST（替代 JWT）——令牌管理面例外（P1-1）：列表仅限 JWT，
	// 令牌主体 403（token_management_forbidden）。
	if resp := getJSON(t, h, "/api/v1/tokens", plain, http.StatusForbidden).(map[string]any); resp["code"] != "token_management_forbidden" {
		t.Fatalf("令牌主体列列表应 403 token_management_forbidden，实际 %v", resp)
	}
	list := getJSON(t, h, "/api/v1/tokens", aAccess, http.StatusOK).([]any)
	if len(list) != 1 {
		t.Fatalf("本人列表应 1 条，实际 %d", len(list))
	}
	item := list[0].(map[string]any)
	if item["token"] != nil || item["secret_hash"] != nil {
		t.Fatalf("列表永不回传明文/哈希：keys=%v", item)
	}
	if item["prefix"] != prefix || item["name"] != "CI 机器人" {
		t.Fatalf("元数据不符：%v", item)
	}
	if len(item["scopes"].([]any)) != 2 {
		t.Fatalf("scopes 应 2 项：%v", item["scopes"])
	}

	// 跨用户：B 的列表为空；B 删 A 的令牌 → 404（与不存在不可区分）。
	if got := getJSON(t, h, "/api/v1/tokens", bAccess, http.StatusOK).([]any); len(got) != 0 {
		t.Fatalf("B 列表应为空，实际 %d", len(got))
	}
	delResp := delJSON(t, h, fmt.Sprintf("/api/v1/tokens/%d", tokenID), bAccess, http.StatusNotFound)
	if delResp["code"] != "not_found" {
		t.Fatalf("跨用户删除应 404 not_found，实际 %v", delResp)
	}

	// 吊销（属主用 JWT 调）→ 明文下一次请求立即 401。
	delJSON(t, h, fmt.Sprintf("/api/v1/tokens/%d", tokenID), aAccess, http.StatusOK)
	req401 := getJSON(t, h, "/api/v1/tokens", plain, http.StatusUnauthorized)
	if req401.(map[string]any)["code"] != "unauthorized" {
		t.Fatalf("吊销后应 401 unauthorized，实际 %v", req401)
	}
	// 列表里 revoked_at 已置。
	items := getJSON(t, h, "/api/v1/tokens", aAccess, http.StatusOK).([]any)
	if items[0].(map[string]any)["revoked_at"] == nil {
		t.Fatal("吊销后 revoked_at 应非空")
	}
	// 重复吊销幂等 200。
	delJSON(t, h, fmt.Sprintf("/api/v1/tokens/%d", tokenID), aAccess, http.StatusOK)
}

// TestTokenExpiredDirect 过期令牌 401（创建端拒绝过去时刻，这里直接落一行过期数据
// 走中间件校验路径）。
func TestTokenExpiredDirect(t *testing.T) {
	srv, mem := newTestServer(t)
	h := srv.Handler()
	registerUser(t, h, mem, "te@ex.com", "tokExp", "password123")
	ctx := context.Background()

	u, err := mem.UserByEmail(ctx, "te@ex.com")
	if err != nil {
		t.Fatalf("查测试用户: %v", err)
	}
	uid := u.ID
	p, err := token.NewPlain(token.KindPAT)
	if err != nil {
		t.Fatal(err)
	}
	past := time.Now().Add(-time.Hour)
	if err := mem.CreateAPIToken(ctx, &store.APIToken{UserID: uid, Name: "过期",
		Prefix: p.Prefix, SecretHash: p.Hash, Kind: token.KindPAT,
		Scopes: "messages.read", ExpiresAt: &past}); err != nil {
		t.Fatal(err)
	}
	body := getJSON(t, h, "/api/v1/tokens", p.Token, http.StatusUnauthorized)
	if body.(map[string]any)["code"] != "unauthorized" {
		t.Fatalf("过期令牌应 401，实际 %v", body)
	}
}

// TestTokenScopeEnforcement scope 校验：不足 403；scope 是上限不是授予
// （角色是 owner 也救不回 scope 没给的权限）。
func TestTokenScopeEnforcement(t *testing.T) {
	srv, mem := newTestServer(t)
	h := srv.Handler()
	aAccess, _ := registerUser(t, h, mem, "ts@ex.com", "tokS", "password123")

	serverResp := postJSON(t, h, "/api/v1/servers", aAccess, map[string]string{
		"name": "令牌服", "icon_name": "music", "icon_color": "#4FA4F3",
	}, http.StatusCreated)
	serverID := asID(serverResp["id"])
	chResp := postJSON(t, h, fmt.Sprintf("/api/v1/servers/%d/channels", serverID), aAccess,
		map[string]string{"name": "文字", "type": "text"}, http.StatusCreated)
	chID := asID(chResp["id"])

	mk := func(scopes []string) string {
		resp := postJSON(t, h, "/api/v1/tokens", aAccess, map[string]any{
			"name": "scope 测试", "scopes": scopes,
		}, http.StatusCreated)
		return resp["token"].(string)
	}

	// messages.read：读 200，写 403（SendMessages 不在 scope 内）。
	ro := mk([]string{"messages.read"})
	getJSON(t, h, fmt.Sprintf("/api/v1/channels/%d/messages", chID), ro, http.StatusOK)
	resp := postJSON(t, h, fmt.Sprintf("/api/v1/channels/%d/messages", chID), ro,
		map[string]string{"content": "越权"}, http.StatusForbidden)
	if resp["code"] != "forbidden" {
		t.Fatalf("读-only 令牌发消息应 403，实际 %v", resp)
	}

	// messages.write：写 201。
	rw := mk([]string{"messages.write"})
	postJSON(t, h, fmt.Sprintf("/api/v1/channels/%d/messages", chID), rw,
		map[string]string{"content": "受权发言"}, http.StatusCreated)

	// 角色是 owner（有 ManageChannels），但 scope 只给 messages.read：
	// 删频道仍 403 —— scope 只是上限，绝不是授予。
	delResp := delJSON(t, h, fmt.Sprintf("/api/v1/channels/%d", chID), ro, http.StatusForbidden)
	if delResp["code"] != "forbidden" {
		t.Fatalf("scope 缺 guild.manage 时删频道应 403，实际 %v", delResp)
	}
}

// TestTokenCreateValidation 创建校验与频控。
func TestTokenCreateValidation(t *testing.T) {
	srv, mem := newTestServer(t)
	h := srv.Handler()
	aAccess, _ := registerUser(t, h, mem, "tv@ex.com", "tokV", "password123")

	// 空 scopes（空 scope = 无权限，不是全权限）。
	resp := postJSON(t, h, "/api/v1/tokens", aAccess, map[string]any{
		"name": "x", "scopes": []string{},
	}, http.StatusBadRequest)
	if resp["code"] != "bad_scopes" {
		t.Fatalf("空 scopes 应 bad_scopes，实际 %v", resp)
	}
	// 未知 scope。
	resp = postJSON(t, h, "/api/v1/tokens", aAccess, map[string]any{
		"name": "x", "scopes": []string{"messages.read", "root.all"},
	}, http.StatusBadRequest)
	if resp["code"] != "bad_scopes" {
		t.Fatalf("未知 scope 应 bad_scopes，实际 %v", resp)
	}
	// 重复 scope。
	resp = postJSON(t, h, "/api/v1/tokens", aAccess, map[string]any{
		"name": "x", "scopes": []string{"messages.read", "messages.read"},
	}, http.StatusBadRequest)
	if resp["code"] != "bad_scopes" {
		t.Fatalf("重复 scope 应 bad_scopes，实际 %v", resp)
	}
	// 空/超长名称。
	resp = postJSON(t, h, "/api/v1/tokens", aAccess, map[string]any{
		"name": "  ", "scopes": []string{"messages.read"},
	}, http.StatusBadRequest)
	if resp["code"] != "bad_name" {
		t.Fatalf("空名称应 bad_name，实际 %v", resp)
	}
	// 过去的 expires_at。
	resp = postJSON(t, h, "/api/v1/tokens", aAccess, map[string]any{
		"name": "x", "scopes": []string{"messages.read"},
		"expires_at": time.Now().Add(-time.Minute).UTC().Format(time.RFC3339),
	}, http.StatusBadRequest)
	if resp["code"] != "bad_expires" {
		t.Fatalf("过去时刻应 bad_expires，实际 %v", resp)
	}

	// 频控：tok:create 10 次/h。前面已用 5 次，再 5 次到限。
	for i := 0; i < 5; i++ {
		postJSON(t, h, "/api/v1/tokens", aAccess, map[string]any{
			"name": fmt.Sprintf("批量%d", i), "scopes": []string{"messages.read"},
		}, http.StatusCreated)
	}
	postJSON(t, h, "/api/v1/tokens", aAccess, map[string]any{
		"name": "超限", "scopes": []string{"messages.read"},
	}, http.StatusTooManyRequests)
}

// TestTokenLastUsedThrottleREST 60s 节流在 REST 路径同样生效：同一令牌两次请求
// 只落库一次 last_used_at。（last_used 在中间件校验路径更新，端点选令牌可用的
// /servers——令牌管理面已收窄为 JWT-only，P1-1。）
func TestTokenLastUsedThrottleREST(t *testing.T) {
	srv, mem := newTestServer(t)
	h := srv.Handler()
	aAccess, _ := registerUser(t, h, mem, "tl@ex.com", "tokL", "password123")

	created := postJSON(t, h, "/api/v1/tokens", aAccess, map[string]any{
		"name": "节流", "scopes": []string{"messages.read"},
	}, http.StatusCreated)
	plain := created["token"].(string)
	tokenID := asID(created["id"])
	ctx := context.Background()

	// 第一次使用：异步落库一次。
	getJSON(t, h, "/api/v1/servers", plain, http.StatusOK)
	srv.apiTokens.WaitIdle()
	first, err := mem.APITokenByID(ctx, tokenID)
	if err != nil {
		t.Fatal(err)
	}
	if first.LastUsedAt == nil {
		t.Fatal("首次使用后 last_used_at 应非空")
	}

	// 立即再用（60s 窗口内）：不重复写库。
	getJSON(t, h, "/api/v1/servers", plain, http.StatusOK)
	srv.apiTokens.WaitIdle()
	second, _ := mem.APITokenByID(ctx, tokenID)
	if !second.LastUsedAt.Equal(*first.LastUsedAt) {
		t.Fatalf("60s 窗口内不应重复落库：%v → %v", *first.LastUsedAt, *second.LastUsedAt)
	}
}

// TestTokenManagementJWTOnly 令牌管理面收窄（P1-1）：签发/列举仅限 JWT 主体，
// 只读 PAT 不可自签更宽 scope 的令牌（探针复现的提权链：messages.read →
// guild.manage）；吊销立即生效且签出的令牌不随原令牌失效——所以必须在签发口
// 堵死。任何主体可吊销自己（TokenID 匹配，泄露自毁通道）；PAT 管理本人其它
// 令牌同样拒绝；跨用户仍 404 不可区分。
func TestTokenManagementJWTOnly(t *testing.T) {
	srv, mem := newTestServer(t)
	h := srv.Handler()
	aAccess, _ := registerUser(t, h, mem, "tj@ex.com", "tokJ", "password123")
	bAccess, _ := registerUser(t, h, mem, "tj2@ex.com", "tokJ2", "password123")

	ro := postJSON(t, h, "/api/v1/tokens", aAccess, map[string]any{
		"name": "只读", "scopes": []string{"messages.read"},
	}, http.StatusCreated)
	roPlain, roID := ro["token"].(string), asID(ro["id"])
	other := postJSON(t, h, "/api/v1/tokens", aAccess, map[string]any{
		"name": "另一张", "scopes": []string{"messages.read"},
	}, http.StatusCreated)
	otherID := asID(other["id"])
	bTokenID := asID(postJSON(t, h, "/api/v1/tokens", bAccess, map[string]any{
		"name": "B 的", "scopes": []string{"messages.read"},
	}, http.StatusCreated)["id"])

	// 提权链：只读 PAT 调 POST /tokens 签 guild.manage → 403；列表同样 403。
	for _, body := range []any{
		map[string]any{"name": "提权", "scopes": []string{"guild.manage"}},
		map[string]any{"name": "再一张", "scopes": []string{"messages.read"}},
	} {
		resp := postJSON(t, h, "/api/v1/tokens", roPlain, body, http.StatusForbidden)
		if resp["code"] != "token_management_forbidden" {
			t.Fatalf("只读 PAT 签发应 403 token_management_forbidden，实际 %v", resp)
		}
	}
	if resp := getJSON(t, h, "/api/v1/tokens", roPlain, http.StatusForbidden).(map[string]any); resp["code"] != "token_management_forbidden" {
		t.Fatalf("只读 PAT 列举应 403，实际 %v", resp)
	}

	// bot 令牌同口径：CLI 直落一行（与 litetalkctl 同形状）。
	ctx := context.Background()
	u, err := mem.UserByEmail(ctx, "tj@ex.com")
	if err != nil {
		t.Fatal(err)
	}
	bp, err := token.NewPlain(token.KindBot)
	if err != nil {
		t.Fatal(err)
	}
	if err := mem.CreateAPIToken(ctx, &store.APIToken{UserID: u.ID, Name: "bot",
		Prefix: bp.Prefix, SecretHash: bp.Hash, Kind: token.KindBot, Scopes: "messages.read"}); err != nil {
		t.Fatal(err)
	}
	resp := postJSON(t, h, "/api/v1/tokens", bp.Token, map[string]any{
		"name": "bot 提权", "scopes": []string{"guild.manage"},
	}, http.StatusForbidden)
	if resp["code"] != "token_management_forbidden" {
		t.Fatalf("bot 签发应 403，实际 %v", resp)
	}
	getJSON(t, h, "/api/v1/tokens", bp.Token, http.StatusForbidden)

	// PAT 删本人其它令牌 → 403；删他人令牌 → 404（跨用户口径不变）。
	if resp := delJSON(t, h, fmt.Sprintf("/api/v1/tokens/%d", otherID), roPlain, http.StatusForbidden); resp["code"] != "token_management_forbidden" {
		t.Fatalf("PAT 删本人其它令牌应 403，实际 %v", resp)
	}
	delJSON(t, h, fmt.Sprintf("/api/v1/tokens/%d", bTokenID), roPlain, http.StatusNotFound)

	// 自毁通道：PAT 吊销自己（TokenID 匹配）→ 200，明文下一次请求立即 401。
	delJSON(t, h, fmt.Sprintf("/api/v1/tokens/%d", roID), roPlain, http.StatusOK)
	getJSON(t, h, "/api/v1/servers", roPlain, http.StatusUnauthorized)
	// JWT 管理本人令牌不受影响：已吊销的只读 + 另一张 + 直落的 bot 行都还在列表里。
	if list := getJSON(t, h, "/api/v1/tokens", aAccess, http.StatusOK).([]any); len(list) != 3 {
		t.Fatalf("JWT 列表应 3 条（含已吊销与 bot 行），实际 %d", len(list))
	}
}

// TestAdminReadTokenReadOnly admin.read 纯只读（P1-2）：映射归零后改不了服务器、
// 建不了出站 webhook；审计读取走双判定（持有 admin.read + 纯角色 ManageServer），
// 无 admin.read 的令牌（messages.read）与角色不够的主体都拿不到审计；成员列表
// 保持成员可读口径（admin.read 令牌作为成员照常可读）。
func TestAdminReadTokenReadOnly(t *testing.T) {
	srv, mem := newTestServer(t)
	h := srv.Handler()
	aAccess, _ := registerUser(t, h, mem, "tar@ex.com", "tokAR", "password123")
	serverID := asID(postJSON(t, h, "/api/v1/servers", aAccess, map[string]string{
		"name": "审计服", "icon_name": "music", "icon_color": "#4FA4F3",
	}, http.StatusCreated)["id"])

	mk := func(scopes []string) string {
		return postJSON(t, h, "/api/v1/tokens", aAccess, map[string]any{
			"name": "scope 测试", "scopes": scopes,
		}, http.StatusCreated)["token"].(string)
	}
	audit := mk([]string{"admin.read"})
	readOnly := mk([]string{"messages.read"})
	guild := mk([]string{"guild.manage"})

	// JWT 改名仍 200（owner），并落一条审计行供查询。
	patchJSON(t, h, fmt.Sprintf("/api/v1/servers/%d", serverID), aAccess,
		map[string]any{"name": "审计服二号"}, http.StatusOK)

	// 审计读取双判定：
	//   - admin.read + 角色够（owner）→ 200；
	//   - 无 admin.read（messages.read / guild.manage）→ 403（guild.manage 位级映射归零后不再顺带可读）。
	getJSON(t, h, fmt.Sprintf("/api/v1/servers/%d/audit-log", serverID), audit, http.StatusOK)
	if resp := getJSON(t, h, fmt.Sprintf("/api/v1/servers/%d/audit-log", serverID), readOnly, http.StatusForbidden).(map[string]any); resp["code"] != "forbidden" {
		t.Fatalf("messages.read 读审计应 403，实际 %v", resp)
	}
	if resp := getJSON(t, h, fmt.Sprintf("/api/v1/servers/%d/audit-log", serverID), guild, http.StatusForbidden).(map[string]any); resp["code"] != "forbidden" {
		t.Fatalf("guild.manage 读审计应 403（admin.read 归零后位级映射不再放行），实际 %v", resp)
	}

	// 写面：admin.read 令牌 PATCH 服务器、创建出站 webhook 一律 403（此前改名
	// 探针实测 200、webhook 可建——外泄私有频道的入口）。
	if resp := patchJSON(t, h, fmt.Sprintf("/api/v1/servers/%d", serverID), audit,
		map[string]any{"name": "越权改名"}, http.StatusForbidden); resp["code"] != "forbidden" {
		t.Fatalf("admin.read 改服务器名应 403，实际 %v", resp)
	}
	if resp := postJSON(t, h, fmt.Sprintf("/api/v1/servers/%d/webhooks", serverID), audit,
		map[string]any{"url": "https://hooks.example.com/x", "events": []string{"message.created"}}, http.StatusForbidden); resp["code"] != "forbidden" {
		t.Fatalf("admin.read 建出站 webhook 应 403，实际 %v", resp)
	}

	// 成员列表保持成员可读：admin.read / messages.read 令牌作为成员都 200。
	getJSON(t, h, fmt.Sprintf("/api/v1/servers/%d/members", serverID), audit, http.StatusOK)
	getJSON(t, h, fmt.Sprintf("/api/v1/servers/%d/members", serverID), readOnly, http.StatusOK)

}
