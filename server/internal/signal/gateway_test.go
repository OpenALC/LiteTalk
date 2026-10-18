package signal

import (
	"encoding/json"
	"strconv"
	"testing"
	"time"
)

// ---- EventLog 事件环（#32）----

func ev(seq int64) Event {
	p, _ := json.Marshal(map[string]int{"n": int(seq)})
	return Event{Type: TypeChatMessage, Payload: p, TS: seq}
}

// TestGatewayRingAppendReplay 追加与补发：seq 从 1 起单调，Replay 从 fromSeq 起升序。
func TestGatewayRingAppendReplay(t *testing.T) {
	l := newMemEventLog(8)
	l.Bind("s1", "u1")
	for i := 1; i <= 5; i++ {
		if seq := l.Append("s1", ev(int64(i))); seq != int64(i) {
			t.Fatalf("Append seq = %d, want %d", seq, i)
		}
	}
	got, reason := l.Replay("s1", 3)
	if reason != "" {
		t.Fatalf("Replay reason = %q", reason)
	}
	if len(got) != 3 || got[0].Seq != 3 || got[2].Seq != 5 {
		t.Fatalf("Replay = %+v", got)
	}
	// fromSeq = latest+1：空补发但合法（已追平）。
	got, reason = l.Replay("s1", 6)
	if reason != "" || len(got) != 0 {
		t.Fatalf("追平后 Replay = (%d条,%q), want (0,\"\")", len(got), reason)
	}
	// fromSeq=0 视为 1（容错）。
	got, reason = l.Replay("s1", 0)
	if reason != "" || len(got) != 5 {
		t.Fatalf("Replay(0) = (%d条,%q), want (5,\"\")", len(got), reason)
	}
}

// TestGatewayRingOverflow 环滚过容量：补发起点落到被淘汰区间 → seq_expired。
func TestGatewayRingOverflow(t *testing.T) {
	l := newMemEventLog(3)
	l.Bind("s1", "u1")
	for i := 1; i <= 5; i++ {
		l.Append("s1", ev(int64(i)))
	}
	if _, reason := l.Replay("s1", 1); reason != "seq_expired" {
		t.Fatalf("滚过后 Replay reason = %q, want seq_expired", reason)
	}
	got, reason := l.Replay("s1", 3)
	if reason != "" || len(got) != 3 || got[0].Seq != 3 {
		t.Fatalf("环内 Replay = (%+v,%q)", got, reason)
	}
}

// TestGatewayRingUnknownSession 无环（未连接过/已淘汰/服务端重启后）→ unknown_session。
func TestGatewayRingUnknownSession(t *testing.T) {
	l := newMemEventLog(8)
	if _, reason := l.Replay("nope", 1); reason != "unknown_session" {
		t.Fatalf("reason = %q, want unknown_session", reason)
	}
	// Bind 后空环：fromSeq=1 是「已追平」，fromSeq=2 是超前（seq_expired）。
	l.Bind("s1", "u1")
	if _, reason := l.Replay("s1", 1); reason != "" {
		t.Fatalf("空环 Replay(1) reason = %q", reason)
	}
	if _, reason := l.Replay("s1", 2); reason != "seq_expired" {
		t.Fatalf("超前 Replay reason = %q, want seq_expired", reason)
	}
	// 空 sid 不建环、不分配 seq。
	if seq := l.Append("", ev(1)); seq != 0 {
		t.Fatalf("空 sid Append = %d, want 0", seq)
	}
}

// TestGatewayRingSidsOf 会话归属：同 uid 多会话都能被广播定向；重绑刷新不重复。
func TestGatewayRingSidsOf(t *testing.T) {
	l := newMemEventLog(8)
	l.Bind("s1", "u1")
	l.Bind("s2", "u1")
	l.Bind("s3", "u2")
	l.Bind("s1", "u1") // 重复绑定幂等
	sids := l.SidsOf("u1")
	if len(sids) != 2 {
		t.Fatalf("SidsOf(u1) = %v, want 2 条", sids)
	}
	if len(l.SidsOf("u2")) != 1 || l.SidsOf("nope") != nil {
		t.Fatalf("SidsOf 其他 uid 不符合预期")
	}
	// 无会话连接（dev/旧令牌）不进索引，广播只走实时投递。
	if got := l.SidsOf("dev-user"); got != nil {
		t.Fatalf("SidsOf(无会话) = %v, want nil", got)
	}
}

// TestGatewayRingIdleSweep 空闲淘汰后 unknown_session（续传窗口 = TTL ∩ 环容量）。
func TestGatewayRingIdleSweep(t *testing.T) {
	l := newMemEventLog(8)
	l.Bind("s1", "u1")
	l.Append("s1", ev(1))
	r := l.rings["s1"]
	r.last = time.Now().Add(-ringIdleTTL - time.Minute)
	l.lastSweep = time.Time{} // 强制下次写入路径触发 sweep
	l.Bind("s2", "u2")        // 触发 sweep
	if _, ok := l.rings["s1"]; ok {
		t.Fatal("超时环未被淘汰")
	}
	if _, reason := l.Replay("s1", 1); reason != "unknown_session" {
		t.Fatalf("淘汰后 Replay reason = %q, want unknown_session", reason)
	}
	if len(l.SidsOf("u1")) != 0 {
		t.Fatalf("淘汰后 uid 索引未清: %v", l.SidsOf("u1"))
	}
}

