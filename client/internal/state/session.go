package state

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/OpenALC/LiteTalk/client/internal/audio"
	"github.com/OpenALC/LiteTalk/client/internal/hotkey"
	"github.com/OpenALC/LiteTalk/client/internal/rtc"
	"github.com/OpenALC/LiteTalk/client/internal/signal"
	"github.com/pion/webrtc/v4"
)

// Events 会话 → UI 的事件回调（由 app 层接到 runtime.EventsEmit）。
type Events struct {
	// OnPeerJoin 有成员进入房间（含自己首次入房的成员全量推送前的逐个通知）。
	OnPeerJoin func(userID, nickname, tag string, speaking, mute, deafen bool)
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
	// OnStateRequest 收到他人发来的闭麦/静音请求（v0.1.2；接收方自行决定执行）。
	OnStateRequest func(fromID, fromNickname string, mute, deafen *bool)
	// OnVoiceUnavailable 媒体管线初始化失败（音频不可用但信令照常连接）。
	OnVoiceUnavailable func(errMsg string)
	// OnKicked 被服务端踢下线（session.kicked 消息 / WS 4001：同类设备新登录，
	// 契约 §0.5）。reason 约定值 login_elsewhere。回调可能跑在 ws 读循环
	// goroutine（会话 wg 内）：实现侧不得同步 Close 会话，须异步执行。
	OnKicked func(reason string)
	// OnVoiceTaken 同账号另一会话抢占语音（session.voice_taken，契约 §0.10）：
	// 服务端已把本端摘出房间，本地按退房清理完毕。与 OnKicked 同样可能跑在
	// ws 读循环 goroutine（会话 wg 内），实现侧不得同步 Close 会话。
	OnVoiceTaken func()
	// OnPresenceUpdate 好友在线状态变化（presence.update，#11）。
	// 跑在 ws 读循环 goroutine，实现侧只做转发。
	OnPresenceUpdate func(userID string, online bool)
	// OnServerShutdown 服务端优雅关闭（server.shutdown，#21）：停自动重连并提示。
	// 与 OnKicked 同线程约定。
	OnServerShutdown func(reason string)
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
	Tag      string `json:"tag"` // 昵称#tag 的 tag，好友请求用
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

	// voiceUnavailable/voiceErr：媒体管线初始化失败（如 Linux 缺 Opus）。
	// 降级而非整体失败——信令照常连接，JoinChannel 明确报错供前端 toast。
	voiceUnavailable bool
	voiceErr         error

	// voiceSend 说话状态发送队列：容量 1 只保留最新值（说话状态高频翻转，
	// 积压旧状态无意义），由专用 goroutine 消费发送（音频回调禁止直发网络）。
	voiceSend    chan signal.VoiceStateData
	mediaStarted atomic.Bool // 采集/播放是否已启动（入房才启动，按需点亮麦克风）
	peerRef      atomic.Pointer[rtc.Peer]
	roomID       atomic.Value // string
	muted        atomic.Bool
	deafened     atomic.Bool

	// sendGate 按键说话发送门（PTT/语音激活/常开）。App 层长期持有的值对象，
	// 会话重建不换门——用户按住的 PTT 状态不能因为重连丢掉。建管线时转交
	// Pipeline，之后由 Pipeline 每帧自行 Load。
	sendGate atomic.Pointer[hotkey.Gate]

	// iceMu/iceServers 预取的 STUN/TURN 凭证缓存（#13）：setupPeer 跑在 WS 读
	// 循环的 channel.joined 处理器里，禁止网络 I/O——凭证由 JoinChannel 在可
	// 阻塞的 Wails 绑定线程预取，读循环只读缓存；缓存空则降级空 ICE 列表。
	iceMu      sync.RWMutex
	iceServers []webrtc.ICEServer

	mu      sync.Mutex
	members map[string]PeerView
	selfID  string

	cancel context.CancelFunc
	wg     sync.WaitGroup
}

// NewSession 创建会话。token 为信令鉴权令牌；nickname 仅 dev 模式用作 user_id。
func NewSession(api *APIClient, token, nickname string, ev Events, logger *log.Logger) *Session {
	if logger == nil {
		logger = log.Default()
	}
	s := &Session{
		api:       api,
		log:       logger,
		ev:        ev,
		token:     token,
		name:      nickname,
		members:   make(map[string]PeerView),
		voiceSend: make(chan signal.VoiceStateData, 1),
	}
	s.roomID.Store("")
	return s
}

