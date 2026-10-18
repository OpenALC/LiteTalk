// Package sfu 实现 LiteTalk 自研 Pion SFU（docs/落地方案.md §5.2）。
// 要点：ICE-Lite + 单 UDP mux（默认 7845）+ 仅注册 Opus + RTP 透传不解码。
package sfu

import (
	"fmt"
	"io"
	"log"
	"net"
	"sync"

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
		api:    api,
		conn:   conn,
		tcpMux: tcpMux,
		tcpLn:  tcpLn,
		cfg:    cfg,
		log:    logger,
		rooms:  make(map[string]*Room),
	}
	logger.Printf("sfu: ICE-Lite UDP mux 就绪 %s（宣告 IP %s，房间上限 %d）",
		conn.LocalAddr(), cfg.NAT1To1IP, cfg.MaxMembers)
	if tcpLn != nil {
		logger.Printf("sfu: ICE-TCP mux 就绪 %s（UDP 被封网络的直连路径）", tcpLn.Addr())
	}
	return s, nil
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

func (s *SFU) findRoom(id string) *Room {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.rooms[id]
}

func (s *SFU) removeRoomIfEmpty(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if r, ok := s.rooms[id]; ok && r.isEmpty() {
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
	room, err := s.getOrCreateRoom(roomID)
	if err != nil {
		return "", err
	}
	peer, err := room.addPeer(userID, connID, sig)
	if err != nil {
		return "", err
	}
	answerSDP, err := peer.handleInitialOffer(sdp)
	if err != nil {
		room.removePeerIf(userID, connID)
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
