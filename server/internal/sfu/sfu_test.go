package sfu

import (
	"fmt"
	"log"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/pion/rtp"
	"github.com/pion/webrtc/v4"
)

// ---- 测试客户端桩：真实 Pion PeerConnection，走完整 ICE/DTLS/SRTP ----

type testClient struct {
	t      *testing.T
	sfu    *SFU
	roomID string
	userID string

	pc    *webrtc.PeerConnection
	track *webrtc.TrackLocalStaticRTP

	mu           sync.Mutex
	received     []*rtp.Packet
	remoteSet    bool
	pendingCands []webrtc.ICECandidateInit

	tracksUp atomic.Int64 // OnTrack 次数（重入房断言用：订阅端必须拿到新 OnTrack）
	gen      atomic.Int64 // 读取代际：stopTrackReaders 后旧循环停计数（模拟 peer_left 拆读取循环）

	failedMu sync.Mutex
	failed   []string // SendFailed 记录（#19）："<channelID>|<userID>|<reason>"
}

// SendFailed 实现 SignalSender（#19）：ICE 失败通知本端，记录供断言。
// 不用 t.Logf：清理期的异步状态回调可能在测试结束后到达（go test 禁止后置日志）。
func (c *testClient) SendFailed(channelID, userID, reason string) {
	c.failedMu.Lock()
	c.failed = append(c.failed, channelID+"|"+userID+"|"+reason)
	c.failedMu.Unlock()
}

// failedCalls 返回 SendFailed 记录快照。
func (c *testClient) failedCalls() []string {
	c.failedMu.Lock()
	defer c.failedMu.Unlock()
	return append([]string(nil), c.failed...)
}

