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
	if env := a.recvUntil(TypeChatMessage); env.Seq != 1 {
		t.Fatalf("m1 seq = %d, want 1", env.Seq)
	}
	_ = a.conn.CloseNow()
	waitOffline(t, f, "1")

	for i := 2; i <= 3; i++ {
		b.send(TypeChatSend, ChatSendData{ChannelID: chID, Type: "text", Content: "missed"})
		if env := b.recvUntil(TypeChatMessage); env.Type != TypeChatMessage {
			t.Fatalf("发送者未收到广播: %s", env.Type)
		}
	}

	a2 := f.dial("1:desktop:sa", "alice")
	a2.recvUntil(TypeHello)
	a2.send(TypeGatewayResume, GatewayResumeData{SessionID: "sa", LastSeq: 1})
	for want := int64(2); want <= 3; want++ {
		env := a2.recvUntil(TypeChatMessage)
		if env.Seq != want {
			t.Fatalf("补发 seq = %d, want %d", env.Seq, want)
		}
		var msg ChatMessageData
		if err := env.Decode(&msg); err != nil || msg.Content != "missed" {
			t.Fatalf("补发载荷 = %+v (%v)", msg, err)
		}
	}
	// 补发完毕后新事件继续流式：seq 续接 4。
	b.send(TypeChatSend, ChatSendData{ChannelID: chID, Type: "text", Content: "live"})
	if env := a2.recvUntil(TypeChatMessage); env.Seq != 4 {
		t.Fatalf("续接 seq = %d, want 4", env.Seq)
	}
}

// TestGatewayResumeUnknownSession 会话未知（sid 与当前连接不符/从未建立）→
// gateway.invalid_session{unknown_session}，不补发。
func TestGatewayResumeUnknownSession(t *testing.T) {
	f := newChatFixture(t)
	a := f.dial("1:desktop:sa", "alice")
	a.recvUntil(TypeHello)

	// 借别的会话读流：session_id 与连接自己的 sid 不符。
	a.send(TypeGatewayResume, GatewayResumeData{SessionID: "sb", LastSeq: 0})
	env := a.recvUntil(TypeGatewayInvalidSession)
	var d GatewayInvalidSessionData
	if err := env.Decode(&d); err != nil || d.Reason != "unknown_session" {
		t.Fatalf("invalid_session = %+v (%v), want unknown_session", d, err)
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

	// 连接期间收到 seq 1（client 侧已知），随后离线积压 3 条把环滚过。
	b.send(TypeChatSend, ChatSendData{ChannelID: chID, Type: "text", Content: "m1"})
	if env := a.recvUntil(TypeChatMessage); env.Seq != 1 {
		t.Fatalf("m1 seq = %d, want 1", env.Seq)
	}
	_ = a.conn.CloseNow()
	waitOffline(t, f, "1")
	for i := 0; i < 3; i++ {
		b.send(TypeChatSend, ChatSendData{ChannelID: chID, Type: "text", Content: "missed"})
		if env := b.recvUntil(TypeChatMessage); env.Type != TypeChatMessage {
			t.Fatalf("发送者未收到广播: %s", env.Type)
		}
	}

	a2 := f.dial("1:desktop:sa", "alice")
	a2.recvUntil(TypeHello)
	a2.send(TypeGatewayResume, GatewayResumeData{SessionID: "sa", LastSeq: 1})
	env := a2.recvUntil(TypeGatewayInvalidSession)
	var d GatewayInvalidSessionData
	if err := env.Decode(&d); err != nil || d.Reason != "seq_expired" {
		t.Fatalf("invalid_session = %+v (%v), want seq_expired", d, err)
	}
}

// TestGatewayResumeTrailing 追平后续传（last_seq = 现状）：合法空补发，不断连不报错。
func TestGatewayResumeTrailing(t *testing.T) {
	f := newChatFixture(t)
	a := f.dial("1:desktop:sa", "alice")
	b := f.dial("2:desktop:sb", "bob")
	chID := strconv.FormatInt(f.ch.ID, 10)

	b.send(TypeChatSend, ChatSendData{ChannelID: chID, Type: "text", Content: "m1"})
	if env := a.recvUntil(TypeChatMessage); env.Seq != 1 {
		t.Fatalf("m1 seq = %d, want 1", env.Seq)
	}
	a.send(TypeGatewayResume, GatewayResumeData{SessionID: "sa", LastSeq: 1})
	// 无缺失：不补发。下一条实时事件 seq=2 即证明连接仍在正常流式。
	b.send(TypeChatSend, ChatSendData{ChannelID: chID, Type: "text", Content: "m2"})
	if env := a.recvUntil(TypeChatMessage); env.Seq != 2 {
		t.Fatalf("m2 seq = %d, want 2", env.Seq)
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
