//go:build windows

// Package hotkey 的 Windows 实现：user32 低级键盘钩子（WH_KEYBOARD_LL）与
// 低级鼠标钩子（WH_MOUSE_LL），经 syscall.NewLazyDLL 直调，
// 不引入 golang.org/x/sys/windows。
//
// 两个钩子装在同一个专用线程上（LockOSThread + GetMessage 消息循环），
// 因此修饰键状态、已按下位图、按下中的绑定表这些可变数据只在该线程上读写，
// 不需要锁；绑定表本身以 atomic.Pointer 整体替换，热更新时回调看到的
// 要么是旧快照要么是新快照。
package hotkey

import (
	"errors"
	"fmt"
	"runtime"
	"strconv"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
	"unsafe"
)

// Win32 过程地址。全部惰性解析，Find 失败时 available() 如实报告不可用。
var (
	user32                  = syscall.NewLazyDLL("user32.dll")
	procSetWindowsHookExW   = user32.NewProc("SetWindowsHookExW")
	procUnhookWindowsHookEx = user32.NewProc("UnhookWindowsHookEx")
	procCallNextHookEx      = user32.NewProc("CallNextHookEx")
	procGetMessageW         = user32.NewProc("GetMessageW")
	procPostThreadMessageW  = user32.NewProc("PostThreadMessageW")
	procGetAsyncKeyState    = user32.NewProc("GetAsyncKeyState")

	kernel32               = syscall.NewLazyDLL("kernel32.dll")
	procGetCurrentThreadId = kernel32.NewProc("GetCurrentThreadId")
)

// 钩子类型与消息常量。
const (
	whKeyboardLL = 13
	whMouseLL    = 14

	wmQuit = 0x0012

	wmKeyDown    = 0x0100
	wmKeyUp      = 0x0101
	wmSysKeyDown = 0x0104
	wmSysKeyUp   = 0x0105

	wmLButtonDown = 0x0201
	wmLButtonUp   = 0x0202
	wmRButtonDown = 0x0204
	wmRButtonUp   = 0x0205
	wmMButtonDown = 0x0207
	wmMButtonUp   = 0x0208
	wmXButtonDown = 0x020B
	wmXButtonUp   = 0x020C
)

// 虚拟键码（修饰键的左右两侧分别列，钩子里归一成同一组位标记）。
const (
	vkShift    = 0x10
	vkControl  = 0x11
	vkMenu     = 0x12
	vkLShift   = 0xA0
	vkRShift   = 0xA1
	vkLControl = 0xA2
	vkRControl = 0xA3
	vkLMenu    = 0xA4
	vkRMenu    = 0xA5
	vkLWin     = 0x5B
	vkRWin     = 0x5C
)

// stopTimeout 是 Stop() 等消息循环退出的上限。正常路径下 PostThreadMessageW
// 一投过去 GetMessageW 立刻返回，这个超时只是兜底，不该被踩到。
const stopTimeout = 2 * time.Second

// kbdllhookstruct 对应 Win32 KBDLLHOOKSTRUCT。
type kbdllhookstruct struct {
	vkCode      uint32
	scanCode    uint32
	flags       uint32
	time        uint32
	dwExtraInfo uintptr
}

// msllhookstruct 对应 Win32 MSLLMOUSESTRUCT。mouseData 的高 16 位在
// WM_XBUTTONDOWN/UP 时是 X 按钮号（1=XBUTTON1→mouse4，2=XBUTTON2→mouse5）。
type msllhookstruct struct {
	x, y        int32
	mouseData   uint32
	flags       uint32
	time        uint32
	dwExtraInfo uintptr
}

// winMsg 对应 Win32 MSG（GetMessageW 的输出缓冲）。
type winMsg struct {
	hwnd    uintptr
	message uint32
	wParam  uintptr
	lParam  uintptr
	time    uint32
	pt      struct{ x, y int32 }
}

// vkOfToken 是主键 token → Win32 虚拟键码。鼠标键不在表内（走鼠标钩子）。
var vkOfToken = buildVKTable()

