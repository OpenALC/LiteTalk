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

// TestOutboundWebhookCreateGateBySubscribedEvents #119：可见性门按订阅的事件集合判定。
// B 持 ManageServer，只看得见语音频道（文字频道被 deny）：
//   - message.created 需要文字频道 → 拒绝（旧实现只要求「任一频道可见」，会误放行，
//     而该事件永远不会投递）；
//   - channel.created 文字/语音频道都会产生 → 允许；
//   - 混合订阅里 message.created 的腿仍空转 → 拒绝；
//   - member.joined / member.left 无频道维度 → 即使所有频道都不可见也允许。
func TestOutboundWebhookCreateGateBySubscribedEvents(t *testing.T) {
	mem := store.NewMem()
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

	access, serverID, textID := mkServerChannel(t, h, mem, "gate-a@ex.com", "gateA")
	voiceID := asID(postJSON(t, h, fmt.Sprintf("/api/v1/servers/%d/channels", serverID), access,
		map[string]any{"name": "lobby", "type": "voice"}, http.StatusCreated)["id"])
	accB, _ := registerUser(t, h, mem, "gate-b@ex.com", "gateB", "password123")
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
	// 默认成员可见全部频道；deny 掉文字频道后 B 只剩语音频道可见。
	if err := mem.SetChannelOverwrite(ctx, &store.ChannelOverwrite{
		ChannelID: textID, TargetType: store.OverwriteTargetMember, TargetID: uidB,
		Deny: store.ViewChannel,
	}); err != nil {
		t.Fatal(err)
	}
	az.InvalidateServer(serverID)

	hookPath := fmt.Sprintf("/api/v1/servers/%d/webhooks", serverID)
	create := func(events []string, want int) map[string]any {
		t.Helper()
		return postJSON(t, h, hookPath, accB,
			map[string]any{"url": "https://hooks.example.com/gate", "events": events}, want)
	}

	create([]string{"message.created"}, http.StatusForbidden)
	create([]string{"channel.created"}, http.StatusCreated)
	create([]string{"channel.deleted"}, http.StatusCreated)
	create([]string{"message.created", "channel.created"}, http.StatusForbidden)

	// 再 deny 语音频道：B 对服务器内所有频道均不可见。
	if err := mem.SetChannelOverwrite(ctx, &store.ChannelOverwrite{
		ChannelID: voiceID, TargetType: store.OverwriteTargetMember, TargetID: uidB,
		Deny: store.ViewChannel,
	}); err != nil {
		t.Fatal(err)
	}
	az.InvalidateServer(serverID)

	create([]string{"message.created"}, http.StatusForbidden)
	create([]string{"channel.created"}, http.StatusForbidden)
	create([]string{"channel.deleted"}, http.StatusForbidden)
	// member.* 不经投递侧可见性闸，频道全不可见也能建。
	create([]string{"member.joined"}, http.StatusCreated)
	create([]string{"member.left"}, http.StatusCreated)
}

// ---- P1-8：改址改绑创建者 ----
// 投递按订阅创建者的频道可见性收窄（P1-5），但 PATCH 改 url 若不改绑创建者，
// 持 ManageServer 而看不到私有频道的管理者就能把服主的订阅改到自己的地址，
// 借服主的可见性收到私有频道全文。

// p18Marker 机密频道消息里的哨兵串：出现在无权接收端即证明全文泄漏。
const p18Marker = "TOP-SECRET-P18"

// rebindFixture 审查场景：服主 O 看得见全部频道；管理者 M 持 ManageServer，但对
// #secret 有成员级 deny ViewChannel。投递器与 API 共享同一个 authz（与生产装配
// 同构），权限数据改动后失效一次快照即对两侧生效。
type rebindFixture struct {
	h        http.Handler
	mem      *store.Mem
	az       *authz.Authorizer
	disp     *webhook.Dispatcher
	serverID int64
	pubID    int64 // M 可见
	secretID int64 // 对 M deny ViewChannel
	accO     string
	accM     string
	uidO     int64
	uidM     int64
}

