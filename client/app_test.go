package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/OpenALC/LiteTalk/client/core/api"
	"github.com/OpenALC/LiteTalk/client/core/config"
	"github.com/OpenALC/LiteTalk/client/core/hotkey"
	"github.com/OpenALC/LiteTalk/client/core/voice"
	"github.com/OpenALC/LiteTalk/client/desktop"
	"github.com/coder/websocket"
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

// TestAppAPIClientVersionHeader app 构造的 REST 客户端必须带构建版本头
// （#31 强制升级门）；未注入时是 dev，服务端放行。
func TestAppAPIClientVersionHeader(t *testing.T) {
	var got string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Get("X-LiteTalk-Client")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"auth_mode":"full"}`))
	}))
	defer srv.Close()

	c := newAppAPI(srv.URL)
	if _, err := c.GetServerConfig(); err != nil {
		t.Fatalf("GetServerConfig: %v", err)
	}
	if got != version {
		t.Fatalf("X-LiteTalk-Client = %q，期望构建版本 %q", got, version)
	}
}

// ListMessages 桥接把 REST 消息归一为 chat:message 事件的同口径视图（#38 B4b）：
// ID 十进制字符串、ts 毫秒字符串，历史补拉才能与实时广播按 id 合并。
// #39：编辑/回复/提及字段一并映射。
func TestListMessagesBridge(t *testing.T) {
	var gotPath, gotLimit string
	created := time.Date(2026, 10, 24, 12, 0, 0, 123_000_000, time.UTC)
	edited := created.Add(time.Minute)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotLimit = r.URL.Path, r.URL.Query().Get("limit")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`[{"id":"9001","channel_id":"123","user_id":"7","content":"hi",
			"created_at":"` + created.Format(time.RFC3339Nano) + `","sender_nickname":"甲",
			"edited_at":"` + edited.Format(time.RFC3339Nano) + `","reply_to":"9000",
			"reply_preview":{"id":"9000","user_id":"8","nickname":"乙","content":"原","deleted":false},
			"mentions":["8","9"]}]`))
	}))
	defer srv.Close()

	app := &App{api: newAppAPI(srv.URL)}
	out, err := app.ListMessages("123", 20)
	if err != nil {
		t.Fatalf("ListMessages: %v", err)
	}
	if gotPath != "/api/v1/channels/123/messages" || gotLimit != "20" {
		t.Fatalf("请求路径/limit = %q %q", gotPath, gotLimit)
	}
	if len(out) != 1 {
		t.Fatalf("消息条数 = %d，期望 1", len(out))
	}
	want := ChatMessageView{
		ID: "9001", ChannelID: "123", UserID: "7", Nickname: "甲", Content: "hi",
		TS:       strconv.FormatInt(created.UnixMilli(), 10),
		EditedAt: strconv.FormatInt(edited.UnixMilli(), 10),
		ReplyTo:  "9000",
		ReplyPreview: &ReplyPreviewView{
			ID: "9000", UserID: "8", Nickname: "乙", Content: "原", Deleted: false,
		},
		Mentions: []string{"8", "9"},
	}
	if !reflect.DeepEqual(out[0], want) {
		t.Fatalf("消息视图 = %+v，期望 %+v", out[0], want)
	}

	// 非法 ID 在发请求前拒绝；空/非数字/非正数都不可当频道。
	for _, id := range []string{"", "abc", "0", "-1"} {
		if _, err := app.ListMessages(id, 10); err == nil {
			t.Fatalf("非法 channel id %q 应报错", id)
		}
	}
}

// TestMessageMetaBridge #39 Wails 绑定：单条回读、编辑、软删、提及候选走
// REST 并映射为桥接视图；非法 ID 在发请求前拒绝。
func TestMessageMetaBridge(t *testing.T) {
	deleted := time.Date(2026, 10, 24, 12, 2, 0, 0, time.UTC)
	msgJSON := `{"id":"9001","channel_id":"123","user_id":"7","content":"",` +
		`"created_at":"2026-10-24T12:00:00Z","sender_nickname":"甲",` +
		`"deleted_at":"` + deleted.Format(time.RFC3339Nano) + `",` +
		`"reply_to":"9000","mentions":["8"]}`
	var gotMethod, gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod, gotPath = r.Method, r.URL.Path
		switch {
		case r.Method == http.MethodDelete:
			w.WriteHeader(http.StatusNoContent)
		case r.URL.Path == "/api/v1/channels/123/mention-candidates":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`[{"user_id":"8","nickname":"乙","tag":"0002"}]`))
		default:
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(msgJSON))
		}
	}))
	defer srv.Close()

	app := &App{api: newAppAPI(srv.URL)}
	m, err := app.GetMessage("123", "9001")
	if err != nil {
		t.Fatalf("GetMessage: %v", err)
	}
	if gotMethod != http.MethodGet || gotPath != "/api/v1/channels/123/messages/9001" {
		t.Fatalf("GetMessage 请求 = %s %s", gotMethod, gotPath)
	}
	if m.DeletedAt != strconv.FormatInt(deleted.UnixMilli(), 10) || m.ReplyTo != "9000" ||
		len(m.Mentions) != 1 || m.Mentions[0] != "8" {
		t.Fatalf("GetMessage 视图错误: %+v", m)
	}

	if _, err := app.EditMessage("123", "9001", "改后", []string{"8"}); err != nil {
		t.Fatalf("EditMessage: %v", err)
	}
	if gotMethod != http.MethodPatch || gotPath != "/api/v1/channels/123/messages/9001" {
		t.Fatalf("EditMessage 请求 = %s %s", gotMethod, gotPath)
	}

	if err := app.DeleteMessage("123", "9001"); err != nil {
		t.Fatalf("DeleteMessage: %v", err)
	}
	if gotMethod != http.MethodDelete || gotPath != "/api/v1/channels/123/messages/9001" {
		t.Fatalf("DeleteMessage 请求 = %s %s", gotMethod, gotPath)
	}

	cands, err := app.ListMentionCandidates("123")
	if err != nil {
		t.Fatalf("ListMentionCandidates: %v", err)
	}
	if gotPath != "/api/v1/channels/123/mention-candidates" || len(cands) != 1 ||
		cands[0].UserID != "8" || cands[0].Nickname != "乙" || cands[0].Tag != "0002" {
		t.Fatalf("候选映射错误: %+v", cands)
	}

	// 非法 ID：四个绑定都必须在发请求前报错。
	for _, id := range []string{"", "abc", "0", "-1"} {
		if _, err := app.GetMessage(id, "9001"); err == nil {
			t.Fatalf("GetMessage 非法 channel id %q 应报错", id)
		}
		if _, err := app.EditMessage("123", id, "x", nil); err == nil {
			t.Fatalf("EditMessage 非法 message id %q 应报错", id)
		}
		if err := app.DeleteMessage("123", id); err == nil {
			t.Fatalf("DeleteMessage 非法 message id %q 应报错", id)
		}
		if _, err := app.ListMentionCandidates(id); err == nil {
			t.Fatalf("ListMentionCandidates 非法 channel id %q 应报错", id)
		}
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
	app.api = api.NewAPIClient(srv.URL)

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
	app.api = api.NewAPIClient(srv.URL)

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
	app.api = api.NewAPIClient("http://127.0.0.1:1")
	app.mu.Lock()
	app.refreshTok = "rt"
	app.user = api.UserProfile{ID: 1, Nickname: "游客-0001", IsGuest: true}
	app.sess = voice.NewSession(app.api, "tok", "n", voice.Events{}, nil)
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

// TestLogoutClearsAccessToken 登出必须无条件清 APIClient 的 access token：
// GetMe 在 user 为空时回落 api.Me()，残留有效令牌会把已登出的用户「登回来」
// （被踢后 rt 已空、api.Logout 根本不走，尤其明显）。
func TestLogoutClearsAccessToken(t *testing.T) {
	m := config.NewWithPath(filepath.Join(t.TempDir(), "config.json"))
	app := newAppWithConfig(m)
	app.api = api.NewAPIClient("http://127.0.0.1:1")

	// rt == "" 路径（被踢后前端 logout）：即使不发吊销请求也要清 token。
	app.api.SetToken("stale-access")
	app.mu.Lock()
	app.user = api.UserProfile{ID: 1, Nickname: "a"}
	app.mu.Unlock()
	if err := app.Logout(); err != nil {
		t.Fatalf("Logout: %v", err)
	}
	if got := app.api.Token(); got != "" {
		t.Fatalf("登出后 access token 应清空，得到 %q", got)
	}

	// rt != "" 路径：吊销失败也必须清 token（否则僵尸会话只在离线时复活）。
	app.api.SetToken("stale-access-2")
	app.mu.Lock()
	app.refreshTok = "rt"
	app.user = api.UserProfile{ID: 1, Nickname: "a"}
	app.mu.Unlock()
	if err := app.Logout(); err == nil {
		t.Fatal("吊销端点不可达时 Logout 应返回错误")
	}
	if got := app.api.Token(); got != "" {
		t.Fatalf("吊销失败后 access token 仍应清空，得到 %q", got)
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
	app.api = api.NewAPIClient("http://127.0.0.1:1")
	app.mu.Lock()
	app.refreshTok = "rt"
	app.user = api.UserProfile{ID: 7, Nickname: "alice"}
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
	if e := desktop.EffectOf(hotkey.Event{Action: hotkey.ActionPTT, Pressed: true}); !e.PTTPress || e.PTTRelease {
		t.Errorf("PTT 按下应只产生 PTTPress，实际 %+v", e)
	}
	if e := desktop.EffectOf(hotkey.Event{Action: hotkey.ActionPTT, Pressed: false}); !e.PTTRelease || e.PTTPress {
		t.Errorf("PTT 抬起应只产生 PTTRelease，实际 %+v", e)
	}

	// 锁存开关：只有按下沿触发，抬起沿必须是空操作。
	// 完整一次「按下+抬起」只能产生一次切换。
	for _, a := range []hotkey.Action{hotkey.ActionMute, hotkey.ActionDeafen, hotkey.ActionDual} {
		press := desktop.EffectOf(hotkey.Event{Action: a, Pressed: true})
		release := desktop.EffectOf(hotkey.Event{Action: a, Pressed: false})
		n := boolToInt(press.Mute) + boolToInt(press.Deafen) + boolToInt(press.Dual)
		if n != 1 {
			t.Errorf("%s 按下应恰好触发一个动作，实际 %+v", a, press)
		}
		if release != (desktop.HotkeyEffect{}) {
			t.Errorf("%s 抬起不应触发任何动作，实际 %+v", a, release)
		}
	}

	// 未知动作两个沿都必须是空操作，不能误触。
	for _, pressed := range []bool{true, false} {
		if e := desktop.EffectOf(hotkey.Event{Action: hotkey.Action("nope"), Pressed: pressed}); e != (desktop.HotkeyEffect{}) {
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

// TestHotkeyGateMode 发送门模式必须把「平台能否装钩子」算进去：钩子不可用时
// PTT 永远等不到按键，必须退化为常开，否则上行音频全丢（#93）；
// 语音激活/常开不依赖钩子，不受影响。
func TestHotkeyGateMode(t *testing.T) {
	bound := hotkey.Settings{Enabled: true, Mode: hotkey.ModePTT, PTT: "ctrl+m"}
	cases := []struct {
		name      string
		st        hotkey.Settings
		available bool
		want      hotkey.Mode
	}{
		{"默认配置钩子可用也常开", hotkey.DefaultSettings(), true, hotkey.ModeOpen},
		{"默认配置钩子不可用也常开", hotkey.DefaultSettings(), false, hotkey.ModeOpen},
		{"钩子可用时保持 PTT", bound, true, hotkey.ModePTT},
		{"钩子不可用时 PTT 退化常开", bound, false, hotkey.ModeOpen},
		{"语音激活不依赖钩子", hotkey.Settings{Enabled: true, Mode: hotkey.ModeVoice, PTT: "ctrl+m"}, false, hotkey.ModeVoice},
		{"常开不依赖钩子", hotkey.Settings{Mode: hotkey.ModeOpen}, false, hotkey.ModeOpen},
	}
	for _, c := range cases {
		if got := hotkeyGateMode(c.st, c.available); got != c.want {
			t.Errorf("%s: hotkeyGateMode = %v，期望 %v", c.name, got, c.want)
		}
	}
}

// TestSendGateModeWithoutManager Manager 未初始化（等价于没有全局热键能力）时，
// PTT 也必须退化为常开，不能把上行全部拦掉。
func TestSendGateModeWithoutManager(t *testing.T) {
	app := &App{}
	st := hotkey.Settings{Enabled: true, Mode: hotkey.ModePTT, PTT: "ctrl+m"}
	if got := app.sendGateMode(st); got != hotkey.ModeOpen {
		t.Fatalf("无 Manager 时 PTT 应退化为常开，得到 %v", got)
	}
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
	app.api = api.NewAPIClient("http://127.0.0.1:1") // 初始地址

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

// TestSameOrigin 钉住换服判定的边界：scheme/host 大小写与默认端口必须归一化，
// 只有 path 变化算同源；scheme、host、端口任一不同都是跨 origin（#17）。
func TestSameOrigin(t *testing.T) {
	cases := []struct {
		a, b string
		want bool
	}{
		{"http://Example.com", "http://example.com:80/", true},
		{"HTTP://example.com", "http://example.com", true},
		{"https://example.com", "https://example.com:443/x", true},
		{"http://example.com:8077", "http://example.com:8077/a/b", true},
		{"http://[::1]", "http://[::1]:80", true},
		{"http://example.com", "https://example.com", false},
		{"http://example.com", "http://example.com:8080", false},
		{"http://example.com:8080", "http://example.com:80", false},
		{"http://example.com", "http://other.example.com", false},
		{"", "http://example.com", false},
		{"not-a-url", "not-a-url", false},
	}
	for _, c := range cases {
		if got := sameOrigin(c.a, c.b); got != c.want {
			t.Errorf("sameOrigin(%q, %q) = %v，期望 %v", c.a, c.b, got, c.want)
		}
	}
}

// TestSetServerConfigCrossOriginDropsCredentials 跨 origin 换服不得泄露旧凭据
// （#17 P1 安全）：旧实现先 SetBase 再 Me() 预检，B 返回 401 后 401 自动刷新
// 还会把 A 的 refresh_token 发给 B。现在跨 origin 不向 B 发任何旧令牌，
// 只清本地会话并回登录页。
func TestSetServerConfigCrossOriginDropsCredentials(t *testing.T) {
	type reqRecord struct {
		path string
		auth string
		body string
	}
	record := func(r *http.Request) reqRecord {
		body, _ := io.ReadAll(r.Body)
		return reqRecord{path: r.URL.Path, auth: r.Header.Get("Authorization"), body: string(body)}
	}

	// A：旧服务器。记录请求，切换过程不应再与它交互。
	var aMu sync.Mutex
	var aReqs []reqRecord
	oldSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		aMu.Lock()
		aReqs = append(aReqs, record(r))
		aMu.Unlock()
		http.NotFound(w, r)
	}))
	defer oldSrv.Close()

	// B：新服务器。healthz 放行；/users/me 故意 401，旧实现会由此触发刷新泄露。
	var bMu sync.Mutex
	var bReqs []reqRecord
	newSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		bMu.Lock()
		bReqs = append(bReqs, record(r))
		bMu.Unlock()
		switch {
		case strings.HasSuffix(r.URL.Path, "/api/v1/healthz"):
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"status":"ok","version":"new"}`))
		case strings.HasSuffix(r.URL.Path, "/api/v1/users/me"):
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"code":"unauthorized","message":"rejected"}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer newSrv.Close()

	m := config.NewWithPath(filepath.Join(t.TempDir(), "config.json"))
	app := newAppWithConfig(m)
	app.api = api.NewAPIClient(oldSrv.URL)
	app.api.SetToken("old-access-A")
	// 与 ServiceStartup 相同的注入：401 自动刷新时把 app 层 refresh_token 发给
	// 当前 REST 基址。旧实现会因此把旧服务器的长期凭据交给 B。
	app.api.SetRefreshSource(func(context.Context) (string, error) {
		app.mu.Lock()
		defer app.mu.Unlock()
		return app.refreshTok, nil
	})
	app.mu.Lock()
	app.refreshTok = "old-refresh-A"
	app.user = api.UserProfile{ID: 1, Nickname: "alice", IsGuest: true}
	app.mu.Unlock()
	if err := m.SetGuestRefresh("old-guest-refresh-A"); err != nil {
		t.Fatal(err)
	}

	type appEvent struct {
		name string
		data any
	}
	events := make(chan appEvent, 8)
	app.emitHook = func(name string, data any) {
		select {
		case events <- appEvent{name: name, data: data}:
		default:
		}
	}
	genBefore := app.loginGen.Load()

	if err := app.SetServerConfig(newSrv.URL); err != nil {
		t.Fatalf("SetServerConfig: %v", err)
	}

	// B 从未收到任何旧凭据：不能有 Authorization、不能走 /auth/refresh，
	// 也不能调用 Me() 预检（那正是旧实现的泄露入口）。
	bMu.Lock()
	gotB := append([]reqRecord(nil), bReqs...)
	bMu.Unlock()
	for _, r := range gotB {
		if r.auth != "" {
			t.Errorf("新服务器收到 Authorization=%q（path=%s）", r.auth, r.path)
		}
		if strings.Contains(r.path, "/auth/refresh") {
			t.Errorf("新服务器收到 /auth/refresh 请求（body=%q）", r.body)
		}
		if strings.HasSuffix(r.path, "/api/v1/users/me") {
			t.Errorf("跨 origin 不应调用 Me() 预检（path=%s）", r.path)
		}
		if strings.Contains(r.body, "old-refresh-A") || strings.Contains(r.body, "old-guest-refresh-A") {
			t.Errorf("新服务器收到旧 refresh_token（path=%s body=%q）", r.path, r.body)
		}
	}
	// 旧服务器同样不应被切换流程触发（会话在本地关闭，不做跨服吊销）。
	aMu.Lock()
	gotA := len(aReqs)
	aMu.Unlock()
	if gotA != 0 {
		t.Errorf("切换过程不应请求旧服务器，收到 %d 个请求", gotA)
	}

	if got := app.api.Base(); got != newSrv.URL {
		t.Errorf("REST 基址未切换: %s（期望 %s）", got, newSrv.URL)
	}
	if got := app.api.Token(); got != "" {
		t.Errorf("旧 access token 未清空: %q", got)
	}
	app.mu.Lock()
	rt, user := app.refreshTok, app.user
	app.mu.Unlock()
	if rt != "" {
		t.Errorf("旧 refresh token 未清空: %q", rt)
	}
	if user.ID != 0 || user.Nickname != "" || user.IsGuest {
		t.Errorf("用户档案未清空: %+v", user)
	}
	if got := m.GetGuestRefresh(); got != "" {
		t.Errorf("落盘游客令牌未作废: %q", got)
	}
	if got := app.loginGen.Load(); got <= genBefore {
		t.Errorf("loginGen 应递增以停掉旧重试循环，得到 %d（前置 %d）", got, genBefore)
	}

	select {
	case ev := <-events:
		if ev.name != "auth:kicked" {
			t.Errorf("事件类型 = %s，期望 auth:kicked", ev.name)
		}
		payload, _ := ev.data.(map[string]any)
		if payload["reason"] != "server_changed" {
			t.Errorf("auth:kicked reason = %v，期望 server_changed", payload["reason"])
		}
	default:
		t.Error("未收到 auth:kicked 事件（前端不会回到登录页）")
	}
}

