package signal

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"
)

// stubServer 本地 WS 桩服务：校验 dev token、下发 hello、回声消息、可注入断连。
type stubServer struct {
	*httptest.Server

	mu       sync.Mutex
	conns    int
	pings    int
	tokens   []string    // 每次拨号携带的 token（校验 TokenFunc 每次拨号调用）
	dropOnce atomic.Bool // 首条连接收到首条业务消息后异常断开（测重连）

	// closeWith 一次性 close code：非 0 时首条连接下发 hello 后以该码关闭
	// （测 4001 被踢 / 4000 静默替换）。Swap(0) 取用即清，只作用一次。
	closeWith atomic.Int32
	// rejectWith 非空时握手直接 401，并带 X-LiteTalk-Auth: <值> 响应头
	// （测被踢类鉴权失败与 token_expired 的区分）。
	rejectWith atomic.Value // string
	// requireToken 非空时只接受该令牌（其余 401、不带被踢头）：模拟 access
	// 过期后服务端拒绝旧令牌，刷新后新令牌才放行（#84）。
	requireToken atomic.Value // string
	// denied 握手 401 次数（含刷新后的重试），用于断言「重试一次即止」。
	denied atomic.Int32

	received chan Envelope
}

func newStubServer(t *testing.T) *stubServer {
	t.Helper()
	s := &stubServer{received: make(chan Envelope, 64)}
	s.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		tok := r.URL.Query().Get("token")
		if !strings.HasPrefix(tok, "dev-") {
			s.denied.Add(1)
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		if want, _ := s.requireToken.Load().(string); want != "" && tok != want {
			s.denied.Add(1)
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		// 握手即拒：带 X-LiteTalk-Auth 头（测被踢类 401）。
		if v, _ := s.rejectWith.Load().(string); v != "" {
			w.Header().Set(authHeader, v)
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{
			OnPingReceived: func(ctx context.Context, payload []byte) bool {
				s.mu.Lock()
				s.pings++
				s.mu.Unlock()
				return true
			},
		})
		if err != nil {
			return
		}
		s.mu.Lock()
		s.conns++
		s.tokens = append(s.tokens, tok)
		connIdx := s.conns
		s.mu.Unlock()
		defer conn.CloseNow()

		// 鉴权成功：先下发 hello。
		hello, _ := json.Marshal(Envelope{Type: TypeHello, Data: json.RawMessage(`{"user":{"user_id":"` + tok + `"}}`), TS: 1})
		if err := conn.Write(r.Context(), websocket.MessageText, hello); err != nil {
			return
		}
		// 一次性 close：首条连接下发 hello 后以指定 code 关闭（踢线/静默替换）。
		if code := s.closeWith.Swap(0); code != 0 && connIdx == 1 {
			_ = conn.Close(websocket.StatusCode(code), "test close")
			return
		}
		firstMsg := true
		for {
			_, raw, err := conn.Read(r.Context())
			if err != nil {
				return
			}
			var env Envelope
			if json.Unmarshal(raw, &env) != nil {
				continue
			}
			select {
			case s.received <- env:
			default:
			}
			// 注入断连：首条连接的首条业务消息后掐线（不发 close 帧）。
			if s.dropOnce.Load() && connIdx == 1 && firstMsg {
				firstMsg = false
				if s.dropOnce.CompareAndSwap(true, false) {
					return // defer CloseNow：模拟异常断线
				}
			}
			firstMsg = false
			// 回声。
			if err := conn.Write(r.Context(), websocket.MessageText, raw); err != nil {
				return
			}
		}
	}))
	t.Cleanup(s.Server.Close)
	return s
}

func (s *stubServer) wsURL() string {
	return "ws" + strings.TrimPrefix(s.URL, "http") + "/ws"
}

func (s *stubServer) connCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.conns
}

func (s *stubServer) pingCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.pings
}

// lastToken 最近一次拨号携带的 token。
func (s *stubServer) lastToken() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.tokens) == 0 {
		return ""
	}
	return s.tokens[len(s.tokens)-1]
}

func testConfig(url, token string) Config {
	return Config{
		URL:          url,
		TokenFunc:    func(context.Context) (string, error) { return token, nil },
		PingInterval: 50 * time.Millisecond,
		ReconnectMin: 20 * time.Millisecond,
		ReconnectMax: 100 * time.Millisecond,
	}
}

func waitFor(t *testing.T, what string, d time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timeout waiting %s", what)
}

