package signal

import (
	"context"
	"strconv"
	"testing"
)

// #45：悄悄话信令校验、MemberInfo.priority 与 voice.priority 的收件人范围。

// joinAndCapture 进房并返回 channel.joined 载荷（成员快照断言用）。
func joinAndCapture(t *testing.T, c *wsClient, chID string) ChannelJoinedData {
	t.Helper()
	c.send(TypeChannelJoin, ChannelJoinData{ChannelID: chID})
	env := c.recvUntil(TypeChannelJoined)
	var d ChannelJoinedData
	if err := env.Decode(&d); err != nil {
		t.Fatalf("decode channel.joined: %v", err)
	}
	return d
}

// negotiate 发 client.hello 并等到 server.hello（返回协商版本）。
func negotiate(t *testing.T, c *wsClient, max int64) int64 {
	t.Helper()
	c.send(TypeClientHello, ClientHelloData{Min: 1, Max: max})
	env := c.recvUntil(TypeServerHello)
	var d ServerHelloData
	if err := env.Decode(&d); err != nil {
		t.Fatalf("decode server.hello: %v", err)
	}
	return d.Version
}

// makeAdmin 把 uid 设为管理员角色（builtin admin），用于优先发言判定。
func makeAdmin(t *testing.T, f *chatFixture, uid int64) {
	t.Helper()
	roles, err := f.st.ListRoles(context.Background(), f.vch.ServerID)
	if err != nil {
		t.Fatalf("ListRoles: %v", err)
	}
	var adminRole int64
	for _, r := range roles {
		if r.BuiltinKey == "admin" {
			adminRole = r.ID
		}
	}
	if adminRole == 0 {
		t.Fatal("缺少 admin 内置角色")
	}
	if err := f.st.SetMemberRoles(context.Background(), f.vch.ServerID, uid, []int64{adminRole}, 1); err != nil {
		t.Fatalf("SetMemberRoles: %v", err)
	}
}

// TestMemberInfoPriority 入房快照按 effective 权限填 priority：
// owner/admin 为 true，普通成员 false（含 Self 与 members 两个面）。
func TestMemberInfoPriority(t *testing.T) {
	f := newChatFixture(t)
	makeAdmin(t, f, 9) // mallory → admin
	vchID := strconv.FormatInt(f.vch.ID, 10)

	a := f.dial("1", "alice")
	aj := joinAndCapture(t, a, vchID)
	if !aj.Self.Priority {
		t.Fatalf("owner Self.priority 应为 true: %+v", aj.Self)
	}
	if len(aj.Members) != 1 || !aj.Members[0].Priority {
		t.Fatalf("owner 的成员快照 priority 不符: %+v", aj.Members)
	}

	b := f.dial("2", "bob")
	bj := joinAndCapture(t, b, vchID)
	if bj.Self.Priority {
		t.Fatalf("普通成员 Self.priority 应为 false: %+v", bj.Self)
	}
	got := map[string]bool{}
	for _, m := range bj.Members {
		got[m.UserID] = m.Priority
	}
	if !got["1"] || got["2"] {
		t.Fatalf("成员 priority 不符（owner 应 true、普通成员 false）: %+v", got)
	}

	m := f.dial("9", "mallory")
	mj := joinAndCapture(t, m, vchID)
	if !mj.Self.Priority {
		t.Fatalf("admin Self.priority 应为 true: %+v", mj.Self)
	}
	for _, mm := range mj.Members {
		if mm.UserID == "2" && mm.Priority {
			t.Fatalf("普通成员 bob 不应为 priority: %+v", mj.Members)
		}
	}
}

