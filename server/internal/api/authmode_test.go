package api

import (
	"net/http"
	"testing"

	"github.com/OpenALC/LiteTalk/server/internal/auth"
	"github.com/OpenALC/LiteTalk/server/internal/mailer"
	"github.com/OpenALC/LiteTalk/server/internal/store"
)

// newTestServerMode 指定 AuthMode 的测试服务器（注入 SessionIndex，理由同 newTestServer）。
func newTestServerMode(t *testing.T, mode string) (*Server, *store.Mem) {
	t.Helper()
	mem := store.NewMem()
	return New(Config{
		Store:    mem,
		JWT:      auth.NewJWT("test-secret"),
		Refresh:  auth.NewRefreshStore(mem),
		Codes:    auth.NewEmailCodes(mem),
		Limits:   auth.NewRateLimit(mem),
		AuthMode: mode,
		Session:  auth.NewSessionIndex(mem, mem),
		Presence: mem, // 在线状态（presence:{uid}）与生产一样走 KV
	}), mem
}

// TestSimpleModeRegister 免验证码注册 + email-code 明确提示 + simple 模式禁用找回密码（P0 S3）。
func TestSimpleModeRegister(t *testing.T) {
	srv, _ := newTestServerMode(t, AuthModeSimple)
	h := srv.Handler()

	// 注册：不带 code 也应 201（code 字段忽略）。
	resp := postJSON(t, h, "/api/v1/auth/register", "", map[string]string{
		"nickname": "简捷用户", "email": "simple@ex.com", "password": "password123", "code": "",
	}, http.StatusCreated)
	if resp["access_token"] == "" {
		t.Fatal("注册未返回令牌")
	}

	// 发验证码接口：明确提示而非 500。
	errResp := postJSON(t, h, "/api/v1/auth/email-code", "", map[string]string{
		"email": "simple@ex.com", "purpose": "register",
	}, http.StatusBadRequest)
	if errResp["code"] != "verification_not_required" {
		t.Fatalf("code = %v, want verification_not_required", errResp["code"])
	}

	// 找回密码：simple 模式无邮件通道，凭邮箱+新密码即可改密 = 任意账号接管，必须 403。
	reset := postJSON(t, h, "/api/v1/auth/password/reset", "", map[string]string{
		"email": "simple@ex.com", "password": "newpassword456",
	}, http.StatusForbidden)
	if reset["code"] != "reset_unavailable" {
		t.Fatalf("code = %v, want reset_unavailable", reset["code"])
	}
	// 密码未被改动：旧密码仍可登录。
	postJSON(t, h, "/api/v1/auth/login", "", map[string]string{
		"email": "simple@ex.com", "password": "password123",
	}, http.StatusOK)
}

// TestFullModeUnchanged full 模式语义不变：注册仍需验证码。
func TestFullModeUnchanged(t *testing.T) {
	srv, _ := newTestServerMode(t, AuthModeFull)
	h := srv.Handler()

	// 无验证码注册必须被拒。
	bad := postJSON(t, h, "/api/v1/auth/register", "", map[string]string{
		"nickname": "完整用户", "email": "full@ex.com", "password": "password123", "code": "",
	}, http.StatusBadRequest)
	if bad["code"] != "bad_code" {
		t.Fatalf("code = %v, want bad_code", bad["code"])
	}

	// 找回密码无验证码同样被拒。
	bad2 := postJSON(t, h, "/api/v1/auth/password/reset", "", map[string]string{
		"email": "full@ex.com", "password": "newpassword456",
	}, http.StatusBadRequest)
	if bad2["code"] != "bad_code" {
		t.Fatalf("reset code = %v, want bad_code", bad2["code"])
	}
}

// TestConfigEndpoint GET /api/v1/config 返回模式与发信配置。
func TestConfigEndpoint(t *testing.T) {
	srv, _ := newTestServerMode(t, AuthModeSimple)
	h := srv.Handler()

	cfg, _ := getJSON(t, h, "/api/v1/config", "", http.StatusOK).(map[string]any)
	if cfg["auth_mode"] != "simple" {
		t.Fatalf("auth_mode = %v, want simple", cfg["auth_mode"])
	}
	if cfg["mail_configured"] != false {
		t.Fatalf("mail_configured = %v, want false (Console)", cfg["mail_configured"])
	}

	// 配置真实发信通道后 mail_configured 为 true。
	mem := store.NewMem()
	srv2 := New(Config{
		Store:    mem,
		JWT:      auth.NewJWT("test-secret"),
		Refresh:  auth.NewRefreshStore(mem),
		Codes:    auth.NewEmailCodes(mem),
		Limits:   auth.NewRateLimit(mem),
		Mailer:   &mailer.Webhook{URL: "https://example.invalid/hook"},
		AuthMode: AuthModeFull,
	})
	cfg2, _ := getJSON(t, srv2.Handler(), "/api/v1/config", "", http.StatusOK).(map[string]any)
	if cfg2["mail_configured"] != true {
		t.Fatalf("mail_configured = %v, want true (Webhook)", cfg2["mail_configured"])
	}
	if cfg2["auth_mode"] != "full" {
		t.Fatalf("auth_mode = %v, want full", cfg2["auth_mode"])
	}
}
