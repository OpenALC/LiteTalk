package hotkey

import (
	"encoding/json"
	"testing"
)

func TestDefaultSettings(t *testing.T) {
	s := DefaultSettings()
	if s.Enabled {
		t.Error("默认应关闭：全局钩子侵入性强")
	}
	if s.Mode != ModePTT {
		t.Errorf("默认模式 = %d，期望 ModePTT", s.Mode)
	}
	if s.PTT != "" || s.Mute != "" || s.Deafen != "" || s.Dual != "" {
		t.Errorf("默认绑定应全空，实际 %+v", s)
	}
	if s.TailMS != DefaultTailMS {
		t.Errorf("默认尾包 = %d，期望 %d", s.TailMS, DefaultTailMS)
	}
	if s.CueSound {
		t.Error("默认不应开提示音")
	}
}

func TestSanitizeConverges(t *testing.T) {
	in := Settings{
		Enabled:  true,
		Mode:     Mode(7),           // 越界 → ModePTT
		PTT:      "Shift+Ctrl+M",    // 合法 → 规范化
		Mute:     "ctrl+ctrl+m",     // 非法 → 清空
		Deafen:   "bogus",           // 非法 → 清空
		Dual:     "ctrl+alt+mouse5", // 合法 → 原样（已规范化）
		TailMS:   -5,                // 负 → 0
		CueSound: true,
	}
	got := in.Sanitize()
	if got.Mode != ModePTT {
		t.Errorf("Mode = %d，期望 ModePTT", got.Mode)
	}
	if got.PTT != "ctrl+shift+m" {
		t.Errorf("PTT = %q，期望 ctrl+shift+m", got.PTT)
	}
	if got.Mute != "" || got.Deafen != "" {
		t.Errorf("非法绑定应清空，实际 Mute=%q Deafen=%q", got.Mute, got.Deafen)
	}
	if got.Dual != "ctrl+alt+mouse5" {
		t.Errorf("Dual = %q，期望 ctrl+alt+mouse5", got.Dual)
	}
	if got.TailMS != 0 {
		t.Errorf("TailMS = %d，期望 0", got.TailMS)
	}
	// 收敛不得动这两个开关
	if !got.Enabled || !got.CueSound {
		t.Errorf("Enabled/CueSound 不应被改动：%+v", got)
	}
	// 入参不得被改（值接收者的语义保证）
	if in.Mode != Mode(7) || in.PTT != "Shift+Ctrl+M" {
		t.Error("Sanitize 不应修改入参")
	}
}

func TestSanitizeClampsTail(t *testing.T) {
	cases := []struct{ in, want int }{
		{-1, 0},
		{0, 0},
		{1, 1},
		{250, 250},
		{1000, 1000},
		{1001, 1000},
		{99999, maxTailMS},
	}
	for _, c := range cases {
		got := Settings{TailMS: c.in}.Sanitize()
		if got.TailMS != c.want {
			t.Errorf("Sanitize(TailMS=%d).TailMS = %d，期望 %d", c.in, got.TailMS, c.want)
		}
	}
}

func TestSanitizeIdempotent(t *testing.T) {
	in := Settings{
		Enabled: true,
		Mode:    Mode(42),
		PTT:     " WIN + F13 ",
		Mute:    "ctrl+ctrl+m",
		Dual:    "ctrl+alt+mouse5",
		TailMS:  5000,
	}
	once := in.Sanitize()
	twice := once.Sanitize()
	if once != twice {
		t.Errorf("Sanitize 不幂等：%+v vs %+v", once, twice)
	}
	if once.PTT != "win+f13" {
		t.Errorf("PTT = %q，期望 win+f13", once.PTT)
	}
}

func TestSettingsJSONTags(t *testing.T) {
	raw := `{"enabled":true,"mode":1,"ptt":"mouse4","mute":"ctrl+m","deafen":"","dual":"ctrl+alt+mouse5","tail_ms":300,"cue_sound":true}`
	var s Settings
	if err := json.Unmarshal([]byte(raw), &s); err != nil {
		t.Fatalf("Unmarshal 失败: %v", err)
	}
	if !s.Enabled || s.Mode != ModeVoice || s.PTT != "mouse4" || s.Mute != "ctrl+m" {
		t.Errorf("反序列化结果不符：%+v", s)
	}
	if s.TailMS != 300 || !s.CueSound {
		t.Errorf("反序列化结果不符：%+v", s)
	}

	// omitempty：空绑定/零尾包/关提示音都不该出现在 json 里
	out, err := json.Marshal(Settings{Enabled: true, Mode: ModePTT})
	if err != nil {
		t.Fatal(err)
	}
	const want = `{"enabled":true,"mode":0}`
	if string(out) != want {
		t.Errorf("Marshal = %s，期望 %s", out, want)
	}
}

func TestCheckBindings(t *testing.T) {
	ok := Settings{PTT: "ctrl+m", Mute: "Shift+Ctrl+M", Dual: "ctrl+alt+mouse5"}
	if err := checkBindings(ok); err != nil {
		t.Errorf("合法绑定应通过：%v", err)
	}
	bad := []Settings{
		{PTT: "bogus"},
		{Mute: "ctrl+ctrl+m"},
		{Deafen: "m+n"},
		{Dual: "ctrl"},
	}
	for _, s := range bad {
		if err := checkBindings(s); err == nil {
			t.Errorf("非法绑定应报错：%+v", s)
		}
	}
	// 空串 = 未绑定，不算错
	if err := checkBindings(Settings{}); err != nil {
		t.Errorf("全空绑定应通过：%v", err)
	}
}
