// Package rtc 实现轻音 LiteTalk 客户端的 WebRTC 媒体层：
// 对 SFU 的单条 PeerConnection（一上 N 下）、RTP 打包上行、per-peer 抖动缓冲。
package rtc

import (
	"crypto/rand"
	"encoding/binary"
	"errors"
	"fmt"
	"log"
	"sync"
	"sync/atomic"
	"time"

	"github.com/pion/interceptor"
	"github.com/pion/rtp"
	"github.com/pion/webrtc/v4"
)

// OpusPayloadType 是双方约定的 Opus 动态负载类型（与服务端一致）。
const OpusPayloadType = 111

// opusSDPFmtpLine Opus fmtp：10ms 最小帧长 + 带内 FEC（与服务端一致）。
const opusSDPFmtpLine = "minptime=10;useinbandfec=1"

// rtpTimestampStep 20ms Opus 帧的 RTP 时戳步进（48kHz × 0.02s）。
const rtpTimestampStep = 960

// DTXPayloadMax 是 Opus DTX 静音心跳包的最大长度（1~2 字节，仅 TOC/极短载荷）。
// 发送端照常发送以保持 RTP 时间线连续（接收端 jitter 不判空洞、不重预填）；
// 接收端不用这种包做带内 FEC（DTX 包不含 FEC 数据）。
const DTXPayloadMax = 2

// audioLevelURI RFC 6464 音频电平头扩展（#33）：SDP 由 MediaEngine 协商，
// RTP 载荷为 one-byte 元素数据 V|level（0x00=0dB … 0x7F=静音）。
const audioLevelURI = "urn:ietf:params:rtp-hdrext:ssrc-audio-level"

// PeerCallbacks 事件回调。除 OnLocalSDP/OnLocalICE 由 Peer 内部 goroutine
// 触发外，OnTrack/OnConnectionStateChange 由 pion 回调触发；均不得阻塞。
type PeerCallbacks struct {
	// OnLocalSDP 本地 SDP 就绪（Offer() 后的 offer / HandleRemoteSDP
	// 处理 SFU offer 后的 answer），调用方应经信令发给服务端。
	OnLocalSDP func(sdp webrtc.SessionDescription)
	// OnLocalICE 本地 ICE 候选（trickle，逐条），调用方应经信令发给服务端。
	OnLocalICE func(c webrtc.ICECandidateInit)
	// OnTrack 远端轨到达（每个说话成员一条，可多次触发）。
	// track.ID() 为 "audio-<成员 user_id>"、StreamID() 为 "litetalk-<房间>"（服务端约定）。
	OnTrack func(track *webrtc.TrackRemote, receiver *webrtc.RTPReceiver)
	// OnConnectionStateChange 连接状态变化。
	OnConnectionStateChange func(s webrtc.PeerConnectionState)
}

// PeerConfig Peer 配置。
type PeerConfig struct {
	ICEServers []webrtc.ICEServer // 来自 POST /rtc/credentials；本机/内网可空

	TrackID  string // 上行 track id，默认 "audio"
	StreamID string // 上行 stream id：本地 user_id（对端据此识别说话人）

	// IncludeLoopback 是否收集 127.0.0.1 环回候选。pion 默认排除环回，
	// 对本机 SFU（宣告 IP 127.0.0.1）联调必须开启；对公网服务端保持 false。
	IncludeLoopback bool

	// LevelFn 返回当前帧 RFC 6464 电平（0=0dB … 127=静音），SendOpus 每包读一次
	//（#33，接 audio.Pipeline.AudioLevel）。必须无锁零分配；nil = 不带电平扩展。
	LevelFn func() uint8

	Callbacks PeerCallbacks
	Logger    *log.Logger // nil 静默
}

