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
	"github.com/pion/logging"
	"github.com/pion/webrtc/v4"
)

// SignalSender 由 WS 层实现：SFU 侧主动下发的两类消息（重协商 offer 与 trickle ICE）。
type SignalSender interface {
	// SendOffer 下发 rtc.offer（S→C 重协商；初始协商之后只允许 SFU 发 offer）
	SendOffer(sdp string)
	// SendCandidate 下发 rtc.ice（S→C trickle）
	SendCandidate(c webrtc.ICECandidateInit)
}

// Config SFU 配置。
type Config struct {
	// UDPPort ICE-Lite UDP mux 端口（env LITETALK_SFU_UDP，默认 7845）；0 = 随机端口（测试用）
	UDPPort int
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
	api  *webrtc.API
	conn *net.UDPConn
	cfg  Config
	log  *log.Logger

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
	udpMux := ice.NewUDPMuxDefault(ice.UDPMuxParams{
		Logger:  logging.NewDefaultLoggerFactory().NewLogger("udpmux"),
		UDPConn: conn,
	})
	se.SetICEUDPMux(udpMux)

	api := webrtc.NewAPI(webrtc.WithMediaEngine(me), webrtc.WithSettingEngine(se))

	s := &SFU{
		api:   api,
		conn:  conn,
		cfg:   cfg,
		log:   logger,
		rooms: make(map[string]*Room),
	}
	logger.Printf("sfu: ICE-Lite UDP mux 就绪 %s（宣告 IP %s，房间上限 %d）",
		conn.LocalAddr(), cfg.NAT1To1IP, cfg.MaxMembers)
	return s, nil
}

// Port 返回 UDP mux 实际绑定端口（测试用随机端口时查询用）。
func (s *SFU) Port() int {
	return s.conn.LocalAddr().(*net.UDPAddr).Port
}

// MaxMembers 房间人数上限。
func (s *SFU) MaxMembers() int { return s.cfg.MaxMembers }

// Close 关闭 UDP mux，所有 PeerConnection 随之中断。
func (s *SFU) Close() error { return s.conn.Close() }

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
// 其他成员的 outbound track 惰性保留（停写即可）。
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
