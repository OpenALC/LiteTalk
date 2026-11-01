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

// nameHasOwnerMethod 是 org.freedesktop.DBus.NameHasOwner 的完整成员名。
// 单独提成常量供回归测试断言调用形状（见 available_linux_test.go）。
const nameHasOwnerMethod = "org.freedesktop.DBus.NameHasOwner"

// Available 检测当前桌面会话是否有托盘宿主：会话总线上存在
// StatusNotifierWatcher 的 owner 即视为可用。检测失败一律视为不可用，
// 宁可靠拢「关窗即退出」也不能把窗口藏进不存在的托盘。
func Available() bool {
	conn, err := dbus.ConnectSessionBus()
	if err != nil {
		return false
	}
	defer conn.Close()
	hasOwner, err := nameHasOwner(conn.BusObject(), statusNotifierWatcher)
	return err == nil && hasOwner
}

// nameHasOwner 探测 name 在会话总线上是否有 owner。obj 抽成接口参数
// （生产传 conn.BusObject()，测试传假实现），调用形状可以不连总线做回归断言。
//
// 规范陷阱（#120）：NameHasOwner 的签名是单参 s（待查名）。多传一个尾部
// 空串会被 dbus >= 1.16（Ubuntu 26.04 起；dbus-broker 同样）以 InvalidArgs
// 拒绝——「wrong args (ss, expected s)」——探测恒失败、trayReady 恒 false，
// 关窗在不知情中整体降级为最小化。
func nameHasOwner(obj dbus.BusObject, name string) (bool, error) {
	var hasOwner bool
	err := obj.Call(nameHasOwnerMethod, 0, name).Store(&hasOwner)
	return hasOwner, err
}
