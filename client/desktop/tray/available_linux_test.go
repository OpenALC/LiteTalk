//go:build linux

package tray

import (
	"errors"
	"testing"

	"github.com/godbus/dbus/v5"
)

// fakeBusObject 记录 nameHasOwner 发出的调用实参并回放预设结果，
// 让 #120 的回归断言不依赖会话总线与真实托盘宿主。嵌入 nil 接口只为
// 补齐 BusObject 其余方法（测试路径只走 Call）。
type fakeBusObject struct {
	dbus.BusObject
	method string
	args   []any
	owner  bool
	err    error
}

func (f *fakeBusObject) Call(method string, _ dbus.Flags, args ...any) *dbus.Call {
	f.method = method
	f.args = args
	c := &dbus.Call{Destination: "org.freedesktop.DBus", Method: method, Args: args}
	if f.err != nil {
		c.Err = f.err
	} else {
		c.Body = []any{f.owner}
	}
	return c
}

// TestNameHasOwnerCallShape 回归（#120）：NameHasOwner 按规范只能带单参
// （待查名）。此前的实现多传一个尾部空串，dbus >= 1.16 严格校验实参个数，
// 以 InvalidArgs 拒绝整个调用——trayReady 恒 false，关窗整体降级为最小化
// 且无任何用户可见报错。这里断言方法名与实参形状，双参写法再回归时直接失败。
func TestNameHasOwnerCallShape(t *testing.T) {
	fake := &fakeBusObject{owner: true}
	hasOwner, err := nameHasOwner(fake, statusNotifierWatcher)
	if err != nil {
		t.Fatalf("nameHasOwner: %v", err)
	}
	if !hasOwner {
		t.Fatal("owner 存在时应返回 true")
	}
	if fake.method != nameHasOwnerMethod {
		t.Fatalf("method = %q，期望 %q", fake.method, nameHasOwnerMethod)
	}
	if len(fake.args) != 1 {
		t.Fatalf("NameHasOwner 实参个数 = %d，规范签名是单参 s（dbus >= 1.16 拒收多参）：%v",
			len(fake.args), fake.args)
	}
	if name, ok := fake.args[0].(string); !ok || name != statusNotifierWatcher {
		t.Fatalf("实参应为待查服务名 %q，得到 %#v", statusNotifierWatcher, fake.args[0])
	}
}

// TestNameHasOwnerResults 探测结果语义：无 owner 与调用失败都必须报告不可用
// （Available 的 err == nil && hasOwner 语义由 nameHasOwner 的返回值承载）。
func TestNameHasOwnerResults(t *testing.T) {
	if hasOwner, err := nameHasOwner(&fakeBusObject{owner: false}, statusNotifierWatcher); err != nil || hasOwner {
		t.Fatalf("无 owner 应返回 (false, nil)，得到 (%v, %v)", hasOwner, err)
	}
	dbusErr := errors.New("org.freedesktop.DBus.Error.InvalidArgs: wrong args")
	if hasOwner, err := nameHasOwner(&fakeBusObject{err: dbusErr}, statusNotifierWatcher); err == nil || hasOwner {
		t.Fatalf("调用失败应返回 (false, err)，得到 (%v, %v)", hasOwner, err)
	}
}

// 编译期护栏：生产路径传入的 conn.BusObject() 必须继续满足 dbus.BusObject
// 接口（godbus 升级改签名时先在这里暴露）。
var _ dbus.BusObject = (*fakeBusObject)(nil)
