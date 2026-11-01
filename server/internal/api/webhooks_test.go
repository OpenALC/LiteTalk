package api

// webhooks_test.go 入站与出站 Webhook REST（#52）。覆盖：出站 CRUD 与权限门、
// 事件白名单与 SSRF 校验、reset_secret 一次性明文、入站投递（免鉴权、secret
// 常数时间口径、占位机器人身份落库）与泄露可就地作废（P2-7：入站 reset 后
// 旧 URL 一律 404、删除后投递 404、跨频道归属 404）。

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/OpenALC/LiteTalk/server/internal/auth"
	"github.com/OpenALC/LiteTalk/server/internal/authz"
	"github.com/OpenALC/LiteTalk/server/internal/store"
	"github.com/OpenALC/LiteTalk/server/internal/webhook"
)

// newTestServerWithWebhooks webhook 管理端点的测试服务器：
// SSRF 校验注入放行桩（httptest 目标是 127.0.0.1，默认校验会拒绝）。
func newTestServerWithWebhooks(t *testing.T) (*Server, *store.Mem) {
	t.Helper()
	mem := store.NewMem()
	srv := New(Config{
		Store:    mem,
		JWT:      auth.NewJWT("test-secret"),
		Refresh:  auth.NewRefreshStore(mem),
		Codes:    auth.NewEmailCodes(mem),
		Limits:   auth.NewRateLimit(mem),
		Session:  auth.NewSessionIndex(mem, mem),
		Presence: mem,
		// httptest 目标是 127.0.0.1，默认 SSRF 校验会拒绝；放行以便对本地桩建订阅。
		WebhookURLCheck: func(string) error { return nil },
	})
	return srv, mem
}

// mkServerChannel 建服务器与文字频道，返回 (access, serverID, channelID)。
func mkServerChannel(t *testing.T, h http.Handler, mem *store.Mem, email, nick string) (string, int64, int64) {
	t.Helper()
	access, _ := registerUser(t, h, mem, email, nick, "password123")
	serverID := asID(postJSON(t, h, "/api/v1/servers", access, map[string]string{
		"name": nick + "的服", "icon_name": "music", "icon_color": "#4FA4F3",
	}, http.StatusCreated)["id"])
	chID := asID(postJSON(t, h, fmt.Sprintf("/api/v1/servers/%d/channels", serverID), access,
		map[string]any{"name": "general", "type": "text"}, http.StatusCreated)["id"])
	return access, serverID, chID
}

