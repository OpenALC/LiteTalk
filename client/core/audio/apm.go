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