func newRebindFixture(t *testing.T, redactURL ...bool) *rebindFixture {
	t.Helper()
	mem := store.NewMem()
	az := authz.New(mem, authz.Options{})
	disp := webhook.New(webhook.Config{CheckURL: func(string) error { return nil }, Visibility: az}, mem)
	disp.Start(nil)
	t.Cleanup(disp.Close)
	srv := New(Config{
		Store:            mem,
		JWT:              auth.NewJWT("test-secret"),
		Refresh:          auth.NewRefreshStore(mem),
		Codes:            auth.NewEmailCodes(mem),
		Limits:           auth.NewRateLimit(mem),
		Session:          auth.NewSessionIndex(mem, mem),
		Presence:         mem,
		Authz:            az,
		Webhooks:         disp,
		WebhookURLCheck:  func(string) error { return nil },
		WebhookRedactURL: len(redactURL) > 0 && redactURL[0],
	})
	h := srv.Handler()
	ctx := context.Background()

	accO, serverID, pubID := mkServerChannel(t, h, mem, "p18-o@ex.com", "p18O")
	secretID := asID(postJSON(t, h, fmt.Sprintf("/api/v1/servers/%d/channels", serverID), accO,
		map[string]any{"name": "secret", "type": "text"}, http.StatusCreated)["id"])
	accM, _ := registerUser(t, h, mem, "p18-m@ex.com", "p18M", "password123")
	inv := postJSON(t, h, fmt.Sprintf("/api/v1/servers/%d/invites", serverID), accO,
		map[string]any{"max_uses": 5}, http.StatusCreated)
	postJSON(t, h, "/api/v1/servers/join", accM, map[string]string{"code": inv["code"].(string)}, http.StatusOK)
	uidO := asID(getJSON(t, h, "/api/v1/users/me", accO, http.StatusOK).(map[string]any)["id"])
	uidM := asID(getJSON(t, h, "/api/v1/users/me", accM, http.StatusOK).(map[string]any)["id"])

	role := &store.Role{ServerID: serverID, Name: "mgr", Permissions: store.ManageServer}
	if err := mem.CreateRole(ctx, role); err != nil {
		t.Fatal(err)
	}
	if err := mem.SetMemberRoles(ctx, serverID, uidM, []int64{role.ID}, 0); err != nil {
		t.Fatal(err)
	}
	f := &rebindFixture{
		h: h, mem: mem, az: az, disp: disp, serverID: serverID,
		pubID: pubID, secretID: secretID, accO: accO, accM: accM, uidO: uidO, uidM: uidM,
	}
	f.setViewDeny(t, secretID, true)
	return f
}

// setViewDeny 给 M 装/撤指定频道的成员级 deny ViewChannel 覆盖，并失效快照。
func (f *rebindFixture) setViewDeny(t *testing.T, channelID int64, deny bool) {
	t.Helper()
	ctx := context.Background()
	if deny {
		if err := f.mem.SetChannelOverwrite(ctx, &store.ChannelOverwrite{
			ChannelID: channelID, TargetType: store.OverwriteTargetMember, TargetID: f.uidM,
			Deny: store.ViewChannel,
		}); err != nil {
			t.Fatal(err)
		}
	} else if err := f.mem.DeleteChannelOverwrite(ctx, channelID, store.OverwriteTargetMember, f.uidM); err != nil {
		t.Fatal(err)
	}
	f.az.InvalidateServer(f.serverID)
}

func (f *rebindFixture) hookPath(hookID int64) string {
	return fmt.Sprintf("/api/v1/servers/%d/webhooks/%d", f.serverID, hookID)
}

// createHook 以 acc 的身份经 REST 建 message.created 订阅，返回 (订阅 id, 一次性 secret)。
func (f *rebindFixture) createHook(t *testing.T, acc, url string) (int64, string) {
	t.Helper()
	created := postJSON(t, f.h, fmt.Sprintf("/api/v1/servers/%d/webhooks", f.serverID), acc,
		map[string]any{"url": url, "events": []string{"message.created"}}, http.StatusCreated)
	return asID(created["id"]), created["secret"].(string)
}

func (f *rebindFixture) row(t *testing.T, hookID int64) *store.OutboundWebhook {
	t.Helper()
	row, err := f.mem.OutboundWebhookByID(context.Background(), hookID)
	if err != nil {
		t.Fatal(err)
	}
	return row
}

// fire 模拟 hub 的出站出口：对频道推一条 message.created（唯一入口，REST 发消息的
// 管线同样经 hub.AfterMessagePersisted 汇入这里；api 测试里 hub 为 nil，直呼出口）。
func (f *rebindFixture) fire(channelID int64, content string) {
	ch := strconv.FormatInt(channelID, 10)
	payload, _ := json.Marshal(map[string]string{"channel_id": ch, "content": content})
	f.disp.OnWebhookEvent(webhook.EventMessageCreated, ch, payload)
}

// hits 目标已收到的投递条数。
func (t *hookTarget) hits() int {
	t.mu.Lock()
	defer t.mu.Unlock()
	return len(t.bodies)
}

// snapshot 目标已收到的全部投递体（拷贝）。
func (t *hookTarget) snapshot() [][]byte {
	t.mu.Lock()
	defer t.mu.Unlock()
	return append([][]byte(nil), t.bodies...)
}

