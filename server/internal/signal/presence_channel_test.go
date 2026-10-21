package signal

import (
	"bytes"
	"context"
	"strconv"
	"testing"
	"time"

	"github.com/OpenALC/LiteTalk/server/internal/store"
)

// #92 好友频道 presence：join/leave/kick/断线 的频道推送、按接收者 ViewChannel
// 过滤、隐私键缺省、去抖与旧连接 cleanup 的映射保护。

// assertNoChanKeys 原始载荷不得出现 channel_id/server_id 键（隐私验收口径：
// 无权限只推 online，键本身不能出现在 JSON 里，而不只是值为空）。
func assertNoChanKeys(t *testing.T, env Envelope) {
	t.Helper()
	for _, k := range []string{"channel_id", "server_id"} {
		if bytes.Contains(env.Data, []byte(k)) {
			t.Fatalf("载荷不得含 %s 键: %s", k, env.Data)
		}
	}
}

// decodeChan 解出频道推送并断言两字段同现同值。
func decodeChan(t *testing.T, env Envelope, wantCh, wantSrv string) PresenceUpdateData {
	t.Helper()
	var d PresenceUpdateData
	if err := env.Decode(&d); err != nil {
		t.Fatalf("decode presence.update: %v", err)
	}
	if d.ChannelID != wantCh || d.ServerID != wantSrv {
		t.Fatalf("频道字段 = (%q,%q)，want (%q,%q)；载荷 %s", d.ChannelID, d.ServerID, wantCh, wantSrv, env.Data)
	}
	if !d.Online {
		t.Fatalf("频道推送应 online=true: %s", env.Data)
	}
	return d
}

// vchIDs 语音房的 (channelID, serverID) 字符串形态。
func vchIDs(f *chatFixture) (string, string) {
	return strconv.FormatInt(f.vch.ID, 10), strconv.FormatInt(f.vch.ServerID, 10)
}

// TestPresenceChannelPushOnJoin 进房后好友收到带 channel_id/server_id 的推送；
// pending 关系不收。
func TestPresenceChannelPushOnJoin(t *testing.T) {
	f := newChatFixture(t).withPresence()
	makeFriends(t, f, 1, 2, store.FriendAccepted)
	makeFriends(t, f, 1, 9, store.FriendPending)
	chID, srvID := vchIDs(f)

	bob := newPump(t, f.dial("2", "bob"))
	mallory := newPump(t, f.dial("9", "mallory"))
	bob.until(TypeHello)
	mallory.until(TypeHello)

	alice := f.dial("1", "alice")
	assertNoChanKeys(t, bob.until(TypePresenceUpdate)) // 上线推送（连接边沿）无频道字段

	alice.send(TypeChannelJoin, ChannelJoinData{ChannelID: chID})
	alice.recvUntil(TypeChannelJoined)

	d := decodeChan(t, bob.until(TypePresenceUpdate), chID, srvID)
	if d.UserID != "1" {
		t.Fatalf("user_id = %q, want 1", d.UserID)
	}
	// pending 关系的 mallory 只收到上线推送，进房不推。
	if got, ok := mallory.within(300 * time.Millisecond); ok {
		t.Fatalf("pending 关系不应收到进房推送，实际 %+v", got)
	}
	// VoiceChannelOf 与推送同源。
	if gotCh, gotSrv, ok := f.hub.VoiceChannelOf("1"); !ok || gotCh != chID || gotSrv != srvID {
		t.Fatalf("VoiceChannelOf = (%q,%q,%v), want (%q,%q,true)", gotCh, gotSrv, ok, chID, srvID)
	}
}

// TestPresenceChannelViewFiltered 无 ViewChannel 权限的接收者只推 online-only，
// 原始 JSON 不得出现 channel_id/server_id 键；有权限的接收者收到完整字段。
// 权限门槛只看 ViewChannel，不要求 ConnectVoice。
func TestPresenceChannelViewFiltered(t *testing.T) {
	f := newChatFixture(t).withPresence()
	makeFriends(t, f, 1, 2, store.FriendAccepted) // bob：将被拒 ViewChannel
	makeFriends(t, f, 1, 9, store.FriendAccepted) // mallory：默认成员有 View
	chID, srvID := vchIDs(f)
	denyOnChannel(t, f, f.vch.ID, 2, store.ViewChannel)

	bob := newPump(t, f.dial("2", "bob"))
	mallory := newPump(t, f.dial("9", "mallory"))
	bob.until(TypeHello)
	mallory.until(TypeHello)

	alice := f.dial("1", "alice")
	assertNoChanKeys(t, bob.until(TypePresenceUpdate)) // 上线推送
	mallory.until(TypePresenceUpdate)

	alice.send(TypeChannelJoin, ChannelJoinData{ChannelID: chID})
	alice.recvUntil(TypeChannelJoined)

	// mallory 有 View →完整字段。
	decodeChan(t, mallory.until(TypePresenceUpdate), chID, srvID)
	// bob 无 View → online-only，且键不得出现。
	bEnv := bob.until(TypePresenceUpdate)
	assertNoChanKeys(t, bEnv)
	var bd PresenceUpdateData
	if err := bEnv.Decode(&bd); err != nil || !bd.Online || bd.UserID != "1" {
		t.Fatalf("无权限应收到 online-only: %+v err=%v", bd, err)
	}
}

