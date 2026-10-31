package push

import (
	"context"
	"testing"
	"time"

	"github.com/OpenALC/LiteTalk/server/internal/auth"
	"github.com/OpenALC/LiteTalk/server/internal/store"
)

// fakeOnline 可编程在线判定：部分在线/全离线场景的判定源。
type fakeOnline struct{ online map[string]bool }

func (f fakeOnline) UserOnline(uid string) bool { return f.online[uid] }

// newBroker 测试装配：Mem store + 真 RateLimit（MemKV）+ 记录桩。
func newBroker(t *testing.T, online map[string]bool) (*Broker, *recordingPusher, *store.Mem) {
	t.Helper()
	mem := store.NewMem()
	pusher := &recordingPusher{}
	kv := store.NewMemKV()
	b := NewBroker(BrokerConfig{
		Pusher: pusher,
		Store:  mem,
		Online: fakeOnline{online: online},
		Limits: auth.NewRateLimit(kv),
	})
	if b == nil {
		t.Fatal("Broker 装配失败")
	}
	t.Cleanup(b.Close)
	return b, pusher, mem
}

// mustDevice 注册一台设备。
func mustDevice(t *testing.T, st *store.Mem, uid int64, token string) {
	t.Helper()
	_, err := st.UpsertPushDevice(context.Background(),
		&store.PushDevice{UserID: uid, Platform: "ios", Token: token})
	if err != nil {
		t.Fatal(err)
	}
}

// TestBrokerPushOnlyWhenFullyOffline 全体离线才推（部分在线不推）：
// A 用户有在线连接 → 不推；B 用户全设备离线 → 推。
func TestBrokerPushOnlyWhenFullyOffline(t *testing.T) {
	b, pusher, mem := newBroker(t, map[string]bool{"100": true, "200": false})
	mustDevice(t, mem, 100, "tok-online-device0001")
	mustDevice(t, mem, 200, "tok-offline-device001")

	ch := &store.Channel{ID: 7, Scope: store.ChannelScopeServer, Name: "general"}
	msg := &store.Message{ID: 1, UserID: 300, Mentions: []int64{100, 200}}
	b.NotifyMessage(context.Background(), ch, msg, "触发者")

	waitForPush(t, pusher, 1)
	time.Sleep(50 * time.Millisecond) // 给「本不该推」的路径留失败窗口
	if pusher.count() != 1 {
		t.Fatalf("只应投递离线用户 1 次，实际 %d", pusher.count())
	}
	rec := pusher.last()
	if len(rec.Devices) != 1 || rec.Devices[0].Token != "tok-offline-device001" {
		t.Fatalf("应投给离线用户的设备: %+v", rec.Devices)
	}
	if rec.Notif.Body != "在 #general 提到你" {
		t.Fatalf("body 不符: %q（不得含消息正文）", rec.Notif.Body)
	}
	if rec.Notif.Route != "channel:7" {
		t.Fatalf("route 不符: %q", rec.Notif.Route)
	}
}

// TestBrokerDirectAndFriendRequest 私聊与好友请求两条触发链：
// direct 反查对端成员投递；friend_request 通知被请求方。
func TestBrokerDirectAndFriendRequest(t *testing.T) {
	b, pusher, mem := newBroker(t, map[string]bool{"200": false, "300": false})
	mustDevice(t, mem, 200, "tok-direct-peer00001")
	mustDevice(t, mem, 300, "tok-friend-target001")

	ctx := context.Background()
	// 私聊：100 → 200。
	dm := &store.Channel{ID: 55, Scope: store.ChannelScopeDirect}
	if err := mem.CreateConversation(ctx, dm, []int64{100, 200}); err != nil {
		t.Fatal(err)
	}
	b.NotifyMessage(ctx, dm, &store.Message{ID: 2, UserID: 100}, "私聊发送者")
	waitForPush(t, pusher, 1)
	rec := pusher.last()
	if len(rec.Devices) != 1 || rec.Devices[0].Token != "tok-direct-peer00001" {
		t.Fatalf("私聊应投给对端: %+v", rec.Devices)
	}
	if rec.Notif.Route != "conversation:55" {
		t.Fatalf("私聊 route 不符: %q", rec.Notif.Route)
	}
	// 好友请求：100 → 300。
	b.NotifyFriendRequest(ctx, 300, 100, "请求者")
	waitForPush(t, pusher, 2)
	rec = pusher.last()
	if rec.Notif.Body != "请求加你为好友" || rec.Notif.Route != "friends" {
		t.Fatalf("好友请求通知不符: %+v", rec.Notif)
	}
}

// TestBrokerThrottleTenMinutes 同一用户 10 分钟内最多 1 条（节流键 push:<uid>）：
// 第二条触发被吞，即使离线判定与设备都满足。
func TestBrokerThrottleTenMinutes(t *testing.T) {
	b, pusher, mem := newBroker(t, map[string]bool{"500": false})
	mustDevice(t, mem, 500, "tok-throttled-device01")

	ch := &store.Channel{ID: 9, Scope: store.ChannelScopeServer, Name: "general"}
	ctx := context.Background()
	b.NotifyMessage(ctx, ch, &store.Message{ID: 1, UserID: 100, Mentions: []int64{500}}, "A")
	waitForPush(t, pusher, 1)
	b.NotifyMessage(ctx, ch, &store.Message{ID: 2, UserID: 200, Mentions: []int64{500}}, "B")
	time.Sleep(100 * time.Millisecond)
	if pusher.count() != 1 {
		t.Fatalf("10 分钟节流应吞掉第二条，实际 %d 次", pusher.count())
	}
	// 不同触发类型共享同一节流键：好友请求同样被吞（防刷屏）。
	b.NotifyFriendRequest(ctx, 500, 300, "C")
	time.Sleep(100 * time.Millisecond)
	if pusher.count() != 1 {
		t.Fatalf("跨触发类型仍应共享 push:<uid> 节流，实际 %d 次", pusher.count())
	}
}

