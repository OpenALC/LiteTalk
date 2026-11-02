package voice

import (
	"context"
	"net"
	"testing"
	"time"

	"github.com/OpenALC/LiteTalk/client/core/api"
	"github.com/OpenALC/LiteTalk/client/core/audio"
	"github.com/OpenALC/LiteTalk/client/core/rtc"
	"github.com/OpenALC/LiteTalk/client/core/signal"
	"github.com/pion/webrtc/v4"
)

// TestSignatureAddr 地址判定表：只有真正参与出网的地址进入签名（#117）。
func TestSignatureAddr(t *testing.T) {
	cases := []struct {
		ip   string
		want string // 空串 = 丢弃
	}{
		{"127.0.0.1", ""},                    // 回环
		{"::1", ""},                          // 回环 v6
		{"169.254.13.7", ""},                 // link-local v4（APIPA）
		{"fe80::1c2b:3d4e:5f60:7081%12", ""}, // link-local v6
		{"ff02::1", ""},                      // link-local multicast
		{"224.0.0.251", ""},                  // multicast
		{"0.0.0.0", ""},                      // unspecified
		{"::", ""},                           // unspecified v6
		{"10.0.0.5", "10.0.0.5"},             // 普通 v4
		{"192.168.1.20", "192.168.1.20"},
		// v4-mapped v6 归一成 v4，不另生成一条 v6 分量。
		{"::ffff:10.0.0.5", "10.0.0.5"},
		// v6 只留 /64 前缀：同一前缀下换接口标识符不会改变签名。
		{"2001:db8:1:2:aaaa:bbbb:cccc:dddd", "2001:db8:1:2::/64"},
		{"2001:db8:1:2:1111:2222:3333:4444", "2001:db8:1:2::/64"},
		{"2001:db8:9:9::1", "2001:db8:9:9::/64"},
		{"2606:4700::1111", "2606:4700::/64"},
	}
	for _, c := range cases {
		ip := net.ParseIP(c.ip)
		got, ok := signatureAddr(ip)
		if c.want == "" {
			if ok {
				t.Errorf("signatureAddr(%q) = %q，应丢弃", c.ip, got)
			}
			continue
		}
		if !ok || got != c.want {
			t.Errorf("signatureAddr(%q) = %q,%v，want %q", c.ip, got, ok, c.want)
		}
	}
}

// TestSignatureFrom 接口级判定：down / 回环 / 虚拟接口不参与签名；隐私地址
// 轮换不改变签名，真实前缀与 v4 变化会让签名变。
func TestSignatureFrom(t *testing.T) {
	base := func() []ifaceAddrs {
		return []ifaceAddrs{
			{name: "Ethernet", up: true, addrs: []net.IP{
				net.ParseIP("10.0.0.5"), net.ParseIP("169.254.3.4"),
				net.ParseIP("fe80::1"),
			}},
			// Hyper-V 虚拟交换机：地址随宿主网络反复变化，不得入签。
			{name: "vEthernet (Default Switch)", up: true, addrs: []net.IP{net.ParseIP("192.168.56.1")}},
			{name: "vEthernet (WSL)", up: true, addrs: []net.IP{net.ParseIP("172.24.144.1")}},
			{name: "docker0", up: true, addrs: []net.IP{net.ParseIP("172.17.0.1")}},
			{name: "virbr0", up: true, addrs: []net.IP{net.ParseIP("192.168.122.1")}},
			{name: "Tunnel tap-windows", up: true, addrs: []net.IP{net.ParseIP("10.8.0.6")}},
			// 隧道型 VPN：连上/断开都只是隧道自己的地址在变。
			{name: "wg0", up: true, addrs: []net.IP{net.ParseIP("10.66.0.2")}},
			// down 的接口不参与。
			{name: "Wi-Fi", up: false, addrs: []net.IP{net.ParseIP("192.168.1.7")}},
			// 回环接口即使带非回环地址也不参与。
			{name: "Loopback", up: true, loop: true, addrs: []net.IP{net.ParseIP("10.9.9.9")}},
		}
	}

	got := signatureFrom(base())
	if got != "Ethernet=10.0.0.5" {
		t.Fatalf("签名 = %q，只应包含物理接口的全局 v4", got)
	}

	// 隐私 IPv6 在同一 /64 下轮换接口标识符：签名必须不变。
	rotated := base()
	rotated[0].addrs = append(rotated[0].addrs,
		net.ParseIP("2001:db8:1:2:aaaa:bbbb:cccc:dddd"))
	withV6 := signatureFrom(rotated)
	if withV6 != "Ethernet=10.0.0.5,2001:db8:1:2::/64" {
		t.Fatalf("IPv6 应折叠成 /64 前缀，实际 %q", withV6)
	}
	rotated[0].addrs[len(rotated[0].addrs)-1] = net.ParseIP("2001:db8:1:2:1111:2222:3333:4444")
	if again := signatureFrom(rotated); again != withV6 {
		t.Fatalf("隐私地址轮换改变了签名：%q → %q", withV6, again)
	}

	// 真换网（前缀与 v4 都变）必须让签名变。
	rotated[0].addrs = []net.IP{net.ParseIP("10.1.0.5"), net.ParseIP("2001:db8:5:6::1")}
	if moved := signatureFrom(rotated); moved == withV6 {
		t.Fatal("换到新网络后签名应变化")
	}

	// 全部接口被过滤（只剩虚拟网卡）：合法空签名，不是枚举失败。
	onlyVirtual := []ifaceAddrs{{name: "vEthernet (WSL)", up: true, addrs: []net.IP{net.ParseIP("172.24.144.1")}}}
	if s := signatureFrom(onlyVirtual); s != "" {
		t.Fatalf("虚拟接口不应入签，实际 %q", s)
	}
}

