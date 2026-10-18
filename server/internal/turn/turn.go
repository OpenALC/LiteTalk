// Package turn 内置 pion/turn 服务（#25）：UDP+TCP 双监听，与 rtcapi 的短期凭证
// 完全互通（coturn use-auth-secret 同款：username=<expiry>:<uid>，
// credential=base64(HMAC-SHA1(secret, username))）。UDP 被封的网络经 TCP 3478 走
// TURN 中继兜底；外部 coturn 仍可选（LITETALK_TURN_PORT=0 关闭内置）。
package turn

import (
	"fmt"
	"log"
	"net"
	"strconv"

	"github.com/pion/logging"
	turnpkg "github.com/pion/turn/v5"
)

// Config 内置 TURN 配置。
type Config struct {
	// UDPPort TURN UDP 监听端口（env LITETALK_TURN_PORT，默认 3478）；0 = 不听 UDP
	UDPPort int
	// TCPPort TURN TCP 监听端口；0 = 不听 TCP。禁 UDP 网络的必经路径（#25 裁定）。
	TCPPort int
	// RelayIP 中继地址对外宣告 IP（env LITETALK_SFU_IP，NAT 1:1 同源）
	RelayIP net.IP
	// RelayMinPort / RelayMaxPort 中继端口段（默认 49152~49200）
	RelayMinPort, RelayMaxPort uint16
	// Realm TURN 域（env LITETALK_TURN_REALM，默认 LITETALK_DOMAIN 或 litetalk）
	Realm string
	// Secret REST 短期凭证共享密钥（env LITETALK_TURN_SECRET，与 rtcapi 同源）
	Secret string
	// Logger 日志；nil 则丢弃
	Logger *log.Logger
}

// Server 内置 TURN 实例。
type Server struct {
	srv    *turnpkg.Server
	udpLn  net.PacketConn
	tcpLn  net.Listener
	logger *log.Logger
}

// New 启动内置 TURN：UDP 与 TCP 各自独立监听（不共 socket）。RelayAddressGenerator
// 用端口段，对外 IP 取 RelayIP（NAT 1:1 与 SFU 同源）。任一监听失败会关闭已建资源。
func New(cfg Config) (*Server, error) {
	if cfg.UDPPort == 0 && cfg.TCPPort == 0 {
		return nil, fmt.Errorf("TURN UDP/TCP 端口不能同时为 0")
	}
	if cfg.Secret == "" {
		return nil, fmt.Errorf("TURN Secret 不能为空")
	}
	if cfg.RelayIP == nil {
		return nil, fmt.Errorf("RelayIP 不能为空")
	}
	if cfg.Realm == "" {
		cfg.Realm = "litetalk"
	}
	if cfg.RelayMinPort == 0 {
		cfg.RelayMinPort = 49152
	}
	if cfg.RelayMaxPort == 0 {
		cfg.RelayMaxPort = 49200
	}
	logger := cfg.Logger
	if logger == nil {
		logger = log.Default()
	}
	// pion 内部日志：错误级别，避免 trace 刷屏。
	pionLog := logging.NewDefaultLoggerFactory().NewLogger("turn")

	// 同一 RelayAddressGenerator 服务 UDP 与 TCP 两种中继（AllocatePacketConn /
	// AllocateListener 各取所需）。
	newRelayGen := func() *turnpkg.RelayAddressGeneratorPortRange {
		return &turnpkg.RelayAddressGeneratorPortRange{
			RelayAddress: cfg.RelayIP,
			MinPort:      cfg.RelayMinPort,
			MaxPort:      cfg.RelayMaxPort,
			Address:      "0.0.0.0",
		}
	}

	sc := turnpkg.ServerConfig{
		Realm:       cfg.Realm,
		AuthHandler: turnpkg.LongTermTURNRESTAuthHandler(cfg.Secret, pionLog),
	}

	s := &Server{logger: logger}

	if cfg.UDPPort != 0 {
		udpLn, err := net.ListenPacket("udp4", net.JoinHostPort("0.0.0.0", strconv.Itoa(cfg.UDPPort)))
		if err != nil {
			return nil, fmt.Errorf("TURN UDP 监听失败（端口 %d）: %w", cfg.UDPPort, err)
		}
		s.udpLn = udpLn
		sc.PacketConnConfigs = append(sc.PacketConnConfigs, turnpkg.PacketConnConfig{
			PacketConn:            udpLn,
			RelayAddressGenerator: newRelayGen(),
		})
	}
	if cfg.TCPPort != 0 {
		tcpLn, err := net.Listen("tcp4", net.JoinHostPort("0.0.0.0", strconv.Itoa(cfg.TCPPort)))
		if err != nil {
			s.closeListeners()
			return nil, fmt.Errorf("TURN TCP 监听失败（端口 %d）: %w", cfg.TCPPort, err)
		}
		s.tcpLn = tcpLn
		sc.ListenerConfigs = append(sc.ListenerConfigs, turnpkg.ListenerConfig{
			Listener:              tcpLn,
			RelayAddressGenerator: newRelayGen(),
		})
	}

	srv, err := turnpkg.NewServer(sc)
	if err != nil {
		s.closeListeners()
		return nil, fmt.Errorf("TURN 服务启动失败: %w", err)
	}
	s.srv = srv

	if s.udpLn != nil {
		logger.Printf("turn: 内置 TURN UDP 就绪 %s（中继 %s，端口段 %d-%d）",
			s.udpLn.LocalAddr(), cfg.RelayIP, cfg.RelayMinPort, cfg.RelayMaxPort)
	}
	if s.tcpLn != nil {
		logger.Printf("turn: 内置 TURN TCP 就绪 %s（禁 UDP 网络的中继兜底）", s.tcpLn.Addr())
	}
	return s, nil
}

// UDPAddr 返回 UDP 监听地址；未启用返回 nil。
func (s *Server) UDPAddr() net.Addr {
	if s.udpLn == nil {
		return nil
	}
	return s.udpLn.LocalAddr()
}

// TCPAddr 返回 TCP 监听地址；未启用返回 nil。
func (s *Server) TCPAddr() net.Addr {
	if s.tcpLn == nil {
		return nil
	}
	return s.tcpLn.Addr()
}

// Close 关闭 TURN 服务与监听 socket。
func (s *Server) Close() error {
	err := s.srv.Close()
	s.closeListeners()
	return err
}

func (s *Server) closeListeners() {
	if s.udpLn != nil {
		_ = s.udpLn.Close()
	}
	if s.tcpLn != nil {
		_ = s.tcpLn.Close()
	}
}
