//go:build windows

package hotkey

import (
	"testing"
	"unsafe"
)

// newTestRuntime 造一个不装真钩子的 hookRuntime：绑定表直接塞进 winSource，
// 事件通道用带缓冲的 chan，从而在单测里跑 onKey/onMouse 的全部分发逻辑。
func newTestRuntime(st *hookState, ch chan Event) *hookRuntime {
	s := &winSource{}
	s.st.Store(st)
	return &hookRuntime{src: s, ch: ch}
}

func drain(ch chan Event) []Event {
	var out []Event
	for {
		select {
		case ev := <-ch:
			out = append(out, ev)
		default:
			return out
		}
	}
}

// TestVKTableCoversKeyTokens 双向覆盖：每个非鼠标 token 都必须有且只有一个
// 虚拟键码，反过来每个虚拟键码也只能被一个 token 占用。
// 这一层和 binding_test.go 的 token 计数断言（=90）合起来，
// 保证「任一侧增删键位测试就红」。
func TestVKTableCoversKeyTokens(t *testing.T) {
	const wantMouse = 5
	wantKeys := len(keyTokens) - wantMouse
	if got := len(vkOfToken); got != wantKeys {
		t.Errorf("vkOfToken 应有 %d 项（token 总数 %d 减 %d 个鼠标键），实际 %d",
			wantKeys, len(keyTokens), wantMouse, got)
	}
	reverse := make(map[uint32]string, len(vkOfToken))
	for tok, vk := range vkOfToken {
		if _, isMouse := mouseButtonOfToken(tok); isMouse {
			t.Errorf("鼠标键 %q 不该出现在 vkOfToken 里（走鼠标钩子）", tok)
		}
		if !isKeyToken(tok) {
			t.Errorf("vkOfToken 含非法 token %q", tok)
		}
		if prev, dup := reverse[vk]; dup {
			t.Errorf("虚拟键码 0x%02X 被 %q 和 %q 同时占用", vk, prev, tok)
		}
		reverse[vk] = tok
	}
	for tok := range keyTokens {
		if _, isMouse := mouseButtonOfToken(tok); isMouse {
			continue
		}
		if _, ok := vkOfToken[tok]; !ok {
			t.Errorf("token %q 缺少 Win32 虚拟键码映射", tok)
		}
	}
}

// TestVKTableKnownValues 逐个钉住容易写错的键位。
func TestVKTableKnownValues(t *testing.T) {
	cases := map[string]uint32{
		"a": 0x41, "z": 0x5A, "0": 0x30, "9": 0x39,
		"f1": 0x70, "f12": 0x7B, "f24": 0x87,
		"space": 0x20, "tab": 0x09, "esc": 0x1B, "enter": 0x0D,
		"up": 0x26, "down": 0x28, "left": 0x25, "right": 0x27,
		"home": 0x24, "end": 0x23, "pgup": 0x21, "pgdn": 0x22,
		"ins": 0x2D, "del": 0x2E,
		"`": 0xC0, "-": 0xBD, "=": 0xBB, "[": 0xDB, "]": 0xDD,
		"\\": 0xDC, ";": 0xBA, "'": 0xDE, ",": 0xBC, ".": 0xBE, "/": 0xBF,
	}
	for tok, want := range cases {
		got, ok := vkOfToken[tok]
		if !ok {
			t.Errorf("token %q 不在映射表里", tok)
			continue
		}
		if got != want {
			t.Errorf("token %q → 0x%02X，期望 0x%02X", tok, got, want)
		}
	}
}

func TestMouseButtonOfToken(t *testing.T) {
	cases := map[string]uint32{
		"mouse1": 1, "mouse2": 2, "mouse3": 3, "mouse4": 4, "mouse5": 5,
	}
	for tok, want := range cases {
		got, ok := mouseButtonOfToken(tok)
		if !ok || got != want {
			t.Errorf("mouseButtonOfToken(%q) = %d,%v，期望 %d,true", tok, got, ok, want)
		}
	}
	for _, tok := range []string{"", "m", "mouse", "mouse0", "mouse6", "ctrl", "f1"} {
		if got, ok := mouseButtonOfToken(tok); ok {
			t.Errorf("mouseButtonOfToken(%q) = %d,true，应判为非鼠标键", tok, got)
		}
	}
}