// PeerStats 运行统计（原子计数）。
type PeerStats struct {
	PacketsSent   atomic.Uint64 // 上行 RTP 包数
	BytesSent     atomic.Uint64 // 上行载荷字节数
	MutedDropped  atomic.Uint64 // 本地静音丢弃的包数（用户主动静音，整包不发）
	SentDTX       atomic.Uint64 // 已发送的 DTX 心跳包数（≤2 字节，静音期 Opus 产物）
	RemoteSDPs    atomic.Uint64 // 处理的对端 SDP 数
	RemoteICEs    atomic.Uint64 // 处理的对端候选数
	TracksRecv    atomic.Uint64 // OnTrack 次数
	Renegotiation atomic.Uint64 // 应答 SFU 重协商 offer 次数
}

// Peer 是对 SFU 的单条 PeerConnection 封装：
//   - 初始由本端 Offer（1 条 sendonly Opus track, pt=111）；
//   - 此后只接收 SFU 的 offer 并 answer（永不主动 re-offer，无 glare）；
//   - trickle ICE 双向；
//   - 上行 SendOpus 打包发送；用户静音时停发，Opus DTX 心跳包照常转发。
type Peer struct {
	pc        *webrtc.PeerConnection
	sendTrack *webrtc.TrackLocalStaticRTP
	sender    *webrtc.RTPSender // AddTrack 返回值：查协商到的头扩展 ID 用
	cb        PeerCallbacks
	log       *log.Logger

	sendMu sync.Mutex
	pkt    rtp.Packet // 复用的打包结构（仅头部字段递增）
	// levelExt RFC 6464 one-byte 元素数据缓冲（#33）：协商完成后挂上 pkt.Header，
	// 之后每包只改写 levelExt[0]，元素头由 pion marshal 按 Extensions[].id 生成。
	levelExt      [1]byte
	levelAttached bool // sendMu 保护；SetExtension 只做一次（append 在初始化期）
	levelFn       func() uint8

	muted atomic.Bool

	remoteMu      sync.Mutex
	remoteDescSet bool
	pendingICE    []webrtc.ICECandidateInit // 远端描述就位前到达的候选

	closed atomic.Bool

	Stats PeerStats
}

