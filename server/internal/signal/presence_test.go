package signal

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/OpenALC/LiteTalk/server/internal/store"
)

// waitPresence 等待 presence 键出现/消失（cleanup 在连接断开后异步跑，需轮询）。
func waitPresence(t *testing.T, st *store.Mem, uid string, wantPresent bool) {
	t.Helper()
	key := presenceKey(uid)
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		_, err := st.Get(context.Background(), key)
		if (err == nil) == wantPresent {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("presence %s: 期望存在=%v，超时未达成", key, wantPresent)
}

// withPresence 给 fixture 挂上 KV presence（在任何拨号之前调用）。
func (f *chatFixture) withPresence() *chatFixture {
	f.hub.presence = f.st
	return f
}

// waitConnCount 等待某 uid 的连接登记数达成（拨号返回与 register 之间有窗口）。
func waitConnCount(t *testing.T, f *chatFixture, uid string, want int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if f.hub.connCount(uid) == want {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("connCount(%s) = %d, want %d", uid, f.hub.connCount(uid), want)
}

// pump 持续读一条 WS 连接并把消息转发到 channel。
// 负向断言（限时等「不该来」的消息）不能用带超时 ctx 的 Read 直接读——
// coder/websocket 在 Read 的 ctx 取消时会把连接一起关掉，测试会自杀。
type pump struct {
	t  *testing.T
	ch chan Envelope
}

func newPump(t *testing.T, c *wsClient) *pump {
	p := &pump{t: t, ch: make(chan Envelope, 64)}
	// dial 为保证服务端已 register 会预读到 hello 并暂存 c.pending；pump 的读循环
	// 直接走 c.conn，不排空缓冲就会永远等不到那条 hello（until(TypeHello) 超时）。
	for _, env := range c.pending {
		p.ch <- env
	}
	c.pending = nil
	go func() {
		for {
			_, data, err := c.conn.Read(context.Background())
			if err != nil {
				close(p.ch)
				return
			}
			var env Envelope
			if err := json.Unmarshal(data, &env); err != nil {
				continue
			}
			p.ch <- env
		}
	}()
	return p
}

// until 读到指定类型（跳过 hello 等中间消息）；5s 未到即失败。
func (p *pump) until(want string) Envelope {
	p.t.Helper()
	deadline := time.After(5 * time.Second)
	for {
		select {
		case env, ok := <-p.ch:
			if !ok {
				p.t.Fatalf("连接已关闭，未等到 %s", want)
			}
			if env.Type == want {
				return env
			}
		case <-deadline:
			p.t.Fatalf("until %s: 超时", want)
		}
	}
}

// within 限时等一条消息（不触碰底层 Read，连接保持存活）；超时 ok=false。
func (p *pump) within(d time.Duration) (Envelope, bool) {
	select {
	case env, ok := <-p.ch:
		return env, ok
	case <-time.After(d):
		return Envelope{}, false
	}
}

// makeFriends 建一条好友关系（status 决定是否已接受）。
func makeFriends(t *testing.T, f *chatFixture, a, b int64, status store.FriendshipStatus) {
	t.Helper()
	if err := f.st.CreateFriendship(context.Background(), &store.Friendship{
		UserID: a, FriendID: b, Status: status,
	}); err != nil {
		t.Fatalf("CreateFriendship: %v", err)
	}
}

// TestPresenceConnectDisconnect 连接 register 后 SET，断开且无其它连接后 DEL。
func TestPresenceConnectDisconnect(t *testing.T) {
	f := newChatFixture(t).withPresence()

	a := f.dial("1", "alice")
	waitPresence(t, f.st, "1", true)

	_ = a.conn.CloseNow()
	waitPresence(t, f.st, "1", false)
}

// TestPresenceMultiDevice 多设备：一台断开不 DEL，全部断开才 DEL（不按 uid 盲删）。
func TestPresenceMultiDevice(t *testing.T) {
	f := newChatFixture(t).withPresence()

	desktop := f.dial("1", "alice-desktop")
	mobile := f.dial("1:mobile:"+defaultTestSid("1"), "alice-mobile")
	waitConnCount(t, f, "1", 2)
	waitPresence(t, f.st, "1", true)

	_ = desktop.conn.CloseNow()
	time.Sleep(200 * time.Millisecond) // 给 cleanup 留窗口
	if _, err := f.st.Get(context.Background(), presenceKey("1")); err != nil {
		t.Fatalf("mobile 仍在线时不应 DEL: %v", err)
	}

	_ = mobile.conn.CloseNow()
	waitPresence(t, f.st, "1", false)
}

// TestPresenceReplaceOldConn 同 uid+dev 被顶替：旧连接 cleanup 不得误删新连接的在线态。
func TestPresenceReplaceOldConn(t *testing.T) {
	f := newChatFixture(t).withPresence()

	f.dial("1:desktop:oldsid", "alice-old")
	waitPresence(t, f.st, "1", true)

	// 同 uid+dev 新连接顶替（sid 不同 = 新登录，服务端 close 旧连接 → 旧 cleanup）。
	fresh := f.dial("1:desktop:newsid", "alice-new")
	time.Sleep(300 * time.Millisecond) // 旧连接 cleanup 异步跑完的窗口
	if _, err := f.st.Get(context.Background(), presenceKey("1")); err != nil {
		t.Fatalf("顶替后的旧 cleanup 误删了在线态: %v", err)
	}
	if got := f.hub.connCount("1"); got != 1 {
		t.Fatalf("顶替后应剩 1 条连接，实际 %d", got)
	}

	_ = fresh.conn.CloseNow()
	waitPresence(t, f.st, "1", false)
}

// TestPresencePushToFriends 在线状态只推给互为已接受好友的在线连接：
// 好友收到 presence.update；pending 关系不收。
func TestPresencePushToFriends(t *testing.T) {
	f := newChatFixture(t).withPresence()
	makeFriends(t, f, 1, 2, store.FriendAccepted) // alice–bob 已接受
	makeFriends(t, f, 1, 9, store.FriendPending)  // alice–mallory 待处理

	bob := newPump(t, f.dial("2", "bob"))
	mallory := newPump(t, f.dial("9", "mallory"))
	bob.until(TypeHello)
	mallory.until(TypeHello)

	alice := f.dial("1", "alice")
	env := bob.until(TypePresenceUpdate)
	var d PresenceUpdateData
	if err := env.Decode(&d); err != nil {
		t.Fatalf("decode presence.update: %v", err)
	}
	if d.UserID != "1" || !d.Online {
		t.Fatalf("presence.update = %+v, want user_id=1 online=true", d)
	}

	// pending 关系的 mallory 不该收到。
	if got, ok := mallory.within(300 * time.Millisecond); ok {
		t.Fatalf("pending 关系不应收到 presence.update，实际 %+v", got)
	}

	_ = alice.conn.CloseNow()
	waitPresence(t, f.st, "1", false)
	env = bob.until(TypePresenceUpdate)
	if err := env.Decode(&d); err != nil || d.Online {
		t.Fatalf("断开应推 online=false，实际 %+v err=%v", d, err)
	}
}

// TestPresenceDebounce 状态未变不推：多设备上线只推一次 online=true，
// 一台断开不推 online=false，全部断开才推一次。
func TestPresenceDebounce(t *testing.T) {
	f := newChatFixture(t).withPresence()
	makeFriends(t, f, 1, 2, store.FriendAccepted)

	bob := newPump(t, f.dial("2", "bob"))
	bob.until(TypeHello)

	desktop := f.dial("1", "alice-desktop")
	env := bob.until(TypePresenceUpdate)
	var d PresenceUpdateData
	if err := env.Decode(&d); err != nil || !d.Online {
		t.Fatalf("首连应推 online=true，实际 %+v err=%v", d, err)
	}

	// 第二台设备上线：状态未变，不重推。
	mobile := f.dial("1:mobile:"+defaultTestSid("1"), "alice-mobile")
	waitConnCount(t, f, "1", 2)
	if got, ok := bob.within(300 * time.Millisecond); ok {
		t.Fatalf("重复在线状态不应推送，实际 %+v", got)
	}

	// 一台断开：仍在线，不推。
	_ = desktop.conn.CloseNow()
	if got, ok := bob.within(300 * time.Millisecond); ok {
		t.Fatalf("多设备仍在线时不应推离线，实际 %+v", got)
	}

	// 全部断开：推一次 online=false。
	_ = mobile.conn.CloseNow()
	env = bob.until(TypePresenceUpdate)
	if err := env.Decode(&d); err != nil || d.Online {
		t.Fatalf("全断应推 online=false，实际 %+v err=%v", d, err)
	}
}

// TestPresenceHeartbeatRenew 心跳成功续期：TTL 短于测试观察窗口时，
// 只要连接活着（Ping/Pong 正常）presence 键就不得过期。
func TestPresenceHeartbeatRenew(t *testing.T) {
	f := newChatFixture(t).withPresence()
	f.hub.presenceTTL = 200 * time.Millisecond
	f.hub.heartbeatEvery = 50 * time.Millisecond

	// pump 常驻读：coder/websocket 的 Pong 在 Read 中处理，不读则 Ping 收不到 Pong。
	alice := f.dial("1", "alice")
	newPump(t, alice)
	waitPresence(t, f.st, "1", true)

	time.Sleep(500 * time.Millisecond) // 超过 2×TTL：无续期早已过期
	if _, err := f.st.Get(context.Background(), presenceKey("1")); err != nil {
		t.Fatalf("心跳续期未生效，键已过期: %v", err)
	}
	_ = alice.conn.CloseNow()
	waitPresence(t, f.st, "1", false)
}

// ---- 边沿判定与推送（#11）----

// newPresenceHub 构造只含连接索引 / presence / 事件面的最小 Hub（不建 SFU/HTTP），
// 供并发边沿测试直接驱动 register/unregister。返回的 bob（uid=2）已登记为连接，
// 与 alice（uid=1）互为已接受好友，会收到 presence.update。
func newPresenceHub(t *testing.T) (*Hub, *store.Mem, *client) {
	t.Helper()
	st := store.NewMem()
	if err := st.CreateFriendship(context.Background(), &store.Friendship{
		UserID: 1, FriendID: 2, Status: store.FriendAccepted,
	}); err != nil {
		t.Fatalf("CreateFriendship: %v", err)
	}
	h := &Hub{
		log:               log.New(io.Discard, "", 0),
		conns:             make(map[connKey]*client),
		presence:          st,
		evlog:             newMemEventLog(16),
		presencePublished: make(map[string]bool),
	}
	bob := &client{hub: h, userID: "2", send: make(chan []byte, 1024)}
	h.register(bob)
	return h, st, bob
}

// drainUpdates 取走下行队列里已入队的 presence.update（同步路径调用后立即可用）。
func drainUpdates(t *testing.T, c *client) []PresenceUpdateData {
	t.Helper()
	var out []PresenceUpdateData
	for {
		select {
		case raw := <-c.send:
			var env Envelope
			if err := json.Unmarshal(raw, &env); err != nil {
				t.Fatalf("解信封: %v", err)
			}
			if env.Type != TypePresenceUpdate {
				continue
			}
			var d PresenceUpdateData
			if err := env.Decode(&d); err != nil {
				t.Fatalf("解 presence.update: %v", err)
			}
			out = append(out, d)
		default:
			return out
		}
	}
}

// TestPresenceEdgeConcurrentPush 多设备并发首连/并发断开（-race 下多轮）：
// 只有 0→1 的那次推 online，只有 1→0 的那次推 offline；不重复、不遗漏。
// 旧实现把「数连接数」放在 register 之后、推送在锁外，两台设备同时首连会
// 各自看到已有连接，可能一条 online 都不推。
func TestPresenceEdgeConcurrentPush(t *testing.T) {
	h, st, bob := newPresenceHub(t)
	ctx := context.Background()
	const rounds = 50
	for round := 0; round < rounds; round++ {
		const devices = 6
		cs := make([]*client, devices)
		for i := range cs {
			cs[i] = &client{hub: h, userID: "1", dev: fmt.Sprintf("d%d", i)}
		}
		var wg sync.WaitGroup
		for _, c := range cs {
			wg.Add(1)
			go func(c *client) {
				defer wg.Done()
				if _, became := h.register(c); became {
					h.syncPresence(c.userID)
				}
			}(c)
		}
		wg.Wait()
		if ups := drainUpdates(t, bob); len(ups) != 1 || !ups[0].Online {
			t.Fatalf("第 %d 轮并发首连：期望恰好 1 条 online=true，实际 %+v", round, ups)
		}
		if _, err := st.Get(ctx, presenceKey("1")); err != nil {
			t.Fatalf("第 %d 轮首连后 presence 键应存在: %v", round, err)
		}

		for _, c := range cs {
			wg.Add(1)
			go func(c *client) {
				defer wg.Done()
				if h.unregister(c) {
					h.syncPresence(c.userID)
				}
			}(c)
		}
		wg.Wait()
		if ups := drainUpdates(t, bob); len(ups) != 1 || ups[0].Online {
			t.Fatalf("第 %d 轮并发断开：期望恰好 1 条 online=false，实际 %+v", round, ups)
		}
		if _, err := st.Get(ctx, presenceKey("1")); err == nil {
			t.Fatalf("第 %d 轮全断后 presence 键应删除", round)
		}
	}
}

// TestPresenceReplaceSameDeviceNoDuplicatePush 同 uid+dev 被顶替（P2）：
// 新连接不产生 0→1 边沿，旧连接 cleanup 不产生 1→0 边沿，好友不应收到重复 online。
func TestPresenceReplaceSameDeviceNoDuplicatePush(t *testing.T) {
	h, _, bob := newPresenceHub(t)

	old := &client{hub: h, userID: "1", dev: "desktop"}
	if _, became := h.register(old); !became {
		t.Fatal("首连应判定为 0→1 边沿")
	}
	h.syncPresence("1")
	if ups := drainUpdates(t, bob); len(ups) != 1 || !ups[0].Online {
		t.Fatalf("首连应推 1 条 online=true，实际 %+v", ups)
	}

	fresh := &client{hub: h, userID: "1", dev: "desktop"}
	if _, became := h.register(fresh); became {
		t.Fatal("同设备顶替不应判定为 0→1 边沿")
	}
	if h.unregister(old) {
		t.Fatal("被顶替的旧连接不应判定为 1→0 边沿")
	}
	if ups := drainUpdates(t, bob); len(ups) != 0 {
		t.Fatalf("顶替不应重复推送 presence.update，实际 %+v", ups)
	}

	if !h.unregister(fresh) {
		t.Fatal("最后一条连接断开应判定为 1→0 边沿")
	}
	h.syncPresence("1")
	if ups := drainUpdates(t, bob); len(ups) != 1 || ups[0].Online {
		t.Fatalf("最终断开应推 1 条 online=false，实际 %+v", ups)
	}
}

// TestPresenceReconnectRace 断线清理与重连交错（-race 多轮）：旧连接的 DEL
// 不能覆盖新连接的 SET——最终 presence 键必须存在。这是一开始复现出的
// 「设备实际在线、好友一直显示离线」的根因路径。
func TestPresenceReconnectRace(t *testing.T) {
	h, st, bob := newPresenceHub(t)
	ctx := context.Background()
	const rounds = 100
	for round := 0; round < rounds; round++ {
		a := &client{hub: h, userID: "1", dev: "desktop"}
		if _, became := h.register(a); !became {
			t.Fatalf("第 %d 轮首连应判定为 0→1 边沿", round)
		}
		h.syncPresence("1")

		b := &client{hub: h, userID: "1", dev: "mobile"}
		var wg sync.WaitGroup
		wg.Add(2)
		go func() {
			defer wg.Done()
			if h.unregister(a) {
				h.syncPresence("1")
			}
		}()
		go func() {
			defer wg.Done()
			if _, became := h.register(b); became {
				h.syncPresence("1")
			}
		}()
		wg.Wait()
		if _, err := st.Get(ctx, presenceKey("1")); err != nil {
			t.Fatalf("第 %d 轮重连后 presence 键丢失（旧连接 DEL 覆盖了新连接）", round)
		}

		// 收尾回离线，进入下一轮。
		if !h.unregister(b) {
			t.Fatalf("第 %d 轮收尾应判定为 1→0 边沿", round)
		}
		h.syncPresence("1")
		if _, err := st.Get(ctx, presenceKey("1")); err == nil {
			t.Fatalf("第 %d 轮收尾后 presence 键应删除", round)
		}
		drainUpdates(t, bob) // 每轮推 0~2 条（边沿交错时会去重），及时清空队列
	}
}

// flakyPresence 包一层 store.Mem：可控 SET 失败次数并记录 SET/DEL 调用次数，
// 复现「首 SET 失败 → 心跳续期成功 → 断线」的时序（复审 4.2）。
type flakyPresence struct {
	*store.Mem
	mu      sync.Mutex
	failSet int
	sets    int
	dels    int
}

func (s *flakyPresence) Set(ctx context.Context, key, value string, ttl time.Duration) error {
	s.mu.Lock()
	s.sets++
	fail := s.failSet > 0
	if fail {
		s.failSet--
	}
	s.mu.Unlock()
	if fail {
		return errors.New("注入的 SET 失败")
	}
	return s.Mem.Set(ctx, key, value, ttl)
}

func (s *flakyPresence) Del(ctx context.Context, keys ...string) error {
	s.mu.Lock()
	s.dels++
	s.mu.Unlock()
	return s.Mem.Del(ctx, keys...)
}

func (s *flakyPresence) counts() (sets, dels int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.sets, s.dels
}

// TestPresenceFirstSetFailureStillDel 复审 4.2：首 SET 失败后心跳续期把键写回，
// 断线必须仍执行 DEL（不能因 published=false 跳过），且好友从未收到 online 时
// 不补推一条 offline。
func TestPresenceFirstSetFailureStillDel(t *testing.T) {
	h, mem, bob := newPresenceHub(t)
	fs := &flakyPresence{Mem: mem, failSet: 1}
	h.presence = fs
	ctx := context.Background()

	alice := &client{hub: h, userID: "1", dev: "desktop"}
	if _, became := h.register(alice); !became {
		t.Fatal("首连应判定为 0→1 边沿")
	}
	h.syncPresence("1") // 首次 SET 失败
	if _, err := mem.Get(ctx, presenceKey("1")); err == nil {
		t.Fatal("SET 失败后键不应存在")
	}
	if ups := drainUpdates(t, bob); len(ups) != 0 {
		t.Fatalf("SET 失败不应推 online，实际 %+v", ups)
	}

	h.renewPresence(alice) // 心跳续期成功，把键写回
	if _, err := mem.Get(ctx, presenceKey("1")); err != nil {
		t.Fatalf("续期后键应存在: %v", err)
	}

	if !h.unregister(alice) {
		t.Fatal("最后一条连接断开应判定为 1→0 边沿")
	}
	h.syncPresence("1")
	if _, err := mem.Get(ctx, presenceKey("1")); err == nil {
		t.Fatal("断线后 presence 键应被 DEL（首 SET 失败不能跳过离线清理）")
	}
	if _, dels := fs.counts(); dels != 1 {
		t.Fatalf("DEL 应被调用 1 次，实际 %d", dels)
	}
	if ups := drainUpdates(t, bob); len(ups) != 0 {
		t.Fatalf("好友未收到 online，断线不应补推 offline，实际 %+v", ups)
	}
}

// TestPresenceRetrySetOnOnlineEdge 首 SET 失败后，下一次在线边沿（断线重连）
// 必须重试 SET，不能被 presencePublished 去重永久挡住；已发布后重复同步也会
// 补写丢失的键，但不重推 online。
func TestPresenceRetrySetOnOnlineEdge(t *testing.T) {
	h, mem, bob := newPresenceHub(t)
	fs := &flakyPresence{Mem: mem, failSet: 1}
	h.presence = fs
	ctx := context.Background()

	alice := &client{hub: h, userID: "1", dev: "desktop"}
	if _, became := h.register(alice); !became {
		t.Fatal("首连应判定为 0→1 边沿")
	}
	h.syncPresence("1") // SET 失败
	if sets, _ := fs.counts(); sets != 1 {
		t.Fatalf("首次 SET 应已尝试，实际 %d 次", sets)
	}

	// 断线：键本来就不存在，离线 DEL 仍是幂等路径。
	if !h.unregister(alice) {
		t.Fatal("断线应判定为 1→0 边沿")
	}
	h.syncPresence("1")

	// 重连：下一次在线边沿必须重试 SET 并成功。
	alice = &client{hub: h, userID: "1", dev: "desktop"}
	if _, became := h.register(alice); !became {
		t.Fatal("重连应判定为 0→1 边沿")
	}
	h.syncPresence("1")
	if _, err := mem.Get(ctx, presenceKey("1")); err != nil {
		t.Fatalf("重连后应重试 SET 并写回键: %v", err)
	}
	if sets, _ := fs.counts(); sets != 2 {
		t.Fatalf("重连应重试 SET（共 2 次），实际 %d 次", sets)
	}
	if ups := drainUpdates(t, bob); len(ups) != 1 || !ups[0].Online {
		t.Fatalf("重连应恰好推 1 条 online=true，实际 %+v", ups)
	}

	// published=true 时再次同步也要补写 KV（键丢失/过期后的自愈），但不重推。
	if err := mem.Del(ctx, presenceKey("1")); err != nil {
		t.Fatalf("删键: %v", err)
	}
	h.syncPresence("1")
	if _, err := mem.Get(ctx, presenceKey("1")); err != nil {
		t.Fatalf("已发布状态下同步仍应重写键: %v", err)
	}
	if ups := drainUpdates(t, bob); len(ups) != 0 {
		t.Fatalf("已推过 online 不应重推，实际 %+v", ups)
	}
}

// TestChannelJoinedOnlineField 成员快照带在线态（#11 验收）：channel.joined
// 的 members 与 self 都从 presence:{uid} 填 online。
func TestChannelJoinedOnlineField(t *testing.T) {
	f := newChatFixture(t).withPresence()
	alice := f.dial("1", "alice")
	bob := f.dial("2", "bob")
	chID := strconv.FormatInt(f.vch.ID, 10)

	alice.send(TypeChannelJoin, ChannelJoinData{ChannelID: chID})
	var joined ChannelJoinedData
	if err := alice.recvUntil(TypeChannelJoined).Decode(&joined); err != nil {
		t.Fatalf("decode channel.joined: %v", err)
	}
	if len(joined.Members) != 1 || joined.Members[0].UserID != "1" || !joined.Members[0].Online {
		t.Fatalf("单人入房快照应含本人且 online=true: %+v", joined)
	}
	if !joined.Self.Online {
		t.Fatalf("self 也应带 online=true: %+v", joined.Self)
	}

	bob.send(TypeChannelJoin, ChannelJoinData{ChannelID: chID})
	if err := alice.recvUntil(TypeChannelJoined).Decode(&joined); err != nil {
		t.Fatalf("decode channel.joined: %v", err)
	}
	if len(joined.Members) != 2 {
		t.Fatalf("两人入房快照应为 2 人，实际 %+v", joined.Members)
	}
	for _, m := range joined.Members {
		if !m.Online {
			t.Fatalf("在线成员 %s 的 online 应为 true: %+v", m.UserID, m)
		}
	}
}
