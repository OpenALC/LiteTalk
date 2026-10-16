package main

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/OpenALC/LiteTalk/client/internal/config"
	"github.com/OpenALC/LiteTalk/client/internal/state"
)

// 验证休眠模式三态（off/hibernate/destroy）的读写、持久化与非法值校验。
func TestHibernateMode(t *testing.T) {
	m := config.NewWithPath(filepath.Join(t.TempDir(), "config.json"))
	app := newAppWithConfig(m)

	if app.GetHibernateMode() != "off" {
		t.Fatal("默认值应为 off")
	}
	if err := app.SetHibernateMode("destroy"); err != nil {
		t.Fatalf("SetHibernateMode: %v", err)
	}
	if app.GetHibernateMode() != "destroy" {
		t.Fatal("设置 destroy 后读取应为 destroy")
	}
	// 持久化：新实例读盘恢复。
	app2 := newAppWithConfig(config.NewWithPath(m.Path()))
	if app2.GetHibernateMode() != "destroy" {
		t.Fatal("休眠模式应已持久化到 config.json")
	}
	if err := app.SetHibernateMode("nope"); err == nil {
		t.Fatal("非法模式应报错")
	}
	if app.GetHibernateMode() != "destroy" {
		t.Fatal("非法值不得改动已存模式")
	}
}

// 验证旧版双布尔开关 → 三态休眠模式的一次性迁移。
func TestHibernateModeMigration(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	legacy := `{"hibernate_on_minimize":true,"ui_destroy_on_hibernate":true}`
	if err := os.WriteFile(path, []byte(legacy), 0o644); err != nil {
		t.Fatal(err)
	}
	m := config.NewWithPath(path)
	if got := m.HibernateMode(); got != "destroy" {
		t.Fatalf("旧版双开应迁移为 destroy，得到 %s", got)
	}
	// 幂等：再次加载不变。
	m2 := config.NewWithPath(path)
	if got := m2.HibernateMode(); got != "destroy" {
		t.Fatalf("迁移应幂等，得到 %s", got)
	}
}

// 验证版本号默认值
func TestGetVersion(t *testing.T) {
	app := NewApp()
	if app.GetVersion() == "" {
		t.Fatal("版本号不应为空")
	}
}

// 验证服务器地址配置：保存 / WS 推导 / 来源
func TestServerConfig(t *testing.T) {
	m := config.NewWithPath(filepath.Join(t.TempDir(), "config.json"))
	app := newAppWithConfig(m)

	// 默认值。
	v := app.GetServerConfig()
	if v.BaseURL != config.DefaultBaseURL || v.Source != "default" {
		t.Fatalf("默认解析错误: %+v", v)
	}

	// 保存后生效（file 来源），WS 自动推导 wss。
	if err := app.SetServerConfig("https://talk.example.com"); err != nil {
		t.Fatalf("SetServerConfig: %v", err)
	}
	v = app.GetServerConfig()
	if v.BaseURL != "https://talk.example.com" || v.WSURL != "wss://talk.example.com/ws" {
		t.Fatalf("保存后解析错误: %+v", v)
	}
	if v.Source != "file" {
		t.Fatalf("来源应为 file，得到 %s", v.Source)
	}

	// 非法地址拒绝保存。
	if err := app.SetServerConfig("ftp://x"); err == nil {
		t.Fatal("非法 scheme 应报错")
	}
	if err := app.SetServerConfig("nope"); err == nil {
		t.Fatal("非法地址应报错")
	}
}

// 验证连通性测试对非法地址的快速失败。
func TestServerConnectionBadURL(t *testing.T) {
	app := newAppWithConfig(config.NewWithPath(filepath.Join(t.TempDir(), "config.json")))
	r := app.TestServerConnection("not-a-url")
	if r.OK || r.ErrorMsg == "" {
		t.Fatalf("非法地址应返回失败结果: %+v", r)
	}
}

