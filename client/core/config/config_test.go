package config

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/OpenALC/LiteTalk/client/core/hotkey"
)

func tmpManager(t *testing.T) *Manager {
	t.Helper()
	return NewWithPath(filepath.Join(t.TempDir(), "LiteTalk", "config.json"))
}

func TestSaveLoad(t *testing.T) {
	m := tmpManager(t)
	hib := true
	if err := m.Save(Config{ServerBase: "https://a.example.com", HibernateOnMinimize: &hib}); err != nil {
		t.Fatalf("Save: %v", err)
	}
	// 新实例重新读盘验证持久化。
	m2 := NewWithPath(m.Path())
	c := m2.Get()
	if c.ServerBase != "https://a.example.com" {
		t.Fatalf("ServerBase = %q", c.ServerBase)
	}
	if c.HibernateOnMinimize == nil || !*c.HibernateOnMinimize {
		t.Fatal("HibernateOnMinimize 未持久化")
	}
}

// TestCloseToTray 关闭行为默认托盘、可切换持久化（问题 4b）。
func TestCloseToTray(t *testing.T) {
	m := tmpManager(t)
	if !m.CloseToTray() {
		t.Fatal("默认应为最小化到托盘")
	}
	if err := m.SetCloseToTray(false); err != nil {
		t.Fatalf("SetCloseToTray: %v", err)
	}
	m2 := NewWithPath(m.Path())
	if m2.CloseToTray() {
		t.Fatal("关闭行为未持久化为退出程序")
	}
}

// TestAudioSettings 语音设置：默认值回落 + 持久化往返 + v0.1.2 新字段与旧版兼容映射。
func TestAudioSettings(t *testing.T) {
	m := tmpManager(t)

	// 未设置：全部回落默认（v0.1.2：强度中档 2、VAD 中档 2、复杂度 9、双音量 100；
	// v1.1：AGC 默认开、输入声道 auto）。
	d := m.GetAudioSettings()
	if d.BitrateKbps != 32 || !*d.VBR || !*d.DTX || !*d.FEC {
		t.Fatalf("默认 Opus 参数异常: %+v", d)
	}
	if d.PacketLoss != 10 || !*d.Denoise || d.DenoiseStrength != 2 ||
		!*d.AGC || *d.VADSens != 2 || d.Complexity != 9 ||
		d.MicVolume != 100 || d.PlaybackVolume != 100 || d.InputMode != "auto" ||
		d.DisableAEC {
		t.Fatalf("默认 DSP 参数异常: %+v", d)
	}

	// 写入自定义值（灵敏度 0=最低档 需能往返，不能用零值当"未设置"）。
	f, tr := false, true
	low := 0
	custom := AudioSettings{
		BitrateKbps: 48, VBR: &f, DTX: &tr, FEC: &f,
		PacketLoss: 15, Denoise: &f, DenoiseStrength: 3, AGC: &tr,
		VADSens: &low, Complexity: 5,
		MicVolume: 60, PlaybackVolume: 120, InputMode: "right", PreGainDB: 6,
		DisableAEC: true,
	}
	if err := m.SetAudioSettings(custom); err != nil {
		t.Fatalf("SetAudioSettings: %v", err)
	}
	m2 := NewWithPath(m.Path())
	got := m2.GetAudioSettings()
	if got.BitrateKbps != 48 || *got.VBR || !*got.DTX || *got.FEC {
		t.Fatalf("Opus 设置未持久化: %+v", got)
	}
	if got.PacketLoss != 15 || *got.Denoise || got.DenoiseStrength != 3 ||
		!*got.AGC || *got.VADSens != 0 || got.Complexity != 5 ||
		got.MicVolume != 60 || got.PlaybackVolume != 120 || got.InputMode != "right" ||
		got.PreGainDB != 6 || !got.DisableAEC {
		t.Fatalf("DSP 设置未持久化: %+v", got)
	}

	// 读路径收敛：手改 config.json 的越界值回落默认。
	if err := m.Update(func(c *Config) error {
		c.Audio = &AudioSettings{BitrateKbps: 999, PacketLoss: 250, Gain: 999, MicVolume: 250, Complexity: 99, InputMode: "bogus", PreGainDB: 999}
		return nil
	}); err != nil {
		t.Fatalf("注入越界值: %v", err)
	}
	got = m.GetAudioSettings()
	if got.BitrateKbps != 32 || got.PacketLoss != 10 || got.Gain != 10 ||
		got.MicVolume != 120 || got.Complexity != 9 || got.InputMode != "auto" ||
		got.PreGainDB != PreGainMaxDB {
		t.Fatalf("越界值未收敛: %+v", got)
	}
}

