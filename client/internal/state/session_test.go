package state

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

	"github.com/OpenALC/LiteTalk/client/internal/audio"
	"github.com/OpenALC/LiteTalk/client/internal/rtc"
	"github.com/OpenALC/LiteTalk/client/internal/signal"
	"github.com/coder/websocket"
	"github.com/pion/webrtc/v4"
)

// TestJoinChannelRollback 发送失败必须回滚 roomID——roomID 先存是为断线重连
// 恢复，失败路径不复位会让 OnConnect 重发 join 造成「幽灵入房」。
func TestJoinChannelRollback(t *testing.T) {
	s := NewSession(NewAPIClient("http://127.0.0.1:1"), "tok", "n", Events{}, nil)
	s.ws = signal.NewClient(signal.Config{URL: "ws://127.0.0.1:1/ws"})
	defer s.ws.Close()

	// ws 未 Run：Send 返回 ErrNotConnected，模拟发送失败路径。
	if err := s.JoinChannel("room1"); err == nil {
		t.Fatal("未连接时 JoinChannel 应返回错误")
	}
	if got := s.roomID.Load().(string); got != "" {
		t.Fatalf("发送失败后 roomID 应回滚为空, 得到 %q", got)
	}
}

// TestJoinChannelVoiceUnavailable 媒体管线初始化失败（如无 Opus 的平台）时
// JoinChannel 必须给出明确错误供前端 toast，而不是静默成功或 panic。
func TestJoinChannelVoiceUnavailable(t *testing.T) {
	s := NewSession(NewAPIClient("http://127.0.0.1:1"), "tok", "n", Events{}, nil)
	s.ws = signal.NewClient(signal.Config{URL: "ws://127.0.0.1:1/ws"})
	defer s.ws.Close()
	s.voiceUnavailable = true
	s.voiceErr = errors.New("opus: unsupported platform")

	err := s.JoinChannel("room1")
	if err == nil {
		t.Fatal("voiceUnavailable 时 JoinChannel 应返回错误")
	}
	if !strings.Contains(err.Error(), "语音不可用") {
		t.Fatalf("错误应提示语音不可用, 得到 %v", err)
	}
	if got := s.roomID.Load().(string); got != "" {
		t.Fatalf("voiceUnavailable 时不应写入 roomID, 得到 %q", got)
	}
}

// TestHandleKickedForwardsReason session.kicked 的上报出口：原因原样透传，
// 未注册回调时静默（Events 零值可安全构造）。
func TestHandleKickedForwardsReason(t *testing.T) {
	got := ""
	s := NewSession(NewAPIClient("http://127.0.0.1:1"), "tok", "n",
		Events{OnKicked: func(reason string) { got = reason }}, nil)
	s.handleKicked("login_elsewhere")
	if got != "login_elsewhere" {
		t.Fatalf("应原样透传被踢原因, 得到 %q", got)
	}

	// 零值 Events：无回调不得 panic。
	quiet := NewSession(NewAPIClient("http://127.0.0.1:1"), "tok", "n", Events{}, nil)
	quiet.handleKicked("login_elsewhere")
}

// downlinkStub 最小 WS 桩：连上后立即下发一条指定类型的消息，并记录客户端
// 此后发来的全部消息类型（用于断言「没有发 channel.leave」）。
type downlinkStub struct {
	*httptest.Server

	mu       sync.Mutex
	received []string
}

func newDownlinkStub(t *testing.T, downType string) *downlinkStub {
	t.Helper()
	s := &downlinkStub{}
	s.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer conn.CloseNow()
		env, err := json.Marshal(signal.Envelope{Type: downType, TS: 1})
		if err != nil {
			return
		}
		if err := conn.Write(r.Context(), websocket.MessageText, env); err != nil {
			return
		}
		for {
			_, raw, err := conn.Read(r.Context())
			if err != nil {
				return
			}
			var e signal.Envelope
			if json.Unmarshal(raw, &e) != nil {
				continue
			}
			s.mu.Lock()
			s.received = append(s.received, e.Type)
			s.mu.Unlock()
		}
	}))
	t.Cleanup(s.Server.Close)
	return s
}

