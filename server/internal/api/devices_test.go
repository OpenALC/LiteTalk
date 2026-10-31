package api

// devices_test.go 设备注册 REST 与推送触发点（#53）。
// 覆盖：幂等注册、删他人设备 404、push_enabled 反映配置、push_token 不回显、
// 全设备离线才推（部分在线不推）、@提及/私聊/好友请求三条触发链的 api 侧接线。

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/OpenALC/LiteTalk/server/internal/auth"
	"github.com/OpenALC/LiteTalk/server/internal/push"
	"github.com/OpenALC/LiteTalk/server/internal/store"
)

// fakeAPIOnline 可编程在线判定（api 测试的连接表替身）。
type fakeAPIOnline struct{ online map[string]bool }

func (f fakeAPIOnline) UserOnline(uid string) bool { return f.online[uid] }

// recordingAPIPusher 记录投递的 Pusher 桩（与 push 包内测试桩同名不同包）。
type recordingAPIPusher struct {
	mu     sync.Mutex
	got    []push.Notification
	tokens []string
}

func (r *recordingAPIPusher) Push(_ context.Context, devices []push.Device, n push.Notification) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.got = append(r.got, n)
	for _, d := range devices {
		r.tokens = append(r.tokens, d.Token)
	}
	return nil
}

func (r *recordingAPIPusher) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.got)
}

func (r *recordingAPIPusher) last() (push.Notification, []string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.got) == 0 {
		return push.Notification{}, nil
	}
	return r.got[len(r.got)-1], r.tokens
}

// newTestServerWithPush 推送开启的测试服务器：全离线（可经 online map 调整）。
func newTestServerWithPush(t *testing.T) (*Server, *store.Mem, *recordingAPIPusher, fakeAPIOnline) {
	t.Helper()
	mem := store.NewMem()
	pusher := &recordingAPIPusher{}
	online := fakeAPIOnline{online: map[string]bool{}}
	b := push.NewBroker(push.BrokerConfig{
		Pusher: pusher,
		Store:  mem,
		Online: online,
		Limits: auth.NewRateLimit(mem),
	})
	srv := New(Config{
		Store:    mem,
		JWT:      auth.NewJWT("test-secret"),
		Refresh:  auth.NewRefreshStore(mem),
		Codes:    auth.NewEmailCodes(mem),
		Limits:   auth.NewRateLimit(mem),
		Session:  auth.NewSessionIndex(mem, mem),
		Presence: mem,
		Push:     b,
	})
	t.Cleanup(b.Close)
	return srv, mem, pusher, online
}