// Connect 建立信令连接并装配媒体管线（采集/播放入房才启动，见 startMedia）。
// 媒体管线初始化失败不再让登录整体失败：置 voiceUnavailable、发
// OnVoiceUnavailable 事件，信令照常连接（纯文字/好友功能仍可用）。
func (s *Session) Connect(serverURL string) {
	wsCfg := signal.Config{
		URL:    serverURL,
		Logger: s.log,
		// 闭包取令牌：REST 侧刷新 access token 后，WS 重连自动携带新令牌。
		TokenFunc: func(context.Context) (string, error) { return s.token, nil },
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
		if s.remoteMgr != nil {
			s.remoteMgr.RemoveAll()
		}
		s.stopMedia()
	}
	// WS close 4001 / 握手 401 带 session_replaced 头：session.kicked 消息在
	// 服务端发送窗口内可能丢帧，close code 是可靠的那一半。回调跑在 ws Run
	// goroutine（会话 wg 内），app 侧 OnKicked 已异步化，此处直接调出口即可。
	wsCfg.OnKicked = func(reason string) { s.handleKicked(reason) }
	s.ws = signal.NewClient(wsCfg)
	s.registerHandlers()

	ctx, cancel := context.WithCancel(context.Background())
	s.cancel = cancel

	// 媒体管线：采集→降噪→Opus→(OnPacket→RTP)；下行混音→播放环。
	// 构造失败（如无 Opus 的平台）降级：跳过音频初始化，信令不受影响。
	pcfg := audio.DefaultPipelineConfig()
	pcfg.Loopback = false
	pcfg.DSP.OnSpeakingChange = func(speaking bool) {
		// DSP 回调跑在音频 goroutine 上，必须非阻塞：投递到容量 1 的
		// voiceSend，由下方专用 goroutine 发送；积压时丢弃旧值只留最新。
		if s.muted.Load() {
			speaking = false
		}
		select {
		case s.voiceSend <- signal.VoiceStateData{Speaking: signal.Bool(speaking)}:
		default:
		}
	}
	pcfg.OnPacket = func(pkt []byte) {
		if p := s.peerRef.Load(); p != nil {
			_ = p.SendOpus(pkt)
		}
	}
	pipe, err := audio.NewPipeline(pcfg)
	if err != nil {
		s.voiceUnavailable = true
		s.voiceErr = err
		s.log.Printf("pipeline unavailable: %v (signaling continues)", err)
		if s.ev.OnVoiceUnavailable != nil {
			s.ev.OnVoiceUnavailable(err.Error())
		}
	} else {
		s.pipe = pipe
		// 发包门：App 层在 NewSession 之后、Connect 之前注入；此处补到管线上
		// （SetSendGate 在管线已存在时也会即时生效，两条路径等价）。
		if g := s.sendGate.Load(); g != nil {
			pipe.SetSendGate(g)
		}
		s.mixer = audio.NewMixer(pipe.WritePlayback)
		s.remoteMgr = audio.NewRemoteManager(s.mixer, s.log)
		s.states = audio.NewStateTable()
		s.states.OnChange = func(id string, st audio.PeerVoiceState) {
			if s.ev.OnPeerSpeaking != nil {
				s.ev.OnPeerSpeaking(id, st.Speaking, st.Mute, st.Deafen)
			}
		}
	}

	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		for {
			select {
			case <-ctx.Done():
				return
			case st := <-s.voiceSend:
				if s.ws != nil {
					_ = s.ws.Send(signal.TypeVoiceState, st)
				}
			}
		}
	}()

	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		if err := s.ws.Run(ctx); err != nil && err != context.Canceled {
			s.log.Printf("signal run: %v", err)
		}
	}()
}

// startMedia 启动采集/播放（幂等）。未入房不启动——空闲时也跑
// 采集+RNNoise+Opus 是白耗 CPU 且 Windows 麦克风指示灯常亮。
// 启动失败仅记录（重入房可重试），不影响信令与对端下行。
func (s *Session) startMedia() {
	if s.voiceUnavailable || s.pipe == nil {
		return
	}
	if !s.mediaStarted.CompareAndSwap(false, true) {
		return
	}
	if err := s.mixer.Start(); err != nil {
		s.mediaStarted.Store(false)
		s.log.Printf("mixer start: %v", err)
		return
	}
	if err := s.pipe.Start(); err != nil {
		s.mixer.Stop()
		s.mediaStarted.Store(false)
		s.log.Printf("pipeline start: %v", err)
	}
}

