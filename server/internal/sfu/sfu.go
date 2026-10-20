// Package sfu 实现 LiteTalk 自研 Pion SFU（docs/落地方案.md §5.2）。
// 要点：ICE-Lite + 单 UDP mux（默认 7845）+ 仅注册 Opus + RTP 透传不解码。
package sfu

import (
	"fmt"
	"io"
	"log"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"github.com/pion/ice/v4"
	"github.com/pion/interceptor"
	"github.com/pion/logging"
	"github.com/pion/webrtc/v4"
)

// SignalSender 由 WS 层实现：SFU 侧主动下发的消息（重协商 offer、trickle ICE、ICE 失败）。
type SignalSender interface {
	// SendOffer 下发 rtc.offer（S→C 重协商；初始协商之后只允许 SFU 发 offer）
	SendOffer(sdp string)
	// SendCandidate 下发 rtc.ice（S→C trickle）
	SendCandidate(c webrtc.ICECandidateInit)
	// SendFailed 本端 PeerConnection 因 ICE 失败被移除（#19）：只通知该 peer 的
	// 连接（rtc.failed，0.2），客户端销毁本地 PC 后重新入房；其余成员走 peer_left。
	SendFailed(channelID, userID, reason string)
}

// OnSpeakingFn 上行说话状态翻转回调（#33）：说话判定循环上触发，必须非阻塞。
type OnSpeakingFn func(channelID, userID string, speaking bool)

// RFC 6464 音频电平（#33）：SDP 协商的头扩展 URI，与客户端 rtc 包一致。
const audioLevelURI = "urn:ietf:params:rtp-hdrext:ssrc-audio-level"

// 说话判定门限（RFC 6464 电平：0=0dB 满幅 … 127=静音，越小越响；头扩展
// 低 7 位的数值就是负的 dBov，44 即 -44dBov）。数值对齐客户端 DSP 静音环境
// 默认（底噪 -50dBFS + 12/+6dB ⇒ enter≈-38dBFS、exit≈-44dBFS，dBov 与 dBFS
// 同满幅基准）。服务端看的是 AGC 后的上行电平，跨客户端刻度已被拉齐。
//
// 进入门限取 44（审查要求）：旧的 38 会把 -42dBov 这类轻声说话人永久挡在
// 候选外，两人房间也不转发。退出门限取 50，保留 6dB 迟滞——45..50 之间
// 保持原态，避免门限附近来回翻转；enter 必须小于 exit，不能退化成无迟滞。
//
// 迟滞拍数按 speakingTick 折算，**时间常数不变**（对齐客户端 DSP：
// EnterFrames=3=30ms 起判、ExitFrames=15=150ms 挂起）。退出挂起和停流判定
// 都是防字间停顿/丢包闪断的宽限，压短会闪断——换判定周期只折算拍数，不改时长。
const (
	speakEnterLevel = 44 // 电平 ≤44（≥-44dBov）进入说话
	speakExitLevel  = 50 // 电平 >50（<-50dBov）计入退出（45..50 之间保持原态，双门限迟滞）
	speakEnterTicks = 1  // 20ms/tick → 20ms 起判（≤客户端现状 30ms）
	speakExitTicks  = 10 // 20ms/tick → 200ms 挂起（时间常数同旧 2 拍×100ms，对齐客户端 150ms 量级）
	levelStaleTicks = 10 // 20ms/tick → 200ms 停流按静音（时间常数同旧 2 拍×100ms）
)

// 活跃转发门控（#33 Phase 1）：进入=进 top-N 且达电平门限；跌出=持续低于
// 门限 gateGraceTicks（500ms）或掉出 top-N，且在 active 至少保持 ActiveHoldMS。
const (
	gateGraceTicks = 5 // 500ms 低电平宽限（activeTick 100ms/tick，与说话判定不同周期）
)

// audioLevelExtID 在协商到的头扩展里找 RFC 6464 电平的本地 ID（RFC 8285
// 单字节头 ID 取值 1..14；未协商返回 0）。
func audioLevelExtID(exts []webrtc.RTPHeaderExtensionParameter) uint8 {
	for _, e := range exts {
		if e.URI == audioLevelURI {
			if e.ID < 1 || e.ID > 14 {
				return 0
			}
			return uint8(e.ID)
		}
	}
	return 0
}