// TestPresenceChannelLeaveOnlineOnly 离房推 online-only（无频道键）。
func TestPresenceChannelLeaveOnlineOnly(t *testing.T) {
	f := newChatFixture(t).withPresence()
	makeFriends(t, f, 1, 2, store.FriendAccepted)
	chID, _ := vchIDs(f)

	bob := newPump(t, f.dial("2", "bob"))
	bob.until(TypeHello)
	alice := f.dial("1", "alice")
	bob.until(TypePresenceUpdate) // 上线推送

	alice.send(TypeChannelJoin, ChannelJoinData{ChannelID: chID})
	alice.recvUntil(TypeChannelJoined)
	bob.until(TypePresenceUpdate) // 进房推送

	alice.send(TypeChannelLeave, ChannelLeaveData{})
	// 离房推送 online-only；本端退房确认不依赖信令回包，以推送为准。
	env := bob.until(TypePresenceUpdate)
	assertNoChanKeys(t, env)
	var d PresenceUpdateData
	if err := env.Decode(&d); err != nil || !d.Online {
		t.Fatalf("离房应推 online=true 且无频道: %+v err=%v", d, err)
	}
}

// TestPresenceChannelLastConnOfflineOnly 最后连接断开只推 offline，不推频道。
func TestPresenceChannelLastConnOfflineOnly(t *testing.T) {
	f := newChatFixture(t).withPresence()
	makeFriends(t, f, 1, 2, store.FriendAccepted)
	chID, _ := vchIDs(f)

	bob := newPump(t, f.dial("2", "bob"))
	bob.until(TypeHello)
	alice := f.dial("1", "alice")
	bob.until(TypePresenceUpdate)

	alice.send(TypeChannelJoin, ChannelJoinData{ChannelID: chID})
	alice.recvUntil(TypeChannelJoined)
	bob.until(TypePresenceUpdate)

	_ = alice.conn.CloseNow()
	env := bob.until(TypePresenceUpdate)
	assertNoChanKeys(t, env)
	var d PresenceUpdateData
	if err := env.Decode(&d); err != nil || d.Online {
		t.Fatalf("断开应推 online=false: %+v err=%v", d, err)
	}
	if got, ok := bob.within(300 * time.Millisecond); ok {
		t.Fatalf("离线后不应再有推送，实际 %+v", got)
	}
}