// stopMedia 停止采集/播放（幂等）。与 LeaveChannel/断线/Close 的清理路径
// 合并调用；Start/Stop 成对后可再 Start（重入房）。
func (s *Session) stopMedia() {
	if !s.mediaStarted.CompareAndSwap(true, false) {
		return
	}
	if s.mixer != nil {
		s.mixer.Stop()
	}
	if s.pipe != nil {
		s.pipe.Stop()
	}
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
		s.startMedia() // 入房才启动采集/播放
		s.mu.Lock()
		s.members = make(map[string]PeerView, len(d.Members))
		for _, m := range d.Members {
			s.members[m.UserID] = PeerView{
				UserID: m.UserID, Nickname: m.Nickname, Tag: m.Tag,
				Speaking: m.Speaking, Mute: m.Mute, Deafen: m.Deafen,
			}
			if m.UserID != d.Self.UserID && s.states != nil {
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
				s.ev.OnPeerJoin(m.UserID, m.Nickname, m.Tag, m.Speaking, m.Mute, m.Deafen)
			}
		}
		// 仅在无存活 peer 时建连（他人入房广播不动媒体链路）。
		if p := s.peerRef.Load(); p != nil &&
			p.ConnectionState() != webrtc.PeerConnectionStateClosed &&
			p.ConnectionState() != webrtc.PeerConnectionStateFailed {
			return
		}
		s.setupPeer()
		// 补发完整语音状态（muted+deafened 现值）：重连/重入房后服务端内存里的
		// 旧状态可能已丢，peer 又是新建（默认不静音）——不发则「UI 显示已闭麦但声音照传」。
		if s.ws != nil {
			_ = s.ws.Send(signal.TypeVoiceState, signal.VoiceStateData{
				Mute:   signal.Bool(s.muted.Load()),
				Deafen: signal.Bool(s.deafened.Load()),
			})
		}
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
		if s.remoteMgr != nil {
			s.remoteMgr.RemovePeer(d.UserID)
		}
		if s.states != nil {
			s.states.Remove(d.UserID)
		}
		s.mu.Lock()
		delete(s.members, d.UserID)
		s.mu.Unlock()
		if s.ev.OnPeerLeave != nil {
			s.ev.OnPeerLeave(d.UserID)
		}
	})
	// voice.state_request：他人请求我闭麦/静音（v0.1.2，定向投递）。
	s.ws.On(signal.TypeVoiceStateRequest, func(env signal.Envelope) {
		var d signal.VoiceStateRequestData
		if env.Decode(&d) != nil {
			return
		}
		if s.ev.OnStateRequest != nil {
			s.ev.OnStateRequest(d.From.ID, d.From.Nickname, d.Mute, d.Deafen)
		}
	})
	// session.kicked：同类设备在其他设备登录，服务端踢本连接（契约 §0.5）。
	// 断开与清令牌在 app 层 OnKicked 完成，此处只上报原因。
	// 消息本身「尽力而为」（发送窗口内丢帧即无踪），WS close 4001 与握手
	// 401 的 session_replaced 头是兜底，见 wsCfg.OnKicked。
	s.ws.On(signal.TypeSessionKicked, func(env signal.Envelope) {
		var d signal.SessionKickedData
		reason := signal.KickReasonLoginElsewhere
		if env.Decode(&d) == nil && d.Reason != "" {
			reason = d.Reason
		}
		s.handleKicked(reason)
	})
	// session.voice_taken：同账号另一会话抢占语音（契约 §0.10）。服务端已把
	// 本端摘出成员表，本地按退房清理——但绝不发 channel.leave：那条 leave 按
	// uid 处理，会把抢占端一起带离房间。
	s.ws.On(signal.TypeSessionVoiceTaken, func(env signal.Envelope) {
		s.leaveRoomLocal()
		if s.ev.OnVoiceTaken != nil {
			s.ev.OnVoiceTaken()
		}
	})
	// presence.update：好友在线状态变化（#11），原样转给 UI 层。
	s.ws.On(signal.TypePresenceUpdate, func(env signal.Envelope) {
		var d signal.PresenceUpdateData
		if env.Decode(&d) != nil || d.UserID == "" {
			return
		}
		if s.ev.OnPresenceUpdate != nil {
			s.ev.OnPresenceUpdate(d.UserID, d.Online)
		}
	})
	// server.shutdown：服务端优雅关闭（#21）。停掉 ws 自动重连并转给 UI 层提示。
	// Close 必须异步：同步调用会与读循环互等（Close 等 Run 退出、Run 等读循环返回）。
	s.ws.On(signal.TypeServerShutdown, func(env signal.Envelope) {
		var d signal.ServerShutdownData
		reason := "shutdown"
		if env.Decode(&d) == nil && d.Reason != "" {
			reason = d.Reason
		}
		if s.ev.OnServerShutdown != nil {
			s.ev.OnServerShutdown(reason)
		}
		go s.ws.Close()
	})
}

