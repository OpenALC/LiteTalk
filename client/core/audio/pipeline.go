package audio

import (
	"errors"
	"fmt"
	"math"
	"sync"
	"sync/atomic"
	"time"

	"github.com/OpenALC/LiteTalk/client/core/codec"
	"github.com/OpenALC/LiteTalk/client/core/hotkey"
)

// Backend 选择管线的音频来源/去向。
type Backend int

const (
	// BackendDevice 使用真实声卡（malgo 采集 + 播放）。
	BackendDevice Backend = iota
	// BackendNull 无声卡自检：10ms 节拍器驱动合成信号源，播放侧排空计数。
	BackendNull
)

// PipelineConfig 管线配置。建议从 DefaultPipelineConfig 出发修改。
type PipelineConfig struct {
	Backend  Backend
	Loopback bool // 编码后本地直接解码送播放环（M1 回环试听；M2 改为 OnPacket 发出）

	CaptureSelector  string    // 采集设备 ID 十六进制串或名称子串，空 = 默认
	PlaybackSelector string    // 播放设备同上
	InputMode        InputMode // 采集下混模式（零值 = auto；RuntimeSettings 可热改）

	DSP DSPConfig // 降噪/增益/说话判定

	// Opus 编码参数（零值回落 ResolveOpusConfig 默认；运行时可用 setter 热调）。
	Opus OpusConfig

	// OnPacket 每个 Opus 包编码完成时回调（M2 对接 RTP 发送的接口）。
	// 注意：pkt 底层缓冲随下一帧复用，需要保留必须自行拷贝。
	OnPacket func(pkt []byte)
	// OnLatency 回环模式下的端到端延迟估计回调（节流 ~200ms）。
	OnLatency func(d time.Duration)
}

// OpusConfig Opus 编码参数。零值字段由 ResolveOpusConfig 回落为管线默认值
// （32kbps / VBR / DTX / FEC 开，丢包率告知 10%）。
type OpusConfig struct {
	BitrateKbps int   // 目标码率 kbps（16/24/32/48/64；0 → 32）
	VBR         *bool // 可变码率（nil → true）
	DTX         *bool // 静音期非连续传输（nil → true）
	FEC         *bool // 带内前向纠错（nil → true）
	PacketLoss  int   // 告知编码器的预期丢包率 %（0 → 10；FEC 关闭时无意义）
}

// DefaultOpusConfig 管线默认 Opus 参数（与既有硬编码等价）。
func DefaultOpusConfig() OpusConfig {
	return OpusConfig{BitrateKbps: 32, VBR: boolPtr(true), DTX: boolPtr(true), FEC: boolPtr(true), PacketLoss: 10}
}

// ResolveOpusConfig 把零值字段回落为默认值，返回可直接应用的有效配置。
func ResolveOpusConfig(c OpusConfig) OpusConfig {
	d := DefaultOpusConfig()
	if c.BitrateKbps <= 0 {
		c.BitrateKbps = d.BitrateKbps
	}
	if c.VBR == nil {
		c.VBR = d.VBR
	}
	if c.DTX == nil {
		c.DTX = d.DTX
	}
	if c.FEC == nil {
		c.FEC = d.FEC
	}
	if c.PacketLoss <= 0 {
		c.PacketLoss = d.PacketLoss
	}
	return c
}

func boolPtr(b bool) *bool { return &b }

// RuntimeSettings 应用层下发的运行时音频设置（Opus 须先经 ResolveOpusConfig）。
// 由 Session.ApplyAudio 一次性热应用到管线。v0.1.2 扩展复杂度/降噪强度/AGC/总音量。
type RuntimeSettings struct {
	Opus            OpusConfig
	Denoise         bool
	DenoiseStrength int       // 1弱/2中/3强（v0.1.2）
	AGC             bool      // 自动增益（v1.1 默认开）
	Gain            float64   // 麦克风线性增益，1.0 为不变
	PlaybackGain    float64   // 扬声器总音量（mixer master gain），1.0 为不变
	VADSens         int       // 0–4 五档
	Complexity      int       // Opus 复杂度 0–10（≤0 表示不改）
	InputMode       InputMode // 采集下混模式（变更即热重启采集流）
	PreGainDB       float64   // 前置增益 dB（-12~+24，RNNoise 之前；0=不变）
}