// TestVoiceWhisperValidation 校验顺序与错误码：缺 targets / 超限 / 不在房；
// 合法路径（去重、排除自己、过滤非成员）不产生 error。
func TestVoiceWhisperValidation(t *testing.T) {
	f := newChatFixture(t)
	vchID := strconv.FormatInt(f.vch.ID, 10)
	a := f.dial("1", "alice")

	// 不在房：not_in_channel（先于 targets 校验？targets 合法，故走到房间检查）。
	a.send(TypeVoiceWhisper, VoiceWhisperData{Targets: []string{"2"}})
	wantError(t, a, ErrCodeNotInChannel)

	// 缺 targets（JSON 里没有该字段 → nil）→ bad_message。
	a.send(TypeVoiceWhisper, map[string]any{})
	wantError(t, a, ErrCodeBadMessage)

	// 超限 → bad_message。
	over := make([]string, maxWhisperTargets+1)
	for i := range over {
		over[i] = strconv.Itoa(i + 100)
	}
	a.send(TypeVoiceWhisper, VoiceWhisperData{Targets: over})
	wantError(t, a, ErrCodeBadMessage)

	// 进房后合法下发：重复、自己、非成员都静默过滤，不应回 error。
	joinAndCapture(t, a, vchID)
	b := f.dial("2", "bob")
	joinAndCapture(t, b, vchID)
	a.send(TypeVoiceWhisper, VoiceWhisperData{
		Targets: []string{"2", "2", "1", "999", ""},
	})
	// 用一条必然被拒的请求把错误队列顶出来：若 whisper 回了 error，先收到的是它。
	mute := true
	a.send(TypeVoiceRequestState, VoiceRequestStateData{ChannelID: vchID, TargetID: "1", Mute: &mute})
	env := a.recvUntil(TypeError)
	var e ErrorData
	if err := env.Decode(&e); err != nil {
		t.Fatalf("decode error: %v", err)
	}
	if e.Message != "不能对自己发起请求" {
		t.Fatalf("合法 voice.whisper 不应报错，收到 %+v", e)
	}

	// 空数组 = 取消，同样不报错。
	a.send(TypeVoiceWhisper, VoiceWhisperData{Targets: []string{}})
	a.send(TypeVoiceRequestState, VoiceRequestStateData{ChannelID: vchID, TargetID: "1", Mute: &mute})
	if env := a.recvUntil(TypeError); env.Decode(&e) != nil || e.Message != "不能对自己发起请求" {
		t.Fatalf("空 targets 取消不应报错，收到 %+v", e)
	}
}

// recvUntilNo 读到 want 为止，途中出现 forbidden 即失败（用于断言「不应收到」）。
func recvUntilNo(t *testing.T, c *wsClient, want, forbidden string) Envelope {
	t.Helper()
	for {
		var env Envelope
		if len(c.pending) > 0 {
			env, c.pending = c.pending[0], c.pending[1:]
		} else {
			env = c.readOne()
		}
		if env.Type == forbidden {
			t.Fatalf("不应收到 %s（user=%s）", forbidden, c.user)
		}
		if env.Type == want {
			return env
		}
	}
}

// TestBroadcastVoicePriorityRecipients 只发给房间成员的全部在线连接（含同 uid
// 其他设备），协议 <3 的连接不下发；payload 原样。
func TestBroadcastVoicePriorityRecipients(t *testing.T) {
	f := newChatFixture(t)
	vchID := strconv.FormatInt(f.vch.ID, 10)

	a := f.dial("1", "alice")
	negotiate(t, a, 3)
	joinAndCapture(t, a, vchID)
	b := f.dial("2", "bob")
	negotiate(t, b, 3)
	joinAndCapture(t, b, vchID)
	// bob 的第二台设备：在同一 uid 下在线，但不在房间内——仍应收到。
	phone := f.dial("2:phone", "bob-phone")
	negotiate(t, phone, 3)
	// mallory 协商 v2：不认 since 3 的消息，不得收到。
	m := f.dial("9", "mallory")
	if v := negotiate(t, m, 2); v != 2 {
		t.Fatalf("mallory 协商版本 = %d，期望 2", v)
	}
	joinAndCapture(t, m, vchID)

	f.hub.BroadcastVoicePriority(vchID, "2", true)

	want := VoicePriorityData{ChannelID: vchID, UserID: "2", Priority: true}
	for _, c := range []*wsClient{a, b, phone} {
		var d VoicePriorityData
		if err := c.recvUntil(TypeVoicePriority).Decode(&d); err != nil {
			t.Fatalf("decode voice.priority（%s）: %v", c.user, err)
		}
		if d != want {
			t.Fatalf("voice.priority 载荷（%s）= %+v，期望 %+v", c.user, d, want)
		}
	}

	// mallory 不应收到 priority：让 alice 离房，mallory 收到 peer_left 之前
	// 若混入 voice.priority 即失败。
	a.send(TypeChannelLeave, ChannelLeaveData{})
	recvUntilNo(t, m, TypeChannelPeerLeft, TypeVoicePriority)
}

// TestBroadcastVoicePriorityNoRoom 房间不存在时静默返回（REST 调用方可直接调）。
func TestBroadcastVoicePriorityNoRoom(t *testing.T) {
	f := newChatFixture(t)
	f.hub.BroadcastVoicePriority("424242", "2", true) // 不 panic 即可
}