// waitHits 等目标累计收到 n 条投递。金丝雀栅栏的等待端：同服订阅按 id 升序在同一个
// worker 里串行投递，id 更大的金丝雀收到事件，即说明 id 更小的目标订阅对该事件的
// 放行/跳过判定已经落定，可以确定性地断言「没收到」，不靠 sleep。
func waitHits(t *testing.T, target *hookTarget, n int) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for target.hits() < n {
		if time.Now().After(deadline) {
			t.Fatalf("等待 %d 条投递超时（实收 %d）", n, target.hits())
		}
		time.Sleep(2 * time.Millisecond)
	}
}

// deliveredChannels 投递体里的 channel_id 列表（按到达顺序）。
func deliveredChannels(t *testing.T, bodies [][]byte) []string {
	t.Helper()
	out := make([]string, 0, len(bodies))
	for _, b := range bodies {
		var env struct {
			ChannelID string `json:"channel_id"`
		}
		if err := json.Unmarshal(b, &env); err != nil {
			t.Fatalf("投递体不是合法 JSON: %v", err)
		}
		out = append(out, env.ChannelID)
	}
	return out
}

// TestWebhookPatchURLRebindsCreator 审查场景主线：M 把服主订阅的 url 改到自己的
// 地址后，订阅按 M 的可见性收窄——#secret 的消息不再投到 M 的地址，M 可见的
// 频道照常投递，原地址不再收到。
func TestWebhookPatchURLRebindsCreator(t *testing.T) {
	f := newRebindFixture(t)
	oT, mT, cT := &hookTarget{}, &hookTarget{}, &hookTarget{}
	oSrv := httptest.NewServer(oT.handler())
	defer oSrv.Close()
	mSrv := httptest.NewServer(mT.handler())
	defer mSrv.Close()
	cSrv := httptest.NewServer(cT.handler())
	defer cSrv.Close()

	// O 建目标订阅，再建金丝雀（id 更大 ⇒ 同一次 process 里排在后面）。
	hookID, _ := f.createHook(t, f.accO, oSrv.URL)
	canaryID, _ := f.createHook(t, f.accO, cSrv.URL)

	// M 把 O 的订阅改到自己的地址：ManageServer 允许，且 M 对 #general 可见 ⇒ 过门。
	patched := patchJSON(t, f.h, f.hookPath(hookID), f.accM, map[string]any{"url": mSrv.URL}, http.StatusOK)
	if patched["url"] != mSrv.URL {
		t.Fatalf("PATCH 响应应带新 url，实际 %v", patched["url"])
	}

	f.fire(f.secretID, p18Marker+" 机密内容")
	f.fire(f.pubID, "公开内容")
	waitHits(t, cT, 2) // 金丝雀栅栏：此刻目标订阅对两条事件的判定都已落定

	got := mT.snapshot()
	for _, b := range got {
		if strings.Contains(string(b), p18Marker) {
			t.Fatalf("机密频道消息全文泄漏到管理者 M 的地址: %s", b)
		}
	}
	if chs := deliveredChannels(t, got); len(chs) != 1 || chs[0] != strconv.FormatInt(f.pubID, 10) {
		t.Fatalf("M 的地址应只收到其可见频道 %d 的 1 条，实际 %v", f.pubID, chs)
	}
	if n := oT.hits(); n != 0 {
		t.Fatalf("改址后原地址不应再收到投递，实际 %d", n)
	}
	// 内部状态：创建者改绑为改址者；未改址的金丝雀保持 O。
	if row := f.row(t, hookID); row.CreatorID != f.uidM {
		t.Errorf("改址后 creator_id 应改绑为 M(%d)，实际 %d", f.uidM, row.CreatorID)
	}
	if row := f.row(t, canaryID); row.CreatorID != f.uidO {
		t.Errorf("未改址的订阅 creator_id 应保持 O(%d)，实际 %d", f.uidO, row.CreatorID)
	}
}

// TestWebhookPatchURLByCreatorKeepsDelivery 创建者自己改址：改绑到自己等于不变，
// 对其可见的私有频道仍然投递，且只投新地址。
func TestWebhookPatchURLByCreatorKeepsDelivery(t *testing.T) {
	f := newRebindFixture(t)
	oldT, newT := &hookTarget{}, &hookTarget{}
	oldSrv := httptest.NewServer(oldT.handler())
	defer oldSrv.Close()
	newSrv := httptest.NewServer(newT.handler())
	defer newSrv.Close()

	hookID, _ := f.createHook(t, f.accO, oldSrv.URL)
	patchJSON(t, f.h, f.hookPath(hookID), f.accO, map[string]any{"url": newSrv.URL}, http.StatusOK)
	if row := f.row(t, hookID); row.CreatorID != f.uidO || row.URL != newSrv.URL {
		t.Fatalf("O 自己改址应保持创建者 O 并写入新 url: %+v", row)
	}

	f.fire(f.secretID, p18Marker)
	body := waitEvent(t, newT, "message.created")
	if !strings.Contains(string(body), p18Marker) {
		t.Fatalf("O 对 #secret 可见，改址后新地址应收到全文: %s", body)
	}
	// 一个订阅一个 url：新地址已收到该订阅唯一的一次投递，旧地址不可能再有。
	if n := oldT.hits(); n != 0 {
		t.Fatalf("改址后旧地址不应收到投递，实际 %d", n)
	}
}

