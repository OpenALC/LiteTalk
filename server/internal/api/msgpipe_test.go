package api

// msgpipe_test.go 消息创建后处理单一管线的端到端验证（P1-4/P2-1）。此前 REST
// 创建不广播、入站 webhook 什么都不走，官方客户端（WS）要等拉取才见、未读/
// 推送/出站 webhook 全不触发；收敛后三条入口共用 signal.Hub.AfterMessagePersisted。
// 这里用真实 hub（广播 + 事件环）+ 真实 Broker + 真实投递器断言四个面向：
// WS 实时可见、未读计数、移动推送、出站 message.created 恰好一次。

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/OpenALC/LiteTalk/server/internal/auth"
	"github.com/OpenALC/LiteTalk/server/internal/push"
	"github.com/OpenALC/LiteTalk/server/internal/sfu"
	"github.com/OpenALC/LiteTalk/server/internal/signal"
	"github.com/OpenALC/LiteTalk/server/internal/store"
	"github.com/OpenALC/LiteTalk/server/internal/webhook"
	"github.com/coder/websocket"
)

// pipeAuth 测试鉴权：令牌即十进制 uid，昵称固定 user-<uid>（UserBrief.Nickname）。
var pipeAuth signal.Authenticator = func(token string) (signal.Identity, error) {
	if token == "" {
		return signal.Identity{}, signal.ErrUnauthorized
	}
	return signal.Identity{UserID: token, Username: "user-" + token, Dev: "desktop"}, nil
}

// newPipeHub 搭一个真实信令 hub（创建管线的家）：UDP 端口 0（临时端口）、无频控；
// authz 缺省自建（与 api 各自缓存，测试内无权限变更，不受失效同步影响）。
func newPipeHub(t *testing.T, mem *store.Mem) *signal.Hub {
	t.Helper()
	inst, err := sfu.New(sfu.Config{UDPPort: 0, MaxMembers: 10, Logger: log.New(io.Discard, "", 0)})
	if err != nil {
		t.Fatalf("sfu.New: %v", err)
	}
	t.Cleanup(func() { _ = inst.Close() })
	return signal.NewHub(inst, pipeAuth, nil, log.New(io.Discard, "", 0), 0, signal.ChatDeps{Store: mem})
}

// pipeWSServer 把 hub 挂到 httptest 服务器上（只有需要 WS 客户端的测试用它）。
func pipeWSServer(t *testing.T, hub *signal.Hub) *httptest.Server {
	t.Helper()
	hs := httptest.NewServer(http.HandlerFunc(hub.ServeWS))
	t.Cleanup(hs.Close)
	return hs
}

// pipeWS 最小 WS 只读客户端：按类型取下行事件（广播/未读验证用）。
type pipeWS struct {
	t    *testing.T
	conn *websocket.Conn
}

// dialPipeWS 建连并等 hello：hello 到达即服务端已完成 register 与事件环 Bind，
// 之后触发的广播不会漏掉本连接（与 signal 侧测试同因）。
func dialPipeWS(t *testing.T, hs *httptest.Server, uid string) *pipeWS {
	t.Helper()
	url := "ws" + strings.TrimPrefix(hs.URL, "http") + "/ws?token=" + uid
	conn, _, err := websocket.Dial(context.Background(), url, nil)
	if err != nil {
		t.Fatalf("dial %s: %v", uid, err)
	}
	t.Cleanup(func() { conn.CloseNow() })
	c := &pipeWS{t: t, conn: conn}
	c.recvUntil(signal.TypeHello)
	return c
}

// recvUntil 逐帧读到指定类型的事件；单帧读超时兜底（事件不来的确定性失败）。
func (c *pipeWS) recvUntil(typ string) signal.Envelope {
	c.t.Helper()
	for {
		readCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		_, data, err := c.conn.Read(readCtx)
		cancel()
		if err != nil {
			c.t.Fatalf("等待 %s 失败: %v", typ, err)
		}
		var env signal.Envelope
		if err := json.Unmarshal(data, &env); err != nil {
			c.t.Fatalf("信封解码失败: %v", err)
		}
		if env.Type == typ {
			return env
		}
	}
}

