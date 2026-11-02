//go:build linux && !gtk3

package config

// gtk4Build 标记本构建是否走 Wails v3 默认的 GTK4 + WebKitGTK 6.0 路径。
// `-tags gtk3` 是 legacy 兼容路径（GTK3 + WebKitGTK 4.1），v1.10.0 迁移后的
// 新路径不带该标签。destroy 休眠模式的强降只在 gtk4Build 下生效
// （见 hibernate_mode.go 的 downgradeDestroyForGTK4）。
const gtk4Build = true
