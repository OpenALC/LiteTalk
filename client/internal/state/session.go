package state

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"sync"
	"sync/atomic"

	"github.com/OpenALC/LiteTalk/client/internal/audio"
	"github.com/OpenALC/LiteTalk/client/internal/rtc"
	"github.com/OpenALC/LiteTalk/client/internal/signal"
	"github.com/pion/webrtc/v4"
)

// Events 会话 → UI 的事件回调（由 app 层接到 runtime.EventsEmit）。
type Events struct {
	// OnPeerJoin 有成员进入房间（含自己首次入房的成员全量推送前的逐个通知）。
	OnPeerJoin func(userID, nickname string, speaking, mute, deafen bool)
	// OnPeerLeave 有成员离开。
	OnPeerLeave func(userID string)
	// OnPeerSpeaking 远端说话状态变化。
	OnPeerSpeaking func(userID string, speaking, mute, deafen bool)
	// OnLatency 回环/远端延迟周期上报（毫秒）。
	OnLatency func(ms int)
	// OnVoiceStats 周期语音统计（JSON，字段见 VoiceStats）。
	OnVoiceStats func(statsJSON string)
	// OnConnection 信令/RTC 连接状态变化。
	OnConnection func(state string)
	// OnSelf 入房后回传自己的 user_id（UI 标记「我」）。
	OnSelf func(userID string)
}

// VoiceStats 推给 UI 的语音房统计快照。
type VoiceStats struct {
	LatencyMS  int      `json:"latency_ms"`
	Members    int      `json:"members"`
	PeerIDs    []string `json:"peer_ids"`
	EncPackets uint64   `json:"enc_packets"`
	RtpSent    uint64   `json:"rtp_sent"`
}

// PeerView UI 侧成员视图。
type PeerView struct {
	UserID   string `json:"user_id"`
	Nickname string `json:"nickname"`
	Speaking bool   `json:"speaking"`
	Mute     bool   `json:"mute"`
	Deafen   bool   `json:"deafen"`
}

// Session 一次登录后的通话会话：信令连接 + 媒体管线。
type Session struct {
	api   *APIClient
	log   *log.Logger
	ev    Events
	token string // 信令令牌（access token 或 dev-<name>）
	name  string

	ws        *signal.Client
	pipe      *audio.Pipeline
	mixer     *audio.Mixer
	remoteMgr *audio.RemoteManager
	states    *audio.StateTable

	peerRef atomic.Pointer[rtc.Peer]
	roomID  atomic.Value // string

	mu       sync.Mutex
	members  map[string]PeerView
	selfID   string
	muted    atomic.Bool
	deafened atomic.Bool

	cancel context.CancelFunc
	wg     sync.WaitGroup
}

// NewSession 创建会话。token 为信令鉴权令牌；nickname 仅 dev 模式用作 user_id。
func NewSession(api *APIClient, token, nickname string, ev Events, logger *log.Logger) *Session {
	if logger == nil {
		logger = log.Default()
	}
	s := &Session{
		api:     api,
		log:     logger,
		ev:      ev,
		token:   token,
		name:    nickname,
		members: make(map[string]PeerView),
	}
	s.roomID.Store("")
	return s
}

