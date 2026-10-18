package sfu

import (
	"sync"
	"testing"
	"time"

	"github.com/pion/webrtc/v4"
)

// fakeSignal 记录 SendFailed 的 SignalSender 桩（#19 断言用，不建真实连接）。
type fakeSignal struct {
	mu     sync.Mutex
	failed []string
}

func (f *fakeSignal) SendOffer(string)                      {}
func (f *fakeSignal) SendCandidate(webrtc.ICECandidateInit) {}
func (f *fakeSignal) SendFailed(channelID, userID, reason string) {
	f.mu.Lock()
	f.failed = append(f.failed, channelID+"|"+userID+"|"+reason)
	f.mu.Unlock()
}

func (f *fakeSignal) calls() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.failed...)
}

// peerOutbound 查询 SFU 侧某成员的下行路由表是否含 srcUserID（#19/#23 断言用）。
func peerOutbound(s *SFU, room, userID, src string) bool {
	p := s.findPeer(room, userID)
	if p == nil {
		return false
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	_, ok := p.outbound[src]
	return ok
}

// TestICEFailCleansPeerOnClientVanish (#19) 对端销毁 PC（DTLS CloseNotify → SFU 侧
// PC 非主动关闭）即 ICE 失败清理：peer 实例摘除、其余成员指向它的下行路由摘除、
// 只通知本端 rtc.failed。走真实 pion 状态回调，不手工触发处理函数。
func TestICEFailCleansPeerOnClientVanish(t *testing.T) {
	s := newTestSFU(t, 10)
	b := newTestClient(t, s, "c1", "bob")
	a := newTestClient(t, s, "c1", "alice")

	sent := newSentSet()
	stop := make(chan struct{})
	go a.sendLoop(stop, sent, "a")
	if b.waitFor(sent, 5*time.Second) == nil {
		close(stop)
		t.Fatal("基线不通：B 未收到 A 的 RTP")
	}
	close(stop)

	// A 的客户端直接销毁 PC（崩溃/强杀，未发 channel.leave）。
	_ = a.pc.Close()

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) && s.findPeer("c1", "alice") != nil {
		time.Sleep(20 * time.Millisecond)
	}
	if p := s.findPeer("c1", "alice"); p != nil {
		t.Fatal("ICE 失败后 peer 未被移除（#19）")
	}
	if peerOutbound(s, "c1", "bob", "alice") {
		t.Fatal("ICE 失败后 B 指向 A 的下行路由未摘除（#23 同因）")
	}
	calls := a.failedCalls()
	if len(calls) != 1 || calls[0] != "c1|alice|ice_failed" {
		t.Fatalf("应只通知本端一次 rtc.failed，实际 %v（#19）", calls)
	}
	if got := b.failedCalls(); len(got) != 0 {
		t.Fatalf("rtc.failed 不得发给其他成员，B 收到 %v（#19）", got)
	}
}

// TestICEFailNoNotifyAfterReplace 同 WS 连接重入房（connID 相同）后旧实例的失败
// 通知不得误杀新 peer、不得发 rtc.failed（#19：同 connID 只有指针核对可分辨）。
func TestICEFailNoNotifyAfterReplace(t *testing.T) {
	s := newTestSFU(t, 10)
	room, err := s.getOrCreateRoom("c1")
	if err != nil {
		t.Fatalf("getOrCreateRoom: %v", err)
	}
	f1 := &fakeSignal{}
	p1, err := room.addPeer("alice", "conn-1", f1)
	if err != nil {
		t.Fatalf("addPeer p1: %v", err)
	}
	f2 := &fakeSignal{}
	p2, err := room.addPeer("alice", "conn-1", f2) // 同 connID 顶替
	if err != nil {
		t.Fatalf("addPeer p2: %v", err)
	}

	p1.onConnFailed() // 旧实例的失败清理迟到

	if got := s.findPeer("c1", "alice"); got != p2 {
		t.Fatal("旧实例的 ICE 失败清理误杀了重入的新 peer（#19）")
	}
	if calls := f1.calls(); len(calls) != 0 {
		t.Fatalf("已被顶替的旧实例不得发 rtc.failed，实际 %v（#19）", calls)
	}
	if calls := f2.calls(); len(calls) != 0 {
		t.Fatalf("新实例不受旧实例失败影响，不得发 rtc.failed，实际 %v（#19）", calls)
	}
}

// TestLeaveNoRTCFailed 主动离房（close 先置 closed）不发 rtc.failed（#19）：
// rtc.failed 是 ICE 失败专用，正常离房的对端走 channel.peer_left。
func TestLeaveNoRTCFailed(t *testing.T) {
	s := newTestSFU(t, 10)
	a := newTestClient(t, s, "c1", "alice")
	_ = newTestClient(t, s, "c1", "bob")

	s.LeaveIf("c1", "alice", "alice")
	time.Sleep(200 * time.Millisecond) // OnConnectionStateChange(Closed) 异步到达窗口
	if got := a.failedCalls(); len(got) != 0 {
		t.Fatalf("主动离房不得发 rtc.failed，实际 %v（#19）", got)
	}
}
