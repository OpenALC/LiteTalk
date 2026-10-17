//go:build linux

// Package hotkey 的 Linux 实现：X11 + XInput2 raw 事件。
//
// 为什么用 raw 事件而不是教科书里的 XGrabKey：XGrabKey 是被动抓取，按下的键会
// 从所有应用里被吃掉，绑一个不带修饰键的单键（例如 space）会让用户没法打字；
// raw 事件是只读通知，既不抢按键也不影响别的客户端，语义与 Windows 的
// WH_KEYBOARD_LL 钩子（看了原样交给 CallNextHookEx）一致。
//
// 本文件只管「OS 事件摄取」和「token → X11 KeySym」两件事；绑定表构建、
// 按下/抬起配对、鼠标统一编号都在 bindings.go，与 windows/darwin 共用。
//
// 平台限制（如实降级，不假装支持）：
//   - Wayland 会话不可用：DISPLAY 为空或 WAYLAND_DISPLAY 非空时 Available()
//     返回 false。Wayland 的 portal GlobalShortcuts 只有按下没有松开，物理上
//     做不了 PTT；
//   - 服务器不支持 XInput2（低于 2.0）时不可用，raw 事件是整套实现的前提；
//   - 修饰键位取事件自带的快照，不自行累积——XI2 每次 raw 事件都带完整修饰键
//     状态（Windows 的 LL 钩子没有所以要累积），启动前就按住的修饰键因此也能
//     正确匹配。Mod1 视为 Alt、Mod4 视为 Win，这是 X 上几乎唯一的约定；
//   - 键位按 keysym 匹配（keycode 第 0 列），用户切到非拉丁布局后原有绑定不再
//     触发——与 Windows/macOS 的行为一致，不特殊处理。
//
// 本文件无法在 Windows 本机编译验证（交叉编译 cgo 需要 Linux 工具链），只由 CI
// 的 check-client-linux 覆盖：X11/XInput2 的函数签名、结构体字段都按
// libx11-dev / libxi-dev 的头文件逐个核对，宁可保守也不猜。
package hotkey

