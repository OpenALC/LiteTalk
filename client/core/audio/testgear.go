package audio

import (
	"fmt"
	"math"
	"sync"
	"sync/atomic"
	"time"
	"unsafe"

	"github.com/gen2brain/malgo"
)

// testgear 设备自检：独立于会话管线的一次性采集/播放测试。
// 设置页「测试」按钮用——不碰 Pipeline/Mixer 状态，测完即释放设备。
//
// 并发与占用说明：测试各自新建 malgo Context 并独占目标设备。包级
// testBusy 用 TryLock 拦截进程内的并发测试（如测试进行中切走设置页再
// 切回导致的组件重建重复触发）；但与进行中的通话管线重叠仍可能发生
// （WASAPI 共享模式通常可共存，独占设备会 Start 失败并向上透传错误）。

var testBusy atomic.Bool

// TestCapture 用指定采集设备（selector 空 = 系统默认）采集 dur 时长，
// 返回 RMS 与峰值（int16 域，按 mode 下混后的单声道——与通话链路口径一致）。
// 回调内累计 sum(x²)/max|x|，零分配。进程内同时只允许一个测试运行。
func TestCapture(selector string, mode InputMode, dur time.Duration) (rms, peak float64, err error) {
	if !testBusy.CompareAndSwap(false, true) {
		return 0, 0, fmt.Errorf("已有测试在进行中，请稍后再试")
	}
	defer testBusy.Store(false)

	ctx, err := NewContext(nil)
	if err != nil {
		return 0, 0, err
	}
	defer ctx.Close()

	devID, err := ctx.resolveDeviceID(malgo.Capture, selector)
	if err != nil {
		return 0, 0, err
	}

	ch := ctx.nativeCaptureChannels(devID)
	cfg := malgo.DefaultDeviceConfig(malgo.Capture)
	cfg.Capture.Format = malgo.FormatS16
	cfg.Capture.Channels = uint32(ch)
	cfg.SampleRate = SampleRate
	cfg.PeriodSizeInMilliseconds = 10
	if devID != nil {
		cfg.Capture.DeviceID = unsafe.Pointer(devID)
	}

	var mu sync.Mutex
	var sum float64
	var peakV int32
	var frames int
	// 下混缓冲与状态：回调外预分配（4096 帧 ≈85ms，超限分块处理）。
	mono := make([]int16, maxDownmixFrames)
	var dm downmixState

	callbacks := malgo.DeviceCallbacks{
		Data: func(_, inputSamples []byte, _ uint32) {
			n := len(inputSamples) / 2
			if n == 0 {
				return
			}
			samples := unsafe.Slice((*int16)(unsafe.Pointer(unsafe.SliceData(inputSamples))), n)
			acc := func(p []int16) {
				var s float64
				var p2 int32
				for _, x := range p {
					ax := int32(x)
					if ax < 0 {
						ax = -ax
					}
					s += float64(x) * float64(x)
					if ax > p2 {
						p2 = ax
					}
				}
				mu.Lock()
				sum += s
				frames += len(p)
				if p2 > peakV {
					peakV = p2
				}
				mu.Unlock()
			}
			if ch <= 1 {
				acc(samples)
				return
			}
			total := n / ch
			for off := 0; off < total; {
				take := total - off
				if take > len(mono) {
					take = len(mono)
				}
				got := downmixFrame(mono[:take], samples[off*ch:(off+take)*ch], ch, mode, &dm)
				acc(mono[:got])
				off += take
			}
		},
	}

	dev, err := malgo.InitDevice(ctx.ctx.Context, cfg, callbacks)
	if err != nil {
		return 0, 0, fmt.Errorf("init capture device: %w", err)
	}
	defer dev.Uninit()
	if err := dev.Start(); err != nil {
		return 0, 0, fmt.Errorf("start capture device: %w", err)
	}

	time.Sleep(dur)

	dev.Stop()
	mu.Lock()
	defer mu.Unlock()
	if frames == 0 {
		return 0, 0, fmt.Errorf("未采集到任何采样")
	}
	return math.Sqrt(sum / float64(frames)), float64(peakV), nil
}

// TestPlayback 用指定播放设备（selector 空 = 系统默认）播放 freq Hz 正弦 dur 时长。
// 播放回调内逐 10ms 生成正弦 PCM（幅度 12000），播完即停。
// 与 TestCapture 共用 testBusy 互斥。
func TestPlayback(selector string, freq float64, dur time.Duration) error {
	if !testBusy.CompareAndSwap(false, true) {
		return fmt.Errorf("已有测试在进行中，请稍后再试")
	}
	defer testBusy.Store(false)

	ctx, err := NewContext(nil)
	if err != nil {
		return err
	}
	defer ctx.Close()

	devID, err := ctx.resolveDeviceID(malgo.Playback, selector)
	if err != nil {
		return err
	}

	cfg := malgo.DefaultDeviceConfig(malgo.Playback)
	cfg.Playback.Format = malgo.FormatS16
	cfg.Playback.Channels = 1
	cfg.SampleRate = SampleRate
	cfg.PeriodSizeInMilliseconds = 10
	if devID != nil {
		cfg.Playback.DeviceID = unsafe.Pointer(devID)
	}

	phase := 0.0
	callbacks := malgo.DeviceCallbacks{
		Data: func(outputSamples, _ []byte, _ uint32) {
			n := len(outputSamples) / 2
			if n == 0 {
				return
			}
			out := unsafe.Slice((*int16)(unsafe.Pointer(unsafe.SliceData(outputSamples))), n)
			for i := range out {
				phase += 2 * math.Pi * freq / SampleRate
				out[i] = int16(12000 * math.Sin(phase))
			}
		},
	}

	dev, err := malgo.InitDevice(ctx.ctx.Context, cfg, callbacks)
	if err != nil {
		return fmt.Errorf("init playback device: %w", err)
	}
	defer dev.Uninit()
	if err := dev.Start(); err != nil {
		return fmt.Errorf("start playback device: %w", err)
	}

	time.Sleep(dur)
	return nil
}
