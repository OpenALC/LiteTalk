package rtc

import (
	"bytes"
	"sync"
	"testing"
	"time"

	"github.com/pion/interceptor"
	"github.com/pion/rtp"
	"github.com/pion/webrtc/v4"
)

// fakeSFU 用原始 pion API 模拟服务端 SFU 行为：应答初始 offer、之后作为
// 唯一 offerer 重协商加下行轨、双向 trickle。
type fakeSFU struct {
	pc *webrtc.PeerConnection

	mu       sync.Mutex
	uplink   *webrtc.TrackRemote // 客户端的上行轨
	gotRTP   chan *rtp.Packet
	onTrack  chan struct{}
	skipRead bool // 基准模式：不读上行（ReadRTP 的 Unmarshal 会污染发送侧分配计数）

	downTrack *webrtc.TrackLocalStaticRTP
}

func newFakeSFU(t testing.TB) *fakeSFU {
	t.Helper()
	me := &webrtc.MediaEngine{}
	if err := me.RegisterCodec(webrtc.RTPCodecParameters{
		RTPCodecCapability: webrtc.RTPCodecCapability{
			MimeType:    webrtc.MimeTypeOpus,
			ClockRate:   48000,
			Channels:    2,
			SDPFmtpLine: opusSDPFmtpLine,
		},
		PayloadType: OpusPayloadType,
	}, webrtc.RTPCodecTypeAudio); err != nil {
		t.Fatalf("sfu register codec: %v", err)
	}
	// 与客户端对齐（#33）：协商 RFC 6464 电平扩展，否则 answer 会拒掉它。
	if err := me.RegisterHeaderExtension(webrtc.RTPHeaderExtensionCapability{
		URI: audioLevelURI,
	}, webrtc.RTPCodecTypeAudio); err != nil {
		t.Fatalf("sfu register audio level ext: %v", err)
	}
	ir := &interceptor.Registry{}
	if err := webrtc.RegisterDefaultInterceptors(me, ir); err != nil {
		t.Fatalf("sfu interceptors: %v", err)
	}
	pc, err := webrtc.NewAPI(webrtc.WithMediaEngine(me), webrtc.WithInterceptorRegistry(ir)).
		NewPeerConnection(webrtc.Configuration{})
	if err != nil {
		t.Fatalf("sfu new pc: %v", err)
	}
	s := &fakeSFU{pc: pc, gotRTP: make(chan *rtp.Packet, 64), onTrack: make(chan struct{}, 4)}
	pc.OnTrack(func(track *webrtc.TrackRemote, _ *webrtc.RTPReceiver) {
		s.mu.Lock()
		s.uplink = track
		skip := s.skipRead
		s.mu.Unlock()
		s.onTrack <- struct{}{}
		if skip {
			return
		}
		go func() {
			for {
				pkt, _, err := track.ReadRTP()
				if err != nil {
					return
				}
				s.gotRTP <- pkt
			}
		}()
	})
	t.Cleanup(func() { _ = pc.Close() })
	return s
}

// waitConnected 等两侧连接建立。
func waitConnected(t testing.TB, peer *Peer, sfu *fakeSFU) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if peer.ConnectionState() == webrtc.PeerConnectionStateConnected &&
			sfu.pc.ConnectionState() == webrtc.PeerConnectionStateConnected {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("not connected: client=%s sfu=%s", peer.ConnectionState(), sfu.pc.ConnectionState())
}