/*
#cgo linux pkg-config: x11 xi

#include <X11/Xlib.h>
#include <X11/keysym.h>
#include <X11/extensions/XInput2.h>
#include <poll.h>

// 归一化后的输入事件。Go 侧只认 kind/code/sym/mods 四个字段，不接触 XEvent
// 联合体——cgo 对 C union 只能当字节数组看，逐字段强转既难读又容易错。
enum {
    LK_KIND_OTHER = 0,
    LK_KIND_KEY_DOWN = 1,
    LK_KIND_KEY_UP = 2,
    LK_KIND_MOUSE_DOWN = 3,
    LK_KIND_MOUSE_UP = 4
};

typedef struct {
    int kind;
    unsigned int code;   // 鼠标：X11 原生按钮号；键盘：0
    KeySym sym;          // 键盘：keycode 第 0 列的 keysym（未按 Shift 的那一列）；鼠标：0
    unsigned int mods;   // 修饰键掩码，已掩掉 Lock/Mod2/Mod3
} liteTalkInput;

// liteTalkInitThreads 打开 Xlib 的线程锁。Xlib 默认不是线程安全的：本实现的
// available() 可能被 Wails 主线程调用，事件循环跑在另一个 goroutine，两边同时
// 调 Xlib 会踩共享的扩展表与错误处理表。进程里调一次就够。
static int liteTalkInitThreads(void) {
    return XInitThreads();
}

// liteTalkXIReady 检查 XInput2 扩展可用。raw 事件是整套实现的前提，服务器不
// 支持就如实报告不可用，不退回 XGrabKey（那条路会吞按键，见文件头）。
static int liteTalkXIReady(Display *dpy) {
    int opcode = 0, event = 0, error = 0;
    if (!XQueryExtension(dpy, "XInputExtension", &opcode, &event, &error)) {
        return 0;
    }
    int major = 2, minor = 0;
    if (!XIQueryVersion(dpy, &major, &minor)) {
        return 0;
    }
    return major >= 2;
}

// liteTalkMods 把 XIModifierState 折算成 X 修饰键掩码，并掩掉 Lock（CapsLock）、
// Mod2（NumLock）、Mod3：绑定不该受大小写锁定和数字键盘锁影响。
// 取 effective（base|latched|locked 加键盘组的效果），个别驱动不填它，退回另
// 三个字段自己或一遍——同时缺位的概率远低于只缺一个。
static unsigned int liteTalkMods(XIModifierState m) {
    unsigned int v = (unsigned int)m.effective;
    if (v == 0) {
        v = (unsigned int)(m.base | m.latched | m.locked);
    }
    return v & (unsigned int)(ShiftMask | ControlMask | Mod1Mask | Mod4Mask);
}

static int lkXIOpcode = 0;

// liteTalkSelectRaw 在 root window 上选择 XI2 raw 键盘/鼠标事件。
// 只选主设备（XIAllMasterDevices）：挂在主设备上的从设备事件会重复上报一遍。
// 返回 1=成功。
static int liteTalkSelectRaw(Display *dpy, Window root) {
    int opcode = 0, event = 0, error = 0;
    if (!XQueryExtension(dpy, "XInputExtension", &opcode, &event, &error)) {
        return 0;
    }
    lkXIOpcode = opcode;
    unsigned char bits[XIMaskLen(XI_RawButtonRelease)] = {0};
    XIEventMask mask;
    mask.deviceid = XIAllMasterDevices;
    mask.mask_len = (int)sizeof(bits);
    mask.mask = bits;
    XISetMask(bits, XI_RawKeyPress);
    XISetMask(bits, XI_RawKeyRelease);
    XISetMask(bits, XI_RawButtonPress);
    XISetMask(bits, XI_RawButtonRelease);
    return XISelectEvents(dpy, root, &mask, 1) == Success;
}

// liteTalkPoll 等 X 连接可读，timeoutMS 毫秒后超时返回 0。
// 事件循环用它而不是裸 XNextEvent：后者会一直阻塞，Stop() 没法确定性地把线程
// 收回来；带超时的轮询让循环每一轮都能看到停止标志。
static int liteTalkPoll(int fd, int timeoutMS) {
    struct pollfd pfd;
    pfd.fd = fd;
    pfd.events = POLLIN;
    pfd.revents = 0;
    return poll(&pfd, 1, timeoutMS) > 0 ? 1 : 0;
}

// liteTalkNext 取一个已经排在队列里的 raw 输入事件，不阻塞。
// 返回 1 表示 *out 有效；0 表示队列空了、不是 raw 事件、或不属于本扩展。
static int liteTalkNext(Display *dpy, liteTalkInput *out) {
    if (!XPending(dpy)) {
        return 0;
    }
    XEvent ev;
    XNextEvent(dpy, &ev);
    if (ev.type != GenericEvent) {
        return 0;
    }
    XGenericEventCookie *cookie = &ev.xcookie;
    if (cookie->extension != lkXIOpcode || !XGetEventData(dpy, cookie)) {
        return 0;
    }
    int kind = LK_KIND_OTHER;
    switch (cookie->evtype) {
        case XI_RawKeyPress:      kind = LK_KIND_KEY_DOWN;   break;
        case XI_RawKeyRelease:    kind = LK_KIND_KEY_UP;     break;
        case XI_RawButtonPress:   kind = LK_KIND_MOUSE_DOWN; break;
        case XI_RawButtonRelease: kind = LK_KIND_MOUSE_UP;   break;
        default:
            XFreeEventData(dpy, cookie);
            return 0;
    }
    XIRawEvent *raw = (XIRawEvent *)cookie->data;
    out->kind = kind;
    out->code = 0;
    out->sym = 0;
    out->mods = liteTalkMods(raw->mods);
    if (kind == LK_KIND_KEY_DOWN || kind == LK_KIND_KEY_UP) {
        // 第 0 列是未按 Shift 的 keysym：绑定 token 全小写，按物理键匹配，
        // 于是 ctrl+shift+m 与 ctrl+m 拿到的是同一个 XK_m，靠 mods 区分。
        out->sym = XKeycodeToKeysym(dpy, (KeyCode)raw->detail, 0);
    } else {
        out->code = (unsigned int)raw->detail;
    }
    XFreeEventData(dpy, cookie);
    return 1;
}
*/
import "C"

import (
	"errors"
	"os"
	"runtime"
	"sync"
	"sync/atomic"
	"time"
)

