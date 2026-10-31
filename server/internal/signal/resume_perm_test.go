package signal

import (
	"context"
	"encoding/json"
	"strconv"
	"testing"
	"time"

	"github.com/OpenALC/LiteTalk/server/internal/store"
)

// #102 事件环重放与权限撤销：失去 ViewChannel 之后，续传窗口内不得再补发
// 该频道已进环的旧消息。

// mustMarshal 测试载荷序列化（失败即 t.Fatal）。
func mustMarshal(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return b
}

// TestResumeWithheldAfterRevoke 撤销 ViewChannel 后续传：环里的旧消息不得补发，
// 改为 invalid_session 让客户端走 REST 全量补拉。
// 被撤销者是成员 bob（alice 是服主，覆盖对 owner 不生效）。
func TestResumeWithheldAfterRevoke(t *testing.T) {
	f := newChatFixture(t)
	chID := strconv.FormatInt(f.ch.ID, 10)

	b := f.dial("2:desktop:sb", "bob")
	a := f.dial("1", "alice")
	a.recvUntil(TypeHello)
	_ = a.conn.CloseNow()
	waitOffline(t, f, "1")

	// bob 断线期间 alice 发一条：它进入 sb 的事件环。
	a2 := f.dial("1", "alice2")
	a2.recvUntil(TypeHello)
	a2.send(TypeChatSend, ChatSendData{ChannelID: chID, Type: "text", Content: "secret"})
	a2.recvUntil(TypeChatMessage)
	waitRingSeq(t, f, "sb", seqOfChat(1)+1)

	_ = b.conn.CloseNow()
	waitOffline(t, f, "2")

	// 撤销 bob 对该文字频道的 ViewChannel（写个人 deny 覆盖并失效快照）。
	denyOnChannel(t, f, f.ch.ID, 2, store.ViewChannel)
	if ok, err := f.hub.authz.CanInServer(context.Background(), 2, f.ch.ServerID, f.ch.ID, store.ViewChannel); err != nil || ok {
		t.Fatalf("撤销后 bob 仍可见该频道（ok=%v err=%v）", ok, err)
	}

	b2 := f.dial("2:desktop:sb", "bob-resume")
	b2.recvUntil(TypeHello)
	b2.send(TypeGatewayResume, GatewayResumeData{SessionID: "sb", LastSeq: 0})

	// 必须收到 invalid_session（转全量补拉），且不得收到被扣留频道的 chat.message。
	env := b2.recvUntil(TypeGatewayInvalidSession)
	var d GatewayInvalidSessionData
	if err := env.Decode(&d); err != nil {
		t.Fatalf("decode invalid_session: %v", err)
	}
	if d.Reason != "seq_expired" {
		t.Fatalf("reason = %q，want seq_expired（补发窗口内有扣留事件）", d.Reason)
	}
	if d.LatestSeq != seqOfChat(1)+1 {
		t.Fatalf("latest_seq = %d，want %d", d.LatestSeq, seqOfChat(1)+1)
	}
	p := newPump(t, b2)
	if leaked, ok := p.within(500 * time.Millisecond); ok {
		t.Fatalf("撤销权限后不得补发该频道消息，实际收到 %+v", leaked)
	}
}

// TestResumeKeepsVisibleChannel 未撤销权限时补发照常（过滤不能误伤正常续传）。
func TestResumeKeepsVisibleChannel(t *testing.T) {
	f := newChatFixture(t)
	chID := strconv.FormatInt(f.ch.ID, 10)

	b := f.dial("2:desktop:sb", "bob")
	a := f.dial("1", "alice")
	a.recvUntil(TypeHello)
	_ = a.conn.CloseNow()
	waitOffline(t, f, "1")

	a2 := f.dial("1", "alice2")
	a2.recvUntil(TypeHello)
	a2.send(TypeChatSend, ChatSendData{ChannelID: chID, Type: "text", Content: "hello"})
	a2.recvUntil(TypeChatMessage)
	waitRingSeq(t, f, "sb", seqOfChat(1)+1)

	_ = b.conn.CloseNow()
	waitOffline(t, f, "2")

	b2 := f.dial("2:desktop:sb", "bob-resume")
	b2.recvUntil(TypeHello)
	b2.send(TypeGatewayResume, GatewayResumeData{SessionID: "sb", LastSeq: 0})

	env := b2.recvUntil(TypeChatMessage)
	var msg ChatMessageData
	if err := env.Decode(&msg); err != nil || msg.Content != "hello" {
		t.Fatalf("补发载荷 = %+v (%v)，want hello", msg, err)
	}
	if env.Seq != seqOfChat(1) {
		t.Fatalf("补发 seq = %d，want %d", env.Seq, seqOfChat(1))
	}
}

