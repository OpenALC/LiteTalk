package sfu

import (
	"testing"
	"time"
)

// #36 服务端强制闭麦：包级丢弃（track 不摘）、解禁恢复、同房重入继承。

// TestServerMutedDropsForward 闭麦后目标继续发包（模拟不配合）也不被转发；
// 电平按静音记；解禁后恢复转发。
func TestServerMutedDropsForward(t *testing.T) {
	s := newTestSFU(t, 10)
	alice := newTestClient(t, s, "c1", "alice")
	bob := newTestClient(t, s, "c1", "bob")

	stop := make(chan struct{})
	defer close(stop)
	sent := newSentSet()
	go alice.sendLoop(stop, sent, "sm")
	if bob.waitFor(sent, 10*time.Second) == nil {
		t.Fatal("基线不通：B 未收到 A 的 RTP")
	}

	// 闭麦：A 的 sendLoop 不停（不配合），B 计数不得再增长。
	if !s.SetServerMuted("c1", "alice", true) {
		t.Fatal("SetServerMuted 应命中在房成员")
	}
	base := bob.countMatching(sent)
	time.Sleep(600 * time.Millisecond)
	if got := bob.countMatching(sent); got != base {
		t.Fatalf("闭麦后仍在转发: %d → %d", base, got)
	}
	if d := s.serverMutedDropped.Load(); d == 0 {
		t.Fatal("serverMutedDropped 计数应增长")
	}
	p := s.findPeer("c1", "alice")
	if p == nil || p.level.Load() != 127 {
		t.Fatalf("闭麦包电平应记 127（静音），实际 %v", p)
	}

	// 解禁恢复转发。
	if !s.SetServerMuted("c1", "alice", false) {
		t.Fatal("解禁应命中")
	}
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) && bob.countMatching(sent) <= base {
		time.Sleep(50 * time.Millisecond)
	}
	if bob.countMatching(sent) <= base {
		t.Fatal("解禁后未恢复转发")
	}
}

// TestServerMutedInheritsOnPeerRebuild 同房间换连接/重入后闭麦仍生效
// （room.serverMuted 在 removePeer 不清，newPeer 继承）。
func TestServerMutedInheritsOnPeerRebuild(t *testing.T) {
	s := newTestSFU(t, 10)
	b := newTestClient(t, s, "c1", "bob")
	a1 := newTestClient(t, s, "c1", "alice")

	sent1 := newSentSet()
	stop1 := make(chan struct{})
	go a1.sendLoop(stop1, sent1, "r1")
	if b.waitFor(sent1, 5*time.Second) == nil {
		close(stop1)
		t.Fatal("基线不通：B 未收到 A 首轮 RTP")
	}
	if !s.SetServerMuted("c1", "alice", true) {
		t.Fatal("SetServerMuted 应命中")
	}

	// A 离房重入（新 PC）：闭麦状态必须继承。
	s.LeaveIf("c1", "alice", "alice")
	close(stop1)
	b.stopTrackReaders()
	a2 := newTestClient(t, s, "c1", "alice")
	if p := s.findPeer("c1", "alice"); p == nil || !p.serverMuted.Load() {
		t.Fatalf("重入 peer 应继承 serverMuted，实际 %v", p)
	}
	sent2 := newSentSet()
	stop2 := make(chan struct{})
	defer close(stop2)
	go a2.sendLoop(stop2, sent2, "r2")
	time.Sleep(600 * time.Millisecond)
	if got := b.countMatching(sent2); got != 0 {
		t.Fatalf("继承闭麦后重入者仍被转发 %d 包", got)
	}

	// 解禁重入者 → 恢复。
	if !s.SetServerMuted("c1", "alice", false) {
		t.Fatal("解禁应命中重入 peer")
	}
	if b.waitFor(sent2, 5*time.Second) == nil {
		t.Fatal("解禁后重入者未恢复转发")
	}
}

// TestSetServerMutedMiss 房间/成员不存在返回 false（调用方按 404 语义处理）。
func TestSetServerMutedMiss(t *testing.T) {
	s := newTestSFU(t, 10)
	if s.SetServerMuted("nope", "alice", true) {
		t.Fatal("房间不存在应返回 false")
	}
	a := newTestClient(t, s, "c1", "alice")
	_ = a
	if s.SetServerMuted("c1", "bob", true) {
		t.Fatal("成员不在房应返回 false")
	}
}
