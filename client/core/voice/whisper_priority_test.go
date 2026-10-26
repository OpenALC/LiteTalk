package voice

import (
	"context"
	"testing"
	"time"

	"github.com/OpenALC/LiteTalk/client/core/api"
	"github.com/OpenALC/LiteTalk/client/core/audio"
	"github.com/OpenALC/LiteTalk/client/core/signal"
)

// #45：悄悄话下发受协议版本门控；优先发言 ducking 只压非优先远端。

// TestWhisperSendProtocolGate 协议 <3 不发 voice.whisper；协商到 v3 后正常下发。
func TestWhisperSendProtocolGate(t *testing.T) {
	stub := newJoinReplyStub(t, func(string) []signal.Envelope { return nil })
	s := NewSession(api.NewAPIClient(stub.wsURL()), "tok", "n", Events{}, nil)
	s.ws = signal.NewClient(signal.Config{URL: stub.wsURL(), PingInterval: 50 * time.Millisecond})
	defer s.ws.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go s.ws.Run(ctx)
	waitSignalConnected(t, s)
	go s.whisperLoop(ctx)

	// v2 服务端不认该消息：按住悄悄话也不得下发。
	s.protoVer.Store(2)
	s.SetWhisperTargets([]string{"bob"})
	time.Sleep(150 * time.Millisecond)
	if stub.sawType(signal.TypeVoiceWhisper) {
		t.Fatal("协议 <3 时不应下发 voice.whisper")
	}

	// 协商到 v3 后下发。
	s.protoVer.Store(3)
	s.SetWhisperTargets([]string{"bob"})
	if !stub.awaitType(signal.TypeVoiceWhisper, 5*time.Second) {
		t.Fatal("协议 v3 下应在 5s 内下发 voice.whisper")
	}
}

