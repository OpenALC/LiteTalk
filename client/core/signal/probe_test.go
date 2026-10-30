package signal

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
)

func wsTestURL(srv *httptest.Server) string {
	return "ws" + strings.TrimPrefix(srv.URL, "http")
}

// TestProbeSuccess 握手成功即关闭，返回耗时并携带 token query（#49 C）。
func TestProbeSuccess(t *testing.T) {
	tokenCh := make(chan string, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case tokenCh <- r.URL.Query().Get("token"):
		default:
		}
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		_ = conn.Close(websocket.StatusNormalClosure, "probe")
	}))
	defer srv.Close()

	lat, err := Probe(context.Background(), wsTestURL(srv), "tok-1", 2*time.Second)
	if err != nil {
		t.Fatalf("Probe: %v", err)
	}
	if lat <= 0 {
		t.Fatalf("握手耗时应大于 0，得到 %v", lat)
	}
	select {
	case tok := <-tokenCh:
		if tok != "tok-1" {
			t.Fatalf("token query = %q, want tok-1", tok)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("服务端未收到拨号")
	}
}

// TestProbeUnauthorized 401 映射为短错误，且错误信息不包含 token（可能带凭据）。
func TestProbeUnauthorized(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer srv.Close()

	_, err := Probe(context.Background(), wsTestURL(srv), "secret-token", 2*time.Second)
	if err == nil {
		t.Fatal("401 应返回错误")
	}
	if !strings.Contains(err.Error(), "401") {
		t.Fatalf("错误应包含状态码: %v", err)
	}
	if strings.Contains(err.Error(), "secret-token") {
		t.Fatalf("错误信息不得泄露 token: %v", err)
	}
}

func TestProbeBadURL(t *testing.T) {
	if _, err := Probe(context.Background(), "not-a-url", "t", time.Second); err == nil {
		t.Fatal("非法地址应报错")
	}
}
