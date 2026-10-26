package voice

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/OpenALC/LiteTalk/client/core/api"
	"github.com/OpenALC/LiteTalk/client/core/audio"
	"github.com/OpenALC/LiteTalk/client/core/rtc"
	"github.com/OpenALC/LiteTalk/client/core/signal"
	"github.com/coder/websocket"
	"github.com/pion/webrtc/v4"
)

// TestJoinChannelRollback 发送失败必须回滚 roomID——roomID 先存是为断线重连
// 恢复，失败路径不复位会让 OnConnect 重发 join 造成「幽灵入房」。
func TestJoinChannelRollback(t *testing.T) {
	s := NewSession(api.NewAPIClient("http://127.0.0.1:1"), "tok", "n", Events{}, nil)
	s.ws = signal.NewClient(signal.Config{URL: "ws://127.0.0.1:1/ws"})
	defer s.ws.Close()

	// ws 未 Run：Send 返回 ErrNotConnected，模拟发送失败路径。
	if err := s.JoinChannel("room1"); err == nil {
		t.Fatal("未连接时 JoinChannel 应返回错误")
	}
	if got := s.roomID.Load().(string); got != "" {
		t.Fatalf("发送失败后 roomID 应回滚为空, 得到 %q", got)
	}
	s.joinMu.Lock()
	pending := len(s.pendingJoins)
	s.joinMu.Unlock()
	if pending != 0 {
		t.Fatalf("发送失败后 pending join 应清空, 剩 %d", pending)
	}
}

// pendingJoinCount 测试读 pending join 队列（joinMu 保护）。
func (s *Session) pendingJoinCount() int {
	s.joinMu.Lock()
	defer s.joinMu.Unlock()
	return len(s.pendingJoins)
}

