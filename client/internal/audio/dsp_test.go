package audio

import (
	"math"
	"testing"
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
//（spike 实测合成正弦 VAD 均值仅 0.069），VAD 正例需真实语音；
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

// TestSetVADSensitivity 灵敏度三档映射到 VAD 双门限，非法档回落中档。
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
		{VADSensLow, 0.7, 0.5},
		{VADSensMid, 0.6, 0.4},
		{VADSensHigh, 0.5, 0.3},
		{99, 0.6, 0.4}, // 非法档 → 中
	}
	for _, c := range cases {
		d.SetVADSensitivity(c.level)
		if e, x := d.VADEnter(), d.VADExit(); e != c.wantEnter || x != c.wantExit {
			t.Fatalf("level %d: enter=%.2f exit=%.2f, want %.2f/%.2f", c.level, e, x, c.wantEnter, c.wantExit)
		}
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