// DefaultPipelineConfig 返回默认管线配置（声卡回环、降噪开、增益 1）。
func DefaultPipelineConfig() PipelineConfig {
	return PipelineConfig{
		Backend:  BackendDevice,
		Loopback: true,
		DSP:      DefaultDSPConfig(),
	}
}

// Stats 运行统计，全部原子计数，热路径无锁。
type Stats struct {
	CaptureCallbacks  atomic.Uint64 // 采集回调次数
	CaptureSamples    atomic.Uint64 // 实际写入采集环的采样数
	CaptureDropped    atomic.Uint64 // 采集环满丢弃的采样数
	FramesProcessed   atomic.Uint64 // 过 DSP 的 10ms 帧数
	PacketsEncoded    atomic.Uint64 // 编码出的 Opus 包数（20ms）
	PacketsDecoded    atomic.Uint64 // 回环解码的包数
	BytesEncoded      atomic.Uint64 // 编码总字节数
	EncodeErrors      atomic.Uint64
	DecodeErrors      atomic.Uint64
	PlaybackCallbacks atomic.Uint64 // 播放回调次数
	PlaybackSamples   atomic.Uint64 // 实际播出的采样数（null 模式 = 排空数）
	PlaybackUnderruns atomic.Uint64 // 播放欠载次数（补静音）
	PlaybackDropped   atomic.Uint64 // 播放环满丢弃的采样数
	SpeakingChanges   atomic.Uint64 // 说话状态翻转次数
	PTTDropped        atomic.Uint64 // 按键说话发送门拦下的包数（20ms）
	LatencyCount      atomic.Uint64 // 延迟测量样本数
	LatencySumUS      atomic.Uint64 // 延迟累计（微秒）
	LatencyMaxUS      atomic.Uint64 // 延迟峰值（微秒）
}

// StatsSnapshot 是 Stats 的某一时刻快照（纯值，便于打印/上报）。
type StatsSnapshot struct {
	CaptureCallbacks  uint64
	CaptureSamples    uint64
	CaptureDropped    uint64
	FramesProcessed   uint64
	PacketsEncoded    uint64
	PacketsDecoded    uint64
	BytesEncoded      uint64
	EncodeErrors      uint64
	DecodeErrors      uint64
	PlaybackCallbacks uint64
	PlaybackSamples   uint64
	PlaybackUnderruns uint64
	PlaybackDropped   uint64
	SpeakingChanges   uint64
	PTTDropped        uint64
	AvgPacketBytes    float64
	LatencyCount      uint64
	LatencyAvgUS      uint64
	LatencyMaxUS      uint64
}

// Snapshot 读取一致性的统计快照。
func (s *Stats) Snapshot() StatsSnapshot {
	pkts := s.PacketsEncoded.Load()
	bytes := s.BytesEncoded.Load()
	var avg float64
	if pkts > 0 {
		avg = float64(bytes) / float64(pkts)
	}
	latCnt := s.LatencyCount.Load()
	latSum := s.LatencySumUS.Load()
	var latAvg uint64
	if latCnt > 0 {
		latAvg = latSum / latCnt
	}
	return StatsSnapshot{
		CaptureCallbacks:  s.CaptureCallbacks.Load(),
		CaptureSamples:    s.CaptureSamples.Load(),
		CaptureDropped:    s.CaptureDropped.Load(),
		FramesProcessed:   s.FramesProcessed.Load(),
		PacketsEncoded:    pkts,
		PacketsDecoded:    s.PacketsDecoded.Load(),
		BytesEncoded:      bytes,
		EncodeErrors:      s.EncodeErrors.Load(),
		DecodeErrors:      s.DecodeErrors.Load(),
		PlaybackCallbacks: s.PlaybackCallbacks.Load(),
		PlaybackSamples:   s.PlaybackSamples.Load(),
		PlaybackUnderruns: s.PlaybackUnderruns.Load(),
		PlaybackDropped:   s.PlaybackDropped.Load(),
		SpeakingChanges:   s.SpeakingChanges.Load(),
		PTTDropped:        s.PTTDropped.Load(),
		AvgPacketBytes:    avg,
		LatencyCount:      latCnt,
		LatencyAvgUS:      latAvg,
		LatencyMaxUS:      s.LatencyMaxUS.Load(),
	}
}