func init() {
	// Xlib 默认不带锁：available() 可能被 Wails 主线程调用，事件循环跑在另一个
	// goroutine 上，两边同时调 Xlib 会踩共享的扩展表与错误处理表。
	C.liteTalkInitThreads()
}

// errXISelect 与 ErrUnsupportedPlatform 分开：XInput2 在但选不上 raw 事件，
// 说明这台机器的 X 服务有问题，该报「有毛病」而不是「平台不支持」。
var errXISelect = errors.New("hotkey: XISelectEvents 失败")

// keysymOfToken 是主键 token → X11 KeySym。鼠标键不在表内（走 raw 按钮事件）。
// 一律用 XK_* 具名常量而不是十六进制字面量：X11 的 keysym 与 Windows 的 VK、
// macOS 的键码是三套互不相干的编号（XK_F1 = 0xFFBE，VK_F1 = 0x70，macOS 的
// F1 = 0x7A），凭记忆抄十六进制必然出错；具名常量写错名字至少是编译错误，
// 不会静默变成「这个键永远不触发」。
var keysymOfToken = buildKeysymTable()

func buildKeysymTable() map[string]C.KeySym {
	m := make(map[string]C.KeySym, 90)
	for t, ks := range map[string]C.KeySym{
		"a": C.XK_a, "b": C.XK_b, "c": C.XK_c, "d": C.XK_d, "e": C.XK_e,
		"f": C.XK_f, "g": C.XK_g, "h": C.XK_h, "i": C.XK_i, "j": C.XK_j,
		"k": C.XK_k, "l": C.XK_l, "m": C.XK_m, "n": C.XK_n, "o": C.XK_o,
		"p": C.XK_p, "q": C.XK_q, "r": C.XK_r, "s": C.XK_s, "t": C.XK_t,
		"u": C.XK_u, "v": C.XK_v, "w": C.XK_w, "x": C.XK_x, "y": C.XK_y,
		"z": C.XK_z,
	} {
		m[t] = ks
	}
	for t, ks := range map[string]C.KeySym{
		"0": C.XK_0, "1": C.XK_1, "2": C.XK_2, "3": C.XK_3, "4": C.XK_4,
		"5": C.XK_5, "6": C.XK_6, "7": C.XK_7, "8": C.XK_8, "9": C.XK_9,
	} {
		m[t] = ks
	}
	// F13–F24 在 keysym 表里存在（多数键盘没有这些物理键，绑了不触发而已），
	// 与 macOS 不同——那边根本没有 F21–F24 的键码，建表时必须跳过。
	for t, ks := range map[string]C.KeySym{
		"f1": C.XK_F1, "f2": C.XK_F2, "f3": C.XK_F3, "f4": C.XK_F4,
		"f5": C.XK_F5, "f6": C.XK_F6, "f7": C.XK_F7, "f8": C.XK_F8,
		"f9": C.XK_F9, "f10": C.XK_F10, "f11": C.XK_F11, "f12": C.XK_F12,
		"f13": C.XK_F13, "f14": C.XK_F14, "f15": C.XK_F15, "f16": C.XK_F16,
		"f17": C.XK_F17, "f18": C.XK_F18, "f19": C.XK_F19, "f20": C.XK_F20,
		"f21": C.XK_F21, "f22": C.XK_F22, "f23": C.XK_F23, "f24": C.XK_F24,
	} {
		m[t] = ks
	}
	for t, ks := range map[string]C.KeySym{
		"space": C.XK_space, "tab": C.XK_Tab, "esc": C.XK_Escape, "enter": C.XK_Return,
		"up": C.XK_Up, "down": C.XK_Down, "left": C.XK_Left, "right": C.XK_Right,
		"home": C.XK_Home, "end": C.XK_End, "pgup": C.XK_Page_Up, "pgdn": C.XK_Page_Down,
		"ins": C.XK_Insert, "del": C.XK_Delete,
	} {
		m[t] = ks
	}
	// 标点键的 keysym 就是 ASCII 值（Latin-1 区），与 Windows 的 VK 又一次不同。
	for t, ks := range map[string]C.KeySym{
		"`": C.XK_grave, "-": C.XK_minus, "=": C.XK_equal, "[": C.XK_bracketleft,
		"]": C.XK_bracketright, "\\": C.XK_backslash, ";": C.XK_semicolon,
		"'": C.XK_apostrophe, ",": C.XK_comma, ".": C.XK_period, "/": C.XK_slash,
	} {
		m[t] = ks
	}
	return m
}