// waitForAPush 等待第 n 次投递到达（异步派发的确定性等待）。
func waitForAPush(t *testing.T, r *recordingAPIPusher, n int) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for r.count() < n {
		if time.Now().After(deadline) {
			t.Fatalf("等待第 %d 次投递超时（实际 %d）", n, r.count())
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// TestDeviceRegistrationIdempotent 幂等注册：同 push_token 二次注册 = 200 更新
// 既有设备（同 ID、platform 更新），不新增行；列表不含 push_token。
func TestDeviceRegistrationIdempotent(t *testing.T) {
	srv, mem, _, _ := newTestServerWithPush(t)
	h := srv.Handler()
	access, _ := registerUser(t, h, mem, "dev@ex.com", "deviceOwner", "password123")

	first := postJSON(t, h, "/api/v1/devices", access,
		map[string]string{"platform": "ios", "push_token": "tok-abcdef0123456789"}, http.StatusCreated)
	if first["push_token"] != nil {
		t.Fatal("响应不得回显 push_token（生产凭据等价物）")
	}
	id1 := asID(first["id"])
	// 换 platform 重复注册：更新而非新增。
	second := postJSON(t, h, "/api/v1/devices", access,
		map[string]string{"platform": "android", "push_token": "tok-abcdef0123456789"}, http.StatusOK)
	if asID(second["id"]) != id1 {
		t.Fatalf("重复注册应复用设备 ID: %d vs %d", asID(second["id"]), id1)
	}
	if second["platform"] != "android" {
		t.Fatalf("重复注册应更新 platform: %v", second["platform"])
	}
	list := getJSON(t, h, "/api/v1/devices", access, http.StatusOK).(map[string]any)
	devs := list["devices"].([]any)
	if len(devs) != 1 {
		t.Fatalf("重复注册后应仍 1 台，实际 %d", len(devs))
	}
	// 校验错误口径。
	postJSON(t, h, "/api/v1/devices", access,
		map[string]string{"platform": "windows", "push_token": "tok"}, http.StatusBadRequest)
	postJSON(t, h, "/api/v1/devices", access,
		map[string]string{"platform": "ios", "push_token": ""}, http.StatusBadRequest)
}

// TestDeviceDeleteOwnershipOnlyMine 删他人设备 404（与不存在同响应）；
// 删自己的 200，二次删除 404。
func TestDeviceDeleteOwnershipOnlyMine(t *testing.T) {
	srv, mem, _, _ := newTestServerWithPush(t)
	h := srv.Handler()
	aAccess, _ := registerUser(t, h, mem, "own@ex.com", "ownerA", "password123")
	bAccess, _ := registerUser(t, h, mem, "other@ex.com", "ownerB", "password123")

	bDev := postJSON(t, h, "/api/v1/devices", bAccess,
		map[string]string{"platform": "ios", "push_token": "tok-bbbbbb0123456789"}, http.StatusCreated)
	aDev := postJSON(t, h, "/api/v1/devices", aAccess,
		map[string]string{"platform": "ios", "push_token": "tok-aaaaaa0123456789"}, http.StatusCreated)

	// A 删 B 的设备 → 404（不泄露存在性）。
	del := delJSON(t, h, fmt.Sprintf("/api/v1/devices/%v", bDev["id"]), aAccess, http.StatusNotFound)
	if del["code"] != "not_found" {
		t.Fatalf("删他人设备应 not_found，实际 %v", del["code"])
	}
	// 删自己的 → 200，再删 → 404。
	delJSON(t, h, fmt.Sprintf("/api/v1/devices/%v", aDev["id"]), aAccess, http.StatusOK)
	delJSON(t, h, fmt.Sprintf("/api/v1/devices/%v", aDev["id"]), aAccess, http.StatusNotFound)
	// 不存在的 ID → 404。
	delJSON(t, h, "/api/v1/devices/999999", aAccess, http.StatusNotFound)
	// B 的设备仍在。
	list := getJSON(t, h, "/api/v1/devices", bAccess, http.StatusOK).(map[string]any)
	if len(list["devices"].([]any)) != 1 {
		t.Fatal("他人误删不得影响本人设备")
	}
}

// TestConfigPushEnabled push_enabled 反映配置：未注入 Push = false，注入 = true。
func TestConfigPushEnabled(t *testing.T) {
	srvPlain, _ := newTestServer(t)
	cfgOff := getJSON(t, srvPlain.Handler(), "/api/v1/config", "", http.StatusOK).(map[string]any)
	if v, ok := cfgOff["push_enabled"].(bool); ok && v {
		t.Fatal("未配置推送应 push_enabled=false")
	}
	srvOn, _, _, _ := newTestServerWithPush(t)
	cfgOn := getJSON(t, srvOn.Handler(), "/api/v1/config", "", http.StatusOK).(map[string]any)
	if cfgOn["push_enabled"] != true {
		t.Fatalf("推送开启应 push_enabled=true，实际 %v", cfgOn["push_enabled"])
	}
}

// setupMentionScene 建服务器/频道并把 B 拉进来（提及校验要求目标可见）。
func setupMentionScene(t *testing.T, h http.Handler, mem *store.Mem) (aAccess, bAccess, chID string, bUID int64) {
	t.Helper()
	aAccess, _ = registerUser(t, h, mem, "m-a@ex.com", "mentionA", "password123")
	bAccess, _ = registerUser(t, h, mem, "m-b@ex.com", "mentionB", "password123")
	sv := postJSON(t, h, "/api/v1/servers", aAccess,
		map[string]string{"name": "推送服", "icon_name": "music", "icon_color": "#4FA4F3"}, http.StatusCreated)
	serverID := asID(sv["id"])
	inv := postJSON(t, h, fmt.Sprintf("/api/v1/servers/%d/invites", serverID), aAccess,
		map[string]any{"max_uses": 1}, http.StatusCreated)
	postJSON(t, h, "/api/v1/servers/join", bAccess,
		map[string]string{"code": inv["code"].(string)}, http.StatusOK)
	ch := postJSON(t, h, fmt.Sprintf("/api/v1/servers/%d/channels", serverID), aAccess,
		map[string]string{"name": "闲聊", "type": "text"}, http.StatusCreated)
	chID = fmt.Sprintf("%d", asID(ch["id"]))
	me := getJSON(t, h, "/api/v1/users/me", bAccess, http.StatusOK).(map[string]any)
	bUID = asID(me["id"])
	return aAccess, bAccess, chID, bUID
}

// TestPushTriggerMentionFullOfflineOnly 全体离线才推：B 离线时被 @ 推一条
// （body 不含消息正文），B 上线后再被 @ 不推（连接表实时判定）。
func TestPushTriggerMentionFullOfflineOnly(t *testing.T) {
	srv, mem, pusher, online := newTestServerWithPush(t)
	h := srv.Handler()
	aAccess, bAccess, chID, bUID := setupMentionScene(t, h, mem)

	// B 注册设备（仍离线：online 表为空）。
	postJSON(t, h, "/api/v1/devices", bAccess,
		map[string]string{"platform": "ios", "push_token": "tok-mention-b0123456"}, http.StatusCreated)
	// A @B。
	postJSON(t, h, fmt.Sprintf("/api/v1/channels/%s/messages", chID), aAccess,
		map[string]any{"content": "这是一段不该出现在推送里的正文", "mentions": []string{fmt.Sprintf("%d", bUID)}},
		http.StatusCreated)
	waitForAPush(t, pusher, 1)
	notif, tokens := pusher.last()
	if len(tokens) != 1 || tokens[0] != "tok-mention-b0123456" {
		t.Fatalf("应投给 B 的设备: %v", tokens)
	}
	if !strings.Contains(notif.Body, "提到你") {
		t.Fatalf("body 应为事实描述: %q", notif.Body)
	}
	if strings.Contains(notif.Body, "不该出现") {
		t.Fatal("推送 body 不得包含消息正文（锁屏泄露红线）")
	}

	// B 上线（连接表实时状态）→ 再被 @ 不推。
	online.online[fmt.Sprintf("%d", bUID)] = true
	postJSON(t, h, fmt.Sprintf("/api/v1/channels/%s/messages", chID), aAccess,
		map[string]any{"content": "上线后再提一次", "mentions": []string{fmt.Sprintf("%d", bUID)}},
		http.StatusCreated)
	time.Sleep(120 * time.Millisecond)
	if pusher.count() != 1 {
		t.Fatalf("在线用户不应收到推送，实际 %d 次", pusher.count())
	}
}

// TestPushTriggerDirectOffline 私聊触发与跨触发类型节流：
// B 离线收到好友请求推送（1 条）后，10 分钟窗口内的私聊被同一节流键吞掉；
// 另一用户 C 无节流记录（注册设备前收到的好友请求不推送也不烧配额），
// 私聊正常推送——验证节流按接收者隔离。
func TestPushTriggerDirectOffline(t *testing.T) {
	srv, mem, pusher, _ := newTestServerWithPush(t)
	h := srv.Handler()
	aAccess, bAccess, _, _ := setupMentionScene(t, h, mem)

	// B 先注册设备（离线）；C 暂不注册（好友请求事件不得烧其节流配额）。
	postJSON(t, h, "/api/v1/devices", bAccess,
		map[string]string{"platform": "android", "push_token": "tok-direct-b012345678"}, http.StatusCreated)
	cAccess, _ := registerUser(t, h, mem, "m-c@ex.com", "dmCee", "password123")
	meC := getJSON(t, h, "/api/v1/users/me", cAccess, http.StatusOK).(map[string]any)

	// A→B 好友请求 → 推 1 条。
	meB := getJSON(t, h, "/api/v1/users/me", bAccess, http.StatusOK).(map[string]any)
	postJSON(t, h, "/api/v1/friends/requests", aAccess,
		map[string]string{"nickname": meB["nickname"].(string), "tag": meB["tag"].(string)}, http.StatusCreated)
	waitForAPush(t, pusher, 1)
	if n, _ := pusher.last(); n.Body != "请求加你为好友" {
		t.Fatalf("好友请求推送 body 不符: %+v", n)
	}

	// A→C 好友请求（C 无设备 → 不推送）；C 接受 → C 注册设备 → 私聊推送 C。
	postJSON(t, h, "/api/v1/friends/requests", aAccess,
		map[string]string{"nickname": meC["nickname"].(string), "tag": meC["tag"].(string)}, http.StatusCreated)
	time.Sleep(100 * time.Millisecond) // 给「本不该推」的路径留失败窗口
	if pusher.count() != 1 {
		t.Fatalf("无设备用户不应推送，实际 %d 次", pusher.count())
	}
	listC := getJSON(t, h, "/api/v1/friends", cAccess, http.StatusOK).([]any)
	var reqID any
	for _, it := range listC {
		entry := it.(map[string]any)
		if entry["status"] == "pending" && entry["incoming"] == true {
			reqID = entry["id"]
		}
	}
	if reqID == nil {
		t.Fatal("未找到 A→C 好友请求")
	}
	postJSON(t, h, fmt.Sprintf("/api/v1/friends/requests/%v/accept", reqID), cAccess, nil, http.StatusOK)
	postJSON(t, h, "/api/v1/devices", cAccess,
		map[string]string{"platform": "ios", "push_token": "tok-direct-c012345678"}, http.StatusCreated)
	conv := postJSON(t, h, "/api/v1/conversations", aAccess,
		map[string]any{"kind": "direct", "user_ids": []any{meC["id"]}}, http.StatusCreated)
	convID := asID(conv["id"])
	postJSON(t, h, fmt.Sprintf("/api/v1/channels/%d/messages", convID), aAccess,
		map[string]string{"content": "私聊正文不进推送"}, http.StatusCreated)
	waitForAPush(t, pusher, 2)
	if n, _ := pusher.last(); !strings.Contains(n.Body, "私聊") {
		t.Fatalf("私聊推送 body 不符: %+v", n)
	}

	// B 接受 A 的好友请求后建私聊：B 在 10 分钟节流窗口内（好友请求已占），
	// 跨触发类型共享 push:<uid> 节流 → 不推。
	listB := getJSON(t, h, "/api/v1/friends", bAccess, http.StatusOK).([]any)
	var reqBID any
	for _, it := range listB {
		entry := it.(map[string]any)
		if entry["status"] == "pending" && entry["incoming"] == true {
			reqBID = entry["id"]
		}
	}
	postJSON(t, h, fmt.Sprintf("/api/v1/friends/requests/%v/accept", reqBID), bAccess, nil, http.StatusOK)
	convB := postJSON(t, h, "/api/v1/conversations", aAccess,
		map[string]any{"kind": "direct", "user_ids": []any{meB["id"]}}, http.StatusCreated)
	postJSON(t, h, fmt.Sprintf("/api/v1/channels/%d/messages", asID(convB["id"])), aAccess,
		map[string]string{"content": "窗口内第二条"}, http.StatusCreated)
	time.Sleep(120 * time.Millisecond)
	if pusher.count() != 2 {
		t.Fatalf("10 分钟节流应吞掉 B 的私聊推送，实际 %d 次", pusher.count())
	}
	// 10 分钟节流：窗口内第二条私聊不推。
	postJSON(t, h, fmt.Sprintf("/api/v1/channels/%d/messages", convID), aAccess,
		map[string]string{"content": "第二条私聊"}, http.StatusCreated)
	time.Sleep(120 * time.Millisecond)
	if pusher.count() != 2 {
		t.Fatalf("10 分钟节流应吞第二条，实际 %d 次", pusher.count())
	}
}
