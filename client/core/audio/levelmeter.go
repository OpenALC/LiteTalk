package audio

import (
	"fmt"
	"math"
	"sync"
	"sync/atomic"
	"time"
)

// LevelSample 100ms 聚合的电平样本（全部为 dBFS 负值）。
type LevelSample struct {
	PeakDB      float64 // 窗内峰值
	RmsDB       float64 // 窗内 RMS
	ThresholdDB float64 // 当前说话进入门限（底噪+12dB，绝对下限 -50dBFS；电平条标线）
}

// LevelMonitor 设置页实时电平：独立采集流（与通话管线互不干扰、不占 testBusy），
// 100ms 聚合一次回调。进程内同时只允许一个监视。
type LevelMonitor struct {
	ctx     *Context
	capture *Capture
	ring    *RingBuffer
	stats   Stats // StartCapture 的计数出口（不对外暴露）

	done     chan struct{}
	wg       sync.WaitGroup
	stopOnce sync.Once
}

var levelBusy atomic.Bool

// StartLevelMonitor 启动实时电平监视。selector 为空 = 系统默认设备，mode 为下混模式。
// emit 跑在监视 goroutine 上：必须非阻塞（app 侧只做事件投递）。
// 与麦克风自检（TestCapture）可并存——WASAPI/PulseAudio 共享模式支持多路采集流。
func StartLevelMonitor(selector string, mode InputMode, emit func(LevelSample)) (*LevelMonitor, error) {
	if emit == nil {
		return nil, fmt.Errorf("缺少电平回调")
	}
	if !levelBusy.CompareAndSwap(false, true) {
		return nil, fmt.Errorf("电平监视已在进行中")
	}
	ctx, err := NewContext(nil)
	if err != nil {
		levelBusy.Store(false)
		return nil, err
	}
	ring := NewRingBuffer(16384)
	m := &LevelMonitor{ctx: ctx, ring: ring, done: make(chan struct{})}
	cap, err := StartCapture(ctx, selector, ring, &m.stats, mode)
	if err != nil {
		_ = ctx.Close()
		levelBusy.Store(false)
		return nil, err
	}
	m.capture = cap
	m.wg.Add(1)
	go m.run(emit)
	return m, nil
}

// run 消费采集环：每 100ms（4800 采样）聚合一个样本回调。零分配（预分配缓冲）。
func (m *LevelMonitor) run(emit func(LevelSample)) {
	defer m.wg.Done()
	buf := make([]int16, FrameSize)
	var noise noiseTracker
	const samplesPerTick = SampleRate / 10 // 100ms
	var n int                              // 聚合窗内采样数
	var sqSum float64                      // Σ(rms² × 采样数)，聚合窗总能量近似
	var peak int32

	ticker := time.NewTimer(20 * time.Millisecond)
	defer ticker.Stop()
	for {
		ticker.Reset(20 * time.Millisecond)
		select {
		case <-m.done:
			return
		case <-m.capture.Wake():
		case <-ticker.C: // 兜底节拍
		}
		for m.ring.Available() >= FrameSize {
			m.ring.Read(buf)
			pk := peakAbs(buf)
			if pk > peak {
				peak = pk
			}
			r := rmsInt16(buf)
			sqSum += r * r * FrameSize
			n += FrameSize
			if pk > 1 {
				noise.observe(r) // 数字静音不入窗（与 DSP 口径一致）
			}
			if n >= samplesPerTick {
				emit(LevelSample{
					PeakDB:      dbFS(float64(peak)),
					RmsDB:       dbFS(math.Sqrt(sqSum / float64(n))),
					ThresholdDB: enterThresholdDB(noise.floorDB()),
				})
				n, sqSum, peak = 0, 0, 0
			}
		}
	}
}

// Stop 停止监视并释放设备（幂等）。
func (m *LevelMonitor) Stop() {
	m.stopOnce.Do(func() {
		close(m.done)
		if m.capture != nil {
			m.capture.Stop()
		}
		m.wg.Wait()
		if m.ctx != nil {
			_ = m.ctx.Close()
		}
		levelBusy.Store(false)
	})
}