// Pipeline 音频管线：采集 → (10ms 帧) DSP → (2 帧拼 20ms) Opus 编码 →
// 回环模式本地解码 → 播放。M2 时编码结果改经 OnPacket 送 RTP。
//
// 使用方式：
//   - 常规：NewPipeline → Start → Stop/Close；
//   - 算法链路测试/离线处理：NewPipeline 后直接 ProcessPCM 注入（不要 Start）。
type Pipeline struct {
	cfg      PipelineConfig
	opus     OpusConfig // 已解析的生效 Opus 配置（setter 同步维护）
	capRing  *RingBuffer
	playRing *RingBuffer
	dsp      *DSP
	enc      *codec.Encoder
	dec      *codec.Decoder
	encMu    sync.Mutex // 保护 enc：opus_encoder_ctl 不得并发 opus_encode
	stats    Stats

	ctx      *Context
	capture  *Capture
	playback *Playback

	devMu sync.Mutex // 保护 capture/playback 字段（RestartDevices 与 captureLoop.wake 并发）

	// restartMu 串行化设备流重启（RestartDevices 与 SetInputMode 的热重启可能
	// 并发：交错会把对方刚打开的设备流覆盖泄漏）。
	restartMu sync.Mutex

	// inputMode 当前下混模式（原子热调；StartCapture/热重启读取）。
	inputMode atomic.Uint32

	// sendGate 按键说话发送门（PTT 按住 / 语音激活 / 常开）。nil = 不设门，
	// 恒放行。原子指针：处理 goroutine 每帧 Load，SetSendGate 可在任意
	// goroutine 调用（值对象长期存活，跨会话复用不会泄漏）。
	sendGate atomic.Pointer[hotkey.Gate]

	done    chan struct{}
	wg      sync.WaitGroup
	started atomic.Bool
	closed  atomic.Bool

	// ---- 处理 goroutine 私有的预分配缓冲（热路径零分配）----
	frame      [FrameSize]int16
	dspOut     [FrameSize]int16
	opusFrame  [OpusFrameSize]int16
	opusHalf   int // 已积攒的 10ms 半帧数（0/1）
	pktBuf     [4000]byte
	decBuf     [OpusFrameSize]int16
	drainBuf   [OpusFrameSize]int16 // null 模式排空用
	frameStart time.Time            // 当前 20ms 帧首半帧进入管线的时刻
	lastLatRep time.Time
	nullRNG    uint32
	nullPhase  float64
}

// NewPipeline 构建管线（不启动任何流/线程）。
// 在非 Windows 平台（codec stub）返回 codec.ErrUnsupportedPlatform。
func NewPipeline(cfg PipelineConfig) (*Pipeline, error) {
	if !codec.Supported() {
		return nil, codec.ErrUnsupportedPlatform
	}

	userSpeakCB := cfg.DSP.OnSpeakingChange
	p := &Pipeline{
		cfg:      cfg,
		capRing:  NewRingBuffer(16384), // ≈341ms > 硬约束 200ms
		playRing: NewRingBuffer(16384),
		done:     make(chan struct{}),
		nullRNG:  0x1EAF1234,
	}
	if cfg.InputMode > InputModeMix {
		cfg.InputMode = InputModeAuto
		p.cfg.InputMode = InputModeAuto
	}
	p.inputMode.Store(uint32(cfg.InputMode))

	cfg.DSP.OnSpeakingChange = func(b bool) {
		// PTT 门关闭时同步压掉说话态：这一帧的发包已被发送门拦下，UI 的
		// 声波球与远端说话状态不能再亮，否则出现「包没发却在说话」。
		// 语义与 session 的静音压制一致（muted 时同样上报 false）。
		if g := p.sendGate.Load(); g != nil && !g.Allow(b) {
			b = false
		}
		p.stats.SpeakingChanges.Add(1)
		if userSpeakCB != nil {
			userSpeakCB(b)
		}
	}
	dsp, err := NewDSP(cfg.DSP)
	if err != nil {
		return nil, fmt.Errorf("init dsp: %w", err)
	}
	p.dsp = dsp

	enc, err := codec.NewEncoder(SampleRate, 1, codec.AppVoIP)
	if err != nil {
		return nil, fmt.Errorf("init opus encoder: %w", err)
	}
	p.opus = ResolveOpusConfig(cfg.Opus)
	if err := p.applyOpusConfig(enc, p.opus); err != nil {
		enc.Close()
		return nil, fmt.Errorf("opus encoder 参数: %w", err)
	}
	p.enc = enc

	if cfg.Loopback {
		dec, err := codec.NewDecoder(SampleRate, 1)
		if err != nil {
			enc.Close()
			return nil, fmt.Errorf("init opus decoder: %w", err)
		}
		p.dec = dec
	}
	return p, nil
}