// startSFUTrickle 把客户端候选按序转投给模拟 SFU：远端描述就位前的候选先
// 缓冲（pion 在 remote description 未设置时拒绝 AddICECandidate，生产 SFU 的
// pendingCandidates 正是这个语义），SetRemoteDescription(offer) 后调返回的
// flush 按序补投；之后到达的候选仍走实时 trickle。done 在源通道关闭后关闭。
func startSFUTrickle(t *testing.T, pc *webrtc.PeerConnection, src <-chan webrtc.ICECandidateInit) (flush func(), done <-chan struct{}) {
	t.Helper()
	var mu sync.Mutex
	var pending []webrtc.ICECandidateInit
	ready := false
	add := func(c webrtc.ICECandidateInit) {
		if err := pc.AddICECandidate(c); err != nil {
			t.Errorf("sfu add ice: %v", err)
		}
	}
	doneCh := make(chan struct{})
	go func() {
		defer close(doneCh)
		for c := range src {
			mu.Lock()
			if !ready {
				pending = append(pending, c)
				mu.Unlock()
				continue
			}
			add(c) // 持锁调用：与 flush 的补投串行，保持候选顺序
			mu.Unlock()
		}
	}()
	return func() {
		mu.Lock()
		defer mu.Unlock()
		ready = true
		for _, c := range pending {
			add(c)
		}
		pending = nil
	}, doneCh
}

