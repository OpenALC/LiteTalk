package audio

import (
	"fmt"
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

	// 频道可配置的 Opus 帧长（#46）：0 在配置层归一成默认 20ms。
	FrameMS10 = 10
	FrameMS20 = 20
)

// 前置增益 dB 值域（0.25×~16×）。越界在 setter 报错、构造时收敛。
const (
	preGainMinDB = -12.0
	preGainMaxDB = 24.0
)

// dbToLinear dB → 线性倍数（满刻度基准 32767）。
func dbToLinear(db float64) float64 { return math.Pow(10, db/20) }

// dbFS 线性幅度 → dBFS（满刻度 32767 基准），下限 -100 防 log(0)=-Inf。
func dbFS(v float64) float64 {
	if v <= 0 {
		return -100
	}
	if db := 20 * math.Log10(v/32767); db < -100 {
		return -100
	} else {
		return db
	}
}

// dbToAmp dBFS → 线性幅度。
func dbToAmp(db float64) float64 { return 32767 * dbToLinear(db) }

// 底噪统计与自适应说话门限（dBFS 域；v1.1）。
const (
	noiseWinFrames    = 200   // minimum-statistics 窗口：2s @10ms
	noiseInitDB       = -50.0 // 窗空时的初始底噪
	noisePushMarginDB = 20.0  // 仅接收 ≤底噪+20dB 的帧入窗（语音不入窗）
	noiseCeilingDB    = -20.0 // 或 ≤-20dBFS（稳态偏高的环境噪声也入窗）
	enterOffsetDB     = 12.0  // 说话进入门限 = 底噪+12dB
	exitOffsetDB      = 6.0   // 说话退出门限 = 底噪+6dB；AGC 门控同用 +6dB
	thresholdFloorDB  = -50.0 // 门限绝对下限（弱底噪环境不再无限下探）
)

// enterThresholdDB / exitThresholdDB 自适应说话门限（dBFS）。
func enterThresholdDB(floorDB float64) float64 { return max(floorDB+enterOffsetDB, thresholdFloorDB) }
func exitThresholdDB(floorDB float64) float64  { return max(floorDB+exitOffsetDB, thresholdFloorDB) }

// noiseTracker minimum-statistics 底噪：2s 滑动窗取最小（预分配环形，零分配）。
// 收帧带准入门槛（≤底噪+20dB 或 ≤-20dBFS），防止连续语音把窗灌满后门限
// 跟着语音抬升；数字静音帧由调用方跳过（静音没有可估底噪，且会把窗压到
// -100 后长时间排斥真实底噪入窗）。
type noiseTracker struct {
	win   [noiseWinFrames]float64
	head  int
	count int
}

// observe 收录一帧 RMS（int16 域）。
func (n *noiseTracker) observe(rms float64) {
	db := dbFS(rms)
	if cur := n.floorDB(); db > cur+noisePushMarginDB && db > noiseCeilingDB {
		return
	}
	if n.count < len(n.win) {
		n.win[n.count] = db
		n.count++
		return
	}
	n.win[n.head] = db
	n.head = (n.head + 1) % len(n.win)
}

// floorDB 窗内最小值 = 当前底噪估计；窗空回落初始值。
func (n *noiseTracker) floorDB() float64 {
	if n.count == 0 {
		return noiseInitDB
	}
	m := n.win[0]
	for i := 1; i < n.count; i++ {
		if n.win[i] < m {
			m = n.win[i]
		}
	}
	return m
}

// DSPConfig 降噪/增益/说话判定配置。
type DSPConfig struct {
	Denoise bool    // 是否启用 RNNoise（关闭时 VAD 不可用，说话判定退化为纯 RMS 门限）
	Gain    float64 // 线性增益，1.0 为不变（输出音量）
	// PreGainDB 前置增益 dB（-12~+24 ≈ 0.25×~16×）：施加在 RNNoise 之前，
	// 弱麦先抬进降噪器/VAD 的有效区；零值 0 = 不变。
	PreGainDB float64

	// 说话判定：VAD 概率与自适应 RMS 门限并联 + 帧数迟滞。
	// spike 实测纯白噪声 VAD 均值约 0.569 偏高，单靠概率会误触发，必须叠加 RMS 门限。
	// RMS 门限自适应底噪（v1.1）：enter=底噪+12dB、exit=底噪+6dB，绝对下限 -50dBFS；
	// 旧固定 RMSEnter/RMSExit 已删除（固定 500 让弱麦克风永远到不了门限）。
	VADEnter    float32 // 进入判定的 VAD 门限（>）
	VADExit     float32 // 退出判定的 VAD 门限（<）
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
		EnterFrames: 3,  // 30ms 起判
		ExitFrames:  15, // 150ms 挂起
	}
}