func newTestSFU(t *testing.T, maxMembers int) *SFU {
	t.Helper()
	logger := log.New(os.Stderr, "[sfu] ", log.Lmicroseconds)
	s, err := New(Config{UDPPort: 0, NAT1To1IP: "127.0.0.1", MaxMembers: maxMembers, Logger: logger})
	if err != nil {
		t.Fatalf("创建 SFU: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

// newTestClient 建一个客户端桩并完成与 SFU 的初始 offer/answer。
func newTestClient(t *testing.T, s *SFU, roomID, userID string) *testClient {
	return newTestClientNet(t, s, roomID, userID, false)
}

// newTestClientNet 建客户端桩；tcpOnly=true 时只启用 TCP 网络类型（模拟禁 UDP 网络，
// 客户端只能用 ICE-TCP 候选与 SFU 建连）。
func newTestClientNet(t *testing.T, s *SFU, roomID, userID string, tcpOnly bool) *testClient {
	t.Helper()
	me := &webrtc.MediaEngine{}
	if err := me.RegisterDefaultCodecs(); err != nil {
		t.Fatal(err)
	}
	// 本机测试：SFU 宣告 127.0.0.1，客户端必须带 loopback 候选才能配对
	//（pion 默认排除 loopback；真实部署时客户端在远端，无需此项）
	se := webrtc.SettingEngine{}
	se.SetIncludeLoopbackCandidate(true)
	if tcpOnly {
		se.SetNetworkTypes([]webrtc.NetworkType{webrtc.NetworkTypeTCP4, webrtc.NetworkTypeTCP6})
	}
	pc, err := webrtc.NewAPI(webrtc.WithMediaEngine(me), webrtc.WithSettingEngine(se)).NewPeerConnection(webrtc.Configuration{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = pc.Close() })

	c := &testClient{t: t, sfu: s, roomID: roomID, userID: userID, pc: pc}

	track, err := webrtc.NewTrackLocalStaticRTP(
		webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeOpus, ClockRate: 48000, Channels: 2},
		"audio", "stub-"+userID,
	)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pc.AddTrack(track); err != nil {
		t.Fatal(err)
	}
	c.track = track

	// 客户端 → SFU trickle
	pc.OnICECandidate(func(ic *webrtc.ICECandidate) {
		if ic == nil {
			return
		}
		if err := s.AddICECandidate(roomID, userID, ic.ToJSON()); err != nil {
			t.Logf("[%s] trickle 到 SFU 失败: %v", userID, err)
		}
	})

	// 下行轨道：读循环记录所有收到的 RTP（代际计数：peer_left 后旧循环停记，模拟真实客户端拆读取循环）
	pc.OnTrack(func(remote *webrtc.TrackRemote, _ *webrtc.RTPReceiver) {
		c.tracksUp.Add(1)
		t.Logf("[%s] 下行轨道建立 ssrc=%d", userID, remote.SSRC())
		gen := c.gen.Load()
		go func() {
			for {
				pkt, _, err := remote.ReadRTP()
				if err != nil {
					return
				}
				if c.gen.Load() != gen {
					continue // 已按 peer_left 语义拆除读取（不再交付应用层）
				}
				c.mu.Lock()
				c.received = append(c.received, pkt)
				c.mu.Unlock()
			}
		}()
	})

	offer, err := pc.CreateOffer(nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := pc.SetLocalDescription(offer); err != nil {
		t.Fatal(err)
	}

	answerSDP, err := s.HandleOffer(roomID, userID, userID, offer.SDP, c)
	if err != nil {
		t.Fatalf("[%s] HandleOffer: %v", userID, err)
	}
	if err := pc.SetRemoteDescription(webrtc.SessionDescription{
		Type: webrtc.SDPTypeAnswer,
		SDP:  answerSDP,
	}); err != nil {
		t.Fatalf("[%s] 应用 answer: %v", userID, err)
	}

	// 补投 answer 应用前到达的 SFU 候选
	c.mu.Lock()
	c.remoteSet = true
	pending := c.pendingCands
	c.pendingCands = nil
	c.mu.Unlock()
	for _, ci := range pending {
		if err := pc.AddICECandidate(ci); err != nil {
			t.Logf("[%s] 补投 SFU 候选失败: %v", userID, err)
		}
	}
	return c
}

// SendOffer 实现 SignalSender：SFU 重协商 offer → 应用到桩并回 answer。
func (c *testClient) SendOffer(sdp string) {
	if err := c.pc.SetRemoteDescription(webrtc.SessionDescription{Type: webrtc.SDPTypeOffer, SDP: sdp}); err != nil {
		c.t.Errorf("[%s] 应用重协商 offer: %v", c.userID, err)
		return
	}
	c.t.Logf("[%s] 收到 SFU 重协商 offer，已应答", c.userID)
	answer, err := c.pc.CreateAnswer(nil)
	if err != nil {
		c.t.Errorf("[%s] 创建重协商 answer: %v", c.userID, err)
		return
	}
	if err := c.pc.SetLocalDescription(answer); err != nil {
		c.t.Errorf("[%s] 设置本地 answer: %v", c.userID, err)
		return
	}
	if err := c.sfu.HandleAnswer(c.roomID, c.userID, answer.SDP); err != nil {
		c.t.Errorf("[%s] 回送 answer: %v", c.userID, err)
	}
}

// SendCandidate 实现 SignalSender：SFU trickle → 应用到桩（answer 前到达的先缓存）。
func (c *testClient) SendCandidate(ci webrtc.ICECandidateInit) {
	c.mu.Lock()
	if !c.remoteSet {
		c.pendingCands = append(c.pendingCands, ci)
		c.mu.Unlock()
		return
	}
	c.mu.Unlock()
	if err := c.pc.AddICECandidate(ci); err != nil {
		c.t.Logf("[%s] 添加 SFU 候选: %v", c.userID, err)
	}
}

// ---- 发包与断言辅助 ----

type sentSet struct {
	mu sync.Mutex
	m  map[string]bool
}

func newSentSet() *sentSet { return &sentSet{m: make(map[string]bool)} }

func (s *sentSet) add(p []byte) {
	s.mu.Lock()
	s.m[string(p)] = true
	s.mu.Unlock()
}

func (s *sentSet) has(p []byte) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.m[string(p)]
}

// sendLoop 以 50pps 发送带唯一标记的 RTP，直到 stop 关闭。
// payload 不是真实 Opus——透传与编解码无关，正好验证"不解包不分析"。
func (c *testClient) sendLoop(stop <-chan struct{}, sent *sentSet, prefix string) {
	seq := uint16(100)
	ts := uint32(1000)
	for i := 0; ; i++ {
		select {
		case <-stop:
			return
		default:
		}
		payload := []byte(fmt.Sprintf("%s-%d", prefix, i))
		sent.add(payload)
		seq++
		ts += 960
		_ = c.track.WriteRTP(&rtp.Packet{
			Header:  rtp.Header{Version: 2, SequenceNumber: seq, Timestamp: ts},
			Payload: payload,
		})
		time.Sleep(20 * time.Millisecond)
	}
}

// waitFor 在超时内等到任一已发送 payload 到达（逐字节精确匹配）。
func (c *testClient) waitFor(sent *sentSet, timeout time.Duration) *rtp.Packet {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		c.mu.Lock()
		for _, pkt := range c.received {
			if sent.has(pkt.Payload) {
				c.mu.Unlock()
				return pkt
			}
		}
		c.mu.Unlock()
		time.Sleep(10 * time.Millisecond)
	}
	return nil
}

// countMatching 统计已收到的、属于 sent 的包数。
func (c *testClient) countMatching(sent *sentSet) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	n := 0
	for _, pkt := range c.received {
		if sent.has(pkt.Payload) {
			n++
		}
	}
	return n
}