// TestWebhookPatchWithoutURLKeepsCreator 不改变投递去向的 PATCH 不改绑：events、
// enabled、reset_secret，以及重发值未变的 url（自动化回放完整对象不应意外改绑）。
// 订阅仍按 O 的可见性投递到原地址。
func TestWebhookPatchWithoutURLKeepsCreator(t *testing.T) {
	f := newRebindFixture(t)
	oT := &hookTarget{}
	oSrv := httptest.NewServer(oT.handler())
	defer oSrv.Close()
	hookID, _ := f.createHook(t, f.accO, oSrv.URL)

	for _, body := range []map[string]any{
		{"events": []string{"message.created", "channel.created"}},
		{"enabled": false},
		{"enabled": true},
		{"reset_secret": true},
		{"url": oSrv.URL, "events": []string{"message.created"}},
	} {
		patchJSON(t, f.h, f.hookPath(hookID), f.accM, body, http.StatusOK)
		if row := f.row(t, hookID); row.CreatorID != f.uidO || row.URL != oSrv.URL {
			t.Fatalf("PATCH %v 不应改绑或改址: creator=%d url=%s", body, row.CreatorID, row.URL)
		}
	}

	f.fire(f.secretID, p18Marker)
	if body := waitEvent(t, oT, "message.created"); !strings.Contains(string(body), p18Marker) {
		t.Fatalf("仍按 O 的可见性投递，原地址应收到 #secret 全文: %s", body)
	}
}

// TestWebhookPatchURLRequiresChannelView 改址与创建同一道门：调用者对服务器内所有
// 频道都无 ViewChannel 时，改址 403 且行不变；不改址的 PATCH 不受门影响；
// 恢复可见后改址成功并改绑。
func TestWebhookPatchURLRequiresChannelView(t *testing.T) {
	f := newRebindFixture(t)
	oT, mT := &hookTarget{}, &hookTarget{}
	oSrv := httptest.NewServer(oT.handler())
	defer oSrv.Close()
	mSrv := httptest.NewServer(mT.handler())
	defer mSrv.Close()
	hookID, _ := f.createHook(t, f.accO, oSrv.URL)

	f.setViewDeny(t, f.pubID, true) // M 现在对所有频道都不可见
	patchJSON(t, f.h, f.hookPath(hookID), f.accM, map[string]any{"url": mSrv.URL}, http.StatusForbidden)
	if row := f.row(t, hookID); row.URL != oSrv.URL || row.CreatorID != f.uidO {
		t.Fatalf("被拒的改址不得落库: %+v", row)
	}
	patchJSON(t, f.h, f.hookPath(hookID), f.accM, map[string]any{"enabled": false}, http.StatusOK)

	f.setViewDeny(t, f.pubID, false)
	patchJSON(t, f.h, f.hookPath(hookID), f.accM, map[string]any{"url": mSrv.URL}, http.StatusOK)
	if row := f.row(t, hookID); row.URL != mSrv.URL || row.CreatorID != f.uidM {
		t.Fatalf("恢复可见后改址应成功并改绑为 M: %+v", row)
	}
}