// DSP 封装 RNNoise 降噪 + VAD + AGC + 增益 + 说话状态机。
// Process 零分配；仅允许单 goroutine 调用（增益/开关/门限/强度/AGC 可跨 goroutine 原子修改）。
type DSP struct {
	cfg DSPConfig
	// denoiserPtr 原子槽位：降噪强度切换时整体替换实例（Model 只读可共享，
	// 替换只在 setter 发生，Process 侧无锁读取指针）。
	denoiserPtr atomic.Pointer[rnnoise.Denoiser]

	gainBits     atomic.Uint64 // math.Float64bits(Gain)
	preGainBits  atomic.Uint64 // math.Float64bits(前置线性增益)，热可调
	denoiseOn    atomic.Bool
	speaking     atomic.Bool
	levelBits    atomic.Uint32 // RFC 6464 电平（0=0dB … 127=静音，#33）：updateSpeaking 写、SendOpus 读
	vadEnterBits atomic.Uint64 // math.Float32bits(VADEnter)，热可调
	vadExitBits  atomic.Uint64 // math.Float32bits(VADExit)，热可调
	agcOn        atomic.Bool   // 自动增益开关（v1.1 默认开，nil→true 见 config 层）

	// preBuf 前置增益的预分配 scratch（Process 里 in→preBuf→RNNoise，不改调用方 in）。
	preBuf [FrameSize]int16

	// noise minimum-statistics 底噪（说话门限与 AGC 门控共用；Process goroutine 私有）。
	noise noiseTracker

	// 处理 goroutine 私有状态
	enterRun  int
	exitRun   int
	silentRun int     // 连续纯零帧计数（防 denormal 短路用，见 Process）
	agcGain   float64 // AGC 当前增益（仅 Process goroutine 触达，初值 1.0）
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
	d := &DSP{cfg: cfg, agcGain: 1.0}
	d.denoiserPtr.Store(dn)
	d.gainBits.Store(math.Float64bits(cfg.Gain))
	// 构造路径越界收敛（运行时 setter 报错）：零值 0dB = 1.0× 无需特殊处理。
	pre := cfg.PreGainDB
	if pre < preGainMinDB {
		pre = preGainMinDB
	}
	if pre > preGainMaxDB {
		pre = preGainMaxDB
	}
	d.preGainBits.Store(math.Float64bits(dbToLinear(pre)))
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

// SetPreGainDB 运行时调整前置增益（原子，热路径无锁）。范围 -12~+24dB，越界报错。
func (d *DSP) SetPreGainDB(db float64) error {
	if db < preGainMinDB || db > preGainMaxDB {
		return fmt.Errorf("前置增益越界: %.1fdB（允许 %.0f~%+.0f）", db, preGainMinDB, preGainMaxDB)
	}
	d.preGainBits.Store(math.Float64bits(dbToLinear(db)))
	return nil
}

// PreGainDB 返回当前前置增益 dB（0 = 不变）。
func (d *DSP) PreGainDB() float64 {
	g := math.Float64frombits(d.preGainBits.Load())
	if g <= 0 {
		return 0
	}
	return 20 * math.Log10(g)
}

// SetDenoise 运行时开关降噪（原子，热路径无锁）。
func (d *DSP) SetDenoise(on bool) { d.denoiseOn.Store(on) }

// Denoise 返回降噪开关状态。
func (d *DSP) Denoise() bool { return d.denoiseOn.Load() }

// AGC（自动增益）参数（v1.1 重做）：以 RMS 目标迭代自适应增益，替代手动增益。
// 步进不对称：下调快（防削波）、上调慢（防泵浦效应/呼吸感）。
const (
	agcTargetRMS  = 1200.0 // 目标 RMS（int16 域，约 -18dBFS 的语音均值）
	agcGainMin    = 0.2    // 增益下限（防止把底噪抬得过高）
	agcGainMax    = 10.0   // 增益上限（v1.1：5→10，弱麦补偿余量）
	agcAttack     = 0.3    // 上调步进系数（慢）：弱信号逐帧小幅补增益
	agcRelease    = 0.8    // 下调步进系数（快）：过强信号迅速压回
	agcFloorRatio = 0.2    // 无 VAD（降噪关）时的绝对 RMS 地板（target×0.2）
	agcInitGain   = 1.0
)

// SetAGC 开关自动增益（原子）。开启后 AGC 增益插在原固定增益之前应用。
func (d *DSP) SetAGC(on bool) { d.agcOn.Store(on) }

// AGC 当前开关状态。
func (d *DSP) AGC() bool { return d.agcOn.Load() }

// denoiseStrengthAggressiveness 降噪强度档 → RNNoise Aggressiveness（增益幂指数）。
// rnnoise-go 构造参数：1.0 为原生行为，>1 抑制更狠；档间同时受默认 GainFloor 影响。
func denoiseStrengthAggressiveness(level int) float64 {
	switch level {
	case 1: // 弱
		return 1.0
	case 3: // 强
		return 2.0
	default: // 中（含非法档回落）
		return 1.5
	}
}

// SetDenoiseStrength 切换降噪强度（v0.1.2）：按档重建 Denoiser 并原子替换。
// Model 只读可跨实例共享（model.MustLoad 自带 once 缓存），重建零热路径开销。
func (d *DSP) SetDenoiseStrength(level int) error {
	dn, err := rnnoise.New(rnnoise.Options{
		Model:          model.MustLoad(),
		Aggressiveness: denoiseStrengthAggressiveness(level),
	})
	if err != nil {
		return err
	}
	d.denoiserPtr.Store(dn)
	return nil
}

// VAD 灵敏度五档（设置页 v0.1.2 刻度 0–4；旧三档 0/1/2 由 config 层映射为 0/2/4）。
const (
	VADSensLowest  = 0 // 最迟钝：enter 0.75 / exit 0.55（强噪环境防误触发）
	VADSensLow     = 1 // 迟钝：enter 0.70 / exit 0.50
	VADSensMid     = 2 // 默认：enter 0.60 / exit 0.40（DefaultDSPConfig 校准值）
	VADSensHigh    = 3 // 灵敏：enter 0.50 / exit 0.30
	VADSensHighest = 4 // 最灵敏：enter 0.45 / exit 0.25（轻声说话也能触发）
)

// SetVADSensitivity 按档位调整 VAD 双门限（原子写，回差恒 ≥0.2）。
// 非法档位按 VADSensMid 处理。
func (d *DSP) SetVADSensitivity(level int) {
	var enter, exit float32
	switch level {
	case VADSensLowest:
		enter, exit = 0.75, 0.55
	case VADSensLow:
		enter, exit = 0.70, 0.50
	case VADSensHigh:
		enter, exit = 0.50, 0.30
	case VADSensHighest:
		enter, exit = 0.45, 0.25
	default:
		enter, exit = 0.60, 0.40
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

// AudioLevel 返回最近一帧的 RFC 6464 电平（0=0dB … 127=静音，#33）。
func (d *DSP) AudioLevel() uint8 { return uint8(d.levelBits.Load()) }

// Process 处理一个 480 采样（10ms）帧：前置增益 → 降噪 → AGC → 固定增益 → RMS/VAD 说话判定。
// out 与 in 长度都必须 >= FrameSize 且不得重叠；返回 VAD 概率（降噪关闭时为 0）。
// 全程在预分配缓冲上操作，零堆分配。
func (d *DSP) Process(out, in []int16) (float32, error) {
	// 纯零/近零帧短路：跳过 RNNoise。设备静音时输入为精确 0，RNNoise 内部的
	// 输入高通 IIR 状态会指数衰减进 float32 次正规数区间并长期停留，x86 上走
	// 微码慢路径，单帧耗时翻数倍（实测 181µs → 250~413µs）。短路后整个静音期
	// 不再触碰降噪器状态，denormal 只是静止数据，没有 CPU 代价；连续静音
	// 500ms 再 Reset 一次，把残留状态清干净，避免下次说话首帧被拖慢。
	// 与完整路径副作用一致：输出全零、vad=0、说话状态机按零 RMS 推进
	// （vad/rms 传 0 与降噪器对纯零输入的输出在门限语义上等价）、AGC 增益冻结。
	if peakAbs(in[:FrameSize]) <= 1 {
		clear(out[:FrameSize])
		d.silentRun++
		if d.silentRun == 50 {
			// Reset 就地清零全部滤波状态（含高通 biquad 的 memHP），零分配；
			// 若恰逢 SetDenoiseStrength 刚替换实例，Reset 落在旧实例上亦无碍。
			d.denoiserPtr.Load().Reset()
		}
		d.updateSpeaking(0, 0)
		return 0, nil
	}
	d.silentRun = 0

	// 底噪观察：取原始输入（pre-gain 之前）的 RMS——门限以设备电平为基准，
	// 前置/输出增益让信号相对固定门限更容易过线。数字静音帧已在上方短路，
	// 走到这里必有真实信号。
	d.noise.observe(rmsInt16(in[:FrameSize]))

	// 前置增益（RNNoise 之前）：弱麦先抬进降噪器/VAD 的有效区。写入预分配
	// scratch（不改调用方的 in）；g==1 时跳过，保持原零拷贝路径。
	src := in[:FrameSize]
	if g := math.Float64frombits(d.preGainBits.Load()); g != 1 {
		applyGainInto(d.preBuf[:FrameSize], in[:FrameSize], g)
		src = d.preBuf[:FrameSize]
	}

	var vad float32
	var err error
	if d.denoiseOn.Load() {
		dn := d.denoiserPtr.Load()
		vad, err = dn.ProcessInt16(out[:FrameSize], src)
		if err != nil {
			return 0, err
		}
	} else {
		copy(out[:FrameSize], src)
	}
	// AGC（自动增益）：以 RMS 目标迭代自适应，插在固定增益前（v1.1 默认开）。
	// 仅在门控开启的帧迭代——静音/底噪期增益冻结，否则安静段会在 ~150ms 内
	// 把增益冲到上限，语音起始瞬间被 10× 放大产生爆音（review S3）。
	if d.agcOn.Load() {
		frameRMS := rmsInt16(out[:FrameSize])
		if d.agcGate(vad, frameRMS) {
			d.agcGain = d.nextAGCGain(frameRMS)
		}
		applyGain(out[:FrameSize], d.agcGain)
	}
	if g := d.Gain(); g != 1.0 {
		applyGain(out[:FrameSize], g)
	}
	d.updateSpeaking(vad, rmsInt16(out[:FrameSize]))
	return vad, nil
}

// agcGate 判定当前帧是否参与 AGC 迭代（v1.1 重做）：「VAD 或 RMS 明显高于
// 底噪（+6dB）」——不再纯靠 VAD（弱信号 VAD 低 → AGC 永不迭代的死循环）。
// 降噪关闭（无 VAD）时保留 v0.1.2 的绝对地板（target×0.2）兜底。
func (d *DSP) agcGate(vad float32, rms float64) bool {
	if rms > dbToAmp(d.noise.floorDB()+exitOffsetDB) {
		return true
	}
	if d.denoiseOn.Load() {
		return vad > d.VADExit() || d.speaking.Load()
	}
	return rms >= agcTargetRMS*agcFloorRatio
}

// nextAGCGain 依当前帧 RMS 迭代 AGC 增益：低于目标慢升（attack 0.3），
// 高于目标快降（release 0.8），限幅 [agcGainMin, agcGainMax]。仅 Process goroutine 调用。
func (d *DSP) nextAGCGain(rms float64) float64 {
	g := d.agcGain
	if g <= 0 {
		g = agcInitGain
	}
	switch {
	case rms < agcTargetRMS*0.8:
		g *= 1 + agcAttack*(agcTargetRMS*0.8-rms)/agcTargetRMS
		if g > agcGainMax {
			g = agcGainMax
		}
	case rms > agcTargetRMS*1.2:
		g *= 1 - agcRelease*(rms-agcTargetRMS*1.2)/agcTargetRMS
		if g < agcGainMin {
			g = agcGainMin
		}
	}
	return g
}

// rmsToLevel 帧 RMS（int16 域）→ RFC 6464 电平（0=0dB 满幅 … 127=静音）。
// 只做标量换算：RMS 是 updateSpeaking 已算好的量，不在这里二次遍历帧缓冲。
func rmsToLevel(rms float64) uint8 {
	if rms <= 0 {
		return 127
	}
	l := int(math.Round(-20 * math.Log10(rms/32767.0)))
	if l < 0 {
		return 0
	}
	if l > 127 {
		return 127
	}
	return uint8(l)
}

// updateSpeaking 双门限 + 迟滞帧数的状态机（v1.1：RMS 门限自适应底噪）。
// 仅处理 goroutine 调用。顺带把换算后的 RFC 6464 电平写入 levelBits（#33）：
// SendOpus 读它写 RTP 头扩展，电平与说话判定共用同一次 RMS，零新增计算。
func (d *DSP) updateSpeaking(vad float32, rms float64) {
	d.levelBits.Store(uint32(rmsToLevel(rms)))
	vadAvailable := d.denoiseOn.Load()
	speaking := d.speaking.Load()
	enterLin := dbToAmp(enterThresholdDB(d.noise.floorDB()))
	exitLin := dbToAmp(exitThresholdDB(d.noise.floorDB()))

	if !speaking {
		enterOK := rms >= enterLin && (!vadAvailable || vad > d.VADEnter())
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

	exitNow := rms < exitLin || (vadAvailable && vad < d.VADExit())
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
func applyGain(p []int16, g float64) { applyGainInto(p, p, g) }

// applyGainInto src×g → dst（就地安全：允许 dst==src），截断到 int16。
func applyGainInto(dst, src []int16, g float64) {
	for i, s := range src {
		dst[i] = clampInt16(float64(s) * g)
	}
}

// peakAbs 返回帧内绝对值峰值（零分配）。短路判定用：只有 |sample| <= 1 的
// 帧才值得走零输出捷径，阈值取 1 是"数字零"与"模拟底噪"的分界。
func peakAbs(p []int16) int32 {
	var peak int32
	for _, s := range p {
		v := int32(s)
		if v < 0 {
			v = -v
		}
		if v > peak {
			peak = v
		}
	}
	return peak
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
