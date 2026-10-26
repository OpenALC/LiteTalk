package audio

import (
	"math"
	"runtime"
	"testing"
)

// requireAPM APM 测试前置：darwin 未接入（stub），是唯一允许跳过的平台；
// Linux/Windows 的 CI 必须带真实实现，apmSupported 为假直接判失败。
func requireAPM(tb testing.TB) {
	tb.Helper()
	if runtime.GOOS == "darwin" {
		tb.Skip("darwin 尚未接入 WebRTC APM（stub）")
	}
	if !apmSupported() {
		tb.Fatalf("apmSupported() = false on %s：Linux/Windows 必须接入 APM", runtime.GOOS)
	}
}

// TestAPMSmoke APM 创建 + 连续 100 帧 capture/render 不报错。
func TestAPMSmoke(t *testing.T) {
	requireAPM(t)
	h, err := newAPM(apmOptions{AEC: true, NS: true, NSLevel: apmNSModerate, AGC2: true, HPF: true})
	if err != nil {
		t.Fatalf("newAPM: %v", err)
	}
	defer h.close()

	cap := make([]int16, FrameSize)
	ren := make([]int16, FrameSize)
	for f := 0; f < 100; f++ {
		for i := range cap {
			v := math.Sin(2 * math.Pi * 440 * float64(f*FrameSize+i) / SampleRate)
			ren[i] = int16(6000 * v)
			cap[i] = int16(3000 * v)
		}
		if err := h.processRender(ren); err != nil {
			t.Fatalf("render frame %d: %v", f, err)
		}
		if err := h.processCapture(cap); err != nil {
			t.Fatalf("capture frame %d: %v", f, err)
		}
	}
}

// aecRenderFrame 生成确定性的宽带参考帧（伪随机噪声，幅度约 ±12000）。
// 宽带激励让 AEC3 的延迟估计与自适应滤波器可靠收敛；纯音会让延迟估计
// 出现周期性歧义，不适合做收敛测试。
func aecRenderFrame(dst []int16, frame int) {
	x := uint32(0x9E3779B9) ^ (uint32(frame+1) * 0x85EBCA6B)
	for i := range dst {
		x ^= x << 13
		x ^= x >> 17
		x ^= x << 5
		dst[i] = int16(int32(x>>16)%24001 - 12000)
	}
}

// nearEndFrame 生成与回声参考不相关的近端信号（1000Hz + 噪声），
// 避免 AEC 把近端测试音也当成回波消掉。
func nearEndFrame(dst []int16, phase *float64, amp, noiseAmp float64, rng *uint32) {
	for i := range dst {
		*phase += 2 * math.Pi * 1000 / SampleRate
		*rng ^= *rng << 13
		*rng ^= *rng >> 17
		*rng ^= *rng << 5
		noise := float64(int32(*rng>>15)%int32(2*noiseAmp+1)) - noiseAmp
		dst[i] = int16(amp*math.Sin(*phase) + noise)
	}
}

// TestAECSuppressesEcho 合成回声收敛测试：render 双音 → 延迟 50ms、衰减 -6dB
// 的 capture 回声（无近端语音）。AEC3 有收敛期，先灌 4s 再测 1s 的 ERLE。
//
// 阈值取 6dB（能量降到约 1/4）：真实 AEC3 在平稳线性回声上通常远高于此，
// 6dB 只要求"明显下降"且给合成信号/AEC 非线性处理留足余量，避免 CI 抖动。
func TestAECSuppressesEcho(t *testing.T) {
	requireAPM(t)

	const (
		delayFrames   = 5   // 50ms 声学延迟
		echoGain      = 0.5 // 回声衰减 -6dB
		warmFrames    = 400 // 4s 收敛期
		measureFrames = 100 // 1s 测量窗
		minReduction  = 6.0 // dB
	)

	h, err := newAPM(apmOptions{AEC: true}) // 只开 AEC，排除 NS/AGC2 对能量的干扰
	if err != nil {
		t.Fatalf("newAPM: %v", err)
	}
	defer h.close()

	ring := make([][FrameSize]int16, delayFrames)
	ren := make([]int16, FrameSize)
	cap := make([]int16, FrameSize)

	var inEnergy, outEnergy float64
	for f := 0; f < warmFrames+measureFrames; f++ {
		aecRenderFrame(ren, f)
		echo := ring[f%delayFrames]
		for i := range cap {
			cap[i] = int16(float64(echo[i]) * echoGain)
		}
		copy(ring[f%delayFrames][:], ren)

		if err := h.processRender(ren); err != nil {
			t.Fatalf("render frame %d: %v", f, err)
		}
		inRMS := rmsInt16(cap)
		if err := h.processCapture(cap); err != nil {
			t.Fatalf("capture frame %d: %v", f, err)
		}
		if f >= warmFrames {
			inEnergy += inRMS * inRMS
			outRMS := rmsInt16(cap)
			outEnergy += outRMS * outRMS
		}
	}
	if inEnergy <= 0 {
		t.Fatal("测试信号无回声能量")
	}
	reduction := 10 * math.Log10(inEnergy/outEnergy)
	t.Logf("AEC ERLE = %.1f dB (in RMS %.0f, out RMS %.0f)",
		reduction, math.Sqrt(inEnergy/measureFrames), math.Sqrt(outEnergy/measureFrames))
	if reduction < minReduction {
		t.Fatalf("回声抑制 %.1f dB < %.1f dB（AEC 未收敛或失效）", reduction, minReduction)
	}
}