// TestOutboundWebhookURLMaskedForNonCreator #119：出站 url 的路径/查询串常内嵌
// 第三方投递凭证（Discord/Slack 的 token 就在路径里），完整值只回传给订阅创建者
// 与服务器 owner；其余 ManageServer 成员在列表与 PATCH 响应里只看到 scheme://host/…。
// creator_id=0 的旧行按 owner 代持；改址者获得完整响应（他刚提交该地址并成为创建者）。
func TestOutboundWebhookURLMaskedForNonCreator(t *testing.T) {
	f := newRebindFixture(t, true)
	const fullURL = "https://hooks.example.com/services/T000/B000/secret-token?key=abc"
	const masked = "https://hooks.example.com/…"
	listPath := fmt.Sprintf("/api/v1/servers/%d/webhooks", f.serverID)

	created := postJSON(t, f.h, listPath, f.accO,
		map[string]any{"url": fullURL, "events": []string{"message.created"}}, http.StatusCreated)
	hookID := asID(created["id"])
	if created["url"] != fullURL {
		t.Fatalf("创建者应看到自己提交的完整 url，实际 %v", created["url"])
	}

	// 非创建者的 ManageServer 成员：列表与 PATCH 响应都只给掩码。
	list := getJSON(t, f.h, listPath, f.accM, http.StatusOK).([]any)
	if len(list) != 1 {
		t.Fatalf("列表应有 1 条，实际 %d", len(list))
	}
	if got := list[0].(map[string]any)["url"]; got != masked {
		t.Fatalf("非创建者列表应得掩码 url %q，实际 %v", masked, got)
	}
	patched := patchJSON(t, f.h, f.hookPath(hookID), f.accM,
		map[string]any{"enabled": false}, http.StatusOK)
	if got := patched["url"]; got != masked {
		t.Fatalf("非创建者 PATCH 响应应得掩码 url %q，实际 %v", masked, got)
	}

	// 服主列表是完整地址（owner 是服务器的信任根）。
	ownerList := getJSON(t, f.h, listPath, f.accO, http.StatusOK).([]any)
	if got := ownerList[0].(map[string]any)["url"]; got != fullURL {
		t.Fatalf("服主应读到完整 url，实际 %v", got)
	}

	// 0022 前的旧行（creator_id=0，owner 代持）：owner 完整，其他管理者掩码。
	legacyURL := "https://legacy.example.com/hook/legacy-secret"
	legacy := &store.OutboundWebhook{ServerID: f.serverID, URL: legacyURL,
		Secret: "s", Events: "member.joined"}
	if err := f.mem.CreateOutboundWebhook(context.Background(), legacy); err != nil {
		t.Fatal(err)
	}
	legacyID := strconv.FormatInt(legacy.ID, 10)
	for _, tc := range []struct {
		acc, want string
	}{
		{f.accO, legacyURL},
		{f.accM, "https://legacy.example.com/…"},
	} {
		rows := getJSON(t, f.h, listPath, tc.acc, http.StatusOK).([]any)
		var got any
		for _, r := range rows {
			m := r.(map[string]any)
			if m["id"] == legacyID {
				got = m["url"]
			}
		}
		if got != tc.want {
			t.Fatalf("%s 对旧行的 url 应为 %q，实际 %v", tc.acc, tc.want, got)
		}
	}

	// 非创建者改址：响应带完整新地址（他刚提交），改绑后他就是创建者。
	newURL := "https://other.example.com/new/hook?token=new"
	rebound := patchJSON(t, f.h, f.hookPath(hookID), f.accM,
		map[string]any{"url": newURL}, http.StatusOK)
	if got := rebound["url"]; got != newURL {
		t.Fatalf("改址者应看到完整新 url，实际 %v", got)
	}
	for _, r := range getJSON(t, f.h, listPath, f.accM, http.StatusOK).([]any) {
		m := r.(map[string]any)
		if m["id"] == strconv.FormatInt(hookID, 10) && m["url"] != newURL {
			t.Fatalf("改绑后创建者列表应给完整新 url，实际 %v", m["url"])
		}
	}
}

func TestOutboundWebhookURLRedactionDefaultOff(t *testing.T) {
	f := newRebindFixture(t)
	const fullURL = "https://hooks.example.com/services/T/B/secret?key=abc"
	hookID, _ := f.createHook(t, f.accO, fullURL)
	listPath := fmt.Sprintf("/api/v1/servers/%d/webhooks", f.serverID)
	rows := getJSON(t, f.h, listPath, f.accM, http.StatusOK).([]any)
	if len(rows) != 1 || rows[0].(map[string]any)["url"] != fullURL {
		t.Fatalf("默认关闭脱敏时 ManageServer 列表应保持已发布完整 url: %v", rows)
	}
	patched := patchJSON(t, f.h, f.hookPath(hookID), f.accM,
		map[string]any{"enabled": false}, http.StatusOK)
	if patched["url"] != fullURL {
		t.Fatalf("默认关闭脱敏时 PATCH 响应仍应是完整 url: %v", patched["url"])
	}
}

func TestOutboundWebhookRejectsRedactedURLWrites(t *testing.T) {
	for _, enabled := range []bool{false, true} {
		t.Run(fmt.Sprintf("redaction=%t", enabled), func(t *testing.T) {
			testOutboundWebhookRejectsRedactedURLWrites(t, enabled)
		})
	}
}

