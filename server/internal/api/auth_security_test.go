package api

import (
	"net/http"
	"strings"
	"testing"
)

// TestLogoutRequiresValidToken 伪造 "<uid>.x" 不能吊销他人 refresh（P1）：
// logout 先校验哈希，不匹配 401 且原令牌仍可用；真令牌 logout 后 refresh 立即失效。
func TestLogoutRequiresValidToken(t *testing.T) {
	srv, mem := newTestServer(t)
	h := srv.Handler()
	_, refresh := registerUser(t, h, mem, "lo@ex.com", "logoutA", "password123")

	// 伪造令牌：401，且真令牌未被吊销。
	uid, _, _ := strings.Cut(refresh, ".")
	forged := postJSON(t, h, "/api/v1/auth/logout", "", map[string]string{
		"refresh_token": uid + ".forged",
	}, http.StatusUnauthorized)
	if forged["code"] != "unauthorized" {
		t.Fatalf("code = %v, want unauthorized", forged["code"])
	}
	// 注意：refresh 会旋转令牌，此处取旋转后的新令牌继续后续断言。
	rot := postJSON(t, h, "/api/v1/auth/refresh", "", map[string]string{
		"refresh_token": refresh,
	}, http.StatusOK)
	refresh = rot["refresh_token"].(string)

	// 非法格式：401。
	postJSON(t, h, "/api/v1/auth/logout", "", map[string]string{
		"refresh_token": "not-a-token",
	}, http.StatusUnauthorized)

	// 真令牌 logout：200，之后 refresh 失效。
	postJSON(t, h, "/api/v1/auth/logout", "", map[string]string{
		"refresh_token": refresh,
	}, http.StatusOK)
	postJSON(t, h, "/api/v1/auth/refresh", "", map[string]string{
		"refresh_token": refresh,
	}, http.StatusUnauthorized)
}