// TestJoinChannelErrorRollsBack 服务端异步拒绝进房（#91）：清理 pending、
// 回滚 roomID（InRoom=false，重连不再重发 join）、触发 OnJoinFailed，且同一次
// 失败不再走 chat:error，避免前端弹两条。
func TestJoinChannelErrorRollsBack(t *testing.T) {
	type failure struct{ channelID, code, message string }
	failed := make(chan failure, 1)
	stub := newJoinReplyStub(t, func(string) []signal.Envelope {
		return []signal.Envelope{{
			Type: signal.TypeError, TS: 1,
			Data: json.RawMessage(`{"code":"forbidden","message":"无权进入该频道"}`),
		}}
	})
	var chatErrs atomic.Int32
	s := NewSession(api.NewAPIClient(stub.wsURL()), "tok", "n", Events{
		OnJoinFailed: func(channelID, code, message string) {
			failed <- failure{channelID, code, message}
		},
	}, nil)
	s.ws = signal.NewClient(signal.Config{URL: stub.wsURL(), ReconnectMin: 20 * time.Millisecond})
	defer s.ws.Close()
	s.registerHandlers()
	s.ErrorHandlerEx(func(code, message string, joinHandled bool) {
		if !joinHandled {
			chatErrs.Add(1)
		}
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go s.ws.Run(ctx)
	waitSignalConnected(t, s)

	if err := s.JoinChannel("room1"); err != nil {
		t.Fatalf("JoinChannel: %v", err)
	}
	select {
	case got := <-failed:
		if got.channelID != "room1" || got.code != "forbidden" || got.message != "无权进入该频道" {
			t.Fatalf("OnJoinFailed = %+v, want room1/forbidden/无权进入该频道", got)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("进房被拒未触发 OnJoinFailed（#91）")
	}
	if s.InRoom() {
		t.Fatal("进房失败后 InRoom 应为 false（重连不得再重发 join）")
	}
	if n := s.pendingJoinCount(); n != 0 {
		t.Fatalf("进房失败后 pending join 应清空, 剩 %d", n)
	}
	if n := chatErrs.Load(); n != 0 {
		t.Fatalf("进房失败不应再走 chat:error, 收到 %d 条", n)
	}
}

// TestJoinChannelJoinedKeepsRoom channel.joined 结算 pending 并保留房间；
// 之后到达的同码 error（例如重复进房的 already_in_channel / 聊天 forbidden）
// 没有 pending 可归属，不得回滚房间、不得触发 OnJoinFailed。
func TestJoinChannelJoinedKeepsRoom(t *testing.T) {
	failed := make(chan struct{}, 1)
	stub := newJoinReplyStub(t, func(channelID string) []signal.Envelope {
		joined := signal.Envelope{
			Type: signal.TypeChannelJoined, TS: 1,
			Data: json.RawMessage(`{"channel_id":"` + channelID + `","members":[],"self":{"user_id":"u1","nickname":"甲"}}`),
		}
		laterErr := signal.Envelope{
			Type: signal.TypeError, TS: 2,
			Data: json.RawMessage(`{"code":"forbidden","message":"无权进入该频道"}`),
		}
		return []signal.Envelope{joined, laterErr}
	})
	errSeen := make(chan bool, 1)
	s := NewSession(api.NewAPIClient(stub.wsURL()), "tok", "n", Events{
		OnJoinFailed: func(channelID, code, message string) { failed <- struct{}{} },
	}, nil)
	s.ws = signal.NewClient(signal.Config{URL: stub.wsURL(), ReconnectMin: 20 * time.Millisecond})
	defer s.ws.Close()
	s.registerHandlers()
	s.ErrorHandlerEx(func(code, message string, joinHandled bool) { errSeen <- joinHandled })
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go s.ws.Run(ctx)
	waitSignalConnected(t, s)

	if err := s.JoinChannel("room1"); err != nil {
		t.Fatalf("JoinChannel: %v", err)
	}
	// 后一条 error 到达时 pending 应已被 channel.joined 结算。
	select {
	case joinHandled := <-errSeen:
		if joinHandled {
			t.Fatal("channel.joined 已结算后，后续 error 不应再归因进房")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("未收到后续 error 帧")
	}
	if !s.InRoom() {
		t.Fatal("channel.joined 后房间应保留")
	}
	if got := s.roomID.Load().(string); got != "room1" {
		t.Fatalf("roomID = %q, want room1", got)
	}
	if n := s.pendingJoinCount(); n != 0 {
		t.Fatalf("channel.joined 后 pending join 应清空, 剩 %d", n)
	}
	select {
	case <-failed:
		t.Fatal("channel.joined 后不应触发 OnJoinFailed")
	default:
	}
}

// TestJoinErrorWithoutPendingIsChatError 普通聊天错误（无 pending join）不得
// 触发进房回滚回调，仍按聊天错误转发（#91：不要误伤聊天）。
func TestJoinErrorWithoutPendingIsChatError(t *testing.T) {
	stub, _ := newDownlinkStubData(t, signal.Envelope{
		Type: signal.TypeError, TS: 1,
		Data: json.RawMessage(`{"code":"forbidden","message":"无权向该频道发送消息"}`),
	})
	errSeen := make(chan bool, 1)
	failed := make(chan struct{}, 1)
	s := NewSession(api.NewAPIClient(stub.wsURL()), "tok", "n", Events{
		OnJoinFailed: func(channelID, code, message string) { failed <- struct{}{} },
	}, nil)
	s.ws = signal.NewClient(signal.Config{URL: stub.wsURL(), ReconnectMin: 20 * time.Millisecond})
	defer s.ws.Close()
	s.registerHandlers()
	s.ErrorHandlerEx(func(code, message string, joinHandled bool) { errSeen <- joinHandled })
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go s.ws.Run(ctx)

	select {
	case joinHandled := <-errSeen:
		if joinHandled {
			t.Fatal("无 pending join 时 forbidden 应按聊天错误转发")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("未收到聊天错误回调")
	}
	select {
	case <-failed:
		t.Fatal("普通聊天错误不得触发 OnJoinFailed")
	default:
	}
}

// TestQuotaExceededWithoutPendingIsChatError quota_exceeded 不是进房专用码：
// rtc.offer 的 SFU 容量上限（hub.handleRTCOffer）也会返回。无 pending join 时
// 必须继续走聊天错误回调，不能被 join-only 静默丢弃。
func TestQuotaExceededWithoutPendingIsChatError(t *testing.T) {
	stub, _ := newDownlinkStubData(t, signal.Envelope{
		Type: signal.TypeError, TS: 1,
		Data: json.RawMessage(`{"code":"quota_exceeded","message":"RTC 协商失败: 房间已满"}`),
	})
	type seenErr struct {
		code, message string
		joinHandled   bool
	}
	got := make(chan seenErr, 1)
	failed := make(chan struct{}, 1)
	s := NewSession(api.NewAPIClient(stub.wsURL()), "tok", "n", Events{
		OnJoinFailed: func(channelID, code, message string) { failed <- struct{}{} },
	}, nil)
	s.ws = signal.NewClient(signal.Config{URL: stub.wsURL(), ReconnectMin: 20 * time.Millisecond})
	defer s.ws.Close()
	s.registerHandlers()
	s.ErrorHandlerEx(func(code, message string, joinHandled bool) {
		got <- seenErr{code, message, joinHandled}
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go s.ws.Run(ctx)

	select {
	case e := <-got:
		if e.joinHandled {
			t.Fatal("无 pending join 时 quota_exceeded 不应归因进房")
		}
		if e.code != "quota_exceeded" || !strings.Contains(e.message, "RTC 协商失败") {
			t.Fatalf("错误回调 = %+v, want quota_exceeded/RTC 协商失败", e)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("无 pending 时 quota_exceeded 必须走聊天错误回调（不得静默丢弃）")
	}
	select {
	case <-failed:
		t.Fatal("rtc.offer 配额错误不得触发 OnJoinFailed")
	default:
	}
}

// TestJoinChannelQuotaExceededRollsBack 有 pending join 时 quota_exceeded
// （如房间满员）仍按进房失败回滚房间态并上报 OnJoinFailed。
func TestJoinChannelQuotaExceededRollsBack(t *testing.T) {
	failed := make(chan [3]string, 1)
	stub := newJoinReplyStub(t, func(string) []signal.Envelope {
		return []signal.Envelope{{
			Type: signal.TypeError, TS: 1,
			Data: json.RawMessage(`{"code":"quota_exceeded","message":"语音房间已满"}`),
		}}
	})
	s := NewSession(api.NewAPIClient(stub.wsURL()), "tok", "n", Events{
		OnJoinFailed: func(channelID, code, message string) {
			failed <- [3]string{channelID, code, message}
		},
	}, nil)
	s.ws = signal.NewClient(signal.Config{URL: stub.wsURL(), ReconnectMin: 20 * time.Millisecond})
	defer s.ws.Close()
	s.registerHandlers()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go s.ws.Run(ctx)
	waitSignalConnected(t, s)

	if err := s.JoinChannel("room1"); err != nil {
		t.Fatalf("JoinChannel: %v", err)
	}
	select {
	case got := <-failed:
		if got != [3]string{"room1", "quota_exceeded", "语音房间已满"} {
			t.Fatalf("OnJoinFailed = %v", got)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("满员拒绝未触发 OnJoinFailed（#91）")
	}
	if s.InRoom() {
		t.Fatal("满员拒绝后 InRoom 应为 false")
	}
	if n := s.pendingJoinCount(); n != 0 {
		t.Fatalf("满员拒绝后 pending join 应清空, 剩 %d", n)
	}
}

// TestJoinRollbackDoesNotClobberNewerJoin 快速切房/连点：旧 join 的发送失败或
// error 只结算自己那条，不能清掉更新的房间意图（#91 并发约束）。
func TestJoinRollbackDoesNotClobberNewerJoin(t *testing.T) {
	s := NewSession(api.NewAPIClient("http://127.0.0.1:1"), "tok", "n", Events{}, nil)
	s.ws = signal.NewClient(signal.Config{URL: "ws://127.0.0.1:1/ws"})
	defer s.ws.Close()

	seqA := s.beginJoin("roomA")
	_ = s.beginJoin("roomB")
	s.abortJoinSend(seqA, true)
	if got := s.roomID.Load().(string); got != "roomB" {
		t.Fatalf("旧 join 发送失败不得清掉新房, roomID = %q, want roomB", got)
	}
	if n := s.pendingJoinCount(); n != 1 {
		t.Fatalf("只应摘除失败的那条, 剩 %d 条", n)
	}

	// 旧 join 的 error 按 FIFO 归属到 roomA：roomID 已是 roomB，不回滚。
	s.joinMu.Lock()
	s.pendingJoins = nil
	s.joinMu.Unlock()
	s.roomID.Store("")
	s.beginJoin("roomA")
	s.beginJoin("roomB")
	channelID, consumed, rolledBack := s.settlePendingJoin(true)
	if channelID != "roomA" || !consumed || rolledBack {
		t.Fatalf("旧 join 错误归属 = (%q, %v, %v), want (roomA, true, false)", channelID, consumed, rolledBack)
	}
	if got := s.roomID.Load().(string); got != "roomB" {
		t.Fatalf("旧 join 失败不得回滚新房, roomID = %q, want roomB", got)
	}
	// 新房自己的失败才回滚。
	channelID, consumed, rolledBack = s.settlePendingJoin(true)
	if channelID != "roomB" || !consumed || !rolledBack {
		t.Fatalf("新房失败归属 = (%q, %v, %v), want (roomB, true, true)", channelID, consumed, rolledBack)
	}
	if s.InRoom() {
		t.Fatal("当前房间进房失败后 InRoom 应为 false")
	}
}

// TestConnectUsesConfiguredAudioBackend Session.Connect 必须使用 SetAudioBackend
// 在 Connect 前选定的后端（#79）：CLI 等无界面消费方靠 BackendNull 无声卡运行。
// Connect 后改后端必须报错——管线已按旧选择装配，运行期换后端不支持。
func TestConnectUsesConfiguredAudioBackend(t *testing.T) {
	stub := newDownlinkStub(t, signal.TypeHello)
	s := NewSession(api.NewAPIClient(stub.wsURL()), "tok", "n", Events{}, nil)
	if err := s.SetAudioBackend(audio.BackendNull); err != nil {
		t.Fatalf("SetAudioBackend: %v", err)
	}
	s.Connect(stub.wsURL())
	defer s.Close()

	if s.pipe == nil {
		t.Fatal("BackendNull 下管线应构造成功")
	}
	if got := s.pipe.Backend(); got != audio.BackendNull {
		t.Fatalf("管线后端 = %v, want BackendNull", got)
	}
	if err := s.SetAudioBackend(audio.BackendDevice); err == nil {
		t.Fatal("Connect 之后 SetAudioBackend 应返回错误")
	}
	if got := s.AudioBackend(); got != audio.BackendNull {
		t.Fatalf("Connect 后后端不得改变, 得到 %v", got)
	}
}

// TestConnectDefaultsToDeviceBackend 桌面端默认行为不变：未显式选择时
// Connect 仍按 BackendDevice 装配。
func TestConnectDefaultsToDeviceBackend(t *testing.T) {
	stub := newDownlinkStub(t, signal.TypeHello)
	s := NewSession(api.NewAPIClient(stub.wsURL()), "tok", "n", Events{}, nil)
	if got := s.AudioBackend(); got != audio.BackendDevice {
		t.Fatalf("默认后端 = %v, want BackendDevice", got)
	}
	s.Connect(stub.wsURL())
	defer s.Close()

	if s.pipe == nil {
		t.Fatal("BackendDevice 下管线应构造成功")
	}
	if got := s.pipe.Backend(); got != audio.BackendDevice {
		t.Fatalf("管线后端 = %v, want BackendDevice", got)
	}
}

// TestSetAudioBackendRejectsUnknown 非法后端值直接拒绝，不能悄悄落到设备后端。
func TestSetAudioBackendRejectsUnknown(t *testing.T) {
	s := NewSession(api.NewAPIClient("http://127.0.0.1:1"), "tok", "n", Events{}, nil)
	if err := s.SetAudioBackend(audio.Backend(9)); err == nil {
		t.Fatal("非法后端值应返回错误")
	}
	if got := s.AudioBackend(); got != audio.BackendDevice {
		t.Fatalf("非法值不得改变后端, 得到 %v", got)
	}
}

// newNullPipeline 构造无声卡自检管线（不启动），测试用于代替真实设备。
func newNullPipeline(t *testing.T) *audio.Pipeline {
	t.Helper()
	cfg := audio.DefaultPipelineConfig()
	cfg.Backend = audio.BackendNull
	// 会话测试不验证音质：绕过 APM，避免 AEC3 初始化开销与 AGC2 对电平断言的干扰。
	cfg.DSP.DisableAPM = true
	p, err := audio.NewPipeline(cfg)
	if err != nil {
		t.Fatalf("NewPipeline: %v", err)
	}
	t.Cleanup(func() { _ = p.Close() })
	return p
}

// TestStartMediaFailureReportsVoiceUnavailable startMedia 的 mixer/pipeline
// 启动失败必须经 OnVoiceUnavailable 上报（#80）：此前只打日志，UI 仍显示在
// 房内、麦克风没起来，用户与消费方都不可观测。
// 同一故障周期只上报一次；启动成功复位，后续新故障可再次上报。
func TestStartMediaFailureReportsVoiceUnavailable(t *testing.T) {
	newSession := func(msgs chan<- string) *Session {
		s := NewSession(api.NewAPIClient("http://127.0.0.1:1"), "tok", "n",
			Events{OnVoiceUnavailable: func(msg string) { msgs <- msg }}, nil)
		s.mixer = audio.NewMixer(func([]int16) int { return 0 })
		return s
	}
	expectMsg := func(t *testing.T, msgs <-chan string, want string) {
		t.Helper()
		select {
		case got := <-msgs:
			if got != want {
				t.Fatalf("提示 = %q, want %q", got, want)
			}
		default:
			t.Fatalf("未上报 OnVoiceUnavailable（want %q）", want)
		}
	}
	expectSilent := func(t *testing.T, msgs <-chan string) {
		t.Helper()
		select {
		case got := <-msgs:
			t.Fatalf("同一故障周期不应重复上报，收到 %q", got)
		default:
		}
	}

	t.Run("mixer", func(t *testing.T) {
		msgs := make(chan string, 4)
		s := newSession(msgs)
		s.pipe = newNullPipeline(t)
		// 预先启动 mixer，使 startMedia 内的 Start 返回 already started。
		if err := s.mixer.Start(); err != nil {
			t.Fatalf("预热 mixer: %v", err)
		}
		s.startMedia()
		expectMsg(t, msgs, "音频播放启动失败，请检查扬声器")
		if s.mediaStarted.Load() {
			t.Fatal("启动失败后 mediaStarted 应为 false（允许重试）")
		}
		s.startMedia()
		expectSilent(t, msgs)
		s.mixer.Stop()
	})

	t.Run("pipeline", func(t *testing.T) {
		msgs := make(chan string, 4)
		s := newSession(msgs)
		closed := newNullPipeline(t)
		_ = closed.Close() // Start 必然失败，且错误文本含内部细节
		s.pipe = closed

		s.startMedia()
		expectMsg(t, msgs, "音频设备启动失败，请检查麦克风和扬声器")
		if s.mediaStarted.Load() {
			t.Fatal("启动失败后 mediaStarted 应为 false（允许重试）")
		}
		s.startMedia()
		expectSilent(t, msgs)

		// 换成可用管线启动成功：去重标记复位，后续新故障应再次提示。
		s.pipe = newNullPipeline(t)
		s.startMedia()
		if !s.mediaStarted.Load() {
			t.Fatal("可用管线应启动成功")
		}
		s.stopMedia()
		closed2 := newNullPipeline(t)
		_ = closed2.Close()
		s.pipe = closed2
		s.startMedia()
		expectMsg(t, msgs, "音频设备启动失败，请检查麦克风和扬声器")
	})
}

// TestJoinChannelVoiceUnavailable 媒体管线初始化失败（如无 Opus 的平台）时
// JoinChannel 必须给出明确错误供前端 toast，而不是静默成功或 panic。
func TestJoinChannelVoiceUnavailable(t *testing.T) {
	s := NewSession(api.NewAPIClient("http://127.0.0.1:1"), "tok", "n", Events{}, nil)
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
	s := NewSession(api.NewAPIClient("http://127.0.0.1:1"), "tok", "n",
		Events{OnKicked: func(reason string) { got = reason }}, nil)
	s.handleKicked("login_elsewhere")
	if got != "login_elsewhere" {
		t.Fatalf("应原样透传被踢原因, 得到 %q", got)
	}

	// 零值 Events：无回调不得 panic。
	quiet := NewSession(api.NewAPIClient("http://127.0.0.1:1"), "tok", "n", Events{}, nil)
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

// joinReplyStub 收到 channel.join 后按 reply 返回一串下行帧（可含
// channel.joined 与 error），用于服务端异步回执/拒绝的进房用例（#91）。
type joinReplyStub struct {
	*httptest.Server

	mu       sync.Mutex
	received []string
}

func newJoinReplyStub(t *testing.T, reply func(channelID string) []signal.Envelope) *joinReplyStub {
	t.Helper()
	s := &joinReplyStub{}
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
			var d signal.ChannelJoinData
			if json.Unmarshal(env.Data, &d) != nil {
				continue
			}
			for _, out := range reply(d.ChannelID) {
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

func (s *joinReplyStub) wsURL() string {
	return "ws" + strings.TrimPrefix(s.URL, "http") + "/ws"
}

// TestVoiceTakenLeavesRoomLocally session.voice_taken：服务端已把本端摘出成员表，
// 本地必须跟着退房（roomID/成员/媒体链路全清），且绝不发 channel.leave——那条
// leave 按 uid 处理，会把抢占端一起带离房间。成员表不清就是「幽灵在房」。
func TestVoiceTakenLeavesRoomLocally(t *testing.T) {
	stub := newDownlinkStub(t, signal.TypeSessionVoiceTaken)

	taken := make(chan struct{}, 1)
	s := NewSession(api.NewAPIClient(stub.wsURL()), "tok", "n",
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
	s.states = audio.NewStateTable()
	s.states.Set("u1", audio.PeerVoiceState{Speaking: true, Mute: true})
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
	if n := len(s.states.Snapshot()); n != 0 {
		t.Fatalf("states = %d, want 0", n)
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

// TestLeaveChannelClearsRoomState 主动退房即使 leave 发送失败，也必须清成员表与
// 状态表——否则同一会话内历史成员累积、UI 残留幽灵成员/旧说话态。
func TestLeaveChannelClearsRoomState(t *testing.T) {
	s := NewSession(api.NewAPIClient("http://127.0.0.1:1"), "tok", "n", Events{}, nil)
	s.ws = signal.NewClient(signal.Config{URL: "ws://127.0.0.1:1/ws"})
	defer s.ws.Close()

	s.roomID.Store("room1")
	s.mu.Lock()
	s.members = map[string]PeerView{"u1": {UserID: "u1", Nickname: "甲"}}
	s.mu.Unlock()
	s.states = audio.NewStateTable()
	s.states.Set("u1", audio.PeerVoiceState{Speaking: true, Mute: true})

	// ws 未 Run：Send 返回 ErrNotConnected，覆盖发送失败路径。
	if err := s.LeaveChannel(); err == nil {
		t.Fatal("未连接的 LeaveChannel 应返回发送错误")
	}
	if got := s.roomID.Load().(string); got != "" {
		t.Fatalf("roomID = %q, want empty", got)
	}
	if n := len(s.Members()); n != 0 {
		t.Fatalf("members = %d, want 0", n)
	}
	if n := len(s.states.Snapshot()); n != 0 {
		t.Fatalf("states = %d, want 0", n)
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
	s := NewSession(api.NewAPIClient(srv.URL), "tok", "n", Events{}, nil)

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
	s2 := NewSession(api.NewAPIClient("http://127.0.0.1:1"), "tok", "n", Events{}, nil)
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

	old := api.RTCredentialsTimeout
	api.RTCredentialsTimeout = 100 * time.Millisecond
	defer func() { api.RTCredentialsTimeout = old }()

	s := NewSession(api.NewAPIClient(srv.URL), "tok", "n", Events{}, nil)
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
	s := NewSession(api.NewAPIClient(srv.URL), "tok", "n", Events{}, nil)
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
	s2 := NewSession(api.NewAPIClient(fail.URL), "tok", "n", Events{}, nil)
	if got2 := s2.fetchICEServers(); got2 != nil {
		t.Fatalf("失败时应降级为空 ICE 列表，实际 %v", got2)
	}

	// 空数组也是合法的（dev 无 TURN）：不报错，返回 nil。
	empty := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"iceServers":[],"expiresAt":0}`))
	}))
	defer empty.Close()
	s3 := NewSession(api.NewAPIClient(empty.URL), "tok", "n", Events{}, nil)
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

// TestPresenceUpdateForwards presence.update 解码后转给 OnPresenceUpdate（#11/#92）；
// 频道快照字段透传；缺 user_id 的坏包不回调。
func TestPresenceUpdateForwards(t *testing.T) {
	got := make(chan struct {
		uid, ch, sid string
		online       bool
	}, 2)
	stub, _ := newDownlinkStubData(t, signal.Envelope{
		Type: signal.TypePresenceUpdate, TS: 1,
		Data: json.RawMessage(`{"user_id":"42","online":true,"ts":1,"channel_id":"700","server_id":"800"}`),
	})
	s := NewSession(api.NewAPIClient(stub.wsURL()), "tok", "n", Events{
		OnPresenceUpdate: func(userID string, online bool, channelID, serverID string) {
			got <- struct {
				uid, ch, sid string
				online       bool
			}{userID, channelID, serverID, online}
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
		if d.uid != "42" || !d.online || d.ch != "700" || d.sid != "800" {
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
	s := NewSession(api.NewAPIClient(stub.wsURL()), "tok", "n", Events{
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
	s := NewSession(api.NewAPIClient(stub.wsURL()), "tok", "n", Events{
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
	s := NewSession(api.NewAPIClient(stub.wsURL()), "tok", "n", Events{
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

// TestServerHelloRecordsVersion server.hello（#29）：协商结果记入 Session，
// 供版本相关行为（如 v2 停发 voice.state{speaking}）门控。
func TestServerHelloRecordsVersion(t *testing.T) {
	stub, _ := newDownlinkStubData(t, signal.Envelope{
		Type: signal.TypeServerHello, TS: 1,
		Data: json.RawMessage(`{"version":2}`),
	})
	s := NewSession(api.NewAPIClient(stub.wsURL()), "tok", "n", Events{}, nil)
	s.ws = signal.NewClient(signal.Config{URL: stub.wsURL(), ReconnectMin: 20 * time.Millisecond})
	defer s.ws.Close()
	s.registerHandlers()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go s.ws.Run(ctx)

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) && s.ProtocolVersion() != 2 {
		time.Sleep(10 * time.Millisecond)
	}
	if s.ProtocolVersion() != 2 {
		t.Fatalf("ProtocolVersion = %d, want 2（server.hello 未记录）", s.ProtocolVersion())
	}
}

// ---- gateway.resume 续传集成（#32）----

// gatewayStub 续传集成桩：每条连接发 server.hello{2,S1}，记录上行信封
// （含载荷），测试可向当前连接推送下行信封；CloseConn 模拟断线。
type gatewayStub struct {
	*httptest.Server
	mu    sync.Mutex
	cur   *websocket.Conn
	up    []signal.Envelope
	conns int
}

func newGatewayStub(t *testing.T) *gatewayStub {
	t.Helper()
	s := &gatewayStub{}
	s.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		s.mu.Lock()
		s.cur = conn
		s.conns++
		s.mu.Unlock()
		hello := signal.Envelope{Type: signal.TypeServerHello, TS: 1,
			Data: json.RawMessage(`{"version":2,"session_id":"S1"}`)}
		raw, _ := json.Marshal(hello)
		_ = conn.Write(r.Context(), websocket.MessageText, raw)
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
			s.up = append(s.up, e)
			s.mu.Unlock()
		}
	}))
	t.Cleanup(s.Server.Close)
	return s
}

func (s *gatewayStub) wsURL() string {
	return "ws" + strings.TrimPrefix(s.URL, "http") + "/ws"
}

// push 向当前连接推一条下行信封（seq>0 即事件面）。
func (s *gatewayStub) push(t *testing.T, seq int64, msgID string) {
	t.Helper()
	env := signal.Envelope{Type: signal.TypeChatMessage, TS: seq, Seq: seq,
		Data: json.RawMessage(`{"id":"` + msgID + `","channel_id":"1","content":"c","ts":1,"user":{"id":"2","nickname":"b"}}`)}
	raw, err := json.Marshal(env)
	if err != nil {
		t.Fatal(err)
	}
	s.mu.Lock()
	conn := s.cur
	s.mu.Unlock()
	if conn == nil {
		t.Fatal("无连接可推送")
	}
	if err := conn.Write(context.Background(), websocket.MessageText, raw); err != nil {
		t.Fatalf("push: %v", err)
	}
}

func (s *gatewayStub) closeConn() {
	s.mu.Lock()
	conn := s.cur
	s.cur = nil
	s.mu.Unlock()
	if conn != nil {
		_ = conn.CloseNow()
	}
}

// resumeSeq 最近一次 gateway.resume 的 last_seq（跨越所有连接，取最新一条）。
// 首连会发 resume{0}（#81），所以判定重连不能只看「有没有值」，必须看具体 seq。
func (s *gatewayStub) resumeSeq() (int64, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := len(s.up) - 1; i >= 0; i-- {
		e := s.up[i]
		if e.Type != signal.TypeGatewayResume {
			continue
		}
		var d signal.GatewayResumeData
		if json.Unmarshal(e.Data, &d) == nil {
			return d.LastSeq, true
		}
	}
	return 0, false
}

// TestGatewayResumeAfterReconnect 断线重连后事件不丢不重（#32/#81 验收）：
// 首连发 resume{S1,0}（服务端环可能已有事件），收到 seq 1..2 后断线；
// 重连经 server.hello 确认同会话后发 gateway.resume{S1,2}，补发的 seq 3 与
// 实时流续接，ChatHandler 逐条各收到一次。
func TestGatewayResumeAfterReconnect(t *testing.T) {
	stub := newGatewayStub(t)
	var mu sync.Mutex
	got := map[string]int{}
	s := NewSession(api.NewAPIClient(stub.wsURL()), "tok", "n", Events{}, nil)
	s.ws = signal.NewClient(signal.Config{
		URL:          stub.wsURL(),
		ReconnectMin: 20 * time.Millisecond,
		Gate:         s.gw.Gate,
	})
	s.ChatHandler(func(channelID, msgID, userID, nickname, content, ts, _ string) {
		mu.Lock()
		got[msgID]++
		mu.Unlock()
	})
	s.registerHandlers()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go s.ws.Run(ctx)
	defer s.ws.Close()

	// 等首连建立（Run 异步拨号），否则 push 无连接可写。
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		stub.mu.Lock()
		ready := stub.cur != nil
		stub.mu.Unlock()
		if ready {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	// 首连也必须发 resume{last_seq:0}（#81）：复用旧 sid 时服务端环里可能已有
	// seq>1 的事件，不补发就会永久卡住。resumeSeq 取最新一条，首连 0 与重连 2
	// 不会串——下面的重连断言会等具体的 2。
	firstDeadline := time.Now().Add(5 * time.Second)
	var firstSeq int64
	var firstOK bool
	for time.Now().Before(firstDeadline) {
		if firstSeq, firstOK = stub.resumeSeq(); firstOK && firstSeq == 0 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if !firstOK || firstSeq != 0 {
		t.Fatalf("首连 resume.last_seq = %d (ok=%v), want 0", firstSeq, firstOK)
	}
	// 随后收 seq 1..2。
	stub.push(t, 1, "m1")
	stub.push(t, 2, "m2")
	waitForMsg := func(id string, n int) {
		t.Helper()
		deadline := time.Now().Add(5 * time.Second)
		for time.Now().Before(deadline) {
			mu.Lock()
			c := got[id]
			mu.Unlock()
			if c >= n {
				return
			}
			time.Sleep(10 * time.Millisecond)
		}
		mu.Lock()
		snapshot := fmt.Sprintf("%v", got)
		c := got[id]
		mu.Unlock()
		t.Fatalf("消息 %s 未及时到达（%d/%d）: %s", id, c, n, snapshot)
	}
	waitForMsg("m1", 1)
	waitForMsg("m2", 1)
	if s.gw.LastSeq() != 2 {
		t.Fatalf("LastSeq = %d, want 2", s.gw.LastSeq())
	}
	if seq, ok := stub.resumeSeq(); !ok || seq != 0 {
		t.Fatalf("首连 resume = (%d,%v), want (0,true)", seq, ok)
	}

	// 断线重连：新连接的 server.hello 确认同会话 → 必须发 resume{last_seq=2}。
	// 首连的 0 已存在，不能以「resumeSeq 有值」为条件，必须等具体 seq=2。
	stub.closeConn()
	reconnectDeadline := time.Now().Add(5 * time.Second)
	var last int64
	var ok bool
	for time.Now().Before(reconnectDeadline) {
		if last, ok = stub.resumeSeq(); ok && last == 2 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if !ok || last != 2 {
		stub.mu.Lock()
		diag := fmt.Sprintf("conns=%d up=%d cur=%v", stub.conns, len(stub.up), stub.cur != nil)
		stub.mu.Unlock()
		t.Fatalf("重连后 resume.last_seq = %d (ok=%v), want 2（%s）", last, ok, diag)
	}
	// 补发 seq 3 + 重复 seq 2（补发与实时重叠）：只各投一次。
	stub.push(t, 2, "m2")
	stub.push(t, 3, "m3")
	waitForMsg("m3", 1)
	time.Sleep(50 * time.Millisecond) // 留出重复投递的窗口（应被 seq 去重）
	mu.Lock()
	defer mu.Unlock()
	for id, n := range map[string]int{"m1": 1, "m2": 1, "m3": 1} {
		if got[id] != n {
			t.Fatalf("消息 %s 收到 %d 次, want %d（全量: %v）", id, got[id], n, got)
		}
	}
}

// TestStatsAudioDiagnostics voice:stats 音频诊断字段（#95）：播放缓冲时长
// 可观测；无下行 peer 时省略 peers，旧前端不受影响。
func TestStatsAudioDiagnostics(t *testing.T) {
	s := NewSession(api.NewAPIClient("http://127.0.0.1:1"), "tok", "n", Events{}, nil)
	s.pipe = newNullPipeline(t)
	s.pipe.WritePlayback(make([]int16, audio.SampleRate/50)) // 20ms

	raw := s.Stats()
	var vs VoiceStats
	if err := json.Unmarshal([]byte(raw), &vs); err != nil {
		t.Fatalf("Stats JSON: %v (%s)", err, raw)
	}
	if vs.PlaybackBufferedMS != 20 {
		t.Fatalf("PlaybackBufferedMS = %d, want 20 (%s)", vs.PlaybackBufferedMS, raw)
	}
	if strings.Contains(raw, "\"peers\"") {
		t.Fatalf("empty peers should be omitted: %s", raw)
	}
}

// ---- 迟到 channel.joined（#99）----

// lateJoinStub 可控下行桩：记录上行消息类型，测试可随时 push 任意下行信封
// （#99 需要在 join/leave 之后精确注入迟到的 channel.joined）。
type lateJoinStub struct {
	*httptest.Server
	mu       sync.Mutex
	cur      *websocket.Conn
	received []string
}

func newLateJoinStub(t *testing.T) *lateJoinStub {
	t.Helper()
	s := &lateJoinStub{}
	s.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer conn.CloseNow()
		s.mu.Lock()
		s.cur = conn
		s.mu.Unlock()
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
		}
	}))
	t.Cleanup(s.Server.Close)
	return s
}

func (s *lateJoinStub) wsURL() string {
	return "ws" + strings.TrimPrefix(s.URL, "http") + "/ws"
}

func (s *lateJoinStub) push(t *testing.T, env signal.Envelope) {
	t.Helper()
	raw, err := json.Marshal(env)
	if err != nil {
		t.Fatalf("marshal push: %v", err)
	}
	s.mu.Lock()
	conn := s.cur
	s.mu.Unlock()
	if conn == nil {
		t.Fatal("无连接可推送")
	}
	if err := conn.Write(context.Background(), websocket.MessageText, raw); err != nil {
		t.Fatalf("push: %v", err)
	}
}

func (s *lateJoinStub) saw(msgType string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, m := range s.received {
		if m == msgType {
			return true
		}
	}
	return false
}

// joinedEnv 构造一条 channel.joined 下行帧。memberIDs 含自己（服务端全量）。
func joinedEnv(t *testing.T, channelID, selfID string, memberIDs ...string) signal.Envelope {
	t.Helper()
	d := signal.ChannelJoinedData{
		ChannelID: channelID,
		Self:      signal.MemberInfo{UserID: selfID, Nickname: selfID},
	}
	for _, id := range memberIDs {
		d.Members = append(d.Members, signal.MemberInfo{UserID: id, Nickname: id})
	}
	raw, err := json.Marshal(d)
	if err != nil {
		t.Fatalf("marshal joined: %v", err)
	}
	return signal.Envelope{Type: signal.TypeChannelJoined, TS: 1, Data: raw}
}

func waitUntil(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("%s: 5s 内未满足条件", what)
}

// pushServerHelloV2 在同一条连接上推 server.hello 作为处理屏障：WS 读循环
// 保序，收到它即证明之前注入的帧已被处理（迟到的 joined 本身没有可观测副作用）。
func pushServerHelloV2(t *testing.T, stub *lateJoinStub, s *Session) {
	t.Helper()
	stub.push(t, signal.Envelope{
		Type: signal.TypeServerHello, TS: 1,
		Data: json.RawMessage(`{"version":2,"session_id":"S1"}`),
	})
	waitUntil(t, "server.hello 屏障", func() bool { return s.ProtocolVersion() == 2 })
}

// newJoinedSession 装配可探测媒体启动的会话：BackendNull 管线真能 Start，
// 误处理迟到 joined 会把 mediaStarted 置 true。
func newJoinedSession(t *testing.T, stub *lateJoinStub, ev Events) *Session {
	t.Helper()
	s := NewSession(api.NewAPIClient(stub.wsURL()), "tok", "n", ev, nil)
	s.ws = signal.NewClient(signal.Config{URL: stub.wsURL(), ReconnectMin: 20 * time.Millisecond})
	t.Cleanup(s.ws.Close)
	s.registerHandlers()
	s.pipe = newNullPipeline(t)
	s.mixer = audio.NewMixer(func([]int16) int { return 0 })
	// remoteMgr 非 nil 才会真正 setupPeer：快速切房用例据 peerRef 断言旧房
	// 迟到的 joined 没有触发 peer 重协商。
	s.remoteMgr = audio.NewRemoteManager(audio.NewMixer(func([]int16) int { return 0 }), nil)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go s.ws.Run(ctx)
	waitSignalConnected(t, s)
	return s
}

// TestLateChannelJoinedAfterLeaveIgnored 退房后迟到的 channel.joined 必须整帧
// 丢弃（#99）：不得把 InRoom 拉回 true、不得重建成员表、不得重启媒体。
func TestLateChannelJoinedAfterLeaveIgnored(t *testing.T) {
	stub := newLateJoinStub(t)
	selfs := make(chan string, 4)
	peerJoins := make(chan string, 8)
	var activity atomic.Int32 // OnSelf/OnPeerJoin 任一回调
	s := newJoinedSession(t, stub, Events{
		OnSelf: func(id string) {
			activity.Add(1)
			selfs <- id
		},
		OnPeerJoin: func(uid, nickname, tag string, speaking, mute, deafen, priority bool) {
			activity.Add(1)
			peerJoins <- uid
		},
	})

	if err := s.JoinChannel("room1"); err != nil {
		t.Fatalf("JoinChannel: %v", err)
	}
	if n := s.pendingJoinCount(); n != 1 {
		t.Fatalf("join 后 pending = %d, want 1", n)
	}
	if err := s.LeaveChannel(); err != nil {
		t.Fatalf("LeaveChannel: %v", err)
	}
	if s.InRoom() {
		t.Fatal("LeaveChannel 后 InRoom 应为 false")
	}

	stub.push(t, joinedEnv(t, "room1", "self", "uA"))
	pushServerHelloV2(t, stub, s) // 屏障：迟到的 joined 已处理

	if s.InRoom() {
		t.Fatal("迟到的 channel.joined 不得把房间态拉回 true（#99）")
	}
	if got := s.roomID.Load().(string); got != "" {
		t.Fatalf("roomID = %q, want empty（退房后不得被迟到回执改写）", got)
	}
	if n := len(s.Members()); n != 0 {
		t.Fatalf("members = %d, want 0（不得重建旧房成员表）", n)
	}
	if s.mediaStarted.Load() {
		t.Fatal("迟到 joined 不得重启媒体（#99）")
	}
	if n := activity.Load(); n != 0 {
		t.Fatalf("迟到 joined 不得发 OnSelf/OnPeerJoin，共 %d 次", n)
	}
}

// TestLateChannelJoinedAfterSwitchIgnored 快速 A→B：#A 的迟到 joined 不改变
// B 的成员/媒体，也不得触发 peer 重协商；B 自己的 joined 正常生效。
func TestLateChannelJoinedAfterSwitchIgnored(t *testing.T) {
	stub := newLateJoinStub(t)
	selfs := make(chan string, 4)
	peerJoins := make(chan string, 16)
	s := newJoinedSession(t, stub, Events{
		OnSelf: func(id string) { selfs <- id },
		OnPeerJoin: func(uid, nickname, tag string, speaking, mute, deafen, priority bool) {
			peerJoins <- uid
		},
	})

	if err := s.JoinChannel("roomA"); err != nil {
		t.Fatalf("Join roomA: %v", err)
	}
	if err := s.JoinChannel("roomB"); err != nil {
		t.Fatalf("Join roomB: %v", err)
	}
	if n := s.pendingJoinCount(); n != 2 {
		t.Fatalf("pending = %d, want 2", n)
	}

	// A 的 joined 迟到：确认了 pending A，但 roomID 已指向 B，整帧忽略。
	stub.push(t, joinedEnv(t, "roomA", "self", "uA"))
	waitUntil(t, "A 的 pending 结算", func() bool { return s.pendingJoinCount() == 1 })
	if got := s.roomID.Load().(string); got != "roomB" {
		t.Fatalf("roomID = %q, want roomB（A 的迟到回执不得切回旧房）", got)
	}
	if n := len(s.Members()); n != 0 {
		t.Fatalf("A 的迟到 joined 不得重建成员表, members = %d", n)
	}
	if s.mediaStarted.Load() {
		t.Fatal("A 的迟到 joined 不得启动媒体（#99）")
	}
	if p := s.peerRef.Load(); p != nil {
		t.Fatal("A 的迟到 joined 不得触发 peer 重协商（#99）")
	}
	select {
	case uid := <-peerJoins:
		t.Fatalf("A 的迟到 joined 不得发 OnPeerJoin（收到 %q）", uid)
	default:
	}
	select {
	case id := <-selfs:
		t.Fatalf("A 的迟到 joined 不得发 OnSelf（收到 %q）", id)
	default:
	}

	// B 的 joined 正常生效。
	stub.push(t, joinedEnv(t, "roomB", "self", "uB", "self"))
	select {
	case id := <-selfs:
		if id != "self" {
			t.Fatalf("OnSelf = %q, want self", id)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("B 的 channel.joined 未生效（#99 回归）")
	}
	if !s.InRoom() {
		t.Fatal("B 的 joined 后应在房内")
	}
	if got := s.roomID.Load().(string); got != "roomB" {
		t.Fatalf("roomID = %q, want roomB", got)
	}
	if n := s.pendingJoinCount(); n != 0 {
		t.Fatalf("B 的 joined 后 pending = %d, want 0", n)
	}
	if n := len(s.Members()); n != 2 {
		t.Fatalf("B 的成员表 = %d, want 2", n)
	}
	if !s.mediaStarted.Load() {
		t.Fatal("B 的 joined 应启动媒体")
	}
	// OnSelf 回调发生在 setupPeer 之前，这里等 peer 真正建起来。
	waitUntil(t, "B 的 peer 建立", func() bool { return s.peerRef.Load() != nil })
	// A 的成员绝不能混进 B 的成员表/回调。
	waitUntil(t, "B 的成员广播", func() bool { return len(peerJoins) >= 2 })
	for i := 0; i < 2; i++ {
		if uid := <-peerJoins; uid == "uA" {
			t.Fatal("旧房成员 uA 不得出现在 B 的 OnPeerJoin 中")
		}
	}
}

// TestChannelJoinedNormalAndBroadcastRebuild 正常入房与房内成员广播回归：
// 无 pending 的广播帧在 roomID 匹配时仍按原逻辑重建成员表（#99 不能误伤）。
func TestChannelJoinedNormalAndBroadcastRebuild(t *testing.T) {
	stub := newLateJoinStub(t)
	selfs := make(chan string, 4)
	s := newJoinedSession(t, stub, Events{
		OnSelf: func(id string) { selfs <- id },
	})

	if err := s.JoinChannel("room1"); err != nil {
		t.Fatalf("JoinChannel: %v", err)
	}
	stub.push(t, joinedEnv(t, "room1", "self", "uA", "self"))
	select {
	case <-selfs:
	case <-time.After(5 * time.Second):
		t.Fatal("正常入房的 channel.joined 未生效")
	}
	if n := len(s.Members()); n != 2 {
		t.Fatalf("入房成员 = %d, want 2", n)
	}
	if n := s.pendingJoinCount(); n != 0 {
		t.Fatalf("入房后 pending = %d, want 0", n)
	}

	// 房内成员变化的全量广播（无 pending，roomID 匹配）：成员表照常重建。
	stub.push(t, joinedEnv(t, "room1", "self", "uA", "uB", "self"))
	waitUntil(t, "房内广播重建成员表", func() bool { return len(s.Members()) == 3 })
	if got := s.roomID.Load().(string); got != "room1" {
		t.Fatalf("广播后 roomID = %q, want room1", got)
	}
	if !s.mediaStarted.Load() {
		t.Fatal("房内广播不得停掉已启动的媒体")
	}
}