// Start 按配置启动采集/播放流与处理 goroutine。幂等：已启动时重复调用是
// 无害 no-op（返回 nil）；Stop 之后可再次 Start 完整重启。已 Close 报错。
// 生命周期状态机：未启动 →(Start)→ 已启动 →(Stop)→ 未启动 →(Close)→ 已关闭。
func (p *Pipeline) Start() error {
	if p.closed.Load() {
		return errors.New("pipeline closed")
	}
	if !p.started.CompareAndSwap(false, true) {
		return nil // 已启动：no-op
	}

	// done 每次启动新建：Stop 关闭后不可复用，重启需要一条新的通知通道。
	p.done = make(chan struct{})
	if p.cfg.Backend == BackendNull {
		p.wg.Add(1)
		go p.nullLoop()
		return nil
	}

	ctx, err := NewContext(nil)
	if err != nil {
		p.started.Store(false)
		return err
	}
	p.ctx = ctx

	cap_, err := StartCapture(ctx, p.cfg.CaptureSelector, p.capRing, &p.stats, InputMode(p.inputMode.Load()))
	if err != nil {
		p.ctx.Close()
		p.started.Store(false)
		return err
	}
	p.capture = cap_

	play, err := StartPlayback(ctx, p.cfg.PlaybackSelector, p.playRing, &p.stats)
	if err != nil {
		p.capture.Stop()
		p.ctx.Close()
		p.started.Store(false)
		return err
	}
	p.playback = play

	p.wg.Add(1)
	go p.captureLoop(p.capture.Wake())
	return nil
}

// RestartDevices 热切换采集/播放设备：停掉当前设备流并按新 selector 重启，
// 编码器/解码器/混音器/处理 goroutine 状态全部保留。selector 语义同
// StartCapture/StartPlayback（空 = 默认设备；失败自动回退默认）。
// 仅对 BackendDevice 有效；未 Start 时仅更新配置待下次 Start 生效。
func (p *Pipeline) RestartDevices(captureSelector, playbackSelector string) error {
	if p.closed.Load() {
		return errors.New("pipeline closed")
	}
	p.restartMu.Lock()
	defer p.restartMu.Unlock()
	p.cfg.CaptureSelector = captureSelector
	p.cfg.PlaybackSelector = playbackSelector
	if !p.started.Load() {
		return nil // 尚未启动：仅记录选择器
	}
	if p.cfg.Backend != BackendDevice {
		return errors.New("device restart only supported with BackendDevice")
	}

	// 停旧设备流（不动 done/captureLoop/codec/mixer）。
	p.devMu.Lock()
	oldCap, oldPlay := p.capture, p.playback
	p.capture, p.playback = nil, nil
	p.devMu.Unlock()
	if oldCap != nil {
		oldCap.Stop()
	}
	if oldPlay != nil {
		oldPlay.Stop()
	}

	cap_, err := StartCapture(p.ctx, captureSelector, p.capRing, &p.stats, InputMode(p.inputMode.Load()))
	if err != nil && captureSelector != "" {
		// 设备消失/失效：回退默认设备。
		p.cfg.CaptureSelector = ""
		cap_, err = StartCapture(p.ctx, "", p.capRing, &p.stats, InputMode(p.inputMode.Load()))
	}
	if err != nil {
		return fmt.Errorf("restart capture: %w", err)
	}

	play, err := StartPlayback(p.ctx, playbackSelector, p.playRing, &p.stats)
	if err != nil && playbackSelector != "" {
		p.cfg.PlaybackSelector = ""
		play, err = StartPlayback(p.ctx, "", p.playRing, &p.stats)
	}
	if err != nil {
		cap_.Stop()
		return fmt.Errorf("restart playback: %w", err)
	}

	p.devMu.Lock()
	p.capture, p.playback = cap_, play
	p.devMu.Unlock()
	return nil
}

