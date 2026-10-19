package hotkey

// bindings.go 是三个平台共用的，它的测试也必须是无构建标签的——否则共享逻辑
// 只在 Windows 上被验证，Linux/macOS 的 CI 等于没测。原先这些用例挂在
// hotkey_windows_test.go 里，已挪到此处；windows 只保留 VK 表等平台专属用例。

import "testing"

// fakeCodeOf 伪造一套键码表，让 newHookState 的测试不依赖任何平台。
func fakeCodeOf(token string) (uint32, bool) {
	switch token {
	case "m":
		return 0x4D, true
	case "d":
		return 0x44, true
	case "ins": // 平台没有这个键：应被跳过而不是填假值
		return 0, false
	}
	return 0, false
}

func TestNewHookState(t *testing.T) {
	st := Settings{
		Enabled: true,
		PTT:     "ctrl+shift+m",
		Mute:    "mouse4",
		Deafen:  "ctrl+alt+d",
		Dual:    "bogus", // 解析失败，应被跳过而不是让整个表空掉
	}
	hs := newHookState(st, fakeCodeOf)
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

	// 平台没有的键被跳过：表里不出现，也不影响其他绑定
	if hs := newHookState(Settings{Enabled: true, PTT: "ins", Mute: "mouse4"}, fakeCodeOf); len(hs.keys) != 0 || len(hs.mice) != 1 {
		t.Errorf("平台缺失的键应被跳过：keys=%d mice=%d", len(hs.keys), len(hs.mice))
	}

	// Enabled=false 返回空表：此时 Manager 根本不会装钩子，空表只是双保险
	if hs := newHookState(Settings{Enabled: false, PTT: "ctrl+m"}, fakeCodeOf); len(hs.keys) != 0 || len(hs.mice) != 0 {
		t.Errorf("关闭时绑定表应为空，实际 keys=%d mice=%d", len(hs.keys), len(hs.mice))
	}
	// 全空设置
	if hs := newHookState(Settings{Enabled: true}, fakeCodeOf); len(hs.keys) != 0 || len(hs.mice) != 0 {
		t.Errorf("无绑定时表应为空，实际 keys=%d mice=%d", len(hs.keys), len(hs.mice))
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

func TestMouseID(t *testing.T) {
	// 鼠标 id 必须落在键盘键码（0–255）占不到的区间，否则 activeSet 会把
	// 键盘和鼠标的按下/抬起配错对。
	if mouseIDBase <= 255 {
		t.Fatalf("mouseIDBase = %d，应大于 255", mouseIDBase)
	}
	for _, btn := range []uint32{1, 2, 3, 4, 5} {
		if got := mouseID(btn); got != mouseIDBase+btn {
			t.Errorf("mouseID(%d) = %d，期望 %d", btn, got, mouseIDBase+btn)
		}
	}
}

func TestPressedSet(t *testing.T) {
	var p pressedSet
	for _, id := range []uint32{0, 1, 63, 64, 127, 128, 255} {
		if p.is(id) {
			t.Errorf("初始不应为已按下：%d", id)
		}
		p.set(id)
		if !p.is(id) {
			t.Errorf("set 后应生效：%d", id)
		}
	}
	p.clear(0x4D)
	if p.is(0x4D) {
		t.Error("clear 后应失效")
	}

	// 相邻位互不影响（用干净的位图，避免上面循环残留的位干扰）
	var q pressedSet
	q.set(64)
	if q.is(63) || q.is(65) {
		t.Error("位图相邻位串了")
	}
	q.clear(64)
	if q.is(64) {
		t.Error("clear 后应失效")
	}

	// 超出位图容量的 id 不过滤也不串位：宁可放过自动重复，不可静默丢事件
	var r pressedSet
	r.set(256)
	if r.is(256) || r.is(0) {
		t.Error("越界 id 不该落到位图里")
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
