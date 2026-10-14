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

// ICEServer 与 rtcapi.ICEServer 字段对齐（本地复刻，避免跨 internal 依赖）。
type ICEServer struct {
	URLs           any    `json:"urls"`
	Username       string `json:"username,omitempty"`
	Credential     string `json:"credential,omitempty"`
	CredentialType string `json:"credentialType,omitempty"`
}

// probeUser 一个真实链路探测端：WS 信令 + Pion PC + 哑 RTP 双向计数。
// 与 loadtest 的差别：token 用真实 JWT，ICE servers 取服务端凭证接口。
type probeUser struct {
	cfg   *config
	id    string
	room  string
	token string
	ice   []ICEServer

	ws *websocket.Conn
	wm sync.Mutex

	pc    *webrtc.PeerConnection
	track *webrtc.TrackLocalStaticRTP

	nm        sync.Mutex
	remoteSet bool
	pending   []webrtc.ICECandidateInit

	connected chan struct{}
	readyOnce sync.Once
	atGate    chan struct{} // 到达发包闸门（已等齐下行轨，正等 sendStart）
	gateOnce  sync.Once

	joinStart   time.Time
	connectedAt time.Time
	sent        atomic.Int64
	wsErr       atomic.Int64
	recvMu      sync.Mutex
	recv        map[string]*atomic.Int64
	tracksUp    atomic.Int64
	trackErr    atomic.Int64
}

func newUser(cfg *config, room, id, token string, ice []ICEServer, others []string) *probeUser {
	// 同房其他成员都是收包来源（下行轨 TrackID=audio-<服务端uid>，room.go 约定）
	recv := make(map[string]*atomic.Int64)
	for _, src := range others {
		recv[src] = &atomic.Int64{}
	}
	return &probeUser{cfg: cfg, id: id, room: room, token: token, ice: ice,
		connected: make(chan struct{}), atGate: make(chan struct{}), recv: recv}
}

func (u *probeUser) run(ctx context.Context, sendStart, sendStop <-chan struct{}) error {
	if err := u.dial(ctx); err != nil {
		return err
	}
	defer u.close()
	go u.readLoop(ctx)

	u.joinStart = time.Now()
	if err := u.wsSend(signal.TypeChannelJoin, signal.ChannelJoinData{ChannelID: u.room}); err != nil {
		return fmt.Errorf("%s channel.join: %w", u.id, err)
	}
	if err := u.startPeer(); err != nil {
		return fmt.Errorf("%s 建连: %w", u.id, err)
	}

	select {
	case <-u.connected:
	case <-ctx.Done():
		return fmt.Errorf("%s 等待 connected 取消", u.id)
	case <-time.After(30 * time.Second):
		return fmt.Errorf("%s 30s 未 connected", u.id)
	}

	deadline := time.Now().Add(10 * time.Second)
	for u.tracksUp.Load() < int64(u.cfg.users-1) && time.Now().Before(deadline) {
		time.Sleep(50 * time.Millisecond)
	}
	u.gateOnce.Do(func() { close(u.atGate) })

	select {
	case <-sendStart:
	case <-ctx.Done():
		return nil
	}
	u.sendLoop(sendStop)
	return nil
}

// ---- 信令 ----

func (u *probeUser) dial(ctx context.Context) error {
	dctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	conn, _, err := websocket.Dial(dctx, fmt.Sprintf("ws://%s/ws?token=%s", u.cfg.addr, u.token), nil)
	if err != nil {
		return fmt.Errorf("%s WS 拨号: %w", u.id, err)
	}
	conn.SetReadLimit(256 * 1024)
	u.ws = conn
	return nil
}

func (u *probeUser) wsSend(msgType string, data any) error {
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
	return nil
}

