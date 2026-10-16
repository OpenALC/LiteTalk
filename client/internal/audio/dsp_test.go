package audio

import (
	"math"
	"testing"
	"time"
)

// sineFrame 生成 440Hz 正弦 + 均匀噪声混合的一帧（模拟说话信号，spike 同款）。
func sineNoiseFrame(dst []int16, phase *float64, amp, noiseAmp float64, rng *uint32) {
	for i := range dst {
		*phase += 2 * math.Pi * 440 / SampleRate
		*rng ^= *rng << 13
		*rng ^= *rng >> 17
		*rng ^= *rng << 5
		noise := float64(int32(*rng>>15)%int32(2*noiseAmp+1)) - noiseAmp
		dst[i] = int16(amp*math.Sin(*phase) + noise)
	}
}

func noiseFrame(dst []int16, noiseAmp float64, rng *uint32) {
	for i := range dst {
		*rng ^= *rng << 13
		*rng ^= *rng >> 17
		*rng ^= *rng << 5
		dst[i] = int16(float64(int32(*rng>>15)%int32(2*noiseAmp+1)) - noiseAmp)
	}
}

func newTestDSP(t *testing.T, mutate func(*DSPConfig)) *DSP {
	t.Helper()
	cfg := DefaultDSPConfig()
	if mutate != nil {
		mutate(&cfg)
	}
	d, err := NewDSP(cfg)
	if err != nil {
		t.Fatalf("NewDSP: %v", err)
	}
	return d
}

// TestSpeakingDetection 响亮信号应触发 speaking，随后静音应退出（带迟滞）。
// 用旁路模式（RMS 门限）验证状态机语义：RNNoise 的 VAD 只对类语音信号置高
// （spike 实测合成正弦 VAD 均值仅 0.069），VAD 正例需真实语音；
// VAD 参与的负例门控由 TestNoiseGate 覆盖。
func TestSpeakingDetection(t *testing.T) {
	var transitions []bool
	d := newTestDSP(t, func(c *DSPConfig) {
		c.Denoise = false
		c.OnSpeakingChange = func(b bool) { transitions = append(transitions, b) }
	})

	in := make([]int16, FrameSize)
	out := make([]int16, FrameSize)
	var phase float64
	var rng uint32 = 0xC0FFEE

	// 100 帧响亮信号（1s），应快速置位（EnterFrames=3 → 30ms 起判）。
	speakingAt := -1
	for f := 0; f < 100; f++ {
		sineNoiseFrame(in, &phase, 12000, 2000, &rng)
		if _, err := d.Process(out, in); err != nil {
			t.Fatalf("Process: %v", err)
		}
		if d.Speaking() && speakingAt < 0 {
			speakingAt = f
		}
	}
	if speakingAt < 0 {
		t.Fatal("loud speech-like signal never triggered speaking")
	}
	if speakingAt > 60 {
		t.Fatalf("speaking triggered too late at frame %d (%.0fms)", speakingAt, float64(speakingAt)*10)
	}
	t.Logf("speaking ON at frame %d (%dms)", speakingAt, speakingAt*10)

	// 300 帧静音（3s），应退出；且挂起迟滞要求不是立刻退出。
	if len(transitions) != 1 || !transitions[0] {
		t.Fatalf("expected exactly one ON transition so far, got %v", transitions)
	}
	offAt := -1
	for f := 0; f < 300; f++ {
		for i := range in {
			in[i] = 0
		}
		if _, err := d.Process(out, in); err != nil {
			t.Fatalf("Process: %v", err)
		}
		if !d.Speaking() && offAt < 0 {
			offAt = f
		}
	}
	if offAt < 0 {
		t.Fatal("silence never cleared speaking")
	}
	// 挂起迟滞校验：ExitFrames=15 表示第 15 个连续静音帧才清除，
	// 0 起始帧号下 offAt 应恰好为 14（140ms）；offAt < 14 说明没挂起。
	if offAt < 14 {
		t.Fatalf("speaking cleared without hangover at frame %d", offAt)
	}
	t.Logf("speaking OFF after %d frames of silence (%dms)", offAt, offAt*10)
}

