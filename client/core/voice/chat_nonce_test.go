package voice

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/OpenALC/LiteTalk/client/core/api"
	"github.com/OpenALC/LiteTalk/client/core/signal"
	"github.com/coder/websocket"
)

// chatWireStub 记录 chat.send 载荷的 WS 桩：连接后先下发 greeting（模拟
// server.hello{v2} 或老服务端 hello{v1}），之后把收到的 chat.send 存起来。
type chatWireStub struct {
	*httptest.Server

	mu    sync.Mutex
	sends []signal.ChatSendData
}

func newChatWireStub(t *testing.T, greeting signal.Envelope) *chatWireStub {
	t.Helper()
	s := &chatWireStub{}
	s.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer conn.CloseNow()
		if greeting.Type != "" {
			raw, err := json.Marshal(greeting)
			if err != nil {
				return
			}
			if err := conn.Write(r.Context(), websocket.MessageText, raw); err != nil {
				return
			}
		}
		for {
			_, raw, err := conn.Read(r.Context())
			if err != nil {
				return
			}
			var env signal.Envelope
			if json.Unmarshal(raw, &env) != nil || env.Type != signal.TypeChatSend {
				continue
			}
			var d signal.ChatSendData
			if env.Decode(&d) != nil {
				continue
			}
			s.mu.Lock()
			s.sends = append(s.sends, d)
			s.mu.Unlock()
		}
	}))
	t.Cleanup(s.Server.Close)
	return s
}

func (s *chatWireStub) wsURL() string {
	return "ws" + strings.TrimPrefix(s.URL, "http") + "/ws"
}

func (s *chatWireStub) snapshot() []signal.ChatSendData {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]signal.ChatSendData, len(s.sends))
	copy(out, s.sends)
	return out
}

// waitChatSends 等至少 n 条 chat.send 到达。
func (s *chatWireStub) waitChatSends(t *testing.T, n int) []signal.ChatSendData {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if got := s.snapshot(); len(got) >= n {
			return got
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("等待 %d 条 chat.send 超时，实际 %d", n, len(s.snapshot()))
	return nil
}

// serverHelloV2 v2 协商结果（OnConnect 在首帧分发后触发，此时 protoVer 已就绪）。
func serverHelloV2() signal.Envelope {
	return signal.Envelope{Type: signal.TypeServerHello, TS: 1,
		Data: json.RawMessage(`{"version":2,"session_id":"S1"}`)}
}

// newWireSession 构造注入了 OnConnect 的会话并启动 ws.Run（不初始化媒体管线）。
func newWireSession(t *testing.T, stub *chatWireStub) *Session {
	t.Helper()
	s := NewSession(api.NewAPIClient(stub.wsURL()), "tok", "n", Events{}, nil)
	s.ws = signal.NewClient(signal.Config{
		URL:          stub.wsURL(),
		ReconnectMin: 20 * time.Millisecond,
		ReconnectMax: 100 * time.Millisecond,
		OnConnect:    s.handleSignalConnect,
	})
	s.registerHandlers()
	ctx, cancel := context.WithCancel(context.Background())
	go s.ws.Run(ctx)
	t.Cleanup(func() {
		cancel()
		s.ws.Close()
	})
	return s
}

// TestSendChatCarriesNonce 协议 v2：每次用户发送生成不同的 nonce，随 chat.send 上行。
func TestSendChatCarriesNonce(t *testing.T) {
	stub := newChatWireStub(t, serverHelloV2())
	s := newWireSession(t, stub)

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) && s.ProtocolVersion() < 2 {
		time.Sleep(10 * time.Millisecond)
	}
	if s.ProtocolVersion() < 2 {
		t.Fatalf("协议未协商到 v2（protoVer=%d）", s.ProtocolVersion())
	}

	if err := s.SendChat("ch1", "你好"); err != nil {
		t.Fatalf("SendChat: %v", err)
	}
	got := stub.waitChatSends(t, 1)[0]
	if got.ChannelID != "ch1" || got.Type != "text" || got.Content != "你好" {
		t.Fatalf("chat.send 载荷错误: %+v", got)
	}
	if got.Nonce == "" || len(got.Nonce) > 64 {
		t.Fatalf("nonce 缺失或超长: %q", got.Nonce)
	}

	// 第二次发送必须是新 nonce（每次用户发送各自生成）。
	if err := s.SendChat("ch1", "第二条"); err != nil {
		t.Fatalf("SendChat: %v", err)
	}
	sends := stub.waitChatSends(t, 2)
	if sends[1].Nonce == "" || sends[1].Nonce == got.Nonce {
		t.Fatalf("不同发送应生成不同 nonce：%q vs %q", got.Nonce, sends[1].Nonce)
	}
}