// NewPeer 创建 PeerConnection 并挂上唯一的上行 sendonly Opus track。
func NewPeer(cfg PeerConfig) (*Peer, error) {
	if cfg.TrackID == "" {
		cfg.TrackID = "audio"
	}
	if cfg.StreamID == "" {
		cfg.StreamID = "litetalk"
	}
	if cfg.Logger == nil {
		cfg.Logger = log.New(nopWriter{}, "", 0)
	}

	// RFC 6464 电平扩展（#33）：必须在 RegisterDefaultInterceptors 之前注册——
	// 拦截器注册的 transport-cc 等扩展排在其后，SDP extmap ID 按注册序分配。
	me := &webrtc.MediaEngine{}
	if err := me.RegisterCodec(webrtc.RTPCodecParameters{
		RTPCodecCapability: webrtc.RTPCodecCapability{
			MimeType:    webrtc.MimeTypeOpus,
			ClockRate:   48000,
			Channels:    2, // RFC7587：SDP 中 opus 恒声明 2 声道，单声道由码流自带
			SDPFmtpLine: opusSDPFmtpLine,
		},
		PayloadType: OpusPayloadType,
	}, webrtc.RTPCodecTypeAudio); err != nil {
		return nil, fmt.Errorf("register opus codec: %w", err)
	}
	if cfg.LevelFn != nil {
		if err := me.RegisterHeaderExtension(webrtc.RTPHeaderExtensionCapability{
			URI: audioLevelURI,
		}, webrtc.RTPCodecTypeAudio); err != nil {
			return nil, fmt.Errorf("register audio level ext: %w", err)
		}
	}

	ir := &interceptor.Registry{}
	if err := webrtc.RegisterDefaultInterceptors(me, ir); err != nil {
		return nil, fmt.Errorf("register interceptors: %w", err)
	}

	se := webrtc.SettingEngine{}
	// #25：显式启用 ICE-TCP 候选。pion 默认只收 UDP 候选，UDP 被封的网络里
	// 直连永远走不到 SFU 的 ICE-TCP mux（LITETALK_SFU_TCP）——这是「禁 UDP
	// 网络可通话」的客户端侧前提。TCP4/TCP6 与 UDP4/UDP6 并存，不改变正常网络。
	se.SetNetworkTypes([]webrtc.NetworkType{
		webrtc.NetworkTypeUDP4,
		webrtc.NetworkTypeUDP6,
		webrtc.NetworkTypeTCP4,
		webrtc.NetworkTypeTCP6,
	})
	if cfg.IncludeLoopback {
		se.SetIncludeLoopbackCandidate(true)
	}
	api := webrtc.NewAPI(webrtc.WithMediaEngine(me), webrtc.WithInterceptorRegistry(ir), webrtc.WithSettingEngine(se))
	pc, err := api.NewPeerConnection(webrtc.Configuration{ICEServers: cfg.ICEServers})
	if err != nil {
		return nil, fmt.Errorf("new peer connection: %w", err)
	}

	cap := webrtc.RTPCodecCapability{
		MimeType:    webrtc.MimeTypeOpus,
		ClockRate:   48000,
		Channels:    2,
		SDPFmtpLine: opusSDPFmtpLine,
	}
	track, err := webrtc.NewTrackLocalStaticRTP(cap, cfg.TrackID, cfg.StreamID)
	if err != nil {
		_ = pc.Close()
		return nil, fmt.Errorf("new send track: %w", err)
	}
	sender, err := pc.AddTrack(track)
	if err != nil {
		_ = pc.Close()
		return nil, fmt.Errorf("add send track: %w", err)
	}

	p := &Peer{pc: pc, sendTrack: track, sender: sender, cb: cfg.Callbacks, log: cfg.Logger, levelFn: cfg.LevelFn}

	// 随机 SSRC / 起始序号与时戳（RFC3550 推荐）。
	var rnd [8]byte
	if _, err := rand.Read(rnd[:]); err != nil {
		_ = pc.Close()
		return nil, fmt.Errorf("seed rtp state: %w", err)
	}
	p.pkt.Header = rtp.Header{
		Version:        2,
		SequenceNumber: binary.BigEndian.Uint16(rnd[0:2]),
		Timestamp:      binary.BigEndian.Uint32(rnd[2:6]),
		SSRC:           binary.BigEndian.Uint32(rnd[4:8]) ^ 0x4C544B00, // 混合 'LTK' 盐
	}

	pc.OnICECandidate(func(c *webrtc.ICECandidate) {
		if c == nil {
			return // 不发送 end-of-candidates 标记
		}
		if p.cb.OnLocalICE != nil {
			p.cb.OnLocalICE(c.ToJSON())
		}
	})
	pc.OnTrack(func(t *webrtc.TrackRemote, r *webrtc.RTPReceiver) {
		p.Stats.TracksRecv.Add(1)
		if p.cb.OnTrack != nil {
			p.cb.OnTrack(t, r)
		}
	})
	pc.OnConnectionStateChange(func(s webrtc.PeerConnectionState) {
		p.logf("connection state: %s", s)
		if p.cb.OnConnectionStateChange != nil {
			p.cb.OnConnectionStateChange(s)
		}
	})
	// 本端永不触发重协商（唯一上行轨在初始 offer 前已加入），
	// OnNegotiationNeeded 无需处理。

	return p, nil
}

// Offer 生成初始 offer 并置为本地描述，经 OnLocalSDP 回调交给信令层发送。
// 仅应在建连后调用一次（此后由 SFU 单方面 re-offer）。
func (p *Peer) Offer() error {
	offer, err := p.pc.CreateOffer(nil)
	if err != nil {
		return fmt.Errorf("create offer: %w", err)
	}
	if err := p.pc.SetLocalDescription(offer); err != nil {
		return fmt.Errorf("set local offer: %w", err)
	}
	if p.cb.OnLocalSDP != nil {
		p.cb.OnLocalSDP(offer)
	}
	return nil
}