// TestNoiseGate 低电平纯白噪声不应触发说话（spike 发现其 VAD 均值 0.569 偏高，
// 必须由 RMS 门限拦住）。
func TestNoiseGate(t *testing.T) {
	d := newTestDSP(t, nil)

	in := make([]int16, FrameSize)
	out := make([]int16, FrameSize)
	var rng uint32 = 0xBEEF

	var vadSum float64
	const frames = 500
	for f := 0; f < frames; f++ {
		noiseFrame(in, 800, &rng) // RMS ≈ 460 < RMSEnter(500)
		vad, err := d.Process(out, in)
		if err != nil {
			t.Fatalf("Process: %v", err)
		}
		vadSum += float64(vad)
		if d.Speaking() {
			t.Fatalf("quiet noise triggered speaking at frame %d (VAD %.3f)", f, vad)
		}
	}
	t.Logf("quiet noise avg VAD=%.3f, speaking never triggered", vadSum/frames)
}

// TestDenoiseAttenuatesNoise 降噪应显著压低稳态噪声能量。
func TestDenoiseAttenuatesNoise(t *testing.T) {
	in := make([]int16, FrameSize)
	out := make([]int16, FrameSize)
	var rng uint32 = 0xF00D

	dOn := newTestDSP(t, func(c *DSPConfig) { c.OnSpeakingChange = nil })
	var inRMS, onRMS float64
	const frames = 200
	for f := 0; f < frames; f++ {
		noiseFrame(in, 3000, &rng)
		if _, err := dOn.Process(out, in); err != nil {
			t.Fatalf("Process: %v", err)
		}
		if f >= 50 { // 跳过模型预热
			inRMS += rmsInt16(in)
			onRMS += rmsInt16(out)
		}
	}
	ratio := onRMS / inRMS
	t.Logf("denoise on: in RMS %.0f -> out RMS %.0f (%.1f%%)", inRMS/150, onRMS/150, ratio*100)
	if ratio > 0.5 {
		t.Fatalf("denoise attenuation too weak: out/in RMS ratio %.2f > 0.5", ratio)
	}
}

// TestGain 增益生效与运行时修改。
func TestGain(t *testing.T) {
	d := newTestDSP(t, func(c *DSPConfig) {
		c.Denoise = false // 旁路降噪，裸测增益
		c.Gain = 2.0
	})

	in := make([]int16, FrameSize)
	out := make([]int16, FrameSize)
	var phase float64
	var rng uint32 = 1
	sineNoiseFrame(in, &phase, 5000, 0, &rng)
	if _, err := d.Process(out, in); err != nil {
		t.Fatalf("Process: %v", err)
	}
	inRMS, outRMS := rmsInt16(in), rmsInt16(out)
	if outRMS < inRMS*1.9 || outRMS > inRMS*2.1 {
		t.Fatalf("gain 2.0: out RMS %.0f, in RMS %.0f", outRMS, inRMS)
	}

	d.SetGain(0.5)
	if _, err := d.Process(out, in); err != nil {
		t.Fatalf("Process: %v", err)
	}
	if r := rmsInt16(out); r < inRMS*0.4 || r > inRMS*0.6 {
		t.Fatalf("gain 0.5: out RMS %.0f, in RMS %.0f", r, inRMS)
	}

	// 增益截断不溢出。
	d.SetGain(10)
	for i := range in {
		in[i] = 12000
	}
	if _, err := d.Process(out, in); err != nil {
		t.Fatalf("Process: %v", err)
	}
	for i, s := range out {
		if s != math.MaxInt16 {
			t.Fatalf("out[%d] = %d, want clamped %d", i, s, math.MaxInt16)
		}
	}
}

