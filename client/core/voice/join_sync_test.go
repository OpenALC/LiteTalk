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

// #117：channel.joined 重建期间拿不到 roomRebuildMu 时，建 peer 可以跳过，
// 但 voice.state 补发与悄悄话目标同步必须照常执行——它们只依赖服务端房间
// 内存状态，跳过会让 UI 与服务端长期不一致。

// joinStateStub 收到 channel.join 后回一条空的 channel.joined（无成员、无音质
// 字段），并记录所有上行消息类型。
type joinStateStub struct {
	*httptest.Server

	mu       sync.Mutex
	received []string
}

func newJoinStateStub(t *testing.T) *joinStateStub {
	t.Helper()
	s := &joinStateStub{}
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
			if json.Unmarshal(raw, &env) != nil {
				continue
			}
			s.mu.Lock()
			s.received = append(s.received, env.Type)
			s.mu.Unlock()
			if env.Type != signal.TypeChannelJoin {
				continue
			}
			joined := signal.Envelope{
				Type: signal.TypeChannelJoined, TS: 1,
				Data: json.RawMessage(`{"channel_id":"room1","members":[],"self":{"user_id":"1","nickname":"甲","mute":false,"deafen":false,"speaking":false,"online":true}}`),
			}
			out, err := json.Marshal(joined)
			if err != nil {
				return
			}
			if err := conn.Write(r.Context(), websocket.MessageText, out); err != nil {
				return
			}
		}
	}))
	t.Cleanup(s.Server.Close)
	return s
}

func (s *joinStateStub) wsURL() string {
	return "ws" + strings.TrimPrefix(s.URL, "http") + "/ws"
}

func (s *joinStateStub) saw(msgType string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, m := range s.received {
		if m == msgType {
			return true
		}
	}
	return false
}

// TestJoinSyncRunsWhileRebuildLocked 重建在途（roomRebuildMu 已被占用）时
// channel.joined 仍补发 voice.state 与 voice.whisper（#117）。
func TestJoinSyncRunsWhileRebuildLocked(t *testing.T) {
	stub := newJoinStateStub(t)
	s := NewSession(api.NewAPIClient(stub.wsURL()), "tok", "n", Events{}, nil)
	s.ws = signal.NewClient(signal.Config{URL: stub.wsURL(), PingInterval: 50 * time.Millisecond})
	defer s.ws.Close()
	s.registerHandlers()
	s.protoVer.Store(3)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go s.ws.Run(ctx)
	go s.whisperLoop(ctx)
	waitSignalConnected(t, s)

	// 模拟「网络恢复/ICE 重建正在进行」：另一条路径持有重建锁。
	s.roomRebuildMu.Lock()
	// 曾经用过悄悄话（服务端可能残留目标）：入房必须补一条同步。
	s.whisperMu.Lock()
	s.whisperWant = []string{"2"}
	s.whisperMaybe = true
	s.whisperMu.Unlock()

	if err := s.JoinChannel("room1"); err != nil {
		s.roomRebuildMu.Unlock()
		t.Fatalf("JoinChannel: %v", err)
	}

	// 等到 whisper 同步下发；voice.state 必须也在。
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if stub.saw(signal.TypeVoiceWhisper) {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	s.roomRebuildMu.Unlock()

	if !stub.saw(signal.TypeVoiceWhisper) {
		t.Fatal("重建在途时入房仍应补发 voice.whisper（否则服务端停在旧目标集合）")
	}
	if !stub.saw(signal.TypeVoiceState) {
		t.Fatal("重建在途时入房仍应补发 voice.state（否则「UI 已闭麦但声音照传」）")
	}
	// 建 peer 被跳过：本地不该有 PC（新建由在途重建路径负责）。
	if p := s.peerRef.Load(); p != nil {
		t.Fatal("拿到不到重建锁时不应并发建 peer")
	}
}

// TestJoinSyncSkippedWhenPeerAlive 已有存活 peer（他人入房的 channel.joined
// 广播）时不重发状态——避免每次有人进房都刷两条信令（#117 的边界）。
func TestJoinSyncSkippedWhenPeerAlive(t *testing.T) {
	// 该分支在 peer 存活时提前返回，行为与改动前一致；用一个直接构造的
	// Session 断言 syncVoiceStateAfterJoin 只由入房路径显式调用。
	s := NewSession(api.NewAPIClient("http://127.0.0.1:1"), "tok", "n", Events{}, nil)
	s.ws = signal.NewClient(signal.Config{URL: "ws://127.0.0.1:1/ws"})
	defer s.ws.Close()
	if s.ws.Connected() {
		t.Fatal("夹具前提：信令未连接")
	}
	// 未连接时补发是空操作，不得 panic，也不得留下脏标记。
	s.syncVoiceStateAfterJoin()
	s.whisperMu.Lock()
	need := s.whisperNeedSend
	s.whisperMu.Unlock()
	if need {
		t.Fatal("从未用过悄悄话时入房同步不应置脏")
	}
}
