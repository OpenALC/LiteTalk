package sfu

import (
	"fmt"
	"testing"

	"github.com/pion/interceptor"
	"github.com/pion/rtp"
	"github.com/pion/webrtc/v4"
)

// 这些基准只量静态路由表的扇出热段。写轨使用真实的 Pion WriteRTP 与 Bind，
// 同步接收汇消费每次写入；不含 TrackRemote.ReadRTP、SRTP、UDP 或调度开销。
// 真实连接、离房/重入和悄悄话语义仍由同包的集成测试验证。
type benchTrackWriter struct {
	calls uint64
	bytes uint64
}

func (w *benchTrackWriter) WriteRTP(h *rtp.Header, payload []byte) (int, error) {
	if h.Version != 2 || h.SSRC == 0 || h.PayloadType != 111 {
		panic("Pion 写轨未使用有效的绑定参数")
	}
	w.calls++
	w.bytes += uint64(len(payload))
	return len(payload), nil
}

func (*benchTrackWriter) Write([]byte) (int, error) {
	panic("基准应经 TrackLocalStaticRTP.WriteRTP")
}

type benchTrackContext struct {
	id     string
	ssrc   webrtc.SSRC
	writer *benchTrackWriter
}

func (c benchTrackContext) CodecParameters() []webrtc.RTPCodecParameters {
	return []webrtc.RTPCodecParameters{{
		RTPCodecCapability: webrtc.RTPCodecCapability{
			MimeType: webrtc.MimeTypeOpus, ClockRate: 48000, Channels: 2,
		},
		PayloadType: 111,
	}}
}
func (benchTrackContext) HeaderExtensions() []webrtc.RTPHeaderExtensionParameter { return nil }
func (c benchTrackContext) SSRC() webrtc.SSRC                                    { return c.ssrc }
func (benchTrackContext) SSRCRetransmission() webrtc.SSRC                        { return 0 }
func (benchTrackContext) SSRCForwardErrorCorrection() webrtc.SSRC                { return 0 }
func (c benchTrackContext) WriteStream() webrtc.TrackLocalWriter                 { return c.writer }
func (c benchTrackContext) ID() string                                           { return c.id }
func (benchTrackContext) RTCPReader() interceptor.RTCPReader                     { return nil }

type benchRoutes struct {
	room    *Room
	src     *Peer
	tracks  []*webrtc.TrackLocalStaticRTP
	writers []*benchTrackWriter
	pkt     *rtp.Packet
}

func newBenchRoutes(b *testing.B, receivers int) *benchRoutes {
	b.Helper()
	src := &Peer{userID: "src"}
	r := &Room{peers: map[string]*Peer{src.userID: src}}
	br := &benchRoutes{room: r, src: src}
	br.pkt = &rtp.Packet{
		Header:  rtp.Header{Version: 2, SequenceNumber: 1, Timestamp: 960},
		Payload: make([]byte, 80),
	}
	for i := range br.pkt.Payload {
		br.pkt.Payload[i] = byte(i)
	}
	for i := 0; i < receivers; i++ {
		uid := fmt.Sprintf("dst%d", i)
		track, err := webrtc.NewTrackLocalStaticRTP(
			webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeOpus, ClockRate: 48000, Channels: 2},
			"audio-src", "bench",
		)
		if err != nil {
			b.Fatal(err)
		}
		writer := &benchTrackWriter{}
		if _, err := track.Bind(benchTrackContext{
			id: uid, ssrc: webrtc.SSRC(1000 + i), writer: writer,
		}); err != nil {
			b.Fatal(err)
		}
		// 预热 Pion 的包池，同时证明接收汇确实收到经 Bind 的 WriteRTP。
		if err := track.WriteRTP(br.pkt); err != nil || writer.calls != 1 {
			b.Fatalf("写轨预热失败: calls=%d err=%v", writer.calls, err)
		}
		writer.calls, writer.bytes = 0, 0
		r.peers[uid] = &Peer{userID: uid, outbound: map[string]*outRoute{
			src.userID: {track: track, src: src},
		}}
		br.tracks = append(br.tracks, track)
		br.writers = append(br.writers, writer)
	}
	return br
}

var benchRouteMatches int

// BenchmarkSFURoutingMap 量静态 map 遍历、路由查找和源实例核对。
func BenchmarkSFURoutingMap(b *testing.B) {
	for _, receivers := range []int{1, 4, 9} {
		b.Run(fmt.Sprintf("receivers=%d", receivers), func(b *testing.B) {
			b.StopTimer()
			br := newBenchRoutes(b, receivers)
			matched := 0
			b.ResetTimer()
			b.ReportAllocs()
			b.StartTimer()
			for i := 0; i < b.N; i++ {
				for uid, p := range br.room.peers {
					if uid == br.src.userID {
						continue
					}
					route := p.outbound[br.src.userID]
					if route != nil && route.src == br.src {
						matched++
					}
				}
			}
			b.StopTimer()
			benchRouteMatches = matched
			if matched != b.N*receivers {
				b.Fatalf("路由命中 %d，期望 %d", matched, b.N*receivers)
			}
		})
	}
}