// TestSetServerConfigCrossOriginWaitsInflightRefresh 换服与在飞刷新交错：
// 刷新先完成安装并卡在 onRefreshed（api 层的 baseMu 读锁段内），此时跨 origin
// 换服会阻塞在 SetBaseAndClearToken 的写锁上；放行后 retarget 再清一次 app 层
// 令牌。终态不得残留任何旧凭据，新服务器只能看到 healthz（#17）。
func TestSetServerConfigCrossOriginWaitsInflightRefresh(t *testing.T) {
	type reqRecord struct {
		path string
		auth string
		body string
	}
	record := func(r *http.Request) reqRecord {
		body, _ := io.ReadAll(r.Body)
		return reqRecord{path: r.URL.Path, auth: r.Header.Get("Authorization"), body: string(body)}
	}

	// A：401 后刷新返回旧服务器的新令牌对。
	oldSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/v1/users/me":
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"code":"token_expired","message":"expired"}`))
		case "/api/v1/auth/refresh":
			_, _ = w.Write([]byte(`{"access_token":"tok-new-A","refresh_token":"rt-new-A","expires_in":7200}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer oldSrv.Close()

	// B：只应收到 healthz。
	var bMu sync.Mutex
	var bReqs []reqRecord
	newSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec := record(r)
		bMu.Lock()
		bReqs = append(bReqs, rec)
		bMu.Unlock()
		if strings.HasSuffix(r.URL.Path, "/api/v1/healthz") {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"status":"ok","version":"new"}`))
			return
		}
		http.NotFound(w, r)
	}))
	defer newSrv.Close()

	m := config.NewWithPath(filepath.Join(t.TempDir(), "config.json"))
	app := newAppWithConfig(m)
	app.api = api.NewAPIClient(oldSrv.URL)
	app.api.SetToken("old-access-A")
	app.mu.Lock()
	app.refreshTok = "old-refresh-A"
	app.user = api.UserProfile{ID: 1, Nickname: "alice", IsGuest: true}
	app.mu.Unlock()
	if err := m.SetGuestRefresh("old-guest-refresh-A"); err != nil {
		t.Fatal(err)
	}

	// 与 ServiceStartup 相同的刷新回调，额外卡在 onRefreshed 里：api 的
	// doRefresh 持 baseMu 读锁调用它，因此换服的写锁必须等到放行。
	onRefreshedEntered := make(chan struct{})
	releaseOnRefreshed := make(chan struct{})
	var enteredOnce, releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(releaseOnRefreshed) }) }
	defer release()
	app.api.SetRefreshSource(func(context.Context) (string, error) {
		app.mu.Lock()
		defer app.mu.Unlock()
		return app.refreshTok, nil
	})
	app.api.SetOnRefreshed(func(tk api.AuthTokens) {
		app.mu.Lock()
		app.refreshTok = tk.RefreshToken
		isGuest := app.user.IsGuest
		app.mu.Unlock()
		if isGuest && tk.RefreshToken != "" {
			_ = m.SetGuestRefresh(tk.RefreshToken)
		}
		enteredOnce.Do(func() { close(onRefreshedEntered) })
		<-releaseOnRefreshed
	})

	type appEvent struct {
		name string
		data any
	}
	events := make(chan appEvent, 8)
	app.emitHook = func(name string, data any) {
		select {
		case events <- appEvent{name: name, data: data}:
		default:
		}
	}

	meDone := make(chan struct{})
	go func() {
		_, _ = app.api.Me()
		close(meDone)
	}()
	select {
	case <-onRefreshedEntered:
	case <-time.After(5 * time.Second):
		t.Fatal("刷新未进入 onRefreshed（读锁段）")
	}

	switched := make(chan error, 1)
	go func() { switched <- app.SetServerConfig(newSrv.URL) }()

	// 等 retarget 走到「切换前清 refreshTok」：此刻它即将（或已经）卡在
	// SetBaseAndClearToken 的写锁上，而在飞刷新的读锁尚未释放。
	deadline := time.Now().Add(5 * time.Second)
	for {
		app.mu.Lock()
		rt := app.refreshTok
		app.mu.Unlock()
		if rt == "" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("retarget 未进行到切换前的令牌清理")
		}
		time.Sleep(5 * time.Millisecond)
	}
	select {
	case err := <-switched:
		t.Fatalf("换服未等在飞刷新结束就返回了: %v", err)
	default:
	}

	release()
	if err := <-switched; err != nil {
		t.Fatalf("SetServerConfig: %v", err)
	}
	select {
	case <-meDone:
	case <-time.After(5 * time.Second):
		t.Fatal("在飞 Me 未结束")
	}

	// 终态：无论刷新在切换前一刻回写了什么，都不能残留。
	if got := app.api.Token(); got != "" {
		t.Errorf("终态 access token 应为空，得到 %q", got)
	}
	app.mu.Lock()
	rt, user := app.refreshTok, app.user
	app.mu.Unlock()
	if rt != "" {
		t.Errorf("终态 refreshTok 应为空，得到 %q", rt)
	}
	if user.ID != 0 || user.IsGuest {
		t.Errorf("终态用户档案未清空: %+v", user)
	}
	if got := m.GetGuestRefresh(); got != "" {
		t.Errorf("终态落盘游客令牌应为空，得到 %q", got)
	}
	if got := app.api.Base(); got != newSrv.URL {
		t.Errorf("REST 基址 = %s，期望 %s", got, newSrv.URL)
	}

	bMu.Lock()
	gotB := append([]reqRecord(nil), bReqs...)
	bMu.Unlock()
	if len(gotB) == 0 {
		t.Fatal("新服务器未收到 healthz 探活")
	}
	for _, r := range gotB {
		if r.auth != "" {
			t.Errorf("新服务器收到 Authorization=%q（path=%s）", r.auth, r.path)
		}
		if !strings.HasSuffix(r.path, "/api/v1/healthz") {
			t.Errorf("新服务器收到非 healthz 请求: %s", r.path)
		}
		if strings.Contains(r.path, "/auth/refresh") ||
			strings.Contains(r.body, "rt-new-A") || strings.Contains(r.body, "old-refresh-A") {
			t.Errorf("新服务器收到刷新凭据（path=%s body=%q）", r.path, r.body)
		}
	}

	select {
	case ev := <-events:
		if ev.name != "auth:kicked" {
			t.Errorf("事件类型 = %s，期望 auth:kicked", ev.name)
		}
		payload, _ := ev.data.(map[string]any)
		if payload["reason"] != "server_changed" {
			t.Errorf("auth:kicked reason = %v，期望 server_changed", payload["reason"])
		}
	default:
		t.Error("未收到 auth:kicked 事件")
	}
}

// TestSetServerConfigSameOriginKeepsToken 仅 path 变化的切换仍是同一服务端：
// 保留令牌并走 Me() 预检（#17 的清理只针对跨 origin）。
func TestSetServerConfigSameOriginKeepsToken(t *testing.T) {
	var mu sync.Mutex
	var meAuth []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/api/v1/healthz"):
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"status":"ok","version":"test"}`))
		case strings.HasSuffix(r.URL.Path, "/api/v1/users/me"):
			mu.Lock()
			meAuth = append(meAuth, r.Header.Get("Authorization"))
			mu.Unlock()
			// 401 只为终止流程（避免 startSession 连 WS）；预检本身已验证。
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"code":"unauthorized","message":"rejected"}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	m := config.NewWithPath(filepath.Join(t.TempDir(), "config.json"))
	app := newAppWithConfig(m)
	app.api = api.NewAPIClient(srv.URL + "/old")
	app.api.SetToken("old-access-same-origin")
	app.mu.Lock()
	app.user = api.UserProfile{ID: 1, Nickname: "alice"}
	app.mu.Unlock()

	if err := app.SetServerConfig(srv.URL + "/new"); err != nil {
		t.Fatalf("SetServerConfig: %v", err)
	}

	mu.Lock()
	got := append([]string(nil), meAuth...)
	mu.Unlock()
	if len(got) != 1 || got[0] != "Bearer old-access-same-origin" {
		t.Fatalf("同源切换应携带旧令牌走 Me() 预检，收到 Authorization: %v", got)
	}
	if got := app.api.Base(); got != srv.URL+"/new" {
		t.Fatalf("REST 基址未切换: %s", got)
	}
	if got := app.GetServerConfig().BaseURL; got != srv.URL+"/new" {
		t.Fatalf("配置地址未切换: %s", got)
	}
}

