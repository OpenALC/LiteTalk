package audio

import (
	"errors"
	"runtime"
	"testing"
	"time"

	"github.com/OpenALC/LiteTalk/client/core/codec"
	"github.com/OpenALC/LiteTalk/client/core/hotkey"
)

// requireOpus 编解码测试前置：darwin 尚未接入 libopus（stub），是唯一允许
// 跳过的平台；Linux/Windows 必须已接入，Supported() 为假直接判失败——
// 不准再用动态 Skip 掩盖平台接入回归。
func requireOpus(tb testing.TB) {
	tb.Helper()
	if runtime.GOOS == "darwin" {
		tb.Skip("darwin 尚未接入 libopus（stub）")
	}
	if !codec.Supported() {
		tb.Fatalf("codec.Supported() = false on %s：Linux/Windows 必须接入 libopus", runtime.GOOS)
	}
}

func newLoopbackPipeline(t *testing.T, mutate func(*PipelineConfig)) *Pipeline {
	t.Helper()
	requireOpus(t)
	cfg := DefaultPipelineConfig()
	cfg.Backend = BackendNull // 不 Start，仅同步注入，用不到后端
	// APM 是 Beta 功能默认关闭；既有链路测试锁定旧 DSP 行为，APM 专项
	// 由调用方 mutate 显式 EnableAPM=true。
	if mutate != nil {
		mutate(&cfg)
	}
	p, err := NewPipeline(cfg)
	if err != nil {
		t.Fatalf("NewPipeline: %v", err)
	}
	t.Cleanup(func() { p.Close() })
	return p
}

// TestLoopbackChain 全链路算法验证（不依赖声卡）：
// 合成正弦 → DSP（旁路，见 dsp_test 说明）→ Opus 编码 → 本地解码 → 播放环，
// 信号必须存活、账目必须对上。
func TestLoopbackChain(t *testing.T) {
	p := newLoopbackPipeline(t, func(c *PipelineConfig) { c.DSP.Denoise = false })

	const frames10ms = 200 // 2s 音频 = 100 个 Opus 包
	in := make([]int16, FrameSize)
	var phase float64
	var rng uint32 = 0xABCD

	// 边注入边排空播放环，收集全部解码输出。
	decoded := make([]int16, 0, frames10ms*FrameSize)
	chunk := make([]int16, OpusFrameSize)
	var inRMSAcc float64

	for f := 0; f < frames10ms; f++ {
		sineNoiseFrame(in, &phase, 12000, 2000, &rng)
		inRMSAcc += rmsInt16(in)
		if err := p.ProcessPCM(in); err != nil {
			t.Fatalf("ProcessPCM frame %d: %v", f, err)
		}
		for {
			n := p.ReadPlayback(chunk)
			if n == 0 {
				break
			}
			decoded = append(decoded, chunk[:n]...)
		}
	}

	st := p.Stats()
	if st.FramesProcessed != frames10ms {
		t.Errorf("FramesProcessed = %d, want %d", st.FramesProcessed, frames10ms)
	}
	if st.PacketsEncoded != frames10ms/2 {
		t.Errorf("PacketsEncoded = %d, want %d", st.PacketsEncoded, frames10ms/2)
	}
	if st.PacketsDecoded != frames10ms/2 {
		t.Errorf("PacketsDecoded = %d, want %d", st.PacketsDecoded, frames10ms/2)
	}
	if st.EncodeErrors != 0 || st.DecodeErrors != 0 {
		t.Errorf("codec errors: enc=%d dec=%d", st.EncodeErrors, st.DecodeErrors)
	}
	if st.PlaybackDropped != 0 {
		t.Errorf("PlaybackDropped = %d, want 0", st.PlaybackDropped)
	}
	if st.AvgPacketBytes <= 0 || st.AvgPacketBytes > 1000 {
		t.Errorf("AvgPacketBytes = %.1f, out of sane range", st.AvgPacketBytes)
	}

	// 解码输出长度 = 输入长度，能量保留（Opus+RNNoise 处理后允许衰减）。
	if len(decoded) != frames10ms*FrameSize {
		t.Fatalf("decoded %d samples, want %d", len(decoded), frames10ms*FrameSize)
	}
	inRMS := inRMSAcc / frames10ms
	outRMS := rmsInt16(decoded)
	t.Logf("chain: in RMS %.0f -> decoded RMS %.0f, avg packet %.1f bytes",
		inRMS, outRMS, st.AvgPacketBytes)
	if outRMS < inRMS*0.3 {
		t.Errorf("decoded signal too weak: %.0f < 30%% of %.0f", outRMS, inRMS)
	}
	if st.LatencyCount == 0 {
		t.Error("latency hook never recorded")
	}
	t.Logf("latency estimate: avg %dµs max %dµs", st.LatencyAvgUS, st.LatencyMaxUS)
}