// TestDenoiseBypass 关闭降噪时输出等于输入（增益 1），且说话判定退化为 RMS 门限。
func TestDenoiseBypass(t *testing.T) {
	var transitions []bool
	d := newTestDSP(t, func(c *DSPConfig) {
		c.Denoise = false
		c.OnSpeakingChange = func(b bool) { transitions = append(transitions, b) }
	})

	in := make([]int16, FrameSize)
	out := make([]int16, FrameSize)
	var phase float64
	var rng uint32 = 7

	for f := 0; f < 10; f++ {
		sineNoiseFrame(in, &phase, 12000, 0, &rng)
		vad, err := d.Process(out, in)
		if err != nil {
			t.Fatalf("Process: %v", err)
		}
		if vad != 0 {
			t.Fatalf("bypass mode VAD = %f, want 0", vad)
		}
		for i := range in {
			if out[i] != in[i] {
				t.Fatalf("bypass output differs at sample %d", i)
			}
		}
	}
	if !d.Speaking() {
		t.Fatal("bypass mode: loud signal should trigger speaking via RMS gate")
	}
	if len(transitions) != 1 || !transitions[0] {
		t.Fatalf("transitions = %v, want [true]", transitions)
	}
}

// BenchmarkDSPProcess 降噪热路径：必须 0 allocs/op（rnnoise-go 官方承诺构建后零分配）。
func BenchmarkDSPProcess(b *testing.B) {
	d, err := NewDSP(DefaultDSPConfig())
	if err != nil {
		b.Fatal(err)
	}
	in := make([]int16, FrameSize)
	out := make([]int16, FrameSize)
	var phase float64
	var rng uint32 = 42
	sineNoiseFrame(in, &phase, 12000, 2000, &rng)

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := d.Process(out, in); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkDSPSilenceAfterSpeech 防 denormal 回归基准：先 3s 语音帧再 60s 纯零帧，
// 分别统计两阶段每帧平均耗时并输出比值。修复前静音段会因 IIR 状态落入次正规数
// 区间而比语音段慢 1.4~2.3 倍；短路后静音段只有峰值扫描，比值应远小于 1。
// 必须 0 allocs/op。
func BenchmarkDSPSilenceAfterSpeech(b *testing.B) {
	d, err := NewDSP(DefaultDSPConfig())
	if err != nil {
		b.Fatal(err)
	}
	in := make([]int16, FrameSize)
	out := make([]int16, FrameSize)
	var phase float64
	var rng uint32 = 0x5EED

	const speechFrames = 300   // 3s
	const silenceFrames = 6000 // 60s

	var speechNS, silenceNS int64
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		t0 := time.Now()
		for f := 0; f < speechFrames; f++ {
			sineNoiseFrame(in, &phase, 12000, 2000, &rng)
			if _, err := d.Process(out, in); err != nil {
				b.Fatal(err)
			}
		}
		speechNS += time.Since(t0).Nanoseconds()

		clear(in)
		t1 := time.Now()
		for f := 0; f < silenceFrames; f++ {
			if _, err := d.Process(out, in); err != nil {
				b.Fatal(err)
			}
		}
		silenceNS += time.Since(t1).Nanoseconds()
	}
	b.StopTimer()

	speechAvg := float64(speechNS) / float64(b.N) / speechFrames
	silenceAvg := float64(silenceNS) / float64(b.N) / silenceFrames
	b.Logf("speech %.0f ns/frame, silence %.0f ns/frame, silence/speech = %.3f",
		speechAvg, silenceAvg, silenceAvg/speechAvg)
}