// leaveRoomLocal 本地退房（不发 channel.leave）。仅用于服务端已单方面把本端
// 摘出房间的路径（session.voice_taken）：再发 leave 会让服务端按 uid 把抢占端
// 也带离房间。成员表必须清——否则 UI 拿着旧 members 显示「幽灵在房」。
func (s *Session) leaveRoomLocal() {
	s.stopMedia()
	s.roomID.Store("")
	if p := s.peerRef.Swap(nil); p != nil {
		_ = p.Close()
	}
	if s.remoteMgr != nil {
		s.remoteMgr.RemoveAll()
	}
	if s.mixer != nil {
		s.mixer.SetDeafen(false)
	}
	s.mu.Lock()
	s.members = make(map[string]PeerView)
	s.mu.Unlock()
}

// handleKicked 上报被踢原因（session.kicked 消息、WS close 4001、握手 401 的
// session_replaced/session_replaced 头，三条路径的统一出口）。
func (s *Session) handleKicked(reason string) {
	if s.ev.OnKicked != nil {
		s.ev.OnKicked(reason)
	}
}

// setupPeer 建立对 SFU 的单条 PeerConnection（初始 offer 一次，此后只应答）。
func (s *Session) setupPeer() {
	if old := s.peerRef.Swap(nil); old != nil {
		_ = old.Close()
	}
	if s.remoteMgr == nil { // 音频降级路径不可能走到（JoinChannel 已拦截），防御
		return
	}
	s.remoteMgr.RemoveAll()
	streamID := s.name
	if s.selfID != "" {
		streamID = s.selfID
	}
	peer, err := rtc.NewPeer(rtc.PeerConfig{
		StreamID: streamID,
		// 只读预取缓存，不做网络 I/O：本函数跑在 WS 读循环的 channel.joined
		// 处理器里，同步拉凭证会把信令收发停摆到撞心跳预算（#13）。
		ICEServers:      s.cachedICE(),
		IncludeLoopback: IsLoopbackURL(s.api.Base()), // 环回候选仅本机/内网 SFU 需要
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
	// 新 peer 默认不静音：必须同步本地静音态，否则重连后出现
	//「UI 显示已闭麦但声音照传」。deafen 是混音器总线层的口，无 peer 级对应。
	peer.SetMuted(s.muted.Load())
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
	if s.voiceUnavailable {
		return fmt.Errorf("语音不可用: %w", s.voiceErr)
	}
	// 预取 ICE 凭证再进房：setupPeer 跑在 WS 读循环上，不能做网络 I/O（#13）。
	// 这里是 Wails 绑定 goroutine，阻塞可接受；失败留空缓存，降级空 ICE 列表。
	s.prefetchICE()
	s.roomID.Store(channelID)
	if err := s.ws.Send(signal.TypeChannelJoin, signal.ChannelJoinData{ChannelID: channelID}); err != nil {
		// roomID 先存是为断线重连恢复；发送失败必须回滚——否则重连后
		// OnConnect 重发 join 造成「幽灵入房」。
		s.roomID.Store("")
		return fmt.Errorf("join channel: %w", err)
	}
	return nil
}

// LeaveChannel 离开当前语音频道。
func (s *Session) LeaveChannel() error {
	room, _ := s.roomID.Load().(string)
	// 无论后续发送成败、是否已在房里：采集/播放随离开停止（幂等），
	// 否则发送失败路径会把媒体留在运行态。
	s.stopMedia()
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
	if s.remoteMgr != nil {
		s.remoteMgr.RemoveAll()
	}
	if s.mixer != nil {
		s.mixer.SetDeafen(false)
	}
	return nil
}

// SetSendGate 注入按键说话发送门（传 nil 清除）。管线已建立时立即生效，
// 未建立（未 Connect 或音频降级）时先存住，Connect 建管线后自动补上。
// 跨 goroutine 安全：内部 atomic。
func (s *Session) SetSendGate(g *hotkey.Gate) {
	s.sendGate.Store(g)
	if s.pipe != nil {
		s.pipe.SetSendGate(g)
	}
}

// SendGate 返回当前发送门（未注入为 nil）。
func (s *Session) SendGate() *hotkey.Gate { return s.sendGate.Load() }

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
// v0.1.2 扩展：复杂度/降噪强度/AGC/扬声器总音量（mixer master gain）。
// v1.1 扩展：前置增益（原子热生效）/输入声道模式（热重启采集流）。
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
		{"pre_gain", func() error { return s.pipe.SetPreGainDB(rs.PreGainDB) }},
		{"input_mode", func() error { return s.pipe.SetInputMode(rs.InputMode) }},
	}
	if rs.Complexity > 0 {
		steps = append(steps, struct {
			name string
			fn   func() error
		}{"complexity", func() error { return s.pipe.SetComplexity(rs.Complexity) }})
	}
	var firstErr error
	for _, st := range steps {
		if err := st.fn(); err != nil && firstErr == nil {
			firstErr = fmt.Errorf("audio %s: %w", st.name, err)
		}
	}
	s.pipe.SetDenoise(rs.Denoise)
	if err := s.pipe.SetDenoiseStrength(rs.DenoiseStrength); err != nil && firstErr == nil {
		firstErr = fmt.Errorf("audio denoise_strength: %w", err)
	}
	s.pipe.SetAGC(rs.AGC)
	s.pipe.SetGain(rs.Gain)
	if s.mixer != nil {
		s.mixer.SetMasterGain(rs.PlaybackGain)
	}
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
	vs := VoiceStats{}
	if s.remoteMgr != nil {
		vs.PeerIDs = s.remoteMgr.PeerIDs()
	}
	if p := s.peerRef.Load(); p != nil {
		vs.RtpSent = p.Stats.PacketsSent.Load()
		// 延迟优先取 WebRTC 选中 ICE pair 的真实 RTT（问题：此前恒 0ms——
		// 管线回环估计只在 Loopback 模式有样本，真实通话不产样本）。
		// 用 ok 作哨兵而非值——RTT 真实但 <1ms 时会被错误覆盖（S1）。
		if rtt, ok := p.RTT(); ok {
			vs.LatencyMS = int(rtt.Milliseconds())
		} else if s.pipe != nil {
			// 无 RTT 样本时回落管线回环估计（本机联调场景）。
			vs.EncPackets = s.pipe.Stats().PacketsEncoded
			vs.LatencyMS = int(s.pipe.Stats().LatencyAvgUS / 1000)
		}
	}
	s.mu.Lock()
	vs.Members = len(s.members)
	s.mu.Unlock()
	b, _ := json.Marshal(vs)
	return string(b)
}

