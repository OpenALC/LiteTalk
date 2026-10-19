package rtc

import (
	"testing"
	"time"

	"github.com/pion/webrtc/v4"
)

// connectLoopback 建一条已连通的 loopback Peer（复use peer_test 的 fakeSFU），
// 供基准与电平扩展测试用。ICE 协商耗时不算进基准。
// quiet=true 时 fakeSFU 不读上行：ReadRTP 的 make+Unmarshal 是进程级分配，
// 会污染发送侧的 allocs/op 计数（testing 按整个进程的 MemStats 统计）。
func connectLoopback(t testing.TB, levelFn func() uint8, quiet bool) (*Peer, *fakeSFU) {
	t.Helper()
	sfu := newFakeSFU(t)
	sfu.mu.Lock()
	sfu.skipRead = quiet
	sfu.mu.Unlock()
	localSDP := make(chan webrtc.SessionDescription, 4)
	localICE := make(chan webrtc.ICECandidateInit, 64)
	peer, err := NewPeer(PeerConfig{
		StreamID: "bench",
		LevelFn:  levelFn,
		Callbacks: PeerCallbacks{
			OnLocalSDP: func(sdp webrtc.SessionDescription) { localSDP <- sdp },
			OnLocalICE: func(c webrtc.ICECandidateInit) { localICE <- c },
		},
	})
	if err != nil {
		t.Fatalf("NewPeer: %v", err)
	}
	t.Cleanup(func() { _ = peer.Close() })
	sfu.pc.OnICECandidate(func(c *webrtc.ICECandidate) {
		if c == nil {
			return
		}
		_ = peer.HandleRemoteICE(c.ToJSON())
	})
	go func() {
		for c := range localICE {
			_ = sfu.pc.AddICECandidate(c)
		}
	}()
	if err := peer.Offer(); err != nil {
		t.Fatalf("Offer: %v", err)
	}
	offer := <-localSDP
	if err := sfu.pc.SetRemoteDescription(offer); err != nil {
		t.Fatalf("set remote offer: %v", err)
	}
	answer, err := sfu.pc.CreateAnswer(nil)
	if err != nil {
		t.Fatalf("create answer: %v", err)
	}
	if err := sfu.pc.SetLocalDescription(answer); err != nil {
		t.Fatalf("set local answer: %v", err)
	}
	if err := peer.HandleRemoteSDP(answer); err != nil {
		t.Fatalf("HandleRemoteSDP: %v", err)
	}
	waitConnected(t, peer, sfu)
	return peer, sfu
}

// BenchmarkSendOpus SendOpus 热路径本体（#33 零分配硬验收）：打包 + RFC 6464
// 电平改写 + TrackLocalStaticRTP.WriteRTP 封装（track 未绑定 ⇒ 绑定环空转，
// 不进 pion 网络写链）。本包代码每包分配必须为 0：电平只改写预分配字节，
// 电平源是 atomic 读。改动前（无扩展路径）同样为 0。
func BenchmarkSendOpus(b *testing.B) {
	peer, err := NewPeer(PeerConfig{StreamID: "bench", LevelFn: func() uint8 { return 27 }})
	if err != nil {
		b.Fatal(err)
	}
	defer peer.Close()
	peer.attachLevelExt()
	if !peer.levelAttached {
		b.Fatal("level ext not attached")
	}
	payload := make([]byte, 80)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = peer.SendOpus(payload)
	}
}

// BenchmarkSendOpusWrite 全链路发送（quiet 接收端）：SendOpus + pion 网络写
// （SRTP/拦截器/marshal）。ext 挂 RFC 6464、noext 为改动前线上形态（对照）。
//
// 已知：ext 比 noext 多约 1 allocs/op，来源是 pion stats 拦截器
// QueueOutgoingRTP 的 header.Clone() 深拷贝 Extensions（pion-internal，
// interceptor v0.1.49），非本包代码——浏览器/libwebrtc 带扩展同样付这笔。
func BenchmarkSendOpusWrite(b *testing.B) {
	b.Run("ext", func(b *testing.B) { benchSendOpusWrite(b, func() uint8 { return 27 }) })
	b.Run("noext", func(b *testing.B) { benchSendOpusWrite(b, nil) })
}

func benchSendOpusWrite(b *testing.B, levelFn func() uint8) {
	b.Helper()
	peer, _ := connectLoopback(b, levelFn, true)
	payload := make([]byte, 80)
	for i := range payload {
		payload[i] = byte(i)
	}
	time.Sleep(20 * time.Millisecond)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if err := peer.SendOpus(payload); err != nil {
			b.Fatalf("SendOpus: %v", err)
		}
	}
}
