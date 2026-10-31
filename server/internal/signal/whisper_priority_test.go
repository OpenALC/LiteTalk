package signal

import (
	"context"
	"strconv"
	"testing"
	"time"
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

// waitHubWhisper 等 hub 侧悄悄话快照达到期望状态（同包测试读 h.mu）。
// wantActive=false 表示条目应被删除（取消/离房）。
func waitHubWhisper(t *testing.T, h *Hub, roomID, uid string, wantActive bool, wantTarget string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		h.mu.Lock()
		var ws whisperState
		ok := false
		if room := h.rooms[roomID]; room != nil {
			ws, ok = room.whisper[uid]
		}
		h.mu.Unlock()
		if !wantActive {
			if !ok {
				return
			}
		} else if ok && ws.active && (wantTarget == "" || ws.targets[wantTarget]) {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("hub 悄悄话快照未达到期望（room=%s uid=%s active=%v target=%s）", roomID, uid, wantActive, wantTarget)
}

// TestWhisperSpeakingScopedV2 悄悄话期间 voice.speaking 只发给目标与说话者本人；
// 目标含同 uid 的其他在线设备；非目标不收到。取消后恢复全房广播。
func TestWhisperSpeakingScopedV2(t *testing.T) {
	f := newChatFixture(t)
	vchID := strconv.FormatInt(f.vch.ID, 10)

	a := f.dial("1", "alice")
	if v := negotiate(t, a, 3); v != 3 {
		t.Fatalf("alice 协商版本 = %d，期望 3", v)
	}
	b := f.dial("2", "bob")
	negotiate(t, b, 3)
	m := f.dial("9", "mallory")
	negotiate(t, m, 3)
	joinAndCapture(t, a, vchID)
	joinAndCapture(t, b, vchID)
	joinAndCapture(t, m, vchID)
	// bob 的第二台设备：同一 uid 在线但不在房间内，仍应收到目标相关的说话指示。
	phone := f.dial("2:phone", "bob-phone")
	negotiate(t, phone, 3)

	a.send(TypeVoiceWhisper, VoiceWhisperData{Targets: []string{"2"}})
	waitHubWhisper(t, f.hub, vchID, "1", true, "2")

	f.hub.broadcastVoiceSpeaking(vchID, "1", true)
	want := VoiceSpeakingData{ChannelID: vchID, UserID: "1", Speaking: true}
	for _, c := range []*wsClient{a, b, phone} {
		var d VoiceSpeakingData
		if err := c.recvUntil(TypeVoiceSpeaking).Decode(&d); err != nil {
			t.Fatalf("decode voice.speaking（%s）: %v", c.user, err)
		}
		if d != want {
			t.Fatalf("voice.speaking（%s）= %+v，期望 %+v", c.user, d, want)
		}
	}

	// 非目标 mallory：marker 之前不得出现 voice.speaking。
	f.hub.sendJSON(f.hub.roomMember(vchID, "9"), "test.marker", nil)
	recvUntilNo(t, m, "test.marker", TypeVoiceSpeaking)

	// 非目标后来入房：channel.joined 的成员快照也不能带出悄悄话说话者的
	// speaking=true（否则新入房客户端会误 duck）。
	m.send(TypeChannelLeave, ChannelLeaveData{})
	mj := joinAndCapture(t, m, vchID)
	speakerSpeaking := false
	for _, mm := range mj.Members {
		if mm.UserID == "1" {
			speakerSpeaking = mm.Speaking
		}
	}
	if speakerSpeaking {
		t.Fatalf("非目标入房快照不应看到悄悄话说话者 speaking=true: %+v", mj.Members)
	}

	// 取消悄悄话后 speaking 恢复全房广播：说话中途取消会先给非目标补一条
	// true 纠正（范围切换），随后全房广播 false。
	a.send(TypeVoiceWhisper, VoiceWhisperData{Targets: []string{}})
	waitHubWhisper(t, f.hub, vchID, "1", false, "")
	var corr VoiceSpeakingData
	if err := m.recvUntil(TypeVoiceSpeaking).Decode(&corr); err != nil {
		t.Fatalf("decode 纠正事件（取消后）: %v", err)
	}
	if corr.UserID != "1" || !corr.Speaking {
		t.Fatalf("取消悄悄话后非目标应收 speaking=true 纠正，实际 %+v", corr)
	}
	f.hub.broadcastVoiceSpeaking(vchID, "1", false)
	var off VoiceSpeakingData
	if err := m.recvUntil(TypeVoiceSpeaking).Decode(&off); err != nil {
		t.Fatalf("decode voice.speaking（取消后）: %v", err)
	}
	if off.UserID != "1" || off.Speaking {
		t.Fatalf("取消后非目标应收到 speaking=false，实际 %+v", off)
	}
}

// TestWhisperSpeakingScopedLegacy v1 legacy 路径同样收窄：目标收到
// voice.state{speaking}，非目标收不到；取消后恢复。
func TestWhisperSpeakingScopedLegacy(t *testing.T) {
	f := newChatFixture(t)
	vchID := strconv.FormatInt(f.vch.ID, 10)

	a := f.dial("1", "alice")
	negotiate(t, a, 3)
	b := f.dial("2", "bob") // 不协商：按 v1 处理
	m := f.dial("9", "mallory")
	joinAndCapture(t, a, vchID)
	joinAndCapture(t, b, vchID)
	joinAndCapture(t, m, vchID)

	a.send(TypeVoiceWhisper, VoiceWhisperData{Targets: []string{"2"}})
	waitHubWhisper(t, f.hub, vchID, "1", true, "2")

	f.hub.broadcastVoiceSpeaking(vchID, "1", true)
	var d VoiceStateData
	if err := b.recvUntil(TypeVoiceState).Decode(&d); err != nil {
		t.Fatalf("decode voice.state（目标）: %v", err)
	}
	if d.UserID != "1" || d.Speaking == nil || !*d.Speaking {
		t.Fatalf("目标 v1 应收 speaking=true，实际 %+v", d)
	}

	// 非目标 mallory：marker 之前不得出现 voice.state。
	f.hub.sendJSON(f.hub.roomMember(vchID, "9"), "test.marker", nil)
	recvUntilNo(t, m, "test.marker", TypeVoiceState)

	// 取消后非目标恢复收 voice.state：先收到 true 纠正（范围切换），再收全房 false。
	a.send(TypeVoiceWhisper, VoiceWhisperData{Targets: []string{}})
	waitHubWhisper(t, f.hub, vchID, "1", false, "")
	var corrOn VoiceStateData
	if err := m.recvUntil(TypeVoiceState).Decode(&corrOn); err != nil {
		t.Fatalf("decode v1 纠正事件（取消后）: %v", err)
	}
	if corrOn.UserID != "1" || corrOn.Speaking == nil || !*corrOn.Speaking {
		t.Fatalf("取消悄悄话后 v1 非目标应收 speaking=true 纠正，实际 %+v", corrOn)
	}
	f.hub.broadcastVoiceSpeaking(vchID, "1", false)
	var off VoiceStateData
	if err := m.recvUntil(TypeVoiceState).Decode(&off); err != nil {
		t.Fatalf("decode voice.state（取消后）: %v", err)
	}
	if off.UserID != "1" || off.Speaking == nil || *off.Speaking {
		t.Fatalf("取消后非目标应收 speaking=false，实际 %+v", off)
	}

	// 说话中途再次开启悄悄话：v1 非目标收到 false 纠正（与 v2 同一套范围切换逻辑）。
	f.hub.broadcastVoiceSpeaking(vchID, "1", true)
	var on VoiceStateData
	if err := m.recvUntil(TypeVoiceState).Decode(&on); err != nil || on.Speaking == nil || !*on.Speaking {
		t.Fatalf("正常 speaking（v1 非目标）异常: %+v %v", on, err)
	}
	a.send(TypeVoiceWhisper, VoiceWhisperData{Targets: []string{"2"}})
	waitHubWhisper(t, f.hub, vchID, "1", true, "2")
	var corr VoiceStateData
	if err := m.recvUntil(TypeVoiceState).Decode(&corr); err != nil {
		t.Fatalf("decode v1 纠正事件: %v", err)
	}
	if corr.UserID != "1" || corr.Speaking == nil || *corr.Speaking {
		t.Fatalf("v1 非目标应收 speaking=false 纠正，实际 %+v", corr)
	}
}

// TestWhisperSpeakingScopeTransition 说话中途开启/关闭悄悄话时补发纠正事件：
// 非目标先收到 true，开启后收到 false；取消后恢复 true。避免保留旧指示并
// 错误触发优先发言 ducking。
func TestWhisperSpeakingScopeTransition(t *testing.T) {
	f := newChatFixture(t)
	vchID := strconv.FormatInt(f.vch.ID, 10)
	a := f.dial("1", "alice")
	b := f.dial("2", "bob")
	m := f.dial("9", "mallory")
	negotiate(t, a, 3)
	negotiate(t, b, 3)
	negotiate(t, m, 3)
	joinAndCapture(t, a, vchID)
	joinAndCapture(t, b, vchID)
	joinAndCapture(t, m, vchID)

	// 正常发言：全房收到 true。
	f.hub.broadcastVoiceSpeaking(vchID, "1", true)
	for _, c := range []*wsClient{a, b, m} {
		var d VoiceSpeakingData
		if err := c.recvUntil(TypeVoiceSpeaking).Decode(&d); err != nil || !d.Speaking {
			t.Fatalf("正常 speaking 广播（%s）异常: %+v %v", c.user, d, err)
		}
	}

	// 说话中途开启悄悄话：非目标 mallory 收到 false 纠正；目标 bob 保持 true，
	// 不应收到纠正事件。
	a.send(TypeVoiceWhisper, VoiceWhisperData{Targets: []string{"2"}})
	waitHubWhisper(t, f.hub, vchID, "1", true, "2")
	var off VoiceSpeakingData
	if err := m.recvUntil(TypeVoiceSpeaking).Decode(&off); err != nil {
		t.Fatalf("decode 纠正事件: %v", err)
	}
	if off.UserID != "1" || off.Speaking {
		t.Fatalf("非目标应收 speaking=false 纠正，实际 %+v", off)
	}
	f.hub.sendJSON(f.hub.roomMember(vchID, "2"), "test.marker", nil)
	recvUntilNo(t, b, "test.marker", TypeVoiceSpeaking)

	// 仍在说话时取消悄悄话：非目标重新收到 true。
	a.send(TypeVoiceWhisper, VoiceWhisperData{Targets: []string{}})
	waitHubWhisper(t, f.hub, vchID, "1", false, "")
	var on VoiceSpeakingData
	if err := m.recvUntil(TypeVoiceSpeaking).Decode(&on); err != nil {
		t.Fatalf("decode 恢复事件: %v", err)
	}
	if on.UserID != "1" || !on.Speaking {
		t.Fatalf("取消悄悄话后非目标应收 speaking=true，实际 %+v", on)
	}
}

// TestJoinedSpeakingScopedToWhisperTargets channel.joined 快照按收件人收窄：
// 目标与本人看到说话者的 speaking，非目标看到 false；无悄悄话时不复制切片。
func TestJoinedSpeakingScopedToWhisperTargets(t *testing.T) {
	mkClient := func(uid string, connID uint64) *client {
		return &client{userID: uid, connID: connID}
	}
	room := &roomState{
		id: "r",
		members: map[string]*client{
			"1": mkClient("1", 1),
			"2": mkClient("2", 2),
			"9": mkClient("9", 3),
		},
		whisper: map[string]whisperState{
			"1": {connID: "1", active: true, targets: map[string]bool{"2": true}},
		},
	}
	h := &Hub{rooms: map[string]*roomState{"r": room}}
	infos := []MemberInfo{
		{UserID: "1", Nickname: "a", Speaking: true},
		{UserID: "2", Nickname: "b"},
		{UserID: "9", Nickname: "c"},
	}
	recipients := []*client{room.members["1"], room.members["2"], room.members["9"]}
	scoped := h.scopeJoinedSpeaking("r", infos, recipients)
	if scoped == nil {
		t.Fatal("有生效悄悄话时应返回按收件人收窄的副本")
	}
	speakerSpeaking := func(recipient string) bool {
		for _, m := range scoped[recipient] {
			if m.UserID == "1" {
				return m.Speaking
			}
		}
		t.Fatalf("收件人 %s 的快照缺说话者", recipient)
		return false
	}
	if !speakerSpeaking("1") {
		t.Fatal("本人应保持 speaking=true")
	}
	if !speakerSpeaking("2") {
		t.Fatal("悄悄话目标应保持 speaking=true")
	}
	if speakerSpeaking("9") {
		t.Fatal("非目标应看到 speaking=false")
	}
	// 原切片不得被改写（其他复用处仍保留真实状态）。
	if !infos[0].Speaking {
		t.Fatal("scopeJoinedSpeaking 不得原地修改原切片")
	}
	// 无生效悄悄话：返回 nil，调用方复用原切片。
	room.whisper = nil
	if got := h.scopeJoinedSpeaking("r", infos, recipients); got != nil {
		t.Fatalf("无悄悄话时应返回 nil，实际 %+v", got)
	}
}

// TestWhisperStateClearedOnLeave 离房/被顶号后 hub 快照必须删除：
// 旧目标不得影响同 uid 的新连接或下次进房。
func TestWhisperStateClearedOnLeave(t *testing.T) {
	f := newChatFixture(t)
	vchID := strconv.FormatInt(f.vch.ID, 10)
	a := f.dial("1", "alice")
	b := f.dial("2", "bob")
	joinAndCapture(t, a, vchID)
	joinAndCapture(t, b, vchID)

	a.send(TypeVoiceWhisper, VoiceWhisperData{Targets: []string{"2"}})
	waitHubWhisper(t, f.hub, vchID, "1", true, "2")

	a.send(TypeChannelLeave, ChannelLeaveData{})
	waitHubWhisper(t, f.hub, vchID, "1", false, "")
	// 房间仍在（bob 未走），确保删除的是成员级快照而不是整房销毁。
	if f.hub.roomMemberCount(vchID) != 1 {
		t.Fatalf("离房后房间人数 = %d，期望 1", f.hub.roomMemberCount(vchID))
	}
}

// TestBroadcastVoicePriorityNoRoom 房间不存在时静默返回（REST 调用方可直接调）。
func TestBroadcastVoicePriorityNoRoom(t *testing.T) {
	f := newChatFixture(t)
	f.hub.BroadcastVoicePriority("424242", "2", true) // 不 panic 即可
}

// TestVoiceWhisperRateLimit 悄悄话目标下发按 uid 限次（#117）：窗口内超出上限
// 回 rate_limited，计数跨连接累计（同 uid 换连接不重置），不同 uid 互不影响。
// 取消（空数组）同样计入——它也是「改目标」的一次下发。
func TestVoiceWhisperRateLimit(t *testing.T) {
	f := newChatFixture(t)
	vchID := strconv.FormatInt(f.vch.ID, 10)
	a := f.dial("1", "alice")
	joinAndCapture(t, a, vchID)
	b := f.dial("2", "bob")
	joinAndCapture(t, b, vchID)

	// 上限内全部成功：不发 error。用一个必然失败的 request_state 做顺序屏障。
	for i := 0; i < voiceWhisperRateLimit; i++ {
		targets := []string{"2"}
		if i%2 == 1 {
			targets = []string{}
		}
		a.send(TypeVoiceWhisper, VoiceWhisperData{Targets: targets})
	}
	mute := true
	a.send(TypeVoiceRequestState, VoiceRequestStateData{ChannelID: vchID, TargetID: "1", Mute: &mute})
	env := a.recvUntil(TypeError)
	var e ErrorData
	if err := env.Decode(&e); err != nil {
		t.Fatalf("decode error: %v", err)
	}
	if e.Code != ErrCodeBadMessage || e.Message != "不能对自己发起请求" {
		t.Fatalf("上限内不应被限流，收到 %+v", e)
	}

	// 第 limit+1 次：rate_limited。
	a.send(TypeVoiceWhisper, VoiceWhisperData{Targets: []string{"2"}})
	wantError(t, a, ErrCodeRateLimited)

	// 换一条同 uid 的连接：窗口按 uid 累计，仍然受限。
	a2 := f.dial("1:phone", "alice-phone")
	joinAndCapture(t, a2, vchID)
	a2.send(TypeVoiceWhisper, VoiceWhisperData{Targets: []string{"2"}})
	wantError(t, a2, ErrCodeRateLimited)

	// 另一个 uid 不受影响：其请求照常成功（顺序屏障仍用 request_state）。
	b.send(TypeVoiceWhisper, VoiceWhisperData{Targets: []string{"1"}})
	b.send(TypeVoiceRequestState, VoiceRequestStateData{ChannelID: vchID, TargetID: "2", Mute: &mute})
	var eb ErrorData
	if env := b.recvUntil(TypeError); env.Decode(&eb) != nil || eb.Message != "不能对自己发起请求" {
		t.Fatalf("其他 uid 不应被限流，收到 %+v", eb)
	}
}