// speakingTick 说话翻转判定的采样周期。贴 Opus 帧周期（48kHz/960 样本 = 20ms，
// 上行 50pps）：判定粒度即一帧，比它更快看不到新样本，比它更慢只加指示延迟。
// 起判 1 拍 ⇒ 「电平到达服务端 → 翻转下发」worst case ≈ 1×20ms + 广播，
// 不高于客户端现状（dsp EnterFrames=3=30ms）。
const speakingTick = 20 * time.Millisecond

// activeTick 活跃转发 top-N 的排序周期。这是带宽决策不是指示：切太频会让活跃
// 说话人在 top-N 里来回跳（thrash），100ms 足够，**不跟说话判定一起加速**。
const activeTick = 100 * time.Millisecond

// Config SFU 配置。
type Config struct {
	// UDPPort ICE-Lite UDP mux 端口（env LITETALK_SFU_UDP，默认 7845）；0 = 随机端口（测试用）
	UDPPort int
	// TCPPort ICE-TCP mux 端口（env LITETALK_SFU_TCP，默认同 UDPPort 号，0 = 关闭）。
	// 与 UDP 同号时是两个独立 socket（TCP/UDP 各一），不是共 socket 复用（#25）。
	TCPPort int
	// NAT1To1IP 对外宣告的 IP（env LITETALK_SFU_IP，默认 127.0.0.1）
	NAT1To1IP string
	// MaxMembers 房间人数上限（§5.2 默认 10）
	MaxMembers int
	// MaxRooms 房间数上限（env LITETALK_MAX_ROOMS，默认 50；#20）。0 = 不限。
	MaxRooms int
	// ActiveN 活跃转发路数（#33，env LITETALK_SFU_ACTIVE_N，默认 5）：只对 top-N
	// 活跃源 WriteRTP，其余源的包直接丢弃（包级门控，不摘 track 不重协商）。
	// ≤0 = 关闭门控全转发。
	ActiveN int
	// ActiveHoldMS 活跃源最少保持毫秒（#33，env LITETALK_SFU_ACTIVE_HOLD_MS，默认 1000）。
	ActiveHoldMS int
	// Logger 日志；nil 则丢弃
	Logger *log.Logger
}

// SFU 进程级单例：持有一套 webrtc.API（ICE-Lite + UDP mux + Opus MediaEngine）与房间表。
type SFU struct {
	api    *webrtc.API
	conn   *net.UDPConn
	tcpMux ice.TCPMux
	tcpLn  net.Listener // 留引用查 LocalAddr（ice.TCPMux 接口不含该方法）
	cfg    Config
	log    *log.Logger

	// onSpeaking 说话翻转回调（#33）：启动阶段 SetOnSpeaking 注入，判定循环读。
	onSpeaking atomic.Value // OnSpeakingFn
	tickDone   chan struct{}
	tickWG     sync.WaitGroup
	closed     atomic.Bool // Close 幂等（tickDone 只准关一次）

	// tickRooms 房间快照缓冲（复用免分配；仅 speakingLoop goroutine 访问）。
	// 快照后必须在锁外跑判定：翻转回调进 WS 层，反过来 hub 会持自己锁时调
	// SFU，交叉持锁必死锁。
	tickRooms []*Room

	// serverMutedDropped 服务端强制闭麦丢弃的上行包数（#36 诊断计数）。
	serverMutedDropped atomic.Uint64

	mu    sync.RWMutex
	rooms map[string]*Room
}