// TestPeerLoopback 本机 loopback pair 全流程：
// 初始 offer/answer → ICE 连通 → 上行 RTP（seq/ts 递增、pt=111）→
// SFU 重协商加下行轨 → 客户端自动 answer → OnTrack → 下行 RTP 到达。
func TestPeerLoopback(t *testing.T) {
	sfu := newFakeSFU(t)

	localSDP := make(chan webrtc.SessionDescription, 4)
	localICE := make(chan webrtc.ICECandidateInit, 64)
	gotTrack := make(chan *webrtc.TrackRemote, 4)

	peer, err := NewPeer(PeerConfig{
		StreamID: "client-A",
		Callbacks: PeerCallbacks{
			OnLocalSDP: func(sdp webrtc.SessionDescription) { localSDP <- sdp },
			OnLocalICE: func(c webrtc.ICECandidateInit) { localICE <- c },
			OnTrack: func(track *webrtc.TrackRemote, _ *webrtc.RTPReceiver) {
				gotTrack <- track
			},
		},
	})
	if err != nil {
		t.Fatalf("NewPeer: %v", err)
	}
	defer peer.Close()

	// 双向 trickle：客户端候选 → SFU；SFU 候选 → 客户端（经 HandleRemoteICE）。
	sfu.pc.OnICECandidate(func(c *webrtc.ICECandidate) {
		if c == nil {
			return
		}
		if err := peer.HandleRemoteICE(c.ToJSON()); err != nil {
			t.Errorf("HandleRemoteICE: %v", err)
		}
	})
	flushICE, iceDone := startSFUTrickle(t, sfu.pc, localICE)

	// 1. 客户端初始 offer → SFU answer。
	if err := peer.Offer(); err != nil {
		t.Fatalf("Offer: %v", err)
	}
	offer := <-localSDP
	if offer.Type != webrtc.SDPTypeOffer {
		t.Fatalf("first local SDP type = %s, want offer", offer.Type)
	}
	if err := sfu.pc.SetRemoteDescription(offer); err != nil {
		t.Fatalf("sfu set remote offer: %v", err)
	}
	flushICE() // offer 就位后补投此前缓冲的客户端候选
	answer, err := sfu.pc.CreateAnswer(nil)
	if err != nil {
		t.Fatalf("sfu create answer: %v", err)
	}
	if err := sfu.pc.SetLocalDescription(answer); err != nil {
		t.Fatalf("sfu set local answer: %v", err)
	}
	if err := peer.HandleRemoteSDP(answer); err != nil {
		t.Fatalf("HandleRemoteSDP(answer): %v", err)
	}

	// 2. ICE 连通。
	waitConnected(t, peer, sfu)

	// 3. 上行 RTP：发 3 包，校验 pt=111、payload 一致、seq 递增、ts 步进 960。
	payloads := [][]byte{
		bytes.Repeat([]byte{0x11}, 80),
		bytes.Repeat([]byte{0x22}, 100),
		bytes.Repeat([]byte{0x33}, 120),
	}
	for _, pl := range payloads {
		if err := peer.SendOpus(pl); err != nil {
			t.Fatalf("SendOpus: %v", err)
		}
	}
	var prev *rtp.Packet
	for i, pl := range payloads {
		select {
		case pkt := <-sfu.gotRTP:
			if pkt.PayloadType != OpusPayloadType {
				t.Fatalf("pkt %d pt = %d, want %d", i, pkt.PayloadType, OpusPayloadType)
			}
			if !bytes.Equal(pkt.Payload, pl) {
				t.Fatalf("pkt %d payload mismatch", i)
			}
			if prev != nil {
				if pkt.SequenceNumber != prev.SequenceNumber+1 {
					t.Fatalf("pkt %d seq = %d, want %d", i, pkt.SequenceNumber, prev.SequenceNumber+1)
				}
				if pkt.Timestamp != prev.Timestamp+rtpTimestampStep {
					t.Fatalf("pkt %d ts = %d, want %d (+%d)", i, pkt.Timestamp, prev.Timestamp+rtpTimestampStep, rtpTimestampStep)
				}
			}
			prev = pkt
		case <-time.After(5 * time.Second):
			t.Fatalf("timeout waiting uplink pkt %d", i)
		}
	}
	if got := peer.Stats.PacketsSent.Load(); got != 3 {
		t.Fatalf("PacketsSent = %d, want 3", got)
	}

	// 4. 静音 = 停发（DTX 语义）。
	peer.SetMuted(true)
	if err := peer.SendOpus(payloads[0]); err != nil {
		t.Fatalf("SendOpus muted: %v", err)
	}
	if got := peer.Stats.MutedDropped.Load(); got != 1 {
		t.Fatalf("MutedDropped = %d, want 1", got)
	}
	select {
	case pkt := <-sfu.gotRTP:
		t.Fatalf("received pkt while muted: seq %d", pkt.SequenceNumber)
	case <-time.After(300 * time.Millisecond):
	}
	peer.SetMuted(false)

	// 5. SFU 加下行轨并重协商：客户端应自动 answer（不主动 offer）。
	downTrack, err := webrtc.NewTrackLocalStaticRTP(
		webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeOpus, ClockRate: 48000, Channels: 2},
		"audio-down", "peer-B", // streamID = 说话成员 user_id
	)
	if err != nil {
		t.Fatalf("sfu new down track: %v", err)
	}
	if _, err := sfu.pc.AddTrack(downTrack); err != nil {
		t.Fatalf("sfu add track: %v", err)
	}
	renegOffer, err := sfu.pc.CreateOffer(nil)
	if err != nil {
		t.Fatalf("sfu reneg offer: %v", err)
	}
	if err := sfu.pc.SetLocalDescription(renegOffer); err != nil {
		t.Fatalf("sfu set local reneg offer: %v", err)
	}
	// 客户端收到 SFU offer → 内部 CreateAnswer → OnLocalSDP(answer)。
	if err := peer.HandleRemoteSDP(renegOffer); err != nil {
		t.Fatalf("HandleRemoteSDP(offer): %v", err)
	}
	select {
	case sdp := <-localSDP:
		if sdp.Type != webrtc.SDPTypeAnswer {
			t.Fatalf("local SDP type = %s, want answer", sdp.Type)
		}
		if err := sfu.pc.SetRemoteDescription(sdp); err != nil {
			t.Fatalf("sfu set remote answer: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timeout waiting auto answer")
	}
	if got := peer.Stats.Renegotiation.Load(); got != 1 {
		t.Fatalf("Renegotiation = %d, want 1", got)
	}

	// 6. SFU 下行持续发包（pion 在首个匹配 RTP 到达时触发 OnTrack）；
	//    客户端 OnTrack 应触发且 StreamID 可识别成员。
	downPayload := bytes.Repeat([]byte{0x44}, 90)
	stopSend := make(chan struct{})
	defer close(stopSend)
	go func() {
		var seq uint16
		var ts uint32
		for {
			select {
			case <-stopSend:
				return
			default:
			}
			seq++
			ts += rtpTimestampStep
			_ = downTrack.WriteRTP(&rtp.Packet{
				Header:  rtp.Header{Version: 2, SequenceNumber: seq, Timestamp: ts, SSRC: 12345},
				Payload: downPayload,
			}) // 绑定/连通前失败属正常，重试
			time.Sleep(20 * time.Millisecond)
		}
	}()
	var track *webrtc.TrackRemote
	select {
	case track = <-gotTrack:
	case <-time.After(8 * time.Second):
		t.Fatal("timeout waiting OnTrack")
	}
	if track.StreamID() != "peer-B" {
		t.Fatalf("down track StreamID = %q, want peer-B", track.StreamID())
	}

	// 7. 客户端 TrackRemote 可读到下行 RTP。
	if err := track.SetReadDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatalf("set read deadline: %v", err)
	}
	got, _, err := track.ReadRTP()
	if err != nil {
		t.Fatalf("client read downlink rtp: %v", err)
	}
	if !bytes.Equal(got.Payload, downPayload) {
		t.Fatalf("downlink payload mismatch: got %d bytes", len(got.Payload))
	}

	close(localICE)
	<-iceDone
}