func testOutboundWebhookRejectsRedactedURLWrites(t *testing.T, enabled bool) {
	f := newRebindFixture(t, enabled)
	const fullURL = "https://hooks.example.com/services/T/B/secret?key=abc"
	hookID, _ := f.createHook(t, f.accO, fullURL)
	listPath := fmt.Sprintf("/api/v1/servers/%d/webhooks", f.serverID)
	masked := maskWebhookURL(fullURL)
	if enabled {
		rows := getJSON(t, f.h, listPath, f.accM, http.StatusOK).([]any)
		if rows[0].(map[string]any)["url"] != masked {
			t.Fatalf("开启脱敏后列表必须返回本次回写测试所用的掩码: %v", rows)
		}
	}
	before := *f.row(t, hookID)
	for _, candidate := range []string{masked, "https://hooks.example.com/%E2%80%A6"} {
		got := patchJSON(t, f.h, f.hookPath(hookID), f.accM,
			map[string]any{"url": candidate, "enabled": false, "reset_secret": true}, http.StatusBadRequest)
		if got["code"] != "bad_url" {
			t.Fatalf("脱敏 URL 回写应报 bad_url: %v", got)
		}
		after := f.row(t, hookID)
		if after.URL != before.URL || after.CreatorID != before.CreatorID ||
			after.Secret != before.Secret || after.Disabled != before.Disabled ||
			after.Events != before.Events || after.FailCount != before.FailCount {
			t.Fatalf("拒绝掩码回写后订阅被修改: before=%+v after=%+v", before, after)
		}
		got = postJSON(t, f.h, listPath, f.accM,
			map[string]any{"url": candidate, "events": []string{"member.joined"}}, http.StatusBadRequest)
		if got["code"] != "bad_url" {
			t.Fatalf("创建订阅不得接受脱敏 URL: %v", got)
		}
	}
	if got := len(getJSON(t, f.h, listPath, f.accO, http.StatusOK).([]any)); got != 1 {
		t.Fatalf("拒绝掩码创建后订阅数应仍为 1，实际 %d", got)
	}
	// 只拒绝完整展示占位符；真实地址的其它 Unicode 路径仍可保存。
	for _, candidate := range []string{
		"https://hooks.example.com/…/valid",
		"https://hooks.example.com/真实…路径?key=ok",
	} {
		created := postJSON(t, f.h, listPath, f.accM,
			map[string]any{"url": candidate, "events": []string{"member.joined"}}, http.StatusCreated)
		if created["url"] != candidate {
			t.Fatalf("合法 Unicode 地址不应被改写: %v", created["url"])
		}
	}
	const changed = "https://hooks.example.com/更新…路径"
	patched := patchJSON(t, f.h, f.hookPath(hookID), f.accM,
		map[string]any{"url": changed}, http.StatusOK)
	if patched["url"] != changed || f.row(t, hookID).URL != changed {
		t.Fatalf("合法 Unicode PATCH 未保存完整地址: %v", patched)
	}
}

// TestWebhookPatchURLEventsGateUsesNewEvents #119：同一次 PATCH 既改址又改 events 时，
// 可见性门按**改后**的事件集合判定。M 对所有频道都不可见：把 member.joined 订阅
// 同时改址并改成 message.created 必须 403（否则改址成功后才改事件即可绕过门）；
// 改址 + 保留无频道维度的 events 则放行并改绑。
func TestWebhookPatchURLEventsGateUsesNewEvents(t *testing.T) {
	f := newRebindFixture(t)
	oSrv := httptest.NewServer((&hookTarget{}).handler())
	defer oSrv.Close()
	mSrv := httptest.NewServer((&hookTarget{}).handler())
	defer mSrv.Close()

	created := postJSON(t, f.h, fmt.Sprintf("/api/v1/servers/%d/webhooks", f.serverID), f.accO,
		map[string]any{"url": oSrv.URL, "events": []string{"member.joined"}}, http.StatusCreated)
	hookID := asID(created["id"])

	f.setViewDeny(t, f.pubID, true) // 加上 fixture 默认对 secret 的 deny：M 对所有文字频道不可见

	// 改址 + 改成 message.created：门按新事件算，M 无文字频道可见 → 拒绝且行不变。
	patchJSON(t, f.h, f.hookPath(hookID), f.accM,
		map[string]any{"url": mSrv.URL, "events": []string{"message.created"}}, http.StatusForbidden)
	if row := f.row(t, hookID); row.URL != oSrv.URL || row.CreatorID != f.uidO {
		t.Fatalf("被拒的改址不得落库: %+v", row)
	}

	// 改址 + 保留 member.joined：无频道维度，门跳过，允许改绑。
	patched := patchJSON(t, f.h, f.hookPath(hookID), f.accM,
		map[string]any{"url": mSrv.URL, "events": []string{"member.joined"}}, http.StatusOK)
	if patched["url"] != mSrv.URL {
		t.Fatalf("PATCH 响应应带新 url，实际 %v", patched["url"])
	}
	if row := f.row(t, hookID); row.URL != mSrv.URL || row.CreatorID != f.uidM {
		t.Fatalf("无频道维度的订阅应允许改址并改绑: %+v", row)
	}

	// 恢复 M 的文字频道可见性后，同次改址和事件应按 M 的角色放行；
	// guild.manage PAT 没有 messages.read，不能让操作凭据的 scope 干扰判定。
	f.setViewDeny(t, f.pubID, false)
	thirdURL := "https://third.example.com/new"
	pat := patFor(t, f.mem, f.uidM, "guild.manage")
	patchJSON(t, f.h, f.hookPath(hookID), pat,
		map[string]any{"url": thirdURL, "events": []string{"message.created"}}, http.StatusOK)
	if row := f.row(t, hookID); row.URL != thirdURL || row.Events != "message.created" || row.CreatorID != f.uidM {
		t.Fatalf("同次改址/事件应按最终创建者角色放行: %+v", row)
	}
}

