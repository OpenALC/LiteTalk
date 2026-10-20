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
	"strings"
	"sync"

	"github.com/pion/logging"
	turnpkg "github.com/pion/turn/v5"
)

const (
	// DefaultMaxAllocationsPerUID 每 uid 的并发 allocation 默认上限（#25）：正常
	// 客户端一条连接只用 1 个中继，4 个留给重连/多网卡场景，同时把单凭证刷爆
	// 中继端口的成本压到最低。
	DefaultMaxAllocationsPerUID = 4
	// DefaultMaxAllocationsPerIP 每客户端 IP 的并发默认上限：同一 NAT 出口后可能
	// 有多个不同 uid 的用户；CGNAT/办公网出口可经 LITETALK_TURN_MAX_PER_IP 放宽。
	DefaultMaxAllocationsPerIP = 8
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
	// MaxPerUID / MaxPerIP 每 uid / 每源 IP 的并发 allocation 上限
	// （env LITETALK_TURN_MAX_PER_UID / LITETALK_TURN_MAX_PER_IP）。
	// 零值或负数回落 DefaultMaxAllocationsPerUID / DefaultMaxAllocationsPerIP。
	MaxPerUID int
	MaxPerIP  int
	// AllowLoopbackPeers 允许中继到环回 peer。仅供本机集成测试/本地联调：
	// 生产必须保持 false——合法凭证不能借中继访问本机服务（#25 P0）。
	// 私网、链路本地（云元数据）与组播地址无论该值如何始终拒绝。
	AllowLoopbackPeers bool
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
	if cfg.MaxPerUID <= 0 {
		cfg.MaxPerUID = DefaultMaxAllocationsPerUID
	}
	if cfg.MaxPerIP <= 0 {
		cfg.MaxPerIP = DefaultMaxAllocationsPerIP
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

	// 配额按 uid 与客户端 IP 两层计数（#25）：pion 的 QuotaHandler 只负责
	// 分配前检查，真实计数由生命周期事件维护。
	quota := newAllocationQuota(cfg.MaxPerUID, cfg.MaxPerIP)
	permission := permitPeer(cfg.AllowLoopbackPeers)
	sc := turnpkg.ServerConfig{
		Realm:        cfg.Realm,
		AuthHandler:  turnpkg.LongTermTURNRESTAuthHandler(cfg.Secret, pionLog),
		QuotaHandler: quota.allow,
		EventHandler: turnpkg.EventHandler{
			OnAllocationCreated: func(srcAddr, _ net.Addr, _, userID, _ string, _ net.Addr, _ int) {
				quota.add(srcAddr, userID, 1)
			},
			OnAllocationDeleted: func(srcAddr, _ net.Addr, _, userID, _ string) {
				quota.add(srcAddr, userID, -1)
			},
		},
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
			PermissionHandler:     permission,
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
			PermissionHandler:     permission,
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

// permitPeer 返回 pion 的 PermissionHandler：中继只能对公网 peer 建立权限（#25）。
// 没有它时 pion 默认放行一切，任何拿到合法凭证的人都能以服务器源 IP 访问
// 内网服务与云元数据（169.254.169.254）。allowLoopback 仅供本机集成测试。
func permitPeer(allowLoopback bool) turnpkg.PermissionHandler {
	return func(_ net.Addr, peerIP net.IP) bool {
		return !isBlockedPeerIP(peerIP, allowLoopback)
	}
}

// isBlockedPeerIP 判定不允许中继的 peer 地址。IPv4-mapped IPv6 先经 To4()
// 归一到 IPv4 再判断，IPv4-compatible IPv6（::a.b.c.d）也归一后递归判断，
// 避免用 ::ffff:127.0.0.1、::10.0.0.1 之类的表示绕过。
// allowLoopback 只放开环回（127.0.0.0/8、::1）；私网/元数据/组播始终拒绝。
//
// 尚未覆盖的罕见保留段：192.0.0.0/24（IETF 协议分配）、198.18/15（基准测试）、
// 文档段（192.0.2/24、198.51.100/24、203.0.113/24），以及 IPv6 过渡/NAT64 段
// （64:ff9b::/96、2002::/16、2001::/32）。它们不指向本机或云元数据，是否可达
// 取决于宿主机路由与运营商转换器，命中概率极低，暂不为它们增加判断分支。
func isBlockedPeerIP(ip net.IP, allowLoopback bool) bool {
	if len(ip) == 0 {
		return true
	}
	if v4 := ip.To4(); v4 != nil {
		switch {
		case v4[0] == 0: // 0.0.0.0/8，含未指定地址
		case v4.IsLoopback(): // 127.0.0.0/8
			return !allowLoopback
		case v4.IsPrivate(): // 10/8、172.16/12、192.168/16
		case v4[0] == 100 && v4[1]&0xc0 == 64: // 100.64/10 CGNAT
		case v4[0] == 169 && v4[1] == 254: // 169.254/16 链路本地（云元数据）
		case v4.IsMulticast(): // 224.0.0.0/4
		case v4[0] >= 240: // 240/4 保留段，含 255.255.255.255 受限广播
		default:
			return false
		}
		return true
	}
	// IPv6：::1 与未指定地址先判，避免 ::1 被下面的 IPv4-compatible 归一
	// 成 0.0.0.1 后连 allowLoopback 也放不开。
	if ip.IsLoopback() {
		return !allowLoopback
	}
	if ip.IsUnspecified() {
		return true
	}
	// IPv4-compatible IPv6（::a.b.c.d，前 12 字节为 0）是已废弃的过渡表示，
	// Go 的 To4() 不认它：归一成 IPv4 递归判定，::10.0.0.1 / ::169.254.169.254
	// 才不会漏网。
	if len(ip) == 16 && isZeroPrefix12(ip) {
		return isBlockedPeerIP(net.IPv4(ip[12], ip[13], ip[14], ip[15]), allowLoopback)
	}
	// fe80::/10、ff00::/8、fc00::/7（ULA）。
	return ip.IsLinkLocalUnicast() || ip.IsMulticast() || ip[0]&0xfe == 0xfc
}

// isZeroPrefix12 判断 16 字节地址前 12 字节是否为 0（IPv4-compatible 形态）。
func isZeroPrefix12(ip net.IP) bool {
	for _, b := range ip[:12] {
		if b != 0 {
			return false
		}
	}
	return true
}

// allocationQuota 按 uid 与客户端 IP 双层限制并发 allocation（#25）。
// QuotaHandler 只在分配前做只读检查，计数由 OnAllocationCreated/Deleted 维护；
// TCP 多连接并发分配时检查与计数之间不具原子性，可能少量超额，作为软上限足够。
type allocationQuota struct {
	mu        sync.Mutex
	byUID     map[string]int
	byIP      map[string]int
	maxPerUID int
	maxPerIP  int
}

// newAllocationQuota 上限 <=0 回落默认值（配置零值必须落在安全默认上）。
func newAllocationQuota(maxPerUID, maxPerIP int) *allocationQuota {
	if maxPerUID <= 0 {
		maxPerUID = DefaultMaxAllocationsPerUID
	}
	if maxPerIP <= 0 {
		maxPerIP = DefaultMaxAllocationsPerIP
	}
	return &allocationQuota{
		byUID:     make(map[string]int),
		byIP:      make(map[string]int),
		maxPerUID: maxPerUID,
		maxPerIP:  maxPerIP,
	}
}

// allow 供 pion QuotaHandler 调用；超限返回 false，pion 回 486（Allocation Quota Reached）。
func (q *allocationQuota) allow(username, _ string, srcAddr net.Addr) bool {
	uid := uidFromUsername(username)
	ip := normalizedIPFromAddr(srcAddr)
	q.mu.Lock()
	defer q.mu.Unlock()
	if uid != "" && q.byUID[uid] >= q.maxPerUID {
		return false
	}
	if ip != "" && q.byIP[ip] >= q.maxPerIP {
		return false
	}
	return true
}

// add 在 allocation 创建/删除事件上维护计数；delta 为 -1 时归零即删键，
// 计数不会为负，map 也不会无限增长。
func (q *allocationQuota) add(srcAddr net.Addr, userID string, delta int) {
	uid := uidFromUsername(userID)
	ip := normalizedIPFromAddr(srcAddr)
	q.mu.Lock()
	defer q.mu.Unlock()
	bumpCount(q.byUID, uid, delta)
	bumpCount(q.byIP, ip, delta)
}

func bumpCount(m map[string]int, key string, delta int) {
	if key == "" {
		return
	}
	if next := m[key] + delta; next > 0 {
		m[key] = next
	} else {
		delete(m, key)
	}
}

// uidFromUsername 从 REST 凭证用户名 `<expiry>:<uid>` 取最后一个冒号后的 uid。
// pion 的 LongTermTURNRESTAuthHandler 通常已剥掉过期段，两种形态都要成立。
func uidFromUsername(username string) string {
	if i := strings.LastIndexByte(username, ':'); i >= 0 {
		return username[i+1:]
	}
	return username
}

// normalizedIPFromAddr 取客户端 IP 的归一形式：v4-mapped IPv6 收敛到 IPv4，
// 避免同一地址用两种表示绕过按 IP 的配额。
func normalizedIPFromAddr(addr net.Addr) string {
	var ip net.IP
	switch a := addr.(type) {
	case *net.UDPAddr:
		if a != nil {
			ip = a.IP
		}
	case *net.TCPAddr:
		if a != nil {
			ip = a.IP
		}
	default:
		return ""
	}
	if v4 := ip.To4(); v4 != nil {
		return v4.String()
	}
	if ip == nil {
		return ""
	}
	return ip.String()
}