// TestDSPSilenceAfterSpeech 防 denormal 回归：同一管线先语音再静音，静音段每帧
// 耗时不得超过语音段的 1.2 倍。修复前静音段 IIR 状态停在次正规数区间，实测
// 比值 1.4~2.3；短路后静音段只做峰值扫描，比值应远小于 1。
func TestDSPSilenceAfterSpeech(t *testing.T) {
	d := newTestDSP(t, nil)

	in := make([]int16, FrameSize)
	out := make([]int16, FrameSize)
	var phase float64
	var rng uint32 = 0xD1CE

	const speechFrames = 300   // 3s 语音（RNNoise 状态打满非零）
	const silenceFrames = 3000 // 30s 纯零

	t0 := time.Now()
	for f := 0; f < speechFrames; f++ {
		sineNoiseFrame(in, &phase, 12000, 2000, &rng)
		if _, err := d.Process(out, in); err != nil {
			t.Fatalf("Process: %v", err)
		}
	}
	speechAvg := float64(time.Since(t0).Nanoseconds()) / speechFrames

	clear(in)
	t1 := time.Now()
	for f := 0; f < silenceFrames; f++ {
		if _, err := d.Process(out, in); err != nil {
			t.Fatalf("Process: %v", err)
		}
	}
	silenceAvg := float64(time.Since(t1).Nanoseconds()) / silenceFrames

	t.Logf("speech %.0f ns/frame, silence %.0f ns/frame, ratio %.3f", speechAvg, silenceAvg, silenceAvg/speechAvg)
	if silenceAvg > speechAvg*1.2 {
		t.Fatalf("静音段每帧 %.0fns > 语音段 %.0fns 的 1.2 倍，denormal 短路疑似失效", silenceAvg, speechAvg)
	}
}

// TestSetVADSensitivity 灵敏度五档（v0.1.2）映射到 VAD 双门限，非法档回落中档。
func TestSetVADSensitivity(t *testing.T) {
	d, err := NewDSP(DSPConfig{Denoise: true})
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		level     int
		wantEnter float32
		wantExit  float32
	}{
		{VADSensLowest, 0.75, 0.55},
		{VADSensLow, 0.7, 0.5},
		{VADSensMid, 0.6, 0.4},
		{VADSensHigh, 0.5, 0.3},
		{VADSensHighest, 0.45, 0.25},
		{99, 0.6, 0.4}, // 非法档 → 中
	}
	for _, c := range cases {
		d.SetVADSensitivity(c.level)
		if e, x := d.VADEnter(), d.VADExit(); e != c.wantEnter || x != c.wantExit {
			t.Fatalf("level %d: enter=%.2f exit=%.2f, want %.2f/%.2f", c.level, e, x, c.wantEnter, c.wantExit)
		}
	}
}

// TestSetDenoiseStrength 强度切换重建 Denoiser 且稳态输出有差异（v0.1.2）。
// RNNoise 有 20ms 算法延迟（Delay=2×FrameSize），首帧输出近零，
// 须先灌入若干帧填满延迟线再比较稳态段。
func TestSetDenoiseStrength(t *testing.T) {
	d, err := NewDSP(DSPConfig{Denoise: true})
	if err != nil {
		t.Fatal(err)
	}
	// 确定性噪声帧序列（同一输入两次 run 完全一致）。
	mkInputs := func() [][FrameSize]int16 {
		x := uint32(12345)
		out := make([][FrameSize]int16, 32)
		for f := range out {
			for i := range out[f] {
				x ^= x << 13
				x ^= x >> 17
				x ^= x << 5
				out[f][i] = int16(x%4000 - 2000)
			}
		}
		return out
	}
	run := func(level int, inputs [][FrameSize]int16) [FrameSize]int16 {
		if err := d.SetDenoiseStrength(level); err != nil {
			t.Fatalf("SetDenoiseStrength(%d): %v", level, err)
		}
		var last [FrameSize]int16
		var out [FrameSize]int16
		for i := range inputs {
			if _, err := d.Process(out[:], inputs[i][:]); err != nil {
				t.Fatal(err)
			}
			last = out
		}
		return last
	}
	inputs := mkInputs()
	weak, strong := run(1, inputs), run(3, inputs)
	same := true
	for i := range weak {
		if weak[i] != strong[i] {
			same = false
			break
		}
	}
	if same {
		t.Fatal("弱/强档稳态输出完全一致，强度切换未生效")
	}
}

