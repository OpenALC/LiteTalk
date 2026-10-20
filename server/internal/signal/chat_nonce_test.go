package signal

import (
	"context"
	"strconv"
	"strings"
	"testing"

	"github.com/OpenALC/LiteTalk/server/internal/store"
)

// TestChatSendNonceIdempotent WS nonce 幂等（#31）：同频道同用户同 nonce 只落一条、
// 只广播一次；重放把原消息定向回显给发起者，其他成员不再收第二次广播。
func TestChatSendNonceIdempotent(t *testing.T) {
	f := newChatFixture(t)
	chID := strconv.FormatInt(f.ch.ID, 10)

	a := f.dial("1", "alice")
	b := f.dial("2", "bob")

	a.send(TypeChatSend, ChatSendData{ChannelID: chID, Type: "text", Content: "首发", Nonce: "n-1"})

	var firstID string
	for _, c := range []*wsClient{a, b} {
		env := c.recvUntil(TypeChatMessage)
		var msg ChatMessageData
		if err := env.Decode(&msg); err != nil {
			t.Fatalf("decode chat.message: %v", err)
		}
		if msg.Content != "首发" || msg.Nonce != "n-1" {
			t.Fatalf("%s 首次广播内容/nonce 不符: %+v", c.user, msg)
		}
		if msg.ID == "" {
			t.Fatalf("%s 首次广播缺 id", c.user)
		}
		if c == a {
			firstID = msg.ID
		}
	}

	// 同 nonce 重放（内容不同）：发起者收原消息定向回显，其他成员不应再收广播。
	a.send(TypeChatSend, ChatSendData{ChannelID: chID, Type: "text", Content: "重放内容", Nonce: "n-1"})
	echo := a.recvUntil(TypeChatMessage)
	var replay ChatMessageData
	if err := echo.Decode(&replay); err != nil {
		t.Fatalf("decode 重放回显: %v", err)
	}
	if replay.ID != firstID || replay.Content != "首发" || replay.Nonce != "n-1" {
		t.Fatalf("重放应回显原消息：%+v（原 id=%s）", replay, firstID)
	}

	// b 的下一条广播必须是新消息——若重放被再次广播，这里会先读到「首发」。
	a.send(TypeChatSend, ChatSendData{ChannelID: chID, Type: "text", Content: "第二条", Nonce: "n-2"})
	next := b.recvUntil(TypeChatMessage)
	var nextMsg ChatMessageData
	if err := next.Decode(&nextMsg); err != nil {
		t.Fatalf("decode 第二条: %v", err)
	}
	if nextMsg.Content != "第二条" || nextMsg.ID == firstID {
		t.Fatalf("b 收到重放广播（应只有一次）：%+v", nextMsg)
	}
	// a 也应收到第二条（重放回显之后）。
	envA := a.recvUntil(TypeChatMessage)
	var msgA ChatMessageData
	if err := envA.Decode(&msgA); err != nil || msgA.Content != "第二条" {
		t.Fatalf("a 未收到第二条广播：%+v %v", msgA, err)
	}

	msgs, err := f.st.MessagesBefore(context.Background(), f.ch.ID, 0, 10)
	if err != nil {
		t.Fatalf("MessagesBefore: %v", err)
	}
	if len(msgs) != 2 {
		t.Fatalf("同 nonce 重放不应重复落库，实际 %d 条: %+v", len(msgs), msgs)
	}
}

