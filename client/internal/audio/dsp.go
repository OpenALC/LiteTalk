package audio

import (
	"math"
	"sync/atomic"

	"github.com/VoiceBlender/rnnoise-go"
	"github.com/VoiceBlender/rnnoise-go/model"
)

// 管线帧结构常量（硬约束：48k / mono / int16）。
const (
	SampleRate    = 48000
	FrameSize     = 480 // RNNoise 帧：10ms @ 48k
	OpusFrameSize = 960 // Opus 帧：20ms = 2 个 RNNoise 帧
)

// DSPConfig 降噪/增益/说话判定配置。
type DSPConfig struct {
	Denoise bool    // 是否启用 RNNoise（关闭时 VAD 不可用，说话判定退化为纯 RMS 门限）
	Gain    float64 // 线性增益，1.0 为不变

	// 说话判定：VAD 概率与 RMS 双门限 + 帧数迟滞。
	// spike 实测纯白噪声 VAD 均值约 0.569 偏高，单靠概率会误触发，必须叠加 RMS 门限。
	VADEnter    float32 // 进入判定的 VAD 门限（>）
	VADExit     float32 // 退出判定的 VAD 门限（<）
	RMSEnter    float64 // 进入判定的 RMS 门限（>=）
	RMSExit     float64 // 退出判定的 RMS 门限（<），低于 RMSEnter 形成回差
	EnterFrames int     // 连续满足进入条件 N 帧才置 speaking（防抖）
	ExitFrames  int     // 连续满足退出条件 N 帧才清除 speaking（挂起，避免字间停顿闪断）

	// OnSpeakingChange 状态翻转时同步回调（在处理 goroutine 上，必须非阻塞）。
	OnSpeakingChange func(speaking bool)
}

// DefaultDSPConfig 返回一组经 spike 数据校准的默认配置。
func DefaultDSPConfig() DSPConfig {
	return DSPConfig{
		Denoise:     true,
		Gain:        1.0,
		VADEnter:    0.6,
		VADExit:     0.4,
		RMSEnter:    500,
		RMSExit:     300,
		EnterFrames: 3,  // 30ms 起判
		ExitFrames:  15, // 150ms 挂起
	}
}

// DSP 封装 RNNoise 降噪 + VAD + 增益 + 说话状态机。
// Process 零分配；仅允许单 goroutine 调用（增益/开关/门限可跨 goroutine 原子修改）。
type DSP struct {
	cfg      DSPConfig
	denoiser *rnnoise.Denoiser

	gainBits     atomic.Uint64 // math.Float64bits(Gain)
	denoiseOn    atomic.Bool
	speaking     atomic.Bool
	vadEnterBits atomic.Uint64 // math.Float32bits(VADEnter)，热可调
	vadExitBits  atomic.Uint64 // math.Float32bits(VADExit)，热可调

	// 处理 goroutine 私有状态
	enterRun int
	exitRun  int
}

// NewDSP 加载内嵌模型并创建降噪器。cfg 中的零值字段回落为默认值。
func NewDSP(cfg DSPConfig) (*DSP, error) {
	def := DefaultDSPConfig()
	if cfg.Gain == 0 {
		cfg.Gain = def.Gain
	}
	if cfg.VADEnter == 0 {
		cfg.VADEnter = def.VADEnter
	}
	if cfg.VADExit == 0 {
		cfg.VADExit = def.VADExit
	}
	if cfg.RMSEnter == 0 {
		cfg.RMSEnter = def.RMSEnter
	}
	if cfg.RMSExit == 0 {
		cfg.RMSExit = def.RMSExit
	}
	if cfg.EnterFrames == 0 {
		cfg.EnterFrames = def.EnterFrames
	}
	if cfg.ExitFrames == 0 {
		cfg.ExitFrames = def.ExitFrames
	}
	dn, err := rnnoise.New(rnnoise.Options{Model: model.MustLoad()})
	if err != nil {
		return nil, err
	}
	d := &DSP{cfg: cfg, denoiser: dn}
	d.gainBits.Store(math.Float64bits(cfg.Gain))
	d.denoiseOn.Store(cfg.Denoise)
	d.vadEnterBits.Store(uint64(math.Float32bits(cfg.VADEnter)))
	d.vadExitBits.Store(uint64(math.Float32bits(cfg.VADExit)))
	return d, nil
}

// SetGain 运行时调整线性增益（原子，热路径无锁）。
func (d *DSP) SetGain(g float64) {
	if g < 0 {
		g = 0
	}
	d.gainBits.Store(math.Float64bits(g))
}

// Gain 返回当前增益。
func (d *DSP) Gain() float64 {
	return math.Float64frombits(d.gainBits.Load())
}

// SetDenoise 运行时开关降噪（原子，热路径无锁）。
func (d *DSP) SetDenoise(on bool) { d.denoiseOn.Store(on) }