// TestHookStructLayout 锁住 KBDLLHOOKSTRUCT / MSLLMOUSESTRUCT 的内存布局。
// 结构体偏移猜错不会编译失败，只会在真机上读到错字段——探针能发现，
// 但先把布局钉在测试里更省事。
func TestHookStructLayout(t *testing.T) {
	var kh kbdllhookstruct
	if got := unsafe.Sizeof(kh); got != 24 {
		t.Errorf("sizeof(KBDLLHOOKSTRUCT) = %d，期望 24", got)
	}
	if unsafe.Offsetof(kh.vkCode) != 0 ||
		unsafe.Offsetof(kh.scanCode) != 4 ||
		unsafe.Offsetof(kh.flags) != 8 ||
		unsafe.Offsetof(kh.time) != 12 ||
		unsafe.Offsetof(kh.dwExtraInfo) != 16 {
		t.Errorf("KBDLLHOOKSTRUCT 字段偏移不符：%+v", kh)
	}

	var mh msllhookstruct
	if got := unsafe.Sizeof(mh); got != 32 {
		t.Errorf("sizeof(MSLLMOUSESTRUCT) = %d，期望 32", got)
	}
	if unsafe.Offsetof(mh.x) != 0 ||
		unsafe.Offsetof(mh.y) != 4 ||
		unsafe.Offsetof(mh.mouseData) != 8 ||
		unsafe.Offsetof(mh.flags) != 12 ||
		unsafe.Offsetof(mh.time) != 16 ||
		unsafe.Offsetof(mh.dwExtraInfo) != 24 {
		t.Errorf("MSLLMOUSESTRUCT 字段偏移不符：%+v", mh)
	}

	var wm winMsg
	if got := unsafe.Sizeof(wm); got != 48 {
		t.Errorf("sizeof(MSG) = %d，期望 48", got)
	}
	if unsafe.Offsetof(wm.wParam) != 16 || unsafe.Offsetof(wm.lParam) != 24 {
		t.Errorf("MSG 字段偏移不符：%+v", wm)
	}
}

// TestXButton 验证 X 按钮索引 → 绑定编号的 +3 换算。这个换算错过一次：
// mouseData 高位字是 X 按钮索引（1/2），而绑定里 XBUTTON1/XBUTTON2 是
// 第 4/5 个鼠标键，直接拿来比会导致 mouse4/mouse5 永远不触发。
func TestXButton(t *testing.T) {
	cases := []struct {
		mouseData uint32
		want      uint32
	}{
		{0, 0},
		{1 << 16, 4}, // XBUTTON1 → mouse4
		{2 << 16, 5}, // XBUTTON2 → mouse5
		{3 << 16, 0}, // 不存在的第三侧键，映射到 0 保证匹配不上任何绑定
		{0xFFFF0000, 0},
	}
	for _, c := range cases {
		mh := msllhookstruct{mouseData: c.mouseData}
		if got := xButton(unsafe.Pointer(&mh)); got != c.want {
			t.Errorf("xButton(mouseData=0x%08X) = %d，期望 %d", c.mouseData, got, c.want)
		}
	}
}

func TestModBitOf(t *testing.T) {
	cases := map[uint32]uint8{
		vkShift: modShift, vkLShift: modShift, vkRShift: modShift,
		vkControl: modCtrl, vkLControl: modCtrl, vkRControl: modCtrl,
		vkMenu: modAlt, vkLMenu: modAlt, vkRMenu: modAlt,
		vkLWin: modWin, vkRWin: modWin,
	}
	for vk, want := range cases {
		got, ok := modBitOf(vk)
		if !ok || got != want {
			t.Errorf("modBitOf(0x%02X) = %d,%v，期望 %d,true", vk, got, ok, want)
		}
	}
	// 普通主键不是修饰键
	for _, vk := range []uint32{0x4D, 0x41, 0x70, 0x20} {
		if _, ok := modBitOf(vk); ok {
			t.Errorf("modBitOf(0x%02X) 不应判为修饰键", vk)
		}
	}
}

