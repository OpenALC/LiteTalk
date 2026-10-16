// Package tray 现仅承载静态托盘图标字节（gen_icon.go 生成的 32x32 品牌蓝声波条 ICO）。
// 托盘本体由 Wails v3 内建 SystemTray 提供（见 app.go），不再依赖 getlantern/systray。
package tray

import _ "embed"

// Icon 静态托盘图标字节，编译期嵌入。
//
//go:embed icon.ico
var Icon []byte