// Connect 建立信令连接并启动媒体管线（未入房，等 JoinChannel）。
func (s *Session) Connect(serverURL string) error {
	wsCfg := signal.Config{
		URL:   serverURL,
		Token: s.token,
		Logger: s.log,
	}
	wsCfg.OnConnect = func(c *signal.Client) {
		if s.ev.OnConnection != nil {
			s.ev.OnConnection("connected")
		}
		// 重连后若仍在房间，重发 join（channel.joined 会重建媒体链路）。
		if room, _ := s.roomID.Load().(string); room != "" {
			_ = c.Send(signal.TypeChannelJoin, signal.ChannelJoinData{ChannelID: room})
		}
	}
	wsCfg.OnDisconnect = func(err error) {
		if s.ev.OnConnection != nil {
			s.ev.OnConnection("disconnected")
		}
		if p := s.peerRef.Swap(nil); p != nil {
			_ = p.Close()
		}
		s.remoteMgr.RemoveAll()
	}
	s.ws = signal.NewClient(wsCfg)
	s.registerHandlers()

	// 媒体管线：采集→降噪→Opus→(OnPacket→RTP)；下行混音→播放环。
	pcfg := audio.DefaultPipelineConfig()
	pcfg.Loopback = false
	pcfg.DSP.OnSpeakingChange = func(speaking bool) {
		// DSP 回调在音频 goroutine 上，必须非阻塞。
		if s.muted.Load() {
			speaking = false
		}
		_ = s.ws.Send(signal.TypeVoiceState, signal.VoiceStateData{Speaking: signal.Bool(speaking)})
	}
	pcfg.OnPacket = func(pkt []byte) {
		if p := s.peerRef.Load(); p != nil {
			_ = p.SendOpus(pkt)
		}
	}
	pipe, err := audio.NewPipeline(pcfg)
	if err != nil {
		return fmt.Errorf("pipeline: %w", err)
	}
	s.pipe = pipe

	s.mixer = audio.NewMixer(pipe.WritePlayback)
	s.remoteMgr = audio.NewRemoteManager(s.mixer, s.log)
	s.states = audio.NewStateTable()
	s.states.OnChange = func(id string, st audio.PeerVoiceState) {
		if s.ev.OnPeerSpeaking != nil {
			s.ev.OnPeerSpeaking(id, st.Speaking, st.Mute, st.Deafen)
		}
	}
	if err := s.mixer.Start(); err != nil {
		_ = pipe.Close()
		return fmt.Errorf("mixer: %w", err)
	}
	if err := pipe.Start(); err != nil {
		s.mixer.Stop()
		_ = pipe.Close()
		return fmt.Errorf("pipeline start: %w", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	s.cancel = cancel
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		if err := s.ws.Run(ctx); err != nil && err != context.Canceled {
			s.log.Printf("signal run: %v", err)
		}
	}()
	return nil
}

// registerHandlers 挂信令消息处理（对照 calltest 的接线范式）。
func (s *Session) registerHandlers() {
	s.ws.On(signal.TypeHello, func(env signal.Envelope) {
		var d signal.HelloData
		if env.Decode(&d) == nil && d.User.ID != "" {
			s.selfID = d.User.ID
		}
	})
	s.ws.On(signal.TypeChannelJoined, func(env signal.Envelope) {
		var d signal.ChannelJoinedData
		if err := env.Decode(&d); err != nil {
			return
		}
		s.mu.Lock()
		s.members = make(map[string]PeerView, len(d.Members))
		for _, m := range d.Members {
			s.members[m.UserID] = PeerView{
				UserID: m.UserID, Nickname: m.Nickname,
				Speaking: m.Speaking, Mute: m.Mute, Deafen: m.Deafen,
			}
			if m.UserID != d.Self.UserID {
				s.states.Set(m.UserID, audio.PeerVoiceState{Speaking: m.Speaking, Mute: m.Mute, Deafen: m.Deafen})
			}
		}
		s.selfID = d.Self.UserID
		s.mu.Unlock()
		if s.ev.OnSelf != nil {
			s.ev.OnSelf(d.Self.UserID)
		}
		if s.ev.OnPeerJoin != nil {
			for _, m := range d.Members {
				s.ev.OnPeerJoin(m.UserID, m.Nickname, m.Speaking, m.Mute, m.Deafen)
			}
		}
		// 仅在无存活 peer 时建连（他人入房广播不动媒体链路）。
		if p := s.peerRef.Load(); p != nil &&
			p.ConnectionState() != webrtc.PeerConnectionStateClosed &&
			p.ConnectionState() != webrtc.PeerConnectionStateFailed {
			return
		}
		s.setupPeer()
	})
	s.ws.On(signal.TypeRTCOffer, func(env signal.Envelope) {
		var d signal.SDPData
		if env.Decode(&d) != nil {
			return
		}
		if p := s.peerRef.Load(); p != nil {
			_ = p.HandleRemoteSDP(webrtc.SessionDescription{Type: webrtc.SDPTypeOffer, SDP: d.SDP})
		}
	})
	s.ws.On(signal.TypeRTCAnswer, func(env signal.Envelope) {
		var d signal.SDPData
		if env.Decode(&d) != nil {
			return
		}
		if p := s.peerRef.Load(); p != nil {
			_ = p.HandleRemoteSDP(webrtc.SessionDescription{Type: webrtc.SDPTypeAnswer, SDP: d.SDP})
		}
	})
	s.ws.On(signal.TypeRTCIce, func(env signal.Envelope) {
		var d signal.ICEData
		if env.Decode(&d) != nil {
			return
		}
		if p := s.peerRef.Load(); p != nil {
			_ = p.HandleRemoteICE(iceToPion(d))
		}
	})
	s.ws.On(signal.TypeVoiceState, func(env signal.Envelope) {
		var d signal.VoiceStateData
		if env.Decode(&d) != nil || d.UserID == "" {
			return
		}
		s.states.Apply(d.UserID, d.Speaking, d.Mute, d.Deafen)
		s.mu.Lock()
		if v, ok := s.members[d.UserID]; ok {
			if d.Speaking != nil {
				v.Speaking = *d.Speaking
			}
			if d.Mute != nil {
				v.Mute = *d.Mute
			}
			if d.Deafen != nil {
				v.Deafen = *d.Deafen
			}
			s.members[d.UserID] = v
		}
		s.mu.Unlock()
	})
	s.ws.On(signal.TypeChannelPeerLeft, func(env signal.Envelope) {
		var d signal.PeerLeftData
		if env.Decode(&d) != nil {
			return
		}
		s.remoteMgr.RemovePeer(d.UserID)
		s.states.Remove(d.UserID)
		s.mu.Lock()
		delete(s.members, d.UserID)
		s.mu.Unlock()
		if s.ev.OnPeerLeave != nil {
			s.ev.OnPeerLeave(d.UserID)
		}
	})
}

// setupPeer 建立对 SFU 的单条 PeerConnection（初始 offer 一次，此后只应答）。
func (s *Session) setupPeer() {
	if old := s.peerRef.Swap(nil); old != nil {
		_ = old.Close()
	}
	s.remoteMgr.RemoveAll()
	streamID := s.name
	if s.selfID != "" {
		streamID = s.selfID
	}
	peer, err := rtc.NewPeer(rtc.PeerConfig{
		StreamID:        streamID,
		IncludeLoopback: true, // 本机 SFU 宣告 127.0.0.1；公网部署应关闭
		Callbacks: rtc.PeerCallbacks{
			OnLocalSDP: func(sdp webrtc.SessionDescription) {
				msgType := signal.TypeRTCOffer
				if sdp.Type == webrtc.SDPTypeAnswer {
					msgType = signal.TypeRTCAnswer
				}
				_ = s.ws.Send(msgType, signal.SDPData{SDP: sdp.SDP})
			},
			OnLocalICE: func(c webrtc.ICECandidateInit) {
				_ = s.ws.Send(signal.TypeRTCIce, iceFromPion(c))
			},
			OnTrack: s.remoteMgr.HandleTrack,
			OnConnectionStateChange: func(st webrtc.PeerConnectionState) {
				if s.ev.OnConnection != nil {
					s.ev.OnConnection("rtc:" + st.String())
				}
			},
		},
		Logger: s.log,
	})
	if err != nil {
		s.log.Printf("new peer: %v", err)
		return
	}
	s.peerRef.Store(peer)
	if err := peer.Offer(); err != nil {
		s.log.Printf("offer: %v", err)
	}
}

// JoinChannel 加入语音频道。
func (s *Session) JoinChannel(channelID string) error {
	if s.ws == nil {
		return fmt.Errorf("session not connected")
	}
	s.roomID.Store(channelID)
	return s.ws.Send(signal.TypeChannelJoin, signal.ChannelJoinData{ChannelID: channelID})
}

// LeaveChannel 离开当前语音频道。
func (s *Session) LeaveChannel() error {
	room, _ := s.roomID.Load().(string)
	if room == "" || s.ws == nil {
		return nil
	}
	s.roomID.Store("")
	if err := s.ws.Send(signal.TypeChannelLeave, signal.ChannelJoinData{ChannelID: room}); err != nil {
		return err
	}
	if p := s.peerRef.Swap(nil); p != nil {
		_ = p.Close()
	}
	s.remoteMgr.RemoveAll()
	s.mixer.SetDeafen(false)
	return nil
}

// SetMute 本地静音（停止上行发包 + 隐藏波形）。
func (s *Session) SetMute(muted bool) {
	s.muted.Store(muted)
	if p := s.peerRef.Load(); p != nil {
		p.SetMuted(muted)
	}
	if s.ws != nil {
		_ = s.ws.Send(signal.TypeVoiceState, signal.VoiceStateData{Mute: signal.Bool(muted)})
	}
}

// SetDeafen 耳聋（不听任何人；实现为混音器总输出静音）。
func (s *Session) SetDeafen(deafened bool) {
	s.deafened.Store(deafened)
	if s.mixer != nil {
		s.mixer.SetDeafen(deafened)
	}
	if s.ws != nil {
		_ = s.ws.Send(signal.TypeVoiceState, signal.VoiceStateData{Deafen: signal.Bool(deafened)})
	}
}

// SetPeerVolume 调整某远端成员音量（0.0–2.0）。
func (s *Session) SetPeerVolume(userID string, gain float64) {
	if s.mixer != nil {
		if in := s.mixer.Peer(userID); in != nil {
			in.SetGain(gain)
		}
	}
}

// RestartDevices 热切换采集/播放设备：管线设备流重启，编解码/混音状态保留。
func (s *Session) RestartDevices(captureID, playbackID string) error {
	if s.pipe == nil {
		return errors.New("no pipeline")
	}
	return s.pipe.RestartDevices(captureID, playbackID)
}

// ApplyAudio 把运行时音频设置热应用到媒体管线（通话中可调，未 Connect 时空操作）。
// Opus 参数经 ResolveOpusConfig 回落默认；逐项应用，返回首个错误（不中断）。
func (s *Session) ApplyAudio(rs audio.RuntimeSettings) error {
	if s.pipe == nil {
		return nil
	}
	rs.Opus = audio.ResolveOpusConfig(rs.Opus)
	steps := []struct {
		name string
		fn   func() error
	}{
		{"bitrate", func() error { return s.pipe.SetOpusBitrate(rs.Opus.BitrateKbps) }},
		{"vbr", func() error { return s.pipe.SetOpusVBR(*rs.Opus.VBR) }},
		{"dtx", func() error { return s.pipe.SetOpusDTX(*rs.Opus.DTX) }},
		{"fec", func() error { return s.pipe.SetOpusFEC(*rs.Opus.FEC) }},
		{"packet_loss", func() error { return s.pipe.SetOpusPacketLoss(rs.Opus.PacketLoss) }},
	}
	var firstErr error
	for _, st := range steps {
		if err := st.fn(); err != nil && firstErr == nil {
			firstErr = fmt.Errorf("audio %s: %w", st.name, err)
		}
	}
	s.pipe.SetDenoise(rs.Denoise)
	s.pipe.SetGain(rs.Gain)
	s.pipe.SetVADSensitivity(rs.VADSens)
	return firstErr
}

// Muted / Deafened 本地状态查询。
func (s *Session) Muted() bool    { return s.muted.Load() }
func (s *Session) Deafened() bool { return s.deafened.Load() }

// Members 房间成员快照。
func (s *Session) Members() []PeerView {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]PeerView, 0, len(s.members))
	for _, m := range s.members {
		out = append(out, m)
	}
	return out
}

