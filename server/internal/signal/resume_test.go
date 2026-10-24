package signal

import (
	"strconv"
	"testing"
	"time"
)

// ---- gateway.resume 续传（#32）----

// TestGatewayResumeReplay 断线期间进环的事件，重连 resume 后按 seq 升序补发，
// 用原 seq 直投（不重新分配）；随后新事件续接递增。
func TestGatewayResumeReplay(t *testing.T) {
	f := newChatFixture(t)
	a := f.dial("1:desktop:sa", "alice")
	b := f.dial("2:desktop:sb", "bob")
	chID := strconv.FormatInt(f.ch.ID, 10)

	b.send(TypeChatSend, ChatSendData{ChannelID: chID, Type: "text", Content: "m1"})
	if env := a.recvUntil(TypeChatMessage); env.Seq != seqOfChat(1) {
		t.Fatalf("m1 seq = %d, want %d", env.Seq, seqOfChat(1))
	}
	_ = a.conn.CloseNow()
	waitOffline(t, f, "1")

	for i := 2; i <= 3; i++ {
		b.send(TypeChatSend, ChatSendData{ChannelID: chID, Type: "text", Content: "missed"})
		if env := b.recvUntil(TypeChatMessage); env.Type != TypeChatMessage {
			t.Fatalf("发送者未收到广播: %s", env.Type)
		}
	}
	// 等最后一条 missed（及其 channel.unread）真正进环再重连：慢调度下 a2 可能
	// 先连上，实时事件先于补发到达，断言首条补发 seq 会偶发偏移（CI 踩过）。
	waitRingSeq(t, f, "sa", seqOfChat(3)+1)

	a2 := f.dial("1:desktop:sa", "alice")
	a2.recvUntil(TypeHello)
	// 客户端已消费 m1 的 chat(1) 与自己的 unread(2)。
	a2.send(TypeGatewayResume, GatewayResumeData{SessionID: "sa", LastSeq: 2})
	for n := int64(2); n <= 3; n++ {
		env := a2.recvUntil(TypeChatMessage)
		if env.Seq != seqOfChat(n) {
			t.Fatalf("补发 seq = %d, want %d", env.Seq, seqOfChat(n))
		}
		var msg ChatMessageData
		if err := env.Decode(&msg); err != nil || msg.Content != "missed" {
			t.Fatalf("补发载荷 = %+v (%v)", msg, err)
		}
	}
	// 补发完毕后新事件继续流式：seq 续接第 4 条 chat。
	b.send(TypeChatSend, ChatSendData{ChannelID: chID, Type: "text", Content: "live"})
	if env := a2.recvUntil(TypeChatMessage); env.Seq != seqOfChat(4) {
		t.Fatalf("续接 seq = %d, want %d", env.Seq, seqOfChat(4))
	}
}

// TestGatewayResumeUnknownSession 会话未知（sid 与当前连接不符/从未建立）→
// gateway.invalid_session{unknown_session}，不补发；latest_seq 带当前连接环现状。
func TestGatewayResumeUnknownSession(t *testing.T) {
	f := newChatFixture(t)
	chID := strconv.FormatInt(f.ch.ID, 10)
	a := f.dial("1:desktop:sa", "alice")
	b := f.dial("2:desktop:sb", "bob")
	a.recvUntil(TypeHello)
	// 先制造一条事件，让 sa 的环有 latest_seq=2（chat 1 + unread 2）。
	b.send(TypeChatSend, ChatSendData{ChannelID: chID, Type: "text", Content: "m1"})
	if env := a.recvUntil(TypeChatMessage); env.Seq != seqOfChat(1) {
		t.Fatalf("m1 seq = %d, want %d", env.Seq, seqOfChat(1))
	}

	// 借别的会话读流：session_id 与连接自己的 sid 不符。latest_seq 仍取本连接的。
	a.send(TypeGatewayResume, GatewayResumeData{SessionID: "sb", LastSeq: 0})
	env := a.recvUntil(TypeGatewayInvalidSession)
	var d GatewayInvalidSessionData
	if err := env.Decode(&d); err != nil || d.Reason != "unknown_session" {
		t.Fatalf("invalid_session = %+v (%v), want unknown_session", d, err)
	}
	if d.LatestSeq != seqOfChat(1)+1 {
		t.Fatalf("latest_seq = %d, want %d（当前连接环现状）", d.LatestSeq, seqOfChat(1)+1)
	}

	// 空 session_id：bad_message（载荷校验），不是 invalid_session。
	a.send(TypeGatewayResume, GatewayResumeData{SessionID: "", LastSeq: 0})
	if env := a.recvUntil(TypeError); env.Type != TypeError {
		t.Fatalf("空 session_id 应报 error: %s", env.Type)
	}
}