func TestOutboundWebhookCRUD(t *testing.T) {
	srv, mem := newTestServerWithWebhooks(t)
	h := srv.Handler()
	access, serverID, _ := mkServerChannel(t, h, mem, "hook-a@ex.com", "hookA")
	other, otherServer, _ := mkServerChannel(t, h, mem, "hook-b@ex.com", "hookB")

	// 空列表。
	list := getJSON(t, h, fmt.Sprintf("/api/v1/servers/%d/webhooks", serverID), access, http.StatusOK).([]any)
	if len(list) != 0 {
		t.Fatalf("初始列表应为空，实际 %d", len(list))
	}
	// 创建：secret 只在此响应出现。
	created := postJSON(t, h, fmt.Sprintf("/api/v1/servers/%d/webhooks", serverID), access, map[string]any{
		"url": "https://hooks.example.com/litetalk", "events": []string{"message.created", "member.joined"},
	}, http.StatusCreated)
	hookID := asID(created["id"])
	secret := created["secret"].(string)
	if len(secret) != 64 {
		t.Fatalf("secret 应为 64 位 hex，实际 %q", secret)
	}
	if created["disabled"] != false {
		t.Fatal("新订阅不应是停用态")
	}
	// 列表永不回传 secret。
	list = getJSON(t, h, fmt.Sprintf("/api/v1/servers/%d/webhooks", serverID), access, http.StatusOK).([]any)
	if len(list) != 1 {
		t.Fatalf("列表应为 1，实际 %d", len(list))
	}
	if list[0].(map[string]any)["secret"] != nil {
		t.Fatal("列表不得回传 secret")
	}

	// 事件白名单校验：空 / 白名单外 / 重复 / WS 消息类型都拒绝。
	for _, bad := range []any{
		[]string{},
		[]string{"chat.message"},
		[]string{"message.created", "message.created"},
		[]string{"star.created"},
	} {
		postJSON(t, h, fmt.Sprintf("/api/v1/servers/%d/webhooks", serverID), access,
			map[string]any{"url": "https://hooks.example.com/x", "events": bad}, http.StatusBadRequest)
	}

	// PATCH：改事件 + reset_secret（新 secret 只在本次响应出现）。
	patched := patchJSON(t, h, fmt.Sprintf("/api/v1/servers/%d/webhooks/%d", serverID, hookID), access, map[string]any{
		"events": []string{"channel.created"}, "reset_secret": true,
	}, http.StatusOK)
	if got := patched["secret"].(string); got == "" || got == secret {
		t.Fatal("reset_secret 应返回新 secret 且不同于旧值")
	}
	secret2 := patched["secret"].(string)
	if evs := patched["events"].([]any); len(evs) != 1 || evs[0] != "channel.created" {
		t.Fatalf("events 应更新为 [channel.created]，实际 %v", evs)
	}
	// PATCH 不带 reset_secret：不换 secret。
	p2 := patchJSON(t, h, fmt.Sprintf("/api/v1/servers/%d/webhooks/%d", serverID, hookID), access,
		map[string]any{"enabled": false}, http.StatusOK)
	if p2["secret"] != nil {
		t.Fatal("未请求 reset_secret 不得回传 secret")
	}
	if p2["disabled"] != true {
		t.Fatal("enabled=false 应手动停用")
	}
	p3 := patchJSON(t, h, fmt.Sprintf("/api/v1/servers/%d/webhooks/%d", serverID, hookID), access,
		map[string]any{"enabled": true}, http.StatusOK)
	if p3["disabled"] != false {
		t.Fatal("enabled=true 应重新启用")
	}
	// 库内连败计数应被手动启用清零。
	row, err := mem.OutboundWebhookByID(context.Background(), hookID)
	if err != nil || row.FailCount != 0 {
		t.Fatalf("重新启用应清零计数: %v %+v", err, row)
	}
	_ = secret2

	// 跨服归属：B 管理端点访问 A 的订阅统一 404（不泄露存在性）。
	delJSON(t, h, fmt.Sprintf("/api/v1/servers/%d/webhooks/%d", otherServer, hookID), other, http.StatusNotFound)
	patchJSON(t, h, fmt.Sprintf("/api/v1/servers/%d/webhooks/%d", otherServer, hookID), other,
		map[string]any{"enabled": true}, http.StatusNotFound)
	// 非成员 403。
	getJSON(t, h, fmt.Sprintf("/api/v1/servers/%d/webhooks", serverID), other, http.StatusForbidden)
	// 删除后 404，再删同样 404。
	delJSON(t, h, fmt.Sprintf("/api/v1/servers/%d/webhooks/%d", serverID, hookID), access, http.StatusOK)
	delJSON(t, h, fmt.Sprintf("/api/v1/servers/%d/webhooks/%d", serverID, hookID), access, http.StatusNotFound)
}

// TestOutboundWebhookSSRFBlocked 默认 SSRF 校验（未注入放行桩）应拒绝内网目标。
func TestOutboundWebhookSSRFBlocked(t *testing.T) {
	srv, mem := newTestServer(t) // 不注入 WebhookURLCheck → linkpreview.CheckDialable
	h := srv.Handler()
	access, serverID, _ := mkServerChannel(t, h, mem, "ssrf-a@ex.com", "ssrfA")
	for _, raw := range []string{
		"http://10.0.0.1/hook",
		"http://169.254.169.254/latest/meta-data",
		"http://localhost:8077/hook",
	} {
		resp := postJSON(t, h, fmt.Sprintf("/api/v1/servers/%d/webhooks", serverID), access,
			map[string]any{"url": raw, "events": []string{"message.created"}}, http.StatusBadRequest)
		if resp["code"] != "bad_url" {
			t.Fatalf("%s 应得 bad_url，实际 %v", raw, resp["code"])
		}
	}
}

