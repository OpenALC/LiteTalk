//go:build darwin

// Package hotkey 的 macOS 实现：CoreGraphics 事件 tap（CGEventTapCreate）
// 监听键盘与鼠标侧键，由专用线程的 CFRunLoop 收事件。
//
// 为什么不像 Windows 那样在回调里直接 emit：cgo 的 Go→C 回调（//export）
// 要求 preamble 里只能有声明，而这里需要一批 static 状态；且 C 回调调 Go 要走
// crosscall2，约束与开销都不划算。因此 C 侧只做「归一化 + 入队 + 唤醒」，
// Go 侧用独立线程阻塞取事件再做匹配。队列满则丢，与 emit 的语义一致。
//
// 平台限制（如实降级，不假装支持）：
//   - 需要「辅助功能」权限（AXIsProcessTrusted）。未授权时 Available() 返回
//     false、Start() 返回 ErrUnsupportedPlatform，由前端提示用户去授权；
//   - macOS 没有 Insert 键，也没有 F21–F24 的虚拟键码，这 5 个 token 在 macOS
//     上绑定后不生效（建表时直接跳过，不填假键码）；
//   - 只捕获鼠标侧键（kCGEventOtherMouseDown/Up，按钮 3/4 = XBUTTON1/2 →
//     mouse4/mouse5），不捕获左/中/右键，所以 mouse1/mouse2/mouse3 绑定后
//     不生效——把全局左键当 PTT 键本身就是坏主意；
//   - 修饰键 win 对应 macOS 的 Command。
//
// 本文件未经编译验证：CI 的 macOS job 只在打 tag 时运行且 continue-on-error，
// 需要有 mac 的环境补验（GOOS=darwin go build ./... 加真机按键）。
package hotkey