// TestWebhookPatchEventsUsesEffectiveCreator 仅改事件不改地址时，判定主体仍是
// 原订阅创建者，不能拿执行 PATCH 的管理员角色或令牌 scope 代替；旧行 creator_id=0
// 由服主代持。无可见性时 409 不应改动库行，允许修改也不应改绑创建者。
func TestWebhookPatchEventsUsesEffectiveCreator(t *testing.T) {
	t.Run("创建者不可见但操作者可见时拒绝且不落库", func(t *testing.T) {
		f := newRebindFixture(t)
		created := postJSON(t, f.h, fmt.Sprintf("/api/v1/servers/%d/webhooks", f.serverID), f.accM,
			map[string]any{"url": "https://hooks.example.com/m", "events": []string{"member.joined"}}, http.StatusCreated)
		hookID := asID(created["id"])
		before := *f.row(t, hookID)
		f.setViewDeny(t, f.pubID, true) // M 已看不到任何文字频道；服主 O 仍可见
		resp := patchJSON(t, f.h, f.hookPath(hookID), f.accO,
			map[string]any{"events": []string{"message.created"}, "enabled": false, "reset_secret": true},
			http.StatusConflict)
		if resp["code"] != "webhook_events_needs_creator" || resp["message"] != "需由创建者或服主修改事件" {
			t.Fatalf("创建者失去可见性应返回专用冲突: %v", resp)
		}
		if row := *f.row(t, hookID); row != before {
			t.Fatalf("409 后订阅行不得变化: before=%+v after=%+v", before, row)
		}

		// 同次真正改址按 O 判定，事件门通过并将投递创建者改绑为 O。
		newURL := "https://hooks.example.com/o"
		patchJSON(t, f.h, f.hookPath(hookID), f.accO,
			map[string]any{"url": newURL, "events": []string{"message.created"}}, http.StatusOK)
		if row := f.row(t, hookID); row.URL != newURL || row.Events != "message.created" || row.CreatorID != f.uidO {
			t.Fatalf("改址应按改址者可见性放行并改绑: %+v", row)
		}
	})

	t.Run("创建者可见但操作者不可见时允许且不改绑", func(t *testing.T) {
		f := newRebindFixture(t)
		created := postJSON(t, f.h, fmt.Sprintf("/api/v1/servers/%d/webhooks", f.serverID), f.accO,
			map[string]any{"url": "https://hooks.example.com/o", "events": []string{"member.joined"}}, http.StatusCreated)
		hookID := asID(created["id"])
		f.setViewDeny(t, f.pubID, true)                 // M 无可见文字频道
		pat := patFor(t, f.mem, f.uidM, "guild.manage") // 也不带 messages.read
		patchJSON(t, f.h, f.hookPath(hookID), pat,
			map[string]any{"events": []string{"message.created"}}, http.StatusOK)
		if row := f.row(t, hookID); row.URL != "https://hooks.example.com/o" ||
			row.Events != "message.created" || row.CreatorID != f.uidO {
			t.Fatalf("事件更新应按原创建者放行，且不改绑: %+v", row)
		}
	})

	t.Run("旧行按服主代持且不补写创建者", func(t *testing.T) {
		f := newRebindFixture(t)
		legacy := &store.OutboundWebhook{ServerID: f.serverID, URL: "https://hooks.example.com/legacy",
			Secret: "s", Events: "member.joined"}
		if err := f.mem.CreateOutboundWebhook(context.Background(), legacy); err != nil {
			t.Fatal(err)
		}
		f.setViewDeny(t, f.pubID, true)
		patchJSON(t, f.h, f.hookPath(legacy.ID), f.accM,
			map[string]any{"events": []string{"message.created"}}, http.StatusOK)
		if row := f.row(t, legacy.ID); row.Events != "message.created" || row.CreatorID != 0 {
			t.Fatalf("旧行应按服主权限更新事件且保持旧创建者字段: %+v", row)
		}
	})
}

