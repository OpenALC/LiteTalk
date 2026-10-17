package main

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/OpenALC/LiteTalk/client/internal/config"
	"github.com/OpenALC/LiteTalk/client/internal/hotkey"
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

// 验证服务器地址配置：保存 / WS 推导 / 来源（热切换需探活通过才落盘）
func TestServerConfig(t *testing.T) {
	m := config.NewWithPath(filepath.Join(t.TempDir(), "config.json"))
	app := newAppWithConfig(m)

	// 默认值。
	v := app.GetServerConfig()
	if v.BaseURL != config.DefaultBaseURL || v.Source != "default" {
		t.Fatalf("默认解析错误: %+v", v)
	}

	// 保存后生效（file 来源），WS 自动推导 wss。热切换先探活，mock healthz。
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"ok","version":"test"}`))
	}))
	defer srv.Close()
	if err := app.SetServerConfig(srv.URL); err != nil {
		t.Fatalf("SetServerConfig: %v", err)
	}
	v = app.GetServerConfig()
	if v.BaseURL != srv.URL {
		t.Fatalf("保存后解析错误: %+v", v)
	}
	if !strings.HasPrefix(v.WSURL, "ws://") || !strings.HasSuffix(v.WSURL, "/ws") {
		t.Fatalf("WS 推导错误: %s", v.WSURL)
	}
	if v.Source != "file" {
		t.Fatalf("来源应为 file，得到 %s", v.Source)
	}

	// 非法地址拒绝保存（不探活，先校验）。
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

// TestHotkeyEffectTriggerEdges 钉住「哪个沿触发什么」——这个不变量没有测试时，
// 闭麦/静音/全关的分支漏判 ev.Pressed，实际行为变成「按住闭麦、松开恢复说话」，
// 与用户对快捷键的预期完全相反，且只有真机连按才能发现。
func TestHotkeyEffectTriggerEdges(t *testing.T) {
	// PTT：两个沿都要响应。少任何一个都会变成开关或卡在说话。
	if e := effectOf(hotkey.Event{Action: hotkey.ActionPTT, Pressed: true}); !e.pttPress || e.pttRelease {
		t.Errorf("PTT 按下应只产生 pttPress，实际 %+v", e)
	}
	if e := effectOf(hotkey.Event{Action: hotkey.ActionPTT, Pressed: false}); !e.pttRelease || e.pttPress {
		t.Errorf("PTT 抬起应只产生 pttRelease，实际 %+v", e)
	}

	// 锁存开关：只有按下沿触发，抬起沿必须是空操作。
	// 完整一次「按下+抬起」只能产生一次切换。
	for _, a := range []hotkey.Action{hotkey.ActionMute, hotkey.ActionDeafen, hotkey.ActionDual} {
		press := effectOf(hotkey.Event{Action: a, Pressed: true})
		release := effectOf(hotkey.Event{Action: a, Pressed: false})
		n := boolToInt(press.mute) + boolToInt(press.deafen) + boolToInt(press.dual)
		if n != 1 {
			t.Errorf("%s 按下应恰好触发一个动作，实际 %+v", a, press)
		}
		if release != (hotkeyEffect{}) {
			t.Errorf("%s 抬起不应触发任何动作，实际 %+v", a, release)
		}
	}

	// 未知动作两个沿都必须是空操作，不能误触。
	for _, pressed := range []bool{true, false} {
		if e := effectOf(hotkey.Event{Action: hotkey.Action("nope"), Pressed: pressed}); e != (hotkeyEffect{}) {
			t.Errorf("未知动作(pressed=%v) 不应有效果，实际 %+v", pressed, e)
		}
	}
}

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

// TestSetServerConfigHotSwitch 验证热切换：探活通过 → 落盘 + 重定向 API 基址；
// 探活失败 → 不动原地址，返回错误。
func TestSetServerConfigHotSwitch(t *testing.T) {
	// 模拟新服务器：healthz 返回 200。
	ok := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/healthz" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"ok","version":"test"}`))
	}))
	defer ok.Close()

	m := config.NewWithPath(filepath.Join(t.TempDir(), "config.json"))
	app := newAppWithConfig(m)
	app.api = state.NewAPIClient("http://127.0.0.1:1") // 初始地址

	// 成功切换。
	if err := app.SetServerConfig(ok.URL); err != nil {
		t.Fatalf("SetServerConfig: %v", err)
	}
	v := app.GetServerConfig()
	if v.BaseURL != ok.URL {
		t.Fatalf("切换后地址错误: %s（期望 %s）", v.BaseURL, ok.URL)
	}
	if got := app.api.Base(); got != ok.URL {
		t.Fatalf("API 基址未重定向: %s（期望 %s）", got, ok.URL)
	}

	// 失败切换：不可达地址 → 报错，原地址不动。
	if err := app.SetServerConfig("http://127.0.0.1:1"); err == nil {
		t.Fatal("不可达地址应报错")
	}
	v2 := app.GetServerConfig()
	if v2.BaseURL != ok.URL {
		t.Fatalf("切换失败后应保持原地址 %s，实际 %s", ok.URL, v2.BaseURL)
	}
	if got := app.api.Base(); got != ok.URL {
		t.Fatalf("切换失败后 API 基址应保持 %s，实际 %s", ok.URL, got)
	}
}

// TestSetServerConfigEnvOverride 环境变量 LITETALK_API 优先于 config.json：
// SetServerConfig 写盘但不改变生效地址（Resolve 仍取 env），也不触发热切换。
func TestSetServerConfigEnvOverride(t *testing.T) {
	envSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"ok","version":"env"}`))
	}))
	defer envSrv.Close()
	fileSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"ok","version":"file"}`))
	}))
	defer fileSrv.Close()

	t.Setenv("LITETALK_API", envSrv.URL)
	m := config.NewWithPath(filepath.Join(t.TempDir(), "config.json"))
	app := newAppWithConfig(m)
	app.api = state.NewAPIClient(envSrv.URL)

	// SetServerConfig 写 fileSrv 地址到盘；Resolve 仍返回 envSrv。
	if err := app.SetServerConfig(fileSrv.URL); err != nil {
		t.Fatalf("SetServerConfig: %v", err)
	}
	v := app.GetServerConfig()
	if v.BaseURL != envSrv.URL {
		t.Fatalf("env 覆盖下生效地址应为 %s，实际 %s", envSrv.URL, v.BaseURL)
	}
	if got := app.api.Base(); got != envSrv.URL {
		t.Fatalf("env 覆盖下 API 基址不应变化：%s（期望 %s）", got, envSrv.URL)
	}
}
