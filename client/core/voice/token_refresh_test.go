package voice

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/OpenALC/LiteTalk/client/core/api"
	"github.com/OpenALC/LiteTalk/client/core/signal"
	"github.com/coder/websocket"
)

// voiceTestJWT 构造只有 payload 有意义的 JWT：客户端只解 exp 判提前续期，
// 不校验签名（服务端仍独立验签）。
func voiceTestJWT(t *testing.T, exp time.Time) string {
	t.Helper()
	payload, err := json.Marshal(map[string]any{"exp": exp.Unix()})
	if err != nil {
		t.Fatalf("marshal jwt payload: %v", err)
	}
	return "eyJhbGciOiJIUzI1NiJ9." + base64.RawURLEncoding.EncodeToString(payload) + ".sig"
}

// tokenStub 同时扮演信令 WS 与 /api/v1/auth/refresh：WS 只放行 acceptToken，
// refresh 按 refreshMode 返回成功 / 普通 401 / 被顶替 401（#84）。
type tokenStub struct {
	*httptest.Server
	mu           sync.Mutex
	handshakes   []string
	refreshCalls int

	acceptToken atomic.Value // string
	refreshMode atomic.Value // string: ok | unauthorized | replaced
	freshToken  string
}

func newTokenStub(t *testing.T) *tokenStub {
	t.Helper()
	s := &tokenStub{freshToken: "fresh-access"}
	s.acceptToken.Store("fresh-access")
	s.refreshMode.Store("ok")
	mux := http.NewServeMux()
	mux.HandleFunc("/ws", func(w http.ResponseWriter, r *http.Request) {
		tok := r.URL.Query().Get("token")
		s.mu.Lock()
		s.handshakes = append(s.handshakes, tok)
		s.mu.Unlock()
		if want, _ := s.acceptToken.Load().(string); tok != want {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer conn.CloseNow()
		hello, _ := json.Marshal(signal.Envelope{Type: signal.TypeHello, TS: 1,
			Data: json.RawMessage(`{"user":{"id":"42"}}`)})
		if err := conn.Write(r.Context(), websocket.MessageText, hello); err != nil {
			return
		}
		for {
			if _, _, err := conn.Read(r.Context()); err != nil {
				return
			}
		}
	})
	mux.HandleFunc("/api/v1/auth/refresh", func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		s.refreshCalls++
		s.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		switch mode, _ := s.refreshMode.Load().(string); mode {
		case "unauthorized":
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"code":"token_expired","detail":"refresh token expired"}`))
		case "forbidden":
			w.WriteHeader(http.StatusForbidden)
			_, _ = w.Write([]byte(`{"code":"forbidden","detail":"refresh token revoked"}`))
		case "replaced":
			w.Header().Set("X-LiteTalk-Auth", "session_replaced")
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"code":"session_replaced","detail":"replaced"}`))
		case "network":
			// 传输层失败：不应答直接断开，客户端拿到 EOF/connection reset，
			// 与服务端明确拒绝（401/403）必须区分（#84 复审 4.1）。
			if hj, ok := w.(http.Hijacker); ok {
				if conn, _, err := hj.Hijack(); err == nil {
					_ = conn.Close()
				}
			}
		default:
			_, _ = w.Write([]byte(`{"access_token":"` + s.freshToken + `","refresh_token":"rt-new","expires_in":7200}`))
		}
	})
	s.Server = httptest.NewServer(mux)
	t.Cleanup(s.Server.Close)
	return s
}

func (s *tokenStub) wsURL() string {
	return "ws" + strings.TrimPrefix(s.URL, "http") + "/ws"
}

func (s *tokenStub) refreshCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.refreshCalls
}

func (s *tokenStub) tokens() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.handshakes...)
}

func waitSignalConnected(t *testing.T, s *Session) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if s.SignalConnected() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("信令 5s 内未连上")
}

