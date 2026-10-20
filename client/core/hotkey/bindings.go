package hotkey

import "time"

// 平台中立部分：绑定表构建、按下/抬起配对、按键位图。
// 三个平台（windows/linux/darwin）共用这一份，各自只提供「主键 token → 平台
// 原生键码」的映射和 OS 事件摄取。曾把这些放在 windows 标签文件里，导致 darwin
// 和 linux 各自抄一份——而按下/抬起配对正是出过 bug 的地方（Windows 的 X 按钮
// 编号差 3），抄多份等于给同类缺陷留多个入口。

// stopTimeout 是 Stop() 等平台事件循环退出的上限。正常路径下退出信号一投过去
// 循环立刻返回，这个超时只是兜底，不该被踩到。三个平台共用同一个值。
const stopTimeout = 2 * time.Second

// keyBind 一条键盘绑定。code 是平台原生键码（Windows VK / Linux KeySym /
// macOS keycode），由各平台的 keycodeOf 解析后填入；token 保留原始主键，
// 便于诊断与测试断言，匹配时不用反查。
type keyBind struct {
	token  string
	code   uint32
	mods   uint8
	action Action
}

// mouseBind 一条鼠标绑定。btn 是跨平台统一的按钮编号（1 左 / 2 右 / 3 中 /
// 4 XBUTTON1 / 5 XBUTTON2），与 mouseButtonOfToken 的返回值一致。
type mouseBind struct {
	btn    uint32
	mods   uint8
	action Action
}

// hookState 是绑定表的不可变快照。热更新时整体替换指针，钩子回调只读快照，
// 跨线程不共享可变状态。
type hookState struct {
	keys []keyBind
	mice []mouseBind
}

// newHookState 从设置构建绑定表。keycodeOf 负责把主键 token 翻成平台原生键码，
// 返回 false 表示该平台没有这个键（例如 macOS 没有 Insert）——直接跳过，
// 不填假键码。Enabled=false 返回空表：此时 Manager 根本不会装钩子，
// 空表只是双保险。
func newHookState(st Settings, keycodeOf func(token string) (uint32, bool)) *hookState {
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
		code, ok := keycodeOf(b.key)
		if !ok {
			continue
		}
		hs.keys = append(hs.keys, keyBind{token: b.key, code: code, mods: b.mods, action: a})
	}
	return hs
}

// mouseButtonOfToken 把鼠标主键 token 翻成统一按钮编号。与平台无关：
// X11 的 button 8/9、Windows 的 XBUTTON1/2、macOS 的 button 3/4 都在各自的
// 事件摄取处换算到这个编号，绑定表只认这一套。
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

// mouseIDBase 给鼠标按钮一个不会与任何键盘键码冲突的 id 空间。
// activeSet 用同一个 id 空间配平按下/抬起，键盘与鼠标因此不会互相串。
const mouseIDBase = 0x10000

func mouseID(btn uint32) uint32 { return mouseIDBase + btn }

// pressedSet 是「已按下」的位图，用于滤掉系统自动重复（按住不放时连续收到的
// 重复按下）。容量 4×64=256 位，覆盖 0–255 的键码；超出范围的 id 直接放行
// （不过滤），避免静默丢事件。
type pressedSet struct {
	bits [4]uint64
}

func (p *pressedSet) set(id uint32) {
	if id >= 256 {
		return
	}
	p.bits[id>>6] |= 1 << (id & 63)
}

func (p *pressedSet) clear(id uint32) {
	if id >= 256 {
		return
	}
	p.bits[id>>6] &^= 1 << (id & 63)
}

func (p *pressedSet) is(id uint32) bool {
	if id >= 256 {
		return false
	}
	return p.bits[id>>6]&(1<<(id&63)) != 0
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
