package acme

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

// TestHostPolicySingleDomain 白名单只放行 --domain：非白名单 SNI/Host 拒绝（403），
// 白名单（含带端口的 Host）放行到取 token 阶段（404，未签发过）。
func TestHostPolicySingleDomain(t *testing.T) {
	m, err := New("voice.example.com", t.TempDir())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	h := m.HTTPHandler()

	// 非白名单域名打挑战路径 → 403（HostPolicy 拒绝）。
	req := httptest.NewRequest(http.MethodGet, "http://evil.example.com/.well-known/acme-challenge/tok", nil)
	req.Host = "evil.example.com"
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("非白名单 Host 应 403，实际 %d", rec.Code)
	}

	// 白名单域名 → 放行到 token 查询（缓存空 → 404）。
	for _, host := range []string{"voice.example.com", "voice.example.com:80"} {
		req = httptest.NewRequest(http.MethodGet, "http://voice.example.com/.well-known/acme-challenge/tok", nil)
		req.Host = host
		rec = httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusNotFound {
			t.Fatalf("Host %q 白名单应放行（404 取不到 token），实际 %d", host, rec.Code)
		}
	}
}

// TestHTTPHandlerRedirect 非挑战路径 301 到 https（契约 301，不是 autocert 默认的 302）。
func TestHTTPHandlerRedirect(t *testing.T) {
	m, err := New("voice.example.com", t.TempDir())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	h := m.HTTPHandler()

	req := httptest.NewRequest(http.MethodGet, "http://voice.example.com/api/v1/healthz?x=1", nil)
	req.Host = "voice.example.com:80"
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusMovedPermanently {
		t.Fatalf("应 301，实际 %d", rec.Code)
	}
	if loc := rec.Header().Get("Location"); loc != "https://voice.example.com/api/v1/healthz?x=1" {
		t.Fatalf("Location = %q，应去端口跳 https", loc)
	}

	// 非 GET/HEAD 不跟随跳转（对齐 autocert 语义）。
	req = httptest.NewRequest(http.MethodPost, "http://voice.example.com/x", nil)
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("POST 应 400，实际 %d", rec.Code)
	}
}

// TestNewCacheDirPerm 证书缓存目录 0700（私钥不能全局可读）。
// Windows 无 POSIX 权限位（ACL 语义，Mode().Perm() 不反映创建参数），该断言只在
// Linux CI 的 check-server 上生效——这不是跳过功能，是权限位在 Windows 不可表达。
func TestNewCacheDirPerm(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "autocert")
	if _, err := New("voice.example.com", dir); err != nil {
		t.Fatalf("New: %v", err)
	}
	info, err := os.Stat(dir)
	if err != nil {
		t.Fatalf("缓存目录应已创建: %v", err)
	}
	if !info.IsDir() {
		t.Fatal("缓存路径应是目录")
	}
	if runtime.GOOS != "windows" {
		if mode := info.Mode().Perm(); mode != 0o700 {
			t.Fatalf("缓存目录权限 = %o，应为 0700", mode)
		}
	}
}

// TestNewRejectsEmpty 参数不合法直接报错，不静默回落。
func TestNewRejectsEmpty(t *testing.T) {
	if _, err := New("", t.TempDir()); err == nil {
		t.Fatal("空域名应报错")
	}
	if _, err := New("voice.example.com", ""); err == nil {
		t.Fatal("空缓存目录应报错")
	}
}