func TestInboundWebhookDelivery(t *testing.T) {
	srv, mem := newTestServerWithWebhooks(t)
	h := srv.Handler()
	access, serverID, chID := mkServerChannel(t, h, mem, "inb-a@ex.com", "inbA")

	// 语音频道不可建入站入口。
	voiceID := asID(postJSON(t, h, fmt.Sprintf("/api/v1/servers/%d/channels", serverID), access,
		map[string]any{"name": "lobby", "type": "voice"}, http.StatusCreated)["id"])
	postJSON(t, h, fmt.Sprintf("/api/v1/channels/%d/inbound-webhooks", voiceID), access,
		map[string]string{}, http.StatusBadRequest)

	// 创建：响应带一次性投递 URL。
	created := postJSON(t, h, fmt.Sprintf("/api/v1/channels/%d/inbound-webhooks", chID), access,
		map[string]string{"description": "CI 告警"}, http.StatusCreated)
	deliverURL := created["url"].(string)
	if !strings.HasPrefix(deliverURL, "/api/v1/webhooks/") {
		t.Fatalf("投递路径形状不符: %q", deliverURL)
	}
	hookID := asID(created["id"])

	// 列表不含 secret 与投递 URL。
	list := getJSON(t, h, fmt.Sprintf("/api/v1/channels/%d/inbound-webhooks", chID), access, http.StatusOK).([]any)
	if len(list) != 1 || list[0].(map[string]any)["url"] != nil {
		t.Fatal("列表不得回传投递 URL（含 secret）")
	}

	// 免鉴权投递：不带 Bearer 直接落库，身份是占位机器人。
	postJSON(t, h, deliverURL, "", map[string]string{"content": "build #42 失败"}, http.StatusOK)
	msgs, err := mem.MessagesBefore(context.Background(), chID, 0, 10)
	if err != nil || len(msgs) != 1 || msgs[0].Content != "build #42 失败" {
		t.Fatalf("入站消息应落库: %v %+v", err, msgs)
	}
	bot, err := mem.UserByEmail(context.Background(), inboundBotEmail)
	if err != nil || msgs[0].UserID != bot.ID {
		t.Fatalf("消息身份应为占位机器人（%d）: %+v err=%v", bot.ID, msgs[0], err)
	}
	if bot.PasswordHash != "" {
		t.Fatal("占位机器人不得有可登录的密码哈希")
	}
	// 落库的发送者昵称应是机器人身份（不出现在对外成员列表之外即可）。
	if bot.Nickname != "Webhook" {
		t.Fatalf("占位机器人昵称应为 Webhook，实际 %q", bot.Nickname)
	}

	// 错误 secret 与不存在的 id 同样 404（不区分，避免枚举）。
	postJSON(t, h, deliverURL+"0", "", map[string]string{"content": "x"}, http.StatusNotFound)
	postJSON(t, h, fmt.Sprintf("/api/v1/webhooks/%d/%s", hookID, strings.Repeat("f", 64)), "",
		map[string]string{"content": "x"}, http.StatusNotFound)
	postJSON(t, h, fmt.Sprintf("/api/v1/webhooks/999/%s", strings.Repeat("f", 64)), "",
		map[string]string{"content": "x"}, http.StatusNotFound)
	// 内容校验。
	postJSON(t, h, deliverURL, "", map[string]string{"content": ""}, http.StatusBadRequest)
	postJSON(t, h, deliverURL, "", map[string]string{"content": strings.Repeat("字", 2001)}, http.StatusBadRequest)

	// 泄露面收口（P2-7 后）：原地轮换与删除走 PATCH/DELETE（RotateAndDelete 用例），
	// 这里验证第二个入口的 URL 与 secret 逐字对应、旧 URL 的错误 secret 依旧 404。
	created2 := postJSON(t, h, fmt.Sprintf("/api/v1/channels/%d/inbound-webhooks", chID), access,
		map[string]string{"description": "第二个入口"}, http.StatusCreated)
	if created2["id"] == created["id"] || created2["url"] == deliverURL {
		t.Fatal("第二个入口应有独立 id 与 secret")
	}
	postJSON(t, h, created2["url"].(string), "", map[string]string{"content": "新入口可用"}, http.StatusOK)
}