// New 构造 SFU：绑定 UDP mux 端口，构建 ICE-Lite SettingEngine 与仅 Opus 的 MediaEngine。
func New(cfg Config) (*SFU, error) {
	if cfg.MaxMembers <= 0 {
		cfg.MaxMembers = 10
	}
	if cfg.NAT1To1IP == "" {
		cfg.NAT1To1IP = "127.0.0.1"
	}
	// 零值回落安全默认（§0.11）：活跃源最少保持 1s。
	// ActiveN 不在此处兜底：配置层（flag/env）缺省取 5，传进来的 ≤0 是显式关闭门控。
	if cfg.ActiveHoldMS <= 0 {
		cfg.ActiveHoldMS = 1000
	}
	logger := cfg.Logger
	if logger == nil {
		logger = log.New(io.Discard, "", 0)
	}

	// MediaEngine：只注册 Opus（minptime=10;useinbandfec=1，§5.2 音频靠 FEC+PLC 兜底）
	me := &webrtc.MediaEngine{}
	if err := me.RegisterCodec(webrtc.RTPCodecParameters{
		RTPCodecCapability: webrtc.RTPCodecCapability{
			MimeType:    webrtc.MimeTypeOpus,
			ClockRate:   48000,
			Channels:    2,
			SDPFmtpLine: "minptime=10;useinbandfec=1",
		},
		PayloadType: 111,
	}, webrtc.RTPCodecTypeAudio); err != nil {
		return nil, fmt.Errorf("注册 Opus 编解码失败: %w", err)
	}
	// RFC 6464 电平扩展（#33）：协商上才收得到客户端/浏览器的电平字节；
	// 未协商（v1 客户端）则读不到扩展，说话判定/活跃门控自动跳过该源。
	if err := me.RegisterHeaderExtension(webrtc.RTPHeaderExtensionCapability{
		URI: audioLevelURI,
	}, webrtc.RTPCodecTypeAudio); err != nil {
		return nil, fmt.Errorf("注册音频电平扩展失败: %w", err)
	}

	// 单 UDP mux：SFU 侧只需开放 1 个 UDP 端口
	conn, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4zero, Port: cfg.UDPPort})
	if err != nil {
		return nil, fmt.Errorf("SFU UDP mux 监听失败（端口 %d）: %w", cfg.UDPPort, err)
	}

	se := webrtc.SettingEngine{}
	se.SetLite(true)
	se.SetNAT1To1IPs([]string{cfg.NAT1To1IP}, webrtc.ICECandidateTypeHost)
	// NAT1To1 为环回（本地开发/测试）时放行环回候选：TCP mux 按「连接落到的本端 IP」
	// 归属 ufrag，环回被 gather 排除时候选归属 IP 与实际 accept 的 IP 对不上，TCP 候选
	// 会失联。生产已由 validateSFUIP 拒绝环回 NAT1To1，不受此影响。
	if ip := net.ParseIP(cfg.NAT1To1IP); ip != nil && ip.IsLoopback() {
		se.SetIncludeLoopbackCandidate(true)
	}
	udpMux := ice.NewUDPMuxDefault(ice.UDPMuxParams{
		Logger:  logging.NewDefaultLoggerFactory().NewLogger("udpmux"),
		UDPConn: conn,
	})
	se.SetICEUDPMux(udpMux)

	// ICE-TCP mux（#25）：UDP 被封的网络靠 TCP 候选直连，不依赖外部 coturn。
	// 与 SFU UDP 同号时各占一个 socket（TCP/UDP 协议栈天然隔离），不是共 socket。
	var tcpMux ice.TCPMux
	var tcpLn net.Listener
	if cfg.TCPPort != 0 {
		// TCP mux 按「accept 的本端 IP」归属 ufrag 连接，gather 侧按网卡 IP 注册。
		// NAT1To1 为环回（dev/测试）时必须把监听绑到该环回地址：否则多网卡机器上
		// gather 注册在第一块网卡的 key 上，环回连接的 accept key 对不上，检查包
		// 会进一个没有候选认领的 conn。生产 NAT1To1 是 NAT 网关地址（本机不可绑），
		// 仍绑 0.0.0.0——单网卡下 accept 的本端 IP 等于网卡 IP，key 天然一致。
		bindIP := net.IPv4zero
		if ip := net.ParseIP(cfg.NAT1To1IP); ip != nil && ip.IsLoopback() {
			bindIP = ip
		}
		ln, err := net.ListenTCP("tcp", &net.TCPAddr{IP: bindIP, Port: cfg.TCPPort})
		if err != nil {
			_ = conn.Close()
			return nil, fmt.Errorf("SFU ICE-TCP 监听失败（端口 %d）: %w", cfg.TCPPort, err)
		}
		tcpMux = ice.NewTCPMuxDefault(ice.TCPMuxParams{
			Listener: ln,
			Logger:   logging.NewDefaultLoggerFactory().NewLogger("tcpmux"),
			// 读缓冲按 pion 示例取值；写缓冲 4MB（pion 推荐），防突发反压丢包。
			ReadBufferSize:  8,
			WriteBufferSize: 4 * 1024 * 1024,
		})
		se.SetICETCPMux(tcpMux)
		tcpLn = ln
		// webrtc 的 supportedNetworkTypes 默认只有 UDP（TCP 注释为 Not supported yet），
		// 不显式打开则 TCP mux 一个候选都采不到。只在启用 TCP mux 时扩展网络类型，
		// TCPPort=0 的旧部署行为不变。
		se.SetNetworkTypes([]webrtc.NetworkType{
			webrtc.NetworkTypeUDP4,
			webrtc.NetworkTypeUDP6,
			webrtc.NetworkTypeTCP4,
			webrtc.NetworkTypeTCP6,
		})
	}

	// 默认拦截器（#19）：NACK 生成/应答 + RTCP SR/RR。音频仍靠 FEC/PLC 兜底，
	// 但 RTCP 反馈链路必须能跑（此前无拦截器不发 SR/RR、无 NACK）。
	ir := &interceptor.Registry{}
	if err := webrtc.RegisterDefaultInterceptors(me, ir); err != nil {
		_ = conn.Close()
		if tcpMux != nil {
			_ = tcpMux.Close()
		}
		return nil, fmt.Errorf("注册默认拦截器失败: %w", err)
	}
	api := webrtc.NewAPI(webrtc.WithMediaEngine(me), webrtc.WithInterceptorRegistry(ir), webrtc.WithSettingEngine(se))

	s := &SFU{
		api:      api,
		conn:     conn,
		tcpMux:   tcpMux,
		tcpLn:    tcpLn,
		cfg:      cfg,
		log:      logger,
		tickDone: make(chan struct{}),
		rooms:    make(map[string]*Room),
	}
	s.tickWG.Add(1)
	go s.speakingLoop()
	logger.Printf("sfu: ICE-Lite UDP mux 就绪 %s（宣告 IP %s，房间上限 %d）",
		conn.LocalAddr(), cfg.NAT1To1IP, cfg.MaxMembers)
	if tcpLn != nil {
		logger.Printf("sfu: ICE-TCP mux 就绪 %s（UDP 被封网络的直连路径）", tcpLn.Addr())
	}
	return s, nil
}