// HandleRemoteSDP 处理信令收到的对端 SDP：
//   - offer（SFU 重协商，新增下行轨）→ SetRemoteDescription → CreateAnswer →
//     SetLocalDescription → OnLocalSDP(answer)；
//   - answer（对初始 offer 的应答）→ SetRemoteDescription。
func (p *Peer) HandleRemoteSDP(sdp webrtc.SessionDescription) error {
	if p.closed.Load() {
		return errors.New("rtc: peer closed")
	}
	p.Stats.RemoteSDPs.Add(1)

	switch sdp.Type {
	case webrtc.SDPTypeAnswer:
		if err := p.pc.SetRemoteDescription(sdp); err != nil {
			return fmt.Errorf("set remote answer: %w", err)
		}
		p.markRemoteDescSet()
		return nil

	case webrtc.SDPTypeOffer:
		if err := p.pc.SetRemoteDescription(sdp); err != nil {
			return fmt.Errorf("set remote offer: %w", err)
		}
		p.markRemoteDescSet()
		answer, err := p.pc.CreateAnswer(nil)
		if err != nil {
			return fmt.Errorf("create answer: %w", err)
		}
		if err := p.pc.SetLocalDescription(answer); err != nil {
			return fmt.Errorf("set local answer: %w", err)
		}
		p.Stats.Renegotiation.Add(1)
		if p.cb.OnLocalSDP != nil {
			p.cb.OnLocalSDP(answer)
		}
		return nil

	default:
		return fmt.Errorf("rtc: unexpected SDP type %s", sdp.Type)
	}
}

// HandleRemoteICE 处理信令收到的对端 trickle 候选；远端描述未就位前先排队。
func (p *Peer) HandleRemoteICE(c webrtc.ICECandidateInit) error {
	if p.closed.Load() {
		return errors.New("rtc: peer closed")
	}
	p.Stats.RemoteICEs.Add(1)

	p.remoteMu.Lock()
	if !p.remoteDescSet {
		p.pendingICE = append(p.pendingICE, c)
		p.remoteMu.Unlock()
		return nil
	}
	p.remoteMu.Unlock()
	return p.pc.AddICECandidate(c)
}

// markRemoteDescSet 标记远端描述就位并冲刷排队的候选。
func (p *Peer) markRemoteDescSet() {
	p.remoteMu.Lock()
	pending := p.pendingICE
	p.pendingICE = nil
	p.remoteDescSet = true
	p.remoteMu.Unlock()
	for _, c := range pending {
		if err := p.pc.AddICECandidate(c); err != nil {
			p.logf("add queued ice candidate: %v", err)
		}
	}
	p.attachLevelExt()
}

// attachLevelExt 把 RFC 6464 电平扩展挂到复用的 pkt.Header（#33）。
// 只能在远端描述就位后调用（extmap ID 此刻才定）；幂等，SetExtension 只真正
// append 一次，此后每包只改写 levelExt[0]。未协商上该扩展（对端不支持）则不挂。
func (p *Peer) attachLevelExt() {
	if p.levelFn == nil || p.sender == nil {
		return
	}
	id, ok := audioLevelExtID(p.sender.GetParameters().HeaderExtensions)
	if !ok {
		return
	}
	p.sendMu.Lock()
	defer p.sendMu.Unlock()
	if p.levelAttached {
		return
	}
	if err := p.pkt.Header.SetExtension(id, p.levelExt[:]); err != nil {
		p.logf("attach audio level ext: %v", err)
		return
	}
	p.levelAttached = true
}

// audioLevelExtID 在协商到的头扩展里找 RFC 6464 电平的本地 ID（RFC 8285
// 单字节头 ID 取值 1..14；0/15 保留）。
func audioLevelExtID(exts []webrtc.RTPHeaderExtensionParameter) (uint8, bool) {
	for _, e := range exts {
		if e.URI == audioLevelURI {
			if e.ID < 1 || e.ID > 14 {
				return 0, false
			}
			return uint8(e.ID), true
		}
	}
	return 0, false
}

