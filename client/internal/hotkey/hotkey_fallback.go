//go:build !windows

// Package hotkey 的非 Windows 暂态 stub：Available() 恒 false、
// Start() 恒返回 ErrUnsupportedPlatform，API 面与 Windows 实现完全一致，
// 保证 go vet / go test / go build 在其余平台不炸。
//
// T3 落地 Linux 实现（hotkey_linux.go，//go:build linux）后，把本文件标签收窄为
// !windows && !linux；T4 落地 darwin 后再收窄为 !windows && !linux && !darwin。
// 只改这一行，不要删文件。
package hotkey

// stubSource 是未接入平台的事件源：什么都不做，如实报告不可用。
type stubSource struct{}

func newPlatformSource() source { return stubSource{} }

func (stubSource) available() bool { return false }

func (stubSource) start(Settings, chan<- Event) error { return ErrUnsupportedPlatform }

func (stubSource) stop() {}

func (stubSource) update(Settings) {}
