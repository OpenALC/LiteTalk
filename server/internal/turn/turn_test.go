package turn

import (
	"crypto/hmac"
	"crypto/sha1"
	"encoding/base64"
	"fmt"
	"net"
	"strconv"
	"testing"
	"time"

	turnpkg "github.com/pion/turn/v5"
)

// signREST 凭证与 rtcapi.SignCredential 逐字节同款（HMAC-SHA1 + base64），
// 用于验证内置 TURN 与 REST 签发端点互通。
func signREST(secret, username string) string {
	mac := hmac.New(sha1.New, []byte(secret))
	mac.Write([]byte(username))
	return base64.StdEncoding.EncodeToString(mac.Sum(nil))
}

// freePort 先占空闲 TCP 端口再释放，供 UDP/TCP 同号绑定用。
func freePort(t *testing.T) int {
	t.Helper()
	ln, err := net.ListenTCP("tcp", &net.TCPAddr{IP: net.IPv4zero, Port: 0})
	if err != nil {
		t.Fatal(err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	_ = ln.Close()
	return port
}

func testConfig(udpPort, tcpPort int) Config {
	return Config{
		UDPPort:      udpPort,
		TCPPort:      tcpPort,
		RelayIP:      net.ParseIP("127.0.0.1"),
		RelayMinPort: 49152,
		RelayMaxPort: 49200,
		Realm:        "test",
		Secret:       "test-secret-0123456789",
	}
}

// TestServerListeners UDP+TCP 同号并存（#25「单端口复用不冲突」的 TURN 侧验收）。
func TestServerListeners(t *testing.T) {
	port := freePort(t)
	srv, err := New(testConfig(port, port))
	if err != nil {
		t.Fatalf("UDP/TCP 同号 %d 启动失败: %v", port, err)
	}
	defer func() { _ = srv.Close() }()

	if srv.UDPAddr() == nil {
		t.Fatal("UDP 监听未建立")
	}
	if srv.TCPAddr() == nil {
		t.Fatal("TCP 监听未建立")
	}
	t.Logf("UDP %s / TCP %s 并存 OK", srv.UDPAddr(), srv.TCPAddr())
}

// TestServerRejectsEmpty 配置缺 Secret / RelayIP / 全零端口必须报错，不静默起。
func TestServerRejectsEmpty(t *testing.T) {
	if _, err := New(Config{}); err == nil {
		t.Fatal("全零端口应报错")
	}
	c := testConfig(0, freePort(t))
	c.Secret = ""
	if _, err := New(c); err == nil {
		t.Fatal("空 Secret 应报错")
	}
	c = testConfig(0, freePort(t))
	c.RelayIP = nil
	if _, err := New(c); err == nil {
		t.Fatal("空 RelayIP 应报错")
	}
}

// restCredential 生成一对 REST 短期凭证（expiry:uid 语义，与 rtcapi.Issue 同款）。
func restCredential(secret, uid string, ttl time.Duration) (username, credential string) {
	expiry := time.Now().Add(ttl).Unix()
	username = fmt.Sprintf("%d:%s", expiry, uid)
	return username, signREST(secret, username)
}

// newUDPAlloc 经 UDP 向内置 TURN 申请中继；成功后中继与客户端保持到测试结束，
// 便于测试对中继端口发起真实读写。失败返回错误（配额测试要观察失败本身）。
func newUDPAlloc(t *testing.T, port int, secret, uid string) (net.PacketConn, error) {
	t.Helper()
	username, credential := restCredential(secret, uid, time.Hour)
	addr := net.JoinHostPort("127.0.0.1", strconv.Itoa(port))
	conn, err := net.ListenPacket("udp4", "0.0.0.0:0")
	if err != nil {
		return nil, fmt.Errorf("监听本地 UDP: %w", err)
	}
	client, err := turnpkg.NewClient(&turnpkg.ClientConfig{
		STUNServerAddr: addr,
		TURNServerAddr: addr,
		Conn:           conn,
		Username:       username,
		Password:       credential,
		Realm:          "test",
	})
	if err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("创建 TURN 客户端: %w", err)
	}
	if err := client.Listen(); err != nil {
		client.Close()
		_ = conn.Close()
		return nil, fmt.Errorf("TURN 客户端 Listen: %w", err)
	}
	relay, err := client.Allocate()
	if err != nil {
		client.Close()
		_ = conn.Close()
		return nil, err
	}
	t.Cleanup(func() {
		_ = relay.Close()
		client.Close()
		_ = conn.Close()
	})
	return relay, nil
}

// allocateUDP 申请中继，失败即 Fatal（凭证互通用例）。
func allocateUDP(t *testing.T, port int, secret, uid string) net.PacketConn {
	t.Helper()
	relay, err := newUDPAlloc(t, port, secret, uid)
	if err != nil {
		t.Fatalf("REST 凭证经 UDP Allocate 失败（凭证互通断裂）: %v", err)
	}
	t.Logf("UDP 中继就绪 %s（凭证互通 OK）", relay.LocalAddr())
	return relay
}

// TestAllocateUDP REST 短期凭证与内置 TURN 互通：签发端点同款 HMAC 可直接 Allocate。
func TestAllocateUDP(t *testing.T) {
	port := freePort(t)
	srv, err := New(testConfig(port, port))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = srv.Close() }()
	allocateUDP(t, port, "test-secret-0123456789", "42")
}