// SetInputMode 运行时切换下混模式：原子写 + 热重启采集流（播放/编解码不动），
// 让设置页改动即时生效。未启动/空卡后端仅记录，下次 Start 生效。
func (p *Pipeline) SetInputMode(m InputMode) error {
	if m > InputModeMix {
		return fmt.Errorf("输入声道模式非法: %v", uint32(m))
	}
	if uint32(m) == p.inputMode.Swap(uint32(m)) {
		return nil // 未变化：no-op
	}
	if !p.started.Load() || p.cfg.Backend != BackendDevice || p.ctx == nil {
		return nil
	}
	p.restartMu.Lock()
	defer p.restartMu.Unlock()
	return p.restartCaptureLocked()
}

// restartCaptureLocked 仅重启采集流（已持 restartMu）。
// 失败语义与 RestartDevices 一致：capture 置 nil，由调用方向上透传错误。
func (p *Pipeline) restartCaptureLocked() error {
	p.devMu.Lock()
	oldCap := p.capture
	p.capture = nil
	p.devMu.Unlock()
	if oldCap != nil {
		oldCap.Stop()
	}
	cap_, err := StartCapture(p.ctx, p.cfg.CaptureSelector, p.capRing, &p.stats, InputMode(p.inputMode.Load()))
	if err != nil && p.cfg.CaptureSelector != "" {
		p.cfg.CaptureSelector = ""
		cap_, err = StartCapture(p.ctx, "", p.capRing, &p.stats, InputMode(p.inputMode.Load()))
	}
	if err != nil {
		return fmt.Errorf("restart capture: %w", err)
	}
	p.devMu.Lock()
	p.capture = cap_
	p.devMu.Unlock()
	return nil
}

// Stop 停止流与 goroutine（幂等，重复调用是无害 no-op）。codec 资源由 Close 释放。
func (p *Pipeline) Stop() {
	if p.started.CompareAndSwap(true, false) {
		close(p.done)
		p.wg.Wait()
	}
	if p.capture != nil {
		p.capture.Stop()
		p.capture = nil
	}
	if p.playback != nil {
		p.playback.Stop()
		p.playback = nil
	}
	if p.ctx != nil {
		p.ctx.Close()
		p.ctx = nil
	}
}

// Close 停止管线并释放 codec 资源。幂等。
// 释放编码器须持 encMu：运行时 setter（SetOpus*）可能正卡在 ctl 内，
// 并发 destroy 同一个 OpusEncoder* 是 C 堆 use-after-free。
func (p *Pipeline) Close() error {
	if !p.closed.CompareAndSwap(false, true) {
		return nil
	}
	p.Stop()
	p.encMu.Lock()
	if p.enc != nil {
		_ = p.enc.Close()
		p.enc = nil // 让 setter 的 nil 检查生效（返回 ErrClosed）
	}
	p.encMu.Unlock()
	if p.dec != nil {
		_ = p.dec.Close()
		p.dec = nil
	}
	return nil
}

// Stats 返回运行统计快照。
func (p *Pipeline) Stats() StatsSnapshot { return p.stats.Snapshot() }

// PlaybackBuffered 播放环当前排队时长（诊断用，2s 统计节拍读取，非热路径）。
func (p *Pipeline) PlaybackBuffered() time.Duration { return p.playRing.BufferedDuration() }

// Backend 返回构造时选定的音频后端（只读，诊断/测试用）。
func (p *Pipeline) Backend() Backend { return p.cfg.Backend }

// SetGain 运行时调整增益。
func (p *Pipeline) SetGain(g float64) { p.dsp.SetGain(g) }

// SetDenoise 运行时开关降噪。
func (p *Pipeline) SetDenoise(on bool) { p.dsp.SetDenoise(on) }

