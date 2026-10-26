package signal

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/pion/rtp"
	"github.com/pion/webrtc/v4"
)

// #45 回归：voice.whisper 早于 rtc.offer 到达（入房后立刻按住）时，目标是
// 暂存在 hub 的；rtc.offer 建立 SFU peer 后必须补应用，首个上行包只能到目标。

// readLoop 单读者把 WS 信封送入 in。coder/websocket 的 Read 只能靠 ctx 取消，
// 取消会关闭连接——不能为「带超时读」反复取消 ctx，故由一个长读 goroutine
// 供 pump 消费。
func (p *rtcProbe) readLoop(initial []Envelope) {
	for _, env := range initial {
		select {
		case p.in <- env:
		case <-p.done:
			return
		}
	}
	for {
		_, data, err := p.ws.conn.Read(context.Background())
		if err != nil {
			close(p.in)
			return
		}
		var env Envelope
		if json.Unmarshal(data, &env) != nil {
			continue
		}
		select {
		case p.in <- env:
		case <-p.done:
			return
		}
	}
}

// trySend 非致命写（Pion 回调 goroutine 里不能用 wsClient.send：后者会 t.Fatalf）。
func (c *wsClient) trySend(msgType string, data any) {
	raw, err := MarshalEnvelope(msgType, data)
	if err != nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	_ = c.conn.Write(ctx, websocket.MessageText, raw)
}

// rtcProbe 通过 hub 的真实 WS 信令建立 Pion PeerConnection。
type rtcProbe struct {
	ws    *wsClient
	pc    *webrtc.PeerConnection
	track *webrtc.TrackLocalStaticRTP

	opMu         sync.Mutex // 串行化 PC 操作（初始 answer 与 SFU 重协商 offer）
	remoteSet    bool
	pendingCands []webrtc.ICECandidateInit

	// localMu/offered/localCands 保证 trickle 候选在 rtc.offer 之后才写 WS：
	// 候选先于 offer 到达服务端会因 SFU peer 尚未创建而被丢弃，ICE 直接失败。
	localMu    sync.Mutex
	offered    bool
	localCands []webrtc.ICECandidateInit

	mu       sync.Mutex
	received []*rtp.Packet

	seq  atomic.Uint32
	in   chan Envelope
	done chan struct{}
}

func newRTCProbe(t *testing.T, ws *wsClient) *rtcProbe {
	t.Helper()
	me := &webrtc.MediaEngine{}
	if err := me.RegisterDefaultCodecs(); err != nil {
		t.Fatal(err)
	}
	se := webrtc.SettingEngine{}
	se.SetIncludeLoopbackCandidate(true)
	pc, err := webrtc.NewAPI(webrtc.WithMediaEngine(me), webrtc.WithSettingEngine(se)).NewPeerConnection(webrtc.Configuration{})
	if err != nil {
		t.Fatalf("NewPeerConnection: %v", err)
	}
	p := &rtcProbe{ws: ws, pc: pc, in: make(chan Envelope, 64), done: make(chan struct{})}
	t.Cleanup(func() {
		close(p.done)
		_ = pc.Close()
	})
	initial := ws.pending
	ws.pending = nil
	go p.readLoop(initial)

	track, err := webrtc.NewTrackLocalStaticRTP(
		webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeOpus, ClockRate: 48000, Channels: 2},
		"audio", "stub-"+ws.user,
	)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pc.AddTrack(track); err != nil {
		t.Fatal(err)
	}
	p.track = track
	p.seq.Store(100)

	pc.OnICECandidate(func(c *webrtc.ICECandidate) {
		if c == nil {
			return
		}
		ci := c.ToJSON()
		p.localMu.Lock()
		if !p.offered {
			p.localCands = append(p.localCands, ci)
			p.localMu.Unlock()
			return
		}
		p.localMu.Unlock()
		ws.trySend(TypeRTCIce, fromICECandidate(ci))
	})
	pc.OnTrack(func(remote *webrtc.TrackRemote, _ *webrtc.RTPReceiver) {
		ws.t.Logf("[%s] OnTrack ssrc=%d", ws.user, remote.SSRC())
		go func() {
			for {
				pkt, _, err := remote.ReadRTP()
				if err != nil {
					return
				}
				p.mu.Lock()
				p.received = append(p.received, pkt)
				p.mu.Unlock()
			}
		}()
	})
	pc.OnICEConnectionStateChange(func(s webrtc.ICEConnectionState) {
		ws.t.Logf("[%s] ICE state=%s", ws.user, s)
	})
	pc.OnConnectionStateChange(func(s webrtc.PeerConnectionState) {
		ws.t.Logf("[%s] PC state=%s", ws.user, s)
	})

	offer, err := pc.CreateOffer(nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := pc.SetLocalDescription(offer); err != nil {
		t.Fatal(err)
	}
	// 先发 offer，再放行候选（否则候选可能先到，SFU 找不到 peer 直接丢弃）。
	ws.trySend(TypeRTCOffer, SDPData{SDP: offer.SDP})
	p.localMu.Lock()
	p.offered = true
	cands := p.localCands
	p.localCands = nil
	p.localMu.Unlock()
	for _, ci := range cands {
		ws.trySend(TypeRTCIce, fromICECandidate(ci))
	}
	go p.pump()
	return p
}

