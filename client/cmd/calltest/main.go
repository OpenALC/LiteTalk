// calltest 是轻音 LiteTalk M2 的联调 harness：入房 → 建 PeerConnection →
// 上行（mic 或合成正弦经管线编码）→ 下行（jitter → 解码 → 混音 → 播放/排空）。
//
// 用法：
//
//	calltest --server ws://127.0.0.1:8077/ws --room test --name A [--tone 440] [--null] --duration 30
//
//	--server   信令地址（默认 ws://127.0.0.1:8077/ws）
//	--room     频道 ID（默认 test）
//	--name     成员名（token=dev-<name>，上行 streamID=<name>）
//	--tone N   合成 N Hz 正弦上行（隐含无声卡模式，便于无麦环境联调）
//	--null     无声卡模式：上行发静音（编码器 DTX），下行混音排空计数
//	--duration 运行秒数，0 = 直到 Ctrl+C（默认 30）
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"math"
	"os"
	ossignal "os/signal"
	"strings"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/OpenALC/LiteTalk/client/core/audio"
	"github.com/OpenALC/LiteTalk/client/core/rtc"
	"github.com/OpenALC/LiteTalk/client/core/signal"
	"github.com/pion/webrtc/v4"
)

func main() {
	var (
		server   = flag.String("server", "ws://127.0.0.1:8077/ws", "信令地址")
		room     = flag.String("room", "test", "频道 ID")
		name     = flag.String("name", "A", "成员名（token=dev-<name>）")
		tone     = flag.Float64("tone", 0, "合成正弦频率 Hz（>0 时隐含无声卡模式）")
		null     = flag.Bool("null", false, "无声卡模式")
		duration = flag.Int("duration", 30, "运行秒数，0 = 直到 Ctrl+C")
	)
	flag.Parse()
	if *name == "" {
		log.Fatal("--name 不能为空")
	}
	manual := *tone > 0 || *null

	logger := log.New(os.Stderr, "calltest: ", log.LstdFlags|log.Lmsgprefix)
	logger.Printf("start: server=%s room=%s name=%s tone=%.0f null=%v duration=%ds",
		*server, *room, *name, *tone, *null, *duration)

	// ---- 信令客户端（先建，管线 DSP 回调要经它发 voice.state）----
	// OnDisconnect 要拆媒体链路，peerRef/remoteMgr 由后文创建/声明；此处先
	// 声明、Run 启动前赋值（回调仅在 Run 后触发，无数据竞争）。
	var remoteMgr *audio.RemoteManager
	var peerRef atomic.Pointer[rtc.Peer]

	wsCfg := signal.Config{
		URL:       *server,
		TokenFunc: func(context.Context) (string, error) { return "dev-" + *name, nil },
		Logger:    logger,
	}
	// （重）连成功 → 重发入房订阅。
	wsCfg.OnConnect = func(c *signal.Client) {
		logger.Printf("connected, joining room %q", *room)
		if err := c.Send(signal.TypeChannelJoin, signal.ChannelJoinData{ChannelID: *room}); err != nil {
			logger.Printf("send channel.join: %v", err)
		}
	}
	// 信令断开 → 拆掉媒体链路；重连成功后 channel.joined 会触发重建。
	wsCfg.OnDisconnect = func(err error) {
		logger.Printf("signal disconnected: %v", err)
		if old := peerRef.Swap(nil); old != nil {
			_ = old.Close()
		}
		remoteMgr.RemoveAll()
	}
	ws := signal.NewClient(wsCfg)
	defer ws.Close()

	// 本地 speaking 变化 → voice.state。DSP 回调在音频处理 goroutine 上触发，
	// 必须非阻塞：经缓冲 channel 转手，由独立 goroutine 做网络发送。
	speakCh := make(chan bool, 1)
	var speakingState atomic.Bool
	go func() {
		for speaking := range speakCh {
			if speakingState.Swap(speaking) == speaking {
				continue
			}
			if err := ws.Send(signal.TypeVoiceState, signal.VoiceStateData{Speaking: signal.Bool(speaking)}); err != nil {
				logger.Printf("send voice.state: %v", err)
			}
			logger.Printf("local speaking=%v", speaking)
		}
	}()

	// ---- 上行：音频管线（编码出口 OnPacket → RTP）----
	var upDropped atomic.Uint64 // peer 未就绪丢弃的编码包

	pcfg := audio.DefaultPipelineConfig()
	pcfg.Loopback = false
	if manual {
		pcfg.Backend = audio.BackendNull // 不 Start，下方手工 10ms 驱动
	}
	pcfg.DSP.OnSpeakingChange = func(b bool) {
		select {
		case speakCh <- b:
		default:
		}
	}
	pcfg.OnPacket = func(pkt []byte) {
		if p := peerRef.Load(); p != nil {
			if err := p.SendOpus(pkt); err != nil {
				logger.Printf("send opus: %v", err)
			}
			return
		}
		upDropped.Add(1)
	}
	pipe, err := audio.NewPipeline(pcfg)
	if err != nil {
		logger.Fatalf("new pipeline: %v", err)
	}
	defer pipe.Close()

	// ---- 下行：混音器 → 管线播放环 ----
	mixer := audio.NewMixer(pipe.WritePlayback)
	remoteMgr = audio.NewRemoteManager(mixer, logger)
	states := audio.NewStateTable()
	states.OnChange = func(id string, st audio.PeerVoiceState) {
		logger.Printf("voice.state: %s speaking=%v mute=%v deafen=%v", id, st.Speaking, st.Mute, st.Deafen)
	}
	if err := mixer.Start(); err != nil {
		logger.Fatalf("mixer start: %v", err)
	}
	defer mixer.Stop()

	// ---- 信令 ----

	setupPeer := func() {
		if old := peerRef.Swap(nil); old != nil {
			_ = old.Close()
		}
		remoteMgr.RemoveAll()
		peer, err := rtc.NewPeer(rtc.PeerConfig{
			StreamID:        *name,
			IncludeLoopback: true, // 本机 SFU 宣告 127.0.0.1，必须收环回候选
			Callbacks: rtc.PeerCallbacks{
				OnLocalSDP: func(sdp webrtc.SessionDescription) {
					msgType := signal.TypeRTCOffer
					if sdp.Type == webrtc.SDPTypeAnswer {
						msgType = signal.TypeRTCAnswer
					}
					if err := ws.Send(msgType, signal.SDPData{SDP: sdp.SDP}); err != nil {
						logger.Printf("send %s: %v", msgType, err)
					} else {
						logger.Printf("sent %s (%d bytes)", msgType, len(sdp.SDP))
					}
				},
				OnLocalICE: func(c webrtc.ICECandidateInit) {
					if err := ws.Send(signal.TypeRTCIce, iceFromPion(c)); err != nil {
						logger.Printf("send rtc.ice: %v", err)
					}
				},
				OnTrack: remoteMgr.HandleTrack,
				OnConnectionStateChange: func(s webrtc.PeerConnectionState) {
					logger.Printf("peer connection: %s", s)
				},
			},
			Logger: logger,
		})
		if err != nil {
			logger.Printf("new peer: %v", err)
			return
		}
		peerRef.Store(peer)
		if err := peer.Offer(); err != nil {
			logger.Printf("offer: %v", err)
		}
	}

	ws.On(signal.TypeHello, func(env signal.Envelope) {
		logger.Printf("hello received (%d bytes)", len(env.Data))
	})
	ws.On(signal.TypeChannelJoined, func(env signal.Envelope) {
		var d signal.ChannelJoinedData
		if err := env.Decode(&d); err != nil {
			logger.Printf("decode channel.joined: %v", err)
			return
		}
		names := make([]string, 0, len(d.Members))
		for _, m := range d.Members {
			names = append(names, m.UserID)
			if m.UserID != d.Self.UserID {
				states.Set(m.UserID, audio.PeerVoiceState{Speaking: m.Speaking, Mute: m.Mute, Deafen: m.Deafen})
			}
		}
		logger.Printf("channel.joined: channel=%s self=%s members=[%s]",
			d.ChannelID, d.Self.UserID, strings.Join(names, ","))
		// 仅在尚无存活 peer 时建连（自己入房/重连后重建）；他人入房的
		// 广播只更新成员表，不动媒体链路（避免拆掉在跑的 PC）。
		if p := peerRef.Load(); p != nil &&
			p.ConnectionState() != webrtc.PeerConnectionStateClosed &&
			p.ConnectionState() != webrtc.PeerConnectionStateFailed {
			return
		}
		setupPeer()
	})
	ws.On(signal.TypeRTCOffer, func(env signal.Envelope) {
		var d signal.SDPData
		if err := env.Decode(&d); err != nil {
			logger.Printf("decode rtc.offer: %v", err)
			return
		}
		logger.Printf("recv rtc.offer (%d bytes)", len(d.SDP))
		if p := peerRef.Load(); p != nil {
			if err := p.HandleRemoteSDP(webrtc.SessionDescription{Type: webrtc.SDPTypeOffer, SDP: d.SDP}); err != nil {
				logger.Printf("handle rtc.offer: %v", err)
			}
		}
	})
	ws.On(signal.TypeRTCAnswer, func(env signal.Envelope) {
		var d signal.SDPData
		if err := env.Decode(&d); err != nil {
			logger.Printf("decode rtc.answer: %v", err)
			return
		}
		logger.Printf("recv rtc.answer (%d bytes)", len(d.SDP))
		if p := peerRef.Load(); p != nil {
			if err := p.HandleRemoteSDP(webrtc.SessionDescription{Type: webrtc.SDPTypeAnswer, SDP: d.SDP}); err != nil {
				logger.Printf("handle rtc.answer: %v", err)
			}
		}
	})
	ws.On(signal.TypeRTCIce, func(env signal.Envelope) {
		var d signal.ICEData
		if err := env.Decode(&d); err != nil {
			logger.Printf("decode rtc.ice: %v", err)
			return
		}
		if p := peerRef.Load(); p != nil {
			if err := p.HandleRemoteICE(iceToPion(d)); err != nil {
				logger.Printf("handle rtc.ice: %v", err)
			}
		}
	})
	ws.On(signal.TypeVoiceState, func(env signal.Envelope) {
		var d signal.VoiceStateData
		if err := env.Decode(&d); err != nil {
			logger.Printf("decode voice.state: %v", err)
			return
		}
		if d.UserID == "" {
			return
		}
		states.Apply(d.UserID, d.Speaking, d.Mute, d.Deafen)
	})
	ws.On(signal.TypeChannelPeerLeft, func(env signal.Envelope) {
		var d signal.PeerLeftData
		if err := env.Decode(&d); err != nil {
			logger.Printf("decode channel.peer_left: %v", err)
			return
		}
		logger.Printf("peer left: %s", d.UserID)
		remoteMgr.RemovePeer(d.UserID)
		states.Remove(d.UserID)
	})
	ws.On(signal.TypeError, func(env signal.Envelope) {
		var d signal.ErrorData
		if err := env.Decode(&d); err != nil {
			logger.Printf("server error (undecodable): %s", string(env.Data))
			return
		}
		logger.Printf("server error %s: %s", d.Code, d.Message)
	})

	sigCtx, stop := ossignal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go func() {
		if err := ws.Run(sigCtx); err != nil {
			logger.Printf("signal run: %v", err)
		}
		stop() // 信令终结（如鉴权失败）→ 整体退出
	}()

	// ---- 手工驱动（--tone/--null）：10ms 节拍注入 PCM + 排空播放环 ----
	var drainedSamples atomic.Uint64
	if manual {
		go func() {
			tick := time.NewTicker(10 * time.Millisecond)
			defer tick.Stop()
			frame := make([]int16, audio.FrameSize)
			drain := make([]int16, audio.OpusFrameSize)
			var phase float64
			for {
				select {
				case <-sigCtx.Done():
					return
				case <-tick.C:
				}
				if *tone > 0 {
					for i := range frame {
						phase += 2 * math.Pi * *tone / audio.SampleRate
						frame[i] = int16(12000 * math.Sin(phase))
					}
				} else {
					for i := range frame {
						frame[i] = 0
					}
				}
				if err := pipe.ProcessPCM(frame); err != nil {
					logger.Printf("process pcm: %v", err)
					return
				}
				drainedSamples.Add(uint64(pipe.ReadPlayback(drain)))
			}
		}()
	} else {
		if err := pipe.Start(); err != nil {
			logger.Fatalf("pipeline start: %v", err)
		}
		logger.Printf("pipeline started (device backend)")
	}

	// ---- 周期统计 ----
	statsTick := time.NewTicker(2 * time.Second)
	defer statsTick.Stop()
	var timer <-chan time.Time
	if *duration > 0 {
		t := time.NewTimer(time.Duration(*duration) * time.Second)
		defer t.Stop()
		timer = t.C
	}
	for run := true; run; {
		select {
		case <-sigCtx.Done():
			run = false
		case <-timer:
			run = false
		case <-statsTick.C:
			printStats(logger, pipe, peerRef.Load(), mixer, remoteMgr, drainedSamples.Load())
		}
	}

	logger.Printf("shutting down")
	_ = ws.Send(signal.TypeChannelLeave, signal.ChannelJoinData{ChannelID: *room})
	time.Sleep(100 * time.Millisecond) // 给 leave 一点发出时间
	if p := peerRef.Load(); p != nil {
		_ = p.Close()
	}
	remoteMgr.RemoveAll()
	ws.Close()
	printStats(logger, pipe, nil, mixer, remoteMgr, drainedSamples.Load())
	logger.Printf("bye")
}