// stopTrackReaders 模拟真实客户端收到 channel.peer_left 后拆除该路读取循环
// （remote.RemovePeer）：旧读取循环不再向应用层交付包。之后新 OnTrack 的读取循环
// 以新代际继续计数。
func (c *testClient) stopTrackReaders() { c.gen.Add(1) }

// ---- 集成测试 ----

// TestForwardTwoClients 双客户端 A→B、B→A 端到端 RTP 透传，payload 逐字节一致。
func TestForwardTwoClients(t *testing.T) {
	s := newTestSFU(t, 10)
	alice := newTestClient(t, s, "c1", "alice")
	bob := newTestClient(t, s, "c1", "bob")

	stop := make(chan struct{})
	defer close(stop)

	// A → B
	sentAB := newSentSet()
	go alice.sendLoop(stop, sentAB, "a2b")
	pkt := bob.waitFor(sentAB, 10*time.Second)
	if pkt == nil {
		t.Fatal("B 未收到 A 的 RTP（10s 超时）")
	}
	t.Logf("A→B 透传成功: payload=%q（逐字节一致）seq=%d ts=%d ssrc=%d",
		pkt.Payload, pkt.SequenceNumber, pkt.Timestamp, pkt.SSRC)

	// B → A
	sentBA := newSentSet()
	go bob.sendLoop(stop, sentBA, "b2a")
	pkt = alice.waitFor(sentBA, 10*time.Second)
	if pkt == nil {
		t.Fatal("A 未收到 B 的 RTP（10s 超时）")
	}
	t.Logf("B→A 透传成功: payload=%q（逐字节一致）seq=%d ssrc=%d",
		pkt.Payload, pkt.SequenceNumber, pkt.SSRC)
}

// TestForwardThirdMemberJoins 第三人加入触发重协商：C 加入后 A/B 收到 C 的流，C 也能收到 A 的流。
func TestForwardThirdMemberJoins(t *testing.T) {
	s := newTestSFU(t, 10)
	alice := newTestClient(t, s, "c1", "alice")
	bob := newTestClient(t, s, "c1", "bob")

	stop := make(chan struct{})
	defer close(stop)

	// 先确认 A→B 已通
	sentAB := newSentSet()
	go alice.sendLoop(stop, sentAB, "a2b")
	if bob.waitFor(sentAB, 10*time.Second) == nil {
		t.Fatal("前置条件失败：B 未收到 A 的 RTP")
	}
	t.Log("A→B 已通，第三人 carol 加入")

	carol := newTestClient(t, s, "c1", "carol")

	// C 的流 → A 和 B 都应收到（重协商后新轨道）
	sentC := newSentSet()
	go carol.sendLoop(stop, sentC, "c2all")
	if pkt := alice.waitFor(sentC, 10*time.Second); pkt == nil {
		t.Fatal("C 加入后 A 未收到 C 的 RTP")
	} else {
		t.Logf("C→A 透传成功: payload=%q ssrc=%d", pkt.Payload, pkt.SSRC)
	}
	if pkt := bob.waitFor(sentC, 10*time.Second); pkt == nil {
		t.Fatal("C 加入后 B 未收到 C 的 RTP")
	} else {
		t.Logf("C→B 透传成功: payload=%q ssrc=%d", pkt.Payload, pkt.SSRC)
	}

	// 既有成员的流 → 新成员 C（onPeerReady 补发轨道）
	if pkt := carol.waitFor(sentAB, 10*time.Second); pkt == nil {
		t.Fatal("C 加入后未收到 A 的 RTP")
	} else {
		t.Logf("A→C 透传成功: payload=%q ssrc=%d", pkt.Payload, pkt.SSRC)
	}
}

// TestForwardStopsAfterLeave 成员离开后停止转发，留下的成员不受影响。
func TestForwardStopsAfterLeave(t *testing.T) {
	s := newTestSFU(t, 10)
	alice := newTestClient(t, s, "c1", "alice")
	bob := newTestClient(t, s, "c1", "bob")

	stop := make(chan struct{})
	defer close(stop)

	sentAB := newSentSet()
	go alice.sendLoop(stop, sentAB, "a2b")
	if bob.waitFor(sentAB, 10*time.Second) == nil {
		t.Fatal("前置条件失败：B 未收到 A 的 RTP")
	}
	t.Log("A→B 已通，bob 离开房间")

	s.LeaveIf("c1", "bob", "bob")

	// 离开后 A 继续发带新前缀的包，B 一个都不应收到
	sentPost := newSentSet()
	postStop := make(chan struct{})
	go alice.sendLoop(postStop, sentPost, "post-leave")
	time.Sleep(1 * time.Second)
	close(postStop)

	if n := bob.countMatching(sentPost); n != 0 {
		t.Fatalf("bob 离开后仍收到 %d 个转发包", n)
	}
	t.Log("bob 离开后未再收到任何转发包，转发已停止")
}