// TestChatSendNonceChannelScoped 去重键含频道（#31）：同一 nonce 在不同频道各自落库，
// 回读不串频道。
func TestChatSendNonceChannelScoped(t *testing.T) {
	f := newChatFixture(t)
	ctx := context.Background()
	ch2 := &store.Channel{ServerID: f.ch.ServerID, Name: "general-2", Type: store.ChannelText}
	if err := f.st.CreateChannel(ctx, ch2); err != nil {
		t.Fatalf("CreateChannel: %v", err)
	}
	ch1ID, ch2ID := strconv.FormatInt(f.ch.ID, 10), strconv.FormatInt(ch2.ID, 10)

	a := f.dial("1", "alice")
	send := func(channelID, content string) ChatMessageData {
		t.Helper()
		a.send(TypeChatSend, ChatSendData{ChannelID: channelID, Type: "text", Content: content, Nonce: "same"})
		var msg ChatMessageData
		if err := a.recvUntil(TypeChatMessage).Decode(&msg); err != nil {
			t.Fatalf("decode chat.message: %v", err)
		}
		return msg
	}

	m1 := send(ch1ID, "频道一")
	m2 := send(ch2ID, "频道二")
	if m1.ID == m2.ID {
		t.Fatalf("跨频道同 nonce 不应命中同一条消息：%s", m1.ID)
	}
	if m1.ChannelID != ch1ID || m2.ChannelID != ch2ID {
		t.Fatalf("回显频道错误：%+v %+v", m1, m2)
	}

	// 各自重放回各自频道的原消息。
	if replay := send(ch1ID, "重放一"); replay.ID != m1.ID || replay.Content != "频道一" {
		t.Fatalf("频道一重放应回原消息：%+v", replay)
	}
	if replay := send(ch2ID, "重放二"); replay.ID != m2.ID || replay.Content != "频道二" {
		t.Fatalf("频道二重放应回原消息：%+v", replay)
	}

	for _, ch := range []*store.Channel{f.ch, ch2} {
		msgs, err := f.st.MessagesBefore(ctx, ch.ID, 0, 10)
		if err != nil {
			t.Fatalf("MessagesBefore(%d): %v", ch.ID, err)
		}
		if len(msgs) != 1 {
			t.Fatalf("频道 %d 应各 1 条，实际 %d", ch.ID, len(msgs))
		}
	}
}

// TestChatNonceIdempotentBeforeRateLimit 幂等判断先于频控（审查 P2）：配额耗尽后
// 重放已成功的 nonce 仍回原消息而不是 rate_limited；新消息才触发 429。
func TestChatNonceIdempotentBeforeRateLimit(t *testing.T) {
	f := newChatFixture(t)
	chID := strconv.FormatInt(f.ch.ID, 10)

	a := f.dial("1", "alice")
	var firstID string
	for i := 0; i < chatRateLimit; i++ {
		nonce := "n-" + strconv.Itoa(i)
		a.send(TypeChatSend, ChatSendData{ChannelID: chID, Type: "text", Content: "msg", Nonce: nonce})
		var msg ChatMessageData
		if err := a.recvUntil(TypeChatMessage).Decode(&msg); err != nil {
			t.Fatalf("decode chat.message: %v", err)
		}
		if i == 0 {
			firstID = msg.ID
		}
	}

	a.send(TypeChatSend, ChatSendData{ChannelID: chID, Type: "text", Content: "超限", Nonce: "n-over"})
	var e ErrorData
	if err := a.recvUntil(TypeError).Decode(&e); err != nil || e.Code != ErrCodeRateLimited {
		t.Fatalf("超限应 rate_limited：%+v %v", e, err)
	}

	// 重放第一条的 nonce：走幂等回读，不消耗配额。
	a.send(TypeChatSend, ChatSendData{ChannelID: chID, Type: "text", Content: "重放", Nonce: "n-0"})
	var replay ChatMessageData
	if err := a.recvUntil(TypeChatMessage).Decode(&replay); err != nil {
		t.Fatalf("decode 幂等回显: %v", err)
	}
	if replay.ID != firstID {
		t.Fatalf("配额耗尽后重放应回原消息 id=%s，实际 %s", firstID, replay.ID)
	}
}

// TestChatSendNonceTooLong 超长 nonce 拒收，且不落库。
func TestChatSendNonceTooLong(t *testing.T) {
	f := newChatFixture(t)
	chID := strconv.FormatInt(f.ch.ID, 10)

	a := f.dial("1", "alice")
	a.send(TypeChatSend, ChatSendData{
		ChannelID: chID, Type: "text", Content: "x", Nonce: strings.Repeat("n", chatMaxNonce+1),
	})
	var e ErrorData
	if err := a.recvUntil(TypeError).Decode(&e); err != nil || e.Code != ErrCodeBadMessage {
		t.Fatalf("超长 nonce 应 bad_message：%+v %v", e, err)
	}
	msgs, err := f.st.MessagesBefore(context.Background(), f.ch.ID, 0, 10)
	if err != nil {
		t.Fatalf("MessagesBefore: %v", err)
	}
	if len(msgs) != 0 {
		t.Fatalf("拒收消息不应落库: %+v", msgs)
	}
}