// TestInboundWebhookRotateAndDelete P2-7：入站入口的轮换与删除（与出站对齐）。
// reset 后旧 URL 一律 404、新 URL 200；明文只在 reset 响应出现一次；删除后投递
// 404、再删再轮换同样 404；跨频道归属与无权限的口径同创建（归属不符 404、
// 非成员 403）。
func TestInboundWebhookRotateAndDelete(t *testing.T) {
	srv, mem := newTestServerWithWebhooks(t)
	h := srv.Handler()
	access, _, chID := mkServerChannel(t, h, mem, "inb-r@ex.com", "inbR")
	other, _, otherCh := mkServerChannel(t, h, mem, "inb-s@ex.com", "inbS")

	created := postJSON(t, h, fmt.Sprintf("/api/v1/channels/%d/inbound-webhooks", chID), access,
		map[string]string{"description": "CI"}, http.StatusCreated)
	oldURL := created["url"].(string)
	hookID := asID(created["id"])
	postJSON(t, h, oldURL, "", map[string]string{"content": "旧 URL 可用"}, http.StatusOK)

	// 空补丁与 reset_secret≠true 拒 400：端点只为轮换存在，静默 200 会误导调用方。
	patchJSON(t, h, fmt.Sprintf("/api/v1/channels/%d/inbound-webhooks/%d", chID, hookID), access,
		map[string]any{}, http.StatusBadRequest)
	patchJSON(t, h, fmt.Sprintf("/api/v1/channels/%d/inbound-webhooks/%d", chID, hookID), access,
		map[string]any{"reset_secret": false}, http.StatusBadRequest)

	// 轮换：新 URL（含一次性明文）只在本次响应出现。
	patched := patchJSON(t, h, fmt.Sprintf("/api/v1/channels/%d/inbound-webhooks/%d", chID, hookID), access,
		map[string]any{"reset_secret": true}, http.StatusOK)
	newURL := patched["url"].(string)
	if newURL == "" || newURL == oldURL {
		t.Fatalf("reset 应返回新投递路径且不同于旧值: %q", newURL)
	}
	if patched["id"] != created["id"] {
		t.Fatal("轮换不改 id")
	}
	// 旧 URL 立即失效，新 URL 可投递。
	postJSON(t, h, oldURL, "", map[string]string{"content": "x"}, http.StatusNotFound)
	postJSON(t, h, newURL, "", map[string]string{"content": "新 URL 可用"}, http.StatusOK)
	// 列表响应永不回传投递路径（明文只在 reset 响应出现一次）。
	list := getJSON(t, h, fmt.Sprintf("/api/v1/channels/%d/inbound-webhooks", chID), access, http.StatusOK).([]any)
	if len(list) != 1 || list[0].(map[string]any)["url"] != nil {
		t.Fatal("列表不得回传投递 URL")
	}

	// 归属不符（把 A 的入口挂到 B 的频道路径下）与不存在统一 404；非成员 403（同创建口径）。
	delJSON(t, h, fmt.Sprintf("/api/v1/channels/%d/inbound-webhooks/%d", otherCh, hookID), other, http.StatusNotFound)
	patchJSON(t, h, fmt.Sprintf("/api/v1/channels/%d/inbound-webhooks/%d", otherCh, hookID), other,
		map[string]any{"reset_secret": true}, http.StatusNotFound)
	delJSON(t, h, fmt.Sprintf("/api/v1/channels/%d/inbound-webhooks/%d", chID, hookID), other, http.StatusForbidden)
	delJSON(t, h, fmt.Sprintf("/api/v1/channels/%d/inbound-webhooks/%d", chID, hookID+999), access, http.StatusNotFound)

	// 删除后旧 URL 一律 404，再删/再轮换同样 404。
	delJSON(t, h, fmt.Sprintf("/api/v1/channels/%d/inbound-webhooks/%d", chID, hookID), access, http.StatusOK)
	postJSON(t, h, newURL, "", map[string]string{"content": "x"}, http.StatusNotFound)
	delJSON(t, h, fmt.Sprintf("/api/v1/channels/%d/inbound-webhooks/%d", chID, hookID), access, http.StatusNotFound)
	patchJSON(t, h, fmt.Sprintf("/api/v1/channels/%d/inbound-webhooks/%d", chID, hookID), access,
		map[string]any{"reset_secret": true}, http.StatusNotFound)
	// 库内行确已删除（不留软删残留）。
	if _, err := mem.InboundWebhookByID(context.Background(), hookID); err != store.ErrNotFound {
		t.Fatalf("删除后应 ErrNotFound，实际 %v", err)
	}
}

// ---- REST 事件源挂钩测试（#52）：member.joined / channel.created / channel.deleted ----
// hub 事件流只有 message.created；这三类由 REST 成功点经 notifyWebhookEvent 派发，
// 这里走真实投递器 + httptest 桩做端到端断言（REST 动作 → 桩收到对应事件）。

// hookTarget 出站投递桩：记录事件名与投递体（api 侧最小桩）。
type hookTarget struct {
	mu     sync.Mutex
	events []string
	bodies [][]byte
}