// TestPipelineSpeaking 管线级说话状态回调（旁路模式，RMS 门限）：
// 响亮信号段翻转 ON，静音段翻转 OFF。
func TestPipelineSpeaking(t *testing.T) {
	var states []bool
	p := newLoopbackPipeline(t, func(c *PipelineConfig) {
		c.DSP.Denoise = false
		c.DSP.OnSpeakingChange = func(b bool) { states = append(states, b) }
	})

	in := make([]int16, FrameSize)
	var phase float64
	var rng uint32 = 0x55AA

	for f := 0; f < 100; f++ { // 1s 语音
		sineNoiseFrame(in, &phase, 12000, 2000, &rng)
		if err := p.ProcessPCM(in); err != nil {
			t.Fatal(err)
		}
	}
	if !p.Speaking() {
		t.Fatal("not speaking after 1s of loud signal")
	}
	for f := 0; f < 300; f++ { // 3s 静音
		for i := range in {
			in[i] = 0
		}
		if err := p.ProcessPCM(in); err != nil {
			t.Fatal(err)
		}
	}
	if p.Speaking() {
		t.Fatal("still speaking after 3s of silence")
	}
	if len(states) < 2 {
		t.Fatalf("speaking transitions = %v, want at least [true false]", states)
	}
	if states[0] != true || states[len(states)-1] != false {
		t.Fatalf("unexpected transition sequence: %v", states)
	}
	if st := p.Stats(); st.SpeakingChanges < 2 {
		t.Fatalf("SpeakingChanges = %d, want >= 2", st.SpeakingChanges)
	}
}

// TestPacketHook 非回环模式：OnPacket 收到全部包，且可用独立解码器解码（M2 出口验证）。
func TestPacketHook(t *testing.T) {
	var packets [][]byte
	p := newLoopbackPipeline(t, func(c *PipelineConfig) {
		c.Loopback = false
		c.OnPacket = func(pkt []byte) {
			packets = append(packets, append([]byte(nil), pkt...))
		}
	})

	const frames10ms = 40 // 20 个包
	in := make([]int16, FrameSize)
	var phase float64
	var rng uint32 = 0x99
	for f := 0; f < frames10ms; f++ {
		sineNoiseFrame(in, &phase, 12000, 2000, &rng)
		if err := p.ProcessPCM(in); err != nil {
			t.Fatal(err)
		}
	}

	if len(packets) != frames10ms/2 {
		t.Fatalf("OnPacket got %d packets, want %d", len(packets), frames10ms/2)
	}
	for i, pkt := range packets {
		if len(pkt) == 0 || len(pkt) > 4000 {
			t.Fatalf("packet %d length %d out of range", i, len(pkt))
		}
	}

	// 模拟对端：独立解码器逐包解码，长度必须还原为 960。
	dec, err := codec.NewDecoder(SampleRate, 1)
	if err != nil {
		t.Fatal(err)
	}
	defer dec.Close()
	out := make([]int16, OpusFrameSize)
	for i, pkt := range packets {
		m, err := dec.Decode(pkt, out, false)
		if err != nil {
			t.Fatalf("peer decode packet %d: %v", i, err)
		}
		if m != OpusFrameSize {
			t.Fatalf("peer decode packet %d: %d samples, want %d", i, m, OpusFrameSize)
		}
	}
	if st := p.Stats(); st.PacketsDecoded != 0 {
		t.Fatalf("non-loopback PacketsDecoded = %d, want 0", st.PacketsDecoded)
	}
}

