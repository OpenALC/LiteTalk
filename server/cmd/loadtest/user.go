package main

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/OpenALC/LiteTalk/server/internal/signal"
	"github.com/coder/websocket"
	"github.com/pion/rtp"
	"github.com/pion/webrtc/v4"
)

// loadUser 一个压测用户桩：完整真实链路——WS 信令 + Pion PeerConnection +
// 50pps 哑 RTP 上行 + 下行收包计数。用户 ID 形如 <base>-<room>-<idx>，
// dev-token 模式下 token = dev-<userID>（服务端需 LITETALK_DEV_AUTH=1）。
type loadUser struct {
	cfg  *config
	id   string // 用户 ID（dev-token 名）
	room string // 房间 ID

	ws *websocket.Conn
	wm sync.Mutex // coder/websocket 单写者约束

	pc    *webrtc.PeerConnection
	track *webrtc.TrackLocalStaticRTP

	nm        sync.Mutex // 协商互斥（初始 offer 与 SFU 重协商串行化）
	remoteSet bool       // 远端描述是否就位
	pending   []webrtc.ICECandidateInit

	connected chan struct{} // PC connected（关闭一次）
	readyOnce sync.Once

	// 指标
	joinStart   time.Time
	connectedAt time.Time
	sent        atomic.Int64
	wsIn        atomic.Int64
	wsOut       atomic.Int64
	wsErr       atomic.Int64
	recvMu      sync.Mutex
	recv        map[string]*atomic.Int64 // 源用户 ID → 收包数
	tracksUp    atomic.Int64
	trackErr    atomic.Int64
}

func newUser(cfg *config, room string, roomIdx int, id string) *loadUser {
	recv := make(map[string]*atomic.Int64)
	// 同房其他成员都是收包来源（下行轨 TrackID=audio-<srcID>）；ID 约定与 main 一致：<base>-<房号>-<序号>
	for i := 0; i < cfg.users; i++ {
		src := fmt.Sprintf("%s-%d-%d", cfg.base(), roomIdx, i)
		if src != id {
			recv[src] = &atomic.Int64{}
		}
	}
	return &loadUser{
		cfg:       cfg,
		id:        id,
		room:      room,
		connected: make(chan struct{}),
		recv:      recv,
	}
}

// run 全流程：WS 连接 → 入房 → 初始 offer/answer → 等 connected →
// 等下行轨齐 → 等发包开始信号 → 50pps 发到 stop → 报告收流。
func (u *loadUser) run(ctx context.Context, sendStart <-chan struct{}, sendStop <-chan struct{}) error {
	if err := u.dial(ctx); err != nil {
		return err
	}
	defer u.close()

	// 信令读循环
	go u.readLoop(ctx)

	// 入房
	u.joinStart = time.Now()
	if err := u.wsSend(signal.TypeChannelJoin, signal.ChannelJoinData{ChannelID: u.room}); err != nil {
		return fmt.Errorf("%s channel.join: %w", u.id, err)
	}

	// 建 PC、挂上行轨、发起初始 offer
	if err := u.startPeer(); err != nil {
		return fmt.Errorf("%s 建连: %w", u.id, err)
	}

	// 等 connected（房间建立时延终点）
	select {
	case <-u.connected:
	case <-ctx.Done():
		return fmt.Errorf("%s 等待 connected 超时", u.id)
	case <-time.After(30 * time.Second):
		return fmt.Errorf("%s 30s 未 connected", u.id)
	}

	// 等下行轨齐（users-1 条）；重协商由 SFU 推 rtc.offer 驱动
	deadline := time.Now().Add(10 * time.Second)
	for u.tracksUp.Load() < int64(u.cfg.users-1) && time.Now().Before(deadline) {
		time.Sleep(50 * time.Millisecond)
	}

	// 发包阶段
	select {
	case <-sendStart:
	case <-ctx.Done():
		return nil
	}
	u.sendLoop(sendStop)
	return nil
}

// ---- 信令 ----

func (u *loadUser) dial(ctx context.Context) error {
	dctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	url := fmt.Sprintf("ws://%s/ws?token=%s", u.cfg.addr, u.token())
	conn, _, err := websocket.Dial(dctx, url, nil)
	if err != nil {
		return fmt.Errorf("%s WS 拨号: %w", u.id, err)
	}
	conn.SetReadLimit(256 * 1024)
	u.ws = conn
	return nil
}

func (u *loadUser) token() string { return "dev-" + u.id }

func (u *loadUser) wsSend(msgType string, data any) error {
	raw, err := signal.MarshalEnvelope(msgType, data)
	if err != nil {
		return err
	}
	u.wm.Lock()
	defer u.wm.Unlock()
	wctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := u.ws.Write(wctx, websocket.MessageText, raw); err != nil {
		u.wsErr.Add(1)
		return err
	}
	u.wsOut.Add(1)
	return nil
}

// readLoop 收信令分发：answer / 重协商 offer / trickle ice / 其余计数。
func (u *loadUser) readLoop(ctx context.Context) {
	for {
		_, msg, err := u.ws.Read(ctx)
		if err != nil {
			return // 断连即结束
		}
		u.wsIn.Add(1)
		var env signal.Envelope
		if err := json.Unmarshal(msg, &env); err != nil {
			continue
		}
		switch env.Type {
		case signal.TypeRTCAnswer:
			var d signal.SDPData
			if env.Decode(&d) != nil {
				continue
			}
			u.applyAnswer(d.SDP)
		case signal.TypeRTCOffer:
			var d signal.SDPData
			if env.Decode(&d) != nil {
				continue
			}
			u.handleRenegotiation(d.SDP)
		case signal.TypeRTCIce:
			var d webrtc.ICECandidateInit
			if env.Decode(&d) != nil {
				continue
			}
			u.applyCandidate(d)
		}
	}
}

