//go:build !windows && !linux

// Package hotkey 的未接入平台暂态 stub：Available() 恒 false、
// Start() 恒返回 ErrUnsupportedPlatform，API 面与 Windows 实现完全一致，
// 保证 go vet / go test / go build 在其余平台不炸。
//
// Linux 实现（hotkey_linux.go，//go:build linux）已落地，本文件标签随之收窄为
// !windows && !linux；T4 的 darwin（hotkey_darwin.go）合入后再收窄为
// !windows && !linux && !darwin。只改这一行，不要删文件。
package hotkey

// stubSource 是未接入平台的事件源：什么都不做，如实报告不可用。
type stubSource struct{}

func newPlatformSource() source { return stubSource{} }

func (stubSource) available() bool { return false }

func (stubSource) start(Settings, chan<- Event) error { return ErrUnsupportedPlatform }

func (stubSource) stop() {}

func (stubSource) update(Settings) {}
