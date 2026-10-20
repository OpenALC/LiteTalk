package sfu

import (
	"log"
	"net"
	"os"
	"strconv"
	"testing"
	"time"

	"github.com/pion/webrtc/v4"

	"github.com/OpenALC/LiteTalk/server/internal/rtcapi"
	"github.com/OpenALC/LiteTalk/server/internal/turn"
)

// TURN 中继集成测试（#13 P1）：e2eprobe 的 -relay 只检查凭证里有没有 turn: URL，
// 这里起真实的内置 TURN + SFU，客户端强制 ICETransportPolicy=relay，断言
// 选中的候选对是 relay 且 RTP 实际在转发——把审查时的临时探针沉淀成可重复用例。
const (
	relayTestSecret = "relay-test-secret-0123456789"
	relayTestRealm  = "litetalk-relay-test"
	// 中继端口段避开 turn 包测试的 49152~49200，降低 go test 并行包之间的端口竞争。
	relayTestPortMin uint16 = 53000
	relayTestPortMax uint16 = 53100
)

// startRelayTURN 起内置 TURN（UDP+TCP 同号）。AllowLoopbackPeers 只为让 SFU
// 跑在本机：中继 peer 是 127.0.0.1，生产路径默认拒绝（#25 P0）。
func startRelayTURN(t *testing.T) (*turn.Server, int) {
	t.Helper()
	port := freePort(t)
	srv, err := turn.New(turn.Config{
		UDPPort:            port,
		TCPPort:            port,
		RelayIP:            net.ParseIP("127.0.0.1"),
		RelayMinPort:       relayTestPortMin,
		RelayMaxPort:       relayTestPortMax,
		Realm:              relayTestRealm,
		Secret:             relayTestSecret,
		AllowLoopbackPeers: true,
		Logger:             log.New(os.Stderr, "[turn-relay] ", log.Lmicroseconds),
	})
	if err != nil {
		t.Fatalf("启动内置 TURN: %v", err)
	}
	t.Cleanup(func() { _ = srv.Close() })
	return srv, port
}

// relayICEConfig 用 rtcapi 签发的短期凭证构造强制中继的客户端配置。
// transport=udp/tcp 决定客户端到 TURN 的通道（内置 TURN 两者同端口）。
func relayICEConfig(t *testing.T, uid, transport string, port int) webrtc.Configuration {
	t.Helper()
	cfg := rtcapi.Config{Secret: relayTestSecret, TTL: time.Hour}
	username, credential, _ := cfg.Issue(uid)
	return webrtc.Configuration{
		ICEServers: []webrtc.ICEServer{{
			URLs:       []string{"turn:127.0.0.1:" + strconv.Itoa(port) + "?transport=" + transport},
			Username:   username,
			Credential: credential,
		}},
		ICETransportPolicy: webrtc.ICETransportPolicyRelay,
	}
}

// waitSelectedPair 等 ICE 选出候选对（未选出返回 nil）。
// 先等 PeerConnection 进入 Connected/Completed 再读 ICETransport：pion 的
// GetSelectedCandidatePair 与 StartContext 并发访问同一字段，连接建立前轮询
// 会被 -race 判为数据竞争（CI 偶发，见 #13 relay run）。连接失败/关闭直接 nil。
func waitSelectedPair(pc *webrtc.PeerConnection, timeout time.Duration) *webrtc.ICECandidatePair {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		switch pc.ConnectionState() {
		case webrtc.PeerConnectionStateConnected:
			goto connected
		case webrtc.PeerConnectionStateFailed, webrtc.PeerConnectionStateClosed:
			return nil
		}
		time.Sleep(50 * time.Millisecond)
	}
	return nil

connected:
	for time.Now().Before(deadline) {
		pair, err := pc.SCTP().Transport().ICETransport().GetSelectedCandidatePair()
		if err == nil && pair != nil {
			return pair
		}
		time.Sleep(50 * time.Millisecond)
	}
	return nil
}

