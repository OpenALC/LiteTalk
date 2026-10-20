package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/OpenALC/LiteTalk/server/internal/auth"
)

// doJSON 发任意方法请求（测试辅助：postJSON 只覆盖 POST，PATCH/DELETE 复用）。
func doJSON(t *testing.T, h http.Handler, method, path, token string, body any, wantStatus int) map[string]any {
	t.Helper()
	var buf []byte
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		buf = b
	}
	req := httptest.NewRequest(method, path, bytesReader(buf))
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set(clientVersionHeader, testClientVersion)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != wantStatus {
		t.Fatalf("%s %s: 期望 %d 实际 %d body=%s", method, path, wantStatus, rec.Code, rec.Body.String())
	}
	out := map[string]any{}
	if rec.Body.Len() > 0 {
		_ = json.Unmarshal(rec.Body.Bytes(), &out)
	}
	return out
}

// bytesReader 把 []byte 包装成 io.Reader（空 body 时返回空 reader）。
func bytesReader(b []byte) *bytesBuf { return &bytesBuf{b: b} }

type bytesBuf struct {
	b []byte
	i int
}

func (r *bytesBuf) Read(p []byte) (int, error) {
	if r.i >= len(r.b) {
		return 0, errEOF
	}
	n := copy(p, r.b[r.i:])
	r.i += n
	return n, nil
}

var errEOF = func() error { return &eofErr{} }()

type eofErr struct{}

func (*eofErr) Error() string { return "EOF" }

// registerSimple simple 模式注册并返回令牌对 + user id（Snowflake 后 wire 为字符串）。
func registerSimple(t *testing.T, h http.Handler, email, device string) (access, refresh string, uid int64) {
	t.Helper()
	resp := postJSON(t, h, "/api/v1/auth/register", "", map[string]string{
		"email": email, "password": "password123", "nickname": "昵称" + email[:4], "device": device,
	}, http.StatusCreated)
	acc, _ := resp["access_token"].(string)
	ref, _ := resp["refresh_token"].(string)
	if user, ok := resp["user"].(map[string]any); ok {
		uid = asID(user["id"])
	}
	return acc, ref, uid
}

// parseSid 从 access token 解析 sid 声明（测试断言用）。
func parseSid(t *testing.T, jwtSigner *auth.JWT, token string) (sid, dev string) {
	t.Helper()
	claims, err := jwtSigner.ParseAccess(token)
	if err != nil {
		t.Fatalf("解析 access token: %v", err)
	}
	return claims.Sid, claims.Dev
}

// TestSessionLoginThenMe 登录后 sid 非空、/me 200（PR #2 审查 P0-1 的最小复现）。
func TestSessionLoginThenMe(t *testing.T) {
	srv, _ := newTestServerMode(t, AuthModeSimple)
	access, _, _ := registerSimple(t, srv.Handler(), "s1@ex.com", "desktop")

	sid, dev := parseSid(t, srv.jwt, access)
	if sid == "" {
		t.Fatal("登录后 access token 的 sid 不应为空（issueTokens 遮蔽 bug 回归）")
	}
	if dev != "desktop" {
		t.Fatalf("dev 应为 desktop，实际 %q", dev)
	}
	getJSON(t, srv.Handler(), "/api/v1/users/me", access, http.StatusOK)
}

// loginSimple simple 模式登录（同账号多次登录 = 换 sid）。
func loginSimple(t *testing.T, h http.Handler, email, device string) (access, refresh string) {
	t.Helper()
	resp := postJSON(t, h, "/api/v1/auth/login", "", map[string]string{
		"email": email, "password": "password123", "device": device,
	}, http.StatusOK)
	acc, _ := resp["access_token"].(string)
	ref, _ := resp["refresh_token"].(string)
	return acc, ref
}