// SetVADSensitivity 运行时调整 VAD 灵敏度档位（低/中/高）。
func (p *Pipeline) SetVADSensitivity(level int) { p.dsp.SetVADSensitivity(level) }

// SetDenoiseStrength 运行时切换降噪强度档（v0.1.2；重建 Denoiser 原子替换）。
func (p *Pipeline) SetDenoiseStrength(level int) error { return p.dsp.SetDenoiseStrength(level) }

// SetAGC 运行时开关自动增益（v1.1 默认开）。
func (p *Pipeline) SetAGC(on bool) { p.dsp.SetAGC(on) }

// SetPreGainDB 运行时调整前置增益（-12~+24dB，越界报错；原子热生效）。
func (p *Pipeline) SetPreGainDB(db float64) error { return p.dsp.SetPreGainDB(db) }

// SetComplexity 热调 Opus 复杂度 0–10（v0.1.2；编码中调用安全，内部持锁）。
func (p *Pipeline) SetComplexity(v int) error {
	if v < 0 || v > 10 {
		return fmt.Errorf("复杂度非法: %d", v)
	}
	p.encMu.Lock()
	defer p.encMu.Unlock()
	if p.enc == nil {
		return codec.ErrClosed
	}
	return p.enc.SetComplexity(v)
}

// applyOpusConfig 把已解析的 Opus 配置刷到编码器（持锁，可在运行中调用）。
func (p *Pipeline) applyOpusConfig(enc *codec.Encoder, c OpusConfig) error {
	for name, fn := range map[string]func() error{
		"SetBitrate":        func() error { return enc.SetBitrate(c.BitrateKbps * 1000) },
		"SetVBR":            func() error { return enc.SetVBR(*c.VBR) },
		"SetDTX":            func() error { return enc.SetDTX(*c.DTX) },
		"SetInBandFEC":      func() error { return enc.SetInBandFEC(*c.FEC) },
		"SetPacketLossPerc": func() error { return enc.SetPacketLossPerc(c.PacketLoss) },
	} {
		if err := fn(); err != nil {
			return fmt.Errorf("%s: %w", name, err)
		}
	}
	return nil
}

// SetOpusBitrate 热调码率（kbps）。编码中调用安全（内部持锁）。
func (p *Pipeline) SetOpusBitrate(kbps int) error {
	if kbps <= 0 {
		return fmt.Errorf("码率非法: %d", kbps)
	}
	p.encMu.Lock()
	defer p.encMu.Unlock()
	if p.enc == nil {
		return codec.ErrClosed
	}
	if err := p.enc.SetBitrate(kbps * 1000); err != nil {
		return err
	}
	p.opus.BitrateKbps = kbps
	return nil
}

// SetOpusVBR 热调可变码率开关。
func (p *Pipeline) SetOpusVBR(on bool) error {
	p.encMu.Lock()
	defer p.encMu.Unlock()
	if p.enc == nil {
		return codec.ErrClosed
	}
	if err := p.enc.SetVBR(on); err != nil {
		return err
	}
	p.opus.VBR = boolPtr(on)
	return nil
}

// SetOpusDTX 热调静音期非连续传输开关。
func (p *Pipeline) SetOpusDTX(on bool) error {
	p.encMu.Lock()
	defer p.encMu.Unlock()
	if p.enc == nil {
		return codec.ErrClosed
	}
	if err := p.enc.SetDTX(on); err != nil {
		return err
	}
	p.opus.DTX = boolPtr(on)
	return nil
}

// SetOpusFEC 热调带内前向纠错开关。
func (p *Pipeline) SetOpusFEC(on bool) error {
	p.encMu.Lock()
	defer p.encMu.Unlock()
	if p.enc == nil {
		return codec.ErrClosed
	}
	if err := p.enc.SetInBandFEC(on); err != nil {
		return err
	}
	p.opus.FEC = boolPtr(on)
	return nil
}

// SetOpusPacketLoss 热调告知编码器的预期丢包率 %（5–20）。
func (p *Pipeline) SetOpusPacketLoss(pct int) error {
	if pct < 0 || pct > 100 {
		return fmt.Errorf("丢包率非法: %d", pct)
	}
	p.encMu.Lock()
	defer p.encMu.Unlock()
	if p.enc == nil {
		return codec.ErrClosed
	}
	if err := p.enc.SetPacketLossPerc(pct); err != nil {
		return err
	}
	p.opus.PacketLoss = pct
	return nil
}