// pump 处理 rtc.answer / SFU 重协商 rtc.offer / trickle rtc.ice。
func (p *rtcProbe) pump() {
	for {
		select {
		case <-p.done:
			return
		case env, ok := <-p.in:
			if !ok {
				return
			}
			p.handle(env)
		}
	}
}

// handle 串行处理一个下行信封（PC 操作必须单线程化，避免 offer glare）。
func (p *rtcProbe) handle(env Envelope) {
	switch env.Type {
	case TypeRTCAnswer:
		p.ws.t.Logf("[%s] 收到 rtc.answer", p.ws.user)
		var d SDPData
		if env.Decode(&d) != nil || d.SDP == "" {
			return
		}
		p.opMu.Lock()
		if p.pc.SetRemoteDescription(webrtc.SessionDescription{Type: webrtc.SDPTypeAnswer, SDP: d.SDP}) == nil {
			p.remoteSet = true
			for _, ci := range p.pendingCands {
				_ = p.pc.AddICECandidate(ci)
			}
			p.pendingCands = nil
		}
		p.opMu.Unlock()
	case TypeRTCOffer:
		var d SDPData
		if env.Decode(&d) != nil || d.SDP == "" {
			return
		}
		p.opMu.Lock()
		if p.pc.SetRemoteDescription(webrtc.SessionDescription{Type: webrtc.SDPTypeOffer, SDP: d.SDP}) == nil {
			answer, err := p.pc.CreateAnswer(nil)
			if err == nil && p.pc.SetLocalDescription(answer) == nil {
				p.ws.trySend(TypeRTCAnswer, SDPData{SDP: answer.SDP})
			}
		}
		p.opMu.Unlock()
	case TypeRTCIce:
		var d ICEData
		if env.Decode(&d) != nil {
			return
		}
		p.ws.t.Logf("[%s] 收到 rtc.ice", p.ws.user)
		p.opMu.Lock()
		if p.remoteSet {
			_ = p.pc.AddICECandidate(toICECandidate(d))
		} else {
			p.pendingCands = append(p.pendingCands, toICECandidate(d))
		}
		p.opMu.Unlock()
	}
}

