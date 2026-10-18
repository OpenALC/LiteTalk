package signal

import (
	"context"
	"encoding/json"
	"strconv"
	"strings"
	"testing"
	"time"
)

// TestWireRTCFailedJSON 协议逐字同步（#19）：rtc.failed 常量与载荷字段名与
// client/internal/signal/protocol.go 必须一致，改一边必须改另一边。
func TestWireRTCFailedJSON(t *testing.T) {
	if TypeRTCFailed != "rtc.failed" {
		t.Fatalf("常量值漂移: %q", TypeRTCFailed)
	}
	raw, err := json.Marshal(RTCFailedData{ChannelID: "7", Reason: "ice_failed"})
	if err != nil {
		t.Fatal(err)
	}
	s := string(raw)
	for _, key := range []string{`"channel_id":"7"`, `"reason":"ice_failed"`} {
		if !strings.Contains(s, key) {
			t.Fatalf("RTCFailedData JSON 缺 %s: %s", key, s)
		}
	}
	var d RTCFailedData
	if err := json.Unmarshal([]byte(`{"channel_id":"9","reason":"ice_failed"}`), &d); err != nil {
		t.Fatal(err)
	}
	if d.ChannelID != "9" || d.Reason != "ice_failed" {
		t.Fatalf("反序列化不一致: %+v", d)
	}
}

// TestICEFailNotifiesOnlyPeerAndAllowsRejoin ICE 失败清理（#19）：只给该 peer 的
// 连接发 rtc.failed，其余成员走 channel.peer_left，且本端随后重发 channel.join
// 能正常重入房（成员摘除是重入的前提，否则被「已在该频道中」拒绝）。
func TestICEFailNotifiesOnlyPeerAndAllowsRejoin(t *testing.T) {
	f := newChatFixture(t)
	chID := strconv.FormatInt(f.vch.ID, 10)

	a := f.dial("1", "alice")
	b := f.dial("2", "bob")
	a.send(TypeChannelJoin, ChannelJoinData{ChannelID: chID})
	b.send(TypeChannelJoin, ChannelJoinData{ChannelID: chID})
	a.recvUntil(TypeChannelJoined)
	b.recvUntil(TypeChannelJoined)

	// 模拟 SFU 侧 ICE 失败回调（与生产同路径：clientSignal.SendFailed）。
	c := f.hub.roomMember(chID, "1")
	if c == nil {
		t.Fatal("alice 不在房间登记中")
	}
	(&clientSignal{hub: f.hub, c: c}).SendFailed(chID, "1", "ice_failed")

	env := a.recvUntil(TypeRTCFailed)
	var d RTCFailedData
	if err := env.Decode(&d); err != nil {
		t.Fatalf("decode rtc.failed: %v", err)
	}
	if d.ChannelID != chID || d.Reason != "ice_failed" {
		t.Fatalf("rtc.failed 载荷错误: %+v", d)
	}

	env = b.recvUntil(TypeChannelPeerLeft)
	var pl ChannelPeerLeftData
	if err := env.Decode(&pl); err != nil {
		t.Fatalf("decode peer_left: %v", err)
	}
	if pl.UserID != "1" || pl.ChannelID != chID {
		t.Fatalf("peer_left 载荷错误: %+v", pl)
	}

	// 重入房（契约 0.2：同 channel_id 重发 channel.join）。
	a.send(TypeChannelJoin, ChannelJoinData{ChannelID: chID})
	a.recvUntil(TypeChannelJoined)
	if f.hub.roomMember(chID, "1") == nil {
		t.Fatal("重入后 alice 应重新登记为房间成员（#19）")
	}
}

// TestICEFailWrongChannelKeepsMembership channelID 与连接当前房间不符的 rtc.failed
// （陈旧通知）只发消息、不摘成员、不广播 peer_left（#19 资源归属校验）。
func TestICEFailWrongChannelKeepsMembership(t *testing.T) {
	f := newChatFixture(t)
	chID := strconv.FormatInt(f.vch.ID, 10)

	a := f.dial("1", "alice")
	b := f.dial("2", "bob")
	a.send(TypeChannelJoin, ChannelJoinData{ChannelID: chID})
	b.send(TypeChannelJoin, ChannelJoinData{ChannelID: chID})
	a.recvUntil(TypeChannelJoined)
	b.recvUntil(TypeChannelJoined)

	c := f.hub.roomMember(chID, "1")
	if c == nil {
		t.Fatal("alice 不在房间登记中")
	}
	(&clientSignal{hub: f.hub, c: c}).SendFailed("9999", "1", "ice_failed")

	env := a.recvUntil(TypeRTCFailed)
	var d RTCFailedData
	if err := env.Decode(&d); err != nil {
		t.Fatalf("decode rtc.failed: %v", err)
	}
	if d.ChannelID != "9999" {
		t.Fatalf("应原样透传 channel_id，实际 %+v", d)
	}
	if f.hub.roomMember(chID, "1") == nil {
		t.Fatal("错 channelID 的失败通知不得摘除成员登记（#19）")
	}

	// bob 不得收到 peer_left（短窗口断言：有消息到达就判类型）。
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	_, raw, err := b.conn.Read(ctx)
	if err == nil {
		var got Envelope
		_ = json.Unmarshal(raw, &got)
		if got.Type == TypeChannelPeerLeft {
			t.Fatal("错 channelID 不得向其余成员广播 peer_left（#19）")
		}
	}
}
