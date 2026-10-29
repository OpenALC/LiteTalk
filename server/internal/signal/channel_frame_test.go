package signal

import (
	"strconv"
	"testing"
	"time"
)

// #116 10ms 帧长能力闸：proto ≥3 允许入房，proto <3（含未协商的 v1）拒绝；
// 被拒连接不登记、不广播，房间成员数不变。

func TestJoin10msFrameProtocolGate(t *testing.T) {
	f := newChatFixture(t)
	vchID := strconv.FormatInt(f.vch.ID, 10)
	setVchAudio(t, f, 0, 10)

	// proto4 放行：入房快照带权威 10ms。
	a := f.dial("1", "alice")
	if v := negotiate(t, a, 4); v != 4 {
		t.Fatalf("alice 协商版本 = %d，期望 4", v)
	}
	aj := joinAndCapture(t, a, vchID)
	if aj.AudioFrameMs == nil || *aj.AudioFrameMs != 10 {
		t.Fatalf("channel.joined 应带 10ms 帧长: %+v", aj)
	}

	// proto3 放行。
	b := f.dial("2", "bob")
	if v := negotiate(t, b, 3); v != 3 {
		t.Fatalf("bob 协商版本 = %d，期望 3", v)
	}
	joinAndCapture(t, b, vchID)

	// proto2 拒绝且不登记进房。
	m := f.dial("9", "mallory")
	if v := negotiate(t, m, 2); v != 2 {
		t.Fatalf("mallory 协商版本 = %d，期望 2", v)
	}
	m.send(TypeChannelJoin, ChannelJoinData{ChannelID: vchID})
	wantError(t, m, ErrCodeProtocolUnsupported)
	if f.hub.roomMember(vchID, "9") != nil {
		t.Fatal("被拒的 proto2 客户端不应登记进房")
	}

	// 未协商的 v1 老客户端同样拒绝（同 uid 另一设备，不影响已入房连接）。
	legacy := f.dial("9:phone", "mallory-phone")
	legacy.send(TypeChannelJoin, ChannelJoinData{ChannelID: vchID})
	wantError(t, legacy, ErrCodeProtocolUnsupported)
	if f.hub.roomMember(vchID, "9") != nil {
		t.Fatal("未协商的 v1 客户端不应登记进房")
	}
	if n := f.hub.roomMemberCount(vchID); n != 2 {
		t.Fatalf("房内人数 = %d，期望 2（仅 proto4/proto3）", n)
	}
	if f.hub.RoomHasLegacyClients(vchID) {
		t.Fatal("房内只有 proto4/proto3，不应判定有旧客户端")
	}

	// 20ms 频道允许 proto2 入房；此后能力闸必须命中（PATCH 侧据此拒绝 10ms）。
	setVchAudio(t, f, 0, 20)
	m.send(TypeChannelJoin, ChannelJoinData{ChannelID: vchID})
	m.recvUntil(TypeChannelJoined)
	if !f.hub.RoomHasLegacyClients(vchID) {
		t.Fatal("房内有 proto2 连接时应判定为有旧客户端")
	}
	// 旧客户端离房后恢复 false。
	m.send(TypeChannelLeave, ChannelLeaveData{})
	deadline := time.Now().Add(5 * time.Second)
	for f.hub.roomMember(vchID, "9") != nil && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if f.hub.RoomHasLegacyClients(vchID) {
		t.Fatal("旧客户端离房后不应再判定有旧客户端")
	}
}