// TestSessionSameDeviceKicksOld 同类设备二次登录：旧令牌 401 session_replaced，新令牌 200。
func TestSessionSameDeviceKicksOld(t *testing.T) {
	srv, _ := newTestServerMode(t, AuthModeSimple)
	registerSimple(t, srv.Handler(), "s2@ex.com", "desktop")

	// 同账号同设备登录两次（第二次换 sid 顶掉第一次）。
	first, _ := loginSimple(t, srv.Handler(), "s2@ex.com", "desktop")
	second, _ := loginSimple(t, srv.Handler(), "s2@ex.com", "desktop")

	// 旧令牌 401 + 响应头 session_replaced。
	req := httptest.NewRequest(http.MethodGet, "/api/v1/users/me", nil)
	req.Header.Set("Authorization", "Bearer "+first)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("同设备再登录后旧令牌应 401，实际 %d body=%s", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("X-LiteTalk-Auth"); got != "session_replaced" {
		t.Fatalf("响应头应为 session_replaced，实际 %q", got)
	}
	// 新令牌正常。
	getJSON(t, srv.Handler(), "/api/v1/users/me", second, http.StatusOK)
}

// TestSessionDifferentDevicesCoexist 不同设备各自登录互不影响。
func TestSessionDifferentDevicesCoexist(t *testing.T) {
	srv, _ := newTestServerMode(t, AuthModeSimple)
	// 同账号 desktop + mobile 各登录一次（第二次登录同设备才顶号，异设备共存）。
	dAcc, _, _ := registerSimple(t, srv.Handler(), "s3d@ex.com", "desktop")
	mAcc, _, _ := registerSimple(t, srv.Handler(), "s3m@ex.com", "mobile")
	getJSON(t, srv.Handler(), "/api/v1/users/me", dAcc, http.StatusOK)
	getJSON(t, srv.Handler(), "/api/v1/users/me", mAcc, http.StatusOK)
}

// TestSessionRefreshRotateAndReplay 刷新后新令牌可用；旧刷新令牌重放 401 session_replayed。
func TestSessionRefreshRotateAndReplay(t *testing.T) {
	srv, _ := newTestServerMode(t, AuthModeSimple)
	_, oldRefresh, _ := registerSimple(t, srv.Handler(), "s4@ex.com", "desktop")

	resp := postJSON(t, srv.Handler(), "/api/v1/auth/refresh", "", map[string]string{
		"refresh_token": oldRefresh,
	}, http.StatusOK)
	newAccess, _ := resp["access_token"].(string)
	newRefresh, _ := resp["refresh_token"].(string)
	if newAccess == "" || newRefresh == "" {
		t.Fatal("刷新应返回新令牌对")
	}
	getJSON(t, srv.Handler(), "/api/v1/users/me", newAccess, http.StatusOK)

	// 旧刷新令牌重放 → session_replayed。
	req := httptest.NewRequest(http.MethodPost, "/api/v1/auth/refresh", bytesReader([]byte(`{"refresh_token":"`+oldRefresh+`"}`)))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(clientVersionHeader, testClientVersion)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("重放应 401，实际 %d", rec.Code)
	}
	if got := rec.Header().Get("X-LiteTalk-Auth"); got != "session_replayed" {
		t.Fatalf("重放应标记 session_replayed，实际 %q body=%s", got, rec.Body.String())
	}
}

// TestSessionLogoutRevokes 登出后 access 与 refresh 均失效。
func TestSessionLogoutRevokes(t *testing.T) {
	srv, _ := newTestServerMode(t, AuthModeSimple)
	access, refresh, _ := registerSimple(t, srv.Handler(), "s5@ex.com", "desktop")
	postJSON(t, srv.Handler(), "/api/v1/auth/logout", "", map[string]string{
		"refresh_token": refresh,
	}, http.StatusOK)

	getJSON(t, srv.Handler(), "/api/v1/users/me", access, http.StatusUnauthorized)
	postJSON(t, srv.Handler(), "/api/v1/auth/refresh", "", map[string]string{
		"refresh_token": refresh,
	}, http.StatusUnauthorized)
}

// TestSessionMobileRefreshLogout mobile 设备全链路：注册→refresh→logout 均命中 mobile 会话
// （PR #2 审查 P0-2：refresh/logout 曾写死 desktop，mobile 刷新必 401）。
func TestSessionMobileRefreshLogout(t *testing.T) {
	srv, _ := newTestServerMode(t, AuthModeSimple)
	access, refresh, _ := registerSimple(t, srv.Handler(), "s6@ex.com", "mobile")
	if _, dev := parseSid(t, srv.jwt, access); dev != "mobile" {
		t.Fatalf("dev 应为 mobile，实际 %q", dev)
	}

	// mobile 刷新必须成功（写死 desktop 时必 401）。
	resp := postJSON(t, srv.Handler(), "/api/v1/auth/refresh", "", map[string]string{
		"refresh_token": refresh,
	}, http.StatusOK)
	newAccess, _ := resp["access_token"].(string)
	if _, dev := parseSid(t, srv.jwt, newAccess); dev != "mobile" {
		t.Fatalf("刷新后 dev 应保持 mobile，实际 %q", dev)
	}
	newRefresh, _ := resp["refresh_token"].(string)
	getJSON(t, srv.Handler(), "/api/v1/users/me", newAccess, http.StatusOK)

	// mobile 登出吊销 mobile 自己的会话。
	postJSON(t, srv.Handler(), "/api/v1/auth/logout", "", map[string]string{
		"refresh_token": newRefresh,
	}, http.StatusOK)
	getJSON(t, srv.Handler(), "/api/v1/users/me", newAccess, http.StatusUnauthorized)
}

// TestSessionNoSessionDegrade sess==nil 时跳过会话校验（降级路径，生产不可达）。
func TestSessionNoSessionDegrade(t *testing.T) {
	srv, _ := newTestServerNoSessionMode(t)
	// 无 Session：issueTokens 返回 500（不签发无 sid 令牌）。
	postJSON(t, srv.Handler(), "/api/v1/auth/register", "", map[string]string{
		"email": "s7@ex.com", "password": "password123", "nickname": "降级路径",
	}, http.StatusInternalServerError)
}