// Stats 统计快照（JSON 给 UI）。
func (s *Session) Stats() string {
	vs := VoiceStats{PeerIDs: s.remoteMgr.PeerIDs()}
	if s.pipe != nil {
		ps := s.pipe.Stats()
		vs.EncPackets = ps.PacketsEncoded
		vs.LatencyMS = int(ps.LatencyAvgUS / 1000)
	}
	if p := s.peerRef.Load(); p != nil {
		vs.RtpSent = p.Stats.PacketsSent.Load()
	}
	s.mu.Lock()
	vs.Members = len(s.members)
	s.mu.Unlock()
	b, _ := json.Marshal(vs)
	return string(b)
}

// SendChat 发送文字消息（chat.send，chat.message 由 WS 回落）。
func (s *Session) SendChat(channelID, content string) error {
	if s.ws == nil {
		return fmt.Errorf("session not connected")
	}
	return s.ws.Send(signal.TypeChatSend, map[string]string{
		"channel_id": channelID,
		"content":    content,
	})
}

// ChatHandler 注入 chat.message 处理（app 层补挂）。
func (s *Session) ChatHandler(fn func(channelID, userID, nickname, content, ts string)) {
	if s.ws == nil {
		return
	}
	s.ws.On(signal.TypeChatMessage, func(env signal.Envelope) {
		var d struct {
			ID        string `json:"id"`
			ChannelID string `json:"channel_id"`
			Content   string `json:"content"`
			TS        int64  `json:"ts"`
			User      struct {
				ID       string `json:"id"`
				Nickname string `json:"nickname"`
			} `json:"user"`
		}
		if env.Decode(&d) != nil {
			return
		}
		fn(d.ChannelID, d.User.ID, d.User.Nickname, d.Content, fmt.Sprintf("%d", d.TS))
	})
}