func (t *hookTarget) handler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		t.mu.Lock()
		t.events = append(t.events, r.Header.Get("X-LiteTalk-Event"))
		t.bodies = append(t.bodies, body)
		t.mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}
}

// waitEvent 等待指定事件到达（异步投递的确定性等待），返回其投递体。
func waitEvent(t *testing.T, target *hookTarget, event string) []byte {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for {
		target.mu.Lock()
		for i, e := range target.events {
			if e == event {
				body := target.bodies[i]
				target.mu.Unlock()
				return body
			}
		}
		target.mu.Unlock()
		if time.Now().After(deadline) {
			t.Fatalf("等待 %s 事件超时（收到 %v）", event, target.events)
		}
		time.Sleep(2 * time.Millisecond)
	}
}

// newTestServerWithDispatch 出站全链路测试服务器：投递器接 httptest 桩（SSRF 放行）
// 且已启动（REST 挂钩路径不做 EnsureStarted——订阅由测试直插 store）。
func newTestServerWithDispatch(t *testing.T, mem *store.Mem, target *hookTarget) *Server {
	t.Helper()
	// Visibility 与生产同构注入 authz：直插订阅 CreatorID 为空，按服务器 owner
	// 代持判定（owner 旁路恒可见），投递链路不受过滤影响。
	disp := webhook.New(webhook.Config{CheckURL: func(string) error { return nil }, Visibility: authz.New(mem, authz.Options{})}, mem)
	disp.Start(nil)
	t.Cleanup(disp.Close)
	return New(Config{
		Store:           mem,
		JWT:             auth.NewJWT("test-secret"),
		Refresh:         auth.NewRefreshStore(mem),
		Codes:           auth.NewEmailCodes(mem),
		Limits:          auth.NewRateLimit(mem),
		Session:         auth.NewSessionIndex(mem, mem),
		Presence:        mem,
		Webhooks:        disp,
		WebhookURLCheck: func(string) error { return nil },
	})
}

// seedOutbound 直插一条出站订阅（订阅全部事件，测试只断言关心的那类）。
func seedOutbound(t *testing.T, mem *store.Mem, serverID int64, url string) {
	t.Helper()
	w := &store.OutboundWebhook{
		ServerID: serverID, URL: url,
		Secret: "secret0123456789abcdef0123456789abcdef",
		Events: "message.created member.joined member.left channel.created channel.deleted",
	}
	if err := mem.CreateOutboundWebhook(context.Background(), w); err != nil {
		t.Fatal(err)
	}
}

// TestChannelCreatedHook 建频道 → 订阅者收到 channel.created，归属与载荷正确。
func TestChannelCreatedHook(t *testing.T) {
	mem := store.NewMem()
	target := &hookTarget{}
	hs := httptest.NewServer(target.handler())
	defer hs.Close()
	srv := newTestServerWithDispatch(t, mem, target)
	h := srv.Handler()
	access, _ := registerUser(t, h, mem, "hook-cc@ex.com", "hookCC", "password123")
	serverID := asID(postJSON(t, h, "/api/v1/servers", access, map[string]string{
		"name": "hookCC的服", "icon_name": "music", "icon_color": "#4FA4F3",
	}, http.StatusCreated)["id"])
	seedOutbound(t, mem, serverID, hs.URL)

	ch := postJSON(t, h, fmt.Sprintf("/api/v1/servers/%d/channels", serverID), access,
		map[string]any{"name": "hooked", "type": "text"}, http.StatusCreated)
	body := waitEvent(t, target, "channel.created")
	var env struct {
		Event     string         `json:"event"`
		ServerID  string         `json:"server_id"`
		ChannelID string         `json:"channel_id"`
		Data      map[string]any `json:"data"`
	}
	if err := json.Unmarshal(body, &env); err != nil {
		t.Fatal(err)
	}
	if env.Event != "channel.created" || env.ServerID != strconv.FormatInt(serverID, 10) {
		t.Fatalf("事件头不符: %s server=%s", env.Event, env.ServerID)
	}
	if env.ChannelID != ch["id"] || env.Data["name"] != "hooked" {
		t.Fatalf("载荷与新建频道不一致: channel_id=%v data=%v", env.ChannelID, env.Data)
	}
}