// ---------- 按键说话发送门（v1.2.0） ----------

// gatePipeline 构造带发包门的非回环管线（不起后端，ProcessPCM 同步注入），
// 与 TestPacketHook 同款；返回管线与已设模式的 Gate。
func gatePipeline(t *testing.T, mode hotkey.Mode, onPacket func(), onSpeak func(bool)) (*Pipeline, *hotkey.Gate) {
	t.Helper()
	p := newLoopbackPipeline(t, func(c *PipelineConfig) {
		c.Loopback = false
		c.DSP.Denoise = false // 省掉 RNNoise；说话判定走 RMS 支路（vadAvailable=false）
		if onPacket != nil {
			c.OnPacket = func([]byte) { onPacket() }
		}
		if onSpeak != nil {
			c.DSP.OnSpeakingChange = onSpeak
		}
	})
	g := &hotkey.Gate{}
	g.SetMode(mode)
	p.SetSendGate(g)
	return p, g
}

// feedLoud / feedSilent 注入 n 个 10ms 帧。loud 的 RMS 远高于自适应门限
// （初始底噪 -50dB → 进入门限 -38dB），EnterFrames=3 帧内必翻说话态。
func feedLoud(t *testing.T, p *Pipeline, in []int16, phase *float64, rng *uint32, frames int) {
	t.Helper()
	for i := 0; i < frames; i++ {
		sineNoiseFrame(in, phase, 12000, 2000, rng)
		if err := p.ProcessPCM(in); err != nil {
			t.Fatalf("ProcessPCM: %v", err)
		}
	}
}

func feedSilent(t *testing.T, p *Pipeline, in []int16, frames int) {
	t.Helper()
	clear(in)
	for i := 0; i < frames; i++ {
		if err := p.ProcessPCM(in); err != nil {
			t.Fatalf("ProcessPCM: %v", err)
		}
	}
}

// TestSendGatePTT 按住说话：未按住全拦（PTTDropped 计数）、按住放行、
// 尾包 0 松手即拦、尾包窗内继续放行且窗过恢复全拦。
func TestSendGatePTT(t *testing.T) {
	var packets int
	p, gate := gatePipeline(t, hotkey.ModePTT, func() { packets++ }, nil)
	in := make([]int16, FrameSize)
	var phase float64
	var rng uint32 = 0x5A5A

	feedLoud(t, p, in, &phase, &rng, 20) // 100ms = 10 个包
	if packets != 0 {
		t.Fatalf("未按住却发出 %d 个包", packets)
	}
	if d := p.Stats().PTTDropped; d != 10 {
		t.Fatalf("PTTDropped = %d, want 10", d)
	}

	gate.Press()
	feedLoud(t, p, in, &phase, &rng, 20)
	if packets != 10 {
		t.Fatalf("按住后只发出 %d 个包, want 10", packets)
	}
	if d := p.Stats().PTTDropped; d != 10 {
		t.Fatalf("按住期间 PTTDropped 不应增长, got %d", d)
	}

	// 尾包 0：松手立即全拦（不补尾音）。
	gate.Release(0)
	feedLoud(t, p, in, &phase, &rng, 20)
	if packets != 10 {
		t.Fatalf("松手（无尾包）后又发出 %d 个包", packets-10)
	}
	if d := p.Stats().PTTDropped; d != 20 {
		t.Fatalf("PTTDropped = %d, want 20", d)
	}

	// 尾包窗：松手后 250ms 内继续放行，窗过恢复全拦。
	gate.Press()
	feedLoud(t, p, in, &phase, &rng, 4) // 2 个包（此时说话态早已立起）
	before := packets
	gate.Release(250 * time.Millisecond)
	feedLoud(t, p, in, &phase, &rng, 4) // 窗内：再 2 个包
	if packets-before != 2 {
		t.Fatalf("尾包窗内包数 = %d, want 2", packets-before)
	}
	if d := p.Stats().PTTDropped; d != 20 {
		t.Fatalf("尾包窗内 PTTDropped 不应增长, got %d", d)
	}
	time.Sleep(300 * time.Millisecond) // 窗（250ms）必已过期
	feedLoud(t, p, in, &phase, &rng, 20)
	if packets != before+2 {
		t.Fatalf("尾包窗过后又发出 %d 个包", packets-before-2)
	}
	if d := p.Stats().PTTDropped; d != 30 {
		t.Fatalf("PTTDropped = %d, want 30", d)
	}
}