// TestGatewayResumeSeqExpired 环滚过容量（补发窗口 = backlog）→ seq_expired。
func TestGatewayResumeSeqExpired(t *testing.T) {
	f := newChatFixture(t)
	f.hub.evlog = newMemEventLog(2) // 缩小环容量，覆盖淘汰路径
	a := f.dial("1:desktop:sa", "alice")
	b := f.dial("2:desktop:sb", "bob")
	chID := strconv.FormatInt(f.ch.ID, 10)

	// 连接期间收到 seq 1（chat）+ 2（unread），随后离线积压 3 条把环滚过。
	b.send(TypeChatSend, ChatSendData{ChannelID: chID, Type: "text", Content: "m1"})
	if env := a.recvUntil(TypeChatMessage); env.Seq != seqOfChat(1) {
		t.Fatalf("m1 seq = %d, want %d", env.Seq, seqOfChat(1))
	}
	_ = a.conn.CloseNow()
	waitOffline(t, f, "1")
	for i := 0; i < 3; i++ {
		b.send(TypeChatSend, ChatSendData{ChannelID: chID, Type: "text", Content: "missed"})
		if env := b.recvUntil(TypeChatMessage); env.Type != TypeChatMessage {
			t.Fatalf("发送者未收到广播: %s", env.Type)
		}
	}
	// 同 Replay：确保 3 条 missed（各自带 unread）都已进环（4 次发送 = 8 条）再重连。
	waitRingSeq(t, f, "sa", 8)

	a2 := f.dial("1:desktop:sa", "alice")
	a2.recvUntil(TypeHello)
	a2.send(TypeGatewayResume, GatewayResumeData{SessionID: "sa", LastSeq: 1})
	env := a2.recvUntil(TypeGatewayInvalidSession)
	var d GatewayInvalidSessionData
	if err := env.Decode(&d); err != nil || d.Reason != "seq_expired" {
		t.Fatalf("invalid_session = %+v (%v), want seq_expired", d, err)
	}
	// latest_seq = 环当前最大 seq（4 次 chat + 4 次 unread = 8）；客户端据此对齐基准。
	if d.LatestSeq != 8 {
		t.Fatalf("latest_seq = %d, want 8", d.LatestSeq)
	}
}

// TestGatewayResumeTrailing 追平后续传（last_seq = 现状）：合法空补发，不断连不报错。
func TestGatewayResumeTrailing(t *testing.T) {
	f := newChatFixture(t)
	a := f.dial("1:desktop:sa", "alice")
	b := f.dial("2:desktop:sb", "bob")
	chID := strconv.FormatInt(f.ch.ID, 10)

	b.send(TypeChatSend, ChatSendData{ChannelID: chID, Type: "text", Content: "m1"})
	if env := a.recvUntil(TypeChatMessage); env.Seq != seqOfChat(1) {
		t.Fatalf("m1 seq = %d, want %d", env.Seq, seqOfChat(1))
	}
	// 已消费 m1 的 chat(seq=1) 与 unread(seq=2)，追平当前环。
	a.send(TypeGatewayResume, GatewayResumeData{SessionID: "sa", LastSeq: 2})
	// 无缺失：不补发。下一条实时事件 seq=3 即证明连接仍在正常流式。
	b.send(TypeChatSend, ChatSendData{ChannelID: chID, Type: "text", Content: "m2"})
	if env := a.recvUntil(TypeChatMessage); env.Seq != seqOfChat(2) {
		t.Fatalf("m2 seq = %d, want %d", env.Seq, seqOfChat(2))
	}
}

// waitRingSeq 等 sid 的事件环分配到的最大 seq 达到 want：重连前用它保证
// 断线期间的最后一个 broadcastEvent 已完成进环，避免“重连与最后一条消息抢跑”
// 的偶发（实时事件先于补发到达会让补发首条 seq 断言偏移）。
func waitRingSeq(t *testing.T, f *chatFixture, sid string, want int64) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for f.hub.evlog.LatestSeq(sid) < want {
		if time.Now().After(deadline) {
			t.Fatalf("sid %s 事件环未到 seq %d（当前 %d）", sid, want, f.hub.evlog.LatestSeq(sid))
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// waitOffline 等目标 uid 的连接被 cleanup 摘除（离线积压的前提）。
func waitOffline(t *testing.T, f *chatFixture, uid string) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for f.hub.connCount(uid) != 0 {
		if time.Now().After(deadline) {
			t.Fatalf("uid %s 连接未摘除", uid)
		}
		time.Sleep(10 * time.Millisecond)
	}
}
