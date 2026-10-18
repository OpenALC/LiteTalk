package sfu

import (
	"net"
	"testing"
	"time"

	"github.com/pion/webrtc/v4"
)

// freePort 先占一个空闲 TCP 端口再释放，返回端口号供 UDP/TCP 同号绑定用。
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

// TestTCPPortSameNumber UDP 与 TCP 同号并存：这是「单端口复用不冲突」的直接验收
// （#25）——两个独立 socket，不是共 socket。同号绑定失败即测试失败。
func TestTCPPortSameNumber(t *testing.T) {
	port := freePort(t)
	s, err := New(Config{
		UDPPort:    port,
		TCPPort:    port,
		NAT1To1IP:  "127.0.0.1",
		MaxMembers: 4,
	})
	if err != nil {
		t.Fatalf("UDP/TCP 同号 %d 并存失败: %v", port, err)
	}
	defer func() { _ = s.Close() }()

	if got := s.Port(); got != port {
		t.Fatalf("UDP 端口 = %d，期望 %d", got, port)
	}
	if got := s.TCPPort(); got != port {
		t.Fatalf("TCP 端口 = %d，期望 %d", got, port)
	}
}

// TestTCPPortDisabled TCPPort=0 不启 ICE-TCP（保持旧部署行为）。
func TestTCPPortDisabled(t *testing.T) {
	s, err := New(Config{UDPPort: 0, NAT1To1IP: "127.0.0.1", MaxMembers: 4})
	if err != nil {
		t.Fatalf("创建 SFU: %v", err)
	}
	defer func() { _ = s.Close() }()
	if s.TCPPort() != 0 {
		t.Fatalf("TCPPort=0 应不启 TCP mux，实际 %d", s.TCPPort())
	}
}

// selectedProtocol 取 ICE 选中候选对的传输协议；未选出时返回 ICEProtocolUnknown。
func selectedProtocol(t *testing.T, pc *webrtc.PeerConnection) webrtc.ICEProtocol {
	t.Helper()
	pair, err := pc.SCTP().Transport().ICETransport().GetSelectedCandidatePair()
	if err != nil || pair == nil {
		return webrtc.ICEProtocolUnknown
	}
	return pair.Local.Protocol
}

// TestForwardOverICETCP 禁 UDP 场景（#25 核心验收）：两端只启用 TCP 网络类型，
// 必须经 ICE-TCP 候选建连并互通 RTP。建不了连或选中 UDP 都算失败——不许跳过。
func TestForwardOverICETCP(t *testing.T) {
	port := freePort(t)
	s, err := New(Config{
		UDPPort:    port,
		TCPPort:    port,
		NAT1To1IP:  "127.0.0.1",
		MaxMembers: 4,
	})
	if err != nil {
		t.Fatalf("创建 SFU（UDP/TCP 同号 %d）: %v", port, err)
	}
	defer func() { _ = s.Close() }()

	alice := newTestClientNet(t, s, "c1", "alice", true)
	bob := newTestClientNet(t, s, "c1", "bob", true)

	// 等 ICE 选中候选对，断言走的是 TCP（客户端禁 UDP，选中 UDP 即实现有问题）
	deadline := time.Now().Add(10 * time.Second)
	proto := webrtc.ICEProtocolUnknown
	for time.Now().Before(deadline) {
		proto = selectedProtocol(t, alice.pc)
		if proto != webrtc.ICEProtocolUnknown {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if proto == webrtc.ICEProtocolUnknown {
		t.Fatal("10s 内未选出 ICE 候选对（ICE-TCP 未建连）")
	}
	if proto != webrtc.ICEProtocolTCP {
		t.Fatalf("选中候选协议 = %v，客户端已禁 UDP，必须是 TCP", proto)
	}
	t.Log("ICE 选中 TCP 候选对（禁 UDP 网络下 ICE-TCP 直连成立）")

	stop := make(chan struct{})
	defer close(stop)
	sent := newSentSet()
	go alice.sendLoop(stop, sent, "tcp-a2b")
	pkt := bob.waitFor(sent, 10*time.Second)
	if pkt == nil {
		t.Fatal("B 未收到 A 经 ICE-TCP 透传的 RTP（10s 超时）")
	}
	t.Logf("A→B over ICE-TCP 透传成功: payload=%q seq=%d", pkt.Payload, pkt.SequenceNumber)
}