// TestLANNotReservedBug 旧实现把非回环一律入签，这里反向确认物理接口上的
// 公共地址仍被保留（过滤不能把真实通路一起砍掉）。
func TestSignatureKeepsPhysicalGlobalAddrs(t *testing.T) {
	got := signatureFrom([]ifaceAddrs{
		{name: "Wi-Fi", up: true, addrs: []net.IP{net.ParseIP("203.0.113.9"), net.ParseIP("2001:db8:aa:bb::7")}},
	})
	if got != "Wi-Fi=2001:db8:aa:bb::/64,203.0.113.9" {
		t.Fatalf("物理接口全局地址应保留并排序，实际 %q", got)
	}
}

// TestNetworkWatcherTriggersOnce 签名稳定变化后才触发一次；持续同签名不重复，
// 抖动回到原签名则取消（#49 A）。
func TestNetworkWatcherTriggersOnce(t *testing.T) {
	sig, sampleOK, inRoom := "a", true, true
	fired := 0
	w := &networkWatcher{
		interval: time.Second,
		debounce: 2 * time.Second,
		sample:   func() (string, bool) { return sig, sampleOK },
		inRoom:   func() bool { return inRoom },
		recover:  func(string) { fired++ },
	}
	t0 := time.Unix(1_700_000_000, 0)

	if w.poll(t0) {
		t.Fatal("首次采样只建立基线，不应触发")
	}
	if w.poll(t0.Add(time.Second)) {
		t.Fatal("签名未变不应触发")
	}

	sig = "b"
	if w.poll(t0.Add(2 * time.Second)) {
		t.Fatal("首次看到新签名只进入待确认，不应触发")
	}
	if w.poll(t0.Add(3 * time.Second)) {
		t.Fatal("去抖窗口内不应触发")
	}
	if !w.poll(t0.Add(4 * time.Second)) {
		t.Fatal("新签名稳定超过去抖窗口后应触发一次")
	}
	if fired != 1 {
		t.Fatalf("recover 调用次数 = %d, want 1", fired)
	}
	if w.poll(t0.Add(5 * time.Second)) {
		t.Fatal("签名稳定后不应重复触发")
	}
	if fired != 1 {
		t.Fatalf("recover 调用次数 = %d, want 1", fired)
	}

	// 变化后又在去抖窗口内回到原签名：取消这次候选。
	sig = "c"
	w.poll(t0.Add(6 * time.Second))
	sig = "b"
	if w.poll(t0.Add(7 * time.Second)) {
		t.Fatal("签名回到基线不应触发")
	}
	if fired != 1 {
		t.Fatalf("recover 调用次数 = %d, want 1", fired)
	}
}