// ---- 事件面广播与 seq（#32）----

// TestChatMessageCarriesSeq chat.message 走事件面：信封带 per-sid seq，
// 发送者与接收者各自会话的 seq 独立递增。
func TestChatMessageCarriesSeq(t *testing.T) {
	f := newChatFixture(t)
	a := f.dial("1:desktop:sa", "alice")
	b := f.dial("2:desktop:sb", "bob")

	b.send(TypeChatSend, ChatSendData{ChannelID: strconv.FormatInt(f.ch.ID, 10), Type: "text", Content: "hi"})
	for _, c := range []*wsClient{a, b} {
		env := c.recvUntil(TypeChatMessage)
		if env.Seq != 1 {
			t.Fatalf("%s 的 chat.message seq = %d, want 1", c.user, env.Seq)
		}
	}
	b.send(TypeChatSend, ChatSendData{ChannelID: strconv.FormatInt(f.ch.ID, 10), Type: "text", Content: "hi2"})
	if env := a.recvUntil(TypeChatMessage); env.Seq != 2 {
		t.Fatalf("第二条 seq = %d, want 2", env.Seq)
	}
}

// TestGatewayRingAccumulatesOffline 事件面订阅不依赖在线：断线会话的环继续留存
// （这是「断线重连后事件不丢」的前提），同 sid 重连后可补发。
func TestGatewayRingAccumulatesOffline(t *testing.T) {
	f := newChatFixture(t)
	a := f.dial("1:desktop:sa", "alice")
	b := f.dial("2:desktop:sb", "bob")
	_ = a.recvUntil(TypeHello)

	// alice 断线（网络抖动窗口）；bob 发两条——此时 alice 不是「在线成员」。
	if err := a.conn.CloseNow(); err != nil {
		t.Fatalf("close: %v", err)
	}
	// 等 cleanup 摘掉连接（presence 下线），确认事件不是靠「在线连接」送达的。
	deadline := time.Now().Add(3 * time.Second)
	for f.hub.connCount("1") != 0 {
		if time.Now().After(deadline) {
			t.Fatal("alice 连接未在窗口内摘除")
		}
		time.Sleep(10 * time.Millisecond)
	}
	for i := 0; i < 2; i++ {
		b.send(TypeChatSend, ChatSendData{ChannelID: strconv.FormatInt(f.ch.ID, 10), Type: "text", Content: "missed"})
		if env := b.recvUntil(TypeChatMessage); env.Type != TypeChatMessage {
			t.Fatalf("发送者未收到广播: %s", env.Type)
		}
	}

	events, reason := f.hub.evlog.Replay("sa", 1)
	if reason != "" {
		t.Fatalf("离线会话 Replay = %q, want \"\"", reason)
	}
	if len(events) != 2 || events[0].Seq != 1 || events[1].Seq != 2 {
		t.Fatalf("离线环 = %+v, want 2 条 seq 1,2", events)
	}
	var msg ChatMessageData
	if err := json.Unmarshal(events[0].Payload, &msg); err != nil || msg.Content != "missed" {
		t.Fatalf("环内载荷 = %+v (%v)", msg, err)
	}

	// 同 sid 重连：环不重建、seq 续接（resume 的补发数据源）。
	a2 := f.dial("1:desktop:sa", "alice-re")
	b.send(TypeChatSend, ChatSendData{ChannelID: strconv.FormatInt(f.ch.ID, 10), Type: "text", Content: "after"})
	// 收到广播即代表服务端已完成进环，再断言环内容（chat.send 是异步处理）。
	if env := a2.recvUntil(TypeChatMessage); env.Seq != 3 {
		t.Fatalf("重连后实时投递 seq = %d, want 3", env.Seq)
	}
	events, reason = f.hub.evlog.Replay("sa", 1)
	if reason != "" || len(events) != 3 || events[2].Seq != 3 {
		t.Fatalf("重连后续接 = (%+v,%q)", events, reason)
	}
}

// TestGatewayRingPerSession 各会话 seq 独立：多设备两条环互不影响。
func TestGatewayRingPerSession(t *testing.T) {
	f := newChatFixture(t)
	a1 := f.dial("1:desktop:s1", "alice-d1")
	a2 := f.dial("1:mobile:s2", "alice-d2")
	b := f.dial("2:desktop:sb", "bob")

	b.send(TypeChatSend, ChatSendData{ChannelID: strconv.FormatInt(f.ch.ID, 10), Type: "text", Content: "x"})
	e1 := a1.recvUntil(TypeChatMessage)
	e2 := a2.recvUntil(TypeChatMessage)
	if e1.Seq != 1 || e2.Seq != 1 {
		t.Fatalf("各会话 seq 应独立: %d %d", e1.Seq, e2.Seq)
	}
	// 同 uid 两条环都能补发（语音单处不影响事件面多设备）。
	if n := len(f.hub.evlog.SidsOf("1")); n != 2 {
		t.Fatalf("SidsOf(1) = %d 条, want 2", n)
	}
}