func buildVKTable() map[string]uint32 {
	m := make(map[string]uint32, 96)
	for c := 'a'; c <= 'z'; c++ {
		m[string(c)] = 0x41 + uint32(c-'a')
	}
	for c := '0'; c <= '9'; c++ {
		m[string(c)] = 0x30 + uint32(c-'0')
	}
	for i := 1; i <= 24; i++ {
		m["f"+strconv.Itoa(i)] = 0x70 + uint32(i-1)
	}
	for t, vk := range map[string]uint32{
		"space": 0x20, "tab": 0x09, "esc": 0x1B, "enter": 0x0D,
		"up": 0x26, "down": 0x28, "left": 0x25, "right": 0x27,
		"home": 0x24, "end": 0x23, "pgup": 0x21, "pgdn": 0x22,
		"ins": 0x2D, "del": 0x2E,
	} {
		m[t] = vk
	}
	for t, vk := range map[string]uint32{
		"`": 0xC0, "-": 0xBD, "=": 0xBB, "[": 0xDB, "]": 0xDD,
		"\\": 0xDC, ";": 0xBA, "'": 0xDE, ",": 0xBC, ".": 0xBE, "/": 0xBF,
	} {
		m[t] = vk
	}
	return m
}

// mouseButtonOfToken 把鼠标 token 映射成按钮号（1 左 / 2 右 / 3 中 /
// 4 XBUTTON1 / 5 XBUTTON2），与 Win32 的 VK_LBUTTON..VK_XBUTTON2 一致。
// 注意 X 按钮的「Win32 索引」是 1/2，与这里的「鼠标键序号」4/5 不是一回事，
// 换算见 xButton。
func mouseButtonOfToken(tok string) (uint32, bool) {
	switch tok {
	case "mouse1":
		return 1, true
	case "mouse2":
		return 2, true
	case "mouse3":
		return 3, true
	case "mouse4":
		return 4, true
	case "mouse5":
		return 5, true
	}
	return 0, false
}

// hookState 是绑定表的不可变快照。热更新时整体替换指针，钩子回调只读快照，
// 跨线程不共享可变状态。
type hookState struct {
	keys []keyBind
	mice []mouseBind
}

type keyBind struct {
	vk     uint32
	mods   uint8
	action Action
}

type mouseBind struct {
	btn    uint32
	mods   uint8
	action Action
}

// newHookState 从设置构建绑定表。Enabled=false 返回空表——此时 Manager 根本
// 不会装钩子，空表只是双保险。
func newHookState(st Settings) *hookState {
	hs := &hookState{}
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
		if btn, ok := mouseButtonOfToken(b.key); ok {
			hs.mice = append(hs.mice, mouseBind{btn: btn, mods: b.mods, action: a})
			continue
		}
		vk, ok := vkOfToken[b.key]
		if !ok {
			continue
		}
		hs.keys = append(hs.keys, keyBind{vk: vk, mods: b.mods, action: a})
	}
	return hs
}

// winSource 是 Windows 平台的事件源实现。
type winSource struct {
	st atomic.Pointer[hookState]

	mu        sync.Mutex
	hooks     [2]uintptr // [0] 键盘钩子，[1] 鼠标钩子；0 值表示未安装
	tid       uint32     // 消息循环所在线程 id，Stop() 靠它投递 WM_QUIT
	done      chan struct{}
	callbacks [2]uintptr // NewCallback 的返回值，必须被持有防 GC
}

func newPlatformSource() source { return &winSource{} }

// available 报告能否真正装钩子。user32 及其过程在 Windows 上必然存在，
// 仍显式 Find 一次，把异常环境如实报告成不可用，而不是运行到一半 panic。
func (s *winSource) available() bool {
	return procSetWindowsHookExW.Find() == nil &&
		procGetMessageW.Find() == nil &&
		procPostThreadMessageW.Find() == nil
}