// TestPresenceChannelOldCleanupKeepsMapping 旧连接迟到的 cleanup 不得删掉
// 新连接写入的 userRooms 映射（多设备/顶替竞态，#92）。
func TestPresenceChannelOldCleanupKeepsMapping(t *testing.T) {
	f := newChatFixture(t).withPresence()
	chID, srvID := vchIDs(f)
	vchID := chID

	newConn := func(sid string) *client {
		return &client{hub: f.hub, userID: "1", username: "alice", dev: "desktop",
			sid: sid, send: make(chan []byte, 64)}
	}

	t.Run("同房顶替后旧 cleanup", func(t *testing.T) {
		old := newConn(defaultTestSid("1"))
		f.hub.register(old)
		f.hub.joinRoom(old, vchID, "")

		fresh := newConn("newsid-1")
		prev, _ := f.hub.register(fresh)
		if prev != old {
			t.Fatalf("register 应顶替旧连接")
		}
		f.hub.joinRoom(fresh, vchID, "")
		// 旧连接 cleanup 晚到：成员已是 fresh，不得清映射。
		f.hub.cleanup(old)
		if ch, srv, ok := f.hub.VoiceChannelOf("1"); !ok || ch != vchID || srv != srvID {
			t.Fatalf("旧 cleanup 不得清掉新连接映射: (%q,%q,%v)", ch, srv, ok)
		}
	})

	t.Run("换房后旧 cleanup", func(t *testing.T) {
		f2 := newChatFixture(t).withPresence()
		aID, aSrv := vchIDs(f2)
		// 第二间房：同 store 再建一个语音频道。
		vch2 := &store.Channel{ServerID: f2.vch.ServerID, Name: "lobby2", Type: store.ChannelVoice}
		if err := f2.st.CreateChannel(context.Background(), vch2); err != nil {
			t.Fatalf("CreateChannel: %v", err)
		}
		bID := strconv.FormatInt(vch2.ID, 10)

		old := &client{hub: f2.hub, userID: "1", username: "alice", dev: "desktop",
			sid: defaultTestSid("1"), send: make(chan []byte, 64)}
		f2.hub.register(old)
		f2.hub.joinRoom(old, aID, "")

		fresh := &client{hub: f2.hub, userID: "1", username: "alice", dev: "desktop",
			sid: "newsid-2", send: make(chan []byte, 64)}
		f2.hub.register(fresh)
		f2.hub.joinRoom(fresh, bID, "")

		f2.hub.cleanup(old)
		if ch, srv, ok := f2.hub.VoiceChannelOf("1"); !ok || ch != bID || srv != aSrv {
			t.Fatalf("旧 cleanup 不得把映射清成空/回退: (%q,%q,%v) want (%q,%q,true)", ch, srv, ok, bID, aSrv)
		}
	})
}

// TestPresenceChannelKickChannel KickChannel 摘除后推 online-only。
func TestPresenceChannelKickChannel(t *testing.T) {
	f := newChatFixture(t).withPresence()
	makeFriends(t, f, 1, 2, store.FriendAccepted)
	chID, _ := vchIDs(f)

	bob := newPump(t, f.dial("2", "bob"))
	bob.until(TypeHello)
	alice := f.dial("1", "alice")
	bob.until(TypePresenceUpdate)

	alice.send(TypeChannelJoin, ChannelJoinData{ChannelID: chID})
	alice.recvUntil(TypeChannelJoined)
	bob.until(TypePresenceUpdate)

	f.hub.KickChannel(chID, "deleted")
	env := bob.until(TypePresenceUpdate)
	assertNoChanKeys(t, env)
	var d PresenceUpdateData
	if err := env.Decode(&d); err != nil || !d.Online {
		t.Fatalf("KickChannel 后应推 online-only: %+v err=%v", d, err)
	}
	if _, _, ok := f.hub.VoiceChannelOf("1"); ok {
		t.Fatal("KickChannel 后 VoiceChannelOf 应为不在房")
	}
}

// TestPresenceChannelAuthzFailureSkips 权限查询故障的接收者被跳过（不推），
// 不把故障当有权/无权。
func TestPresenceChannelAuthzFailureSkips(t *testing.T) {
	f := newChatFixture(t).withPresence()
	makeFriends(t, f, 1, 2, store.FriendAccepted)
	makeFriends(t, f, 1, 9, store.FriendAccepted)
	chID, srvID := vchIDs(f)
	f.hub.authz = &stubAuthz{fail: map[int64]bool{2: true}} // bob 查询故障

	bob := newPump(t, f.dial("2", "bob"))
	mallory := newPump(t, f.dial("9", "mallory"))
	bob.until(TypeHello)
	mallory.until(TypeHello)

	alice := f.dial("1", "alice")
	bob.until(TypePresenceUpdate) // 上线推送照常（不走 authz）
	mallory.until(TypePresenceUpdate)

	alice.send(TypeChannelJoin, ChannelJoinData{ChannelID: chID})
	alice.recvUntil(TypeChannelJoined)

	// mallory 正常收频道推送；bob 故障被跳过——一条都不推。
	decodeChan(t, mallory.until(TypePresenceUpdate), chID, srvID)
	if got, ok := bob.within(300 * time.Millisecond); ok {
		t.Fatalf("authz 故障应跳过该好友，实际收到 %+v", got)
	}
}

