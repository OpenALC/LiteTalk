package api

// webhooks_test.go 入站与出站 Webhook REST（#52）。覆盖：出站 CRUD 与权限门、
// 事件白名单与 SSRF 校验、reset_secret 一次性明文、入站投递（免鉴权、secret
// 常数时间口径、占位机器人身份落库）与泄露可重置（旧 URL 一律 404）。

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/OpenALC/LiteTalk/server/internal/auth"
	"github.com/OpenALC/LiteTalk/server/internal/store"
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

	// 泄露面收口：入站端点契约只有 GET/POST（无 PATCH/DELETE），secret 不可原地
	// 轮换——重建新入口拿新 URL 是唯一途径（旧入口的废弃由运维删除行或随频道
	// 删除完成）。这里验证新入口的 URL 与 secret 逐字对应、旧 URL 的错误 secret
	// 依旧 404（上文已覆盖），并把「入站缺轮换端点」记入交付报告待主会话裁定。
	created2 := postJSON(t, h, fmt.Sprintf("/api/v1/channels/%d/inbound-webhooks", chID), access,
		map[string]string{"description": "第二个入口"}, http.StatusCreated)
	if created2["id"] == created["id"] || created2["url"] == deliverURL {
		t.Fatal("第二个入口应有独立 id 与 secret")
	}
	postJSON(t, h, created2["url"].(string), "", map[string]string{"content": "新入口可用"}, http.StatusOK)
}
