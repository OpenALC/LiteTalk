package sfu

import (
	"fmt"
	"io"
	"log"
	"sync"
	"testing"
)

// newBareSFU 只有 rooms map 的 SFU 桩（#19 生命周期单测：不建 PeerConnection、
// 不跑判定循环）。
func newBareSFU(maxRooms int) *SFU {
	return &SFU{
		cfg:   Config{MaxRooms: maxRooms},
		log:   log.New(io.Discard, "", 0),
		rooms: make(map[string]*Room),
	}
}

// addBarePeer 按 addPeer 的临界区语义登记一个无 PC 的 peer（单测用）。
func addBarePeer(r *Room, userID string) *Peer {
	r.mu.Lock()
	defer r.mu.Unlock()
	p := &Peer{userID: userID, room: r}
	r.peers[userID] = p
	return p
}

// TestRoomJoinReservation (#19)：acquireRoom 与 addPeer 之间房间即使为空也不得
// 被回收，否则 peer 会落进已从 rooms 摘掉的孤儿房；加入完成后房间保留，
// 摘掉成员后才允许回收。
func TestRoomJoinReservation(t *testing.T) {
	s := newBareSFU(0)
	room, err := s.acquireRoom("c1")
	if err != nil {
		t.Fatalf("acquireRoom: %v", err)
	}
	// 入房进行中（模拟 HandleOffer 尚未 addPeer）：不得回收。
	s.removeRoomIfEmpty("c1")
	if got := s.findRoom("c1"); got != room {
		t.Fatal("入房进行中的空房间被误删——peer 会落进孤儿房（#19）")
	}

	addBarePeer(room, "alice")
	room.releaseJoin()
	// 有成员：即使调用方再调回收也保留。
	s.removeRoomIfEmpty("c1")
	if got := s.findRoom("c1"); got != room {
		t.Fatal("有 peer 的房间被误删（#19）")
	}

	// 成员摘除后（模拟 LeaveIf/onConnFailed 的 removePeerInstance），允许回收。
	room.mu.Lock()
	delete(room.peers, "alice")
	room.mu.Unlock()
	s.removeRoomIfEmpty("c1")
	if got := s.findRoom("c1"); got != nil {
		t.Fatal("空房间未回收（#19）")
	}
}

// TestICEFailReclaimsEmptyRoom (#19)：ICE 失败摘除最后一个 peer 后，空房必须
// 从 rooms map 回收，否则会逐步占满 MaxRooms。
func TestICEFailReclaimsEmptyRoom(t *testing.T) {
	s := newTestSFU(t, 10)
	room, err := s.acquireRoom("c1")
	if err != nil {
		t.Fatalf("acquireRoom: %v", err)
	}
	f := &fakeSignal{}
	p, err := room.addPeer("alice", "conn-1", f)
	if err != nil {
		t.Fatalf("addPeer: %v", err)
	}
	room.releaseJoin()

	p.onConnFailed()

	if got := s.findRoom("c1"); got != nil {
		t.Fatal("ICE 失败后空房间未回收（#19）")
	}
	if calls := f.calls(); len(calls) != 1 || calls[0] != "c1|alice|ice_failed" {
		t.Fatalf("应只通知本端一次 rtc.failed，实际 %v", calls)
	}
}

// TestICEFailKeepsRoomQuotaAvailable (#19)：每轮「建房 → 入房 → ICE 失败清理」
// 都必须释放名额。修复前空房滞留，第 MaxRooms+1 个房间起全部被拒（review 复现）。
func TestICEFailKeepsRoomQuotaAvailable(t *testing.T) {
	s := newTestSFU(t, 10)
	s.cfg.MaxRooms = 2
	for i := 0; i < 8; i++ {
		id := fmt.Sprintf("q%d", i)
		room, err := s.acquireRoom(id)
		if err != nil {
			t.Fatalf("第 %d 个房间被配额卡住（空房未回收）: %v", i, err)
		}
		p, err := room.addPeer(fmt.Sprintf("u%d", i), "conn", nil)
		if err != nil {
			room.releaseJoin()
			t.Fatalf("addPeer: %v", err)
		}
		room.releaseJoin()
		p.onConnFailed()
		if got := s.findRoom(id); got != nil {
			t.Fatalf("房间 %s 在 ICE 失败后未回收", id)
		}
	}
}

// TestRoomCreateRemoveRace (#19)：acquireRoom/releaseJoin 与 removeRoomIfEmpty
// 并发交错（-race）：一旦 peer 落位，房间必须仍在 rooms map 中，不能出现
// 「已从 map 删除但仍有成员」的孤儿房。
func TestRoomCreateRemoveRace(t *testing.T) {
	s := newBareSFU(0)
	const workers = 64
	var wg sync.WaitGroup
	var mu sync.Mutex
	orphans := 0
	for i := 0; i < workers; i++ {
		uid := fmt.Sprintf("u%d", i)
		wg.Add(2)
		go func() {
			defer wg.Done()
			room, err := s.acquireRoom("race")
			if err != nil {
				return
			}
			addBarePeer(room, uid)
			room.releaseJoin()
			// peer 已落位：并发的回收不允许删掉这个房间。
			mu.Lock()
			if s.findRoom("race") != room {
				orphans++
			}
			mu.Unlock()
		}()
		go func() {
			defer wg.Done()
			s.removeRoomIfEmpty("race")
		}()
	}
	wg.Wait()
	if orphans != 0 {
		t.Fatalf("%d 次并发交错产生了孤儿房（#19 竞态）", orphans)
	}
}
