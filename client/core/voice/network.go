package voice

// 网络切换轻量恢复（#49 A）：轮询本机网络接口签名，Wi-Fi/有线/VPN 切换后
// 销毁旧 PeerConnection，用新 PC 重发 rtc.offer。服务端 sfu.Room.addPeer 对
// 同 uid 是顶替语义（#23），不需要真 ICE restart（SFU 在同一 PC 上接受
// re-offer 的能力见 #114），也不需要 channel.join（会回 already_in_channel）。

import (
	"context"
	"net"
	"sort"
	"strings"
	"time"

	"github.com/pion/webrtc/v4"
)

const (
	// networkPollInterval 接口签名采样周期。太短会无谓唤醒，太长会让
	// 换网后的恢复延迟可感；3s 采样 + 2s 去抖，最坏约两个采样周期（6s）
	// 完成重建。
	networkPollInterval = 3 * time.Second
	// networkDebounce 签名稳定期：接口在切换过程中会连续抖动，必须在
	// 新签名上稳定这么久才触发一次恢复。
	networkDebounce = 2 * time.Second
	// networkRecoverCooldown 两次恢复的最小间隔：切换过程中签名可能再次
	// 变化（VPN 阶段、DHCP 续租），冷却窗内不重复拆链路。
	networkRecoverCooldown = 15 * time.Second
)

// ifaceAddrs 一次接口枚举的纯数据快照。判定逻辑只吃这份快照，单测才能
// 用判定表覆盖真实网卡枚举不到的形态（VPN、虚拟交换机、隐私地址轮换）。
type ifaceAddrs struct {
	name  string
	up    bool
	loop  bool
	addrs []net.IP
}

// virtualIfacePrefixes 不参与出网的接口名前缀（已小写）。这些接口要么是
// 本机内部通路（Hyper-V 虚拟交换机、容器网桥、WSL），要么是隧道型 VPN——
// 它们的地址会随隧道重连、DHCP 租约和隐私地址轮换反复变化，纳进签名会让
// 「网络没有真的变」也触发一次 PC 重建（#117）。
//
// 物理网卡名（Ethernet / Wi-Fi / en0 / eth0 / wlan0 / enp3s0 / ppp0）不与
// 任何前缀相交；匹配是前缀式，因此 vEthernet (WSL) 这类带空格的名字也能
// 命中 "vethernet"。
var virtualIfacePrefixes = []string{
	"vethernet", "virbr", "veth", "docker", "br-", "cni", "flannel",
	"vmnet", "vboxnet", "lxc", "lxd", "hns", "wsl",
	"tun", "tap", "utun", "wg", "wireguard", "openvpn", "vpn",
	"tailscale", "zerotier", "hamachi", "clash", "mihomo",
	"isatap", "teredo", "6to4", "npcap",
}

// isVirtualIface 判断接口名是否属于上表。大小写不敏感。
func isVirtualIface(name string) bool {
	n := strings.ToLower(name)
	for _, p := range virtualIfacePrefixes {
		if strings.HasPrefix(n, p) {
			return true
		}
	}
	return false
}

// signatureAddr 把单个地址折算成签名分量。第二个返回值为 false 表示该地址
// 与连通性无关，丢弃。
//
// IPv6 只取 /64 前缀：RFC 4941 隐私地址与 SLAAC 会在同一前缀下轮换接口
// 标识符，按完整地址入签会让签名每隔几小时就变一次；前缀变化才是真正的
// 网络变化。
func signatureAddr(ip net.IP) (string, bool) {
	if ip == nil || ip.IsLoopback() || ip.IsUnspecified() ||
		ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() || ip.IsMulticast() {
		return "", false
	}
	if v4 := ip.To4(); v4 != nil {
		return v4.String(), true
	}
	v6 := ip.To16()
	if v6 == nil {
		return "", false
	}
	mask := net.CIDRMask(64, 128)
	return (&net.IPNet{IP: v6.Mask(mask), Mask: mask}).String(), true
}

// signatureFrom 由接口快照算签名：只保留 up、非回环、非虚拟接口上的有效
// 全局地址，按接口名与地址排序后拼成稳定字符串。没有任何可用接口时返回
// 空串——这是合法状态（离网），调用方照常按「签名变了」处理。
func signatureFrom(ifaces []ifaceAddrs) string {
	entries := make([]string, 0, len(ifaces))
	for _, ifi := range ifaces {
		if !ifi.up || ifi.loop || isVirtualIface(ifi.name) {
			continue
		}
		ips := make([]string, 0, len(ifi.addrs))
		seen := make(map[string]struct{}, len(ifi.addrs))
		for _, ip := range ifi.addrs {
			s, ok := signatureAddr(ip)
			if !ok {
				continue
			}
			if _, dup := seen[s]; dup {
				continue
			}
			seen[s] = struct{}{}
			ips = append(ips, s)
		}
		if len(ips) == 0 {
			continue
		}
		sort.Strings(ips)
		entries = append(entries, ifi.name+"="+strings.Join(ips, ","))
	}
	sort.Strings(entries)
	return strings.Join(entries, ";")
}

