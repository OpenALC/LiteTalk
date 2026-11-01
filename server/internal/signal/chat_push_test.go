package signal

// chat_push_test.go 创建管线推送步（P1-4 核心回归）：WS chat.send 落库后经
// AfterMessagePersisted 派发推送——@提及与私聊在目标全设备离线时必须到达
// Broker 并投递，目标在线则不推。此前推送只挂在 REST CreateMessage，官方
// 客户端（桌面/Web 都走 WS）的 @提及/私聊推送实际永不触发。

import (
	"context"
	"io"
	"log"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/OpenALC/LiteTalk/server/internal/auth"
	"github.com/OpenALC/LiteTalk/server/internal/push"
	"github.com/OpenALC/LiteTalk/server/internal/store"
)

// recordingPipePusher 记录投递结果的 Pusher 桩。
type recordingPipePusher struct {
	mu  sync.Mutex
	got []push.Notification
}

func (r *recordingPipePusher) Push(_ context.Context, _ []push.Device, n push.Notification) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.got = append(r.got, n)
	return nil
}

func (r *recordingPipePusher) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.got)
}

func (r *recordingPipePusher) last() push.Notification {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.got) == 0 {
		return push.Notification{}
	}
	return r.got[len(r.got)-1]
}

// attachPushBroker 在 chat 夹具上接一个真实 Broker：在线判定复用 hub 连接表
// （未连接 = 全设备离线），Pusher 换成记录桩，与生产 main.go 同构。
func attachPushBroker(t *testing.T, f *chatFixture) *recordingPipePusher {
	t.Helper()
	pusher := &recordingPipePusher{}
	b := push.NewBroker(push.BrokerConfig{
		Pusher: pusher,
		Store:  f.st,
		Online: f.hub,
		Limits: auth.NewRateLimit(f.st),
		Log:    log.New(io.Discard, "", 0),
	})
	if b == nil {
		t.Fatal("push.Broker 未构造")
	}
	f.hub.SetPushNotifier(b)
	t.Cleanup(b.Close)
	return pusher
}

// waitPipePush 等待第 n 次投递到达（异步派发的确定性等待）。
func waitPipePush(t *testing.T, r *recordingPipePusher, n int) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for r.count() < n {
		if time.Now().After(deadline) {
			t.Fatalf("等待第 %d 次投递超时（实际 %d）", n, r.count())
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// TestChatSendMentionPushesFullyOffline WS @提及：目标未连接（全设备离线）且注册
// 了移动设备 → 推送到达；目标上线后同样路径不再推（连接表实时判定）。
func TestChatSendMentionPushesFullyOffline(t *testing.T) {
	f := newChatFixture(t)
	pusher := attachPushBroker(t, f)
	if _, err := f.st.UpsertPushDevice(context.Background(), &store.PushDevice{
		UserID: 2, Platform: "ios", Token: "tok-pipe-b0123456789",
	}); err != nil {
		t.Fatalf("UpsertPushDevice: %v", err)
	}

	a := f.dial("1", "alice")
	a.send(TypeChatSend, ChatSendData{
		ChannelID: strconv.FormatInt(f.ch.ID, 10), Type: "text",
		Content: "@bob 看一眼", Mentions: []string{"2"},
	})
	// 广播先于推送入队（管线的广播步在推送步之前），广播到达即推送已入队。
	a.recvUntil(TypeChatMessage)
	waitPipePush(t, pusher, 1)
	if n := pusher.last(); !strings.Contains(n.Body, "提到你") {
		t.Fatalf("body 应为事实描述: %+v", n)
	}

	// bob 上线（任一连接）→ 再被 @ 不推。
	b := f.dial("2", "bob")
	a.send(TypeChatSend, ChatSendData{
		ChannelID: strconv.FormatInt(f.ch.ID, 10), Type: "text",
		Content: "上线后再提一次", Mentions: []string{"2"},
	})
	a.recvUntil(TypeChatMessage)
	b.recvUntil(TypeChatMessage)
	time.Sleep(120 * time.Millisecond)
	if pusher.count() != 1 {
		t.Fatalf("在线目标不应收到推送，实际 %d 次", pusher.count())
	}
}

// TestChatSendDirectPushesFullyOffline 私聊：发送者在线、对端全设备离线且注册
// 了设备 → 推送到达（对端成员由 Broker worker 反查，发送路径零额外查询）。
func TestChatSendDirectPushesFullyOffline(t *testing.T) {
	f := newChatFixture(t)
	pusher := attachPushBroker(t, f)
	ctx := context.Background()
	conv := &store.Channel{Scope: store.ChannelScopeDirect}
	if err := f.st.CreateConversation(ctx, conv, []int64{1, 2}); err != nil {
		t.Fatalf("CreateConversation: %v", err)
	}
	if _, err := f.st.UpsertPushDevice(ctx, &store.PushDevice{
		UserID: 2, Platform: "android", Token: "tok-pipe-d0123456789",
	}); err != nil {
		t.Fatalf("UpsertPushDevice: %v", err)
	}

	a := f.dial("1", "alice")
	a.send(TypeChatSend, ChatSendData{
		ChannelID: strconv.FormatInt(conv.ID, 10), Type: "text", Content: "在吗",
	})
	a.recvUntil(TypeChatMessage)
	waitPipePush(t, pusher, 1)
	if n := pusher.last(); !strings.Contains(n.Body, "私聊") {
		t.Fatalf("私聊推送 body 不符: %+v", n)
	}
}