// keysymLookup 把主键 token 翻成 X11 KeySym，供 bindings.go 的 newHookState
// 回调。X11 的 keysym 表覆盖全部 85 个非鼠标主键，没有「平台没有这个键」的情况
// （键盘上真没有那个物理键时绑定只是永不触发，与 macOS 建表即跳过不同）。
func keysymLookup(token string) (uint32, bool) {
	ks, ok := keysymOfToken[token]
	return uint32(ks), ok
}

// x11UnifiedButton 把 X11 原生的鼠标按钮号翻成 bindings.go 定义的统一编号
// （1 左 / 2 右 / 3 中 / 4 XBUTTON1 / 5 XBUTTON2）。
//
// 坑在两边编号基准完全不同：X11 的顺序是 1 左 / 2 中 / 3 右——中键排在右键
// 前面；滚轮上下左右又占掉 4–7；侧键 XBUTTON1/XBUTTON2 落在 8/9。
// Windows 侧 xButton() 的 +3、macOS 侧 darwinUnifiedButton 的 +1 是同一个坑的
// 不同表现（T1 在 Windows 上正栽在这里：mouse4/mouse5 永远不触发），所以统一
// 编号只在这一处换算，绑定表不认原生号。
func x11UnifiedButton(native C.uint) uint32 {
	switch native {
	case 1:
		return 1 // 左
	case 3:
		return 2 // 右
	case 2:
		return 3 // 中
	case 8:
		return 4 // XBUTTON1，侧键「后退」
	case 9:
		return 5 // XBUTTON2，侧键「前进」
	}
	return 0 // 滚轮 4–7 及其它按钮不可绑定
}

// x11SessionOK 报告当前是不是可用的 X11 会话。DISPLAY 为空说明没有 X 服务；
// WAYLAND_DISPLAY 非空说明跑在 Wayland 合成器下（XWayland 只是兼容层）——
// Wayland 的 portal GlobalShortcuts 只有按下没有松开，物理上做不了 PTT，
// 如实降级，由前端提示用户。
func x11SessionOK() bool {
	if os.Getenv("DISPLAY") == "" {
		return false
	}
	return os.Getenv("WAYLAND_DISPLAY") == ""
}

// linuxSource 是 Linux（X11）平台的事件源实现。
type linuxSource struct {
	st atomic.Pointer[hookState]

	mu      sync.Mutex
	display *C.Display    // start 开、pump 关；Xlib 调用都收在 pump 线程里做
	quit    chan struct{} // stop() 关闭它，pump 下一轮看到就退出；nil 表示未运行
	done    chan struct{} // pump 退出后关闭
}

func newPlatformSource() source { return &linuxSource{} }

// available 报告能否真正收到 raw 事件：X11 会话 + 连接打得开 + 服务器有 XInput2。
// 探测用的连接立刻关掉、不缓存——一次「可用吗」的询问不该长期占一条连接，
// 缓存还会在 X 服务重启后变成假阳性。
// Manager.Available() 直透传到本方法，中间没有锁，所以这里不依赖调用方同步：
// 每条探测连接都是本函数私有的，XInitThreads 也已保证 Xlib 可多线程调用。
func (s *linuxSource) available() bool {
	if !x11SessionOK() {
		return false
	}
	dpy := C.XOpenDisplay(nil)
	if dpy == nil {
		return false
	}
	defer C.XCloseDisplay(dpy)
	return C.liteTalkXIReady(dpy) == 1
}

// start 在专用线程上选 raw 事件并跑事件循环，装完（或装失败）才返回。
func (s *linuxSource) start(st Settings, ch chan<- Event) error {
	s.mu.Lock()
	if s.quit != nil {
		s.mu.Unlock()
		return nil // 已在运行
	}
	if !x11SessionOK() {
		s.mu.Unlock()
		return ErrUnsupportedPlatform
	}
	dpy := C.XOpenDisplay(nil)
	if dpy == nil {
		s.mu.Unlock()
		return ErrUnsupportedPlatform
	}
	s.st.Store(newHookState(st, keysymLookup))
	installed := make(chan error, 1)
	quit := make(chan struct{})
	done := make(chan struct{})
	s.display, s.quit, s.done = dpy, quit, done
	s.mu.Unlock()

	go func() {
		runtime.LockOSThread()
		defer runtime.UnlockOSThread()
		defer close(done)
		s.pump(ch, installed)
	}()

	if err := <-installed; err != nil {
		<-done // pump 的 defer 负责 XCloseDisplay，等它做完再返回
		s.mu.Lock()
		s.display, s.quit, s.done = nil, nil, nil
		s.mu.Unlock()
		return err
	}
	return nil
}