// TestSendGateVoice 语音激活：门控来源是 DSP VAD——静音帧全拦、语音帧放行。
// 这是 ModeVoice 与 ModeOpen 的唯一区别（常开不看 speaking，见下一例）。
func TestSendGateVoice(t *testing.T) {
	var packets int
	p, _ := gatePipeline(t, hotkey.ModeVoice, func() { packets++ }, nil)
	in := make([]int16, FrameSize)

	feedSilent(t, p, in, 20) // VAD 不起：全拦
	if packets != 0 {
		t.Fatalf("静音期却发出 %d 个包", packets)
	}
	if d := p.Stats().PTTDropped; d != 10 {
		t.Fatalf("PTTDropped = %d, want 10", d)
	}

	var phase float64
	var rng uint32 = 0x77
	feedLoud(t, p, in, &phase, &rng, 20) // EnterFrames=3 迟滞后放行
	if packets == 0 {
		t.Fatal("语音期一个包都没发出")
	}
	if packets >= 10 {
		t.Fatalf("语音期发出 %d/10 个包，EnterFrames 迟滞似乎失效", packets)
	}
	// 丢弃数 = 静音期 10 个 + 语音期进门限迟滞丢掉的那几个。
	if d := p.Stats().PTTDropped; d != uint64(10+(10-packets)) {
		t.Fatalf("PTTDropped = %d, want %d", d, 10+(10-packets))
	}
}

// TestSendGateOpen 常开：恒放行，静音帧也照发（与 ModeVoice 正相反）。
func TestSendGateOpen(t *testing.T) {
	var packets int
	p, _ := gatePipeline(t, hotkey.ModeOpen, func() { packets++ }, nil)
	in := make([]int16, FrameSize)
	feedSilent(t, p, in, 20)
	if packets != 10 {
		t.Fatalf("常开模式静音期包数 = %d, want 10", packets)
	}
	if d := p.Stats().PTTDropped; d != 0 {
		t.Fatalf("常开模式 PTTDropped = %d, want 0", d)
	}
}

// TestSendGateNil 未设门 = 恒放行：门是可选装配，nil 不改变既有行为。
func TestSendGateNil(t *testing.T) {
	var packets int
	p := newLoopbackPipeline(t, func(c *PipelineConfig) {
		c.Loopback = false
		c.OnPacket = func([]byte) { packets++ }
	})
	in := make([]int16, FrameSize)
	feedSilent(t, p, in, 20)
	if packets != 10 {
		t.Fatalf("无门时包数 = %d, want 10", packets)
	}
	if d := p.Stats().PTTDropped; d != 0 {
		t.Fatalf("无门时 PTTDropped = %d, want 0", d)
	}
}

