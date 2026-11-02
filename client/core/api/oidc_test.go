package api

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestOidcAuthorize 锁 authorize 端点契约（#118 P2-5）：POST /auth/oidc/authorize、
// 200 → url 字段、免鉴权。
func TestOidcAuthorize(t *testing.T) {
	var gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"url":"https://idp.example/authorize?state=s1"}`))
	}))
	defer srv.Close()

	c := NewAPIClient(srv.URL)
	u, err := c.OidcAuthorize(context.Background())
	if err != nil {
		t.Fatalf("OidcAuthorize: %v", err)
	}
	if gotPath != "/api/v1/auth/oidc/authorize" {
		t.Fatalf("请求路径 = %q", gotPath)
	}
	if u != "https://idp.example/authorize?state=s1" {
		t.Fatalf("url = %q", u)
	}
}

// TestOidcAuthorizeError 服务端 problem+json（如 429 频控）按 APIError 透传，
// 前端 toast 与回调页文案依赖 Code/Message 字段。429 在传输层已转 *APIError
// 再被 post 包一层（Retry-After 只在头里），必须 errors.As 解开。
func TestOidcAuthorizeError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"code":"rate_limited","detail":"操作过于频繁，请稍后再试"}`))
	}))
	defer srv.Close()

	c := NewAPIClient(srv.URL)
	_, err := c.OidcAuthorize(context.Background())
	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("期望 *APIError，得到 %T: %v", err, err)
	}
	if apiErr.Status != http.StatusTooManyRequests || apiErr.Message != "操作过于频繁，请稍后再试" {
		t.Fatalf("错误映射不符: %+v", apiErr)
	}
}

// TestOidcAuthorizeEmptyURL 空 url 是契约违约：返回错误而不是把空串交给上层
// 去做「打开浏览器」。
func TestOidcAuthorizeEmptyURL(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"url":"  "}`))
	}))
	defer srv.Close()

	c := NewAPIClient(srv.URL)
	if _, err := c.OidcAuthorize(context.Background()); err == nil || !strings.Contains(err.Error(), "缺少授权 URL") {
		t.Fatalf("空 url 应报错，得到: %v", err)
	}
}

// TestServerConfigOidcEnabled 锁 /config 的 oidc_enabled 解析：桌面侧此前没有
// 该字段（JSON 忽略未知键），永远看不到 OIDC 开关。
func TestServerConfigOidcEnabled(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"auth_mode":"oidc","mail_configured":false,"self_host":false,"bootstrap_required":false,"oidc_enabled":true}`))
	}))
	defer srv.Close()

	c := NewAPIClient(srv.URL)
	cfg, err := c.GetServerConfig()
	if err != nil {
		t.Fatalf("GetServerConfig: %v", err)
	}
	if cfg.AuthMode != "oidc" || !cfg.OidcEnabled {
		t.Fatalf("解析不符: %+v", cfg)
	}
}