// TestAudioSettingsLegacy 旧版字段兼容：vad_sens 旧三档在「加载时」一次性迁移 0/2/4
// （带 VadMigrated 标记幂等，不做每次读取的映射）；旧 Gain×10 换算麦克风音量并 clamp 120。
// 模拟方式：直接写旧版 config.json 到盘，再构造 Manager（迁移发生在加载期）。
func TestAudioSettingsLegacy(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	old := `{"audio":{"gain":15,"vad_sens":1}}`
	if err := os.WriteFile(path, []byte(old), 0o644); err != nil {
		t.Fatal(err)
	}
	m := NewWithPath(path)
	got := m.GetAudioSettings()
	if *got.VADSens != 2 {
		t.Fatalf("旧 VADSens 1 应在加载时迁移为 2，实际 %d", *got.VADSens)
	}
	if got.MicVolume != 120 {
		t.Fatalf("旧 Gain 15 应换算并 clamp 到 MicVolume 120，实际 %d", got.MicVolume)
	}
	// 幂等：加载迁移已落盘（vad_sens=2 + 标记），重新加载不得二次映射。
	m2 := NewWithPath(path)
	if v := *m2.GetAudioSettings().VADSens; v != 2 {
		t.Fatalf("迁移应幂等，重新加载 vad_sens=%d", v)
	}
	// 新刻度保护：迁移后用户设五档刻度 1（较低档），重启不得被旧映射污染。
	three := 1
	if err := m2.SetAudioSettings(AudioSettings{VADSens: &three}); err != nil {
		t.Fatal(err)
	}
	m3 := NewWithPath(path)
	if v := *m3.GetAudioSettings().VADSens; v != 1 {
		t.Fatalf("新刻度 1 不应被旧映射改写，实际 %d", v)
	}
}

// TestHotkeySettings 全局热键设置三段式：默认回落 / 自定义往返（新实例重读盘）/
// 读路径越界收敛（手改 config.json）。
func TestHotkeySettings(t *testing.T) {
	m := tmpManager(t)

	// 未配置：回落出厂值（默认关闭、PTT 模式、无绑定、尾包 250ms）。
	d := m.GetHotkeySettings()
	if d.Enabled || d.Mode != hotkey.ModePTT || d.TailMS != hotkey.DefaultTailMS ||
		d.PTT != "" || d.Mute != "" || d.Deafen != "" || d.Dual != "" || d.CueSound {
		t.Fatalf("默认热键设置异常: %+v", d)
	}
	if DefaultHotkeySettings() != d {
		t.Fatalf("DefaultHotkeySettings 与读路径不一致: %+v vs %+v", DefaultHotkeySettings(), d)
	}

	// 自定义值往返。tail_ms=0 必须能往返：该字段没有 omitempty，0 是用户主动
	// 关尾包，不是"没写这个字段"。
	custom := hotkey.Settings{
		Enabled: true, Mode: hotkey.ModeVoice,
		PTT: "ctrl+shift+m", Mute: "mouse4", Deafen: "f13", Dual: "ctrl+alt+mouse5",
		TailMS: 0, CueSound: true,
	}
	if err := m.SetHotkeySettings(custom); err != nil {
		t.Fatalf("SetHotkeySettings: %v", err)
	}
	m2 := NewWithPath(m.Path())
	got := m2.GetHotkeySettings()
	if got != custom {
		t.Fatalf("热键设置未按原样持久化: got %+v want %+v", got, custom)
	}

	// 读路径收敛：手改 config.json 的越界值（模式越界→PTT、尾包夹到 [0,1000]、
	// 非法绑定按未绑定清空）。
	if err := m.Update(func(c *Config) error {
		c.Hotkeys = &hotkey.Settings{
			Enabled: true, Mode: hotkey.Mode(9),
			PTT: "ctrl+ctrl+m", Mute: "boguskey", Deafen: "", Dual: "shift",
			TailMS: 5000,
		}
		return nil
	}); err != nil {
		t.Fatalf("注入越界值: %v", err)
	}
	got = m.GetHotkeySettings()
	if got.Mode != hotkey.ModePTT || got.TailMS != 1000 {
		t.Fatalf("越界值未收敛: %+v", got)
	}
	if got.PTT != "" || got.Mute != "" || got.Dual != "" {
		t.Fatalf("非法绑定应被清空: %+v", got)
	}
}

// TestUIPrefs v0.1.2 界面偏好：UI 销毁休眠/首页服务器/介绍页隐藏。
func TestUIPrefs(t *testing.T) {
	m := tmpManager(t)
	if m.UIDestroyOnHibernate() || m.IntroHidden() || m.HomeServerID() != "" {
		t.Fatal("默认值应为 关/未隐藏/介绍页")
	}
	if err := m.SetUIDestroyOnHibernate(true); err != nil {
		t.Fatal(err)
	}
	if err := m.SetHomeServerID("srv-42"); err != nil {
		t.Fatal(err)
	}
	if err := m.SetIntroHidden(true); err != nil {
		t.Fatal(err)
	}
	m2 := NewWithPath(m.Path())
	if !m2.UIDestroyOnHibernate() || m2.HomeServerID() != "srv-42" || !m2.IntroHidden() {
		t.Fatalf("界面偏好未持久化: %+v", m2.Get())
	}
}