// TestNetworkWatcherNotInRoom 不在房内时更新基线但不触发；之后进房不补触发。
func TestNetworkWatcherNotInRoom(t *testing.T) {
	sig, inRoom := "a", false
	fired := 0
	w := &networkWatcher{
		interval: time.Second,
		debounce: time.Second,
		sample:   func() (string, bool) { return sig, true },
		inRoom:   func() bool { return inRoom },
		recover:  func(string) { fired++ },
	}
	t0 := time.Unix(1_700_000_000, 0)

	w.poll(t0)
	sig = "b"
	if w.poll(t0.Add(time.Second)) {
		t.Fatal("离房时不应触发恢复")
	}
	if w.poll(t0.Add(2 * time.Second)) {
		t.Fatal("离房时去抖确认也不应触发")
	}
	if fired != 0 {
		t.Fatalf("recover 调用次数 = %d, want 0", fired)
	}

	// 基线已更新为 b：进房后同一签名不应触发。
	inRoom = true
	if w.poll(t0.Add(3 * time.Second)) {
		t.Fatal("进房不该因为入房前换过网而重建")
	}
}

// TestNetworkWatcherSampleError 枚举失败（ok=false）本次采样作废，不能把
// 瞬时错误当成网络变化。
func TestNetworkWatcherSampleError(t *testing.T) {
	sig, sampleOK := "a", true
	fired := 0
	w := &networkWatcher{
		interval: time.Second,
		debounce: time.Second,
		sample:   func() (string, bool) { return sig, sampleOK },
		inRoom:   func() bool { return true },
		recover:  func(string) { fired++ },
	}
	t0 := time.Unix(1_700_000_000, 0)
	w.poll(t0)

	sampleOK = false
	if w.poll(t0.Add(time.Second)) {
		t.Fatal("采样失败不应触发")
	}
	// 恢复后签名与基线一致：仍不触发。
	sampleOK, sig = true, "a"
	if w.poll(t0.Add(2 * time.Second)) {
		t.Fatal("采样恢复且签名未变不应触发")
	}
	if fired != 0 {
		t.Fatalf("recover 调用次数 = %d, want 0", fired)
	}
}

// newNetworkTestSession 构造可发信令的会话（复用 lateJoinStub 记录上行消息）。
func newNetworkTestSession(t *testing.T) (*Session, *lateJoinStub) {
	t.Helper()
	stub := newLateJoinStub(t)
	s := NewSession(api.NewAPIClient(stub.URL), "tok", "n", Events{}, nil)
	s.ws = signal.NewClient(signal.Config{URL: stub.wsURL(), ReconnectMin: 20 * time.Millisecond})
	t.Cleanup(s.ws.Close)
	s.registerHandlers()
	s.pipe = newNullPipeline(t)
	s.remoteMgr = audio.NewRemoteManager(audio.NewMixer(func([]int16) int { return 0 }), nil)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go s.ws.Run(ctx)
	waitSignalConnected(t, s)
	return s, stub
}

// TestRecoverNetworkRebuildsPeer 网络变化后拆掉本地 PC，用新 PC 重发
// rtc.offer（服务端同 uid 顶替），不发 channel.join/leave——服务端在仍处于
// 目标房时对 channel.join 回 already_in_channel，拿不到新回执（#49/#114）。
func TestRecoverNetworkRebuildsPeer(t *testing.T) {
	s, stub := newNetworkTestSession(t)
	rtcFailedCalled := false
	s.ev.OnRTCFailed = func(string) { rtcFailedCalled = true }
	s.roomID.Store("room1")
	oldPeer, err := rtc.NewPeer(rtc.PeerConfig{StreamID: "u1"})
	if err != nil {
		t.Fatalf("NewPeer: %v", err)
	}
	s.peerRef.Store(oldPeer)

	s.recoverNetwork("test")

	waitUntil(t, "网络变化重发 rtc.offer", func() bool { return stub.saw(signal.TypeRTCOffer) })
	newPeer := s.peerRef.Load()
	if newPeer == nil {
		t.Fatal("恢复后应换上新的 PC（等 rtc.answer / 重协商）")
	}
	if newPeer == oldPeer {
		t.Fatal("应换新 PC，而不是复用旧实例")
	}
	if st := oldPeer.ConnectionState(); st != webrtc.PeerConnectionStateClosed {
		t.Fatalf("旧 PC 应关闭，实际 %v", st)
	}
	if got := s.roomID.Load().(string); got != "room1" {
		t.Fatalf("恢复期间房间态不应回滚，roomID = %q", got)
	}
	if stub.saw(signal.TypeChannelJoin) {
		t.Fatal("网络恢复不应重发 channel.join（服务端会回 already_in_channel）")
	}
	if stub.saw(signal.TypeChannelLeave) {
		t.Fatal("恢复不得发 channel.leave（房间成员会看到假离房）")
	}
	if rtcFailedCalled {
		t.Fatal("网络恢复是静默路径，不应触发 rtc:failed 提示")
	}
}

