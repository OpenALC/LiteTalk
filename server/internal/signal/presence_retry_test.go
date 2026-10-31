package signal

import (
	"testing"
	"time"

	"github.com/OpenALC/LiteTalk/server/internal/store"
)

// #102 presence 去抖缓存：权限查询故障时被跳过的查看者不得被当成「已发送」，
// 下一次心跳/状态变化必须补发。

// TestPresenceChannelRetryAfterAuthzRecovers 首推时 bob 的权限查询故障被跳过，
// 恢复后一次「状态未变」的重算必须把频道推给他，且不重复打扰已经收到过的
// mallory。
func TestPresenceChannelRetryAfterAuthzRecovers(t *testing.T) {
	f := newChatFixture(t).withPresence()
	makeFriends(t, f, 1, 2, store.FriendAccepted)
	makeFriends(t, f, 1, 9, store.FriendAccepted)
	chID, srvID := vchIDs(f)
	stub := &stubAuthz{fail: map[int64]bool{2: true}} // bob 查询故障
	f.hub.authz = stub

	bob := newPump(t, f.dial("2", "bob"))
	mallory := newPump(t, f.dial("9", "mallory"))
	bob.until(TypeHello)
	mallory.until(TypeHello)

	alice := f.dial("1", "alice")
	bob.until(TypePresenceUpdate) // 上线推送（不走 authz）
	mallory.until(TypePresenceUpdate)

	alice.send(TypeChannelJoin, ChannelJoinData{ChannelID: chID})
	alice.recvUntil(TypeChannelJoined)

	// mallory 收到频道推送；bob 故障被跳过。
	decodeChan(t, mallory.until(TypePresenceUpdate), chID, srvID)
	if got, ok := bob.within(300 * time.Millisecond); ok {
		t.Fatalf("authz 故障应跳过该好友，实际收到 %+v", got)
	}

	// 故障恢复。直接调一次（等价于心跳触发的重算）：状态未变，但待重试集合
	// 非空，必须绕过去抖把频道补发给 bob。
	stub.mu.Lock()
	stub.fail = map[int64]bool{}
	stub.mu.Unlock()
	f.hub.syncPresenceChannel("1")

	decodeChan(t, bob.until(TypePresenceUpdate), chID, srvID)
	if got, ok := mallory.within(300 * time.Millisecond); ok {
		t.Fatalf("已送达的查看者不应重复收到推送，实际 %+v", got)
	}

	// 待重试集合已清空：再重算一次什么都不推（回到正常去抖）。
	f.hub.presenceMu.Lock()
	_, pending := f.hub.presenceChanRetry["1"]
	f.hub.presenceMu.Unlock()
	if pending {
		t.Fatal("补发完成后不应残留待重试标记")
	}
	f.hub.syncPresenceChannel("1")
	if got, ok := bob.within(300 * time.Millisecond); ok {
		t.Fatalf("补发完成后状态未变不应再推，实际 %+v", got)
	}
}

// TestPresenceChannelRetryOnHeartbeat 待重试标记由心跳驱动：心跳续期时发现标记
// 就重算，不需要等下一次状态变化。
func TestPresenceChannelRetryOnHeartbeat(t *testing.T) {
	f := newChatFixture(t).withPresence()
	makeFriends(t, f, 1, 2, store.FriendAccepted)
	chID, srvID := vchIDs(f)
	stub := &stubAuthz{fail: map[int64]bool{2: true}}
	f.hub.authz = stub
	// 心跳压到 50ms：ticker 在连接建立时取 heartbeatEvery，必须在 dial 之前设。
	f.hub.heartbeatEvery = 50 * time.Millisecond

	bob := newPump(t, f.dial("2", "bob"))
	bob.until(TypeHello)
	// alice 的手写在读循环里由 pump 接管：心跳 ping/pong 靠读连接处理，
	// 不读连接 renewal 永远不会被调到。
	aliceWS := f.dial("1", "alice")
	bob.until(TypePresenceUpdate)
	aliceWS.send(TypeChannelJoin, ChannelJoinData{ChannelID: chID})
	aliceWS.recvUntil(TypeChannelJoined)
	_ = newPump(t, aliceWS)
	if got, ok := bob.within(300 * time.Millisecond); ok {
		t.Fatalf("authz 故障应跳过该好友，实际收到 %+v", got)
	}

	// 恢复权限：下一拍心跳就必须补发。
	stub.mu.Lock()
	stub.fail = map[int64]bool{}
	stub.mu.Unlock()

	deadline := time.After(5 * time.Second)
	for {
		select {
		case env := <-bob.ch:
			if env.Type == TypePresenceUpdate {
				decodeChan(t, env, chID, srvID)
				return
			}
		case <-deadline:
			t.Fatal("心跳未触发待重试的频道补发")
		}
	}
}

// TestPresenceChannelOfflineClearsRetry 离线边沿同时清掉待重试集合，
// 重连后不会残留一个永远补不出去的标记。
func TestPresenceChannelOfflineClearsRetry(t *testing.T) {
	f := newChatFixture(t).withPresence()
	makeFriends(t, f, 1, 2, store.FriendAccepted)
	chID, _ := vchIDs(f)
	stub := &stubAuthz{fail: map[int64]bool{2: true}}
	f.hub.authz = stub

	bob := newPump(t, f.dial("2", "bob"))
	bob.until(TypeHello)
	alice := f.dial("1", "alice")
	bob.until(TypePresenceUpdate)
	alice.send(TypeChannelJoin, ChannelJoinData{ChannelID: chID})
	alice.recvUntil(TypeChannelJoined)
	if got, ok := bob.within(300 * time.Millisecond); ok {
		t.Fatalf("authz 故障应跳过该好友，实际收到 %+v", got)
	}

	f.hub.presenceMu.Lock()
	_, pending := f.hub.presenceChanRetry["1"]
	f.hub.presenceMu.Unlock()
	if !pending {
		t.Fatal("故障跳过之后应有待重试标记")
	}

	// 断开 alice：cleanup 触发离线边沿，标记应被清掉。
	_ = alice.conn.CloseNow()
	waitOffline(t, f, "1")
	f.hub.presenceMu.Lock()
	_, pending = f.hub.presenceChanRetry["1"]
	f.hub.presenceMu.Unlock()
	if pending {
		t.Fatal("离线边沿应清掉待重试标记")
	}
}
