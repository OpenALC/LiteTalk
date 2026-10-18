package sfu

import (
	"bytes"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/pion/rtp"
)

// sendLevel 发一包带 RFC 6464 电平的上行 RTP（#33 测试桩）。
// 测试客户端只注册 audio-level 一个扩展，extmap ID 恒为 1（注册序首个）。
func (c *testClient) sendLevel(seq uint16, level byte, payload []byte) error {
	pkt := &rtp.Packet{
		Header:  rtp.Header{Version: 2, SequenceNumber: seq, Timestamp: uint32(seq) * 960},
		Payload: payload,
	}
	if err := pkt.Header.SetExtension(1, []byte{level}); err != nil {
		return err
	}
	return c.track.WriteRTP(pkt)
}

// flipRec 一次说话翻转记录。
type flipRec struct {
	user     string
	speaking bool
}

// flipLog 并发安全的翻转收集器。
type flipLog struct {
	mu    sync.Mutex
	flips []flipRec
}

func (l *flipLog) add(userID string, speaking bool) {
	l.mu.Lock()
	l.flips = append(l.flips, flipRec{userID, speaking})
	l.mu.Unlock()
}

func (l *flipLog) has(userID string, speaking bool) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, f := range l.flips {
		if f.user == userID && f.speaking == speaking {
			return true
		}
	}
	return false
}

// waitFlip 轮询等待目标翻转出现。
func waitFlip(t *testing.T, l *flipLog, userID string, speaking bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if l.has(userID, speaking) {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("等待翻转 %s speaking=%v 超时", userID, speaking)
}

// streamLevel 以 50pps 持续发包（带电平），stop 关闭后退出。
func streamLevel(c *testClient, stop <-chan struct{}, level byte, prefix string) {
	for i := 0; ; i++ {
		select {
		case <-stop:
			return
		default:
		}
		_ = c.sendLevel(uint16(100+i), level, []byte(fmt.Sprintf("%s-%d", prefix, i)))
		time.Sleep(20 * time.Millisecond)
	}
}

// TestSpeakingFromLevel 说话判定全链路（#33）：上行电平 → ticker 迟滞 →
// OnSpeakingFn 翻转。响亮电平起判说话；停流（muted/DTX 语义）按静音退出——
// 最后一包的电平不允许把说话态永久卡住。
func TestSpeakingFromLevel(t *testing.T) {
	s := newTestSFU(t, 10)
	log := &flipLog{}
	s.SetOnSpeaking(func(channelID, userID string, speaking bool) {
		log.add(userID, speaking)
	})
	c := newTestClient(t, s, "lv1", "alice")

	stop := make(chan struct{})
	go streamLevel(c, stop, 20, "loud") // 20 ≤ 38：进入门限内
	waitFlip(t, log, "alice", true)

	// 停流 ≈ 服务端视同静音：500ms 内应翻回 false（stale 2 tick + exit 2 tick）。
	close(stop)
	waitFlip(t, log, "alice", false)
}

// TestActiveGateTopN 活跃转发包级门控（#33 Phase 1）：ActiveN=1 时只转发最响
// 的一路，静默候选让位；全程不摘 track（outbound 路由保持挂载）。
func TestActiveGateTopN(t *testing.T) {
	s := newTestSFUCfg(t, Config{UDPPort: 0, NAT1To1IP: "127.0.0.1", MaxMembers: 10, ActiveN: 1, ActiveHoldMS: 200})

	recv := newTestClient(t, s, "g1", "watcher")
	loud := newTestClient(t, s, "g1", "loud")
	quiet := newTestClient(t, s, "g1", "quiet")

	stop := make(chan struct{})
	go streamLevel(loud, stop, 10, "A")  // 最响
	go streamLevel(quiet, stop, 30, "B") // 也过门限但排第二
	defer close(stop)

	// 等门控生效（首拍置 hold 起点，第二拍后静默候选让位），再取样。
	time.Sleep(800 * time.Millisecond)
	recv.mu.Lock()
	recv.received = nil
	recv.mu.Unlock()
	time.Sleep(600 * time.Millisecond)

	recv.mu.Lock()
	pkts := append([]*rtp.Packet(nil), recv.received...)
	recv.mu.Unlock()
	var gotA, gotB int
	for _, p := range pkts {
		switch {
		case bytes.HasPrefix(p.Payload, []byte("A-")):
			gotA++
		case bytes.HasPrefix(p.Payload, []byte("B-")):
			gotB++
		}
	}
	if gotA == 0 {
		t.Fatalf("最响源未被转发（样本 %d 包）", len(pkts))
	}
	if gotB != 0 {
		t.Fatalf("非 top-1 源被转发 %d 包，包级门控失效", gotB)
	}
	t.Logf("top-1 门控生效：A=%d B=%d（样本 %d）", gotA, gotB, len(pkts))

	// 禁 RemoveTrack（#23 重协商风暴）：watcher 的下行路由必须还在。
	room := s.findRoom("g1")
	if room == nil {
		t.Fatal("房间不存在")
	}
	w := room.getPeer("watcher")
	if w == nil {
		t.Fatal("watcher peer 不存在")
	}
	w.mu.Lock()
	routes := len(w.outbound)
	w.mu.Unlock()
	if routes != 2 {
		t.Fatalf("watcher 下行路由 %d 条，want 2（未摘 track）", routes)
	}
	t.Log("outbound 路由保持挂载，无 RemoveTrack")
}
