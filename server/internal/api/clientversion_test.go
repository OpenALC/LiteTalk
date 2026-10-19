package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestClientVersionGateMissing v1.1 客户端不带版本头：/auth/* 必须 426，
// 且 problem+json 同时带 detail 与 message（旧客户端 decodeError 只认 message）。
func TestClientVersionGateMissing(t *testing.T) {
	srv, _ := newTestServer(t)
	h := srv.Handler()

	do := func(path, version string, withHeader bool) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(`{}`))
		req.Header.Set("Content-Type", "application/json")
		if withHeader {
			req.Header.Set(clientVersionHeader, version)
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec
	}

	// 六个受门控的端点逐一验证，避免只守住了 login。
	for _, path := range []string{
		"/api/v1/auth/register",
		"/api/v1/auth/login",
		"/api/v1/auth/refresh",
		"/api/v1/auth/email-code",
		"/api/v1/auth/guest",
		"/api/v1/auth/password/reset",
	} {
		rec := do(path, "", false)
		if rec.Code != http.StatusUpgradeRequired {
			t.Fatalf("%s 无版本头期望 426，实际 %d body=%s", path, rec.Code, rec.Body.String())
		}
		if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/problem+json") {
			t.Fatalf("%s 426 Content-Type 应为 problem+json，实际 %q", path, ct)
		}
		var body struct {
			Code    string `json:"code"`
			Detail  string `json:"detail"`
			Message string `json:"message"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatalf("%s 426 body 解析: %v (%s)", path, err, rec.Body.String())
		}
		if body.Code != "client_too_old" {
			t.Fatalf("%s 426 code 应为 client_too_old，实际 %q", path, body.Code)
		}
		if body.Detail != clientTooOldMessage || body.Message != clientTooOldMessage {
			t.Fatalf("%s 426 提示不符：detail=%q message=%q", path, body.Detail, body.Message)
		}
	}
}

// TestClientVersionGateVersions 版本判定：低于 1.5 或无法解析一律 426；
// 1.5+、v1.5.0-alpha、2.0、dev 放行到业务分支。
func TestClientVersionGateVersions(t *testing.T) {
	const loginBody = `{"email":"nobody@example.com","password":"wrong-password-123"}`

	run := func(version string) int {
		// 每个版本用独立服务端：登录频控 5 次/min，复用会拿到 429 干扰断言。
		srv, _ := newTestServer(t)
		req := httptest.NewRequest(http.MethodPost, "/api/v1/auth/login", strings.NewReader(loginBody))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set(clientVersionHeader, version)
		rec := httptest.NewRecorder()
		srv.Handler().ServeHTTP(rec, req)
		return rec.Code
	}

	for _, v := range []string{"1.1.0", "1.4.9", "1.0", "1", "abc", "v1.4.9"} {
		if code := run(v); code != http.StatusUpgradeRequired {
			t.Fatalf("版本 %q 期望 426，实际 %d", v, code)
		}
	}
	// 放行后打到登录业务分支：账号不存在 → 401 bad_credentials，不是 426。
	for _, v := range []string{"1.5", "1.5.0", "v1.5.0-alpha", "2.0", "dev", "DEV", "e2eprobe-dev"} {
		if code := run(v); code == http.StatusUpgradeRequired {
			t.Fatalf("版本 %q 不应被 426 挡住", v)
		} else if code != http.StatusUnauthorized {
			t.Fatalf("版本 %q 期望业务 401，实际 %d", v, code)
		}
	}
}

// TestClientVersionGateExemptions logout 豁免（旧客户端仍能吊销令牌）；
// 非 /auth/* 端点（config、healthz 之外的探活面）不受影响。
func TestClientVersionGateExemptions(t *testing.T) {
	srv, _ := newTestServer(t)
	h := srv.Handler()

	// logout 无版本头：不进 426，按业务校验返回 401。
	req := httptest.NewRequest(http.MethodPost, "/api/v1/auth/logout", strings.NewReader(`{"refresh_token":"nope"}`))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code == http.StatusUpgradeRequired {
		t.Fatalf("logout 不应被版本门挡住：%s", rec.Body.String())
	}
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("logout 无效令牌期望 401，实际 %d body=%s", rec.Code, rec.Body.String())
	}

	// 免鉴权非 auth 端点（config）无版本头仍 200。
	req = httptest.NewRequest(http.MethodGet, "/api/v1/config", nil)
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("config 不应受版本门影响，实际 %d body=%s", rec.Code, rec.Body.String())
	}
}