// TestPeerIPPolicy 直接覆盖 PermissionHandler 的地址判定（#25）：内网/环回/
// 元数据/组播一律拒绝，公网与 v4-mapped 公网放行。
func TestPeerIPPolicy(t *testing.T) {
	blocked := []string{
		"127.0.0.1", "127.255.255.254", "::ffff:127.0.0.1",
		"0.0.0.0", "0.1.2.3", "::",
		"10.0.0.1", "172.16.0.1", "172.31.255.255", "192.168.1.1", "::ffff:192.168.1.1",
		"100.64.0.1", "100.127.255.254",
		"169.254.169.254", "::ffff:169.254.169.254",
		"224.0.0.1", "239.255.255.255", "255.255.255.255",
		"::1", "fe80::1", "ff02::1", "fc00::1", "fdff:ffff::1",
	}
	for _, s := range blocked {
		if !isBlockedPeerIP(net.ParseIP(s)) {
			t.Errorf("%s 应被拒绝", s)
		}
		if permitPeer(nil, net.ParseIP(s)) {
			t.Errorf("%s 不应通过 PermissionHandler", s)
		}
	}
	allowed := []string{
		"8.8.8.8", "1.1.1.1", "203.0.113.10",
		"::ffff:8.8.8.8", "2001:4860:4860::8888", "2606:4700::1111",
	}
	for _, s := range allowed {
		if isBlockedPeerIP(net.ParseIP(s)) {
			t.Errorf("%s 应放行", s)
		}
		if !permitPeer(nil, net.ParseIP(s)) {
			t.Errorf("%s 应通过 PermissionHandler", s)
		}
	}
	if !isBlockedPeerIP(nil) || !isBlockedPeerIP(net.IP{}) {
		t.Error("空 peer IP 应拒绝")
	}
}

// TestRelayToLoopbackDenied 合法凭证只应换来中继端口，不能借中继访问 127.0.0.1
// （#25 P0：实测可打穿本机 Redis/MySQL）。分配本身必须仍成功。
func TestRelayToLoopbackDenied(t *testing.T) {
	port := freePort(t)
	srv, err := New(testConfig(port, port))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = srv.Close() }()

	relay := allocateUDP(t, port, "test-secret-0123456789", "46")
	if _, err := relay.WriteTo([]byte("probe"), &net.UDPAddr{IP: net.ParseIP("127.0.0.1"), Port: 8080}); err == nil {
		t.Fatal("中继到 127.0.0.1 应被 CreatePermission 拒绝")
	} else {
		t.Logf("环回 peer 被拒: %v", err)
	}
}