// TestRecoverNetworkNotInRoom 不在房内不重建、不发信令。
func TestRecoverNetworkNotInRoom(t *testing.T) {
	s, stub := newNetworkTestSession(t)
	peer, err := rtc.NewPeer(rtc.PeerConfig{StreamID: "u1"})
	if err != nil {
		t.Fatalf("NewPeer: %v", err)
	}
	s.peerRef.Store(peer)

	s.recoverNetwork("test")
	time.Sleep(100 * time.Millisecond)

	if stub.saw(signal.TypeChannelJoin) {
		t.Fatal("离房时不得重发 channel.join")
	}
	if s.peerRef.Load() != peer {
		t.Fatal("离房时不得拆 PC")
	}
}

// TestSetupPeerOfferErrorNoRemoteManager 重建路径的失败出口可返回错误而不是
// panic（无媒体链路时防御性早退）。
func TestSetupPeerOfferErrorNoRemoteManager(t *testing.T) {
	s := NewSession(api.NewAPIClient("http://127.0.0.1:1"), "tok", "n", Events{}, nil)
	if err := s.setupPeerOffer(); err == nil {
		t.Fatal("remoteMgr 为空时应返回错误")
	}
	if s.peerRef.Load() != nil {
		t.Fatal("失败后不应留下半成品 PC")
	}
}

// TestRecoverNetworkSignalDown 信令未连通（重连窗口）时不动媒体链路：WS
// OnConnect 的重发 join 会兜底重建，此时拆 PC 只会制造空窗。
func TestRecoverNetworkSignalDown(t *testing.T) {
	s := NewSession(api.NewAPIClient("http://127.0.0.1:1"), "tok", "n", Events{}, nil)
	s.ws = signal.NewClient(signal.Config{URL: "ws://127.0.0.1:1/ws"}) // 未 Run
	t.Cleanup(s.ws.Close)
	s.roomID.Store("room1")
	peer, err := rtc.NewPeer(rtc.PeerConfig{StreamID: "u1"})
	if err != nil {
		t.Fatalf("NewPeer: %v", err)
	}
	s.peerRef.Store(peer)

	s.recoverNetwork("test") // 不得 panic

	if s.peerRef.Load() != peer {
		t.Fatal("信令未连通时不得拆 PC，交给 WS 重连恢复")
	}
}

// TestRecoverNetworkCooldown 冷却窗内不重复拆链路（接口切换时签名可能连续变化）。
func TestRecoverNetworkCooldown(t *testing.T) {
	s, _ := newNetworkTestSession(t)
	s.roomID.Store("room1")
	peer, err := rtc.NewPeer(rtc.PeerConfig{StreamID: "u1"})
	if err != nil {
		t.Fatalf("NewPeer: %v", err)
	}
	s.peerRef.Store(peer)
	s.recoverNetwork("first")

	// 恢复流程已把 peerRef 置空；模拟 channel.joined 后新建了 PC，再次换网。
	peer2, err := rtc.NewPeer(rtc.PeerConfig{StreamID: "u1"})
	if err != nil {
		t.Fatalf("NewPeer: %v", err)
	}
	s.peerRef.Store(peer2)
	s.recoverNetwork("second")

	if s.peerRef.Load() != peer2 {
		t.Fatal("冷却窗内不应再次拆掉新建的 PC")
	}
}