// TestSendGateSuppressesSpeaking 门关时压掉说话态：包已被发包门拦下，
// UI 声波球与远端说话状态不能再亮（否则"包没发却在说话"）。
// DSP 自身状态不受影响（Speaking() 仍为真），门开后重新上报。
func TestSendGateSuppressesSpeaking(t *testing.T) {
	var states []bool
	p, gate := gatePipeline(t, hotkey.ModePTT, nil, func(b bool) { states = append(states, b) })
	in := make([]int16, FrameSize)
	var phase float64
	var rng uint32 = 0x33

	feedLoud(t, p, in, &phase, &rng, 100) // 1s 大声：DSP 必判说话
	if !p.Speaking() {
		t.Fatal("DSP 应已判说话（门只压上报，不改 DSP 状态）")
	}
	for _, s := range states {
		if s {
			t.Fatalf("门关时不应上报说话: %v", states)
		}
	}

	feedSilent(t, p, in, 300) // 3s 静音：DSP 退出说话态
	if p.Speaking() {
		t.Fatal("3s 静音后 DSP 应退出说话态")
	}

	gate.Press()
	feedLoud(t, p, in, &phase, &rng, 100) // 按住 + 大声：正常上报 true
	sawTrue := false
	for _, s := range states {
		sawTrue = sawTrue || s
	}
	if !sawTrue {
		t.Fatalf("按住后应上报说话: %v", states)
	}
}

// BenchmarkProcessPCMWithSendGate 带发包门的同步注入基准：门加在编码出口，
// 每帧只多一次原子指针 Load + Gate 内 atomic 读，必须零分配（热路径红线）。
func BenchmarkProcessPCMWithSendGate(b *testing.B) {
	requireOpus(b)
	cfg := DefaultPipelineConfig()
	cfg.Backend = BackendNull
	cfg.Loopback = false
	cfg.DSP.Denoise = false
	// APM 默认关闭；基准目标是发包门开销，保持默认旧链路。
	p, err := NewPipeline(cfg)
	if err != nil {
		b.Fatalf("NewPipeline: %v", err)
	}
	defer p.Close()
	gate := &hotkey.Gate{}
	gate.SetMode(hotkey.ModePTT)
	gate.Press() // 按住：放行，走完整发包路径
	p.SetSendGate(gate)

	in := make([]int16, FrameSize)
	var phase float64
	var rng uint32 = 0xABCD
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		sineNoiseFrame(in, &phase, 12000, 2000, &rng)
		if err := p.ProcessPCM(in); err != nil {
			b.Fatal(err)
		}
	}
}

// TestPipelineAPMRenderWiring 管线暴露的 AEC 参考入口：APM 启用时 ProcessRender
// 与 ProcessPCM 可并行接线运行，且不改变编码/播放账目。
func TestPipelineAPMRenderWiring(t *testing.T) {
	requireAPM(t)
	p := newLoopbackPipeline(t, func(c *PipelineConfig) { c.DSP.EnableAPM = true })
	if !p.APMActive() {
		t.Fatal("Pipeline 应启用 APM")
	}
	ren := make([]int16, FrameSize)
	in := make([]int16, FrameSize)
	var phase float64
	var rng uint32 = 0x43AE
	for f := 0; f < 40; f++ {
		aecRenderFrame(ren, f)
		p.ProcessRender(ren) // 混音器 tick 的实际接线
		sineNoiseFrame(in, &phase, 6000, 500, &rng)
		if err := p.ProcessPCM(in); err != nil {
			t.Fatalf("ProcessPCM frame %d: %v", f, err)
		}
	}
	if st := p.Stats(); st.FramesProcessed != 40 || st.PacketsEncoded != 20 {
		t.Fatalf("统计异常: frames=%d packets=%d", st.FramesProcessed, st.PacketsEncoded)
	}
}

