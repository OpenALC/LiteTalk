package config

import (
	"os"
	"path/filepath"
	"testing"
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