// printStats 打印一轮运行统计。
func printStats(logger *log.Logger, pipe *audio.Pipeline, peer *rtc.Peer, mixer *audio.Mixer, remoteMgr *audio.RemoteManager, drained uint64) {
	ps := pipe.Stats()
	var b strings.Builder
	fmt.Fprintf(&b, "stats: enc=%d(avg %.0fB) err=%d | play: samples=%d underrun=%d drop=%d buf=%dms",
		ps.PacketsEncoded, ps.AvgPacketBytes, ps.EncodeErrors+ps.DecodeErrors,
		ps.PlaybackSamples, ps.PlaybackUnderruns, ps.PlaybackDropped,
		pipe.PlaybackBuffered().Milliseconds())
	if peer != nil {
		fmt.Fprintf(&b, " | up: rtp=%d(%dB) dtx=%d muted-drop=%d state=%s",
			peer.Stats.PacketsSent.Load(), peer.Stats.BytesSent.Load(),
			peer.Stats.SentDTX.Load(), peer.Stats.MutedDropped.Load(), peer.ConnectionState())
	}
	fmt.Fprintf(&b, " | mix: frames=%d clip=%d", mixer.FramesMixed.Load(), mixer.ClippedSamples.Load())
	if drained > 0 {
		fmt.Fprintf(&b, " drained=%d", drained)
	}
	for _, id := range remoteMgr.PeerIDs() {
		ls, ok := remoteMgr.LinkStats(id)
		if !ok {
			continue
		}
		fmt.Fprintf(&b, " | %s: rtp=%d lost=%d plc=%d fec=%d jit=%.1fms tgt=%d jbuf=%d ring=%d pcm=%d underrun=%d drop=%d",
			id, ls.RTPReceived, ls.Jitter.Lost, ls.PLCFrames, ls.FECFrames,
			ls.Jitter.JitterMs, ls.Jitter.TargetPackets, ls.Jitter.Buffered,
			ls.RingAvailable, ls.WrittenSamples, ls.UnderrunFrames, ls.DroppedSamples)
	}
	logger.Print(b.String())
}

// iceFromPion pion → 信令线格式。
func iceFromPion(c webrtc.ICECandidateInit) signal.ICEData {
	d := signal.ICEData{Candidate: c.Candidate, SDPMLineIndex: c.SDPMLineIndex}
	if c.SDPMid != nil {
		d.SDPMid = *c.SDPMid
	}
	if c.UsernameFragment != nil {
		d.UsernameFragment = *c.UsernameFragment
	}
	return d
}

// iceToPion 信令线格式 → pion。
func iceToPion(d signal.ICEData) webrtc.ICECandidateInit {
	c := webrtc.ICECandidateInit{Candidate: d.Candidate, SDPMLineIndex: d.SDPMLineIndex}
	if d.SDPMid != "" {
		c.SDPMid = &d.SDPMid
	}
	if d.UsernameFragment != "" {
		c.UsernameFragment = &d.UsernameFragment
	}
	return c
}