func TestNewHookState(t *testing.T) {
	st := Settings{
		Enabled: true,
		Mode:    ModePTT,
		PTT:     "ctrl+shift+m",
		Mute:    "mouse4",
		Deafen:  "ctrl+alt+d",
		Dual:    "bogus", // 解析失败，应被跳过而不是让整个表空掉
	}
	hs := newHookState(st, vkLookup)
	if len(hs.keys) != 2 || len(hs.mice) != 1 {
		t.Fatalf("keys=%d mice=%d，期望 2/1", len(hs.keys), len(hs.mice))
	}
	if hs.keys[0] != (keyBind{token: "m", code: 0x4D, mods: modCtrl | modShift, action: ActionPTT}) {
		t.Errorf("PTT 绑定项 = %+v", hs.keys[0])
	}
	if hs.keys[1] != (keyBind{token: "d", code: 0x44, mods: modCtrl | modAlt, action: ActionDeafen}) {
		t.Errorf("Deafen 绑定项 = %+v", hs.keys[1])
	}
	if hs.mice[0] != (mouseBind{btn: 4, mods: 0, action: ActionMute}) {
		t.Errorf("Mute 绑定项 = %+v", hs.mice[0])
	}

	// Enabled=false 返回空表：此时 Manager 根本不会装钩子，空表只是双保险
	if hs := newHookState(Settings{Enabled: false, PTT: "ctrl+m"}, vkLookup); len(hs.keys) != 0 || len(hs.mice) != 0 {
		t.Errorf("关闭时绑定表应为空，实际 keys=%d mice=%d", len(hs.keys), len(hs.mice))
	}
	// 全空设置
	if hs := newHookState(Settings{Enabled: true}, vkLookup); len(hs.keys) != 0 || len(hs.mice) != 0 {
		t.Errorf("无绑定时表应为空，实际 keys=%d mice=%d", len(hs.keys), len(hs.mice))
	}
}

func TestOnKeyDispatch(t *testing.T) {
	st := &hookState{keys: []keyBind{
		{token: "m", code: 0x4D, mods: modCtrl | modShift, action: ActionPTT},
		{token: "d", code: 0x44, mods: modCtrl | modAlt, action: ActionDeafen},
	}}
	ch := make(chan Event, 16)
	rt := newTestRuntime(st, ch)

	rt.onKey(vkControl, false)
	rt.onKey(vkShift, false)
	rt.onKey(0x4D, false) // ctrl+shift+m 按下
	got := drain(ch)
	if len(got) != 1 || got[0] != (Event{Action: ActionPTT, Pressed: true}) {
		t.Fatalf("按下事件 = %+v，期望单条 ptt/按下", got)
	}
	if rt.mods != modCtrl|modShift {
		t.Errorf("修饰键状态 = %d，期望 %d", rt.mods, modCtrl|modShift)
	}

	// 自动重复：同一键不放，再来的 KEYDOWN 不该再出事件
	rt.onKey(0x4D, false)
	rt.onKey(0x4D, false)
	if n := len(drain(ch)); n != 0 {
		t.Errorf("自动重复多发了 %d 条事件", n)
	}

	// 先松修饰键再松主键：靠 active 表配平，仍要出抬起
	rt.onKey(vkControl, true)
	rt.onKey(vkShift, true)
	if rt.mods != 0 {
		t.Errorf("松手后修饰键状态 = %d，期望 0", rt.mods)
	}
	rt.onKey(0x4D, true)
	got = drain(ch)
	if len(got) != 1 || got[0] != (Event{Action: ActionPTT, Pressed: false}) {
		t.Fatalf("抬起事件 = %+v，期望单条 ptt/抬起", got)
	}

	// 修饰键不精确一致：多按一个 alt 不该触发
	rt.onKey(vkControl, false)
	rt.onKey(vkShift, false)
	rt.onKey(vkMenu, false)
	rt.onKey(0x4D, false)
	if n := len(drain(ch)); n != 0 {
		t.Errorf("修饰键不一致时多发了 %d 条事件", n)
	}
	// 收尾清场
	rt.onKey(0x4D, true)
	rt.onKey(vkMenu, true)
	rt.onKey(vkShift, true)
	rt.onKey(vkControl, true)
	if n := len(drain(ch)); n != 0 {
		t.Errorf("收尾多发了 %d 条事件", n)
	}
	if rt.mods != 0 {
		t.Errorf("收尾后修饰键状态 = %d，期望 0", rt.mods)
	}

	// 未绑定键不产生事件（0x4B = 'k'）
	rt.onKey(0x4B, false)
	rt.onKey(0x4B, true)
	if n := len(drain(ch)); n != 0 {
		t.Errorf("未绑定键多发了 %d 条事件", n)
	}

	// 第二个动作：ctrl+alt+d
	rt.onKey(vkControl, false)
	rt.onKey(vkMenu, false)
	rt.onKey(0x44, false)
	rt.onKey(0x44, true)
	rt.onKey(vkMenu, true)
	rt.onKey(vkControl, true)
	got = drain(ch)
	if len(got) != 2 ||
		got[0] != (Event{Action: ActionDeafen, Pressed: true}) ||
		got[1] != (Event{Action: ActionDeafen, Pressed: false}) {
		t.Errorf("deafen 事件序列 = %+v", got)
	}
}