// stop 让事件循环退出并等它收尾。幂等。
// 真正关连接的动作只能在 pump 线程做：Xlib 的 Display 不是线程安全的，
// 跨线程 XCloseDisplay 会踩到还在用它的 XNextEvent。
func (s *linuxSource) stop() {
	s.mu.Lock()
	quit, done := s.quit, s.done
	s.quit, s.done, s.display = nil, nil, nil
	s.mu.Unlock()
	if quit == nil {
		return
	}
	close(quit)
	select {
	case <-done:
	case <-time.After(stopTimeout):
	}
}

// update 热替换绑定表。
func (s *linuxSource) update(st Settings) { s.st.Store(newHookState(st, keysymLookup)) }

// pump 是事件循环主体：选 raw 事件 → 带超时地轮询连接 → 把队列里的事件分发完。
// 必须在 LockOSThread 之后的那个线程上跑：关连接与取事件都压在这个线程，
// 换线程等于把 Xlib 调用分散到两个 OS 线程上。
func (s *linuxSource) pump(ch chan<- Event, installed chan<- error) {
	s.mu.Lock()
	dpy, quit := s.display, s.quit
	s.mu.Unlock()
	if dpy == nil || quit == nil {
		// 生命周期上不该走到：start() 要等 installed 才返回，而 installed 由
		// 本函数发出，字段必然已被读过。真走到了也不能拿空 Display 调 Xlib。
		installed <- ErrUnsupportedPlatform
		return
	}
	defer C.XCloseDisplay(dpy)

	if C.liteTalkXIReady(dpy) != 1 {
		installed <- ErrUnsupportedPlatform
		return
	}
	if C.liteTalkSelectRaw(dpy, C.XDefaultRootWindow(dpy)) != 1 {
		installed <- errXISelect
		return
	}
	C.XFlush(dpy)
	installed <- nil

	fd := C.XConnectionNumber(dpy)
	rt := &linuxRuntime{src: s, ch: ch}
	for {
		select {
		case <-quit:
			return
		default:
		}
		if C.liteTalkPoll(fd, 200) != 1 {
			continue // 超时：回到循环开头看停止标志
		}
		for {
			var in C.liteTalkInput
			if C.liteTalkNext(dpy, &in) != 1 {
				break
			}
			rt.handle(&in)
		}
	}
}

// linuxRuntime 持有事件分发的可变状态，只在 pump 所在的单线程上读写。
// 按下/抬起配对用 bindings.go 的 activeSet，与 windows/darwin 共用同一份实现。
type linuxRuntime struct {
	src *linuxSource
	ch  chan<- Event

	mods   uint8 // 累积的修饰键位，事件快照的兜底
	active activeSet
}

// handle 把一条 raw 事件分发给键盘/鼠标处理。
func (rt *linuxRuntime) handle(in *C.liteTalkInput) {
	switch in.kind {
	case C.LK_KIND_KEY_DOWN:
		rt.onKey(in.sym, in.mods, false)
	case C.LK_KIND_KEY_UP:
		rt.onKey(in.sym, in.mods, true)
	case C.LK_KIND_MOUSE_DOWN:
		rt.onMouse(in.code, in.mods, false)
	case C.LK_KIND_MOUSE_UP:
		rt.onMouse(in.code, in.mods, true)
	}
}

