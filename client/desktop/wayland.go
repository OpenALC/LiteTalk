// Package desktop 承载桌面适配层的纯函数与壳件：不碰 App 状态、不依赖 Wails
// 运行时的可独立测试逻辑放这里，供 main 包（app.go 绑定面）调用。
// 托盘图标见 desktop/tray。
package desktop

import (
	"os"
	"runtime"
)

// IsWaylandEnv 按给定环境判定 Wayland 会话（纯函数，便于测试注入）：
// Linux 下有 WAYLAND_DISPLAY 即 Wayland；无 DISPLAY（纯 Wayland、无 XWayland）也算。
// 非 Linux 恒 false。
func IsWaylandEnv(goos, waylandDisplay, x11Display string) bool {
	if goos != "linux" {
		return false
	}
	if waylandDisplay != "" {
		return true
	}
	return x11Display == ""
}

// IsWaylandSession 进程环境的 Wayland 会话判定（Wayland 下禁止坐标定位窗口）。
func IsWaylandSession() bool {
	return IsWaylandEnv(runtime.GOOS, os.Getenv("WAYLAND_DISPLAY"), os.Getenv("DISPLAY"))
}
