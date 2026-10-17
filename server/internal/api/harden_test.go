package api

import (
	"bytes"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

var errSentinel = errors.New("sentinel: db connection lost")

// TestWriteStoreErrLogs500 writeStoreErr default 分支必须记底层错误并返回 500（#21）。
func TestWriteStoreErrLogs500(t *testing.T) {
	var logBuf bytes.Buffer
	srv, _ := newTestServerMode(t, AuthModeFull)
	srv.log = log.New(&logBuf, "", 0)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/users/me", nil)
	rec := httptest.NewRecorder()
	srv.writeStoreErr(rec, req, errSentinel)

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("期望 500，实际 %d", rec.Code)
	}
	if !strings.Contains(logBuf.String(), "sentinel") {
		t.Fatalf("日志应包含底层错误，实际: %q", logBuf.String())
	}
	if !strings.Contains(logBuf.String(), "/api/v1/users/me") {
		t.Fatalf("日志应包含请求路径，实际: %q", logBuf.String())
	}
}

// TestDecodeJSONTooLarge 超过 1MB 的请求体返回 413 too_large（#21，含 chunked）。
func TestDecodeJSONTooLarge(t *testing.T) {
	srv, _ := newTestServerMode(t, AuthModeFull)
	h := srv.Handler()
	// 构造超过 1MB 的 JSON 字段。
	big := strings.Repeat("x", 2<<20)
	body := `{"nickname":"` + big + `"}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/auth/register", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.ContentLength = int64(len(body))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("期望 413，实际 %d body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "too_large") {
		t.Fatalf("响应应包含 too_large，实际 %s", rec.Body.String())
	}
}

// TestDecodeJSONTooLargeChunked chunked 编码（ContentLength 未知）同样返回 413。
func TestDecodeJSONTooLargeChunked(t *testing.T) {
	srv, _ := newTestServerMode(t, AuthModeFull)
	h := srv.Handler()
	big := strings.Repeat("x", 2<<20)
	body := `{"nickname":"` + big + `"}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/auth/register", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.ContentLength = -1 // 模拟 chunked：Content-Length 未知
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("chunked 期望 413，实际 %d body=%s", rec.Code, rec.Body.String())
	}
}

// TestValidPassword 密码 8–72 字节（#22）。
func TestValidPassword(t *testing.T) {
	valid := []string{"12345678", "a-very-long-but-acceptable-password-string-0123456789", strings.Repeat("x", 72)}
	for _, s := range valid {
		if !validPassword(s) {
			t.Fatalf("密码 %q 应合法", s[:20])
		}
	}
	invalid := []string{"", "1234567", strings.Repeat("x", 73)}
	for _, s := range invalid {
		if validPassword(s) {
			t.Fatalf("密码（%d 字节）应非法", len(s))
		}
	}
}

// TestPatchMeOK 合法昵称变更正常通过（409 路径由 writeStoreErr 单测覆盖）。
func TestPatchMeOK(t *testing.T) {
	srv, _ := newTestServerMode(t, AuthModeSimple)
	h := srv.Handler()
	// simple 模式直接注册。
	out := postJSON(t, h, "/api/v1/auth/register", "", map[string]string{
		"nickname": "conflict-user", "email": "conflict@test.com", "password": "password123",
	}, 201)
	tok, _ := out["access_token"].(string)
	if tok == "" {
		t.Fatalf("注册响应缺 access_token: %v", out)
	}

	body := map[string]any{"nickname": "new-name-ok"}
	buf := new(bytes.Buffer)
	_ = json.NewEncoder(buf).Encode(body)
	req := httptest.NewRequest(http.MethodPatch, "/api/v1/users/me", buf)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+tok)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("合法昵称变更应 200，实际 %d body=%s", rec.Code, rec.Body.String())
	}
}

// TestRateLimitCodesUnified 频控超限一律 429 rate_limited（#22 / §0.1.3）。
func TestRateLimitCodesUnified(t *testing.T) {
	srv, _ := newTestServerMode(t, AuthModeFull)
	h := srv.Handler()
	// 登录频控 5 次/min：第 6 次应 429 且 code=rate_limited。
	for i := 0; i < 6; i++ {
		body := map[string]any{"email": "rl@test.com", "password": "wrong-password-123"}
		buf := new(bytes.Buffer)
		_ = json.NewEncoder(buf).Encode(body)
		req := httptest.NewRequest(http.MethodPost, "/api/v1/auth/login", buf)
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if i == 5 {
			if rec.Code != http.StatusTooManyRequests {
				t.Fatalf("第 6 次应 429，实际 %d", rec.Code)
			}
			if !strings.Contains(rec.Body.String(), "rate_limited") {
				t.Fatalf("code 应为 rate_limited，实际 %s", rec.Body.String())
			}
		}
	}
}
