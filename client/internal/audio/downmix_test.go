package audio

import (
	"math"
	"testing"
)

// stereoFrame 生成一帧 2 声道交织采样：ch0 用 amp0*sin，ch1 用 amp1*sin（sign 控反相）。
func stereoFrame(dst []int16, phase *float64, amp0, amp1 float64, sign1 float64) {
	for i := 0; i < len(dst)/2; i++ {
		*phase += 2 * math.Pi * 440 / SampleRate
		s := math.Sin(*phase)
		dst[i*2] = int16(amp0 * s)
		dst[i*2+1] = int16(amp1 * s * sign1)
	}
}

// monoRefRMS 单声道参考（左声道）的 RMS。
func monoRefRMS(amp float64) float64 { return amp / math.Sqrt2 }

func approxEqualDB(t *testing.T, got, want float64, what string) {
	t.Helper()
	if want <= 0 {
		t.Fatalf("%s 参考 RMS 非法: %v", what, want)
	}
	db := 20 * math.Log10(got/want)
	if db < -1 || db > 1 {
		t.Fatalf("%s: got RMS %.1f vs 参考 %.1f（%.2f dB，超出 ±1dB）", what, got, want, db)
	}
}

// runDownmix 连续灌 n 帧，返回最后一帧下混输出（供 RMS 比较）。
func runDownmix(frames int, ch int, mode InputMode, fill func(dst []int16, f int)) ([]int16, downmixState) {
	src := make([]int16, FrameSize*ch)
	dst := make([]int16, FrameSize)
	var st downmixState
	var phase float64
	for f := 0; f < frames; f++ {
		stereoFrame(src, &phase, 0, 0, 1)
		fill(src, f)
		downmixFrame(dst, src, ch, mode, &st)
	}
	return dst, st
}

// TestDownmixLeftOnly 左 only：auto 与 left 输出 = 左声道（±1dB）；右声道为零。
func TestDownmixLeftOnly(t *testing.T) {
	for _, mode := range []InputMode{InputModeAuto, InputModeLeft} {
		out, _ := runDownmix(30, 2, mode, func(dst []int16, _ int) {
			var ph float64
			stereoFrame(dst, &ph, 8000, 0, 1)
		})
		// runDownmix 每帧重置相位会破坏连续性，直接对输出做能量比较即可。
		approxEqualDB(t, rmsInt16(out), monoRefRMS(8000), "左 only")
	}
}

// TestDownmixRightOnly 右 only：auto 与 right 输出 = 右声道（±1dB）。
func TestDownmixRightOnly(t *testing.T) {
	for _, mode := range []InputMode{InputModeAuto, InputModeRight} {
		out, _ := runDownmix(30, 2, mode, func(dst []int16, _ int) {
			var ph float64
			stereoFrame(dst, &ph, 0, 8000, 1)
		})
		approxEqualDB(t, rmsInt16(out), monoRefRMS(8000), "右 only")
	}
}

// TestDownmixAntiPhase 等能量反相两路：auto 锁定单声道输出（不会互相抵消），
// 输出能量 = 单路参考（±1dB）；且多帧运行中选择不翻转（相位连续）。
func TestDownmixAntiPhase(t *testing.T) {
	src := make([]int16, FrameSize*2)
	dst := make([]int16, FrameSize)
	var st downmixState
	var phase float64
	const frames = 50
	heldSeen := map[int]bool{}
	for f := 0; f < frames; f++ {
		for i := 0; i < FrameSize; i++ {
			phase += 2 * math.Pi * 440 / SampleRate
			s := math.Sin(phase)
			src[i*2] = int16(8000 * s)
			src[i*2+1] = int16(-8000 * s)
		}
		downmixFrame(dst, src, 2, InputModeAuto, &st)
		heldSeen[st.held] = true
		if f == frames-1 {
			approxEqualDB(t, rmsInt16(dst), monoRefRMS(8000), "反相")
		}
	}
	if len(heldSeen) != 1 {
		t.Fatalf("反相锁定失效：输出声道翻转过 %v", heldSeen)
	}
}