// TestAllocationQuotaPerUID 同一 uid 第 5 个并发 allocation 必须失败（486），
// 换一个 uid 仍可分配（#25 P1：单凭证刷爆中继端口 = 对所有人拒绝服务）。
func TestAllocationQuotaPerUID(t *testing.T) {
	port := freePort(t)
	cfg := testConfig(port, port)
	srv, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = srv.Close() }()

	for i := 0; i < maxAllocationsPerUID; i++ {
		if _, err := newUDPAlloc(t, port, cfg.Secret, "quota-uid"); err != nil {
			t.Fatalf("第 %d 个 allocation 应成功: %v", i+1, err)
		}
	}
	if _, err := newUDPAlloc(t, port, cfg.Secret, "quota-uid"); err == nil {
		t.Fatalf("同一 uid 第 %d 个 allocation 应被配额拒绝", maxAllocationsPerUID+1)
	} else {
		t.Logf("同一 uid 超限被拒: %v", err)
	}
	if _, err := newUDPAlloc(t, port, cfg.Secret, "other-uid"); err != nil {
		t.Fatalf("换 uid 后应可分配: %v", err)
	}
}

// TestAllocationQuotaIPPure IP 层配额是纯计数逻辑，直接测 tracker：8 个不同
// uid 后第 9 个即使 uid 未超限也必须被拒。
func TestAllocationQuotaIPPure(t *testing.T) {
	q := newAllocationQuota()
	src := &net.UDPAddr{IP: net.ParseIP("203.0.113.5"), Port: 12345}
	for i := 0; i < maxAllocationsPerIP; i++ {
		uid := fmt.Sprintf("uid-%d", i)
		if !q.allow(uid, "test", src) {
			t.Fatalf("第 %d 个 uid 不应被拒", i+1)
		}
		q.add(src, uid, 1)
	}
	if q.allow("uid-new", "test", src) {
		t.Fatal("同一 IP 超过上限后应拒绝")
	}
	// 计数必须能归零且不为负：全部删除后重新放行。
	for i := 0; i < maxAllocationsPerIP; i++ {
		q.add(src, fmt.Sprintf("uid-%d", i), -1)
	}
	q.add(src, "uid-new", -1) // 多余的删除不得把计数打成负数
	if !q.allow("uid-new", "test", src) {
		t.Fatal("计数归零后应重新放行")
	}
}

// TestUIDFromUsername 凭证用户名的两种形态（pion 有时已剥掉过期段）。
func TestUIDFromUsername(t *testing.T) {
	cases := map[string]string{
		"1234567890:42": "42",
		"42":            "42",
		"":              "",
		"1:2:3":         "3",
	}
	for in, want := range cases {
		if got := uidFromUsername(in); got != want {
			t.Errorf("uidFromUsername(%q) = %q，期望 %q", in, got, want)
		}
	}
}

// allocateTCP 经 TCP 向内置 TURN 申请中继（模拟禁 UDP 网络：客户端零 UDP socket，
// 走 turn.NewSTUNConn 把 TURN 报文封装在 TCP 流上）。
func allocateTCP(t *testing.T, port int, secret, uid string) {
	t.Helper()
	username, credential := restCredential(secret, uid, time.Hour)
	addr := net.JoinHostPort("127.0.0.1", strconv.Itoa(port))
	conn, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatalf("拨号 TURN TCP: %v", err)
	}
	client, err := turnpkg.NewClient(&turnpkg.ClientConfig{
		STUNServerAddr: addr,
		TURNServerAddr: addr,
		Conn:           turnpkg.NewSTUNConn(conn),
		Username:       username,
		Password:       credential,
		Realm:          "test",
	})
	if err != nil {
		_ = conn.Close()
		t.Fatalf("创建 TURN TCP 客户端: %v", err)
	}
	defer client.Close()
	if err := client.Listen(); err != nil {
		t.Fatalf("TURN TCP 客户端 Listen: %v", err)
	}
	relay, err := client.Allocate()
	if err != nil {
		t.Fatalf("REST 凭证经 TCP Allocate 失败（禁 UDP 兜底断裂）: %v", err)
	}
	defer func() { _ = relay.Close() }()
	t.Logf("TCP 通道中继就绪 %s（禁 UDP 网络兜底 OK）", relay.LocalAddr())
}

