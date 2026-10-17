package audio

import (
	"math"
	"strings"
)

// InputMode 采集输入声道模式（下混策略）。零值 = auto。
type InputMode uint32

const (
	InputModeAuto  InputMode = iota // 自动：指数平滑能量选最强声道，反相锁定（默认）
	InputModeLeft                   // 固定取第 0 声道
	InputModeRight                  // 固定取第 1 声道（单声道设备退化为唯一声道）
	InputModeMix                    // 全声道算术平均
)

// ParseInputMode 字符串 → InputMode；空串与非法值回落 auto（配置读路径用）。
func ParseInputMode(s string) InputMode {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "left":
		return InputModeLeft
	case "right":
		return InputModeRight
	case "mix":
		return InputModeMix
	default: // "", "auto" 与一切非法值
		return InputModeAuto
	}
}

// String 输出配置层存储用的稳定字符串（与 ParseInputMode 互逆）。
func (m InputMode) String() string {
	switch m {
	case InputModeLeft:
		return "left"
	case InputModeRight:
		return "right"
	case InputModeMix:
		return "mix"
	default:
		return "auto"
	}
}

// downmixState auto 模式的跨帧状态：各声道能量 EMA、当前锁定声道、反相标志。
// 仅采集回调 goroutine 触碰，无锁；全部零值可用。
type downmixState struct {
	ema     [4]float64 // 各声道帧能量的指数平滑（≤4 声道）
	held    int        // 当前输出声道（auto）
	inited  bool
	negCorr bool // 顶对声道相关系数持续为负（反相），锁定 held 防翻转
}

const (
	downmixEMAAlpha   = 0.2  // EMA 步进：≈50ms 时间常数（10ms 回调周期）
	downmixSwitchGain = 1.5  // 换声道需超过 held 能量的倍数（≈3.5dB 迟滞）
	downmixCorrLock   = -0.2 // 顶对相关系数低于此值判反相，冻结声道选择
)

// downmixFrame 把 ch 声道交织采样下混为单声道写入 dst，返回写入的采样数（=帧数）。
// 零分配：dst 为调用方预分配缓冲，状态挂在 st 上。src 长度不是 ch 整数倍时丢弃尾部。
// auto：EMA 能量选最强声道直取；顶对相关为负（反相）时锁定当前声道，防止等能量
// 反相两路在帧间来回切换造成相位跳变。left/right 直取声道，mix 算术平均。
func downmixFrame(dst, src []int16, ch int, mode InputMode, st *downmixState) int {
	if ch <= 1 {
		n := len(dst)
		if len(src) < n {
			n = len(src)
		}
		copy(dst[:n], src[:n])
		return n
	}
	frames := len(src) / ch
	if len(dst) < frames {
		frames = len(dst)
	}
	switch mode {
	case InputModeLeft:
		for i := 0; i < frames; i++ {
			dst[i] = src[i*ch]
		}
	case InputModeRight:
		c := 1
		if ch < 2 {
			c = 0
		}
		for i := 0; i < frames; i++ {
			dst[i] = src[i*ch+c]
		}
	case InputModeMix:
		inv := 1 / float64(ch)
		for i := 0; i < frames; i++ {
			var acc float64
			base := i * ch
			for c := 0; c < ch; c++ {
				acc += float64(src[base+c])
			}
			dst[i] = clampInt16(acc * inv)
		}
	default: // InputModeAuto
		downmixAuto(dst, src, ch, frames, st)
	}
	return frames
}

// downmixAuto 自动模式：更新各声道能量 EMA → 相关性检测 → 锁定声道直取。
func downmixAuto(dst, src []int16, ch, frames int, st *downmixState) {
	// 1. 帧能量（均方）→ EMA。
	var e [4]float64
	inv := 1 / float64(frames)
	for c := 0; c < ch; c++ {
		var acc float64
		for i := 0; i < frames; i++ {
			v := float64(src[i*ch+c])
			acc += v * v
		}
		e[c] = acc * inv
	}
	if !st.inited {
		st.ema, st.inited = e, true
	} else {
		for c := 0; c < ch; c++ {
			st.ema[c] += downmixEMAAlpha * (e[c] - st.ema[c])
		}
	}

	// 2. EMA 最强/次强声道。
	top1, top2 := 0, -1
	for c := 1; c < ch; c++ {
		if st.ema[c] > st.ema[top1] {
			top1 = c
		}
	}
	for c := 0; c < ch; c++ {
		if c == top1 {
			continue
		}
		if top2 < 0 || st.ema[c] > st.ema[top2] {
			top2 = c
		}
	}

	// 3. 反相检测：仅当次强声道能量不可忽略时算相关系数（前两帧能量未立稳时跳过）。
	st.negCorr = false
	if top2 >= 0 && st.ema[top2] > st.ema[top1]*0.01 {
		var sab, saa, sbb float64
		for i := 0; i < frames; i++ {
			a := float64(src[i*ch+top1])
			b := float64(src[i*ch+top2])
			sab += a * b
			saa += a * a
			sbb += b * b
		}
		st.negCorr = sab/math.Sqrt(saa*sbb+1) < downmixCorrLock
	}

	// 4. 选择：反相时冻结当前声道（防等能量两路帧间翻转）；否则带迟滞跟随最强。
	if !st.negCorr && top1 != st.held && st.ema[top1] > st.ema[st.held]*downmixSwitchGain {
		st.held = top1
	}
	sel := st.held
	for i := 0; i < frames; i++ {
		dst[i] = src[i*ch+sel]
	}
}

// clampInt16 浮点就地截断到 int16 值域。
func clampInt16(v float64) int16 {
	switch {
	case v > math.MaxInt16:
		return math.MaxInt16
	case v < math.MinInt16:
		return math.MinInt16
	default:
		return int16(v)
	}
}