// Speaking 当前说话状态。
func (p *Pipeline) Speaking() bool { return p.dsp.Speaking() }

// AudioLevel 最近一帧的 RFC 6464 电平（0=0dB … 127=静音，#33）。
// SendOpus 每包读取写入 RTP 头扩展；跨 goroutine 无锁（atomic）。
func (p *Pipeline) AudioLevel() uint8 { return p.dsp.AudioLevel() }

// SetSendGate 设置按键说话发送门（传 nil 清除）。Gate 是零分配的值对象，
// 可在会话建立时注入一次、长期复用；热路径只做一次原子指针 Load。
func (p *Pipeline) SetSendGate(g *hotkey.Gate) { p.sendGate.Store(g) }

// ReadPlayback 从播放环读出解码后的采样（测试/null 汇用），返回采样数。
func (p *Pipeline) ReadPlayback(out []int16) int { return p.playRing.Read(out) }

// WritePlayback 向播放环写入外部混音后的采样（M2 混音器出口；Loopback=false
// 时播放环无其他写者）。返回实际写入数，环满短写部分计入 PlaybackDropped。
func (p *Pipeline) WritePlayback(pcm []int16) int {
	n := p.playRing.Write(pcm)
	if d := len(pcm) - n; d > 0 {
		p.stats.PlaybackDropped.Add(uint64(d))
	}
	return n
}

// ProcessPCM 同步注入 PCM（长度须为 480 的整数倍）走完 DSP→编码→(回环)解码
// 全链路，不经过采集设备与采集环。用于算法链路测试与离线处理。
// 不得与 Start 并发使用。
func (p *Pipeline) ProcessPCM(pcm []int16) error {
	if len(pcm)%FrameSize != 0 {
		return fmt.Errorf("ProcessPCM: length %d not a multiple of %d", len(pcm), FrameSize)
	}
	for len(pcm) > 0 {
		p.processFrame(pcm[:FrameSize])
		pcm = pcm[FrameSize:]
	}
	return nil
}

// captureLoop 声卡模式处理循环：被采集回调唤醒，按 10ms 帧消费采集环。
// wake 每轮动态取当前设备的唤醒通道（RestartDevices 热切换后指向新设备）。
func (p *Pipeline) captureLoop(_ <-chan struct{}) {
	defer p.wg.Done()
	timer := time.NewTimer(20 * time.Millisecond)
	defer timer.Stop()
	for {
		timer.Reset(20 * time.Millisecond)
		wake := p.currentWake()
		select {
		case <-p.done:
			return
		case <-wake:
		case <-timer.C: // 兜底节拍：唤醒信号合并丢失时也能前进
		}
		for p.capRing.Available() >= FrameSize {
			p.capRing.Read(p.frame[:])
			p.processFrame(p.frame[:])
		}
	}
}

// currentWake 返回当前采集设备的唤醒通道（设备未就绪时返回 nil 通道，
// select 将永久阻塞该分支，由兜底节拍驱动）。
func (p *Pipeline) currentWake() <-chan struct{} {
	p.devMu.Lock()
	defer p.devMu.Unlock()
	if p.capture == nil {
		return nil
	}
	return p.capture.Wake()
}

// nullLoop 无声卡自检循环：10ms 节拍合成 440Hz 正弦+轻噪声帧注入管线，
// 播放侧每拍排空计数，等价走完除 malgo 之外的全部路径。
func (p *Pipeline) nullLoop() {
	defer p.wg.Done()
	tick := time.NewTicker(10 * time.Millisecond)
	defer tick.Stop()
	for {
		select {
		case <-p.done:
			return
		case <-tick.C:
		}
		p.synthFrame(p.frame[:])
		p.stats.CaptureCallbacks.Add(1)
		p.stats.CaptureSamples.Add(FrameSize)
		p.processFrame(p.frame[:])
		if n := p.playRing.Read(p.drainBuf[:]); n > 0 {
			p.stats.PlaybackCallbacks.Add(1)
			p.stats.PlaybackSamples.Add(uint64(n))
		}
	}
}

