package audio

import (
	"fmt"
	"unsafe"

	"github.com/gen2brain/malgo"
)

// Playback 一路扬声器播放流：48k / mono / int16，回调内零分配零锁。
type Playback struct {
	dev *malgo.Device
}

// StartPlayback 打开播放设备并启动流。selector 为空用默认设备。
// 回调从 ring 读采样；欠载（数据不足）部分补静音并计数。
func StartPlayback(ctx *Context, selector string, ring *RingBuffer, stats *Stats) (*Playback, error) {
	devID, err := ctx.resolveDeviceID(malgo.Playback, selector)
	if err != nil {
		return nil, err
	}

	cfg := malgo.DefaultDeviceConfig(malgo.Playback)
	cfg.Playback.Format = malgo.FormatS16
	cfg.Playback.Channels = 1
	cfg.SampleRate = SampleRate
	cfg.PeriodSizeInMilliseconds = 10
	if devID != nil {
		cfg.Playback.DeviceID = unsafe.Pointer(devID)
	}

	callbacks := malgo.DeviceCallbacks{
		Data: func(outputSamples, _ []byte, _ uint32) {
			stats.PlaybackCallbacks.Add(1)
			n := len(outputSamples) / 2
			if n == 0 {
				return
			}
			// 同采集：只造切片头，RingBuffer.Read 直接拷进输出缓冲。
			out := unsafe.Slice((*int16)(unsafe.Pointer(unsafe.SliceData(outputSamples))), n)
			r := ring.Read(out)
			if r < n {
				clear(out[r:]) // 欠载补静音
				stats.PlaybackUnderruns.Add(1)
			}
			stats.PlaybackSamples.Add(uint64(r))
		},
	}

	dev, err := malgo.InitDevice(ctx.ctx.Context, cfg, callbacks)
	if err != nil {
		return nil, fmt.Errorf("init playback device: %w", err)
	}
	if err := dev.Start(); err != nil {
		dev.Uninit()
		return nil, fmt.Errorf("start playback device: %w", err)
	}
	p := &Playback{dev: dev}
	return p, nil
}

// Stop 停流并释放设备。
func (p *Playback) Stop() {
	if p.dev != nil {
		p.dev.Uninit()
		p.dev = nil
	}
}