// newPipeWorld 管线端到端夹具：真实 hub（含 WS 服务器）+ 真实 Broker + 真实
// 出站投递器（httptest 桩）+ REST 服务器。alice 建服建频道；bob 入服（需要观察
// 广播的测试再显式连 WS）；carol 入服、注册设备、保持离线。
type pipeWorld struct {
	srv      *Server
	h        http.Handler
	mem      *store.Mem
	pusher   *recordingAPIPusher
	target   *hookTarget
	hookURL  string
	wssrv    *httptest.Server
	bws      *pipeWS // bob 的 WS 会话（connectBob 后可用）
	access   string  // alice（服主）
	serverID int64
	chID     int64
	bobID    int64
	carolID  int64
}

func newPipeWorld(t *testing.T) *pipeWorld {
	t.Helper()
	mem := store.NewMem()
	target := &hookTarget{}
	hookSrv := httptest.NewServer(target.handler())
	t.Cleanup(hookSrv.Close)

	hub := newPipeHub(t, mem)
	wssrv := pipeWSServer(t, hub)
	pusher := &recordingAPIPusher{}
	b := push.NewBroker(push.BrokerConfig{
		Pusher: pusher,
		Store:  mem,
		Online: hub, // 连接表实时判定：未连接 = 全设备离线
		Limits: auth.NewRateLimit(mem),
	})
	if b == nil {
		t.Fatal("push.Broker 未构造")
	}
	hub.SetPushNotifier(b)
	t.Cleanup(b.Close)

	disp := webhook.New(webhook.Config{CheckURL: func(string) error { return nil }}, mem)
	disp.Start(nil)
	t.Cleanup(disp.Close)
	hub.SetWebhookOutlet(disp) // message.created 出口挂钩（与生产 main.go 同构）

	srv := New(Config{
		Store:           mem,
		JWT:             auth.NewJWT("test-secret"),
		Refresh:         auth.NewRefreshStore(mem),
		Codes:           auth.NewEmailCodes(mem),
		Limits:          auth.NewRateLimit(mem),
		Session:         auth.NewSessionIndex(mem, mem),
		Presence:        mem,
		Hub:             hub,
		Push:            b,
		Webhooks:        disp,
		WebhookURLCheck: func(string) error { return nil },
	})
	h := srv.Handler()

	access, _ := registerUser(t, h, mem, "pipe-a@ex.com", "pipeA", "password123")
	serverID := asID(postJSON(t, h, "/api/v1/servers", access, map[string]string{
		"name": "管线服", "icon_name": "music", "icon_color": "#4FA4F3",
	}, http.StatusCreated)["id"])
	chID := asID(postJSON(t, h, fmt.Sprintf("/api/v1/servers/%d/channels", serverID), access,
		map[string]any{"name": "general", "type": "text"}, http.StatusCreated)["id"])

	join := func(email, nick string) (string, int64) {
		acc, _ := registerUser(t, h, mem, email, nick, "password123")
		inv := postJSON(t, h, fmt.Sprintf("/api/v1/servers/%d/invites", serverID), access,
			map[string]any{"max_uses": 5}, http.StatusCreated)
		postJSON(t, h, "/api/v1/servers/join", acc, map[string]string{"code": inv["code"].(string)}, http.StatusOK)
		me := getJSON(t, h, "/api/v1/users/me", acc, http.StatusOK).(map[string]any)
		return acc, asID(me["id"])
	}
	_, bobID := join("pipe-b@ex.com", "pipeB")
	carolAcc, carolID := join("pipe-c@ex.com", "pipeC")
	postJSON(t, h, "/api/v1/devices", carolAcc,
		map[string]string{"platform": "ios", "push_token": "tok-pipe-c012345678"}, http.StatusCreated)

	return &pipeWorld{
		srv: srv, h: h, mem: mem, pusher: pusher, target: target,
		hookURL: hookSrv.URL, wssrv: wssrv, access: access,
		serverID: serverID, chID: chID, bobID: bobID, carolID: carolID,
	}
}