// TestLoginAsGuestNetworkErrorKeepsToken 网络错误（5xx）时不得清除已存游客
// refresh_token：令牌可能仍有效，下次启动续用同一身份（S6 回归）。
func TestLoginAsGuestNetworkErrorKeepsToken(t *testing.T) {
	m := config.NewWithPath(filepath.Join(t.TempDir(), "config.json"))
	app := newAppWithConfig(m)
	if err := m.SetGuestRefresh("saved-refresh-token"); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"code":"internal","message":"boom"}`))
	}))
	defer srv.Close()
	app.api = state.NewAPIClient(srv.URL)

	if _, err := app.LoginAsGuest(); err == nil {
		t.Fatal("服务端 500 时登录应失败")
	}
	if got := m.GetGuestRefresh(); got != "saved-refresh-token" {
		t.Fatalf("网络错误不得清除已存游客令牌，得到 %q", got)
	}
}

// TestLoginAsGuest401ClearsToken 明确 401（过期/被吊销）才清令牌并走新建。
func TestLoginAsGuest401ClearsToken(t *testing.T) {
	m := config.NewWithPath(filepath.Join(t.TempDir(), "config.json"))
	app := newAppWithConfig(m)
	if err := m.SetGuestRefresh("dead-refresh-token"); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/v1/auth/refresh":
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"code":"unauthorized","message":"refresh_token 无效或已吊销"}`))
		default:
			// 新建游客也失败：让 LoginAsGuest 在连接信令前返回，避免慢路径。
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = w.Write([]byte(`{"code":"internal","message":"boom"}`))
		}
	}))
	defer srv.Close()
	app.api = state.NewAPIClient(srv.URL)

	if _, err := app.LoginAsGuest(); err == nil {
		t.Fatal("refresh 401 + guest 500 时登录应失败")
	}
	if got := m.GetGuestRefresh(); got != "" {
		t.Fatalf("明确 401 应清除已存游客令牌，得到 %q", got)
	}
}

// TestOnKickedClearsSessionAndTokens 游客被踢：停重试代际、断会话、清内存
// refresh 并作废落盘的游客令牌（契约 §0.5 全链路的 Go 侧清理）。
func TestOnKickedClearsSessionAndTokens(t *testing.T) {
	m := config.NewWithPath(filepath.Join(t.TempDir(), "config.json"))
	app := newAppWithConfig(m)
	if err := m.SetGuestRefresh("guest-rt"); err != nil {
		t.Fatal(err)
	}
	app.api = state.NewAPIClient("http://127.0.0.1:1")
	app.mu.Lock()
	app.refreshTok = "rt"
	app.user = state.UserProfile{ID: 1, Nickname: "游客-0001", IsGuest: true}
	app.sess = state.NewSession(app.api, "tok", "n", state.Events{}, nil)
	app.mu.Unlock()

	genBefore := app.loginGen.Load()
	app.onKicked("login_elsewhere")

	if got := app.loginGen.Load(); got != genBefore+1 {
		t.Fatalf("被踢应使 loginGen+1 停掉信令重试循环，得到 %d（前置 %d）", got, genBefore)
	}
	app.mu.Lock()
	rt, sess := app.refreshTok, app.sess
	app.mu.Unlock()
	if rt != "" {
		t.Fatalf("被踢应清空内存 refreshTok，得到 %q", rt)
	}
	if sess != nil {
		t.Fatal("被踢应断开会话（sess 置 nil）")
	}
	if got := m.GetGuestRefresh(); got != "" {
		t.Fatalf("游客被踢应作废落盘令牌，得到 %q", got)
	}
}

// TestOnKickedKeepsGuestTokenForAccountUser 账号用户被踢不得动落盘的游客
// 令牌——那是另一个身份的凭据，与本次会话无关。
func TestOnKickedKeepsGuestTokenForAccountUser(t *testing.T) {
	m := config.NewWithPath(filepath.Join(t.TempDir(), "config.json"))
	app := newAppWithConfig(m)
	if err := m.SetGuestRefresh("other-guest-rt"); err != nil {
		t.Fatal(err)
	}
	app.api = state.NewAPIClient("http://127.0.0.1:1")
	app.mu.Lock()
	app.refreshTok = "rt"
	app.user = state.UserProfile{ID: 7, Nickname: "alice"}
	app.mu.Unlock()

	app.onKicked("login_elsewhere")

	if got := m.GetGuestRefresh(); got != "other-guest-rt" {
		t.Fatalf("账号用户被踢不得清游客令牌，得到 %q", got)
	}
	app.mu.Lock()
	rt := app.refreshTok
	app.mu.Unlock()
	if rt != "" {
		t.Fatalf("被踢应清空内存 refreshTok，得到 %q", rt)
	}
}
