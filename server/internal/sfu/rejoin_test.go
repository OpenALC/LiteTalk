package sfu

import (
	"testing"
	"time"
)

// TestRejoinAfterLeaveRebuildsRoute (#23) 复现「离开后重新入房，其他人听不到他」：
// 订阅端（真实客户端）在 channel.peer_left 时会拆除该路读取循环（remote.RemovePeer），
// 因此 A 重入后 B 必须拿到全新的 OnTrack 才能恢复收听。若 SFU 惰性保留旧 outbound
// （ensureRoute 见同名条目即跳过），重入的 RTP 被写进旧轨道——B 的旧读取循环已拆、
// 新 OnTrack 不会来，永远无声。
func TestRejoinAfterLeaveRebuildsRoute(t *testing.T) {
	s := newTestSFU(t, 10)
	b := newTestClient(t, s, "c1", "bob")
	a1 := newTestClient(t, s, "c1", "alice")

	// 首轮：A 发包，B 应收到。
	sent1 := newSentSet()
	stop1 := make(chan struct{})
	go a1.sendLoop(stop1, sent1, "a1")
	if b.waitFor(sent1, 5*time.Second) == nil {
		close(stop1)
		t.Fatal("B 未收到 A 首轮 RTP（基线不通，无法验证重入房）")
	}

	// A 离房（信令层随后广播 peer_left），B 拆掉 A 的读取循环——真实客户端语义。
	s.LeaveIf("c1", "alice", "alice")
	close(stop1)
	b.stopTrackReaders()

	// A 重入（新 PC、新上行轨道，同 userID）。
	a2 := newTestClient(t, s, "c1", "alice")
	sent2 := newSentSet()
	stop2 := make(chan struct{})
	defer close(stop2)
	go a2.sendLoop(stop2, sent2, "a2")

	if b.waitFor(sent2, 5*time.Second) == nil {
		t.Fatal("A 重入后 B 未收到新 RTP：下行轨道惰性保留，旧读取循环已拆、新 OnTrack 不来（#23）")
	}
	if got := b.tracksUp.Load(); got != 2 {
		t.Fatalf("A 重入后 B 应有 2 次 OnTrack（离房摘轨 + 重入建轨），实际 %d（#23）", got)
	}
}