// synthFrame 生成一帧合成测试信号（440Hz 正弦 amp 12000 + ±1000 噪声）。
func (p *Pipeline) synthFrame(dst []int16) {
	for i := range dst {
		p.nullPhase += 2 * math.Pi * 440 / SampleRate
		p.nullRNG ^= p.nullRNG << 13
		p.nullRNG ^= p.nullRNG >> 17
		p.nullRNG ^= p.nullRNG << 5
		noise := int32(p.nullRNG>>16)%2001 - 1000
		dst[i] = int16(12000*math.Sin(p.nullPhase) + float64(noise))
	}
}

// processFrame 单个 10ms 帧的核心链路：DSP → 拼 20ms → 编码 → 发包门 →
// 回环解码 → 播放环。全部使用预分配缓冲，零堆分配。仅处理 goroutine 调用。
func (p *Pipeline) processFrame(frame []int16) {
	if p.opusHalf == 0 {
		p.frameStart = time.Now()
	}
	if _, err := p.dsp.Process(p.dspOut[:], frame); err != nil {
		p.stats.EncodeErrors.Add(1)
		return
	}
	p.stats.FramesProcessed.Add(1)

	copy(p.opusFrame[p.opusHalf*FrameSize:(p.opusHalf+1)*FrameSize], p.dspOut[:])
	p.opusHalf++
	if p.opusHalf < 2 {
		return
	}
	p.opusHalf = 0

	n, err := func() (int, error) {
		p.encMu.Lock()
		defer p.encMu.Unlock()
		return p.enc.Encode(p.opusFrame[:], p.pktBuf[:])
	}()
	if err != nil {
		p.stats.EncodeErrors.Add(1)
		return
	}
	p.stats.PacketsEncoded.Add(1)
	p.stats.BytesEncoded.Add(uint64(n))

	// 发包门：发送 = !muted && (gate == nil || gate.Allow(speaking))。
	// muted 门在 rtc/peer.go（SendOpus）那一侧，此处只做按键说话门控。
	// speaking 取 DSP 本帧判定（驱动 OnSpeakingChange 的同一个量，见
	// dsp.updateSpeaking），不重算；门关时计数丢弃并整帧短路，回环试听
	// 一并停掉——没发出去的声音也不该回放。
	if g := p.sendGate.Load(); g != nil && !g.Allow(p.dsp.Speaking()) {
		p.stats.PTTDropped.Add(1)
		return
	}

	if p.cfg.OnPacket != nil {
		p.cfg.OnPacket(p.pktBuf[:n])
	}

	if p.cfg.Loopback && p.dec != nil {
		m, err := p.dec.Decode(p.pktBuf[:n], p.decBuf[:], false)
		if err != nil {
			p.stats.DecodeErrors.Add(1)
			return
		}
		p.stats.PacketsDecoded.Add(1)
		p.recordLatency() // 在写入前统计：估计新帧需在播放环中排队的时长
		if w := p.playRing.Write(p.decBuf[:m]); w < m {
			p.stats.PlaybackDropped.Add(uint64(m - w))
		}
	}
}

// recordLatency 回环端到端延迟估计：处理耗时 + 播放环排队时长。
// 真实空口延迟（含设备缓冲/网络）由 M2 用 RTCP 测量，此处为本地估计钩子。
func (p *Pipeline) recordLatency() {
	e2e := time.Since(p.frameStart) + p.playRing.BufferedDuration()
	us := uint64(e2e.Microseconds())
	p.stats.LatencyCount.Add(1)
	p.stats.LatencySumUS.Add(us)
	for {
		cur := p.stats.LatencyMaxUS.Load()
		if us <= cur || p.stats.LatencyMaxUS.CompareAndSwap(cur, us) {
			break
		}
	}
	if cb := p.cfg.OnLatency; cb != nil && time.Since(p.lastLatRep) >= 200*time.Millisecond {
		p.lastLatRep = time.Now()
		cb(e2e)
	}
}

// BufferedDuration 返回播放环当前排队采样对应的时长。
func (r *RingBuffer) BufferedDuration() time.Duration {
	return time.Duration(r.Available()) * time.Second / SampleRate
}