func (s *downlinkStub) wsURL() string {
	return "ws" + strings.TrimPrefix(s.URL, "http") + "/ws"
}

func (s *downlinkStub) saw(msgType string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, m := range s.received {
		if m == msgType {
			return true
		}
	}
	return false
}

// TestVoiceTakenLeavesRoomLocally session.voice_taken：服务端已把本端摘出成员表，
// 本地必须跟着退房（roomID/成员/媒体链路全清），且绝不发 channel.leave——那条
// leave 按 uid 处理，会把抢占端一起带离房间。成员表不清就是「幽灵在房」。
func TestVoiceTakenLeavesRoomLocally(t *testing.T) {
	stub := newDownlinkStub(t, signal.TypeSessionVoiceTaken)

	taken := make(chan struct{}, 1)
	s := NewSession(NewAPIClient(stub.wsURL()), "tok", "n",
		Events{OnVoiceTaken: func() { taken <- struct{}{} }}, nil)
	s.ws = signal.NewClient(signal.Config{
		URL:          stub.wsURL(),
		PingInterval: 50 * time.Millisecond,
		ReconnectMin: 20 * time.Millisecond,
		ReconnectMax: 100 * time.Millisecond,
	})
	defer s.ws.Close()
	s.registerHandlers()

	// 伪造在房状态（不依赖真实媒体管线：leaveRoomLocal 对 nil 管线是空操作）。
	s.roomID.Store("room1")
	s.mu.Lock()
	s.members = map[string]PeerView{"u1": {UserID: "u1", Nickname: "甲"}}
	s.mu.Unlock()
	s.remoteMgr = audio.NewRemoteManager(audio.NewMixer(func([]int16) int { return 0 }), nil)
	peer, err := rtc.NewPeer(rtc.PeerConfig{StreamID: "u1"})
	if err != nil {
		t.Fatalf("NewPeer: %v", err)
	}
	s.peerRef.Store(peer)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go s.ws.Run(ctx)

	select {
	case <-taken:
	case <-time.After(5 * time.Second):
		t.Fatal("no OnVoiceTaken after session.voice_taken")
	}
	if got := s.roomID.Load().(string); got != "" {
		t.Fatalf("roomID = %q, want empty", got)
	}
	if n := len(s.Members()); n != 0 {
		t.Fatalf("members = %d, want 0", n)
	}
	if p := s.peerRef.Load(); p != nil {
		t.Fatal("peerRef should be cleared")
	}
	if st := peer.ConnectionState(); st != webrtc.PeerConnectionStateClosed {
		t.Fatalf("peer state = %v, want closed", st)
	}
	if got := s.InRoom(); got {
		t.Fatal("InRoom should be false after voice_taken")
	}
	// 绝不再发 channel.leave：本端已被服务端摘出，再发会把新端也带离房间。
	time.Sleep(200 * time.Millisecond)
	if stub.saw(signal.TypeChannelLeave) {
		t.Fatal("must not send channel.leave on session.voice_taken")
	}
}