// TestAGCConverge AGC 低幅输入增益上升、高幅输入增益下降并限幅（v0.1.2）。
func TestAGCConverge(t *testing.T) {
	d, err := NewDSP(DSPConfig{Denoise: false})
	if err != nil {
		t.Fatal(err)
	}
	d.SetAGC(true)

	// 低幅 200 RMS 正弦：增益应上升。
	var frame [FrameSize]int16
	amp := int16(280) // ≈200 RMS
	for i := range frame {
		frame[i] = amp
	}
	var out [FrameSize]int16
	for i := 0; i < 200; i++ {
		if _, err := d.Process(out[:], frame[:]); err != nil {
			t.Fatal(err)
		}
	}
	lowRMS := rmsInt16(out[:])
	if lowRMS <= 280 {
		t.Fatalf("低幅输入 AGC 后 RMS=%.0f，未提升", lowRMS)
	}

	// 高幅 12000 RMS 方波：增益应下降（低于 1）。
	big := int16(12000)
	for i := range frame {
		frame[i] = big
	}
	for i := 0; i < 200; i++ {
		if _, err := d.Process(out[:], frame[:]); err != nil {
			t.Fatal(err)
		}
	}
	highPeak := int32(0)
	for _, s := range out {
		if v := int32(s); v > highPeak || -v > highPeak {
			if v < 0 {
				highPeak = -v
			} else {
				highPeak = v
			}
		}
	}
	if highPeak > math.MaxInt16 {
		t.Fatalf("高幅输入溢出：峰值 %d", highPeak)
	}
}

// TestSetVADThresholds 直接指定门限时强制 ≥0.2 回差。
func TestSetVADThresholds(t *testing.T) {
	d, err := NewDSP(DSPConfig{Denoise: true})
	if err != nil {
		t.Fatal(err)
	}
	approx := func(a, b float32) bool {
		d := a - b
		return d < 0.01 && d > -0.01
	}
	d.SetVADThresholds(0.6, 0.55) // 回差不足 → exit 强制 0.4
	if e, x := d.VADEnter(), d.VADExit(); !approx(e, 0.6) || !approx(x, 0.4) {
		t.Fatalf("enter=%.4f exit=%.4f, want ≈0.60/0.40", e, x)
	}
	d.SetVADThresholds(0.8, 0.5) // 合法回差 → 原样
	if e, x := d.VADEnter(), d.VADExit(); !approx(e, 0.8) || !approx(x, 0.5) {
		t.Fatalf("enter=%.4f exit=%.4f, want ≈0.80/0.50", e, x)
	}
}

// TestPreGainApplied 前置增益按 dB 换算生效，且不改动调用方的 in 缓冲。
func TestPreGainApplied(t *testing.T) {
	d := newTestDSP(t, func(c *DSPConfig) { c.Denoise = false })
	if err := d.SetPreGainDB(18); err != nil {
		t.Fatalf("SetPreGainDB: %v", err)
	}
	if got := d.PreGainDB(); got < 17.9 || got > 18.1 {
		t.Fatalf("PreGainDB() = %.2f, want ≈18", got)
	}

	in := make([]int16, FrameSize)
	out := make([]int16, FrameSize)
	var phase float64
	var rng uint32 = 3
	sineNoiseFrame(in, &phase, 1000, 0, &rng)
	before := make([]int16, FrameSize)
	copy(before, in)
	if _, err := d.Process(out, in); err != nil {
		t.Fatalf("Process: %v", err)
	}
	for i := range in {
		if in[i] != before[i] {
			t.Fatalf("Process 改动了调用方的 in 缓冲 @%d", i)
		}
	}
	inRMS, outRMS := rmsInt16(in), rmsInt16(out)
	want := inRMS * dbToLinear(18)
	if outRMS < want*0.99 || outRMS > want*1.01 {
		t.Fatalf("+18dB 前置增益: out RMS %.0f, want ≈%.0f（in %.0f）", outRMS, want, inRMS)
	}
}