// start 在专用线程上装钩子并跑消息循环，装完（或装失败）才返回。
func (s *winSource) start(st Settings, ch chan<- Event) error {
	s.mu.Lock()
	if s.done != nil {
		s.mu.Unlock()
		return nil // 已在运行
	}
	if !s.available() {
		s.mu.Unlock()
		return ErrUnsupportedPlatform
	}
	s.st.Store(newHookState(st))
	installed := make(chan error, 1)
	done := make(chan struct{})
	s.done = done
	// 等 installed 之前必须放锁：pump 一进门就要拿这把锁记线程 id。
	s.mu.Unlock()

	go func() {
		runtime.LockOSThread()
		defer runtime.UnlockOSThread()
		defer close(done)
		s.pump(ch, installed)
	}()

	if err := <-installed; err != nil {
		<-done
		s.mu.Lock()
		s.done = nil
		s.hooks = [2]uintptr{}
		s.mu.Unlock()
		return err
	}
	return nil
}

// stop 卸载钩子并让消息循环退出。幂等。
func (s *winSource) stop() {
	s.mu.Lock()
	kbd, mouse := s.hooks[0], s.hooks[1]
	tid, done := s.tid, s.done
	s.hooks[0], s.hooks[1] = 0, 0
	s.done = nil
	s.mu.Unlock()

	if kbd != 0 {
		procUnhookWindowsHookEx.Call(kbd)
	}
	if mouse != 0 {
		procUnhookWindowsHookEx.Call(mouse)
	}
	// PostQuitMessage 只能退出「调用方线程」自己的消息循环，这里跨线程，
	// 必须用 PostThreadMessageW 把 WM_QUIT 投到消息循环所在线程。
	if tid != 0 {
		procPostThreadMessageW.Call(uintptr(tid), wmQuit, 0, 0)
	}
	if done != nil {
		select {
		case <-done:
		case <-time.After(stopTimeout):
		}
	}
}

// update 热替换绑定表。
func (s *winSource) update(st Settings) { s.st.Store(newHookState(st)) }

// pump 是钩子线程主体：装钩子 → GetMessage 消息循环 → 收到 WM_QUIT 退出。
// 必须在 LockOSThread 之后的那个线程上跑：SetWindowsHookEx 的 lpfn 由
// 安装线程的消息队列驱动，换线程会导致钩子不触发。
func (s *winSource) pump(ch chan<- Event, installed chan<- error) {
	tid, _, _ := procGetCurrentThreadId.Call()
	s.mu.Lock()
	s.tid = uint32(tid)
	s.mu.Unlock()

	rt := &hookRuntime{src: s, ch: ch}
	kbd := syscall.NewCallback(rt.keyboardProc)
	mouse := syscall.NewCallback(rt.mouseProc)

	s.mu.Lock()
	s.callbacks[0], s.callbacks[1] = kbd, mouse
	s.mu.Unlock()

	// 装钩子之前就按住的修饰键要算进去，否则第一次按键会丢修饰键位。
	rt.seedMods()

	hk, _, err := procSetWindowsHookExW.Call(whKeyboardLL, kbd, 0, 0)
	if hk == 0 {
		installed <- hookErr("SetWindowsHookExW(WH_KEYBOARD_LL)", err)
		return
	}
	s.mu.Lock()
	s.hooks[0] = hk
	s.mu.Unlock()

	hk, _, err = procSetWindowsHookExW.Call(whMouseLL, mouse, 0, 0)
	if hk == 0 {
		s.mu.Lock()
		kbdHook := s.hooks[0]
		s.hooks[0] = 0
		s.mu.Unlock()
		if kbdHook != 0 {
			procUnhookWindowsHookEx.Call(kbdHook)
		}
		installed <- hookErr("SetWindowsHookExW(WH_MOUSE_LL)", err)
		return
	}
	s.mu.Lock()
	s.hooks[1] = hk
	s.mu.Unlock()

	installed <- nil

	var m winMsg
	for {
		// 0 = 收到 WM_QUIT；-1 = 取消息出错。两者都结束循环。
		switch r, _, _ := procGetMessageW.Call(uintptr(unsafe.Pointer(&m)), 0, 0, 0); int32(r) {
		case 0, -1:
			return
		}
	}
}

// hookErr 整理 SetWindowsHookExW 的失败原因（GetLastError 可能为空）。
func hookErr(stage string, err error) error {
	if err == nil {
		return errors.New("hotkey: " + stage + " 失败")
	}
	return fmt.Errorf("hotkey: %s: %w", stage, err)
}