// TestPipelineAPMHotSwitch 管线默认关闭 APM；Beta 开关运行期创建/销毁句柄，
// 关闭后 ProcessPCM 完全走旧链路。
func TestPipelineAPMHotSwitch(t *testing.T) {
	requireAPM(t)
	p := newLoopbackPipeline(t, nil)
	if p.APMActive() {
		t.Fatal("APM 默认应关闭")
	}
	ren := make([]int16, FrameSize)
	in := make([]int16, FrameSize)
	var phase float64
	var rng uint32 = 0x43AF
	feed := func(frames int) {
		t.Helper()
		for f := 0; f < frames; f++ {
			aecRenderFrame(ren, f)
			p.ProcessRender(ren)
			sineNoiseFrame(in, &phase, 6000, 500, &rng)
			if err := p.ProcessPCM(in); err != nil {
				t.Fatalf("ProcessPCM: %v", err)
			}
		}
	}
	feed(10)
	if err := p.SetEnableAPM(true); err != nil {
		t.Fatalf("SetEnableAPM(true): %v", err)
	}
	if !p.APMActive() {
		t.Fatal("开启后 APM 应生效")
	}
	feed(40)
	if err := p.SetEnableAPM(false); err != nil {
		t.Fatalf("SetEnableAPM(false): %v", err)
	}
	if p.APMActive() {
		t.Fatal("关闭后 APM 应停用")
	}
	feed(10)
	if err := p.SetEnableAPM(true); err != nil {
		t.Fatalf("重新开启: %v", err)
	}
	if err := p.SetEnableAPM(true); err != nil { // 幂等
		t.Fatalf("重复开启: %v", err)
	}
	if err := p.SetEnableAPM(false); err != nil {
		t.Fatalf("最终关闭: %v", err)
	}
}

// TestProcessPCMBadLength 非 480 整数倍的注入必须报错。
func TestProcessPCMBadLength(t *testing.T) {
	p := newLoopbackPipeline(t, nil)
	if err := p.ProcessPCM(make([]int16, 500)); err == nil {
		t.Fatal("ProcessPCM(500) should fail")
	}
	if err := p.ProcessPCM(nil); err != nil {
		t.Fatalf("ProcessPCM(nil): %v", err)
	}
}

// TestNullBackendRun 真实启动 null 后端跑 500ms，验证 Start/Stop 生命周期与账目。
func TestNullBackendRun(t *testing.T) {
	p := newLoopbackPipeline(t, func(c *PipelineConfig) { c.Backend = BackendNull })
	if err := p.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if err := p.Start(); err != nil {
		t.Fatalf("second Start should be a no-op, got: %v", err)
	}
	waitFor := func(cond func() bool, msg string) {
		t.Helper()
		deadline := 0
		for !cond() {
			if deadline++; deadline > 2000 { // ~2s
				t.Fatalf("timeout waiting for: %s", msg)
			}
			time.Sleep(time.Millisecond)
		}
	}
	// encoded 与 decoded 由不同 goroutine 推进（解码滞后编码）：必须等两者都
	// 到 10 再读快照，否则 Stop 时刻 decoded 可能还是 9（CI -race 负载下必现）。
	waitFor(func() bool {
		st := p.Stats()
		return st.PacketsEncoded >= 10 && st.PacketsDecoded >= 10
	}, "10 encoded+decoded packets")
	st := p.Stats()
	p.Stop()

	if st.FramesProcessed < 20 {
		t.Errorf("FramesProcessed = %d, suspiciously low for 500ms run", st.FramesProcessed)
	}
	if st.PacketsEncoded == 0 || st.PacketsDecoded != st.PacketsEncoded {
		t.Errorf("packets: encoded=%d decoded=%d", st.PacketsEncoded, st.PacketsDecoded)
	}
	if st.PlaybackSamples == 0 {
		t.Error("null sink drained 0 samples")
	}
	if st.CaptureDropped != 0 || st.PlaybackUnderruns != 0 {
		t.Errorf("drops: capture=%d underruns=%d", st.CaptureDropped, st.PlaybackUnderruns)
	}
	t.Logf("null backend 500ms: frames=%d pkts=%d played=%d samples, latency avg %dµs",
		st.FramesProcessed, st.PacketsEncoded, st.PlaybackSamples, st.LatencyAvgUS)
}

