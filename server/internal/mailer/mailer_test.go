package mailer

import (
	"encoding/json"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
)

// TestNewPriority webhook > SMTP > Console。
func TestNewPriority(t *testing.T) {
	lg := log.New(io.Discard, "", 0)

	m, name := New(Config{}, lg)
	if name != "console" {
		t.Fatalf("空配置 name = %s, want console", name)
	}
	if _, ok := m.(*Console); !ok {
		t.Fatalf("空配置类型 %T, want *Console", m)
	}

	m, name = New(Config{SMTPHost: "smtp.ex.com", SMTPFrom: "a@ex.com"}, lg)
	if name != "smtp" {
		t.Fatalf("SMTP 配置 name = %s, want smtp", name)
	}

	m, name = New(Config{
		WebhookURL: "https://hook.ex.com",
		SMTPHost:   "smtp.ex.com",
		SMTPFrom:   "a@ex.com",
	}, lg)
	if name != "webhook" {
		t.Fatalf("双配置 name = %s, want webhook（优先级最高）", name)
	}
	if _, ok := m.(*Webhook); !ok {
		t.Fatalf("双配置类型 %T, want *Webhook", m)
	}

	// SMTP 配置不全降级 Console。
	m, name = New(Config{SMTPHost: "smtp.ex.com"}, lg)
	if name != "console" {
		t.Fatalf("不全 SMTP name = %s, want console（降级）", name)
	}
	if _, ok := m.(*Console); !ok {
		t.Fatalf("降级类型 %T, want *Console", m)
	}
}

// TestWebhookSendCode 第三方发码平台最小契约：POST JSON + Bearer + 2xx 成功。
func TestWebhookSendCode(t *testing.T) {
	type recv struct {
		Email   string `json:"email"`
		Code    string `json:"code"`
		Purpose string `json:"purpose"`
		Auth    string
	}
	var got recv
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got.Auth = r.Header.Get("Authorization")
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &got)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	w := &Webhook{URL: srv.URL, Token: "tok123"}
	if err := w.SendCode("u@ex.com", "register", "123456"); err != nil {
		t.Fatal(err)
	}
	if got.Email != "u@ex.com" || got.Code != "123456" || got.Purpose != "register" {
		t.Fatalf("载荷 = %+v", got)
	}
	if got.Auth != "Bearer tok123" {
		t.Fatalf("Authorization = %q", got.Auth)
	}

	// 非 2xx 视为失败。
	srv2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
	}))
	defer srv2.Close()
	if err := (&Webhook{URL: srv2.URL}).SendCode("u@ex.com", "register", "1"); err == nil {
		t.Fatal("502 应返回错误")
	}
}

// TestWebhookNoToken 无 token 时不带 Authorization 头。
func TestWebhookNoToken(t *testing.T) {
	authHdr := "unset"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		authHdr = r.Header.Get("Authorization")
	}))
	defer srv.Close()
	if err := (&Webhook{URL: srv.URL}).SendCode("u@ex.com", "reset", "654321"); err != nil {
		t.Fatal(err)
	}
	if authHdr != "" {
		t.Fatalf("无 token 时 Authorization = %q, want 空", authHdr)
	}
}

// TestConsoleEnvSMTP 环境变量经 Config 传递（回归 main.go 装配路径）。
func TestConsoleEnvSMTP(t *testing.T) {
	t.Setenv("LITETALK_SMTP_HOST", "smtp.ex.com")
	t.Setenv("LITETALK_SMTP_FROM", "bot@ex.com")
	cfg := Config{
		SMTPHost: os.Getenv("LITETALK_SMTP_HOST"),
		SMTPFrom: os.Getenv("LITETALK_SMTP_FROM"),
	}
	m, name := New(cfg, log.New(io.Discard, "", 0))
	if name != "smtp" {
		t.Fatalf("name = %s, want smtp", name)
	}
	s := m.(*SMTP)
	if s.Port != "587" {
		t.Fatalf("默认端口 = %s, want 587", s.Port)
	}
}