// assertRelayPair 断言选中的是「客户端 relay ↔ SFU host」候选对，并返回它。
func assertRelayPair(t *testing.T, alias string, pc *webrtc.PeerConnection) *webrtc.ICECandidatePair {
	t.Helper()
	pair := waitSelectedPair(pc, 20*time.Second)
	if pair == nil {
		t.Fatalf("%s: 20s 内未选出 ICE 候选对（relay 建连失败）", alias)
	}
	if pair.Local.Typ != webrtc.ICECandidateTypeRelay {
		t.Fatalf("%s: 本地候选类型 = %s，期望 relay（实际未走 TURN 中继）", alias, pair.Local.Typ)
	}
	if pair.Remote.Typ != webrtc.ICECandidateTypeHost {
		t.Fatalf("%s: 远端候选类型 = %s，期望 host（SFU ICE-Lite）", alias, pair.Remote.Typ)
	}
	t.Logf("%s 选中候选对: local=%s %s:%d ↔ remote=%s %s:%d",
		alias, pair.Local.Typ, pair.Local.Address, pair.Local.Port,
		pair.Remote.Typ, pair.Remote.Address, pair.Remote.Port)
	return pair
}

// assertRTPGrowing 断言 A→B 有 RTP 且包数在观察窗口内继续增长。
func assertRTPGrowing(t *testing.T, alice, bob *testClient, prefix string) {
	t.Helper()
	stop := make(chan struct{})
	defer close(stop)
	sent := newSentSet()
	go alice.sendLoop(stop, sent, prefix)
	if bob.waitFor(sent, 20*time.Second) == nil {
		t.Fatalf("%s: B 未收到 A 经 TURN 中继的 RTP（20s 超时）", prefix)
	}
	first := bob.countMatching(sent)
	time.Sleep(600 * time.Millisecond)
	second := bob.countMatching(sent)
	if second <= first {
		t.Fatalf("%s: RTP 包数未增长: %d → %d", prefix, first, second)
	}
	t.Logf("%s: RTP 包数增长 %d → %d（A→B）", prefix, first, second)
}

// TestRelayMediaOverTURN 完整媒体路径走 UDP TURN 中继：
// 两端客户端都强制 relay，选中候选对必须是 relay，且 A→B 的 RTP 包数持续增长。
func TestRelayMediaOverTURN(t *testing.T) {
	_, turnPort := startRelayTURN(t)
	s := newTestSFU(t, 4)
	alice := newTestClientConfig(t, s, "relay-udp", "alice", nil, relayICEConfig(t, "relay-alice", "udp", turnPort))
	bob := newTestClientConfig(t, s, "relay-udp", "bob", nil, relayICEConfig(t, "relay-bob", "udp", turnPort))

	assertRelayPair(t, "alice(udp)", alice.pc)
	assertRelayPair(t, "bob(udp)", bob.pc)
	assertRTPGrowing(t, alice, bob, "relay-udp-a2b")
}

// TestRelayMediaOverTURNTCP 客户端到 TURN 的通道走 TCP（?transport=tcp），
// 覆盖「UDP 被封，只能经 TCP 中继」的路径。UDP4 仍保留在客户端网络类型里：
// pion 只在配置了 UDP 候选网络类型时发布 relay 候选；relay allocation 本身
// 始终是 UDP，TCP 只承载客户端↔TURN 的控制与数据。
func TestRelayMediaOverTURNTCP(t *testing.T) {
	_, turnPort := startRelayTURN(t)
	s := newTestSFU(t, 4)
	networks := []webrtc.NetworkType{webrtc.NetworkTypeUDP4, webrtc.NetworkTypeTCP4}
	alice := newTestClientConfig(t, s, "relay-tcp", "alice", networks, relayICEConfig(t, "relay-tcp-alice", "tcp", turnPort))
	bob := newTestClientConfig(t, s, "relay-tcp", "bob", networks, relayICEConfig(t, "relay-tcp-bob", "tcp", turnPort))

	assertRelayPair(t, "alice(tcp)", alice.pc)
	assertRelayPair(t, "bob(tcp)", bob.pc)
	assertRTPGrowing(t, alice, bob, "relay-tcp-a2b")
}