// TestPeerSendDTXHeartbeat 发送端照常发送 DTX 心跳包：≤2 字节载荷进入 RTP，
// seq/时戳逐包连续（接收端 jitter 不判空洞）；用户主动静音仍然整包丢弃。
func TestPeerSendDTXHeartbeat(t *testing.T) {
	sfu := newFakeSFU(t)

	localSDP := make(chan webrtc.SessionDescription, 4)
	localICE := make(chan webrtc.ICECandidateInit, 64)
	peer, err := NewPeer(PeerConfig{
		StreamID: "client-A",
		Callbacks: PeerCallbacks{
			OnLocalSDP: func(sdp webrtc.SessionDescription) { localSDP <- sdp },
			OnLocalICE: func(c webrtc.ICECandidateInit) { localICE <- c },
		},
	})
	if err != nil {
		t.Fatalf("NewPeer: %v", err)
	}
	defer peer.Close()

	sfu.pc.OnICECandidate(func(c *webrtc.ICECandidate) {
		if c == nil {
			return
		}
		if err := peer.HandleRemoteICE(c.ToJSON()); err != nil {
			t.Errorf("HandleRemoteICE: %v", err)
		}
	})
	flushICE, iceDone := startSFUTrickle(t, sfu.pc, localICE)

	if err := peer.Offer(); err != nil {
		t.Fatalf("Offer: %v", err)
	}
	offer := <-localSDP
	if err := sfu.pc.SetRemoteDescription(offer); err != nil {
		t.Fatalf("sfu set remote offer: %v", err)
	}
	flushICE() // offer 就位后补投此前缓冲的客户端候选
	answer, err := sfu.pc.CreateAnswer(nil)
	if err != nil {
		t.Fatalf("sfu create answer: %v", err)
	}
	if err := sfu.pc.SetLocalDescription(answer); err != nil {
		t.Fatalf("sfu set local answer: %v", err)
	}
	if err := peer.HandleRemoteSDP(answer); err != nil {
		t.Fatalf("HandleRemoteSDP(answer): %v", err)
	}
	waitConnected(t, peer, sfu)

	speech := bytes.Repeat([]byte{0xAB}, 80)
	dtx := []byte{0x00}
	if err := peer.SendOpus(speech); err != nil {
		t.Fatalf("SendOpus speech1: %v", err)
	}
	for i := 0; i < 3; i++ {
		if err := peer.SendOpus(dtx); err != nil {
			t.Fatalf("SendOpus dtx %d: %v", i, err)
		}
	}
	if err := peer.SendOpus(speech); err != nil {
		t.Fatalf("SendOpus speech2: %v", err)
	}

	// 5 个包全部到达：seq/时戳逐包 +1/+960，DTX 载荷原样转发。
	wantPayloads := [][]byte{speech, dtx, dtx, dtx, speech}
	var prev *rtp.Packet
	for i, want := range wantPayloads {
		var pkt *rtp.Packet
		select {
		case pkt = <-sfu.gotRTP:
		case <-time.After(5 * time.Second):
			t.Fatalf("timeout waiting uplink pkt %d", i)
		}
		if !bytes.Equal(pkt.Payload, want) {
			t.Fatalf("pkt %d payload len=%d, want len=%d", i, len(pkt.Payload), len(want))
		}
		if prev != nil {
			if pkt.SequenceNumber != prev.SequenceNumber+1 {
				t.Fatalf("pkt %d seq = %d, want %d", i, pkt.SequenceNumber, prev.SequenceNumber+1)
			}
			if pkt.Timestamp != prev.Timestamp+rtpTimestampStep {
				t.Fatalf("pkt %d ts = %d, want %d (+%d)", i, pkt.Timestamp, prev.Timestamp+rtpTimestampStep, rtpTimestampStep)
			}
		}
		prev = pkt
	}
	if got := peer.Stats.PacketsSent.Load(); got != 5 {
		t.Fatalf("PacketsSent = %d, want 5", got)
	}
	if got := peer.Stats.SentDTX.Load(); got != 3 {
		t.Fatalf("SentDTX = %d, want 3", got)
	}

	// 用户主动静音：连 DTX 心跳也不发，计数为静音丢弃。
	peer.SetMuted(true)
	if err := peer.SendOpus(dtx); err != nil {
		t.Fatalf("SendOpus muted dtx: %v", err)
	}
	if got := peer.Stats.MutedDropped.Load(); got != 1 {
		t.Fatalf("MutedDropped = %d, want 1", got)
	}
	if got := peer.Stats.SentDTX.Load(); got != 3 {
		t.Fatalf("SentDTX after mute = %d, want 3", got)
	}
	select {
	case pkt := <-sfu.gotRTP:
		t.Fatalf("received pkt while muted: seq %d", pkt.SequenceNumber)
	case <-time.After(300 * time.Millisecond):
	}
	peer.SetMuted(false)

	close(localICE)
	<-iceDone
}