func TestOnMouseDispatch(t *testing.T) {
	st := &hookState{mice: []mouseBind{
		{btn: 4, mods: 0, action: ActionMute},
		{btn: 5, mods: modCtrl | modAlt, action: ActionPTT},
	}}
	ch := make(chan Event, 16)
	rt := newTestRuntime(st, ch)

	rt.onMouse(4, false)
	got := drain(ch)
	if len(got) != 1 || got[0] != (Event{Action: ActionMute, Pressed: true}) {
		t.Fatalf("mouse4 按下事件 = %+v", got)
	}
	// 连点尚未抬起，重复按下不该再发
	rt.onMouse(4, false)
	if n := len(drain(ch)); n != 0 {
		t.Errorf("mouse4 重复按下多发了 %d 条", n)
	}
	rt.onMouse(4, true)
	got = drain(ch)
	if len(got) != 1 || got[0] != (Event{Action: ActionMute, Pressed: false}) {
		t.Fatalf("mouse4 抬起事件 = %+v", got)
	}

	// 未绑定按钮
	rt.onMouse(1, false)
	rt.onMouse(1, true)
	rt.onMouse(3, false)
	rt.onMouse(3, true)
	if n := len(drain(ch)); n != 0 {
		t.Errorf("未绑定鼠标键多发了 %d 条", n)
	}

	// 带修饰键的鼠标绑定：修饰键位由键盘钩子累积，这里手动摆好
	rt.mods = modCtrl | modAlt
	rt.onMouse(5, false)
	rt.onMouse(5, true)
	got = drain(ch)
	if len(got) != 2 ||
		got[0] != (Event{Action: ActionPTT, Pressed: true}) ||
		got[1] != (Event{Action: ActionPTT, Pressed: false}) {
		t.Errorf("ctrl+alt+mouse5 事件序列 = %+v", got)
	}
	// 修饰键不齐不触发
	rt.mods = modCtrl
	rt.onMouse(5, false)
	rt.onMouse(5, true)
	if n := len(drain(ch)); n != 0 {
		t.Errorf("修饰键不足时 mouse5 多发了 %d 条", n)
	}
	rt.mods = 0
}

func TestMatchKeyAndMouse(t *testing.T) {
	st := &hookState{
		keys: []keyBind{{token: "m", code: 0x4D, mods: modCtrl, action: ActionPTT}},
		mice: []mouseBind{{btn: 4, mods: modShift, action: ActionMute}},
	}
	rt := newTestRuntime(st, nil)

	rt.mods = modCtrl
	if a, ok := rt.matchKey(0x4D); !ok || a != ActionPTT {
		t.Errorf("matchKey(ctrl+m) = %q,%v", a, ok)
	}
	rt.mods = modCtrl | modShift
	if _, ok := rt.matchKey(0x4D); ok {
		t.Error("修饰键超集时不该命中")
	}
	rt.mods = 0
	if _, ok := rt.matchKey(0x4D); ok {
		t.Error("无修饰键时不该命中")
	}
	if _, ok := rt.matchKey(0x41); ok {
		t.Error("未绑定键不该命中")
	}

	rt.mods = modShift
	if a, ok := rt.matchMouse(4); !ok || a != ActionMute {
		t.Errorf("matchMouse(shift+mouse4) = %q,%v", a, ok)
	}
	rt.mods = 0
	if _, ok := rt.matchMouse(4); ok {
		t.Error("无修饰键时不该命中鼠标绑定")
	}
	if _, ok := rt.matchMouse(5); ok {
		t.Error("未绑定鼠标键不该命中")
	}

	// 绑定表为 nil（极端情况）不能 panic
	empty := newTestRuntime(nil, nil)
	if _, ok := empty.matchKey(0x4D); ok {
		t.Error("空表不该命中")
	}
	if _, ok := empty.matchMouse(4); ok {
		t.Error("空表不该命中")
	}
}