// hookRuntime 持有钩子回调的可变状态。两个 LL 钩子共用安装线程，回调只在该
// 线程上被调用，因此不需要锁或原子操作。
type hookRuntime struct {
	src *winSource
	ch  chan<- Event

	mods    uint8     // 当前修饰键位（跟随真实键盘累积）
	pressed [4]uint64 // 已按下的 vk 位图（0–255），用于滤掉自动重复
	active  activeSet
}

// seedMods 用 GetAsyncKeyState 补一份初始修饰键状态。
func (rt *hookRuntime) seedMods() {
	if asyncDown(vkControl) {
		rt.mods |= modCtrl
	}
	if asyncDown(vkMenu) {
		rt.mods |= modAlt
	}
	if asyncDown(vkShift) {
		rt.mods |= modShift
	}
	if asyncDown(vkLWin) || asyncDown(vkRWin) {
		rt.mods |= modWin
	}
}

func asyncDown(vk uint32) bool {
	r, _, _ := procGetAsyncKeyState.Call(uintptr(vk))
	return r&0x8000 != 0
}

// keyboardProc 是 WH_KEYBOARD_LL 的钩子过程。nCode<0 时必须原样交给
// CallNextHookEx 且不做任何处理。lParam 声明成 unsafe.Pointer 而不是 uintptr：
// 回调桩按寄存器透传，32 位参数的高位可能是调用方留下的垃圾，
// 指针参数则没有这个问题。
func (rt *hookRuntime) keyboardProc(nCode, wParam uintptr, lParam unsafe.Pointer) uintptr {
	if int32(nCode) >= 0 && lParam != nil {
		kh := (*kbdllhookstruct)(lParam)
		switch uint32(wParam) {
		case wmKeyDown, wmSysKeyDown:
			rt.onKey(kh.vkCode, false)
		case wmKeyUp, wmSysKeyUp:
			rt.onKey(kh.vkCode, true)
		}
	}
	return rt.nextHook(nCode, wParam, lParam)
}

// mouseProc 是 WH_MOUSE_LL 的钩子过程。
func (rt *hookRuntime) mouseProc(nCode, wParam uintptr, lParam unsafe.Pointer) uintptr {
	if int32(nCode) >= 0 && lParam != nil {
		var btn uint32
		var up bool
		switch uint32(wParam) {
		case wmLButtonDown:
			btn, up = 1, false
		case wmLButtonUp:
			btn, up = 1, true
		case wmRButtonDown:
			btn, up = 2, false
		case wmRButtonUp:
			btn, up = 2, true
		case wmMButtonDown:
			btn, up = 3, false
		case wmMButtonUp:
			btn, up = 3, true
		case wmXButtonDown:
			btn, up = xButton(lParam), false
		case wmXButtonUp:
			btn, up = xButton(lParam), true
		default:
			return rt.nextHook(nCode, wParam, lParam)
		}
		rt.onMouse(btn, up)
	}
	return rt.nextHook(nCode, wParam, lParam)
}

// xButton 从 MSLLMOUSESTRUCT.mouseData 取 X 按钮号，并换算成与绑定 token
// 一致的按钮编号：mouseData 高位字是 X 按钮索引（1=XBUTTON1、2=XBUTTON2），
// 而绑定编号里 XBUTTON1/XBUTTON2 是第 4/5 个鼠标键（mouse4/mouse5），
// 前三个已被左/右/中键占用，所以这里要 +3。
func xButton(lParam unsafe.Pointer) uint32 {
	mh := (*msllhookstruct)(lParam)
	switch mh.mouseData >> 16 {
	case 1:
		return 4
	case 2:
		return 5
	}
	return 0
}

func (rt *hookRuntime) nextHook(nCode, wParam uintptr, lParam unsafe.Pointer) uintptr {
	r, _, _ := procCallNextHookEx.Call(0, nCode, wParam, uintptr(lParam))
	return r
}