// TestResumeWithheldAfterKick 被移出服务器后，续传不得补发该服频道消息。
func TestResumeWithheldAfterKick(t *testing.T) {
	f := newChatFixture(t)
	chID := strconv.FormatInt(f.ch.ID, 10)

	b := f.dial("2:desktop:sb", "bob")
	a := f.dial("1", "alice")
	a.recvUntil(TypeHello)
	_ = a.conn.CloseNow()
	waitOffline(t, f, "1")

	a2 := f.dial("1", "alice2")
	a2.recvUntil(TypeHello)
	a2.send(TypeChatSend, ChatSendData{ChannelID: chID, Type: "text", Content: "before-kick"})
	a2.recvUntil(TypeChatMessage)
	waitRingSeq(t, f, "sb", seqOfChat(1)+1)

	_ = b.conn.CloseNow()
	waitOffline(t, f, "2")

	if err := f.st.RemoveMember(context.Background(), f.ch.ServerID, 2); err != nil {
		t.Fatalf("RemoveMember: %v", err)
	}
	f.hub.authz.InvalidateServer(f.ch.ServerID)

	b2 := f.dial("2:desktop:sb", "bob-resume")
	b2.recvUntil(TypeHello)
	b2.send(TypeGatewayResume, GatewayResumeData{SessionID: "sb", LastSeq: 0})

	if env := b2.recvUntil(TypeGatewayInvalidSession); env.Type != TypeGatewayInvalidSession {
		t.Fatalf("被移出服务器后应 invalid_session，实际 %s", env.Type)
	}
	p := newPump(t, b2)
	if leaked, ok := p.within(500 * time.Millisecond); ok {
		t.Fatalf("被移出服务器后不得补发该服消息，实际收到 %+v", leaked)
	}
}

// TestFilterReplayByPermUnit filterReplayByPerm 的直接断言：可见保留、不可见
// 扣留、无频道维度的事件原样放过、presence 降级为 online-only。
func TestFilterReplayByPermUnit(t *testing.T) {
	f := newChatFixture(t)
	chID := strconv.FormatInt(f.ch.ID, 10)

	mk := func(seq int64, typ string, payload any) Event {
		return Event{Seq: seq, Type: typ, Payload: mustMarshal(t, payload)}
	}
	events := []Event{
		mk(1, TypeChatMessage, ChatMessageData{ChannelID: chID, Content: "m1"}),
		mk(2, TypeSessionKicked, SessionKickedData{Reason: "x"}),
		mk(3, TypePresenceUpdate, PresenceUpdateData{
			UserID: "9", Online: true, ChannelID: "99", ServerID: "9",
		}),
	}
	c := &client{userID: "2"}

	// bob(2) 对该频道有 ViewChannel（默认成员权限）：全部保留，presence 原样。
	out, withheld := f.hub.filterReplayByPerm(c, events)
	if withheld != 0 || len(out) != 3 {
		t.Fatalf("有权时 withheld=%d len(out)=%d，want 0/3", withheld, len(out))
	}

	// 撤销 bob 对文字频道的 ViewChannel → chat.message 被扣留；presence 降级。
	denyOnChannel(t, f, f.ch.ID, 2, store.ViewChannel)
	out, withheld = f.hub.filterReplayByPerm(c, events)
	if withheld != 1 {
		t.Fatalf("撤销后 withheld=%d，want 1", withheld)
	}
	if len(out) != 2 {
		t.Fatalf("len(out)=%d，want 2（kicked + 降级 presence）", len(out))
	}
	if out[0].Type != TypeSessionKicked {
		t.Fatalf("首条应为 session.kicked，实际 %s", out[0].Type)
	}
	if replayChannelID(out[1]) != "" {
		t.Fatalf("presence 频道字段应被降级清空：%s", out[1].Payload)
	}
	var p PresenceUpdateData
	if err := json.Unmarshal(out[1].Payload, &p); err != nil || !p.Online || p.UserID != "9" {
		t.Fatalf("降级后仍应保留在线信息：%+v err=%v", p, err)
	}
}

// TestFilterReplayByPermSourceOnError 权限查询故障时按不可见处理（fail closed）。
func TestFilterReplayByPermSourceOnError(t *testing.T) {
	f := newChatFixture(t)
	chID := strconv.FormatInt(f.ch.ID, 10)
	f.hub.authz = &stubAuthz{fail: map[int64]bool{2: true}}

	ev := Event{Seq: 1, Type: TypeChatMessage, Payload: mustMarshal(t, ChatMessageData{ChannelID: chID, Content: "leak"})}
	out, withheld := f.hub.filterReplayByPerm(&client{userID: "2"}, []Event{ev})
	if withheld != 1 || len(out) != 0 {
		t.Fatalf("故障时应扣留（withheld=%d len=%d）", withheld, len(out))
	}
}