// TestSessionRefreshesExpiredAccessBeforeDial access 已过期 → 重连拨号前先
// 单飞续期，带上新令牌（TokenFunc 不再返回构造时的旧值，#84）。
func TestSessionRefreshesExpiredAccessBeforeDial(t *testing.T) {
	stub := newTokenStub(t)
	c := api.NewAPIClient(stub.URL)
	c.SetToken(voiceTestJWT(t, time.Now().Add(-time.Minute)))
	c.SetRefreshSource(func(context.Context) (string, error) { return "rt-1", nil })

	s := NewSession(c, "fallback", "n", Events{}, nil)
	s.Connect(stub.wsURL())
	defer s.Close()
	waitSignalConnected(t, s)

	if got := stub.refreshCount(); got != 1 {
		t.Fatalf("拨号前应续期一次, 实际 %d 次", got)
	}
	if toks := stub.tokens(); len(toks) != 1 || toks[0] != "fresh-access" {
		t.Fatalf("拨号令牌 = %v, want [fresh-access]（不得带过期令牌）", toks)
	}
	if c.Token() != "fresh-access" {
		t.Fatalf("APIClient 令牌未更新: %q", c.Token())
	}
}

// TestSessionRefreshesOnHandshake401AndReconnects 普通 401（令牌被服务端拒绝）：
// 刷新一次 → 重拨携带新令牌 → 连上；会话不退出。
func TestSessionRefreshesOnHandshake401AndReconnects(t *testing.T) {
	stub := newTokenStub(t)
	c := api.NewAPIClient(stub.URL)
	c.SetToken("stale-access") // opaque：本地判不出过期，靠握手 401 驱动
	c.SetRefreshSource(func(context.Context) (string, error) { return "rt-1", nil })

	s := NewSession(c, "fallback", "n", Events{}, nil)
	s.Connect(stub.wsURL())
	defer s.Close()
	waitSignalConnected(t, s)

	if got := stub.refreshCount(); got != 1 {
		t.Fatalf("握手 401 应刷新一次, 实际 %d 次", got)
	}
	want := []string{"stale-access", "fresh-access"}
	got := stub.tokens()
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("拨号令牌 = %v, want %v", got, want)
	}
}

// TestSessionRefreshRejectedKicksLoginExpired 服务端明确拒绝刷新（401/403）：
// refresh_token 已失效/被吊销，走被踢链路回登录页（reason=login_expired），
// 停止重连、不无限撞墙（#84 复审 4.1）。
func TestSessionRefreshRejectedKicksLoginExpired(t *testing.T) {
	for _, mode := range []string{"unauthorized", "forbidden"} {
		t.Run(mode, func(t *testing.T) {
			stub := newTokenStub(t)
			stub.refreshMode.Store(mode)
			c := api.NewAPIClient(stub.URL)
			c.SetToken("stale-access")
			c.SetRefreshSource(func(context.Context) (string, error) { return "rt-1", nil })

			kicked := make(chan string, 1)
			s := NewSession(c, "fallback", "n", Events{
				OnKicked: func(reason string) { kicked <- reason },
			}, nil)
			s.Connect(stub.wsURL())
			defer s.Close()

			select {
			case reason := <-kicked:
				if reason != KickReasonLoginExpired {
					t.Fatalf("被踢原因 = %q, want %q", reason, KickReasonLoginExpired)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("刷新被服务端拒绝后未触发被踢")
			}
			if got := stub.refreshCount(); got != 1 {
				t.Fatalf("刷新次数 = %d, want 1", got)
			}
			// 默认 ReconnectMin 500ms：给足退避窗口，仍应只有一次拨号、一次刷新。
			time.Sleep(700 * time.Millisecond)
			if got := stub.refreshCount(); got != 1 {
				t.Fatalf("刷新失败后仍在重试: %d 次", got)
			}
			if toks := stub.tokens(); len(toks) != 1 {
				t.Fatalf("刷新失败后仍在拨号: %v", toks)
			}
			if s.SignalConnected() {
				t.Fatal("刷新失败后不应保持连接")
			}
		})
	}
}

// TestSessionRefreshTransientFailureKeepsRetrying 刷新请求遇到传输层错误
// （服务端并未应答）：不能按登录过期处理、不触发被踢；signal 层退避重试，
// 网络恢复后自动连上（#84 复审 4.1）。
func TestSessionRefreshTransientFailureKeepsRetrying(t *testing.T) {
	stub := newTokenStub(t)
	stub.refreshMode.Store("network")
	stub.acceptToken.Store("fresh-access")
	c := api.NewAPIClient(stub.URL)
	c.SetToken("stale-access") // opaque：本地判不出过期，靠握手 401 驱动刷新
	c.SetRefreshSource(func(context.Context) (string, error) { return "rt-1", nil })

	kicked := make(chan string, 1)
	s := NewSession(c, "fallback", "n", Events{
		OnKicked: func(reason string) { kicked <- reason },
	}, nil)
	s.Connect(stub.wsURL())
	defer s.Close()

	// 首次刷新撞网络错误。
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) && stub.refreshCount() == 0 {
		time.Sleep(10 * time.Millisecond)
	}
	if got := stub.refreshCount(); got != 1 {
		t.Fatalf("刷新次数 = %d, want 1", got)
	}
	select {
	case reason := <-kicked:
		t.Fatalf("网络错误不应触发被踢（reason=%q）", reason)
	case <-time.After(300 * time.Millisecond):
	}
	if s.SignalConnected() {
		t.Fatal("刷新失败期间不应保持连接")
	}

	// 网络恢复：退避重试的下一次刷新成功并连上。
	stub.refreshMode.Store("ok")
	waitSignalConnected(t, s)
	select {
	case reason := <-kicked:
		t.Fatalf("恢复后不应触发被踢（reason=%q）", reason)
	default:
	}
}

