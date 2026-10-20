//go:build linux

package tray

import (
	"github.com/godbus/dbus/v5"
)

// statusNotifierWatcher 是托盘图标协议（StatusNotifierItem）的宿主服务名。
// KDE Plasma 原生持有；GNOME 需 AppIndicator 扩展，未装时该服务不存在，
// 托盘图标不会出现——此时报告不可用，让关窗语义回落为「退出程序」，
// 避免窗口隐藏后用户再也找不回应用。
const statusNotifierWatcher = "org.kde.StatusNotifierWatcher"

// Available 检测当前桌面会话是否有托盘宿主：会话总线上存在
// StatusNotifierWatcher 的 owner 即视为可用。检测失败一律视为不可用，
// 宁可靠拢「关窗即退出」也不能把窗口藏进不存在的托盘。
func Available() bool {
	conn, err := dbus.ConnectSessionBus()
	if err != nil {
		return false
	}
	defer conn.Close()
	var hasOwner bool
	err = conn.BusObject().Call("org.freedesktop.DBus.NameHasOwner", 0,
		statusNotifierWatcher, "").Store(&hasOwner)
	return err == nil && hasOwner
}