/*
#cgo darwin CFLAGS: -x objective-c
#cgo darwin LDFLAGS: -framework Cocoa -framework CoreGraphics -framework ApplicationServices -framework CoreFoundation

#include <ApplicationServices/ApplicationServices.h>
#include <CoreFoundation/CoreFoundation.h>
#include <CoreGraphics/CGEvent.h>
#include <pthread.h>
#include <stdbool.h>

// 归一化后的事件类型。Go 侧直接用 C.LK_KIND_* 引用，不存在两份真相。
enum {
    LK_KIND_KEY_DOWN = 1,
    LK_KIND_KEY_UP = 2,
    LK_KIND_MOUSE_DOWN = 3,
    LK_KIND_MOUSE_UP = 4
};

// 队列容量。满则丢：回调线程绝不能阻塞，否则 macOS 会因回调超时
// 把整个事件 tap 停用。
#define LK_QUEUE_CAP 128

typedef struct {
    int kind;
    int code;       // 键盘：macOS 虚拟键码；鼠标：0
    int button;     // 鼠标：0 起的按钮号（3/4 = XBUTTON1/2）；键盘：0
    int mods;       // 位定义与 Go 侧 modCtrl/modAlt/modShift/modWin 逐位一致
    int autoRepeat; // 键盘自动重复标志
} liteTalkEvent;

static liteTalkEvent lkQueue[LK_QUEUE_CAP];
static int lkHead = 0;
static int lkTail = 0;
static int lkStopped = 0;
static pthread_mutex_t lkMutex = PTHREAD_MUTEX_INITIALIZER;
static pthread_cond_t lkCond = PTHREAD_COND_INITIALIZER;

static CFMachPortRef lkTap = NULL;
static CFRunLoopSourceRef lkSource = NULL;

static int lkIsStopped(void) {
    pthread_mutex_lock(&lkMutex);
    int v = lkStopped;
    pthread_mutex_unlock(&lkMutex);
    return v;
}

// liteTalkIsTrusted 报告进程是否已获得「辅助功能」权限。
// 包一层返回 int：Go 侧不必关心 C.Boolean 的确切类型。
static int liteTalkIsTrusted(void) {
    return AXIsProcessTrusted() != 0 ? 1 : 0;
}

// liteTalkMods 把 CGEventFlags 折算成 Go 侧的修饰键位。只认
// Shift/Control/Alternate/Command 四位，其余（Fn、CapsLock、NumericPad、
// NonCoalesced…）一律忽略，否则任何绑定都匹配不上。
// 位值与 Go 侧 modCtrl=1 / modAlt=2 / modShift=4 / modWin=8 一致。
static unsigned int liteTalkMods(CGEventFlags flags) {
    unsigned int m = 0;
    if ((flags & kCGEventFlagMaskShift) != 0) {
        m |= 4;
    }
    if ((flags & kCGEventFlagMaskControl) != 0) {
        m |= 1;
    }
    if ((flags & kCGEventFlagMaskAlternate) != 0) {
        m |= 2;
    }
    if ((flags & kCGEventFlagMaskCommand) != 0) {
        m |= 8;
    }
    return m;
}

// liteTalkPush 把一个归一化事件入队并唤醒消费线程。必须在回调之前定义：
// preamble 里没有前向声明，C 里先调用后定义在现代 clang 下是硬错误。
static void liteTalkPush(const liteTalkEvent *ev) {
    pthread_mutex_lock(&lkMutex);
    int next = (lkTail + 1) % LK_QUEUE_CAP;
    if (next != lkHead) {
        lkQueue[lkTail] = *ev;
        lkTail = next;
    }
    // 队列满则丢：绝不能为了等空位阻塞回调线程。
    pthread_cond_signal(&lkCond);
    pthread_mutex_unlock(&lkMutex);
}

// liteTalkTapCallback 是事件 tap 的唯一回调：归一化 + 入队，不碰任何 Go 对象。
// 事件原样返回——本实现不需要改写输入，行为等同于 ListenOnly，
// 但权限要求与默认 tap 相同。
static CGEventRef liteTalkTapCallback(CGEventTapProxy proxy, CGEventType type,
                                      CGEventRef event, void *userInfo) {
    if (type == kCGEventTapDisabledByTimeout || type == kCGEventTapDisabledByUserInput) {
        // 回调超时或被用户抢占后 tap 会被系统停用，必须显式重新使能，
        // 否则之后再也不会有事件进来。
        if (lkTap != NULL) {
            CGEventTapEnable(lkTap, true);
        }
        return event;
    }
    liteTalkEvent ev;
    ev.code = 0;
    ev.button = 0;
    ev.autoRepeat = 0;
    ev.mods = (int)liteTalkMods(CGEventGetFlags(event));
    switch (type) {
        case kCGEventKeyDown:
            ev.kind = LK_KIND_KEY_DOWN;
            ev.code = (int)CGEventGetIntegerValueField(event, kCGKeyboardEventKeycode);
            ev.autoRepeat = CGEventGetIntegerValueField(event, kCGKeyboardEventAutorepeat) != 0;
            break;
        case kCGEventKeyUp:
            ev.kind = LK_KIND_KEY_UP;
            ev.code = (int)CGEventGetIntegerValueField(event, kCGKeyboardEventKeycode);
            break;
        case kCGEventOtherMouseDown:
            ev.kind = LK_KIND_MOUSE_DOWN;
            ev.button = (int)CGEventGetIntegerValueField(event, kCGMouseEventButtonNumber);
            break;
        case kCGEventOtherMouseUp:
            ev.kind = LK_KIND_MOUSE_UP;
            ev.button = (int)CGEventGetIntegerValueField(event, kCGMouseEventButtonNumber);
            break;
        default:
            return event;
    }
    liteTalkPush(&ev);
    return event;
}

// liteTalkNext 阻塞到有事件可取或已停止。返回 1 表示 *out 有效，0 表示已停止。
static int liteTalkNext(liteTalkEvent *out) {
    pthread_mutex_lock(&lkMutex);
    for (;;) {
        if (lkHead != lkTail) {
            *out = lkQueue[lkHead];
            lkHead = (lkHead + 1) % LK_QUEUE_CAP;
            pthread_mutex_unlock(&lkMutex);
            return 1;
        }
        if (lkStopped) {
            pthread_mutex_unlock(&lkMutex);
            return 0;
        }
        pthread_cond_wait(&lkCond, &lkMutex);
    }
}

// liteTalkStart 建事件 tap 并挂到「当前线程」的 run loop 上。
// 必须在将要跑 run loop 的那个线程上调用。返回 1=成功，0=失败
// （未授权辅助功能权限，或 CGEventTapCreate / CFMachPortCreateRunLoopSource 失败）。
static int liteTalkStart(void) {
    lkStopped = 0; // 上一轮可能没走到 liteTalkRunLoop 的清理路径
    if (lkTap != NULL) {
        return 1;
    }
    if (!liteTalkIsTrusted()) {
        return 0;
    }
    CGEventMask mask = (1ULL << kCGEventKeyDown) | (1ULL << kCGEventKeyUp) |
                       (1ULL << kCGEventOtherMouseDown) | (1ULL << kCGEventOtherMouseUp);
    lkTap = CGEventTapCreate(kCGHIDEventTap, kCGHeadInsertEventTap, kCGEventTapOptionDefault,
                             mask, liteTalkTapCallback, NULL);
    if (lkTap == NULL) {
        return 0;
    }
    lkSource = CFMachPortCreateRunLoopSource(kCFAllocatorDefault, lkTap, 0);
    if (lkSource == NULL) {
        CFRelease(lkTap);
        lkTap = NULL;
        return 0;
    }
    CFRunLoopAddSource(CFRunLoopGetCurrent(), lkSource, kCFRunLoopDefaultMode);
    return 1;
}

// liteTalkRunLoop 跑 run loop 直到 liteTalkStop 被调用，然后在本线程清理。
// 用 20ms 的 RunInMode 切片而不是 CFRunLoopRun：退出条件是一个普通标志，
// 不依赖跨线程唤醒 run loop 的时序，行为可预测。
static void liteTalkRunLoop(void) {
    while (!lkIsStopped()) {
        CFRunLoopRunInMode(kCFRunLoopDefaultMode, 0.02, true);
    }
    // CFRunLoopRemoveSource 只能在 run loop 所在线程调用，释放只能放在这里。
    if (lkSource != NULL) {
        CFRunLoopRemoveSource(CFRunLoopGetCurrent(), lkSource, kCFRunLoopDefaultMode);
        CFRelease(lkSource);
        lkSource = NULL;
    }
    if (lkTap != NULL) {
        CFRelease(lkTap);
        lkTap = NULL;
    }
    lkHead = 0;
    lkTail = 0;
}

// liteTalkStop 置停止标志并唤醒 run loop 与消费线程。真正释放在 run loop
// 线程里做，调用方必须等该线程退出后才能认为资源已回收。
static void liteTalkStop(void) {
    pthread_mutex_lock(&lkMutex);
    lkStopped = 1;
    pthread_cond_broadcast(&lkCond);
    pthread_mutex_unlock(&lkMutex);
}
*/
import "C"