// SendChat 发送文字消息（chat.send，chat.message 由 WS 回落）。
// type 字段是服务端 chat.send 的必填校验（v1.1.0 §0.12：本期仅 text）。
func (s *Session) SendChat(channelID, content string) error {
	if s.ws == nil {
		return fmt.Errorf("session not connected")
	}
	return s.ws.Send(signal.TypeChatSend, map[string]string{
		"channel_id": channelID,
		"type":       "text",
		"content":    content,
	})
}

// RequestVoiceState 请求目标成员闭麦/静音（v0.1.2 voice.request_state；
// 纯请求语义，目标接受后自行执行，服务端无强制）。
func (s *Session) RequestVoiceState(channelID, targetID string, mute, deafen bool) error {
	if s.ws == nil {
		return fmt.Errorf("session not connected")
	}
	return s.ws.Send(signal.TypeVoiceRequestState, signal.VoiceRequestStateData{
		ChannelID: channelID,
		TargetID:  targetID,
		Mute:      &mute,
		Deafen:    &deafen,
	})
}

// VoiceStateView 供 UI 销毁重载后回填的语音态快照（v0.1.2）。
type VoiceStateView struct {
	ChannelID string     `json:"channel_id"`
	Muted     bool       `json:"muted"`
	Deafened  bool       `json:"deafened"`
	Members   []PeerView `json:"members"`
}