// TestSavedCredentials 凭据存取与清除（问题 6b）。
// Windows 上密码经 DPAPI 往返还原；非 Windows 只记邮箱（密码为空）。
func TestSavedCredentials(t *testing.T) {
	m := tmpManager(t)
	if c := m.GetSavedCredentials(); c.Saved {
		t.Fatal("初始不应有已存凭据")
	}
	if err := m.SetSavedCredentials("u@example.com", "p@ssw0rd123"); err != nil {
		t.Fatalf("SetSavedCredentials: %v", err)
	}
	m2 := NewWithPath(m.Path())
	c := m2.GetSavedCredentials()
	if !c.Saved || c.Email != "u@example.com" {
		t.Fatalf("凭据读回异常: %+v", c)
	}
	if c.Password != "" && c.Password != "p@ssw0rd123" {
		t.Fatalf("密码解密不一致: %q", c.Password)
	}
	if err := m2.ClearSavedCredentials(); err != nil {
		t.Fatalf("ClearSavedCredentials: %v", err)
	}
	if c := NewWithPath(m.Path()).GetSavedCredentials(); c.Saved {
		t.Fatal("清除后仍返回已存凭据")
	}
}

// TestGuestRefresh 游客 refresh_token 存取与清除。
// Windows 上经 DPAPI 往返还原；非 Windows 加密失败则读回空（视为无记住的游客）。
func TestGuestRefresh(t *testing.T) {
	m := tmpManager(t)
	if got := m.GetGuestRefresh(); got != "" {
		t.Fatal("初始不应有已存游客令牌")
	}
	if err := m.SetGuestRefresh("refresh-token-abc"); err != nil {
		t.Fatalf("SetGuestRefresh: %v", err)
	}
	m2 := NewWithPath(m.Path())
	got := m2.GetGuestRefresh()
	if got != "" && got != "refresh-token-abc" {
		t.Fatalf("令牌解密不一致: %q", got)
	}
	if err := m2.SetGuestRefresh(""); err != nil {
		t.Fatalf("清除游客令牌: %v", err)
	}
	if got := NewWithPath(m.Path()).GetGuestRefresh(); got != "" {
		t.Fatal("清除后仍返回游客令牌")
	}
}

func TestCorruptFileFallsBack(t *testing.T) {
	m := tmpManager(t)
	if err := os.MkdirAll(filepath.Dir(m.Path()), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(m.Path(), []byte("{not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	m2 := NewWithPath(m.Path())
	if c := m2.Get(); c.ServerBase != "" || c.HibernateOnMinimize != nil {
		t.Fatalf("损坏文件应回默认值零值配置，得到 %+v", c)
	}
}

func TestDeriveWS(t *testing.T) {
	cases := []struct {
		base string
		want string
		ok   bool
	}{
		{"http://127.0.0.1:8077", "ws://127.0.0.1:8077/ws", true},
		{"https://example.com", "wss://example.com/ws", true},
		{"https://example.com/", "wss://example.com/ws", true},
		{"https://example.com/ws", "wss://example.com/ws", true}, // 已带 /ws 不重复拼
		{"https://example.com/chat", "wss://example.com/chat/ws", true},
		{"ftp://example.com", "", false},
		{"not a url", "", false},
	}
	for _, c := range cases {
		got, err := DeriveWS(c.base)
		if c.ok && (err != nil || got != c.want) {
			t.Errorf("DeriveWS(%q) = %q, %v; want %q", c.base, got, err, c.want)
		}
		if !c.ok && err == nil {
			t.Errorf("DeriveWS(%q) 应报错，得到 %q", c.base, got)
		}
	}
}

func TestResolvePriority(t *testing.T) {
	m := tmpManager(t)
	t.Setenv(EnvAPI, "")
	t.Setenv(EnvWS, "")

	// 全默认。
	r := m.Resolve()
	if r.BaseURL != DefaultBaseURL || r.WSURL != DefaultWSURL || r.Source != SourceDefault {
		t.Fatalf("default 解析错误: %+v", r)
	}

	// 文件覆盖默认。
	_ = m.Save(Config{ServerBase: "https://file.example.com"})
	r = m.Resolve()
	if r.BaseURL != "https://file.example.com" || r.WSURL != "wss://file.example.com/ws" || r.Source != SourceFile {
		t.Fatalf("file 解析错误: %+v", r)
	}

	// 环境变量覆盖一切。
	t.Setenv(EnvAPI, "https://env.example.com")
	r = m.Resolve()
	if r.BaseURL != "https://env.example.com" || r.Source != SourceEnv {
		t.Fatalf("env 解析错误: %+v", r)
	}

	// LITETALK_WS 单独覆盖 WS。
	t.Setenv(EnvWS, "wss://ws.example.com/ws")
	r = m.Resolve()
	if r.WSURL != "wss://ws.example.com/ws" {
		t.Fatalf("env ws 覆盖错误: %+v", r)
	}
}

func TestValidateBase(t *testing.T) {
	if err := ValidateBase("https://a.com"); err != nil {
		t.Fatal(err)
	}
	if err := ValidateBase("http://127.0.0.1:8077"); err != nil {
		t.Fatal(err)
	}
	if err := ValidateBase("ftp://x"); err == nil {
		t.Fatal("ftp 应报错")
	}
	if err := ValidateBase("nope"); err == nil {
		t.Fatal("非法地址应报错")
	}
}
