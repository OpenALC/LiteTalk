package signal

import (
	"context"
	"encoding/json"
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