import (
	"errors"
	"runtime"
	"sync"
	"sync/atomic"
	"time"
)

// errTapCreate 与 ErrUnsupportedPlatform 分开：走到这里说明已经有辅助功能权限
// 但 tap 装不上，调用方该报「这机器有毛病」而不是「去授权」。
var errTapCreate = errors.New("hotkey: CGEventTapCreate 失败")

// stopTimeout 是 stop() 等 run loop 与消费线程退出的上限。正常路径下
// liteTalkStop 一置位，20ms 的 RunInMode 切片立刻返回，这个超时只是兜底。
const stopTimeout = 2 * time.Second

// darwinCodeOfToken 是主键 token → macOS 虚拟键码。鼠标键不在表内（走 OtherMouse 事件）。
// 键码取自 Carbon Events.h 的 kVK_* 常量，与 Windows 的 VK 完全不是一套。
// 最大的坑：字母键码按 ANSI 物理布局走（A/S/D/F 一排、Q/W/E 一排、Z/X/C 一排），
// 既不按字母序也不连续，且 5/6、t/y、g/h 三对相邻键的码值是反的；
// 数字键同理。所以这里必须逐个钉死，不能按序号推。
var darwinCodeOfToken = buildDarwinKeycodeTable()

func buildDarwinKeycodeTable() map[string]int {
	m := make(map[string]int, 96)
	// 字母：kVK_ANSI_*，按物理布局。b 前面跳过了 0x0A。
	for t, code := range map[string]int{
		"a": 0x00, "s": 0x01, "d": 0x02, "f": 0x03, "h": 0x04,
		"g": 0x05, "z": 0x06, "x": 0x07, "c": 0x08, "v": 0x09,
		"b": 0x0B, "q": 0x0C, "w": 0x0D, "e": 0x0E, "r": 0x0F,
		"y": 0x10, "t": 0x11, "o": 0x1F, "u": 0x20, "i": 0x22,
		"p": 0x23, "l": 0x25, "j": 0x26, "k": 0x28, "n": 0x2D, "m": 0x2E,
	} {
		m[t] = code
	}
	// 数字键码由 ANSI 物理布局决定：5 和 6 的顺序与数字相反。
	for t, code := range map[string]int{
		"0": 0x1D, "1": 0x12, "2": 0x13, "3": 0x14, "4": 0x15,
		"5": 0x17, "6": 0x16, "7": 0x1A, "8": 0x1C, "9": 0x19,
	} {
		m[t] = code
	}
	// 功能键 F1–F20。macOS 没有 F21–F24 的虚拟键码，见文件头说明。
	for t, code := range map[string]int{
		"f1": 0x7A, "f2": 0x78, "f3": 0x63, "f4": 0x76, "f5": 0x60,
		"f6": 0x61, "f7": 0x62, "f8": 0x64, "f9": 0x65, "f10": 0x6D,
		"f11": 0x67, "f12": 0x6F, "f13": 0x69, "f14": 0x6B, "f15": 0x71,
		"f16": 0x6A, "f17": 0x40, "f18": 0x4F, "f19": 0x50, "f20": 0x5A,
	} {
		m[t] = code
	}
	// 命名键。esc/enter/space 的键码与 Windows 的 VK 毫无关系。
	for t, code := range map[string]int{
		"space": 0x31, "tab": 0x30, "esc": 0x35, "enter": 0x24,
		"up": 0x7E, "down": 0x7D, "left": 0x7B, "right": 0x7C,
		"home": 0x73, "end": 0x77, "pgup": 0x74, "pgdn": 0x79,
		"del": 0x75, // ForwardDelete；0x33 是退格，不映射
	} {
		m[t] = code
	}
	for t, code := range map[string]int{
		"`": 0x32, "-": 0x1B, "=": 0x18, "[": 0x21, "]": 0x1E,
		"\\": 0x2A, ";": 0x29, "'": 0x27, ",": 0x2B, ".": 0x2F, "/": 0x2C,
	} {
		m[t] = code
	}
	return m
}

