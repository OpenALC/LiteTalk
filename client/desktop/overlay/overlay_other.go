//go:build !windows

package overlay

// 非 Windows 平台没有原生悬浮层实现：报告不支持、启动返回
// ErrUnsupportedPlatform，由设置页禁用开关并提示「仅 Windows」。
// 注意这是真实的能力缺失，不是待实现的 stub——Linux GTK4 没有任意定位的
// 顶层窗口，macOS 也没有计划内的实现（issue #47 明确仅 Windows）。
func supportedPlatform() bool { return false }

func startPlatform(Snapshot) error { return ErrUnsupportedPlatform }

func stopPlatform() {}