// TestSessionRefreshSessionReplacedKicks 刷新窗口发现会话已被顶替：
// 走被踢链路（app 层清令牌回登录页），不重连。
func TestSessionRefreshSessionReplacedKicks(t *testing.T) {
	stub := newTokenStub(t)
	stub.refreshMode.Store("replaced")
	c := api.NewAPIClient(stub.URL)
	c.SetToken("stale-access")
	c.SetRefreshSource(func(context.Context) (string, error) { return "rt-1", nil })

	kicked := make(chan string, 1)
	s := NewSession(c, "fallback", "n", Events{
		OnKicked: func(reason string) { kicked <- reason },
	}, nil)
	s.Connect(stub.wsURL())
	defer s.Close()

	select {
	case reason := <-kicked:
		if reason != signal.KickReasonLoginElsewhere {
			t.Fatalf("被踢原因 = %q, want %q", reason, signal.KickReasonLoginElsewhere)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("刷新撞上 session_replaced 后未触发被踢")
	}
	if got := stub.refreshCount(); got != 1 {
		t.Fatalf("刷新次数 = %d, want 1", got)
	}
	if toks := stub.tokens(); len(toks) != 1 {
		t.Fatalf("被顶替不得重拨: %v", toks)
	}
}

// TestSessionTokenFallbackWhenAPIClientEmpty APIClient 无 access（dev-令牌/
// 直连测试）：TokenFunc 回落构造时的令牌，且不触发任何刷新。
func TestSessionTokenFallbackWhenAPIClientEmpty(t *testing.T) {
	stub := newTokenStub(t)
	stub.acceptToken.Store("dev-fallback")
	c := api.NewAPIClient(stub.URL) // 无令牌、无刷新源
	s := NewSession(c, "dev-fallback", "n", Events{}, nil)
	s.Connect(stub.wsURL())
	defer s.Close()
	waitSignalConnected(t, s)

	if toks := stub.tokens(); len(toks) != 1 || toks[0] != "dev-fallback" {
		t.Fatalf("拨号令牌 = %v, want [dev-fallback]", toks)
	}
	if got := stub.refreshCount(); got != 0 {
		t.Fatalf("无刷新源不应发刷新请求, 实际 %d 次", got)
	}
}