// connectBob 连上 bob 的 WS 会话（在需要观察广播的测试里显式调用：连接即在线，
// 会抑制对 bob 的推送）。
func (w *pipeWorld) connectBob(t *testing.T) {
	t.Helper()
	w.bws = dialPipeWS(t, w.wssrv, strconv.FormatInt(w.bobID, 10))
}

// countEvents 统计桩已收到的指定事件数。
func (w *pipeWorld) countEvents(event string) int {
	w.target.mu.Lock()
	defer w.target.mu.Unlock()
	n := 0
	for _, e := range w.target.events {
		if e == event {
			n++
		}
	}
	return n
}

// assertMessageCreatedOnce 断言 message.created 恰好一次且载荷正确（异步投递
// 的确定性等待 + 静置后复核不重复）。
func (w *pipeWorld) assertMessageCreatedOnce(t *testing.T, content string) {
	t.Helper()
	body := waitEvent(t, w.target, "message.created")
	var env struct {
		Event     string         `json:"event"`
		ServerID  string         `json:"server_id"`
		ChannelID string         `json:"channel_id"`
		Data      map[string]any `json:"data"`
	}
	if err := json.Unmarshal(body, &env); err != nil {
		t.Fatal(err)
	}
	if env.Event != "message.created" ||
		env.ServerID != strconv.FormatInt(w.serverID, 10) ||
		env.ChannelID != strconv.FormatInt(w.chID, 10) ||
		env.Data["content"] != content {
		t.Fatalf("message.created 载荷不符: %+v", env)
	}
	time.Sleep(150 * time.Millisecond)
	if got := w.countEvents("message.created"); got != 1 {
		t.Fatalf("message.created 应恰好一次（广播挂钩唯一派发路径），实际 %d", got)
	}
}

// TestRESTCreateMessagePipeline REST 创建 → 管线四面向（P2-1 核心回归）：
// WS 实时收到 chat.message、观察者收到自己的精确未读、离线被提及者收到推送
// （body 不含正文）、出站订阅收到恰好一次 message.created。
func TestRESTCreateMessagePipeline(t *testing.T) {
	w := newPipeWorld(t)
	w.connectBob(t)
	seedOutbound(t, w.mem, w.serverID, w.hookURL)

	aliceID := meID(t, w.h, w.access)
	postJSON(t, w.h, fmt.Sprintf("/api/v1/channels/%d/messages", w.chID), w.access, map[string]any{
		"content":  "这是一段不该出现在推送里的正文",
		"mentions": []any{w.bobID, w.carolID},
	}, http.StatusCreated)

	// ① WS 实时可见（此前 REST 不广播，要等拉取才见）。
	var msg signal.ChatMessageData
	if err := w.bws.recvUntil(signal.TypeChatMessage).Decode(&msg); err != nil {
		t.Fatalf("decode chat.message: %v", err)
	}
	if msg.Content != "这是一段不该出现在推送里的正文" || msg.User.ID != strconv.FormatInt(aliceID, 10) {
		t.Fatalf("chat.message 载荷不符: %+v", msg)
	}
	if len(msg.Mentions) != 2 {
		t.Fatalf("mentions 应为 2 项: %v", msg.Mentions)
	}

	// ② 未读：观察者自己的精确计数（1 未读 / 1 提及）。
	var unread signal.ChannelUnreadData
	if err := w.bws.recvUntil(signal.TypeChannelUnread).Decode(&unread); err != nil {
		t.Fatalf("decode channel.unread: %v", err)
	}
	if unread.ChannelID != strconv.FormatInt(w.chID, 10) ||
		unread.UnreadCount != 1 || unread.MentionCount != 1 {
		t.Fatalf("channel.unread 不符: %+v", unread)
	}

	// ③ 推送：carol 全设备离线 → 恰 1 条，body 不含消息正文（锁屏红线）。
	waitForAPush(t, w.pusher, 1)
	if n, tokens := w.pusher.last(); len(tokens) != 1 || tokens[0] != "tok-pipe-c012345678" ||
		!strings.Contains(n.Body, "提到你") || strings.Contains(n.Body, "不该出现") {
		t.Fatalf("推送不符: notif=%+v tokens=%v", n, tokens)
	}

	// ④ 出站 webhook：恰好一次（广播挂钩是唯一派发路径，REST 无直发）。
	w.assertMessageCreatedOnce(t, "这是一段不该出现在推送里的正文")
}