// SendOpus 把一个 Opus 包打包为 RTP 发出（seq 自增 1，时戳步进 960）。
// 用户主动静音时整包丢弃（MutedDropped 计数，恢复后重新预填）。
// Opus DTX 在静音期产出的 1~2 字节心跳包照常发送：RTP seq/时戳时间线连续，
// 对端 jitter 看到的是正常到达的空帧（libopus 解码 DTX 包得到对应长度的
// 静音），不会判空洞、不会 2×target 后重回预填——此前丢弃心跳包导致每次
// 起句多 60~120ms 缓冲且 PLC 涂抹。
// 在管线 OnPacket 回调（20ms）上调用；payload 底层缓冲不会被保留。
func (p *Peer) SendOpus(payload []byte) error {
	if p.closed.Load() {
		return errors.New("rtc: peer closed")
	}
	if p.muted.Load() {
		p.Stats.MutedDropped.Add(1)
		return nil
	}
	p.sendMu.Lock()
	p.pkt.SequenceNumber++
	p.pkt.Timestamp += rtpTimestampStep
	p.pkt.Payload = payload
	// RFC 6464 电平（#33）：只改写预分配的 levelExt[0]，零分配。
	if p.levelAttached {
		p.levelExt[0] = p.levelFn()
	}
	err := p.sendTrack.WriteRTP(&p.pkt)
	p.sendMu.Unlock()
	if err != nil {
		return fmt.Errorf("write rtp: %w", err)
	}
	p.Stats.PacketsSent.Add(1)
	p.Stats.BytesSent.Add(uint64(len(payload)))
	if len(payload) <= DTXPayloadMax {
		p.Stats.SentDTX.Add(1)
	}
	return nil
}

// SetMuted 设置本地静音（停止上行发包）。
func (p *Peer) SetMuted(m bool) { p.muted.Store(m) }

// Muted 当前本地静音状态。
func (p *Peer) Muted() bool { return p.muted.Load() }

// ConnectionState 当前连接状态。
func (p *Peer) ConnectionState() webrtc.PeerConnectionState { return p.pc.ConnectionState() }

// LocalSSRC 上行轨 SSRC（调试用）。
func (p *Peer) LocalSSRC() uint32 { return p.pkt.SSRC }

// RTT 返回当前选中 ICE candidate pair 的最新往返时延
// （WebRTC stats 的 currentRoundTripTime，真实网络 RTT）。
// 此前 UI 的延迟恒为 0：Session.Stats 取的是管线回环延迟估计，
// 而回环估计只在 Loopback 模式（M1 试听）有样本，真实通话 Loopback=false。
// ok=false 表示连接未建立或尚无 RTT 样本（此时调用方应保留上一次值或显示 --）。
func (p *Peer) RTT() (time.Duration, bool) {
	if p.closed.Load() {
		return 0, false
	}
	var best float64
	found := false
	for _, s := range p.pc.GetStats() {
		pair, ok := s.(webrtc.ICECandidatePairStats)
		if !ok {
			continue
		}
		// 只采纳已 nominated 且检查成功的 pair（媒体实际走的通路）
		if !pair.Nominated || pair.State != webrtc.StatsICECandidatePairStateSucceeded {
			continue
		}
		if pair.CurrentRoundTripTime > 0 && (!found || pair.CurrentRoundTripTime < best) {
			best = pair.CurrentRoundTripTime
			found = true
		}
	}
	if !found {
		return 0, false
	}
	return time.Duration(best * float64(time.Second)), true
}

// Close 关闭 PeerConnection（幂等）。
func (p *Peer) Close() error {
	if !p.closed.CompareAndSwap(false, true) {
		return nil
	}
	return p.pc.Close()
}

func (p *Peer) logf(format string, args ...any) {
	p.log.Printf("rtc: "+format, args...)
}

type nopWriter struct{}

func (nopWriter) Write(b []byte) (int, error) { return len(b), nil }
