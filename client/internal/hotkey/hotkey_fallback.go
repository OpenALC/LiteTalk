//go:build !windows && !linux && !darwin

// Package hotkey 的未接入平台暂态 stub：Available() 恒 false、
// Start() 恒返回 ErrUnsupportedPlatform，API 面与 Windows 实现完全一致，
// 保证 go vet / go test / go build 在这些平台不炸。
//
// T3 落地 Linux 实现（hotkey_linux.go，//go:build linux）、T4 落地 macOS 实现
// （hotkey_darwin.go，//go:build darwin）后，标签已收窄为
// !windows && !linux && !darwin，只覆盖其余平台（freebsd 等）。
// linux/darwin 在关闭 CGO 时由 hotkey_nocgo.go 兜底——那两个平台的实现都是
// cgo 文件，CGO_ENABLED=0 下会被构建系统自动排除。
// 只改这一行，不要删文件。
package hotkey

// stubSource 是未接入平台的事件源：什么都不做，如实报告不可用。
type stubSource struct{}

func newPlatformSource() source { return stubSource{} }

func (stubSource) available() bool { return false }

func (stubSource) start(Settings, chan<- Event) error { return ErrUnsupportedPlatform }

func (stubSource) stop() {}

func (stubSource) update(Settings) {}
