package sfu

import (
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/pion/webrtc/v4"
)

// routeOf 读取某成员当前指向 srcUID 的下行路由（测试断言用）。
func routeOf(s *SFU, roomID, userID, srcUID string) *outRoute {
	p := s.findPeer(roomID, userID)
	if p == nil {
		return nil
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.outbound[srcUID]
}

// waitRouteSrc 轮询等待某成员建立一条指向 src 实例的下行路由。
func waitRouteSrc(t *testing.T, s *SFU, roomID, userID, srcUID string, src *Peer) *outRoute {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if r := routeOf(s, roomID, userID, srcUID); r != nil && r.src == src {
			return r
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("5s 内未建立 %s → %s 的路由（src 实例 %p）", srcUID, userID, src)
	return nil
}

// installTestRoute 给 target 装一条指向 src 实例的 outbound 路由（不经真实 RTP，
// 只验证路由表的实例核对与锁）。
func installTestRoute(t *testing.T, target, src *Peer) *webrtc.TrackLocalStaticRTP {
	t.Helper()
	track, err := webrtc.NewTrackLocalStaticRTP(
		webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeOpus, ClockRate: 48000, Channels: 2},
		"audio-"+src.userID, "test",
	)
	if err != nil {
		t.Fatalf("NewTrackLocalStaticRTP: %v", err)
	}
	sender, err := target.pc.AddTrack(track)
	if err != nil {
		t.Fatalf("AddTrack: %v", err)
	}
	target.mu.Lock()
	target.outbound[src.userID] = &outRoute{track: track, sender: sender, src: src}
	target.mu.Unlock()
	return track
}

// TestTeardownOldInstanceKeepsReentryRoutes (#23)：同 uid、同 connID 重入后，旧实例
// 迟到的 teardown 不得摘掉新实例刚建立的路由。旧实现按 userID 摘路由，会把新路由
// 一并抹掉，订阅端此后永远收不到该成员的 RTP（无声窗口）。
func TestTeardownOldInstanceKeepsReentryRoutes(t *testing.T) {
	s := newTestSFU(t, 10)
	bob := newTestClient(t, s, "c1", "bob")
	oldClient := newTestClient(t, s, "c1", "alice") // connID = userID

	oldPeer := s.findPeer("c1", "alice")
	if oldPeer == nil {
		t.Fatal("旧实例未登记")
	}
	// 旧实例上行一包，触发 OnTrack → addRoutesFrom，建立 bob 指向旧实例的路由。
	stopOld := make(chan struct{})
	go oldClient.sendLoop(stopOld, newSentSet(), "old")
	rOld := waitRouteSrc(t, s, "c1", "bob", "alice", oldPeer)
	close(stopOld)
	oldTrack := rOld.track

	// 同一 WS 连接重入（同 uid、同 connID）：addPeer 顶替旧实例并摘旧路由。
	reClient := newTestClient(t, s, "c1", "alice")
	newPeer := s.findPeer("c1", "alice")
	if newPeer == nil || newPeer == oldPeer {
		t.Fatalf("重入后应登记新实例，实际 %p（旧 %p）", newPeer, oldPeer)
	}
	if r := routeOf(s, "c1", "bob", "alice"); r != nil {
		t.Fatal("重入时应摘除旧实例的路由")
	}

	// 新实例上行，重建路由。
	stopNew := make(chan struct{})
	go reClient.sendLoop(stopNew, newSentSet(), "new")
	rNew := waitRouteSrc(t, s, "c1", "bob", "alice", newPeer)
	close(stopNew)
	if rNew.track == oldTrack {
		t.Fatal("重入后的路由仍指向旧轨道（#23）")
	}

	// 旧实例迟到的 teardown（removePeerIf/removePeerInstance 在 delete 与 teardown
	// 之间被重入插队）：按实例核对后不得动新实例的路由。
	s.findRoom("c1").teardownPeer(oldPeer)
	got := routeOf(s, "c1", "bob", "alice")
	if got == nil {
		t.Fatal("旧实例 teardown 摘掉了重入后的新路由（#23 无声窗口）")
	}
	if got.src != newPeer || got.track != rNew.track {
		t.Fatalf("路由应仍指向新实例：src=%p（期望 %p）", got.src, newPeer)
	}

	// 端到端确认：旧实例清理后，新实例的 RTP 仍能转发到 bob。
	sentPost := newSentSet()
	postStop := make(chan struct{})
	defer close(postStop)
	go reClient.sendLoop(postStop, sentPost, "post")
	if bob.waitFor(sentPost, 5*time.Second) == nil {
		t.Fatal("旧实例 teardown 后 bob 收不到新实例的 RTP（#23）")
	}
}

// TestDropRoutesFromInstanceRace (#23)：旧实例的 teardown 与新实例路由并发交错时，
// 路由不被误删（-race 下同时验证锁的正确性）。旧路由在顶替时已摘，新路由全程保持。
func TestDropRoutesFromInstanceRace(t *testing.T) {
	s := newTestSFU(t, 10)
	room, err := s.getOrCreateRoom("c1")
	if err != nil {
		t.Fatalf("getOrCreateRoom: %v", err)
	}
	bob, err := room.addPeer("bob", "conn-b", nil)
	if err != nil {
		t.Fatalf("addPeer bob: %v", err)
	}
	oldPeer, err := room.addPeer("alice", "conn-1", nil)
	if err != nil {
		t.Fatalf("addPeer old: %v", err)
	}
	installTestRoute(t, bob, oldPeer)
	newPeer, err := room.addPeer("alice", "conn-1", nil) // 同 uid+connID 顶替
	if err != nil {
		t.Fatalf("addPeer new: %v", err)
	}
	if r := routeOf(s, "c1", "bob", "alice"); r != nil {
		t.Fatal("顶替时旧实例路由应已摘除")
	}
	newTrack := installTestRoute(t, bob, newPeer)

	var bad atomic.Int64
	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(2)
		go func() {
			defer wg.Done()
			// 旧实例的清理反复迟到：按实例核对，绝不能碰到新路由。
			room.teardownPeer(oldPeer)
			room.dropRoutesFrom(oldPeer)
		}()
		go func() {
			defer wg.Done()
			for j := 0; j < 500; j++ {
				bob.mu.Lock()
				r := bob.outbound["alice"]
				ok := r != nil && r.src == newPeer && r.track == newTrack
				bob.mu.Unlock()
				if !ok {
					bad.Add(1)
				}
			}
		}()
	}
	wg.Wait()
	if n := bad.Load(); n != 0 {
		t.Fatalf("并发旧实例清理误删/改写了新实例路由 %d 次（#23）", n)
	}

	// 新实例的 teardown 必须能正常摘除自己的路由。
	room.teardownPeer(newPeer)
	if r := routeOf(s, "c1", "bob", "alice"); r != nil {
		t.Fatal("新实例 teardown 未摘除自己的路由")
	}
}
