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

// allocateUDP 经 UDP 向内置 TURN 申请中继（凭证走 REST 短期凭证）。
func allocateUDP(t *testing.T, port int, secret, uid string) {
	t.Helper()
	username, credential := restCredential(secret, uid, time.Hour)
	addr := net.JoinHostPort("127.0.0.1", strconv.Itoa(port))
	conn, err := net.ListenPacket("udp4", "0.0.0.0:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()
	client, err := turnpkg.NewClient(&turnpkg.ClientConfig{
		STUNServerAddr: addr,
		TURNServerAddr: addr,
		Conn:           conn,
		Username:       username,
		Password:       credential,
		Realm:          "test",
	})
	if err != nil {
		t.Fatalf("创建 TURN 客户端: %v", err)
	}
	defer client.Close()
	if err := client.Listen(); err != nil {
		t.Fatalf("TURN 客户端 Listen: %v", err)
	}
	relay, err := client.Allocate()
	if err != nil {
		t.Fatalf("REST 凭证经 UDP Allocate 失败（凭证互通断裂）: %v", err)
	}
	defer func() { _ = relay.Close() }()
	t.Logf("UDP 中继就绪 %s（凭证互通 OK）", relay.LocalAddr())
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