// TestIsLoopbackURL 钉住环回判定：本机联调要收集环回候选，公网必须关闭——
// 判反了要么连不上本机 SFU，要么把 127.0.0.1 暴露给远端。
// TestPrefetchICEDecouplesReadLoop 预取缓存与读循环解耦（#13）：cachedICE 纯读
// 缓存，凭证接口关掉后照样返回；JoinChannel 负责预取（可阻塞的绑定线程）。
func TestPrefetchICEDecouplesReadLoop(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"iceServers":[{"urls":["stun:s:3478"]}],"expiresAt":1}`))
	}))
	s := NewSession(NewAPIClient(srv.URL), "tok", "n", Events{}, nil)

	// JoinChannel 预取：未连接的 ws 让 Send 报错回滚 roomID，但预取已经完成。
	s.ws = signal.NewClient(signal.Config{URL: "ws://127.0.0.1:1/ws"})
	defer s.ws.Close()
	_ = s.JoinChannel("room1")
	if got := hits.Load(); got != 1 {
		t.Fatalf("JoinChannel 应预取一次凭证，实际 %d 次", got)
	}

	// cachedICE 只读缓存：凭证接口下线后仍返回，且不再产生请求。
	srv.Close()
	got := s.cachedICE()
	if len(got) != 1 || got[0].URLs[0] != "stun:s:3478" {
		t.Fatalf("cachedICE 应返回预取结果，实际 %v", got)
	}
	if n := hits.Load(); n != 1 {
		t.Fatalf("cachedICE 不得触发网络请求，凭证请求累计 %d 次", n)
	}

	// 从未预取 → 空列表（降级），不 panic。
	s2 := NewSession(NewAPIClient("http://127.0.0.1:1"), "tok", "n", Events{}, nil)
	if got2 := s2.cachedICE(); got2 != nil {
		t.Fatalf("未预取时 cachedICE 应为 nil，实际 %v", got2)
	}
}

// TestRTCCredentialsShortTimeout 凭证接口黑洞时按短超时快速失败（#13）：
// 默认 15s 会把 WS 读循环停摆到撞心跳预算，这里必须数秒内返回错误。
func TestRTCCredentialsShortTimeout(t *testing.T) {
	block := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-block // 黑洞：不放行就一直挂着
	}))
	defer srv.Close()
	defer close(block) // 先放行 handler 再关 server，避免 Close 等待悬挂请求

	old := rtCredentialsTimeout
	rtCredentialsTimeout = 100 * time.Millisecond
	defer func() { rtCredentialsTimeout = old }()

	s := NewSession(NewAPIClient(srv.URL), "tok", "n", Events{}, nil)
	start := time.Now()
	if _, err := s.api.RTCCredentials(); err == nil {
		t.Fatal("黑洞端点应返回超时错误")
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("短超时未生效，耗时 %v", elapsed)
	}
	// 超时同样走降级：fetchICEServers 返回空列表。
	if got := s.fetchICEServers(); got != nil {
		t.Fatalf("超时应降级空 ICE 列表，实际 %v", got)
	}
}

func TestIsLoopbackURL(t *testing.T) {
	cases := []struct {
		base string
		want bool
	}{
		{"http://127.0.0.1:8077", true},
		{"http://127.0.0.1", true},
		{"http://127.1.2.3:8077", true},
		{"http://[::1]:8077", true},
		{"http://localhost:8077", true},
		{"https://localhost", true},
		{"http://LocalHost:8077", true},
		{"https://talk.example.com", false},
		{"http://192.168.1.10:8077", false},
		{"http://10.0.0.1:8077", false},
		{"http://8.8.8.8", false},
		{"https://[2001:db8::1]:8077", false},
		{"not-a-url", false},
		{"", false},
		{"http://", false},
	}
	for _, c := range cases {
		if got := IsLoopbackURL(c.base); got != c.want {
			t.Errorf("IsLoopbackURL(%q) = %v, want %v", c.base, got, c.want)
		}
	}
}

// TestFetchICEServers 验证凭证拉取的两条路径：正常拿到 ICE servers 与失败降级为空。
func TestFetchICEServers(t *testing.T) {
	// 正常路径：服务端返回 stun+turn 两条。
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"iceServers":[
			{"urls":["stun:stun.example.com:3478"]},
			{"urls":["turn:turn.example.com:3478?transport=tcp"],"username":"u","credential":"c"}
		],"expiresAt":1}`))
	}))
	defer srv.Close()
	s := NewSession(NewAPIClient(srv.URL), "tok", "n", Events{}, nil)
	got := s.fetchICEServers()
	if len(got) != 2 {
		t.Fatalf("期望 2 条 ICE server，实际 %d", len(got))
	}
	if got[0].URLs[0] != "stun:stun.example.com:3478" {
		t.Errorf("stun URL 错误: %v", got[0].URLs)
	}
	if got[1].Username != "u" || got[1].Credential != "c" {
		t.Errorf("turn 凭证错误: %+v", got[1])
	}

	// 降级路径：服务端未配置 TURN（503）→ 空 ICE 列表，不 panic 不阻断。
	fail := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "turn not configured", http.StatusServiceUnavailable)
	}))
	defer fail.Close()
	s2 := NewSession(NewAPIClient(fail.URL), "tok", "n", Events{}, nil)
	if got2 := s2.fetchICEServers(); got2 != nil {
		t.Fatalf("失败时应降级为空 ICE 列表，实际 %v", got2)
	}

	// 空数组也是合法的（dev 无 TURN）：不报错，返回 nil。
	empty := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"iceServers":[],"expiresAt":0}`))
	}))
	defer empty.Close()
	s3 := NewSession(NewAPIClient(empty.URL), "tok", "n", Events{}, nil)
	if got3 := s3.fetchICEServers(); got3 != nil {
		t.Fatalf("空 iceServers 应返回 nil，实际 %v", got3)
	}
}

// newDownlinkStubData 同 downlinkStub，但下行消息带 data 载荷；记录上行消息类型
// （rtc.failed 重建断言用）并计数连接次数（server.shutdown 停重连断言用）。
func newDownlinkStubData(t *testing.T, env signal.Envelope) (*downlinkStub, *int32) {
	t.Helper()
	s := &downlinkStub{}
	var conns int32
	s.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&conns, 1)
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer conn.CloseNow()
		raw, err := json.Marshal(env)
		if err != nil {
			return
		}
		if err := conn.Write(r.Context(), websocket.MessageText, raw); err != nil {
			return
		}
		for {
			_, data, err := conn.Read(r.Context())
			if err != nil {
				return
			}
			var e signal.Envelope
			if json.Unmarshal(data, &e) != nil {
				continue
			}
			s.mu.Lock()
			s.received = append(s.received, e.Type)
			s.mu.Unlock()
		}
	}))
	t.Cleanup(s.Server.Close)
	return s, &conns
}

// TestPresenceUpdateForwards presence.update 解码后转给 OnPresenceUpdate（#11）；
// 缺 user_id 的坏包不回调。
func TestPresenceUpdateForwards(t *testing.T) {
	got := make(chan struct {
		uid    string
		online bool
	}, 2)
	stub, _ := newDownlinkStubData(t, signal.Envelope{
		Type: signal.TypePresenceUpdate, TS: 1,
		Data: json.RawMessage(`{"user_id":"42","online":true,"ts":1}`),
	})
	s := NewSession(NewAPIClient(stub.wsURL()), "tok", "n", Events{
		OnPresenceUpdate: func(userID string, online bool) {
			got <- struct {
				uid    string
				online bool
			}{userID, online}
		},
	}, nil)
	s.ws = signal.NewClient(signal.Config{URL: stub.wsURL(), ReconnectMin: 20 * time.Millisecond})
	defer s.ws.Close()
	s.registerHandlers()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go s.ws.Run(ctx)

	select {
	case d := <-got:
		if d.uid != "42" || !d.online {
			t.Fatalf("回调参数错误: %+v", d)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("未收到 OnPresenceUpdate")
	}
}

// TestServerShutdownStopsReconnect server.shutdown（#21）：回调一次后停掉自动
// 重连——桩保持在线，若未停应反复重连收到多条；计数 1 次即证明已停。
func TestServerShutdownStopsReconnect(t *testing.T) {
	got := make(chan string, 4)
	stub, conns := newDownlinkStubData(t, signal.Envelope{
		Type: signal.TypeServerShutdown, TS: 1,
		Data: json.RawMessage(`{"reason":"shutdown","ts":1}`),
	})
	s := NewSession(NewAPIClient(stub.wsURL()), "tok", "n", Events{
		OnServerShutdown: func(reason string) { got <- reason },
	}, nil)
	s.ws = signal.NewClient(signal.Config{
		URL:          stub.wsURL(),
		ReconnectMin: 20 * time.Millisecond,
		ReconnectMax: 50 * time.Millisecond,
	})
	defer s.ws.Close()
	s.registerHandlers()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- s.ws.Run(ctx) }()

	select {
	case reason := <-got:
		if reason != "shutdown" {
			t.Fatalf("reason = %q, want shutdown", reason)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("未收到 OnServerShutdown")
	}
	// Run 应随即返回（Close 停止重连），且连接计数不再增长。
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("server.shutdown 后 Run 未退出（重连未停）")
	}
	time.Sleep(150 * time.Millisecond)
	if n := atomic.LoadInt32(conns); n != 1 {
		t.Fatalf("应只拨号 1 次（已停止重连），实际 %d", n)
	}
	if len(got) != 0 {
		t.Fatalf("shutdown 只应上报一次，实际还有 %d 条", len(got))
	}
}

// TestRTCFailedRebuildsPeer rtc.failed（#19）：销毁本地 PC、清空远端链路、
// 重发 channel.join 重建（契约 0.2），并上报 OnRTCFailed。绝不发 channel.leave。
func TestRTCFailedRebuildsPeer(t *testing.T) {
	got := make(chan string, 2)
	stub, _ := newDownlinkStubData(t, signal.Envelope{
		Type: signal.TypeRTCFailed, TS: 1,
		Data: json.RawMessage(`{"channel_id":"room1","reason":"ice_failed"}`),
	})
	s := NewSession(NewAPIClient(stub.wsURL()), "tok", "n", Events{
		OnRTCFailed: func(channelID string) { got <- channelID },
	}, nil)
	s.ws = signal.NewClient(signal.Config{URL: stub.wsURL(), ReconnectMin: 20 * time.Millisecond})
	defer s.ws.Close()
	s.registerHandlers()
	s.roomID.Store("room1")
	s.remoteMgr = audio.NewRemoteManager(audio.NewMixer(func([]int16) int { return 0 }), nil)
	peer, err := rtc.NewPeer(rtc.PeerConfig{StreamID: "u1"})
	if err != nil {
		t.Fatalf("NewPeer: %v", err)
	}
	s.peerRef.Store(peer)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go s.ws.Run(ctx)

	select {
	case ch := <-got:
		if ch != "room1" {
			t.Fatalf("OnRTCFailed channelID = %q, want room1", ch)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("未收到 OnRTCFailed（#19）")
	}
	if p := s.peerRef.Load(); p != nil {
		t.Fatal("peerRef 应清空（channel.joined 建新 PC 的入口条件）")
	}
	if st := peer.ConnectionState(); st != webrtc.PeerConnectionStateClosed {
		t.Fatalf("旧 PC 应关闭，实际 %v", st)
	}
	if ids := s.remoteMgr.PeerIDs(); len(ids) != 0 {
		t.Fatalf("远端链路应清空，实际 %v", ids)
	}
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if stub.saw(signal.TypeChannelJoin) {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if !stub.saw(signal.TypeChannelJoin) {
		t.Fatal("必须重发 channel.join 重建入房（#19）")
	}
	if stub.saw(signal.TypeChannelLeave) {
		t.Fatal("重建不得发 channel.leave")
	}
}

// TestRTCFailedStaleIgnored 陈旧 rtc.failed（channel_id 非当前房）不得拆掉会话
// （#19）：误拆会把健康链路打断；已手动离开（roomID 空）同样忽略。
func TestRTCFailedStaleIgnored(t *testing.T) {
	got := make(chan string, 2)
	stub, _ := newDownlinkStubData(t, signal.Envelope{
		Type: signal.TypeRTCFailed, TS: 1,
		Data: json.RawMessage(`{"channel_id":"other-room","reason":"ice_failed"}`),
	})
	s := NewSession(NewAPIClient(stub.wsURL()), "tok", "n", Events{
		OnRTCFailed: func(channelID string) { got <- channelID },
	}, nil)
	s.ws = signal.NewClient(signal.Config{URL: stub.wsURL(), ReconnectMin: 20 * time.Millisecond})
	defer s.ws.Close()
	s.registerHandlers()
	s.roomID.Store("room1")
	peer, err := rtc.NewPeer(rtc.PeerConfig{StreamID: "u1"})
	if err != nil {
		t.Fatalf("NewPeer: %v", err)
	}
	s.peerRef.Store(peer)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go s.ws.Run(ctx)

	select {
	case ch := <-got:
		t.Fatalf("陈旧通知不应触发重建回调，收到 %q（#19）", ch)
	case <-time.After(300 * time.Millisecond):
	}
	if p := s.peerRef.Load(); p != peer {
		t.Fatal("陈旧 rtc.failed 不得拆掉当前 PC（#19）")
	}
	if stub.saw(signal.TypeChannelJoin) {
		t.Fatal("陈旧 rtc.failed 不得重发 channel.join（#19）")
	}
}
