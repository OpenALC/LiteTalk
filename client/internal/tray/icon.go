// Package tray 现仅承载静态托盘图标字节（gen_icon.go 生成：32x32 ICO + 256x256 PNG）。
// 托盘本体由 Wails v3 内建 SystemTray 提供（见 app.go），不再依赖 getlantern/systray。
//
// 图标格式按平台区分（Wails v3 Linux 托盘走 DBus StatusNotifierItem，SetIcon 只认
// PNG——喂 ICO 会静默失败导致托盘无图标；Windows 用 ICO）：
//
//	iconBytes := tray.IconForRuntime() // 按 runtime.GOOS 选 ICO/PNG
package tray

import (
	_ "embed"
	"runtime"
)

// IconICO 静态托盘图标字节（Windows 格式），编译期嵌入。
//
//go:embed icon.ico
var IconICO []byte

// IconPNG 静态托盘图标字节（PNG，256x256），编译期嵌入。
//
//go:embed icon.png
var IconPNG []byte

// IconForRuntime 按运行平台选择托盘图标字节：Windows 用 ICO，
// 其余平台（Linux StatusNotifierItem / macOS）用 PNG。
func IconForRuntime() []byte {
	if runtime.GOOS == "windows" {
		return IconICO
	}
	return IconPNG
}