// TestRESTCreatePipelineIdempotent 幂等回放不进管线：同 nonce 二次提交回 200
// 原消息，管线只在首次创建时刻跑一次（不重复广播/推送/出站 webhook）。
func TestRESTCreatePipelineIdempotent(t *testing.T) {
	w := newPipeWorld(t)
	fake := &fakeHub{}
	w.srv.hub = fake // 管线入口换成记录桩：只断言调用次数，不看广播行为

	body := map[string]any{"content": "幂等验证", "nonce": "pipe-nonce-123"}
	postJSON(t, w.h, fmt.Sprintf("/api/v1/channels/%d/messages", w.chID), w.access, body, http.StatusCreated)
	postJSON(t, w.h, fmt.Sprintf("/api/v1/channels/%d/messages", w.chID), w.access, body, http.StatusOK)

	calls := fake.createLog()
	if len(calls) != 1 {
		t.Fatalf("管线应只跑一次，实际 %d: %+v", len(calls), calls)
	}
	if calls[0].senderUID != strconv.FormatInt(meID(t, w.h, w.access), 10) ||
		calls[0].channelID != w.chID || calls[0].senderNick != "pipeA" {
		t.Fatalf("管线入参不符: %+v", calls[0])
	}
}

// TestInboundWebhookPipeline 入站投递 → 管线（P2-1）：机器人消息同样实时广播、
// 计未读、走出站 webhook 恰一次；无提及且非私聊 → 推送零触发。
func TestInboundWebhookPipeline(t *testing.T) {
	w := newPipeWorld(t)
	w.connectBob(t)
	seedOutbound(t, w.mem, w.serverID, w.hookURL)

	iw := postJSON(t, w.h, fmt.Sprintf("/api/v1/channels/%d/inbound-webhooks", w.chID), w.access,
		map[string]any{"description": "CI"}, http.StatusCreated)
	postJSON(t, w.h, iw["url"].(string), "", map[string]string{"content": "来自 CI 的消息"}, http.StatusOK)

	// ① WS 实时可见，发送者为 webhook 机器人身份。
	var msg signal.ChatMessageData
	if err := w.bws.recvUntil(signal.TypeChatMessage).Decode(&msg); err != nil {
		t.Fatalf("decode chat.message: %v", err)
	}
	if msg.Content != "来自 CI 的消息" || msg.User.Nickname != "Webhook" {
		t.Fatalf("入站消息广播不符: %+v", msg)
	}

	// ② 未读：观察者 1 未读 / 0 提及。
	var unread signal.ChannelUnreadData
	if err := w.bws.recvUntil(signal.TypeChannelUnread).Decode(&unread); err != nil {
		t.Fatalf("decode channel.unread: %v", err)
	}
	if unread.UnreadCount != 1 || unread.MentionCount != 0 {
		t.Fatalf("channel.unread 不符: %+v", unread)
	}

	// ③ 推送零触发：服务器频道无提及、非私聊，管线的推送步自然短路。
	time.Sleep(150 * time.Millisecond)
	if got := w.pusher.count(); got != 0 {
		t.Fatalf("无提及的机器人消息不应推送，实际 %d 次", got)
	}

	// ④ 出站 webhook：恰好一次。
	w.assertMessageCreatedOnce(t, "来自 CI 的消息")
}