// darwinMouseButtonOfToken 把鼠标 token 映射成 macOS 的按钮号。macOS 的按钮号
// 从 0 起（0 左 / 1 右 / 2 中 / 3 XBUTTON1 / 4 XBUTTON2），与 Windows 的 1 起编号
// 差一。只认侧键：另三个按钮根本不在事件 mask 里，见文件头说明。
func darwinMouseButtonOfToken(tok string) (int, bool) {
	switch tok {
	case "mouse4":
		return 3, true
	case "mouse5":
		return 4, true
	}
	return 0, false
}

// darwinHookState / darwinKeyBind / darwinMouseBind 与 hotkey_windows.go 里的
// 同名结构等价，但那份在 windows 标签下、darwin 构建看不到，只能在这里再写一份。
type darwinHookState struct {
	keys []darwinKeyBind
	mice []darwinMouseBind
}

type darwinKeyBind struct {
	code   int
	mods   uint8
	action Action
}

type darwinMouseBind struct {
	btn    int
	mods   uint8
	action Action
}

// newDarwinHookState 从设置构建绑定表。Enabled=false 返回空表——此时 Manager
// 根本不会装 tap，空表只是双保险。平台上不存在的键在这里被跳过。
func newDarwinHookState(st Settings) *darwinHookState {
	hs := &darwinHookState{}
	if !st.Enabled {
		return hs
	}
	for _, a := range allActions {
		spec := st.specFor(a)
		if spec == "" {
			continue
		}
		b, err := ParseBinding(spec)
		if err != nil {
			continue
		}
		if btn, ok := darwinMouseButtonOfToken(b.key); ok {
			hs.mice = append(hs.mice, darwinMouseBind{btn: btn, mods: b.mods, action: a})
			continue
		}
		code, ok := darwinCodeOfToken[b.key]
		if !ok {
			continue
		}
		hs.keys = append(hs.keys, darwinKeyBind{code: code, mods: b.mods, action: a})
	}
	return hs
}