// TestPriorityFromJoinedAndEvent channel.joined 的 MemberInfo.priority 进入
// PeerView，voice.priority 就地更新并回调 UI（#45）。
func TestPriorityFromJoinedAndEvent(t *testing.T) {
	joined := signal.Envelope{
		Type: signal.TypeChannelJoined, TS: 1,
		Data: []byte(`{"channel_id":"room1","members":[
			{"user_id":"u1","nickname":"甲","mute":false,"deafen":false,"speaking":false,"online":true,"priority":true},
			{"user_id":"u2","nickname":"乙","mute":false,"deafen":false,"speaking":false,"online":true}
		],"self":{"user_id":"me","nickname":"我","mute":false,"deafen":false,"speaking":false,"online":true}}`),
	}
	prio := signal.Envelope{
		Type: signal.TypeVoicePriority, TS: 2,
		Data: []byte(`{"channel_id":"room1","user_id":"u2","priority":true}`),
	}
	stub := newJoinReplyStub(t, func(string) []signal.Envelope {
		return []signal.Envelope{joined, prio}
	})
	got := make(chan struct {
		uid string
		ok  bool
	}, 2)
	s := NewSession(api.NewAPIClient(stub.wsURL()), "tok", "n", Events{
		OnPeerPriority: func(userID string, priority bool) {
			got <- struct {
				uid string
				ok  bool
			}{userID, priority}
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
	case ev := <-got:
		if ev.uid != "u2" || !ev.ok {
			t.Fatalf("OnPeerPriority = %+v，期望 u2/true", ev)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("未收到 OnPeerPriority")
	}

	byID := map[string]PeerView{}
	for _, m := range s.Members() {
		byID[m.UserID] = m
	}
	if !byID["u1"].Priority {
		t.Fatalf("joined 快照的 priority 未进入 PeerView: %+v", byID)
	}
	if !byID["u2"].Priority {
		t.Fatalf("voice.priority 未更新 PeerView: %+v", byID)
	}
}

// TestWhisperWantLifecycle 目标下发状态机（#45）：脏标记只发一次；取消为空
// 也要下发；退房清空不发；入房同步强制发一次（空目标也算）。
func TestWhisperWantLifecycle(t *testing.T) {
	s := NewSession(api.NewAPIClient("http://127.0.0.1:1"), "tok", "n", Events{}, nil)

	// 从未用过悄悄话：入房同步不应白发信令。
	s.syncWhisperOnJoin()
	if _, send := s.pendingWhisper(); send {
		t.Fatal("从未启用悄悄话时入房不应下发")
	}

	s.SetWhisperTargets([]string{"u1", "", "u2"})
	targets, send := s.pendingWhisper()
	if !send || len(targets) != 2 || targets[0] != "u1" || targets[1] != "u2" {
		t.Fatalf("首次下发 = %v/%v", targets, send)
	}
	if _, send = s.pendingWhisper(); send {
		t.Fatal("脏标记消费后不应重复下发")
	}

	// 取消（之前有目标）必须发空数组。
	s.SetWhisperTargets(nil)
	targets, send = s.pendingWhisper()
	if !send || len(targets) != 0 {
		t.Fatalf("取消应下发空数组，实际 %v/%v", targets, send)
	}
	// 重复取消（本就为空）不再下发。
	s.SetWhisperTargets(nil)
	if _, send = s.pendingWhisper(); send {
		t.Fatal("重复取消不应下发")
	}

	// 退房清空：不发信令。
	s.SetWhisperTargets([]string{"u1"})
	_, _ = s.pendingWhisper()
	s.ClearWhisperTargets()
	if _, send = s.pendingWhisper(); send {
		t.Fatal("退房清空不应下发")
	}

	// 入房同步：即使 want 为空也强制发一次空数组，清掉服务端残留。
	s.syncWhisperOnJoin()
	targets, send = s.pendingWhisper()
	if !send || len(targets) != 0 {
		t.Fatalf("入房同步应强制下发空数组，实际 %v/%v", targets, send)
	}
}

// sawType 是否已收到过某类型消息。
func (s *joinReplyStub) sawType(want string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, typ := range s.received {
		if typ == want {
			return true
		}
	}
	return false
}

// awaitType 在超时内等到某类型消息。
func (s *joinReplyStub) awaitType(want string, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if s.sawType(want) {
			return true
		}
		time.Sleep(10 * time.Millisecond)
	}
	return s.sawType(want)
}

// TestUpdateDuckingPrioritySpeaker 远端优先发言者说话时压其他远端；自己
// （即使也是优先发言）不触发；用户手动音量不被覆盖。
func TestUpdateDuckingPrioritySpeaker(t *testing.T) {
	s := NewSession(api.NewAPIClient("http://127.0.0.1:1"), "tok", "n", Events{}, nil)
	s.mixer = audio.NewMixer(func([]int16) int { return 0 })
	s.selfID = "me"
	prio := s.mixer.AddPeer("p")
	other := s.mixer.AddPeer("o")
	other.SetGain(0.8)

	s.mu.Lock()
	s.members = map[string]PeerView{
		"me": {UserID: "me", Priority: true, Speaking: true},
		"p":  {UserID: "p", Priority: true, Speaking: true},
		"o":  {UserID: "o", Speaking: true},
	}
	s.mu.Unlock()

	s.updateDucking()
	if prio.Ducked() {
		t.Fatal("优先发言者本人不应被压低")
	}
	if !other.Ducked() {
		t.Fatal("优先发言者说话时其他远端应被压低")
	}
	if other.Gain() != 0.8 {
		t.Fatalf("duck 不得覆盖用户音量，Gain = %v", other.Gain())
	}

	// 只有自己（优先发言）在说话：不触发本地 ducking（本地混音不含自己）。
	s.mu.Lock()
	v := s.members["p"]
	v.Speaking = false
	s.members["p"] = v
	s.mu.Unlock()
	s.updateDucking()
	if other.Ducked() {
		t.Fatal("自己说话不应触发本地 ducking")
	}

	// 优先发言者离开：duck 立即解除。
	s.mu.Lock()
	v = s.members["p"]
	v.Speaking = true
	s.members["p"] = v
	s.mu.Unlock()
	s.updateDucking()
	if !other.Ducked() {
		t.Fatal("前置条件：优先发言者说话应压低 others")
	}
	s.mu.Lock()
	delete(s.members, "p")
	s.mu.Unlock()
	s.updateDucking()
	if other.Ducked() {
		t.Fatal("优先发言者离开后应恢复其它远端音量")
	}
}