func (u *probeUser) readLoop(ctx context.Context) {
	for {
		_, msg, err := u.ws.Read(ctx)
		if err != nil {
			return
		}
		var env signal.Envelope
		if err := json.Unmarshal(msg, &env); err != nil {
			continue
		}
		switch env.Type {
		case signal.TypeRTCAnswer:
			var d signal.SDPData
			if env.Decode(&d) == nil {
				u.applyAnswer(d.SDP)
			}
		case signal.TypeRTCOffer:
			var d signal.SDPData
			if env.Decode(&d) == nil {
				u.handleRenegotiation(d.SDP)
			}
		case signal.TypeRTCICE:
			var d webrtc.ICECandidateInit
			if env.Decode(&d) == nil {
				u.applyCandidate(d)
			}
		}
	}
}

// ---- WebRTC ----

func (u *probeUser) startPeer() error {
	me := &webrtc.MediaEngine{}
	if err := me.RegisterDefaultCodecs(); err != nil {
		return err
	}
	se := webrtc.SettingEngine{}
	if u.cfg.loopback {
		se.SetIncludeLoopbackCandidate(true)
	}
	// 服务端下发的 STUN/TURN（生产配置，可能为空/不可达——退 host/prflx）
	var servers []webrtc.ICEServer
	for _, s := range u.ice {
		var urls []string
		switch v := s.URLs.(type) {
		case string:
			urls = []string{v}
		case []any:
			for _, item := range v {
				if str, ok := item.(string); ok {
					urls = append(urls, str)
				}
			}
		case []string:
			urls = v
		}
		servers = append(servers, webrtc.ICEServer{
			URLs: urls, Username: s.Username, Credential: s.Credential,
		})
	}
	pc, err := webrtc.NewAPI(webrtc.WithMediaEngine(me), webrtc.WithSettingEngine(se)).
		NewPeerConnection(webrtc.Configuration{ICEServers: servers})
	if err != nil {
		return err
	}
	u.pc = pc

	track, err := webrtc.NewTrackLocalStaticRTP(
		webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeOpus, ClockRate: 48000, Channels: 2},
		"audio", u.id)
	if err != nil {
		return err
	}
	sender, err := pc.AddTrack(track)
	if err != nil {
		return err
	}
	u.track = track
	// 消费 rtcp.Read 缓冲，防底层反压阻塞写路径
	go func() {
		buf := make([]byte, 1500)
		for {
			if _, _, err := sender.Read(buf); err != nil {
				return
			}
		}
	}()

	pc.OnICECandidate(func(ic *webrtc.ICECandidate) {
		if ic != nil {
			_ = u.wsSend(signal.TypeRTCICE, ic.ToJSON())
		}
	})
	pc.OnConnectionStateChange(func(s webrtc.PeerConnectionState) {
		if s == webrtc.PeerConnectionStateConnected {
			u.readyOnce.Do(func() {
				u.connectedAt = time.Now()
				close(u.connected)
			})
		}
	})
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
				u.trackErr.Add(1)
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

func (u *probeUser) applyAnswer(sdp string) {
	u.nm.Lock()
	defer u.nm.Unlock()
	if err := u.pc.SetRemoteDescription(webrtc.SessionDescription{Type: webrtc.SDPTypeAnswer, SDP: sdp}); err != nil {
		u.wsErr.Add(1)
		return
	}
	u.remoteSet = true
	for _, c := range u.pending {
		_ = u.pc.AddICECandidate(c)
	}
	u.pending = nil
}

func (u *probeUser) handleRenegotiation(sdp string) {
	u.nm.Lock()
	defer u.nm.Unlock()
	if err := u.pc.SetRemoteDescription(webrtc.SessionDescription{Type: webrtc.SDPTypeOffer, SDP: sdp}); err != nil {
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

func (u *probeUser) applyCandidate(c webrtc.ICECandidateInit) {
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

func (u *probeUser) sendLoop(stop <-chan struct{}) {
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
		}); err != nil {
			u.trackErr.Add(1)
		} else {
			u.sent.Add(1)
		}
	}
}

func (u *probeUser) close() {
	if u.pc != nil {
		_ = u.pc.Close()
	}
	if u.ws != nil {
		_ = u.ws.Close(websocket.StatusNormalClosure, "done")
	}
}