// darwinSource 是 macOS 平台的事件源实现。
type darwinSource struct {
	st atomic.Pointer[darwinHookState]

	mu       sync.Mutex
	done     chan struct{} // run loop 线程退出后关闭
	consumed chan struct{} // 事件消费线程退出后关闭
}

func newPlatformSource() source { return &darwinSource{} }

// available 报告能否真正装事件 tap。唯一前置条件是「辅助功能」权限：
// 没授权时 CGEventTapCreate 直接返回 NULL，如实报告不可用，
// 让前端显示「当前平台不支持，已退化为语音激活」并引导用户去授权。
func (s *darwinSource) available() bool { return C.liteTalkIsTrusted() == 1 }

// start 在专用线程上装 tap 并跑 run loop，装完（或装失败）才返回。
func (s *darwinSource) start(st Settings, ch chan<- Event) error {
	s.mu.Lock()
	if s.done != nil {
		s.mu.Unlock()
		return nil // 已在运行
	}
	if !s.available() {
		s.mu.Unlock()
		return ErrUnsupportedPlatform
	}
	s.st.Store(newDarwinHookState(st))
	installed := make(chan error, 1)
	done := make(chan struct{})
	consumed := make(chan struct{})
	s.done = done
	s.consumed = consumed
	s.mu.Unlock()

	go func() {
		runtime.LockOSThread()
		defer runtime.UnlockOSThread()
		defer close(done)
		s.pump(installed)
	}()

	if err := <-installed; err != nil {
		// 装失败时消费线程还没起，不存在要唤醒的对象。
		<-done
		s.mu.Lock()
		s.done = nil
		s.consumed = nil
		s.mu.Unlock()
		return err
	}

	// 装成功才起消费线程：失败路径上少一个需要善后的 goroutine。
	// 安装成功到消费线程真正取事件之间到达的输入会先堆在 C 侧队列里。
	go func() {
		defer close(consumed)
		s.consume(ch)
	}()
	return nil
}

// stop 停止监听并卸载事件 tap。幂等。
func (s *darwinSource) stop() {
	s.mu.Lock()
	done, consumed := s.done, s.consumed
	s.done = nil
	s.consumed = nil
	s.mu.Unlock()
	if done == nil {
		return
	}
	// 只置标志 + 唤醒：释放 tap/source 的动作必须在 run loop 线程里做。
	C.liteTalkStop()
	select {
	case <-done:
	case <-time.After(stopTimeout):
	}
	select {
	case <-consumed:
	case <-time.After(stopTimeout):
	}
}

// update 热替换绑定表。
func (s *darwinSource) update(st Settings) { s.st.Store(newDarwinHookState(st)) }

// pump 是 run loop 线程主体：装 tap → 信号安装结果 → 跑 run loop。
// 必须在 LockOSThread 之后的那个线程上跑：CFRunLoopAddSource 加的是
// 「当前线程」的 run loop，换线程等于把事件源挂到另一个循环上。
func (s *darwinSource) pump(installed chan<- error) {
	if C.liteTalkStart() == 0 {
		installed <- errTapCreate
		return
	}
	installed <- nil
	// 阻塞到 liteTalkStop 置位；返回前在本线程释放 tap 与 source。
	C.liteTalkRunLoop()
}

// consume 阻塞取事件并分发。只有一个 goroutine 跑它，因此按下中的绑定表
// 不需要锁；绑定表本身以 atomic.Pointer 整体替换，热更新时要么看到旧快照
// 要么看到新快照。
func (s *darwinSource) consume(ch chan<- Event) {
	rt := &darwinRuntime{src: s, ch: ch}
	for {
		var ev C.liteTalkEvent
		if C.liteTalkNext(&ev) == 0 {
			return // 已停止
		}
		switch ev.kind {
		case C.LK_KIND_KEY_DOWN:
			if ev.autoRepeat != 0 {
				continue // 自动重复：按住不放时系统会连发 KeyDown
			}
			rt.onKey(ev.code, ev.mods)
		case C.LK_KIND_KEY_UP:
			rt.onKeyUp(ev.code, ev.mods)
		case C.LK_KIND_MOUSE_DOWN:
			rt.onMouse(ev.button, ev.mods, false)
		case C.LK_KIND_MOUSE_UP:
			rt.onMouse(ev.button, ev.mods, true)
		}
	}
}