// TestPeerPendingICE 远端描述就位前到达的候选应排队不报错。
func TestPeerPendingICE(t *testing.T) {
	peer, err := NewPeer(PeerConfig{})
	if err != nil {
		t.Fatalf("NewPeer: %v", err)
	}
	defer peer.Close()

	cand := webrtc.ICECandidateInit{Candidate: "candidate:1 1 udp 2130706431 127.0.0.1 9999 typ host"}
	if err := peer.HandleRemoteICE(cand); err != nil {
		t.Fatalf("HandleRemoteICE before remote desc: %v", err)
	}
	peer.remoteMu.Lock()
	queued := len(peer.pendingICE)
	peer.remoteMu.Unlock()
	if queued != 1 {
		t.Fatalf("pending ICE = %d, want 1", queued)
	}
}

// TestSetFrameMSRTPTimestampStep 帧长 → RTP 时戳步进（#46）：20ms=960、10ms=480；
// 非法值拒绝且不改当前步进。
func TestSetFrameMSRTPTimestampStep(t *testing.T) {
	p := &Peer{}
	if err := p.SetFrameMS(10); err != nil {
		t.Fatalf("SetFrameMS(10): %v", err)
	}
	if got := p.tsStep.Load(); got != 480 {
		t.Fatalf("10ms 时戳步进 = %d, want 480", got)
	}
	if err := p.SetFrameMS(20); err != nil {
		t.Fatalf("SetFrameMS(20): %v", err)
	}
	if got := p.tsStep.Load(); got != rtpTimestampStep {
		t.Fatalf("20ms 时戳步进 = %d, want %d", got, rtpTimestampStep)
	}
	if err := p.SetFrameMS(15); err == nil {
		t.Fatal("非法帧长应报错")
	}
	if got := p.tsStep.Load(); got != rtpTimestampStep {
		t.Fatalf("非法值不应改变步进: %d", got)
	}
	if got := timestampStepForMS(10); got != 480 {
		t.Fatalf("timestampStepForMS(10) = %d, want 480", got)
	}
}
