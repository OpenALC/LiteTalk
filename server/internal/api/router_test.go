package api

import (
	"log"
	"net/http/httptest"
	"testing"
)

// TestClientIPDefaultDistrustsProxy 默认不信任任何代理：X-Real-IP 被忽略（P1）。
func TestClientIPDefaultDistrustsProxy(t *testing.T) {
	srv := New(Config{})
	req := httptest.NewRequest("GET", "/x", nil)
	req.RemoteAddr = "203.0.113.7:5555"
	req.Header.Set("X-Real-IP", "1.2.3.4")
	if got := srv.clientIP(req); got != "203.0.113.7" {
		t.Fatalf("clientIP = %q, want RemoteAddr 203.0.113.7（默认不信任代理）", got)
	}
}

// TestClientIPTrustedProxy 命中可信列表才采用 X-Real-IP。
func TestClientIPTrustedProxy(t *testing.T) {
	srv := New(Config{TrustedProxies: []string{"10.0.0.0/8", "192.0.2.1"}})

	// CIDR 命中：采用 X-Real-IP。
	req := httptest.NewRequest("GET", "/x", nil)
	req.RemoteAddr = "10.1.2.3:40000"
	req.Header.Set("X-Real-IP", "1.2.3.4")
	if got := srv.clientIP(req); got != "1.2.3.4" {
		t.Fatalf("clientIP = %q, want 1.2.3.4", got)
	}

	// 精确 IP 命中。
	req = httptest.NewRequest("GET", "/x", nil)
	req.RemoteAddr = "192.0.2.1:40000"
	req.Header.Set("X-Real-IP", "5.6.7.8")
	if got := srv.clientIP(req); got != "5.6.7.8" {
		t.Fatalf("clientIP = %q, want 5.6.7.8", got)
	}

	// 不在列表：忽略 X-Real-IP。
	req = httptest.NewRequest("GET", "/x", nil)
	req.RemoteAddr = "198.51.100.9:40000"
	req.Header.Set("X-Real-IP", "1.2.3.4")
	if got := srv.clientIP(req); got != "198.51.100.9" {
		t.Fatalf("clientIP = %q, want 198.51.100.9", got)
	}

	// 可信代理但 X-Real-IP 非法：回落 RemoteAddr。
	req = httptest.NewRequest("GET", "/x", nil)
	req.RemoteAddr = "10.1.2.3:40000"
	req.Header.Set("X-Real-IP", "not-an-ip")
	if got := srv.clientIP(req); got != "10.1.2.3" {
		t.Fatalf("clientIP = %q, want 10.1.2.3（非法 X-Real-IP 回落）", got)
	}
}

// TestClientIPIPv6 IPv6 不截断成 "["：v6 RemoteAddr 与 v6 可信代理都能正确解析。
func TestClientIPIPv6(t *testing.T) {
	srv := New(Config{})

	// v6 RemoteAddr（默认不信任代理）：host 完整取出。
	req := httptest.NewRequest("GET", "/x", nil)
	req.RemoteAddr = "[2001:db8::42]:8077"
	req.Header.Set("X-Real-IP", "::1")
	if got := srv.clientIP(req); got != "2001:db8::42" {
		t.Fatalf("clientIP = %q, want 2001:db8::42", got)
	}

	// v6 可信代理 + v6 X-Real-IP。
	srv6 := New(Config{TrustedProxies: []string{"2001:db8::/64"}})
	req = httptest.NewRequest("GET", "/x", nil)
	req.RemoteAddr = "[2001:db8::1]:8077"
	req.Header.Set("X-Real-IP", "2001:db8::99")
	if got := srv6.clientIP(req); got != "2001:db8::99" {
		t.Fatalf("clientIP = %q, want 2001:db8::99", got)
	}
}

// TestParseTrustedProxies 非法项跳过不致命。
func TestParseTrustedProxies(t *testing.T) {
	nets := parseTrustedProxies(log.Default(), []string{"10.0.0.0/8", "bad-cidr", "not-an-ip", "  ", "192.168.1.1"})
	if len(nets) != 2 {
		t.Fatalf("应解析出 2 个网络，实际 %d", len(nets))
	}
}