// TestSetServerConfigSerializesRetarget 并发保存必须串行：第一个保存卡在
// retarget 的 Me() 预检期间，第二个不得抢先落盘，否则终态配置与会话错位（#17）。
// 两个 base 只能是同一 server 的不同 path：跨 origin 不再走进 Me() 预检
// （见 TestSetServerConfigCrossOriginDropsCredentials），这个用例要测的正是
// 预检期间的锁占位，所以必须保持同源。
func TestSetServerConfigSerializesRetarget(t *testing.T) {
	inMe := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	var releaseOnce sync.Once
	releaseAll := func() { releaseOnce.Do(func() { close(release) }) }
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/api/v1/healthz"):
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"status":"ok","version":"t"}`))
		case strings.HasSuffix(r.URL.Path, "/api/v1/users/me"):
			// 第一个保存的 Me 卡住；第二个保存（/fast）立即 401。
			if strings.HasPrefix(r.URL.Path, "/slow/") {
				once.Do(func() { close(inMe) })
				<-release
			}
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"code":"unauthorized","message":"rejected"}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	defer releaseAll() // 断言中途失败也放行，避免 goroutine 悬挂
	slow, fast := srv.URL+"/slow", srv.URL+"/fast"

	m := config.NewWithPath(filepath.Join(t.TempDir(), "config.json"))
	app := newAppWithConfig(m)
	app.api = api.NewAPIClient(slow)
	app.api.SetToken("tok")
	app.mu.Lock()
	app.user = api.UserProfile{ID: 1, Nickname: "a"}
	app.mu.Unlock()

	first := make(chan error, 1)
	go func() { first <- app.SetServerConfig(slow) }()
	select {
	case <-inMe:
	case <-time.After(5 * time.Second):
		t.Fatal("第一个保存未进入 retarget 的 Me() 预检")
	}

	second := make(chan error, 1)
	go func() { second <- app.SetServerConfig(fast) }()
	time.Sleep(300 * time.Millisecond)
	if got := app.GetServerConfig().BaseURL; got != slow {
		t.Fatalf("第一个切换未完成时配置已被第二个保存改写: %s", got)
	}

	releaseAll()
	if err := <-first; err != nil {
		t.Fatalf("第一个保存: %v", err)
	}
	if err := <-second; err != nil {
		t.Fatalf("第二个保存: %v", err)
	}
	// 终态一致：最后完成的保存同时决定配置与 REST 基址。
	if got, want := app.GetServerConfig().BaseURL, fast; got != want {
		t.Fatalf("终态配置 = %s，期望 %s", got, want)
	}
	if got := app.api.Base(); got != fast {
		t.Fatalf("终态 REST 基址 = %s，期望 %s", got, fast)
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
	app.api = api.NewAPIClient(envSrv.URL)

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

// TestIsWaylandEnv 钉住 Wayland 会话判定：重建窗口时坐标定位必须禁用的条件。
// 判定错了会在 Wayland 上把窗口定位丢掉或在 X11 上失去位置记忆。
func TestIsWaylandEnv(t *testing.T) {
	cases := []struct {
		goos, wayland, x11 string
		want               bool
	}{
		{"linux", "wayland-0", ":0", true},  // XWayland：仍是 Wayland 会话
		{"linux", "wayland-0", "", true},    // 纯 Wayland
		{"linux", "", "", true},             // 无 DISPLAY 也无 WAYLAND_DISPLAY：按 Wayland 处理（协议无定位）
		{"linux", "", ":0", false},          // 纯 X11
		{"windows", "wayland-0", "", false}, // 非 Linux 恒 false
		{"darwin", "", "", false},
	}
	for _, c := range cases {
		if got := desktop.IsWaylandEnv(c.goos, c.wayland, c.x11); got != c.want {
			t.Errorf("IsWaylandEnv(%q,%q,%q) = %v，期望 %v", c.goos, c.wayland, c.x11, got, c.want)
		}
	}
}

// TestCloseActionFor 穷举关窗决策组合（issue #7 P2）：托盘可用性优先于
// destroy 休眠模式与「关闭窗口时」设置。
func TestCloseActionFor(t *testing.T) {
	cases := []struct {
		mode        string
		trayReady   bool
		closeToTray bool
		want        closeAction
	}{
		// 托盘不可用：任何模式/设置都只能最小化，保留任务栏与 Alt-Tab 恢复路径。
		{"off", false, true, closeActionMinimise},
		{"off", false, false, closeActionMinimise},
		{"hibernate", false, true, closeActionMinimise},
		{"hibernate", false, false, closeActionMinimise},
		{"destroy", false, true, closeActionMinimise},
		{"destroy", false, false, closeActionMinimise},
		// 托盘可用 + destroy：销毁渲染进程（可由托盘重建）。
		{"destroy", true, true, closeActionDestroy},
		{"destroy", true, false, closeActionDestroy},
		// 托盘可用 + 用户明确关闭即退出。
		{"off", true, false, closeActionQuit},
		{"hibernate", true, false, closeActionQuit},
		// 托盘可用 + 关闭到托盘。
		{"off", true, true, closeActionTray},
		{"hibernate", true, true, closeActionTray},
	}
	for _, c := range cases {
		if got := closeActionFor(c.mode, c.trayReady, c.closeToTray); got != c.want {
			t.Errorf("closeActionFor(%q, tray=%v, closeToTray=%v) = %v，期望 %v",
				c.mode, c.trayReady, c.closeToTray, got, c.want)
		}
	}
}

// failWriter 固定失败的 io.Writer，模拟日志文件轮转后重新打开失败。
type failWriter struct{ err error }

func (w failWriter) Write([]byte) (int, error) { return 0, w.err }

// TestLogFanoutNoShortCircuit 单个日志目标失败不得吞掉其余目标（issue #10 P2）：
// 旧实现用 io.MultiWriter，轮转后文件重新打开失败时 stderr 收不到那一行。
func TestLogFanoutNoShortCircuit(t *testing.T) {
	reopenErr := errors.New("rotate reopen failed")

	// 文件在前（生产顺序）：文件失败时 stderr 仍要拿到整条日志。
	var stderr bytes.Buffer
	f := logFanout{failWriter{reopenErr}, &stderr}
	if n, err := f.Write([]byte("line\n")); err != nil || n != len("line\n") {
		t.Fatalf("至少一个目标成功时不应报错：n=%d err=%v", n, err)
	}
	if got := stderr.String(); got != "line\n" {
		t.Fatalf("stderr 未收到日志：%q", got)
	}

	// stderr 失败（GUI 无控制台）时文件侧仍要写入。
	var file bytes.Buffer
	f = logFanout{&file, failWriter{errors.New("stderr unavailable")}}
	if _, err := f.Write([]byte("line\n")); err != nil {
		t.Fatalf("文件写入成功时不应报错：%v", err)
	}
	if got := file.String(); got != "line\n" {
		t.Fatalf("文件未收到日志：%q", got)
	}

	// 全部失败才返回错误。
	f = logFanout{failWriter{reopenErr}, failWriter{errors.New("stderr unavailable")}}
	if n, err := f.Write([]byte("line\n")); err == nil || n != 0 {
		t.Fatalf("全部目标失败应返回错误：n=%d err=%v", n, err)
	}
}

// TestNetworkDiagWiring 一键网络诊断（#49 C）接线：REST/WS 走本地服务，
// STUN/TURN 无配置按 skipped 处理，整体判通过；不触碰真实网络。
func TestNetworkDiagWiring(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/healthz", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"ok","version":"diag-test"}`))
	})
	mux.HandleFunc("/api/v1/rtc/credentials", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"iceServers":[],"expiresAt":0}`))
	})
	mux.HandleFunc("/ws", func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		_ = conn.Close(websocket.StatusNormalClosure, "probe")
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	m := config.NewWithPath(filepath.Join(t.TempDir(), "config.json"))
	if err := m.Save(config.Config{ServerBase: srv.URL}); err != nil {
		t.Fatalf("保存配置: %v", err)
	}
	app := newAppWithConfig(m)
	app.api = api.NewAPIClient(srv.URL)

	res := app.NetworkDiag()
	if !res.OK {
		t.Fatalf("诊断应通过: %+v", res)
	}
	if !res.REST.OK || res.REST.Version != "diag-test" {
		t.Fatalf("REST 探测错误: %+v", res.REST)
	}
	if !res.WS.OK || res.WS.Skipped {
		t.Fatalf("WS 探测错误: %+v", res.WS)
	}
	if !res.STUN.Skipped || !res.TURN.Skipped {
		t.Fatalf("无 ICE 配置应为 skipped: stun=%+v turn=%+v", res.STUN, res.TURN)
	}
}

// TestNetworkDiagNoAPIDoesNotPanic 未登录/未初始化 API 客户端（测试环境）时
// 诊断仍能返回结果：REST 可通、WS 可拨号（无 token）、ICE 一律 skipped。
func TestNetworkDiagNoAPIDoesNotPanic(t *testing.T) {
	app := newAppWithConfig(config.NewWithPath(filepath.Join(t.TempDir(), "config.json")))
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("NetworkDiag 不应 panic: %v", r)
		}
	}()
	res := app.NetworkDiag()
	if res == nil {
		t.Fatal("应返回诊断结果")
	}
	if !res.STUN.Skipped || !res.TURN.Skipped {
		t.Fatalf("无凭证时 STUN/TURN 应 skipped: %+v", res)
	}
}

// TestDREDRollbackDecision 无会话/有会话两条路径的 DRED 回滚判定（#117）：
// 有会话以编码器实际状态为准；无会话只看平台能力，避免在支持的平台上
// 因为「还没连接」就白白关掉 DRED。
func TestDREDRollbackDecision(t *testing.T) {
	cases := []struct {
		name                                            string
		requested, haveSession, active, supported, want bool
	}{
		{"未请求开启", false, true, false, true, false},
		{"有会话且已启用", true, true, true, true, false},
		{"有会话但未启用", true, true, false, false, true},
		{"无会话且不支持平台", true, false, false, false, true},
		{"无会话但平台支持", true, false, false, true, false},
	}
	for _, c := range cases {
		if got := dredRollbackNeeded(c.requested, c.haveSession, c.active, c.supported); got != c.want {
			t.Errorf("%s: dredRollbackNeeded = %v, want %v", c.name, got, c.want)
		}
	}
}

// TestDREDRollbackPersists 回滚必须真的落盘：持久化的 dred 改回 false，下次
// 启动设置页才不会又显示「已开启」。用注入的能力覆盖驱动「不支持」分支，
// 在 Windows/Linux 上也真实执行（不靠 t.Skip 掩盖）。
func TestDREDRollbackPersists(t *testing.T) {
	m := config.NewWithPath(filepath.Join(t.TempDir(), "config.json"))
	app := newAppWithConfig(m)
	app.dredSupportFn = func() bool { return false }

	on := true
	as := m.GetAudioSettings()
	as.DRED = &on
	if err := m.SetAudioSettings(as); err != nil {
		t.Fatal(err)
	}

	if !app.rollbackDREDIfInactive(nil) {
		t.Fatal("不支持 DRED 的平台应发生回滚")
	}
	if got := m.GetAudioSettings(); got.DRED == nil || *got.DRED {
		t.Fatalf("回滚后持久化 dred = %+v, want false", got.DRED)
	}
	if app.rollbackDREDIfInactive(nil) {
		t.Fatal("已是关闭态时不应再回滚")
	}
	// 平台支持时不回滚：DRED 只是「还没连接」，不是不支持。
	app.dredSupportFn = func() bool { return true }
	if err := m.SetAudioSettings(as); err != nil {
		t.Fatal(err)
	}
	if app.rollbackDREDIfInactive(nil) {
		t.Fatal("支持 DRED 的平台在未连接时不应回滚")
	}
}

// TestSetAudioSettingsDREDUnsupported 不支持 DRED 时拒绝把开关打开（#117），
// 且不写入配置；关闭方向放行。用注入的能力覆盖驱动该分支。
func TestSetAudioSettingsDREDUnsupported(t *testing.T) {
	m := config.NewWithPath(filepath.Join(t.TempDir(), "config.json"))
	app := newAppWithConfig(m)
	app.dredSupportFn = func() bool { return false }

	// 先把持久化值落到明确的关闭态（默认是开，见 config 层缺省 true）。
	off := false
	base := m.GetAudioSettings()
	base.DRED = &off
	if err := m.SetAudioSettings(base); err != nil {
		t.Fatal(err)
	}

	on := true
	as := m.GetAudioSettings()
	as.DRED = &on
	if err := app.SetAudioSettings(as); err == nil {
		t.Fatal("不支持 DRED 时开启应报错")
	}
	if got := app.GetAudioSettings(); got.DRED == nil || *got.DRED {
		t.Fatalf("被拒绝的设置不应写入配置: %+v", got.DRED)
	}

	// 关闭方向放行（即便平台不支持，关闭本来就是合法目标）。
	as.DRED = &off
	if err := app.SetAudioSettings(as); err != nil {
		t.Fatalf("关闭 DRED 应放行: %v", err)
	}

	// 支持的平台正常开启，且不需要回滚。
	app.dredSupportFn = func() bool { return true }
	as.DRED = &on
	if err := app.SetAudioSettings(as); err != nil {
		t.Fatalf("支持的平台开启 DRED 不应报错: %v", err)
	}
	if got := app.GetAudioSettings(); got.DRED == nil || !*got.DRED {
		t.Fatalf("支持的平台开启后应持久化为 true: %+v", got.DRED)
	}
}