// TestRoomFull 房间人数上限：满员后 HandleOffer 返回 ErrRoomFull。
func TestRoomFull(t *testing.T) {
	s := newTestSFU(t, 2)
	newTestClient(t, s, "c1", "alice")
	newTestClient(t, s, "c1", "bob")

	me := &webrtc.MediaEngine{}
	if err := me.RegisterDefaultCodecs(); err != nil {
		t.Fatal(err)
	}
	pc, err := webrtc.NewAPI(webrtc.WithMediaEngine(me)).NewPeerConnection(webrtc.Configuration{})
	if err != nil {
		t.Fatal(err)
	}
	defer pc.Close()
	offer, err := pc.CreateOffer(nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.HandleOffer("c1", "carol", "carol", offer.SDP, nil); err != ErrRoomFull {
		t.Fatalf("期望 ErrRoomFull，实际 %v", err)
	}
	t.Log("满员房间拒绝第三人加入，返回 ErrRoomFull")
}

// TestTooManyRooms 房间数上限（#20）：建满 MaxRooms 间后拒建新房，已有房间不受影响。
func TestTooManyRooms(t *testing.T) {
	s := newTestSFU(t, 10)
	s.cfg.MaxRooms = 2

	r1, err := s.getOrCreateRoom("r1")
	if err != nil || r1 == nil {
		t.Fatalf("第 1 间应创建成功: %v", err)
	}
	if _, err := s.getOrCreateRoom("r2"); err != nil {
		t.Fatalf("第 2 间应创建成功: %v", err)
	}
	if _, err := s.getOrCreateRoom("r3"); err != ErrTooManyRooms {
		t.Fatalf("第 3 间应 ErrTooManyRooms，实际 %v", err)
	}
	// 已有房间不受影响（复用不占新名额）。
	if r, err := s.getOrCreateRoom("r1"); err != nil || r != r1 {
		t.Fatalf("已有房间应复用: %v", err)
	}
	// MaxRooms=0 不限。
	s.cfg.MaxRooms = 0
	if _, err := s.getOrCreateRoom("r3"); err != nil {
		t.Fatalf("MaxRooms=0 应不限房间数，实际 %v", err)
	}
	t.Log("房间数达到上限拒绝新建，已有房间可复用")
}

// TestLeaveIfConnIDIdentity peer 归属核对（§0.9）：同 room 同 uid 仅一个 peer，
// 新 connID 顶替旧的；LeaveIf 异 connID 无操作、同 connID 才移除并销毁空房。
func TestLeaveIfConnIDIdentity(t *testing.T) {
	s := newTestSFU(t, 10)
	room, err := s.getOrCreateRoom("c1")
	if err != nil {
		t.Fatalf("getOrCreateRoom: %v", err)
	}

	pOld, err := room.addPeer("alice", "conn-1", nil)
	if err != nil {
		t.Fatalf("addPeer conn-1: %v", err)
	}
	pNew, err := room.addPeer("alice", "conn-2", nil) // 同 uid 异 connID：顶替
	if err != nil {
		t.Fatalf("addPeer conn-2: %v", err)
	}
	if pNew == pOld {
		t.Fatal("新 connID 应顶替旧 peer")
	}
	if got := room.getPeer("alice"); got != pNew {
		t.Fatalf("登记的 peer 应为 conn-2 的新 peer")
	}
	<-time.After(100 * time.Millisecond) // 旧 peer 由 go old.close() 异步关闭

	// 旧 connID 清理：身份不符，不动新 peer。
	s.LeaveIf("c1", "alice", "conn-1")
	if got := room.getPeer("alice"); got != pNew {
		t.Fatal("异 connID 的 LeaveIf 误删了新 peer")
	}

	// 新 connID 清理：移除并销毁空房。
	s.LeaveIf("c1", "alice", "conn-2")
	if got := room.getPeer("alice"); got != nil {
		t.Fatal("同 connID 的 LeaveIf 应移除 peer")
	}
	if s.findRoom("c1") != nil {
		t.Fatal("全员离开后房间应被销毁")
	}
}
