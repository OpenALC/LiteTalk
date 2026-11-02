//go:build !linux || gtk3

package config

// gtk4Build 标记本构建是否走 GTK4 路径，仅 linux && !gtk3 为 true
// （见 hibernate_gtk4.go）：gtk3 legacy 构建与非 Linux 平台的 destroy
// 休眠模式不受强降影响。
const gtk4Build = false