func TestWSConnectSendEcho(t *testing.T) {
	stub := newStubServer(t)
	c := NewClient(testConfig(stub.wsURL(), "dev-alice"))
	defer c.Close()

	connected := make(chan struct{}, 1)
	c.cfg.OnConnect = func(*Client) { connected <- struct{}{} }

	// 桩服务按原 type 回声，订阅 channel.join 收回声。
	echoed := make(chan Envelope, 1)
	c.On(TypeChannelJoin, func(env Envelope) { echoed <- env })

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go c.Run(ctx)

	select {
	case <-connected:
	case <-time.After(3 * time.Second):
		t.Fatal("no OnConnect (hello) within 3s")
	}

	if err := c.Send(TypeChannelJoin, ChannelJoinData{ChannelID: "test"}); err != nil {
		t.Fatalf("Send: %v", err)
	}
	select {
	case env := <-echoed:
		var d ChannelJoinData
		if err := env.Decode(&d); err != nil {
			t.Fatalf("decode echo: %v", err)
		}
		if d.ChannelID != "test" {
			t.Fatalf("echo channel_id = %q, want test", d.ChannelID)
		}
		if env.TS == 0 {
			t.Fatal("echo envelope missing ts")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("no echo within 3s")
	}
}

func TestWSAuthFailure(t *testing.T) {
	stub := newStubServer(t)
	c := NewClient(testConfig(stub.wsURL(), "bad-token"))
	defer c.Close()

	done := make(chan error, 1)
	go func() { done <- c.Run(context.Background()) }()
	select {
	case err := <-done:
		if err != ErrAuthFatal {
			t.Fatalf("Run err = %v, want ErrAuthFatal", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return after 401")
	}
}

// TestWSHandshake401KickedHeader 握手 401 的 X-LiteTalk-Auth 头区分两类失败：
// session_replaced/session_replayed = 本端已被踢，必须走被踢链路（此前只打日志，
// 用户看到的是静默的登录失败）；token_expired = 普通令牌失败，走刷新重试一次，
// 不能当被踢处理（#84）。
func TestWSHandshake401KickedHeader(t *testing.T) {
	for _, tc := range []struct {
		name        string
		header      string
		wantKick    bool
		wantRefresh int32
	}{
		{"session_replaced 走被踢", "session_replaced", true, 0},
		{"session_replayed 走被踢", "session_replayed", true, 0},
		{"token_expired 刷新一次后停", "token_expired", false, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			stub := newStubServer(t)
			stub.rejectWith.Store(tc.header)
			c := NewClient(testConfig(stub.wsURL(), "dev-alice"))
			defer c.Close()

			kicked := make(chan string, 1)
			c.cfg.OnKicked = func(reason string) { kicked <- reason }
			var refreshCalls atomic.Int32
			// 刷新「成功」但服务端恒拒：只允许重试一次，第二次 401 必须致命。
			c.cfg.OnAuthFailed = func(context.Context) error {
				refreshCalls.Add(1)
				return nil
			}
			done := make(chan error, 1)
			go func() { done <- c.Run(context.Background()) }()

			select {
			case err := <-done:
				if err != ErrAuthFatal {
					t.Fatalf("Run err = %v, want ErrAuthFatal", err)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("Run did not return after 401")
			}
			select {
			case got := <-kicked:
				if !tc.wantKick {
					t.Fatalf("header %q 不应触发被踢，却收到 reason=%q", tc.header, got)
				}
				if got != KickReasonLoginElsewhere {
					t.Fatalf("kicked reason = %q, want %q", got, KickReasonLoginElsewhere)
				}
			default:
				if tc.wantKick {
					t.Fatalf("header %q 应触发被踢，OnKicked 未调用", tc.header)
				}
			}
			if got := refreshCalls.Load(); got != tc.wantRefresh {
				t.Fatalf("刷新次数 = %d, want %d（被顶替类不得刷新）", got, tc.wantRefresh)
			}
		})
	}
}

// TestWSHandshake401RefreshRetry 普通 401（access 过期）：OnAuthFailed 刷新成功
// 后立即重拨一次并连上，Run 不退出——此前 401 直接 ErrAuthFatal，常驻托盘的
// 用户 access 到期后只要断线就永久离线（#84）。
func TestWSHandshake401RefreshRetry(t *testing.T) {
	stub := newStubServer(t)
	stub.requireToken.Store("dev-new")

	var tok atomic.Value
	tok.Store("dev-old")
	var refreshCalls atomic.Int32
	c := NewClient(Config{
		URL: stub.wsURL(),
		TokenFunc: func(context.Context) (string, error) {
			return tok.Load().(string), nil
		},
		OnAuthFailed: func(context.Context) error {
			refreshCalls.Add(1)
			tok.Store("dev-new")
			return nil
		},
		PingInterval: 50 * time.Millisecond,
		ReconnectMin: 20 * time.Millisecond,
		ReconnectMax: 100 * time.Millisecond,
	})
	defer c.Close()

	done := make(chan error, 1)
	go func() { done <- c.Run(context.Background()) }()
	waitFor(t, "connect after refresh", 5*time.Second, c.Connected)

	if got := refreshCalls.Load(); got != 1 {
		t.Fatalf("刷新次数 = %d, want 1", got)
	}
	if got := stub.lastToken(); got != "dev-new" {
		t.Fatalf("重拨应携带刷新后的令牌, 实际 %q", got)
	}
	select {
	case err := <-done:
		t.Fatalf("刷新重连成功后 Run 不应退出: %v", err)
	default:
	}
}

// TestWSHandshake401RefreshFailsFatal 刷新失败：Run 立即以 ErrAuthFatal 返回，
// 只拨号一次（不进入无限重连；上层据此回登录态）。
func TestWSHandshake401RefreshFailsFatal(t *testing.T) {
	stub := newStubServer(t)
	stub.requireToken.Store("dev-new")

	var refreshCalls atomic.Int32
	c := NewClient(testConfig(stub.wsURL(), "dev-old"))
	defer c.Close()
	c.cfg.OnAuthFailed = func(context.Context) error {
		refreshCalls.Add(1)
		return errors.New("refresh rejected")
	}

	done := make(chan error, 1)
	go func() { done <- c.Run(context.Background()) }()
	select {
	case err := <-done:
		if err != ErrAuthFatal {
			t.Fatalf("Run err = %v, want ErrAuthFatal", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("刷新失败后 Run 未返回")
	}
	if got := refreshCalls.Load(); got != 1 {
		t.Fatalf("刷新次数 = %d, want 1", got)
	}
	if got := stub.denied.Load(); got != 1 {
		t.Fatalf("握手 401 次数 = %d, want 1（刷新失败不得重试）", got)
	}
	// 再等一个退避窗口：不得出现后台重连。
	time.Sleep(200 * time.Millisecond)
	if got := stub.denied.Load(); got != 1 {
		t.Fatalf("刷新失败后仍在重连: 401 次数 %d", got)
	}
}

// TestWSHandshake401RefreshTransientRetry 刷新请求遇到传输层瞬时错误
// （OnAuthFailed 返回 ErrAuthTransient）：Run 复位刷新预算、退避后重试，
// 网络恢复即连上，不按致命收场（#84 复审 4.1）。
func TestWSHandshake401RefreshTransientRetry(t *testing.T) {
	stub := newStubServer(t)
	stub.requireToken.Store("dev-new")

	var tok atomic.Value
	tok.Store("dev-old")
	var refreshCalls atomic.Int32
	c := NewClient(Config{
		URL: stub.wsURL(),
		TokenFunc: func(context.Context) (string, error) {
			return tok.Load().(string), nil
		},
		OnAuthFailed: func(context.Context) error {
			if refreshCalls.Add(1) == 1 {
				return ErrAuthTransient // 第一次刷新请求网络中断
			}
			tok.Store("dev-new")
			return nil
		},
		PingInterval: 50 * time.Millisecond,
		ReconnectMin: 20 * time.Millisecond,
		ReconnectMax: 100 * time.Millisecond,
	})
	defer c.Close()

	done := make(chan error, 1)
	go func() { done <- c.Run(context.Background()) }()
	waitFor(t, "connect after transient refresh failure", 5*time.Second, c.Connected)

	if got := refreshCalls.Load(); got != 2 {
		t.Fatalf("刷新次数 = %d, want 2（瞬时失败后应复位预算）", got)
	}
	if got := stub.lastToken(); got != "dev-new" {
		t.Fatalf("重拨应携带刷新后的令牌, 实际 %q", got)
	}
	select {
	case err := <-done:
		t.Fatalf("瞬时刷新失败后 Run 不应退出: %v", err)
	default:
	}
}

// TestWSHandshake401RefreshTransientKeepsRetrying 刷新始终瞬时失败：Run 不退出，
// 但必须按退避节奏重拨而不是紧循环（每次 401 都重新拿到一次刷新预算）。
func TestWSHandshake401RefreshTransientKeepsRetrying(t *testing.T) {
	stub := newStubServer(t)
	stub.requireToken.Store("dev-never")

	var refreshCalls atomic.Int32
	c := NewClient(testConfig(stub.wsURL(), "dev-old"))
	defer c.Close()
	c.cfg.OnAuthFailed = func(context.Context) error {
		refreshCalls.Add(1)
		return ErrAuthTransient
	}

	done := make(chan error, 1)
	go func() { done <- c.Run(context.Background()) }()
	waitFor(t, "multiple dials while transient", 5*time.Second, func() bool {
		return stub.denied.Load() >= 3
	})
	select {
	case err := <-done:
		t.Fatalf("瞬时刷新失败后 Run 不应退出: %v", err)
	default:
	}
	if got := refreshCalls.Load(); got < 2 {
		t.Fatalf("刷新次数 = %d, want >= 2（预算应随瞬时失败复位）", got)
	}

	c.Close()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Close 后 Run = %v, want nil", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Close 后 Run 未退出")
	}
}

// TestWSHandshake401RefreshRetryOnce 刷新成功但新令牌仍被拒：只重试一次即停，
// 不发生握手 401 风暴。
func TestWSHandshake401RefreshRetryOnce(t *testing.T) {
	stub := newStubServer(t)
	stub.requireToken.Store("dev-never")

	var refreshCalls atomic.Int32
	c := NewClient(testConfig(stub.wsURL(), "dev-old"))
	defer c.Close()
	c.cfg.OnAuthFailed = func(context.Context) error {
		refreshCalls.Add(1)
		return nil // 「刷新成功」，但服务端仍然拒绝新令牌
	}

	done := make(chan error, 1)
	go func() { done <- c.Run(context.Background()) }()
	select {
	case err := <-done:
		if err != ErrAuthFatal {
			t.Fatalf("Run err = %v, want ErrAuthFatal", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("刷新重试仍 401 后 Run 未返回")
	}
	if got := refreshCalls.Load(); got != 1 {
		t.Fatalf("刷新次数 = %d, want 1", got)
	}
	if got := stub.denied.Load(); got != 2 {
		t.Fatalf("握手 401 次数 = %d, want 2（首拨 + 一次重试）", got)
	}
	time.Sleep(200 * time.Millisecond)
	if got := stub.denied.Load(); got != 2 {
		t.Fatalf("重试预算用尽后仍在拨号: 401 次数 %d", got)
	}
}

// TestWSClose4001Kicked session.kicked 消息在服务端发送窗口内丢帧就什么都不剩，
// close 4001 是可靠的那一半：必须补走被踢链路。
func TestWSClose4001Kicked(t *testing.T) {
	stub := newStubServer(t)
	stub.closeWith.Store(4001)
	c := NewClient(testConfig(stub.wsURL(), "dev-alice"))
	defer c.Close()

	kicked := make(chan string, 4)
	c.cfg.OnKicked = func(reason string) { kicked <- reason }
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go c.Run(ctx)

	select {
	case got := <-kicked:
		if got != KickReasonLoginElsewhere {
			t.Fatalf("kicked reason = %q, want %q", got, KickReasonLoginElsewhere)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no OnKicked after close 4001")
	}
}

// TestWSClose4000SilentReplace close 4000 = 同 sid 半开连接被新连接替换，
// 服务端已有新连接在跑：Run 必须直接返回 ErrSilentReplace 且绝不重连
// （重连只会把新连接再顶掉，互相踢）。
func TestWSClose4000SilentReplace(t *testing.T) {
	stub := newStubServer(t)
	stub.closeWith.Store(4000)
	c := NewClient(testConfig(stub.wsURL(), "dev-bob"))
	defer c.Close()

	done := make(chan error, 1)
	go func() { done <- c.Run(context.Background()) }()
	select {
	case err := <-done:
		if !errors.Is(err, ErrSilentReplace) {
			t.Fatalf("Run err = %v, want ErrSilentReplace", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return after close 4000")
	}
	// 退避窗口足以重连好几次：稳态后服务端仍只应见到一条连接。
	time.Sleep(300 * time.Millisecond)
	if n := stub.connCount(); n != 1 {
		t.Fatalf("connCount = %d, want 1 (silent replace must not reconnect)", n)
	}
}

func TestWSReconnect(t *testing.T) {
	stub := newStubServer(t)
	stub.dropOnce.Store(true)

	c := NewClient(testConfig(stub.wsURL(), "dev-bob"))
	defer c.Close()

	var connects atomic.Int32
	c.cfg.OnConnect = func(cl *Client) {
		connects.Add(1)
		// 每次（重）连成功后重发订阅状态。
		_ = cl.Send(TypeChannelJoin, ChannelJoinData{ChannelID: "room"})
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go c.Run(ctx)

	// 首连 → 发 join → 桩掐线 → 自动重连 → OnConnect 再触发并重发 join。
	waitFor(t, "reconnect (2+ hellos)", 5*time.Second, func() bool {
		return connects.Load() >= 2
	})
	waitFor(t, "2+ server connections", 3*time.Second, func() bool {
		return stub.connCount() >= 2
	})
	if got := c.Stats.Reconnects.Load(); got < 1 {
		t.Fatalf("Reconnects = %d, want >= 1", got)
	}

	// 重连后回声链路仍可用。
	echoed := make(chan Envelope, 1)
	c.On(TypeVoiceState, func(env Envelope) { echoed <- env })
	if err := c.Send(TypeVoiceState, VoiceStateData{Speaking: Bool(true)}); err != nil {
		t.Fatalf("Send after reconnect: %v", err)
	}
	select {
	case env := <-echoed:
		var d VoiceStateData
		if err := env.Decode(&d); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if d.Speaking == nil || !*d.Speaking {
			t.Fatalf("echo speaking = %v, want true", d.Speaking)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("no echo after reconnect")
	}
}

func TestWSHeartbeat(t *testing.T) {
	stub := newStubServer(t)
	c := NewClient(testConfig(stub.wsURL(), "dev-carol"))
	defer c.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go c.Run(ctx)

	waitFor(t, "client ping frames", 3*time.Second, func() bool {
		return stub.pingCount() >= 2
	})
	if !c.Connected() {
		t.Fatal("should stay connected during heartbeat")
	}
}

// TestWSTokenFuncPerDial 校验 TokenFunc 每次拨号都被调用、且取的是当时的最新值
// （令牌刷新后重连必须带新令牌，否则服务端按旧令牌鉴权永远 401）。
func TestWSTokenFuncPerDial(t *testing.T) {
	stub := newStubServer(t)
	stub.dropOnce.Store(true)

	var tok atomic.Value // string
	tok.Store("dev-first")
	var calls atomic.Int32
	c := NewClient(Config{
		URL: stub.wsURL(),
		TokenFunc: func(context.Context) (string, error) {
			calls.Add(1)
			return tok.Load().(string), nil
		},
		PingInterval: 50 * time.Millisecond,
		ReconnectMin: 20 * time.Millisecond,
		ReconnectMax: 100 * time.Millisecond,
	})
	defer c.Close()

	var connects atomic.Int32
	c.cfg.OnConnect = func(*Client) { connects.Add(1) }
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go c.Run(ctx)

	waitFor(t, "first connect", 3*time.Second, func() bool { return connects.Load() >= 1 })

	// 首连后换令牌。桩的 dropOnce 在收到首条上行（client.hello）时即掐线，
	// 本条 Send 只是业务面探活：可能正撞断线窗口，ErrNotConnected 与重连无关
	//（重连由桩掐线触发），其余错误才是真失败。
	tok.Store("dev-second")
	if err := c.Send(TypeChannelJoin, ChannelJoinData{ChannelID: "x"}); err != nil && !errors.Is(err, ErrNotConnected) {
		t.Fatalf("Send: %v", err)
	}
	waitFor(t, "reconnect with new token", 5*time.Second, func() bool {
		return stub.connCount() >= 2 && stub.lastToken() == "dev-second"
	})
	if n := calls.Load(); n < 2 {
		t.Fatalf("TokenFunc calls = %d, want >= 2 (one per dial)", n)
	}
}

func TestWSSendWhileDisconnected(t *testing.T) {
	stub := newStubServer(t)
	c := NewClient(testConfig(stub.wsURL(), "dev-dave"))
	// 未 Run：无连接。
	if err := c.Send(TypeChannelJoin, ChannelJoinData{ChannelID: "x"}); err != ErrNotConnected {
		t.Fatalf("Send before connect = %v, want ErrNotConnected", err)
	}
}

func TestWSCloseStopsRun(t *testing.T) {
	stub := newStubServer(t)
	c := NewClient(testConfig(stub.wsURL(), "dev-erin"))
	done := make(chan error, 1)
	go func() { done <- c.Run(context.Background()) }()
	waitFor(t, "connect", 3*time.Second, c.Connected)
	c.Close()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run after Close = %v, want nil", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Run did not stop after Close")
	}
}
