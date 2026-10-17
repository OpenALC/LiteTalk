package api

import (
	"bytes"
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