// VoiceState 当前语音态快照（channelId 空 = 未在频道）。
func (s *Session) VoiceState() VoiceStateView {
	v := VoiceStateView{
		ChannelID: s.roomID.Load().(string),
		Muted:     s.muted.Load(),
		Deafened:  s.deafened.Load(),
		Members:   s.Members(),
	}
	return v
}

// ChatHandler 注入 chat.message 处理（app 层补挂）。
// msgID 为服务端消息 ID（文字消息按频道隔离与去重需要真实 ID，
// 此前回调丢弃了 ID，前端只能拿时间戳当 key，无法可靠去重）。
func (s *Session) ChatHandler(fn func(channelID, msgID, userID, nickname, content, ts string)) {
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
		fn(d.ChannelID, d.ID, d.User.ID, d.User.Nickname, d.Content, fmt.Sprintf("%d", d.TS))
	})
}

// ErrorHandler 注入服务端 error 帧处理（app 层补挂）。
// 服务端对 chat.send 等消息的校验/拒收是异步 error 帧回落，
// 此前未订阅导致服务端拒收时消息凭空消失且无提示（阻断 B1）。
func (s *Session) ErrorHandler(fn func(code, message string)) {
	if s.ws == nil {
		return
	}
	s.ws.On(signal.TypeError, func(env signal.Envelope) {
		var d struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		}
		if env.Decode(&d) != nil {
			return
		}
		fn(d.Code, d.Message)
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
	s.stopMedia()
	if s.mixer != nil {
		s.mixer.Stop()
	}
	if s.pipe != nil {
		_ = s.pipe.Close()
	}
}

// SignalConnected 报告信令 WS 当前是否有活跃连接（app 层据首次连接结果定 session_ok）。
func (s *Session) SignalConnected() bool { return s.ws != nil && s.ws.Connected() }

// InRoom 报告当前是否在语音频道（app 层 voice:stats ticker 据它门控发送）。
func (s *Session) InRoom() bool {
	room, _ := s.roomID.Load().(string)
	return room != ""
}

// IsLoopbackURL 判断服务器基址是否指向环回（127.0.0.0/8、::1、localhost）。
// 环回目标仅本机/内网 SFU 需要收集环回 ICE 候选；公网部署必须关闭，
// 否则本机环回地址会作为候选暴露给远端对端。
// 解析失败视为非环回（安全侧，不收集环回候选）。
func IsLoopbackURL(base string) bool {
	u, err := url.Parse(strings.TrimSpace(base))
	if err != nil || u.Host == "" {
		return false
	}
	host := u.Hostname()
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// prefetchICE 预取 STUN/TURN 凭证并写入缓存。只在可阻塞的 goroutine 调用
// （JoinChannel 的 Wails 绑定线程）；WS 读循环上的 setupPeer 只读缓存（#13）。
// 失败留空缓存，由 cachedICE 降级空 ICE 列表——本机/内网直连仍可通。
func (s *Session) prefetchICE() {
	servers := s.fetchICEServers()
	s.iceMu.Lock()
	s.iceServers = servers
	s.iceMu.Unlock()
}

// cachedICE 读预取结果。零 I/O，专供 WS 读循环使用；空即降级空 ICE 列表。
func (s *Session) cachedICE() []webrtc.ICEServer {
	s.iceMu.RLock()
	defer s.iceMu.RUnlock()
	return s.iceServers
}

// fetchICEServers 拉取 STUN/TURN 凭证并转成 pion 格式。失败或服务端未配置
// TURN（dev 环境常见）时降级为空 ICE 列表——本机/内网直连仍可通，不阻断进房。
func (s *Session) fetchICEServers() []webrtc.ICEServer {
	resp, err := s.api.RTCCredentials()
	if err != nil {
		s.log.Printf("rtc credentials: %v（降级为空 ICE 列表）", err)
		return nil
	}
	if resp == nil || len(resp.ICEServers) == 0 {
		return nil
	}
	out := make([]webrtc.ICEServer, 0, len(resp.ICEServers))
	for _, srv := range resp.ICEServers {
		out = append(out, webrtc.ICEServer{
			URLs:       srv.URLs,
			Username:   srv.Username,
			Credential: srv.Credential,
		})
	}
	return out
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
