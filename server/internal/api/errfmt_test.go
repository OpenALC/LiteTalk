package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestProblemJSON 错误体 RFC 9457（#31）：application/problem+json，
// {type,title,status,code,detail,message}，type 形如 urn:litetalk:error:<code>。
// message 与 detail 同值：旧客户端只认 message，缺了它读不到任何提示。
func TestProblemJSON(t *testing.T) {
	srv, _ := newTestServer(t)
	h := srv.Handler()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/users/me", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("期望 401 实际 %d", rec.Code)
	}
	ct := rec.Header().Get("Content-Type")
	if !strings.HasPrefix(ct, "application/problem+json") {
		t.Fatalf("Content-Type 应为 problem+json，实际 %q", ct)
	}
	var body struct {
		Type    string `json:"type"`
		Title   string `json:"title"`
		Status  int    `json:"status"`
		Code    string `json:"code"`
		Detail  string `json:"detail"`
		Message string `json:"message"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("problem 解析: %v (%s)", err, rec.Body.String())
	}
	if body.Type != "urn:litetalk:error:unauthorized" || body.Code != "unauthorized" {
		t.Fatalf("type/code 不符：%+v", body)
	}
	if body.Status != 401 || body.Title == "" || body.Detail == "" {
		t.Fatalf("status/title/detail 不符：%+v", body)
	}
	// 旧客户端 decodeError 只认 message（#31 兼容）；两者必须同时存在且同值。
	if body.Message == "" || body.Message != body.Detail {
		t.Fatalf("message 应与 detail 同值且非空：detail=%q message=%q", body.Detail, body.Message)
	}
}

// TestRateLimitHeaders 限流头（#31）：成功响应带 X-RateLimit-*，超限 429 另带
// Retry-After 且 Remaining 归零。
func TestRateLimitHeaders(t *testing.T) {
	srv, _ := newTestServer(t)
	h := srv.Handler()

	do := func() *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "/api/v1/auth/login", strings.NewReader(`{}`))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set(clientVersionHeader, testClientVersion)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec
	}

	// 第 1 次（401 密码错）应带配额头，Remaining = 4。
	rec := do()
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("期望 401 实际 %d", rec.Code)
	}
	if rec.Header().Get("X-RateLimit-Limit") != "5" || rec.Header().Get("X-RateLimit-Remaining") != "4" {
		t.Fatalf("限流头不符：%v", rec.Header())
	}
	if rec.Header().Get("X-RateLimit-Reset") == "" {
		t.Fatal("缺 X-RateLimit-Reset")
	}
	if rec.Header().Get("Retry-After") != "" {
		t.Fatal("非 429 不应有 Retry-After")
	}

	// 跑满 5 次后第 6 次 429，带 Retry-After。
	for i := 0; i < 4; i++ {
		do()
	}
	rec = do()
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("期望 429 实际 %d", rec.Code)
	}
	if rec.Header().Get("X-RateLimit-Remaining") != "0" {
		t.Fatalf("超限 Remaining 应为 0，实际 %q", rec.Header().Get("X-RateLimit-Remaining"))
	}
	if rec.Header().Get("Retry-After") == "" {
		t.Fatal("429 应带 Retry-After")
	}
	var body struct {
		Code    string `json:"code"`
		Detail  string `json:"detail"`
		Message string `json:"message"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil || body.Code != "rate_limited" {
		t.Fatalf("429 problem：%v %s", err, rec.Body.String())
	}
	if body.Message == "" || body.Message != body.Detail {
		t.Fatalf("429 message 应与 detail 同值：detail=%q message=%q", body.Detail, body.Message)
	}
}