// SetOnSpeaking 注入说话翻转回调（#33）。启动阶段调用一次；nil 则翻转不下发。
// 存的是包装后的非 nil 接口（atomic.Value 不收裸 nil）。
func (s *SFU) SetOnSpeaking(fn OnSpeakingFn) {
	s.onSpeaking.Store(fn)
}

// speakingFn 取当前回调；未注入返回 nil。
func (s *SFU) speakingFn() OnSpeakingFn {
	if v := s.onSpeaking.Load(); v != nil {
		return v.(OnSpeakingFn)
	}
	return nil
}

// speakingLoop 说话翻转判定与活跃门控的驱动循环（#33）。两个独立周期：
//   - speakingTick 只跑说话迟滞——指示要求低延迟，贴帧采样；
//   - activeTick 只跑活跃门 top-N——带宽决策，慢以免 thrash。
//
// forward 热路径不参与任何判定，只读电平/包计数/转发门这几个原子。
func (s *SFU) speakingLoop() {
	defer s.tickWG.Done()
	spk := time.NewTicker(speakingTick)
	defer spk.Stop()
	act := time.NewTicker(activeTick)
	defer act.Stop()
	for {
		select {
		case <-s.tickDone:
			return
		case <-spk.C:
			s.tickSpeakAll()
		case <-act.C:
			s.tickActiveAll()
		}
	}
}

// roomSnap 把当前房间快照进 s.tickRooms（复用缓冲）。
func (s *SFU) roomSnap() []*Room {
	s.mu.RLock()
	s.tickRooms = s.tickRooms[:0]
	for _, r := range s.rooms {
		s.tickRooms = append(s.tickRooms, r)
	}
	s.mu.RUnlock()
	return s.tickRooms
}

