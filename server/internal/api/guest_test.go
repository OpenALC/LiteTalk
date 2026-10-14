package api

import (
	"net/http"
	"regexp"
	"testing"
)

// TestGuestLoginSimple simple 模式：一键游客 → 201 + is_guest + 游客-XXXX 昵称。
func TestGuestLoginSimple(t *testing.T) {
	srv, _ := newTestServerMode(t, AuthModeSimple)
	h := srv.Handler()

	resp := postJSON(t, h, "/api/v1/auth/guest", "", nil, http.StatusCreated)
	if resp["access_token"] == "" || resp["refresh_token"] == "" {
		t.Fatal("游客登录未返回令牌对")
	}
	user, ok := resp["user"].(map[string]any)
	if !ok {
		t.Fatalf("user 字段缺失或类型错误: %v", resp["user"])
	}
	if user["is_guest"] != true {
		t.Fatalf("is_guest = %v, want true", user["is_guest"])
	}
	if !regexp.MustCompile(`^游客-\d{4}$`).MatchString(user["nickname"].(string)) {
		t.Fatalf("nickname = %v, want 游客-XXXX", user["nickname"])
	}

	// 探测账号可立即访问鉴权接口
	me := getJSON(t, h, "/api/v1/users/me", resp["access_token"].(string), http.StatusOK).(map[string]any)
	if me["is_guest"] != true {
		t.Fatalf("users/me is_guest = %v, want true", me["is_guest"])
	}

	// 游客无法被密码登录（空哈希）：即使拿到占位邮箱也过不了。
	email := user["email"].(string)
	postJSON(t, h, "/api/v1/auth/login", "", map[string]string{
		"email": email, "password": "",
	}, http.StatusUnauthorized)
	postJSON(t, h, "/api/v1/auth/login", "", map[string]string{
		"email": email, "password": "whatever123",
	}, http.StatusUnauthorized)
}

// TestGuestLoginFull full 模式不开放游客。
func TestGuestLoginFull(t *testing.T) {
	srv, _ := newTestServerMode(t, AuthModeFull)
	h := srv.Handler()

	resp := postJSON(t, h, "/api/v1/auth/guest", "", nil, http.StatusForbidden)
	if resp["code"] != "guest_disabled" {
		t.Fatalf("code = %v, want guest_disabled", resp["code"])
	}
}

// TestGuestRateLimit 游客签发有 IP 限流（与登录同档 5 次/分钟）。
func TestGuestRateLimit(t *testing.T) {
	srv, _ := newTestServerMode(t, AuthModeSimple)
	h := srv.Handler()

	for i := 0; i < 5; i++ {
		postJSON(t, h, "/api/v1/auth/guest", "", nil, http.StatusCreated)
	}
	resp := postJSON(t, h, "/api/v1/auth/guest", "", nil, http.StatusTooManyRequests)
	if resp["code"] != "rate_limited" {
		t.Fatalf("code = %v, want rate_limited", resp["code"])
	}
}