// TestChannelDeletedHook 删频道 → channel.deleted，频道行已删仍带删除前快照与归属。
func TestChannelDeletedHook(t *testing.T) {
	mem := store.NewMem()
	target := &hookTarget{}
	hs := httptest.NewServer(target.handler())
	defer hs.Close()
	srv := newTestServerWithDispatch(t, mem, target)
	h := srv.Handler()
	access, _ := registerUser(t, h, mem, "hook-cd@ex.com", "hookCD", "password123")
	serverID := asID(postJSON(t, h, "/api/v1/servers", access, map[string]string{
		"name": "hookCD的服", "icon_name": "music", "icon_color": "#4FA4F3",
	}, http.StatusCreated)["id"])
	seedOutbound(t, mem, serverID, hs.URL)
	ch := postJSON(t, h, fmt.Sprintf("/api/v1/servers/%d/channels", serverID), access,
		map[string]any{"name": "doomed", "type": "text"}, http.StatusCreated)
	waitEvent(t, target, "channel.created") // 建频道事件先落袋，避免干扰计数

	delJSON(t, h, fmt.Sprintf("/api/v1/channels/%v", ch["id"]), access, http.StatusOK)
	body := waitEvent(t, target, "channel.deleted")
	var env struct {
		Event     string         `json:"event"`
		ServerID  string         `json:"server_id"`
		ChannelID string         `json:"channel_id"`
		Data      map[string]any `json:"data"`
	}
	if err := json.Unmarshal(body, &env); err != nil {
		t.Fatal(err)
	}
	if env.Event != "channel.deleted" || env.ServerID != strconv.FormatInt(serverID, 10) {
		t.Fatalf("事件头不符: %s server=%s", env.Event, env.ServerID)
	}
	if env.ChannelID != ch["id"] || env.Data["name"] != "doomed" {
		t.Fatalf("载荷应携带删除前快照: channel_id=%v data=%v", env.ChannelID, env.Data)
	}
}

// TestMemberJoinedHook 邀请入服 → member.joined（无频道维度），并发幂等重放不重复发。
func TestMemberJoinedHook(t *testing.T) {
	mem := store.NewMem()
	target := &hookTarget{}
	hs := httptest.NewServer(target.handler())
	defer hs.Close()
	srv := newTestServerWithDispatch(t, mem, target)
	h := srv.Handler()
	access, _ := registerUser(t, h, mem, "hook-mj@ex.com", "hookMJ", "password123")
	serverID := asID(postJSON(t, h, "/api/v1/servers", access, map[string]string{
		"name": "hookMJ的服", "icon_name": "music", "icon_color": "#4FA4F3",
	}, http.StatusCreated)["id"])
	seedOutbound(t, mem, serverID, hs.URL)
	inv := postJSON(t, h, fmt.Sprintf("/api/v1/servers/%d/invites", serverID), access,
		map[string]any{}, http.StatusCreated)

	bAccess, _ := registerUser(t, h, mem, "hook-mj-b@ex.com", "hookMJB", "password123")
	postJSON(t, h, "/api/v1/servers/join", bAccess, map[string]string{
		"code": inv["code"].(string),
	}, http.StatusOK)
	body := waitEvent(t, target, "member.joined")
	var env struct {
		Event    string         `json:"event"`
		ServerID string         `json:"channel_id,omitempty"`
		Data     map[string]any `json:"data"`
	}
	if err := json.Unmarshal(body, &env); err != nil {
		t.Fatal(err)
	}
	if env.Event != "member.joined" {
		t.Fatalf("事件名不符: %s", env.Event)
	}
	if env.Data["user_id"] == nil || env.Data["role"] != "member" {
		t.Fatalf("载荷应含新成员 user_id 与 role: %v", env.Data)
	}
	// 同一邀请码重复 POST（已成员幂等路径）不再触发 member.joined。
	time.Sleep(30 * time.Millisecond)
	target.mu.Lock()
	n := len(target.events)
	target.mu.Unlock()
	postJSON(t, h, "/api/v1/servers/join", bAccess, map[string]string{
		"code": inv["code"].(string),
	}, http.StatusOK)
	time.Sleep(30 * time.Millisecond)
	target.mu.Lock()
	after := len(target.events)
	target.mu.Unlock()
	if after != n {
		t.Fatalf("幂等重放不应再触发事件（%d → %d）", n, after)
	}
}