func TestPressedBitmap(t *testing.T) {
	var rt hookRuntime
	for _, vk := range []uint32{0, 1, 63, 64, 127, 128, 255} {
		if rt.isPressed(vk) {
			t.Errorf("初始不应为已按下：0x%02X", vk)
		}
		rt.setPressed(vk)
		if !rt.isPressed(vk) {
			t.Errorf("setPressed 后应生效：0x%02X", vk)
		}
	}
	rt.clearPressed(0x4D)
	if rt.isPressed(0x4D) {
		t.Error("clearPressed 后应失效")
	}

	// 相邻位互不影响（用干净的 runtime，避免上面循环残留的位干扰）
	var rt2 hookRuntime
	rt2.setPressed(64)
	if rt2.isPressed(63) || rt2.isPressed(65) {
		t.Error("位图相邻位串了")
	}
	rt2.clearPressed(64)
	if rt2.isPressed(64) {
		t.Error("clearPressed 后应失效")
	}
}

func TestActiveSet(t *testing.T) {
	var a activeSet
	if a.find(1) >= 0 {
		t.Error("空表不该找到")
	}
	a.add(1, ActionPTT)
	a.add(2, ActionMute)
	a.add(1, ActionDeafen) // 同 id 去重
	if a.n != 2 {
		t.Fatalf("n = %d，期望 2", a.n)
	}
	if i := a.find(1); i < 0 || a.items[i].action != ActionPTT {
		t.Errorf("id=1 的动作 = %+v", a.items[a.find(1)])
	}
	if i := a.find(2); i < 0 || a.items[i].action != ActionMute {
		t.Errorf("id=2 的动作 = %+v", a.items[a.find(2)])
	}

	// 删中间一个后，剩下的仍可按 id 找到
	a.removeAt(0)
	if a.n != 1 || a.find(2) < 0 || a.find(1) >= 0 {
		t.Errorf("removeAt(0) 后状态异常：n=%d", a.n)
	}

	// 填满之后再加应被忽略而不是越界
	var b activeSet
	for i := 0; i < len(b.items); i++ {
		b.add(uint32(i+1), ActionPTT)
	}
	if b.n != len(b.items) {
		t.Fatalf("b.n = %d，期望 %d", b.n, len(b.items))
	}
	b.add(999, ActionMute)
	if b.n != len(b.items) {
		t.Errorf("满表再加应被忽略，n 变成 %d", b.n)
	}
}

func TestSeedModsNoPanic(t *testing.T) {
	// 只验证不 panic、不产生非法位（本机此时应该没有修饰键按下；
	// 即使有也只会多置位，这里不断言具体值）
	var rt hookRuntime
	rt.seedMods()
	if rt.mods & ^uint8(modCtrl|modAlt|modShift|modWin) != 0 {
		t.Errorf("seedMods 产生了非法位：%d", rt.mods)
	}
}

func TestAsyncDownNoPanic(t *testing.T) {
	_ = asyncDown(vkControl)
	_ = asyncDown(0xFF)
}

func TestNewPlatformSource(t *testing.T) {
	src := newPlatformSource()
	if !src.available() {
		t.Error("Windows 上应报告可用")
	}
	if _, ok := src.(*winSource); !ok {
		t.Errorf("Windows 平台源类型 = %T，期望 *winSource", src)
	}
}