// onKey 处理一次键盘按下/抬起。
//
// 不用 bindings.go 的 pressedSet 滤自动重复，改用 activeSet 兼作过滤：X11 的
// keysym 会超过 pressedSet 的 255 位上限（XK_F1 = 0xFFBE），而 activeSet 对任意
// id 都有效——且只有真被绑定的键才需要滤重，那些键本来就在 activeSet 里。
func (rt *linuxRuntime) onKey(sym C.KeySym, evMods C.uint, up bool) {
	if bit, ok := modBitOfKeysym(sym); ok {
		if up {
			rt.mods &^= bit
		} else {
			rt.mods |= bit
		}
		return // 修饰键不是合法主键，不作为绑定触发
	}
	id := uint32(sym)
	if up {
		if i := rt.active.find(id); i >= 0 {
			emit(rt.ch, Event{Action: rt.active.items[i].action, Pressed: false})
			rt.active.removeAt(i)
		}
		return
	}
	if rt.active.find(id) >= 0 {
		return // 自动重复：X 会对按住不放的键连发 KeyPress
	}
	if a, ok := rt.matchKey(id, modBitsOfMask(evMods)|rt.mods); ok {
		rt.active.add(id, a)
		emit(rt.ch, Event{Action: a, Pressed: true})
	}
}

// modBitOfKeysym 把修饰键的 KeySym 归一成位标记（左右两侧是不同 keysym）。
// Meta_L/R 故意不映射：某些布局下 Alt 键报的是 Meta，映射成 win 会把 Alt 误记成
// Super；那种布局下 Alt 由事件快照的 Mod1Mask 兜底。
func modBitOfKeysym(sym C.KeySym) (uint8, bool) {
	switch sym {
	case C.XK_Shift_L, C.XK_Shift_R:
		return modShift, true
	case C.XK_Control_L, C.XK_Control_R:
		return modCtrl, true
	case C.XK_Alt_L, C.XK_Alt_R:
		return modAlt, true
	case C.XK_Super_L, C.XK_Super_R:
		return modWin, true
	}
	return 0, false
}

// modBitsOfMask 把 X 修饰键掩码翻成 Go 侧的位标记。掩码已在 C 侧掩掉
// Lock/Mod2/Mod3，这里只剩 Shift/Control/Mod1/Mod4 四位。
func modBitsOfMask(mask C.uint) uint8 {
	var m uint8
	if mask&C.ShiftMask != 0 {
		m |= modShift
	}
	if mask&C.ControlMask != 0 {
		m |= modCtrl
	}
	if mask&C.Mod1Mask != 0 {
		m |= modAlt
	}
	if mask&C.Mod4Mask != 0 {
		m |= modWin
	}
	return m
}

// onMouse 处理鼠标按钮的按下/抬起。native 是 X11 原生按钮号，先换算成统一编号，
// 匹配与配平才和绑定表（以及 windows/darwin）对得上。修饰键位沿用键盘累积的
// 状态并或上事件快照，因此 ctrl+alt+mouse5 这类组合能正确匹配。
func (rt *linuxRuntime) onMouse(native, evMods C.uint, up bool) {
	btn := x11UnifiedButton(native)
	if btn == 0 {
		return // 滚轮等不可绑定的按钮
	}
	id := mouseID(btn)
	if up {
		if i := rt.active.find(id); i >= 0 {
			emit(rt.ch, Event{Action: rt.active.items[i].action, Pressed: false})
			rt.active.removeAt(i)
		}
		return
	}
	if rt.active.find(id) >= 0 {
		return // 重复按下（连点尚未抬起）
	}
	if a, ok := rt.matchMouse(btn, modBitsOfMask(evMods)|rt.mods); ok {
		rt.active.add(id, a)
		emit(rt.ch, Event{Action: a, Pressed: true})
	}
}

// matchKey 查找与「keysym + 修饰键」精确匹配的绑定。修饰键要求完全一致：
// 绑 ctrl+shift+m 时多按一个 alt 不触发，避免误触。
func (rt *linuxRuntime) matchKey(sym uint32, mods uint8) (Action, bool) {
	st := rt.src.st.Load()
	if st == nil {
		return "", false
	}
	for i := range st.keys {
		if st.keys[i].code == sym && st.keys[i].mods == mods {
			return st.keys[i].action, true
		}
	}
	return "", false
}

// matchMouse 按统一按钮编号查找绑定。
func (rt *linuxRuntime) matchMouse(btn uint32, mods uint8) (Action, bool) {
	st := rt.src.st.Load()
	if st == nil {
		return "", false
	}
	for i := range st.mice {
		if st.mice[i].btn == btn && st.mice[i].mods == mods {
			return st.mice[i].action, true
		}
	}
	return "", false
}
