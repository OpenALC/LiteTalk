package audio

import (
	"fmt"
	"unsafe"

	"github.com/gen2brain/malgo"
)

// maxDownmixFrames 下混预分配缓冲长度：PeriodSizeInMilliseconds=10ms → 480 帧，
// 取 4096（≈85ms）覆盖后端首回调可能给超大块的情况；仍超限时分块写环兜底。
const maxDownmixFrames = 4096

// Capture 一路麦克风采集流：48k / int16，按设备原生声道数（≤4）采集，
// 回调内下混为单声道写环；零分配零锁。
type Capture struct {
	dev  *malgo.Device
	wake chan struct{} // 容量 1，回调非阻塞投递，唤醒处理 goroutine

	ch   int          // 生效声道数（≥1；1 = 直通无下混）
	mode InputMode    // 下混模式（构造时定格，热切换走新 Capture）
	dm   downmixState // auto 模式跨帧状态（仅回调 goroutine 触碰）
	mono []int16      // 下混预分配缓冲（启动时一次分配）
}

// StartCapture 打开采集设备并启动流。selector 为空用默认设备。
// 采集到的采样经下混后写入 ring；ring 满时丢弃并计数。
// 声道数取设备原生值（≤4，查询失败回落 1 = 旧行为），再按 mode 下混——
// 单声道有信号的耳麦不再被矩形平均掉 6dB。
func StartCapture(ctx *Context, selector string, ring *RingBuffer, stats *Stats, mode InputMode) (*Capture, error) {
	devID, err := ctx.resolveDeviceID(malgo.Capture, selector)
	if err != nil {
		return nil, err
	}

	ch := ctx.nativeCaptureChannels(devID)
	cfg := malgo.DefaultDeviceConfig(malgo.Capture)
	cfg.Capture.Format = malgo.FormatS16
	cfg.Capture.Channels = uint32(ch)
	cfg.SampleRate = SampleRate
	cfg.PeriodSizeInMilliseconds = 10
	if devID != nil {
		// malgo DeviceID 与 C ma_device_id 内存布局一致，直接取 Go 侧数组指针；
		// miniaudio 仅在 InitDevice 期间读取，调用返回后不再持有。
		cfg.Capture.DeviceID = unsafe.Pointer(devID)
	}

	c := &Capture{
		wake: make(chan struct{}, 1),
		ch:   ch,
		mode: mode,
	}
	if ch > 1 {
		c.mono = make([]int16, maxDownmixFrames)
	}
	callbacks := malgo.DeviceCallbacks{
		Data: func(_, inputSamples []byte, _ uint32) {
			stats.CaptureCallbacks.Add(1)
			if n := len(inputSamples) / 2; n > 0 {
				// bytes → int16：unsafe.Slice 只构造切片头（栈上），不拷贝、
				// 不持有 C 内存——下混/RingBuffer.Write 立即把数据拷进预分配缓冲。
				samples := unsafe.Slice((*int16)(unsafe.Pointer(unsafe.SliceData(inputSamples))), n)
				if c.ch <= 1 {
					w := ring.Write(samples)
					stats.CaptureSamples.Add(uint64(w))
					if w < n {
						stats.CaptureDropped.Add(uint64(n - w))
					}
				} else {
					frames := n / c.ch
					for off := 0; off < frames; {
						take := frames - off
						if take > len(c.mono) {
							take = len(c.mono)
						}
						got := downmixFrame(c.mono[:take], samples[off*c.ch:(off+take)*c.ch], c.ch, c.mode, &c.dm)
						w := ring.Write(c.mono[:got])
						stats.CaptureSamples.Add(uint64(w))
						if w < got {
							stats.CaptureDropped.Add(uint64(got - w))
						}
						off += take
					}
				}
			}
			select {
			case c.wake <- struct{}{}:
			default: // 处理 goroutine 尚未消费上次的唤醒，无妨
			}
		},
	}

	dev, err := malgo.InitDevice(ctx.ctx.Context, cfg, callbacks)
	if err != nil {
		return nil, fmt.Errorf("init capture device: %w", err)
	}
	if err := dev.Start(); err != nil {
		dev.Uninit()
		return nil, fmt.Errorf("start capture device: %w", err)
	}
	c.dev = dev
	return c, nil
}

// Wake 返回唤醒通道：每次采集回调投递一次（可合并）。
func (c *Capture) Wake() <-chan struct{} { return c.wake }

// Stop 停流并释放设备。
func (c *Capture) Stop() {
	if c.dev != nil {
		c.dev.Uninit()
		c.dev = nil
	}
}