// TestStartStopIdempotent Start/Stop 重复调用无害：重复 Start 是 no-op、
// 重复 Stop 不 panic，且 Stop 后可再次 Start 完整重启（退出频道再进的使用方式）。
func TestStartStopIdempotent(t *testing.T) {
	p := newLoopbackPipeline(t, func(c *PipelineConfig) { c.Backend = BackendNull })

	if err := p.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if err := p.Start(); err != nil {
		t.Fatalf("重复 Start 应为 no-op: %v", err)
	}
	p.Stop()
	p.Stop() // 重复 Stop 不应 panic，也不应死锁

	if err := p.Start(); err != nil {
		t.Fatalf("Stop 后重启: %v", err)
	}
	before := p.Stats().PacketsEncoded
	deadline := 0
	for p.Stats().PacketsEncoded <= before {
		if deadline++; deadline > 2000 {
			t.Fatalf("重启后管线无进展: before=%d after=%d", before, p.Stats().PacketsEncoded)
		}
		time.Sleep(time.Millisecond)
	}
	p.Stop()
}

// TestOpusSettersSmoke Opus 参数热调：合法值生效并可继续编码，非法值报错。
func TestOpusSettersSmoke(t *testing.T) {
	p := newLoopbackPipeline(t, func(c *PipelineConfig) { c.DSP.Denoise = false })
	if err := p.SetOpusBitrate(64); err != nil {
		t.Fatalf("SetOpusBitrate: %v", err)
	}
	if err := p.SetOpusVBR(false); err != nil {
		t.Fatalf("SetOpusVBR: %v", err)
	}
	if err := p.SetOpusDTX(false); err != nil {
		t.Fatalf("SetOpusDTX: %v", err)
	}
	if err := p.SetOpusFEC(false); err != nil {
		t.Fatalf("SetOpusFEC: %v", err)
	}
	if err := p.SetOpusPacketLoss(20); err != nil {
		t.Fatalf("SetOpusPacketLoss: %v", err)
	}
	if p.opus.BitrateKbps != 64 || *p.opus.VBR || *p.opus.DTX || *p.opus.FEC || p.opus.PacketLoss != 20 {
		t.Fatalf("opus 配置未同步: %+v", p.opus)
	}
	if err := p.SetOpusBitrate(0); err == nil {
		t.Fatal("SetOpusBitrate(0) 应报错")
	}
	if err := p.SetOpusPacketLoss(101); err == nil {
		t.Fatal("SetOpusPacketLoss(101) 应报错")
	}

	// 热调后管线仍可编码（2 个 10ms 帧 = 1 个 20ms Opus 包）。
	in := make([]int16, FrameSize)
	var phase float64
	var rng uint32 = 7
	for i := 0; i < 2; i++ {
		sineNoiseFrame(in, &phase, 8000, 1000, &rng)
		if err := p.ProcessPCM(in); err != nil {
			t.Fatalf("ProcessPCM after setters: %v", err)
		}
	}
	if p.Stats().PacketsEncoded == 0 {
		t.Fatal("热调后未编码出包")
	}
}