// TestDownmixAutoPicksLoudest auto 选取能量更高的声道（±1dB 内与该声道一致）。
func TestDownmixAutoPicksLoudest(t *testing.T) {
	// 左 -6dB、右满幅：应选右。
	out, st := runDownmix(30, 2, InputModeAuto, func(dst []int16, _ int) {
		var ph float64
		stereoFrame(dst, &ph, 4000, 8000, 1)
	})
	if st.held != 1 {
		t.Fatalf("auto 应选右声道（EMA 更大），held=%d", st.held)
	}
	approxEqualDB(t, rmsInt16(out), monoRefRMS(8000), "auto 右更大")
}

// TestDownmixAutoSwitchWithHysteresis 强度反转后（超过迟滞倍数）跟随切换声道。
func TestDownmixAutoSwitchWithHysteresis(t *testing.T) {
	src := make([]int16, FrameSize*2)
	dst := make([]int16, FrameSize)
	var st downmixState
	var phase float64
	fill := func(a, b float64) {
		for i := 0; i < FrameSize; i++ {
			phase += 2 * math.Pi * 440 / SampleRate
			s := math.Sin(phase)
			src[i*2] = int16(a * s)
			src[i*2+1] = int16(b * s)
		}
	}
	for f := 0; f < 20; f++ {
		fill(8000, 100)
		downmixFrame(dst, src, 2, InputModeAuto, &st)
	}
	if st.held != 0 {
		t.Fatalf("左主导阶段 held=%d, want 0", st.held)
	}
	for f := 0; f < 20; f++ {
		fill(100, 8000)
		downmixFrame(dst, src, 2, InputModeAuto, &st)
	}
	if st.held != 1 {
		t.Fatalf("右反超后应切换：held=%d, want 1", st.held)
	}
}

// TestDownmixMixAndPassthrough mix=各声道平均；单声道直通；left/right/mix 解析。
func TestDownmixMixAndPassthrough(t *testing.T) {
	src := make([]int16, FrameSize*2)
	dst := make([]int16, FrameSize)
	var st downmixState
	var ph float64
	// 同相两路 8000：mix 平均仍是 8000（无损）。
	stereoFrame(src, &ph, 8000, 8000, 1)
	downmixFrame(dst, src, 2, InputModeMix, &st)
	approxEqualDB(t, rmsInt16(dst), monoRefRMS(8000), "mix 同相平均")

	// 单声道直通。
	mono := make([]int16, FrameSize)
	var mph float64
	for i := range mono {
		mph += 2 * math.Pi * 440 / SampleRate
		mono[i] = int16(5000 * math.Sin(mph))
	}
	if n := downmixFrame(dst, mono, 1, InputModeAuto, &st); n != FrameSize {
		t.Fatalf("单声道直通帧数 %d, want %d", n, FrameSize)
	}
	for i := range mono {
		if dst[i] != mono[i] {
			t.Fatalf("单声道直通被改动 @%d", i)
		}
	}
}

// TestParseInputMode 字符串解析与逆变换。
func TestParseInputMode(t *testing.T) {
	cases := map[string]InputMode{
		"": InputModeAuto, "auto": InputModeAuto, "AUTO": InputModeAuto,
		"left": InputModeLeft, "right": InputModeRight, "mix": InputModeMix,
		"bogus": InputModeAuto,
	}
	for in, want := range cases {
		if got := ParseInputMode(in); got != want {
			t.Fatalf("ParseInputMode(%q)=%v, want %v", in, got, want)
		}
	}
	for _, m := range []InputMode{InputModeAuto, InputModeLeft, InputModeRight, InputModeMix} {
		if got := ParseInputMode(m.String()); got != m {
			t.Fatalf("String/Parse 不互逆: %v → %q → %v", m, m.String(), got)
		}
	}
}

// BenchmarkDownmix 下混函数基准：四种模式都必须 0 allocs/op。
func BenchmarkDownmix(b *testing.B) {
	src := make([]int16, FrameSize*4)
	dst := make([]int16, FrameSize)
	var st downmixState
	var phase float64
	for i := 0; i < FrameSize; i++ {
		phase += 2 * math.Pi * 440 / SampleRate
		s := math.Sin(phase)
		for c := 0; c < 4; c++ {
			src[i*4+c] = int16(6000 * s)
		}
	}
	for _, mode := range []InputMode{InputModeAuto, InputModeLeft, InputModeRight, InputModeMix} {
		b.Run(mode.String(), func(b *testing.B) {
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				downmixFrame(dst, src, 4, mode, &st)
			}
		})
	}
}