type failingWebhookChannelListStore struct {
	*store.Mem
	err error
}

func (s *failingWebhookChannelListStore) ChannelsOfServer(context.Context, int64) ([]store.Channel, error) {
	return nil, s.err
}

func TestWebhookPatchEventsKeepsChannelQueryErrors(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
		want int
		code string
	}{
		{"forbidden", store.ErrForbidden, http.StatusForbidden, "forbidden"},
		{"internal", fmt.Errorf("频道查询失败"), http.StatusInternalServerError, "internal"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newRebindFixture(t)
			created := postJSON(t, f.h, fmt.Sprintf("/api/v1/servers/%d/webhooks", f.serverID), f.accO,
				map[string]any{"url": "https://hooks.example.com/o", "events": []string{"member.joined"}},
				http.StatusCreated)
			hookID := asID(created["id"])
			before := *f.row(t, hookID)
			srv := New(Config{
				Store: &failingWebhookChannelListStore{Mem: f.mem, err: tc.err},
				JWT:   auth.NewJWT("test-secret"), Refresh: auth.NewRefreshStore(f.mem),
				Codes: auth.NewEmailCodes(f.mem), Limits: auth.NewRateLimit(f.mem),
				Session: auth.NewSessionIndex(f.mem, f.mem), Presence: f.mem,
				Authz: f.az, Webhooks: f.disp,
				WebhookURLCheck: func(string) error { return nil },
			})
			resp := patchJSON(t, srv.Handler(), f.hookPath(hookID), f.accM,
				map[string]any{"events": []string{"message.created"}}, tc.want)
			if resp["code"] != tc.code {
				t.Fatalf("查询失败应保持原错误 %s: %v", tc.code, resp)
			}
			if row := *f.row(t, hookID); row != before {
				t.Fatalf("查询失败后订阅行不得变化: before=%+v after=%+v", before, row)
			}
		})
	}
}

// TestWebhookPatchURLWritesAudit 改址写审计 webhook.update_url：记旧/新创建者，
// 不记 url 与 secret；不改址的 PATCH 不产生该审计。
func TestWebhookPatchURLWritesAudit(t *testing.T) {
	f := newRebindFixture(t)
	oT, mT := &hookTarget{}, &hookTarget{}
	oSrv := httptest.NewServer(oT.handler())
	defer oSrv.Close()
	mSrv := httptest.NewServer(mT.handler())
	defer mSrv.Close()

	// M 建订阅（过门），O 再改址：改绑到 O 是显式的可见性拓宽，必须留痕。
	hookID, secret := f.createHook(t, f.accM, mSrv.URL)
	patchJSON(t, f.h, f.hookPath(hookID), f.accO, map[string]any{"url": oSrv.URL}, http.StatusOK)
	patchJSON(t, f.h, f.hookPath(hookID), f.accO, map[string]any{"enabled": false}, http.StatusOK)

	logs, err := f.mem.AuditLogBefore(context.Background(), f.serverID, 0, 50)
	if err != nil {
		t.Fatal(err)
	}
	var found []store.AuditLog
	for _, l := range logs {
		if l.Action == "webhook.update_url" {
			found = append(found, l)
		}
	}
	if len(found) != 1 {
		t.Fatalf("应恰有 1 条 webhook.update_url 审计，实际 %d（%v）", len(found), logs)
	}
	l := found[0]
	if l.ActorID != f.uidO || l.TargetType != "webhook" || l.TargetID == nil || *l.TargetID != hookID {
		t.Fatalf("审计主体/目标不符: %+v", l)
	}
	var detail map[string]any
	if err := json.Unmarshal([]byte(l.Detail), &detail); err != nil {
		t.Fatalf("detail 应为 JSON: %v (%s)", err, l.Detail)
	}
	if detail["old_creator_id"] != strconv.FormatInt(f.uidM, 10) || detail["new_creator_id"] != strconv.FormatInt(f.uidO, 10) {
		t.Fatalf("detail 应记旧/新创建者（字符串 id）: %s", l.Detail)
	}
	for _, leak := range []string{oSrv.URL, mSrv.URL, secret} {
		if strings.Contains(l.Detail, leak) {
			t.Fatalf("审计 detail 不得含 url/secret: %s", l.Detail)
		}
	}
}