// Close 释放全部资源。
func (s *Session) Close() {
	if s.cancel != nil {
		s.cancel()
	}
	if s.ws != nil {
		s.ws.Close()
	}
	s.wg.Wait()
	if p := s.peerRef.Swap(nil); p != nil {
		_ = p.Close()
	}
	if s.remoteMgr != nil {
		s.remoteMgr.RemoveAll()
	}
	if s.mixer != nil {
		s.mixer.Stop()
	}
	if s.pipe != nil {
		_ = s.pipe.Close()
	}
}

// iceFromPion / iceToPion 信令线格式 ↔ pion（与 calltest 同款）。
func iceFromPion(c webrtc.ICECandidateInit) signal.ICEData {
	d := signal.ICEData{Candidate: c.Candidate, SDPMLineIndex: c.SDPMLineIndex}
	if c.SDPMid != nil {
		d.SDPMid = *c.SDPMid
	}
	if c.UsernameFragment != nil {
		d.UsernameFragment = *c.UsernameFragment
	}
	return d
}

func iceToPion(d signal.ICEData) webrtc.ICECandidateInit {
	c := webrtc.ICECandidateInit{Candidate: d.Candidate, SDPMLineIndex: d.SDPMLineIndex}
	if d.SDPMid != "" {
		c.SDPMid = &d.SDPMid
	}
	if d.UsernameFragment != "" {
		c.UsernameFragment = &d.UsernameFragment
	}
	return c
}