// TestPresenceChannelReconnectRepush 离线清缓存后，重连再进同一频道必须重推。
func TestPresenceChannelReconnectRepush(t *testing.T) {
	f := newChatFixture(t).withPresence()
	makeFriends(t, f, 1, 2, store.FriendAccepted)
	chID, srvID := vchIDs(f)

	bob := newPump(t, f.dial("2", "bob"))
	bob.until(TypeHello)
	alice := f.dial("1", "alice")
	bob.until(TypePresenceUpdate)
	alice.send(TypeChannelJoin, ChannelJoinData{ChannelID: chID})
	alice.recvUntil(TypeChannelJoined)
	decodeChan(t, bob.until(TypePresenceUpdate), chID, srvID)

	// 断开（离线边沿清缓存）→ 重连 → 再进同一房：必须再推一次。
	_ = alice.conn.CloseNow()
	env := bob.until(TypePresenceUpdate)
	var d PresenceUpdateData
	if err := env.Decode(&d); err != nil || d.Online {
		t.Fatalf("断开应推 offline: %+v err=%v", d, err)
	}

	alice2 := f.dial("1", "alice-rejoin")
	bob.until(TypePresenceUpdate) // 重连上线推送
	alice2.send(TypeChannelJoin, ChannelJoinData{ChannelID: chID})
	alice2.recvUntil(TypeChannelJoined)
	decodeChan(t, bob.until(TypePresenceUpdate), chID, srvID)
}

// TestPresenceChannelDevRoom dev 非数字房间（serverID==0）不推频道字段。
func TestPresenceChannelDevRoom(t *testing.T) {
	f := newChatFixture(t).withPresence()
	makeFriends(t, f, 1, 2, store.FriendAccepted)
	f.hub.dev = true

	bob := newPump(t, f.dial("2", "bob"))
	bob.until(TypeHello)
	alice := f.dial("1", "alice")
	bob.until(TypePresenceUpdate)

	alice.send(TypeChannelJoin, ChannelJoinData{ChannelID: "dev-lobby"})
	alice.recvUntil(TypeChannelJoined)

	env := bob.until(TypePresenceUpdate)
	assertNoChanKeys(t, env)
	var d PresenceUpdateData
	if err := env.Decode(&d); err != nil || !d.Online {
		t.Fatalf("dev 房间应推 online-only: %+v err=%v", d, err)
	}
	if ch, _, ok := f.hub.VoiceChannelOf("1"); !ok || ch != "dev-lobby" {
		t.Fatalf("VoiceChannelOf 应仍反查到 dev 房: (%q,%v)", ch, ok)
	}
}

// TestPresenceChannelUnfriendedStopsPushing 删除好友后不再推送。
func TestPresenceChannelUnfriendedStopsPushing(t *testing.T) {
	f := newChatFixture(t).withPresence()
	makeFriends(t, f, 1, 2, store.FriendAccepted)
	chID, _ := vchIDs(f)

	bob := newPump(t, f.dial("2", "bob"))
	bob.until(TypeHello)
	alice := f.dial("1", "alice")
	bob.until(TypePresenceUpdate) // 上线推送（还是好友）

	// 删好友后进房：不再推。
	fs, err := f.st.FriendshipsOfUser(context.Background(), 1)
	if err != nil || len(fs) == 0 {
		t.Fatalf("FriendshipsOfUser: %v %v", fs, err)
	}
	if err := f.st.DeleteFriendship(context.Background(), fs[0].ID); err != nil {
		t.Fatalf("DeleteFriendship: %v", err)
	}
	alice.send(TypeChannelJoin, ChannelJoinData{ChannelID: chID})
	alice.recvUntil(TypeChannelJoined)
	if got, ok := bob.within(300 * time.Millisecond); ok {
		t.Fatalf("删除好友后不应再推，实际 %+v", got)
	}
}

// TestPresenceChannelDebounce 状态未变不重推：同房换设备（voice_taken 顶替）后
// 频道状态相同，好友不收到第二条频道推送。
func TestPresenceChannelDebounce(t *testing.T) {
	f := newChatFixture(t).withPresence()
	makeFriends(t, f, 1, 2, store.FriendAccepted)
	chID, _ := vchIDs(f)

	bob := newPump(t, f.dial("2", "bob"))
	bob.until(TypeHello)
	desktop := f.dial("1", "alice-desktop")
	bob.until(TypePresenceUpdate) // 上线推送

	desktop.send(TypeChannelJoin, ChannelJoinData{ChannelID: chID})
	desktop.recvUntil(TypeChannelJoined)
	bob.until(TypePresenceUpdate) // 首次频道推送

	// 同 uid 另一设备进同一房（顶替）：状态未变，不重推。
	mobile := f.dial("1:mobile:"+defaultTestSid("1"), "alice-mobile")
	mobile.send(TypeChannelJoin, ChannelJoinData{ChannelID: chID})
	mobile.recvUntil(TypeChannelJoined)
	if got, ok := bob.within(300 * time.Millisecond); ok {
		t.Fatalf("状态未变不应重推，实际 %+v", got)
	}
}