// tickSpeakAll 遍历全部房间做一次说话判定（speakingTick）。
func (s *SFU) tickSpeakAll() {
	for _, r := range s.roomSnap() {
		r.tickSpeak(s.speakingFn())
	}
}

// tickActiveAll 遍历全部房间做一次活跃门 top-N（activeTick）。
func (s *SFU) tickActiveAll() {
	for _, r := range s.roomSnap() {
		r.tickActive()
	}
}

// Port 返回 UDP mux 实际绑定端口（测试用随机端口时查询用）。
func (s *SFU) Port() int {
	return s.conn.LocalAddr().(*net.UDPAddr).Port
}

// TCPPort 返回 ICE-TCP mux 实际绑定端口；未启用返回 0。
func (s *SFU) TCPPort() int {
	if s.tcpLn == nil {
		return 0
	}
	addr, ok := s.tcpLn.Addr().(*net.TCPAddr)
	if !ok {
		return 0
	}
	return addr.Port
}

// MaxMembers 房间人数上限。
func (s *SFU) MaxMembers() int { return s.cfg.MaxMembers }

// Close 关闭 UDP/TCP mux，所有 PeerConnection 随之中断。先逐个主动关闭 peer（置
// closed 再关 PC）：否则关机路径的连接状态回调会被当成 ICE 失败，向已死的
// 下游发 rtc.failed（#19）。TCP mux 必须一并关，否则监听端口不释放。
func (s *SFU) Close() error {
	if s.closed.Swap(true) {
		return nil
	}
	close(s.tickDone)
	s.tickWG.Wait()
	s.mu.Lock()
	rooms := s.rooms
	s.rooms = make(map[string]*Room)
	s.mu.Unlock()
	for _, r := range rooms {
		r.mu.Lock()
		peers := r.peers
		r.peers = make(map[string]*Peer)
		r.mu.Unlock()
		for _, p := range peers {
			p.close()
		}
	}
	err := s.conn.Close()
	if s.tcpMux != nil {
		if tcpErr := s.tcpMux.Close(); tcpErr != nil && err == nil {
			err = tcpErr
		}
	}
	return err
}

// getOrCreateRoom 只创建/查找房间，不登记入房（#19）：返回后到 addPeer 之间
// 空房间可能被 removeRoomIfEmpty 回收。生产入房必须走 acquireRoom（带 joining
// 登记）；这里仅供确实不接 addPeer 的测试与配额检查使用。
func (s *SFU) getOrCreateRoom(id string) (*Room, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if r, ok := s.rooms[id]; ok {
		return r, nil
	}
	// 房间数配额（#20）：满额拒建新房，已有房间不受影响。
	if s.cfg.MaxRooms > 0 && len(s.rooms) >= s.cfg.MaxRooms {
		return nil, ErrTooManyRooms
	}
	r := newRoom(s, id)
	s.rooms[id] = r
	return r, nil
}

// acquireRoom 取得（或创建）房间并登记一次「入房进行中」（#19）。
// 与 removeRoomIfEmpty 用同一锁序 s.mu → r.mu：返回后到 addPeer 落位之间，
// 即使房间此刻还是空的，removeRoomIfEmpty 也会因 joining>0 而保留它，
// 不会出现「peer 落进已从 rooms 摘掉的孤儿房」。调用方必须在 addPeer
// 之后（无论成功与否）调用 releaseJoin。
func (s *SFU) acquireRoom(id string) (*Room, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	r, ok := s.rooms[id]
	if !ok {
		if s.cfg.MaxRooms > 0 && len(s.rooms) >= s.cfg.MaxRooms {
			return nil, ErrTooManyRooms
		}
		r = newRoom(s, id)
		s.rooms[id] = r
	}
	r.mu.Lock()
	r.joining++
	r.mu.Unlock()
	return r, nil
}

func (s *SFU) findRoom(id string) *Room {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.rooms[id]
}

// removeRoomIfEmpty 回收空房间（#19）：只有在没有 peer 且没有入房进行中
// （joining==0）时删除。锁序与 acquireRoom 一致（s.mu → r.mu），两者串行化，
// 因此不会误删刚被 acquireRoom 拿到、正在 addPeer 的房间。
func (s *SFU) removeRoomIfEmpty(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	r, ok := s.rooms[id]
	if !ok {
		return
	}
	r.mu.RLock()
	empty := len(r.peers) == 0 && r.joining == 0
	r.mu.RUnlock()
	if empty {
		delete(s.rooms, id)
		s.log.Printf("sfu: 房间 %s 已空，销毁", id)
	}
}