// Denoise 返回降噪开关状态。
func (d *DSP) Denoise() bool { return d.denoiseOn.Load() }

// VAD 灵敏度档位（设置页「低/中/高」）。
const (
	VADSensLow  = 0 // 迟钝：enter 0.7 / exit 0.5（嘈杂环境防误触发）
	VADSensMid  = 1 // 默认：enter 0.6 / exit 0.4（DefaultDSPConfig 校准值）
	VADSensHigh = 2 // 灵敏：enter 0.5 / exit 0.3（轻声说话也能触发）
)

// SetVADSensitivity 按档位调整 VAD 双门限（原子写，回差恒 ≥0.2）。
// 非法档位按 VADSensMid 处理。
func (d *DSP) SetVADSensitivity(level int) {
	var enter, exit float32
	switch level {
	case VADSensLow:
		enter, exit = 0.7, 0.5
	case VADSensHigh:
		enter, exit = 0.5, 0.3
	default:
		enter, exit = 0.6, 0.4
	}
	d.vadEnterBits.Store(uint64(math.Float32bits(enter)))
	d.vadExitBits.Store(uint64(math.Float32bits(exit)))
}

// SetVADThresholds 直接指定 VAD 双门限（原子写）。exit > enter-0.1 时强制
// exit = enter-0.2，保证回差。设置页用 SetVADSensitivity 档位即可。
func (d *DSP) SetVADThresholds(enter, exit float32) {
	if exit > enter-0.1 {
		exit = enter - 0.2
	}
	d.vadEnterBits.Store(uint64(math.Float32bits(enter)))
	d.vadExitBits.Store(uint64(math.Float32bits(exit)))
}

// VADEnter / VADExit 返回当前门限（原子读）。
func (d *DSP) VADEnter() float32 {
	return math.Float32frombits(uint32(d.vadEnterBits.Load()))
}

// VADExit 返回当前退出门限。
func (d *DSP) VADExit() float32 {
	return math.Float32frombits(uint32(d.vadExitBits.Load()))
}

// Speaking 返回当前说话状态。
func (d *DSP) Speaking() bool { return d.speaking.Load() }

// Process 处理一个 480 采样（10ms）帧：降噪 → 增益 → RMS/VAD 说话判定。
// out 与 in 长度都必须 >= FrameSize 且不得重叠；返回 VAD 概率（降噪关闭时为 0）。
// 全程在预分配缓冲上操作，零堆分配。
func (d *DSP) Process(out, in []int16) (float32, error) {
	var vad float32
	var err error
	if d.denoiseOn.Load() {
		vad, err = d.denoiser.ProcessInt16(out[:FrameSize], in[:FrameSize])
		if err != nil {
			return 0, err
		}
	} else {
		copy(out[:FrameSize], in[:FrameSize])
	}
	if g := d.Gain(); g != 1.0 {
		applyGain(out[:FrameSize], g)
	}
	d.updateSpeaking(vad, rmsInt16(out[:FrameSize]))
	return vad, nil
}

// updateSpeaking 双门限 + 迟滞帧数的状态机。仅处理 goroutine 调用。
func (d *DSP) updateSpeaking(vad float32, rms float64) {
	vadAvailable := d.denoiseOn.Load()
	speaking := d.speaking.Load()

	if !speaking {
		enterOK := rms >= d.cfg.RMSEnter && (!vadAvailable || vad > d.VADEnter())
		if enterOK {
			d.enterRun++
			if d.enterRun >= d.cfg.EnterFrames {
				d.enterRun = 0
				d.exitRun = 0
				d.speaking.Store(true)
				if cb := d.cfg.OnSpeakingChange; cb != nil {
					cb(true)
				}
			}
		} else {
			d.enterRun = 0
		}
		return
	}

	exitNow := rms < d.cfg.RMSExit || (vadAvailable && vad < d.VADExit())
	if exitNow {
		d.exitRun++
		if d.exitRun >= d.cfg.ExitFrames {
			d.exitRun = 0
			d.speaking.Store(false)
			if cb := d.cfg.OnSpeakingChange; cb != nil {
				cb(false)
			}
		}
	} else {
		d.exitRun = 0
	}
}

// applyGain 就地施加线性增益并截断到 int16。
func applyGain(p []int16, g float64) {
	for i, s := range p {
		v := float64(s) * g
		switch {
		case v > math.MaxInt16:
			p[i] = math.MaxInt16
		case v < math.MinInt16:
			p[i] = math.MinInt16
		default:
			p[i] = int16(v)
		}
	}
}

// rmsInt16 计算 RMS（零分配）。
func rmsInt16(p []int16) float64 {
	if len(p) == 0 {
		return 0
	}
	var acc float64
	for _, s := range p {
		v := float64(s)
		acc += v * v
	}
	return math.Sqrt(acc / float64(len(p)))
}
