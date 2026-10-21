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

// #37：入房密码的发送、缓存、重连复用与清空。

// passwordStub 记录 channel.join 载荷的 WS 桩；每次 join 后回 reply 帧。
type passwordStub struct {
	*httptest.Server
	mu    sync.Mutex
	joins []signal.ChannelJoinData
	reply []signal.Envelope
}

func newPasswordStub(t *testing.T, reply ...signal.Envelope) *passwordStub {
	t.Helper()
	s := &passwordStub{reply: reply}
	s.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer conn.CloseNow()
		for {
			_, raw, err := conn.Read(r.Context())
			if err != nil {
				return
			}
			var env signal.Envelope
			if json.Unmarshal(raw, &env) != nil || env.Type != signal.TypeChannelJoin {
				continue
			}
			var d signal.ChannelJoinData
			if json.Unmarshal(env.Data, &d) != nil {
				continue
			}
			s.mu.Lock()
			s.joins = append(s.joins, d)
			reply := s.reply
			s.mu.Unlock()
			for _, out := range reply {
				raw, err := json.Marshal(out)
				if err != nil {
					return
				}
				if err := conn.Write(r.Context(), websocket.MessageText, raw); err != nil {
					return
				}
			}
		}
	}))
	t.Cleanup(s.Server.Close)
	return s
}

func (s *passwordStub) wsURL() string { return "ws" + strings.TrimPrefix(s.URL, "http") + "/ws" }

func (s *passwordStub) joinCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.joins)
}

func (s *passwordStub) joinAt(i int) signal.ChannelJoinData {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.joins[i]
}

// newPasswordSession 接好 WS 的测试会话（joined 回执为默认成功路径）。
func newPasswordSession(t *testing.T, stub *passwordStub, ev Events) *Session {
	t.Helper()
	s := NewSession(api.NewAPIClient(stub.wsURL()), "tok", "n", ev, nil)
	s.ws = signal.NewClient(signal.Config{URL: stub.wsURL(), ReconnectMin: 20 * time.Millisecond})
	t.Cleanup(func() { s.ws.Close() })
	s.registerHandlers()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go s.ws.Run(ctx)
	waitSignalConnected(t, s)
	return s
}

// TestJoinChannelWithPasswordSends 带密码入房：channel.join 载荷携带密码；
// 不带密码的 JoinChannel 是空密码包装。
func TestJoinChannelWithPasswordSends(t *testing.T) {
	stub := newPasswordStub(t, signal.Envelope{
		Type: signal.TypeChannelJoined,
		Data: json.RawMessage(`{"channel_id":"room1","members":[],"self":{}}`),
	})
	s := newPasswordSession(t, stub, Events{})
	if err := s.JoinChannelWithPassword("room1", "pw-123"); err != nil {
		t.Fatalf("JoinChannelWithPassword: %v", err)
	}
	deadline := time.Now().Add(3 * time.Second)
	for stub.joinCount() == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if stub.joinCount() == 0 {
		t.Fatal("channel.join 未发出")
	}
	got := stub.joinAt(0)
	if got.ChannelID != "room1" || got.Password != "pw-123" {
		t.Fatalf("join 载荷 = %+v", got)
	}
	if pw := s.roomPasswordCached(); pw != "pw-123" {
		t.Fatalf("密码缓存 = %q", pw)
	}

	// JoinChannel 传空密码（缓存随之覆盖为「无密码」）。
	if err := s.JoinChannel("room2"); err != nil {
		t.Fatalf("JoinChannel: %v", err)
	}
	deadline = time.Now().Add(3 * time.Second)
	for stub.joinCount() < 2 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if stub.joinCount() < 2 {
		t.Fatal("第二次 join 未发出")
	}
	if got := stub.joinAt(1); got.ChannelID != "room2" || got.Password != "" {
		t.Fatalf("JoinChannel 应带空密码: %+v", got)
	}
	if pw := s.roomPasswordCached(); pw != "" {
		t.Fatalf("空密码 join 后缓存应清为「无密码」，实际 %q", pw)
	}
}