// ErrRoomFull 房间人数达到上限。
var ErrRoomFull = fmt.Errorf("房间已满")

// ErrTooManyRooms 房间数达到上限（#20，LITETALK_MAX_ROOMS）。
var ErrTooManyRooms = fmt.Errorf("房间数已达上限")

// HandleOffer 处理客户端初始 rtc.offer：创建/复用 PeerConnection、应答、
// 并把房间既有成员的轨道补发给新成员（触发 SFU 侧重协商 offer）。
// connID 为归属 WS 连接标识（§0.9）：LeaveIf 按此核对，避免误清新连接的 peer。
// 返回 rtc.answer 的 SDP。
func (s *SFU) HandleOffer(roomID, userID, connID, sdp string, sig SignalSender) (string, error) {
	room, err := s.acquireRoom(roomID)
	if err != nil {
		return "", err
	}
	peer, err := room.addPeer(userID, connID, sig)
	room.releaseJoin()
	if err != nil {
		s.removeRoomIfEmpty(roomID)
		return "", err
	}
	answerSDP, err := peer.handleInitialOffer(sdp)
	if err != nil {
		// 按实例摘除（#23）：同 connID 的重入可能已建出新 peer，只按 connID 核
		// 会误删新实例。
		room.removePeerInstance(peer)
		s.removeRoomIfEmpty(roomID)
		return "", err
	}
	// 既有成员轨道 → 给新成员加 outbound 并重 offer
	room.onPeerReady(peer)
	return answerSDP, nil
}

// HandleAnswer 处理客户端对 SFU 重协商 offer 的 rtc.answer。
func (s *SFU) HandleAnswer(roomID, userID, sdp string) error {
	peer := s.findPeer(roomID, userID)
	if peer == nil {
		return fmt.Errorf("成员 %s 不在房间 %s", userID, roomID)
	}
	return peer.handleAnswer(sdp)
}

// AddICECandidate 处理客户端 trickle rtc.ice。
func (s *SFU) AddICECandidate(roomID, userID string, c webrtc.ICECandidateInit) error {
	peer := s.findPeer(roomID, userID)
	if peer == nil {
		return fmt.Errorf("成员 %s 不在房间 %s", userID, roomID)
	}
	return peer.addICECandidate(c)
}

// LeaveIf 成员离开（§0.9 身份核对）：仅当 peer 归属 connID 时移除路由、关闭其
// PeerConnection；同 uid 的新连接已顶替旧 peer 时这里是无操作，不会误删。
// 其他成员指向它的下行轨道一并摘除（#23：惰性保留会让重入房拿不到新 OnTrack）。
func (s *SFU) LeaveIf(roomID, userID, connID string) {
	room := s.findRoom(roomID)
	if room == nil {
		return
	}
	room.removePeerIf(userID, connID)
	s.removeRoomIfEmpty(roomID)
}

func (s *SFU) findPeer(roomID, userID string) *Peer {
	room := s.findRoom(roomID)
	if room == nil {
		return nil
	}
	return room.getPeer(userID)
}

// SetServerMuted 设置/解除服务端强制闭麦（#36）：RTP 包级丢弃，track 保持挂载
// （禁 RemoveTrack）。状态记在 room.serverMuted——removePeer/teardownPeer 不清，
// 同房间重入/换连接经 newPeer 继承；房间销毁才清。
// 房间或成员不存在返回 false（调用方按「目标不在房」404）。
func (s *SFU) SetServerMuted(roomID, userID string, muted bool) bool {
	room := s.findRoom(roomID)
	if room == nil {
		return false
	}
	room.mu.Lock()
	p, ok := room.peers[userID]
	if !ok {
		room.mu.Unlock()
		return false
	}
	room.serverMuted[userID] = muted
	p.serverMuted.Store(muted)
	room.mu.Unlock()
	s.log.Printf("sfu: 房间 %s 成员 %s 服务端闭麦=%v", roomID, userID, muted)
	return true
}