// ---- WebRTC ----

func (u *loadUser) startPeer() error {
	me := &webrtc.MediaEngine{}
	if err := me.RegisterDefaultCodecs(); err != nil {
		return err
	}
	se := webrtc.SettingEngine{}
	if u.cfg.loopback {
		// 服务端在 127.0.0.1 时 pion 默认排除环回候选，压测本机需放开
		se.SetIncludeLoopbackCandidate(true)
	}
	pc, err := webrtc.NewAPI(webrtc.WithMediaEngine(me), webrtc.WithSettingEngine(se)).
		NewPeerConnection(webrtc.Configuration{})
	if err != nil {
		return err
	}
	u.pc = pc

	// 上行哑轨（Opus 声明即可，payload 不需真实编码；SFU 不解码）
	track, err := webrtc.NewTrackLocalStaticRTP(
		webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeOpus, ClockRate: 48000, Channels: 2},
		"audio", u.id,
	)
	if err != nil {
		return err
	}
	if _, err := pc.AddTrack(track); err != nil {
		return err
	}
	u.track = track

	pc.OnICECandidate(func(ic *webrtc.ICECandidate) {
		if ic == nil {
			return
		}
		_ = u.wsSend(signal.TypeRTCIce, ic.ToJSON())
	})
	pc.OnConnectionStateChange(func(s webrtc.PeerConnectionState) {
		if s == webrtc.PeerConnectionStateConnected {
			u.readyOnce.Do(func() {
				u.connectedAt = time.Now()
				close(u.connected)
			})
		}
	})
	// 下行：TrackID=audio-<srcID>（room.go 约定），逐包计数
	pc.OnTrack(func(remote *webrtc.TrackRemote, _ *webrtc.RTPReceiver) {
		u.tracksUp.Add(1)
		src := strings.TrimPrefix(remote.ID(), "audio-")
		u.recvMu.Lock()
		counter := u.recv[src]
		u.recvMu.Unlock()
		for {
			_, _, err := remote.ReadRTP()
			if err != nil {
				return
			}
			if counter != nil {
				counter.Add(1)
			} else {
				u.trackErr.Add(1) // 未知来源轨（协议错位信号）
			}
		}
	})

	offer, err := pc.CreateOffer(nil)
	if err != nil {
		return err
	}
	if err := pc.SetLocalDescription(offer); err != nil {
		return err
	}
	return u.wsSend(signal.TypeRTCOffer, signal.SDPData{SDP: offer.SDP})
}

func (u *loadUser) applyAnswer(sdp string) {
	u.nm.Lock()
	defer u.nm.Unlock()
	if err := u.pc.SetRemoteDescription(webrtc.SessionDescription{
		Type: webrtc.SDPTypeAnswer, SDP: sdp,
	}); err != nil {
		u.wsErr.Add(1)
		return
	}
	u.remoteSet = true
	for _, c := range u.pending {
		_ = u.pc.AddICECandidate(c)
	}
	u.pending = nil
}

// handleRenegotiation 应答 SFU 侧 rtc.offer（新增下行轨时触发）。
func (u *loadUser) handleRenegotiation(sdp string) {
	u.nm.Lock()
	defer u.nm.Unlock()
	if err := u.pc.SetRemoteDescription(webrtc.SessionDescription{
		Type: webrtc.SDPTypeOffer, SDP: sdp,
	}); err != nil {
		u.wsErr.Add(1)
		return
	}
	answer, err := u.pc.CreateAnswer(nil)
	if err != nil {
		u.wsErr.Add(1)
		return
	}
	if err := u.pc.SetLocalDescription(answer); err != nil {
		u.wsErr.Add(1)
		return
	}
	_ = u.wsSend(signal.TypeRTCAnswer, signal.SDPData{SDP: answer.SDP})
}

func (u *loadUser) applyCandidate(c webrtc.ICECandidateInit) {
	u.nm.Lock()
	if !u.remoteSet {
		u.pending = append(u.pending, c)
		u.nm.Unlock()
		return
	}
	u.nm.Unlock()
	_ = u.pc.AddICECandidate(c)
}

// ---- 媒体 ----

// sendLoop 50pps 哑 RTP（80B payload ≈ 32kbps Opus 帧长），seq/ts 按 20ms 步进。
func (u *loadUser) sendLoop(stop <-chan struct{}) {
	ticker := time.NewTicker(20 * time.Millisecond)
	defer ticker.Stop()
	payload := make([]byte, 80)
	seq := uint16(1000)
	ts := uint32(10000)
	for {
		select {
		case <-stop:
			return
		case <-ticker.C:
		}
		seq++
		ts += 960
		if err := u.track.WriteRTP(&rtp.Packet{
			Header:  rtp.Header{Version: 2, SequenceNumber: seq, Timestamp: ts},
			Payload: payload,
		}); err == nil {
			u.sent.Add(1)
		} else {
			u.trackErr.Add(1)
		}
	}
}

func (u *loadUser) close() {
	if u.pc != nil {
		_ = u.pc.Close()
	}
	if u.ws != nil {
		_ = u.ws.Close(websocket.StatusNormalClosure, "done")
	}
}