// TestOpusSetterAfterClose 关闭后所有热调 setter 返回 ErrClosed（钉住 Close 置 nil 行为）。
func TestOpusSetterAfterClose(t *testing.T) {
	p := newLoopbackPipeline(t, func(c *PipelineConfig) { c.DSP.Denoise = false })
	if err := p.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if err := p.SetOpusBitrate(64); !errors.Is(err, codec.ErrClosed) {
		t.Fatalf("SetOpusBitrate after Close: %v", err)
	}
	if err := p.SetOpusVBR(false); !errors.Is(err, codec.ErrClosed) {
		t.Fatalf("SetOpusVBR after Close: %v", err)
	}
	if err := p.SetOpusDTX(false); !errors.Is(err, codec.ErrClosed) {
		t.Fatalf("SetOpusDTX after Close: %v", err)
	}
	if err := p.SetOpusFEC(false); !errors.Is(err, codec.ErrClosed) {
		t.Fatalf("SetOpusFEC after Close: %v", err)
	}
	if err := p.SetOpusPacketLoss(20); !errors.Is(err, codec.ErrClosed) {
		t.Fatalf("SetOpusPacketLoss after Close: %v", err)
	}
}

// TestOpusConfigResolve 零值字段回落默认。
func TestOpusConfigResolve(t *testing.T) {
	r := ResolveOpusConfig(OpusConfig{})
	if r.BitrateKbps != 32 || !*r.VBR || !*r.DTX || !*r.FEC || r.PacketLoss != 10 {
		t.Fatalf("ResolveOpusConfig 零值回落异常: %+v", r)
	}
	custom := OpusConfig{BitrateKbps: 48, PacketLoss: 15}
	r = ResolveOpusConfig(custom)
	if r.BitrateKbps != 48 || r.PacketLoss != 15 || !*r.VBR {
		t.Fatalf("ResolveOpusConfig 应保留显式值: %+v", r)
	}
}

// TestFrameMS10 10ms 帧长（#46）：每个 10ms 帧单独编码，包数 = 帧数，
// 解码返回 480 采样；切回 20ms 恢复两帧一包。
func TestFrameMS10(t *testing.T) {
	var packets [][]byte
	p := newLoopbackPipeline(t, func(c *PipelineConfig) {
		c.Loopback = false
		c.DSP.Denoise = false
		c.OnPacket = func(pkt []byte) {
			packets = append(packets, append([]byte(nil), pkt...))
		}
	})
	if got := p.FrameMS(); got != FrameMS20 {
		t.Fatalf("默认帧长 = %d, want %d", got, FrameMS20)
	}
	if err := p.SetFrameMS(FrameMS10); err != nil {
		t.Fatalf("SetFrameMS(10): %v", err)
	}
	if err := p.SetFrameMS(15); err == nil {
		t.Fatal("非法帧长应报错")
	}

	in := make([]int16, FrameSize)
	var phase float64
	var rng uint32 = 0x77
	for f := 0; f < 4; f++ {
		sineNoiseFrame(in, &phase, 12000, 2000, &rng)
		if err := p.ProcessPCM(in); err != nil {
			t.Fatal(err)
		}
	}
	if len(packets) != 4 {
		t.Fatalf("10ms 模式包数 = %d, want 4", len(packets))
	}
	dec, err := codec.NewDecoder(SampleRate, 1)
	if err != nil {
		t.Fatal(err)
	}
	defer dec.Close()
	out := make([]int16, OpusFrameSize)
	for i, pkt := range packets {
		m, err := dec.Decode(pkt, out, false)
		if err != nil {
			t.Fatalf("decode packet %d: %v", i, err)
		}
		if m != FrameSize {
			t.Fatalf("10ms 包解码 %d 采样, want %d", m, FrameSize)
		}
	}

	// 切回 20ms：再喂 4 个 10ms 帧应得到 2 个包。
	if err := p.SetFrameMS(FrameMS20); err != nil {
		t.Fatalf("SetFrameMS(20): %v", err)
	}
	before := len(packets)
	for f := 0; f < 4; f++ {
		sineNoiseFrame(in, &phase, 12000, 2000, &rng)
		if err := p.ProcessPCM(in); err != nil {
			t.Fatal(err)
		}
	}
	if got := len(packets) - before; got != 2 {
		t.Fatalf("切回 20ms 后包数 = %d, want 2", got)
	}
}