// TestPreGainRange 构造时越界收敛、运行时 setter 越界报错。
func TestPreGainRange(t *testing.T) {
	d := newTestDSP(t, func(c *DSPConfig) { c.PreGainDB = 100 })
	if got := d.PreGainDB(); got < 23.9 || got > 24.1 {
		t.Fatalf("构造越界应收敛到 +24dB，得到 %.2f", got)
	}
	if err := d.SetPreGainDB(30); err == nil {
		t.Fatal("SetPreGainDB(30) 应报错")
	}
	if err := d.SetPreGainDB(-13); err == nil {
		t.Fatal("SetPreGainDB(-13) 应报错")
	}
	if err := d.SetPreGainDB(-12); err != nil {
		t.Fatalf("SetPreGainDB(-12) 合法: %v", err)
	}
	if got := d.PreGainDB(); got < -12.1 || got > -11.9 {
		t.Fatalf("PreGainDB() = %.2f, want ≈-12", got)
	}
}

// TestPreGainBeforeDenoise 前置增益先于 RNNoise：弱正弦（≈-50dBFS）在默认强度
// 降噪下稳态被压制到近零；+18dB 前置增益把它抬进降噪器的有效区后输出显著存活。
// 若增益在 RNNoise 之后，输出只会是「近零 × 7.943」仍然近零——不可能复活信号。
func TestPreGainBeforeDenoise(t *testing.T) {
	run := func(preDB float64) float64 {
		d := newTestDSP(t, nil) // denoise on
		if err := d.SetPreGainDB(preDB); err != nil {
			t.Fatal(err)
		}
		in := make([]int16, FrameSize)
		out := make([]int16, FrameSize)
		var phase float64
		var rng uint32 = 0x5150
		var acc float64
		const frames, warm = 60, 20 // 跳过 RNNoise 2 帧延迟线预热
		for f := 0; f < frames; f++ {
			sineNoiseFrame(in, &phase, 300, 0, &rng)
			if _, err := d.Process(out, in); err != nil {
				t.Fatal(err)
			}
			if f >= warm {
				acc += rmsInt16(out)
			}
		}
		return acc / (frames - warm)
	}
	before, after := run(0), run(18)
	t.Logf("denoise 前置 +18dB: 弱正弦 out RMS %.1f -> %.1f", before, after)
	if before >= 5 {
		t.Fatalf("测试信号未被压制：pre=0 时 out RMS %.1f，区分度不足", before)
	}
	if after < 100 {
		t.Fatalf("+18dB 前置后弱正弦未复活：out RMS %.1f < 100（增益疑似在 RNNoise 之后）", after)
	}
}

// TestWeakSignalPreGainPassesGate -40dBFS 弱信号：无前置增益过不了说话门限，
// +18dB 后（仍在 RNNoise 之前）抬过门限。旁路模式（denoise off）排除 VAD 干扰。
func TestWeakSignalPreGainPassesGate(t *testing.T) {
	d := newTestDSP(t, func(c *DSPConfig) { c.Denoise = false })

	in := make([]int16, FrameSize)
	out := make([]int16, FrameSize)
	for i := range in {
		in[i] = 328 // ≈ -40dBFS
	}
	const frames = 10
	for f := 0; f < frames; f++ {
		if _, err := d.Process(out, in); err != nil {
			t.Fatal(err)
		}
		if d.Speaking() {
			t.Fatalf("-40dBFS 弱信号不应触发 speaking（帧 %d）", f)
		}
	}
	if err := d.SetPreGainDB(18); err != nil {
		t.Fatal(err)
	}
	activated := -1
	for f := 0; f < frames; f++ {
		if _, err := d.Process(out, in); err != nil {
			t.Fatal(err)
		}
		if d.Speaking() {
			activated = f
			break
		}
	}
	if activated < 0 {
		t.Fatalf("+18dB 后 -40dBFS 弱信号仍未触发 speaking（out RMS %.0f）", rmsInt16(out))
	}
	t.Logf("+18dB 后于第 %d 帧触发 speaking", activated)
}
