package hotkey

import "testing"

func TestParseBindingCanonical(t *testing.T) {
	cases := []struct{ in, want string }{
		{"m", "m"},
		{"ctrl+m", "ctrl+m"},
		{"Shift+Ctrl+M", "ctrl+shift+m"},
		{"ctrl + shift + m", "ctrl+shift+m"},
		{"  CTRL+ALT+SHIFT+WIN+F13  ", "ctrl+alt+shift+win+f13"},
		{"win+shift+alt+ctrl+a", "ctrl+alt+shift+win+a"},
		{"f1", "f1"},
		{"f24", "f24"},
		{"mouse1", "mouse1"},
		{"mouse4", "mouse4"},
		{"mouse5", "mouse5"},
		{"ctrl+alt+mouse5", "ctrl+alt+mouse5"},
		{"space", "space"},
		{"ctrl+enter", "ctrl+enter"},
		{"ctrl+esc", "ctrl+esc"},
		{"alt+pgup", "alt+pgup"},
		{"shift+ins", "shift+ins"},
		{"ctrl+del", "ctrl+del"},
		{"`", "`"},
		{"ctrl+/", "ctrl+/"},
		{"ctrl+alt+\\", "ctrl+alt+\\"},
		{"shift+;", "shift+;"},
		{"ctrl+'", "ctrl+'"},
		{"ctrl+,", "ctrl+,"},
		{"ctrl+.", "ctrl+."},
		{"ctrl+[", "ctrl+["},
		{"ctrl+]", "ctrl+]"},
		{"ctrl+-", "ctrl+-"},
		{"ctrl+=", "ctrl+="},
		{"0", "0"},
		{"ctrl+9", "ctrl+9"},
	}
	for _, c := range cases {
		b, err := ParseBinding(c.in)
		if err != nil {
			t.Fatalf("ParseBinding(%q) 失败: %v", c.in, err)
		}
		if got := b.String(); got != c.want {
			t.Errorf("ParseBinding(%q).String() = %q，期望 %q", c.in, got, c.want)
		}
		if b.IsZero() {
			t.Errorf("ParseBinding(%q) 不应是零值", c.in)
		}
		// round-trip：规范化串必须能再解析成同一个规范化串
		again, err := ParseBinding(b.String())
		if err != nil {
			t.Fatalf("round-trip ParseBinding(%q) 失败: %v", b.String(), err)
		}
		if again.String() != b.String() {
			t.Errorf("round-trip：%q → %q", b.String(), again.String())
		}
		if again.mods != b.mods || again.key != b.key {
			t.Errorf("round-trip 后字段不一致：%+v vs %+v", again, b)
		}
	}
}

// TestParseBindingAllTokens 覆盖 §0.2 列出的全部主键，并锁住 token 总数——
// 任何一边增删键位都必须同步前端 TS 的归一化实现，计数断言就是那道门。
func TestParseBindingAllTokens(t *testing.T) {
	// 26 字母 + 10 数字 + f1–f24 + 14 命名键 + 11 标点 + 5 鼠标键 = 90
	const wantTokens = 90
	if len(keyTokens) != wantTokens {
		t.Errorf("keyTokens 应有 %d 项，实际 %d（增删键位需同步前端）", wantTokens, len(keyTokens))
	}
	for tok := range keyTokens {
		b, err := ParseBinding("ctrl+" + tok)
		if err != nil {
			t.Fatalf("token %q 应合法: %v", tok, err)
		}
		if got, want := b.String(), "ctrl+"+tok; got != want {
			t.Errorf("token %q 规范化为 %q，期望 %q", tok, got, want)
		}
		// 鼠标键同样可以不带修饰键
		if _, err := ParseBinding(tok); err != nil {
			t.Errorf("token %q 单独作为主键应合法: %v", tok, err)
		}
	}
}

func TestParseBindingRejects(t *testing.T) {
	bad := []string{
		"",     // 空串
		"   ",  // 只有空白
		"ctrl", // 只有修饰键没有主键
		"ctrl+alt+shift+win",
		"ctrl+",       // 尾部空 token
		"+m",          // 头部空 token
		"ctrl++m",     // 中间空 token
		"ctrl+ctrl+m", // 修饰键重复
		"shift+shift+a",
		"m+n",          // 两个主键
		"ctrl+m+alt+k", // 修饰键夹在两个主键之间
		"bogus",        // 未知 token
		"ctrl+bogus",
		"mouse6", // 超出鼠标键范围
		"mouse0",
		"f25", // 超出功能键范围
		"f0",
		"escape", // 合法键名只有 esc（契约锁定，不做别名）
		"pageup",
		"return",
		"backspace",
		"a b",       // token 内含空白
		"ctrl+alt+", // 尾部空 token
	}
	for _, spec := range bad {
		if b, err := ParseBinding(spec); err == nil {
			t.Errorf("ParseBinding(%q) 应失败，却得到 %q", spec, b.String())
		}
	}
}

func TestBindingIsZero(t *testing.T) {
	var b Binding
	if !b.IsZero() {
		t.Error("零值 Binding 应报告 IsZero")
	}
	if b.String() != "" {
		t.Errorf("零值 Binding.String() = %q，期望空串", b.String())
	}
	parsed, err := ParseBinding("ctrl+m")
	if err != nil {
		t.Fatal(err)
	}
	if parsed.IsZero() {
		t.Error("已解析的 Binding 不应报告 IsZero")
	}
}

func TestParseMode(t *testing.T) {
	cases := []struct {
		in   string
		want Mode
	}{
		{"ptt", ModePTT},
		{"voice", ModeVoice},
		{"open", ModeOpen},
		{"PTT", ModePTT},
		{" Open ", ModeOpen},
	}
	for _, c := range cases {
		got, err := ParseMode(c.in)
		if err != nil {
			t.Fatalf("ParseMode(%q) 失败: %v", c.in, err)
		}
		if got != c.want {
			t.Errorf("ParseMode(%q) = %d，期望 %d", c.in, got, c.want)
		}
	}
	for _, bad := range []string{"", "  ", "pt", "vad", "push-to-talk"} {
		if _, err := ParseMode(bad); err == nil {
			t.Errorf("ParseMode(%q) 应失败", bad)
		}
	}
}

func TestModeString(t *testing.T) {
	cases := map[Mode]string{
		ModePTT:   "ptt",
		ModeVoice: "voice",
		ModeOpen:  "open",
		Mode(99):  "ptt", // 越界值按零档输出，与 Sanitize 一致
	}
	for m, want := range cases {
		if got := m.String(); got != want {
			t.Errorf("Mode(%d).String() = %q，期望 %q", uint32(m), got, want)
		}
	}
	// 合法模式的 String() 必须能被 ParseMode 吃回去
	for _, m := range []Mode{ModePTT, ModeVoice, ModeOpen} {
		got, err := ParseMode(m.String())
		if err != nil {
			t.Fatalf("ParseMode(%q) 失败: %v", m.String(), err)
		}
		if got != m {
			t.Errorf("round-trip 模式 %d → %d", m, got)
		}
	}
}