// onKey 处理一次键盘按下/抬起。修饰键位始终跟随真实键盘累积，即使当前没有
// 任何绑定也要记，否则按下顺序不同会漏判。
func (rt *hookRuntime) onKey(vk uint32, up bool) {
	if bit, ok := modBitOf(vk); ok {
		if up {
			rt.mods &^= bit
			rt.clearPressed(vk)
		} else {
			rt.mods |= bit
			rt.setPressed(vk)
		}
		return // 修饰键本身不是合法主键，不作为绑定触发
	}
	if up {
		rt.clearPressed(vk)
		if i := rt.active.find(vk); i >= 0 {
			emit(rt.ch, Event{Action: rt.active.items[i].action, Pressed: false})
			rt.active.removeAt(i)
		}
		return
	}
	if rt.isPressed(vk) {
		return // 自动重复：按住不放时系统会连发 WM_KEYDOWN
	}
	rt.setPressed(vk)
	if a, ok := rt.matchKey(vk); ok {
		rt.active.add(vk, a)
		emit(rt.ch, Event{Action: a, Pressed: true})
	}
}

// modBitOf 把修饰键的虚拟键码归一成位标记（左右两侧的 Ctrl/Alt/Shift/Win 同一组）。
func modBitOf(vk uint32) (uint8, bool) {
	switch vk {
	case vkShift, vkLShift, vkRShift:
		return modShift, true
	case vkControl, vkLControl, vkRControl:
		return modCtrl, true
	case vkMenu, vkLMenu, vkRMenu:
		return modAlt, true
	case vkLWin, vkRWin:
		return modWin, true
	}
	return 0, false
}

// matchKey 查找与「vk + 当前修饰键」精确匹配的绑定。修饰键要求完全一致：
// 绑 ctrl+shift+m 时多按一个 alt 不触发，避免误触。
func (rt *hookRuntime) matchKey(vk uint32) (Action, bool) {
	st := rt.src.st.Load()
	if st == nil {
		return "", false
	}
	for i := range st.keys {
		if st.keys[i].vk == vk && st.keys[i].mods == rt.mods {
			return st.keys[i].action, true
		}
	}
	return "", false
}

// onMouse 处理鼠标按钮的按下/抬起。修饰键位沿用键盘钩子累积的状态
// （两个钩子同线程），因此 ctrl+alt+mouse5 这类组合能正确匹配。
func (rt *hookRuntime) onMouse(btn uint32, up bool) {
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
	if a, ok := rt.matchMouse(btn); ok {
		rt.active.add(id, a)
		emit(rt.ch, Event{Action: a, Pressed: true})
	}
}

// mouseIDBase 给鼠标按钮一个不会与任何虚拟键码（0–255）冲突的 id 空间。
const mouseIDBase = 0x10000

func mouseID(btn uint32) uint32 { return mouseIDBase + btn }

func (rt *hookRuntime) matchMouse(btn uint32) (Action, bool) {
	st := rt.src.st.Load()
	if st == nil {
		return "", false
	}
	for i := range st.mice {
		if st.mice[i].btn == btn && st.mice[i].mods == rt.mods {
			return st.mice[i].action, true
		}
	}
	return "", false
}

func (rt *hookRuntime) setPressed(vk uint32)   { rt.pressed[vk>>6] |= 1 << (vk & 63) }
func (rt *hookRuntime) clearPressed(vk uint32) { rt.pressed[vk>>6] &^= 1 << (vk & 63) }

func (rt *hookRuntime) isPressed(vk uint32) bool {
	return rt.pressed[vk>>6]&(1<<(vk&63)) != 0
}

// activeEntry 记录一个已按下尚未抬起的绑定。
type activeEntry struct {
	id     uint32
	action Action
}

// activeSet 是「按下中」的绑定表。按下时就地记下是哪个动作，抬起时按同一个
// id 配平——这样用户先松修饰键再松主键（PTT 最常见收尾）也能正确收到抬起，
// 不会卡在「已按下」导致说话停不下来。
type activeSet struct {
	items [8]activeEntry
	n     int
}

func (a *activeSet) find(id uint32) int {
	for i := 0; i < a.n; i++ {
		if a.items[i].id == id {
			return i
		}
	}
	return -1
}

func (a *activeSet) add(id uint32, action Action) {
	if a.find(id) >= 0 || a.n == len(a.items) {
		return
	}
	a.items[a.n] = activeEntry{id: id, action: action}
	a.n++
}

func (a *activeSet) removeAt(i int) {
	a.n--
	a.items[i] = a.items[a.n]
}
