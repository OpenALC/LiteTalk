package api

import (
	"fmt"
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

// TestRegisterRateLimitIP 注册 10 次/h/IP：第 11 次 429（无论请求是否合法，先限流后校验）。
func TestRegisterRateLimitIP(t *testing.T) {
	srv, _ := newTestServer(t)
	h := srv.Handler()

	for i := 0; i < 10; i++ {
		postJSON(t, h, "/api/v1/auth/register", "", map[string]string{
			"nickname": "限流用户", "email": fmt.Sprintf("rl%d@ex.com", i),
			"password": "password123", "code": "",
		}, http.StatusBadRequest) // full 模式无验证码 → 400，但已计入限流
	}
	resp := postJSON(t, h, "/api/v1/auth/register", "", map[string]string{
		"nickname": "限流用户", "email": "rlx@ex.com", "password": "password123", "code": "",
	}, http.StatusTooManyRequests)
	if resp["code"] != "rate_limited" {
		t.Fatalf("code = %v, want rate_limited", resp["code"])
	}
}

// TestEmailCodeRateLimitIP 发验证码 10 次/h/IP：换不同邮箱也挡不住 IP 维度限流。
func TestEmailCodeRateLimitIP(t *testing.T) {
	srv, _ := newTestServer(t)
	h := srv.Handler()

	for i := 0; i < 10; i++ {
		postJSON(t, h, "/api/v1/auth/email-code", "", map[string]string{
			"email": fmt.Sprintf("spray%d@ex.com", i), "purpose": "register",
		}, http.StatusOK)
	}
	resp := postJSON(t, h, "/api/v1/auth/email-code", "", map[string]string{
		"email": "spray11@ex.com", "purpose": "register",
	}, http.StatusTooManyRequests)
	if resp["code"] != "rate_limited" {
		t.Fatalf("code = %v, want rate_limited", resp["code"])
	}
}

// TestPasswordResetRateLimitIP 找回密码 10 次/h/IP。
func TestPasswordResetRateLimitIP(t *testing.T) {
	srv, _ := newTestServer(t)
	h := srv.Handler()

	for i := 0; i < 10; i++ {
		postJSON(t, h, "/api/v1/auth/password/reset", "", map[string]string{
			"email": "victim@ex.com", "code": "000000", "password": "newpassword456",
		}, http.StatusBadRequest) // 验证码错误 → 400，但已计入限流
	}
	resp := postJSON(t, h, "/api/v1/auth/password/reset", "", map[string]string{
		"email": "victim@ex.com", "code": "000000", "password": "newpassword456",
	}, http.StatusTooManyRequests)
	if resp["code"] != "rate_limited" {
		t.Fatalf("code = %v, want rate_limited", resp["code"])
	}
}