// TestAllocateTCPOnly 禁 UDP 场景（#25 核心）：TURN 只听 TCP，客户端零 UDP，
// 必须能经 TCP 3478 完成 Allocate 拿到中继。
func TestAllocateTCPOnly(t *testing.T) {
	port := freePort(t)
	cfg := testConfig(0, port) // UDP=0 不听
	srv, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = srv.Close() }()
	if srv.UDPAddr() != nil {
		t.Fatal("UDPPort=0 不应建立 UDP 监听")
	}
	allocateTCP(t, port, cfg.Secret, "43")
}

// TestAllocateTCPDual 双栈监听下 TCP 通道同样可用（TURN UDP+TCP 并存互不干扰）。
func TestAllocateTCPDual(t *testing.T) {
	port := freePort(t)
	srv, err := New(testConfig(port, port))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = srv.Close() }()
	allocateTCP(t, port, "test-secret-0123456789", "44")
}

// newTCPConnector 建立经 TURN TCP 的 allocation，返回发起 RFC6062 Connect 的
// 闭包；客户端与 allocation 保持到测试结束。
func newTCPConnector(t *testing.T, port int, secret, uid string) func(peer net.Addr) error {
	t.Helper()
	username, credential := restCredential(secret, uid, time.Hour)
	addr := net.JoinHostPort("127.0.0.1", strconv.Itoa(port))
	conn, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatalf("拨号 TURN TCP: %v", err)
	}
	client, err := turnpkg.NewClient(&turnpkg.ClientConfig{
		STUNServerAddr: addr,
		TURNServerAddr: addr,
		Conn:           turnpkg.NewSTUNConn(conn),
		Username:       username,
		Password:       credential,
		Realm:          "test",
	})
	if err != nil {
		_ = conn.Close()
		t.Fatalf("创建 TURN TCP 客户端: %v", err)
	}
	if err := client.Listen(); err != nil {
		client.Close()
		_ = conn.Close()
		t.Fatalf("TURN TCP 客户端 Listen: %v", err)
	}
	alloc, err := client.AllocateTCP()
	if err != nil {
		client.Close()
		_ = conn.Close()
		t.Fatalf("TCP allocation 失败: %v", err)
	}
	t.Cleanup(func() {
		_ = alloc.Close()
		client.Close()
		_ = conn.Close()
	})
	return func(peer net.Addr) error {
		_, err := alloc.Connect(peer)
		return err
	}
}

// TestConnectTCPToLoopbackDenied RFC6062 CONNECT 同样受 PermissionHandler 约束
// （#25 P0）：TCP 中继不能连到 127.0.0.1 上的内部 TCP 服务。
func TestConnectTCPToLoopbackDenied(t *testing.T) {
	port := freePort(t)
	cfg := testConfig(0, port)
	srv, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = srv.Close() }()

	connect := newTCPConnector(t, port, cfg.Secret, "47")
	if err := connect(&net.TCPAddr{IP: net.ParseIP("127.0.0.1"), Port: 6379}); err == nil {
		t.Fatal("TCP CONNECT 到 127.0.0.1 应被拒绝")
	} else {
		t.Logf("环回 peer 的 TCP CONNECT 被拒: %v", err)
	}
}

// TestExpiredCredentialRejected 过期的 REST 凭证必须被拒（LongTermTURNRESTAuthHandler
// 的时间窗语义，防止凭证永久有效）。
func TestExpiredCredentialRejected(t *testing.T) {
	port := freePort(t)
	cfg := testConfig(0, port)
	srv, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = srv.Close() }()

	username, credential := restCredential(cfg.Secret, "45", -time.Minute) // 已过期
	addr := net.JoinHostPort("127.0.0.1", strconv.Itoa(port))
	conn, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	client, err := turnpkg.NewClient(&turnpkg.ClientConfig{
		STUNServerAddr: addr,
		TURNServerAddr: addr,
		Conn:           turnpkg.NewSTUNConn(conn),
		Username:       username,
		Password:       credential,
		Realm:          "test",
	})
	if err != nil {
		_ = conn.Close()
		t.Fatal(err)
	}
	defer client.Close()
	if err := client.Listen(); err != nil {
		t.Fatal(err)
	}
	if _, err := client.Allocate(); err == nil {
		t.Fatal("过期凭证 Allocate 应失败")
	} else {
		t.Logf("过期凭证被拒: %v", err)
	}
}
