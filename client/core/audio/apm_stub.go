//go:build !cgo || (!windows && (!linux || (!amd64 && !arm64)))

// WebRTC APM 的平台回落：darwin 等未接入平台，以及关闭 CGO 的构建。
// 不伪装已实现——apmSupported 返回 false，DSP 保持原 RNNoise/自研 AGC 链路。
package audio

// apmSupported 报告当前平台是否带真实 APM 实现。
func apmSupported() bool { return false }

// apmHandle stub：不会产生实例。
type apmHandle struct{}

// newAPM stub：永远返回 errAPMUnsupported。
func newAPM(apmOptions) (*apmHandle, error) { return nil, errAPMUnsupported }

func (a *apmHandle) configure(apmOptions) error   { return errAPMUnsupported }
func (a *apmHandle) processCapture([]int16) error { return errAPMUnsupported }
func (a *apmHandle) processRender([]int16) error  { return errAPMUnsupported }
func (a *apmHandle) close()                       {}