// TestAECDisabledPassesEcho 对照组：所有子模块关闭时 APM 必须近似直通，
// 证明上面的抑制确实来自 AEC，而不是 APM 的固定衰减。
func TestAECDisabledPassesEcho(t *testing.T) {
	requireAPM(t)
	h, err := newAPM(apmOptions{}) // AEC/NS/AGC2/HPF 全关
	if err != nil {
		t.Fatalf("newAPM: %v", err)
	}
	defer h.close()

	const delayFrames = 5
	ring := make([][FrameSize]int16, delayFrames)
	ren := make([]int16, FrameSize)
	cap := make([]int16, FrameSize)

	var inEnergy, outEnergy float64
	for f := 0; f < 20; f++ {
		aecRenderFrame(ren, f)
		echo := ring[f%delayFrames]
		for i := range cap {
			cap[i] = int16(float64(echo[i]) * 0.5)
		}
		copy(ring[f%delayFrames][:], ren)
		if err := h.processRender(ren); err != nil {
			t.Fatalf("render frame %d: %v", f, err)
		}
		inRMS := rmsInt16(cap)
		if err := h.processCapture(cap); err != nil {
			t.Fatalf("capture frame %d: %v", f, err)
		}
		if f >= 10 { // 跳过前 10 帧（含全零回声），只统计稳态直通
			inEnergy += inRMS * inRMS
			outRMS := rmsInt16(cap)
			outEnergy += outRMS * outRMS
		}
	}
	ratio := outEnergy / inEnergy
	t.Logf("子模块全关时输出/输入能量比 = %.4f", ratio)
	if ratio < 0.95 {
		t.Fatalf("全关时 APM 不应衰减信号：能量比 %.4f < 0.95", ratio)
	}
}

// BenchmarkAPMRender APM 参考帧处理成本（混音 tick 每 10ms 一次）。
// 不依赖 Go 堆，allocs 必须为 0。
func BenchmarkAPMRender(b *testing.B) {
	requireAPM(b)
	h, err := newAPM(apmOptions{AEC: true, NS: true, NSLevel: apmNSModerate, AGC2: true, HPF: true})
	if err != nil {
		b.Fatal(err)
	}
	defer h.close()
	frame := make([]int16, FrameSize)
	aecRenderFrame(frame, 0)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if err := h.processRender(frame); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkAPMCapture APM 采集帧处理成本（含 AEC3/NS/AGC2/HPF）。
func BenchmarkAPMCapture(b *testing.B) {
	requireAPM(b)
	h, err := newAPM(apmOptions{AEC: true, NS: true, NSLevel: apmNSModerate, AGC2: true, HPF: true})
	if err != nil {
		b.Fatal(err)
	}
	defer h.close()
	frame := make([]int16, FrameSize)
	var phase float64
	var rng uint32 = 7
	sineNoiseFrame(frame, &phase, 12000, 2000, &rng)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if err := h.processCapture(frame); err != nil {
			b.Fatal(err)
		}
	}
}

// TestDSPAPMIntegration DSP 的 APM 链路接线：render 参考 + capture 近端信号
// 均能处理，AGC/AEC 热切换不报错，数字静音帧输出全零（AEC 状态仍推进）。
func TestDSPAPMIntegration(t *testing.T) {
	requireAPM(t)

	cfg := DefaultDSPConfig()
	cfg.Denoise = false // 用 RMS 门限验证说话状态，避免合成音的 RNNoise VAD 偏低
	d, err := NewDSP(cfg)
	if err != nil {
		t.Fatalf("NewDSP: %v", err)
	}
	defer d.Close()
	if !d.APMActive() {
		t.Fatal("APM 应已启用")
	}

	d.SetAGC(true) // 映射到 APM AGC2，同时验证重配置路径
	in := make([]int16, FrameSize)
	out := make([]int16, FrameSize)
	ren := make([]int16, FrameSize)
	var phase float64
	var rng uint32 = 0xAEC1
	for f := 0; f < 100; f++ {
		aecRenderFrame(ren, f)
		nearEndFrame(in, &phase, 12000, 1000, &rng)
		d.ProcessRender(ren)
		if _, err := d.Process(out, in); err != nil {
			t.Fatalf("Process frame %d: %v", f, err)
		}
	}
	if peakAbs(out) == 0 {
		t.Fatal("近端信号经 APM 后全为零")
	}
	if !d.Speaking() {
		t.Fatal("近端信号应触发说话状态")
	}

	// 数字静音：APM 仍处理（AEC 状态推进），但输出应落到近零；说话态退出。
	// AEC3 的残余回声抑制可能注入极低电平舒适噪声，故断言能量而非逐样本为零。
	clear(ren)
	for f := 0; f < 300; f++ {
		clear(in)
		d.ProcessRender(ren)
		if _, err := d.Process(out, in); err != nil {
			t.Fatalf("silence frame %d: %v", f, err)
		}
		if f == 299 && rmsInt16(out) >= 100 {
			t.Fatalf("静音帧输出 RMS %.0f 过高", rmsInt16(out))
		}
	}
	if d.Speaking() {
		t.Fatal("持续静音后说话态未退出")
	}

	if d.DisableAEC() {
		t.Fatal("DisableAEC 默认应为 false")
	}
	d.SetDisableAEC(true)
	if !d.DisableAEC() {
		t.Fatal("SetDisableAEC(true) 未生效")
	}
	// AEC 关闭且参考为零时，纯数字静音必须逐样本为零（与旧链路短路等价）。
	for f := 0; f < 20; f++ {
		clear(in)
		clear(ren)
		d.ProcessRender(ren)
		if _, err := d.Process(out, in); err != nil {
			t.Fatalf("AEC off silence frame %d: %v", f, err)
		}
		if peakAbs(out) != 0 {
			t.Fatalf("AEC 关闭后静音输出非零（peak=%d）", peakAbs(out))
		}
	}
	d.SetDisableAEC(false)
	if d.DisableAEC() {
		t.Fatal("SetDisableAEC(false) 未生效")
	}
}