// BenchmarkSFURoutingRoomLock 在同一静态路由上加房间读锁。
func BenchmarkSFURoutingRoomLock(b *testing.B) {
	for _, receivers := range []int{1, 4, 9} {
		b.Run(fmt.Sprintf("receivers=%d", receivers), func(b *testing.B) {
			b.StopTimer()
			br := newBenchRoutes(b, receivers)
			matched := 0
			b.ResetTimer()
			b.ReportAllocs()
			b.StartTimer()
			for i := 0; i < b.N; i++ {
				br.room.mu.RLock()
				for uid, p := range br.room.peers {
					if uid == br.src.userID {
						continue
					}
					route := p.outbound[br.src.userID]
					if route != nil && route.src == br.src {
						matched++
					}
				}
				br.room.mu.RUnlock()
			}
			b.StopTimer()
			benchRouteMatches = matched
			if matched != b.N*receivers {
				b.Fatalf("路由命中 %d，期望 %d", matched, b.N*receivers)
			}
		})
	}
}

// BenchmarkSFURoutingRoomPeerLocks 对应当前扇出热段的两级锁与查找。
func BenchmarkSFURoutingRoomPeerLocks(b *testing.B) {
	for _, receivers := range []int{1, 4, 9} {
		b.Run(fmt.Sprintf("receivers=%d", receivers), func(b *testing.B) {
			b.StopTimer()
			br := newBenchRoutes(b, receivers)
			matched := 0
			b.ResetTimer()
			b.ReportAllocs()
			b.StartTimer()
			for i := 0; i < b.N; i++ {
				br.room.mu.RLock()
				for uid, p := range br.room.peers {
					if uid == br.src.userID {
						continue
					}
					p.mu.Lock()
					route := p.outbound[br.src.userID]
					p.mu.Unlock()
					if route != nil && route.src == br.src {
						matched++
					}
				}
				br.room.mu.RUnlock()
			}
			b.StopTimer()
			benchRouteMatches = matched
			if matched != b.N*receivers {
				b.Fatalf("路由命中 %d，期望 %d", matched, b.N*receivers)
			}
		})
	}
}

// BenchmarkSFUBoundPionWrite 只量已绑定的 Pion WriteRTP 与同步接收汇。
func BenchmarkSFUBoundPionWrite(b *testing.B) {
	for _, receivers := range []int{1, 4, 9} {
		b.Run(fmt.Sprintf("receivers=%d", receivers), func(b *testing.B) {
			b.StopTimer()
			br := newBenchRoutes(b, receivers)
			b.ResetTimer()
			b.ReportAllocs()
			b.SetBytes(int64(receivers * len(br.pkt.Payload)))
			b.StartTimer()
			for i := 0; i < b.N; i++ {
				for _, track := range br.tracks {
					if err := track.WriteRTP(br.pkt); err != nil {
						b.Fatal(err)
					}
				}
			}
			b.StopTimer()
			br.checkWrites(b, receivers)
		})
	}
}

// BenchmarkSFUFanoutKernel 合并当前房间路由热段和已绑定 Pion 写轨。
// 它没有 TrackRemote.ReadRTP 和网络收发，不能冒充完整 SFU CPU 或音质。
func BenchmarkSFUFanoutKernel(b *testing.B) {
	for _, receivers := range []int{1, 4, 9} {
		b.Run(fmt.Sprintf("receivers=%d", receivers), func(b *testing.B) {
			b.StopTimer()
			br := newBenchRoutes(b, receivers)
			b.ResetTimer()
			b.ReportAllocs()
			b.SetBytes(int64(receivers * len(br.pkt.Payload)))
			b.StartTimer()
			for i := 0; i < b.N; i++ {
				br.room.mu.RLock()
				for uid, p := range br.room.peers {
					if uid == br.src.userID {
						continue
					}
					p.mu.Lock()
					route := p.outbound[br.src.userID]
					p.mu.Unlock()
					if route == nil || route.src != br.src {
						continue
					}
					if err := route.track.WriteRTP(br.pkt); err != nil {
						br.room.mu.RUnlock()
						b.Fatal(err)
					}
				}
				br.room.mu.RUnlock()
			}
			b.StopTimer()
			br.checkWrites(b, receivers)
		})
	}
}

func (br *benchRoutes) checkWrites(b *testing.B, receivers int) {
	b.Helper()
	var calls, bytes uint64
	for _, writer := range br.writers {
		if writer.calls != uint64(b.N) || writer.bytes != uint64(b.N*len(br.pkt.Payload)) {
			b.Fatalf("接收汇次数/字节错误: calls=%d bytes=%d", writer.calls, writer.bytes)
		}
		calls += writer.calls
		bytes += writer.bytes
	}
	if calls != uint64(b.N*receivers) || bytes != uint64(b.N*receivers*len(br.pkt.Payload)) {
		b.Fatalf("扇出不完整: calls=%d bytes=%d", calls, bytes)
	}
}