// TestOutboundWebhookCreateRequiresVisibleChannel P1-5 创建门：创建者（非 owner）
// 持 ManageServer 但对服务器内所有频道都无 ViewChannel 时，订阅永远空转，创建 403；
// 授予可见频道后可建，且 creator_id 落创建者 uid、对外 wire 不回传 creator。
func TestOutboundWebhookCreateRequiresVisibleChannel(t *testing.T) {
	mem := store.NewMem()
	// 显式持有 authz 实例：权限数据直改 mem 后须手动失效快照再走 REST。
	az := authz.New(mem, authz.Options{})
	srv := New(Config{
		Store:           mem,
		JWT:             auth.NewJWT("test-secret"),
		Refresh:         auth.NewRefreshStore(mem),
		Codes:           auth.NewEmailCodes(mem),
		Limits:          auth.NewRateLimit(mem),
		Session:         auth.NewSessionIndex(mem, mem),
		Presence:        mem,
		Authz:           az,
		WebhookURLCheck: func(string) error { return nil },
	})
	h := srv.Handler()
	ctx := context.Background()

	access, serverID, chID := mkServerChannel(t, h, mem, "vis-a@ex.com", "visA")
	// 成员 B 加入后授予 ManageServer 自定义角色，再对该服唯一频道装成员级 deny
	// ViewChannel 覆盖（审查场景：ManageServer 但对频道不可见）。
	accB, _ := registerUser(t, h, mem, "vis-b@ex.com", "visB", "password123")
	inv := postJSON(t, h, fmt.Sprintf("/api/v1/servers/%d/invites", serverID), access,
		map[string]any{"max_uses": 5}, http.StatusCreated)
	postJSON(t, h, "/api/v1/servers/join", accB, map[string]string{"code": inv["code"].(string)}, http.StatusOK)
	uidB := asID(getJSON(t, h, "/api/v1/users/me", accB, http.StatusOK).(map[string]any)["id"])
	role := &store.Role{ServerID: serverID, Name: "mgr", Permissions: store.ManageServer}
	if err := mem.CreateRole(ctx, role); err != nil {
		t.Fatal(err)
	}
	if err := mem.SetMemberRoles(ctx, serverID, uidB, []int64{role.ID}, 0); err != nil {
		t.Fatal(err)
	}
	if err := mem.SetChannelOverwrite(ctx, &store.ChannelOverwrite{
		ChannelID: chID, TargetType: store.OverwriteTargetMember, TargetID: uidB,
		Deny: store.ViewChannel,
	}); err != nil {
		t.Fatal(err)
	}
	az.InvalidateServer(serverID)

	// B 有 ManageServer 但看不到任何频道：创建被拒（空转订阅不让建）。
	postJSON(t, h, fmt.Sprintf("/api/v1/servers/%d/webhooks", serverID), accB,
		map[string]any{"url": "https://hooks.example.com/x", "events": []string{"message.created"}},
		http.StatusForbidden)

	// 解除 deny（重新授予 ViewChannel）→ 可建。
	if err := mem.DeleteChannelOverwrite(ctx, chID, store.OverwriteTargetMember, uidB); err != nil {
		t.Fatal(err)
	}
	az.InvalidateServer(serverID)
	created := postJSON(t, h, fmt.Sprintf("/api/v1/servers/%d/webhooks", serverID), accB,
		map[string]any{"url": "https://hooks.example.com/x", "events": []string{"message.created"}},
		http.StatusCreated)
	if created["creator_id"] != nil {
		t.Fatal("对外 wire 不得回传 creator")
	}
	row, err := mem.OutboundWebhookByID(ctx, asID(created["id"]))
	if err != nil {
		t.Fatal(err)
	}
	if row.CreatorID != uidB {
		t.Fatalf("creator_id 应落创建者 uid %d，实际 %d", uidB, row.CreatorID)
	}

	// 服主创建：owner 旁路恒可见，创建不受门影响且 creator_id 落服主 uid。
	createdOwner := postJSON(t, h, fmt.Sprintf("/api/v1/servers/%d/webhooks", serverID), access,
		map[string]any{"url": "https://hooks.example.com/owner", "events": []string{"member.joined"}},
		http.StatusCreated)
	rowOwner, err := mem.OutboundWebhookByID(ctx, asID(createdOwner["id"]))
	if err != nil {
		t.Fatal(err)
	}
	uidA := asID(getJSON(t, h, "/api/v1/users/me", access, http.StatusOK).(map[string]any)["id"])
	if rowOwner.CreatorID != uidA {
		t.Fatalf("owner 的 creator_id 应为 %d，实际 %d", uidA, rowOwner.CreatorID)
	}
}