// darwinRuntime 持有事件分发的可变状态，只在 consume 所在的单线程上读写。
type darwinRuntime struct {
	src    *darwinSource
	ch     chan<- Event
	active darwinActiveSet
}

// onKey 处理一次键盘按下。修饰键位直接取自事件的 CGEventGetFlags——macOS 每个
// 事件都带完整修饰键快照，不需要像 Windows 那样自己累积，因此「按下顺序不同
// 导致漏判」这个问题在这里不存在。
func (rt *darwinRuntime) onKey(code, mods C.int) {
	if a, ok := rt.matchKey(code, mods); ok {
		rt.active.add(darwinKeyID(int(code)), a)
		emit(rt.ch, Event{Action: a, Pressed: true})
	}
}

// onKeyUp 处理一次键盘抬起。修饰键不参与：用户先松修饰键再松主键（PTT 最常见
// 收尾）时，主键抬起事件里的修饰键快照已经少了那位，若拿来匹配就会配平不了。
func (rt *darwinRuntime) onKeyUp(code, _ C.int) {
	if i := rt.active.find(darwinKeyID(int(code))); i >= 0 {
		emit(rt.ch, Event{Action: rt.active.items[i].action, Pressed: false})
		rt.active.removeAt(i)
	}
}

// onMouse 处理鼠标侧键的按下/抬起。
func (rt *darwinRuntime) onMouse(button, mods C.int, up bool) {
	id := darwinMouseID(int(button))
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
	if a, ok := rt.matchMouse(button, mods); ok {
		rt.active.add(id, a)
		emit(rt.ch, Event{Action: a, Pressed: true})
	}
}

// matchKey 查找与「键码 + 修饰键」精确匹配的绑定。修饰键要求完全一致：
// 绑 ctrl+shift+m 时多按一个 alt 不触发，避免误触。
func (rt *darwinRuntime) matchKey(code, mods C.int) (Action, bool) {
	st := rt.src.st.Load()
	if st == nil {
		return "", false
	}
	for i := range st.keys {
		if st.keys[i].code == int(code) && st.keys[i].mods == uint8(mods) {
			return st.keys[i].action, true
		}
	}
	return "", false
}

func (rt *darwinRuntime) matchMouse(button, mods C.int) (Action, bool) {
	st := rt.src.st.Load()
	if st == nil {
		return "", false
	}
	for i := range st.mice {
		if st.mice[i].btn == int(button) && st.mice[i].mods == uint8(mods) {
			return st.mice[i].action, true
		}
	}
	return "", false
}

// darwinMouseIDBase 给鼠标按钮一个不会与任何虚拟键码（0–0x7E）冲突的 id 空间。
const darwinMouseIDBase = 0x10000

func darwinKeyID(code int) uint32  { return uint32(code) }
func darwinMouseID(btn int) uint32 { return darwinMouseIDBase + uint32(btn) }

// darwinActiveEntry / darwinActiveSet 与 hotkey_windows.go 里的同名实现等价，
// 但那份在 windows 标签下，darwin 构建看不到，只能在这里再写一份。
// 按下时就地记下是哪个动作，抬起时按同一个 id 配平——这样用户先松修饰键再松
// 主键（PTT 最常见收尾）也能正确收到抬起，不会卡在「已按下」导致说话停不下来。
type darwinActiveEntry struct {
	id     uint32
	action Action
}

type darwinActiveSet struct {
	items [8]darwinActiveEntry
	n     int
}

func (a *darwinActiveSet) find(id uint32) int {
	for i := 0; i < a.n; i++ {
		if a.items[i].id == id {
			return i
		}
	}
	return -1
}

func (a *darwinActiveSet) add(id uint32, action Action) {
	if a.find(id) >= 0 || a.n == len(a.items) {
		return
	}
	a.items[a.n] = darwinActiveEntry{id: id, action: action}
	a.n++
}

func (a *darwinActiveSet) removeAt(i int) {
	a.n--
	a.items[i] = a.items[a.n]
}