// waitConnected 等 ICE/DTLS 就绪（候选通过 pump 交换）。
func (p *rtcProbe) waitConnected(t *testing.T) {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		switch p.pc.ConnectionState() {
		case webrtc.PeerConnectionStateConnected:
			return
		case webrtc.PeerConnectionStateFailed, webrtc.PeerConnectionStateClosed:
			t.Fatalf("PeerConnection 失败（user=%s, state=%s）", p.ws.user, p.pc.ConnectionState())
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("PeerConnection 20s 内未连接（user=%s, state=%s）", p.ws.user, p.pc.ConnectionState())
}

// sendLoop 以 50pps 发送带唯一前缀的 RTP，直到 stop 关闭。
func (p *rtcProbe) sendLoop(stop <-chan struct{}, prefix string) {
	for i := 0; ; i++ {
		select {
		case <-stop:
			return
		default:
		}
		seq := uint16(p.seq.Add(1))
		_ = p.track.WriteRTP(&rtp.Packet{
			Header:  rtp.Header{Version: 2, SequenceNumber: seq, Timestamp: uint32(seq) * 960},
			Payload: []byte(fmt.Sprintf("%s-%d", prefix, i)),
		})
		time.Sleep(20 * time.Millisecond)
	}
}

func (p *rtcProbe) countPrefix(prefix string) int {
	p.mu.Lock()
	defer p.mu.Unlock()
	n := 0
	for _, pkt := range p.received {
		if strings.HasPrefix(string(pkt.Payload), prefix) {
			n++
		}
	}
	return n
}

func (p *rtcProbe) waitPrefix(prefix string, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if p.countPrefix(prefix) > 0 {
			return true
		}
		time.Sleep(10 * time.Millisecond)
	}
	return p.countPrefix(prefix) > 0
}

// TestWhisperAppliedAfterPeerCreation voice.whisper 先于 rtc.offer：hub 暂存，
// peer 建立后补应用；首包只到目标，非目标零包；取消后恢复全房。
func TestWhisperAppliedAfterPeerCreation(t *testing.T) {
	f := newChatFixture(t)
	vchID := strconv.FormatInt(f.vch.ID, 10)

	a := f.dial("1", "alice")
	b := f.dial("2", "bob")
	c := f.dial("9", "mallory")
	for _, cl := range []*wsClient{a, b, c} {
		joinAndCapture(t, cl, vchID)
	}

	// 1) 在任何 rtc.offer 之前下发目标：hub 必须暂存，SFU 此时没有 peer。
	a.send(TypeVoiceWhisper, VoiceWhisperData{Targets: []string{"2"}})
	waitHubWhisper(t, f.hub, vchID, "1", true, "2")

	// 2) 依次通过真实 hub 信令建 RTC；alice 的 rtc.offer 会触发补应用。
	ap := newRTCProbe(t, a)
	ap.waitConnected(t)
	bp := newRTCProbe(t, b)
	bp.waitConnected(t)
	cp := newRTCProbe(t, c)
	cp.waitConnected(t)
	time.Sleep(200 * time.Millisecond) // 让重协商补轨道完成

	stopWhisper := make(chan struct{})
	go ap.sendLoop(stopWhisper, "whisper-first")
	if !bp.waitPrefix("whisper-first", 15*time.Second) {
		close(stopWhisper)
		t.Fatal("目标 bob 未收到悄悄话首包（hub 补应用失效）")
	}
	time.Sleep(500 * time.Millisecond)
	if n := cp.countPrefix("whisper-first"); n != 0 {
		close(stopWhisper)
		t.Fatalf("非目标 mallory 收到 %d 个悄悄话包", n)
	}
	close(stopWhisper)

	// 3) 取消后恢复普通全房转发，确认非目标链路本身是通的。
	a.send(TypeVoiceWhisper, VoiceWhisperData{Targets: []string{}})
	waitHubWhisper(t, f.hub, vchID, "1", false, "")
	stopNormal := make(chan struct{})
	defer close(stopNormal)
	go ap.sendLoop(stopNormal, "normal")
	if !cp.waitPrefix("normal", 15*time.Second) {
		t.Fatal("取消悄悄话后非目标未恢复收到 RTP")
	}
}
