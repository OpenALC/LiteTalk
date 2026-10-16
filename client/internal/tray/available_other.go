//go:build !linux

package tray

// Available 非 Linux 平台恒可用：Windows 任务栏托盘与 macOS 状态栏
// 由系统提供，wails v3 SystemTray 总能挂上。
func Available() bool { return true }