// TestSendChatRetryReusesNonce 断线窗口发送入队；重连 OnConnect 重发时复用
// 入队时生成的同一个 nonce（服务端据此幂等，不会重复落库/广播）。
func TestSendChatRetryReusesNonce(t *testing.T) {
	stub := newChatWireStub(t, serverHelloV2())
	s := NewSession(api.NewAPIClient(stub.wsURL()), "tok", "n", Events{}, nil)
	s.ws = signal.NewClient(signal.Config{
		URL:          stub.wsURL(),
		ReconnectMin: 20 * time.Millisecond,
		ReconnectMax: 100 * time.Millisecond,
		OnConnect:    s.handleSignalConnect,
	})
	s.registerHandlers()
	// 不启动 Run：模拟断线窗口，Send 返回 ErrNotConnected，消息应连同 nonce 入队。
	if err := s.SendChat("ch1", "离线消息"); err != nil {
		t.Fatalf("断线时 SendChat 应入队而不是报错: %v", err)
	}
	s.chatMu.Lock()
	if len(s.pendingChats) != 1 {
		s.chatMu.Unlock()
		t.Fatalf("待发队列应有 1 条，实际 %d", len(s.pendingChats))
	}
	wantNonce := s.pendingChats[0].nonce
	s.chatMu.Unlock()
	if wantNonce == "" {
		t.Fatal("入队消息缺 nonce")
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go s.ws.Run(ctx)
	defer s.ws.Close()

	sends := stub.waitChatSends(t, 1)
	if sends[0].Nonce != wantNonce {
		t.Fatalf("重发应复用入队 nonce：got %q want %q", sends[0].Nonce, wantNonce)
	}
	if sends[0].Content != "离线消息" {
		t.Fatalf("重发内容错误: %+v", sends[0])
	}
	// 只重发一次。
	time.Sleep(200 * time.Millisecond)
	if n := len(stub.snapshot()); n != 1 {
		t.Fatalf("积压消息应只重发一次，实际 %d 条", n)
	}
}

// TestSendChatOmitsNonceLegacyServer 老服务端不协商协议（protoVer=0）：
// 不发送 v2 才有的 nonce 字段，wire 保持 v1 原样。
func TestSendChatOmitsNonceLegacyServer(t *testing.T) {
	stub := newChatWireStub(t, signal.Envelope{Type: signal.TypeHello, TS: 1, Data: json.RawMessage(`{}`)})
	s := newWireSession(t, stub)

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) && !s.ws.Connected() {
		time.Sleep(10 * time.Millisecond)
	}
	if !s.ws.Connected() {
		t.Fatal("ws 未连接")
	}
	if err := s.SendChat("ch1", "老服务端"); err != nil {
		t.Fatalf("SendChat: %v", err)
	}
	got := stub.waitChatSends(t, 1)[0]
	if got.Nonce != "" {
		t.Fatalf("老服务端不应收到 nonce，实际 %q", got.Nonce)
	}
}

// TestChatMessageNonceForwarded chat.message 回显解析：nonce 原样传给 ChatHandler
// （UI 仍按消息 id 去重，nonce 仅供对账/调试）。
func TestChatMessageNonceForwarded(t *testing.T) {
	stub, _ := newDownlinkStubData(t, signal.Envelope{
		Type: signal.TypeChatMessage, TS: 1,
		Data: json.RawMessage(`{"id":"m1","channel_id":"c1","content":"hi","ts":123,"nonce":"n-1",` +
			`"user":{"id":"u2","nickname":"bob"}}`),
	})
	s := NewSession(api.NewAPIClient(stub.wsURL()), "tok", "n", Events{}, nil)
	s.ws = signal.NewClient(signal.Config{URL: stub.wsURL(), ReconnectMin: 20 * time.Millisecond})
	defer s.ws.Close()
	s.registerHandlers()

	got := make(chan [7]string, 1)
	s.ChatHandler(func(channelID, msgID, userID, nickname, content, ts, nonce string) {
		got <- [7]string{channelID, msgID, userID, nickname, content, ts, nonce}
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go s.ws.Run(ctx)

	select {
	case v := <-got:
		want := [7]string{"c1", "m1", "u2", "bob", "hi", "123", "n-1"}
		if v != want {
			t.Fatalf("chat.message 解析/透传错误：got %v want %v", v, want)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("未收到 chat.message")
	}
}