// TestReconnectResendsCachedPassword 重连重发 channel.join 带缓存密码（#37）；
// ICE 重建走同一缓存读取路径。
func TestReconnectResendsCachedPassword(t *testing.T) {
	stub := newPasswordStub(t, signal.Envelope{
		Type: signal.TypeChannelJoined,
		Data: json.RawMessage(`{"channel_id":"room1","members":[],"self":{}}`),
	})
	s := newPasswordSession(t, stub, Events{})
	if err := s.JoinChannelWithPassword("room1", "pw-re"); err != nil {
		t.Fatalf("JoinChannelWithPassword: %v", err)
	}
	deadline := time.Now().Add(3 * time.Second)
	for stub.joinCount() < 1 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	// 模拟重连：OnConnect 路径重发 join。
	s.handleSignalConnect(s.ws)
	deadline = time.Now().Add(3 * time.Second)
	for stub.joinCount() < 2 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if stub.joinCount() < 2 {
		t.Fatal("重连未重发 channel.join")
	}
	if got := stub.joinAt(1); got.ChannelID != "room1" || got.Password != "pw-re" {
		t.Fatalf("重连 join 应带缓存密码: %+v", got)
	}
}

// TestLeaveClearsRoomPassword Leave/voice_taken/失败回滚都清密码缓存（#37）。
func TestLeaveClearsRoomPassword(t *testing.T) {
	stub := newPasswordStub(t, signal.Envelope{
		Type: signal.TypeChannelJoined,
		Data: json.RawMessage(`{"channel_id":"room1","members":[],"self":{}}`),
	})
	s := newPasswordSession(t, stub, Events{})
	if err := s.JoinChannelWithPassword("room1", "pw-x"); err != nil {
		t.Fatalf("JoinChannelWithPassword: %v", err)
	}
	deadline := time.Now().Add(3 * time.Second)
	for stub.joinCount() == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if pw := s.roomPasswordCached(); pw != "pw-x" {
		t.Fatalf("缓存 = %q", pw)
	}
	if err := s.LeaveChannel(); err != nil {
		t.Fatalf("LeaveChannel: %v", err)
	}
	if pw := s.roomPasswordCached(); pw != "" {
		t.Fatalf("Leave 后密码缓存应清空，实际 %q", pw)
	}

	// voice_taken → leaveRoomLocal 同样清。
	if err := s.JoinChannelWithPassword("room1", "pw-y"); err != nil {
		t.Fatalf("JoinChannelWithPassword(2): %v", err)
	}
	deadline = time.Now().Add(3 * time.Second)
	for stub.joinCount() < 2 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	s.leaveRoomLocal()
	if pw := s.roomPasswordCached(); pw != "" {
		t.Fatalf("voice_taken 路径（leaveRoomLocal）应清密码缓存，实际 %q", pw)
	}
}

// TestBadPasswordRollsBack bad_password 按进房失败回滚：pending 清空、roomID
// 复位、密码缓存清空、触发 OnJoinFailed。
func TestBadPasswordRollsBack(t *testing.T) {
	failed := make(chan string, 1)
	stub := newPasswordStub(t, signal.Envelope{
		Type: signal.TypeError, TS: 1,
		Data: json.RawMessage(`{"code":"bad_password","message":"频道密码错误"}`),
	})
	s := newPasswordSession(t, stub, Events{
		OnJoinFailed: func(channelID, code, message string) { failed <- code },
	})
	if err := s.JoinChannelWithPassword("room1", "wrong"); err != nil {
		t.Fatalf("JoinChannelWithPassword: %v", err)
	}
	select {
	case code := <-failed:
		if code != "bad_password" {
			t.Fatalf("OnJoinFailed code = %q", code)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("bad_password 未触发 OnJoinFailed")
	}
	if s.InRoom() {
		t.Fatal("bad_password 回滚后 InRoom 应为 false")
	}
	if n := s.pendingJoinCount(); n != 0 {
		t.Fatalf("bad_password 后 pending 应清空，剩 %d", n)
	}
	if pw := s.roomPasswordCached(); pw != "" {
		t.Fatalf("bad_password 回滚应清密码缓存，实际 %q", pw)
	}
	if !isJoinSharedErrorCode(signal.ErrCodeBadPassword) {
		t.Fatal("bad_password 应归入 join 共享错误码（回滚路径）")
	}
}