// networkSignature 采样真实接口列表并算签名。ok=false 表示枚举失败——本次
// 采样作废，不能把瞬时错误当成「网络变了」。
func networkSignature() (string, bool) {
	ifaces, err := net.Interfaces()
	if err != nil {
		return "", false
	}
	snap := make([]ifaceAddrs, 0, len(ifaces))
	for _, ifi := range ifaces {
		s := ifaceAddrs{
			name: ifi.Name,
			up:   ifi.Flags&net.FlagUp != 0,
			loop: ifi.Flags&net.FlagLoopback != 0,
		}
		addrs, err := ifi.Addrs()
		if err != nil {
			continue
		}
		for _, a := range addrs {
			switch v := a.(type) {
			case *net.IPNet:
				s.addrs = append(s.addrs, v.IP)
			case *net.IPAddr:
				s.addrs = append(s.addrs, v.IP)
			}
		}
		snap = append(snap, s)
	}
	return signatureFrom(snap), true
}

// networkWatcher 接口签名轮询器。时间源与采样函数可注入，便于单测。
type networkWatcher struct {
	interval time.Duration
	debounce time.Duration
	sample   func() (string, bool)
	inRoom   func() bool
	recover  func(reason string)

	base    string // 上次确认的签名
	baseSet bool
	pending string
	since   time.Time
}

// poll 采样一次并推进去抖状态。返回 true 表示本次完成一次确认并调用了
// recover。不在房间时照常更新基线但不触发——进房瞬间不该因为「入房前
// 换过网」就拆一次健康链路。
func (w *networkWatcher) poll(now time.Time) bool {
	sig, ok := w.sample()
	if !ok {
		return false
	}
	if !w.baseSet {
		w.base, w.baseSet = sig, true
		return false
	}
	if sig == w.base {
		w.pending = ""
		return false
	}
	if sig != w.pending {
		w.pending, w.since = sig, now
		return false
	}
	if now.Sub(w.since) < w.debounce {
		return false
	}
	w.base, w.pending = sig, ""
	if w.inRoom != nil && !w.inRoom() {
		return false
	}
	if w.recover != nil {
		w.recover("network_changed")
	}
	return true
}

func (w *networkWatcher) run(ctx context.Context) {
	t := time.NewTicker(w.interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-t.C:
			w.poll(now)
		}
	}
}

// watchNetwork 会话级网络监听：随 Connect 的 ctx 退出。
func (s *Session) watchNetwork(ctx context.Context) {
	w := &networkWatcher{
		interval: networkPollInterval,
		debounce: networkDebounce,
		sample:   networkSignature,
		inRoom:   s.InRoom,
		recover:  s.recoverNetwork,
	}
	w.run(ctx)
}

// recoverNetwork 网络接口变化后的轻量恢复：预取新凭证、销毁本地 PC，用新
// PC 直接重发 rtc.offer。服务端 handleRTCOffer → sfu.Room.addPeer 对同 uid
// 是顶替语义（#23），旧 peer 与其它成员指向它的下行路由会一起摘掉，本端
// 收到的 answer / 后续 re-offer 由新 PC 处理。
//
// 不走 channel.join：服务端在连接仍处于目标房时回 already_in_channel 且不
// 重建 peer，本端拆完旧 PC 后拿不到任何回执。真 ICE restart 需要 SFU 在同一
// PC 上接受 re-offer，见 #114。
//
// 幂等：同一时刻只允许一次重建（TryLock），冷却窗内不重复触发。offer 发送
// 失败只记日志——roomID 保留，WS 重连的 OnConnect 会重发 join 兜底。调用方
// 是网络轮询 goroutine，允许网络 I/O，但绝不阻塞 WS 读循环。
func (s *Session) recoverNetwork(reason string) {
	if s.voiceUnavailable || s.ws == nil || s.ws.Closed() || !s.ws.Connected() || s.remoteMgr == nil {
		return
	}
	if !s.InRoom() {
		return
	}
	if p := s.peerRef.Load(); p == nil || p.ConnectionState() == webrtc.PeerConnectionStateClosed {
		// 没有存活 PC：已有 join 在途或处于断线恢复，交给原有路径。
		return
	}
	if !s.roomRebuildMu.TryLock() {
		return
	}
	defer s.roomRebuildMu.Unlock()

	room, _ := s.roomID.Load().(string)
	if room == "" {
		return
	}
	now := time.Now()
	if last := s.lastNetRecover.Load(); last != 0 &&
		now.Sub(time.Unix(0, last)) < networkRecoverCooldown {
		s.log.Printf("session: 网络变化（%s）距上次重建不足 %s，忽略", reason, networkRecoverCooldown)
		return
	}
	// 先取新凭证再拆旧链路：REST 失败也只是降级空 ICE 列表，不应阻断重建。
	s.prefetchICE()
	// 锁内重读：预取期间可能已退房/换房。
	room, _ = s.roomID.Load().(string)
	if room == "" || s.peerRef.Load() == nil {
		return
	}
	s.lastNetRecover.Store(now.UnixNano())
	if p := s.peerRef.Swap(nil); p != nil {
		_ = p.Close()
	}
	s.remoteMgr.RemoveAll()
	// setupPeerOffer 会建新 PC、Store 到 peerRef 并 Offer（rtc.offer）。
	if err := s.setupPeerOffer(); err != nil {
		if p := s.peerRef.Swap(nil); p != nil {
			_ = p.Close()
		}
		s.log.Printf("session: 网络变化重建语音链路失败（%s）: %v（等 WS 重连兜底）", reason, err)
		return
	}
	s.log.Printf("session: 网络接口变化（%s），已用 rtc.offer 重建语音链路 %s（#49）", reason, room)
}
