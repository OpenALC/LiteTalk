package audio

import "errors"

// WebRTC APM 的降噪档位（与 webrtc::AudioProcessing::Config::NoiseSuppression::Level
// 的枚举值一致）。LiteTalk 固定用 moderate：RNNoise 作为可选的强降噪叠加，
// APM 侧不再暴露档位，避免两个降噪器互相抢参数。
const (
	apmNSLow = iota
	apmNSModerate
	apmNSHigh
	apmNSVeryHigh
)

// apmOptions 一次 APM 子模块配置。运行期只有 AEC / AGC2 会随设置变化，
// NS 与 HPF 固定开启。
type apmOptions struct {
	AEC     bool
	NS      bool
	NSLevel int
	AGC2    bool
	HPF     bool
}

// errAPMUnsupported 表示当前平台没有 APM 实现（stub 平台）或 C++ 初始化失败。
var errAPMUnsupported = errors.New("webrtc apm: unsupported platform")

// apmProcessor 是 DSP 持有的 APM 句柄抽象：生产实现只有 *apmHandle（apm_cgo.go
// 的 cgo 封装或 apm_stub.go 的 no-op）。抽成接口是为了在测试里放进可控替身，
// 断言「句柄销毁不会与进行中的 processRender 重叠」——也就是 dsp.go 里
// 「读锁贯穿整帧」那条不变量。接口调用无分配，热路径不受影响。
//
// 生命周期约定（与 dsp.go 的 apmMu 配套，改动前先读 apm_cgo.go 的 close 注释）：
// 调用方只在 apmMu 写锁下摘除句柄并 close，Process/ProcessRender 持读锁贯穿
// 整帧，因此 close 与任何在途帧互斥。
type apmProcessor interface {
	configure(apmOptions) error
	processCapture([]int16) error
	processRender([]int16) error
	close()
}

// APMSupported 报告当前平台是否提供 WebRTC APM 实现（设置页 Beta 开关用）。
// false 的平台应隐藏/禁用开关并提示不支持。
func APMSupported() bool { return apmSupported() }