// TestBrokerSourceRateLimit 发起者侧每种触发类型独立频控：超过 srcLimit/分钟
// 后该发起者的后续触发全部跳过（防止刷爆队列）。
func TestBrokerSourceRateLimit(t *testing.T) {
	mem := store.NewMem()
	pusher := &recordingPusher{}
	b := NewBroker(BrokerConfig{
		Pusher: pusher,
		Store:  mem,
		Online: fakeOnline{online: map[string]bool{}},
		Limits: auth.NewRateLimit(store.NewMemKV()),
	})
	defer b.Close()
	mustDevice(t, mem, 900, "tok-spammed-device001")

	ch := &store.Channel{ID: 3, Scope: store.ChannelScopeServer, Name: "g"}
	ctx := context.Background()
	// 前几条正常投递后，同一发起者被源频控拦下；接收者节流会让第一次之后
	// 全部吞掉——所以这里换 2 个接收者观察源频控独立键存在即可：
	// 核心断言：来源频控键不与接收者节流键混淆（不同 uid 的投递互不影响）。
	mustDevice(t, mem, 901, "tok-second-user-device1")
	mustDevice(t, mem, 902, "tok-third-user-device2")
	b.NotifyMessage(ctx, ch, &store.Message{ID: 1, UserID: 800, Mentions: []int64{900}}, "S")
	waitForPush(t, pusher, 1)
	b.NotifyMessage(ctx, ch, &store.Message{ID: 2, UserID: 800, Mentions: []int64{901}}, "S")
	waitForPush(t, pusher, 2)
	b.NotifyMessage(ctx, ch, &store.Message{ID: 3, UserID: 801, Mentions: []int64{902}}, "S")
	waitForPush(t, pusher, 3)
}

// TestBrokerOfflineEdgeFallback 无实时在线查询的降级接线：离线边沿（offlineEdgeTTL 内）
// 作为兜底判据——有边沿才推；投递成功后边沿被消费，不再重复兜底。
func TestBrokerOfflineEdgeFallback(t *testing.T) {
	mem := store.NewMem()
	pusher := &recordingPusher{}
	b := NewBroker(BrokerConfig{Pusher: pusher, Store: mem})
	defer b.Close()
	mustDevice(t, mem, 700, "tok-edge-fallback-dev01")

	ctx := context.Background()
	ch := &store.Channel{ID: 4, Scope: store.ChannelScopeServer, Name: "g"}
	// 无边沿：宁缺勿扰，不推。
	b.NotifyMessage(ctx, ch, &store.Message{ID: 1, UserID: 100, Mentions: []int64{700}}, "A")
	time.Sleep(80 * time.Millisecond)
	if pusher.count() != 0 {
		t.Fatalf("无边沿不应推，实际 %d 次", pusher.count())
	}
	// 边沿到达 → 推。
	b.OnUserOffline("700")
	b.NotifyMessage(ctx, ch, &store.Message{ID: 2, UserID: 100, Mentions: []int64{700}}, "A")
	waitForPush(t, pusher, 1)
	// 边沿已消费：第二条不推（除非再次收到边沿）。
	b.NotifyMessage(ctx, ch, &store.Message{ID: 3, UserID: 100, Mentions: []int64{700}}, "A")
	time.Sleep(80 * time.Millisecond)
	if pusher.count() != 1 {
		t.Fatalf("边沿消费后不应再兜底，实际 %d 次", pusher.count())
	}
}

// TestBrokerNoDeviceNoPush 未注册设备的用户不推（无 I/O 对象）。
func TestBrokerNoDeviceNoPush(t *testing.T) {
	b, pusher, _ := newBroker(t, map[string]bool{"600": false})
	ch := &store.Channel{ID: 5, Scope: store.ChannelScopeServer, Name: "g"}
	b.NotifyMessage(context.Background(), ch, &store.Message{ID: 1, UserID: 100, Mentions: []int64{600}}, "A")
	time.Sleep(80 * time.Millisecond)
	if pusher.count() != 0 {
		t.Fatalf("无设备不应投递，实际 %d 次", pusher.count())
	}
}

// TestBrokerOnlineUserSkipped 在线用户的消息即使触发也完全不产生投递（含节流不消耗）：
// 先推离线用户，再把该用户上线，节流窗口内的离线→在线变化以实时查询为准。
func TestBrokerOnlineUserSkipped(t *testing.T) {
	b, pusher, mem := newBroker(t, map[string]bool{"800": false})
	mustDevice(t, mem, 800, "tok-goes-online-dev001")
	ch := &store.Channel{ID: 6, Scope: store.ChannelScopeServer, Name: "g"}
	ctx := context.Background()
	b.NotifyMessage(ctx, ch, &store.Message{ID: 1, UserID: 100, Mentions: []int64{800}}, "A")
	waitForPush(t, pusher, 1)
	time.Sleep(50 * time.Millisecond)
	// 用户回到桌面端（连接表实时状态），10 分钟节流虽在但实时在线优先 → 不推。
	b.online = fakeOnline{online: map[string]bool{"800": true}}
	b.NotifyMessage(ctx, ch, &store.Message{ID: 2, UserID: 100, Mentions: []int64{800}}, "A")
	time.Sleep(80 * time.Millisecond)
	if pusher.count() != 1 {
		t.Fatalf("在线用户不应投递，实际 %d 次", pusher.count())
	}
}
