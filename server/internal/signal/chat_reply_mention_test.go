package signal

import (
	"context"
	"strconv"
	"testing"
	"time"

	"github.com/OpenALC/LiteTalk/server/internal/store"
)

// #39 B3：chat.send 携带回复/提及、广播字段回填，以及 message.edited /
// message.deleted 的事件投递。

// recvChat 读一条 chat.message 并解码。
func recvChat(t *testing.T, c *wsClient) ChatMessageData {
	t.Helper()
	var msg ChatMessageData
	if err := c.recvUntil(TypeChatMessage).Decode(&msg); err != nil {
		t.Fatalf("decode chat.message: %v", err)
	}
	return msg
}

// sendChat 发送一条 chat.send 并返回发送者收到的广播。
func sendChat(t *testing.T, c *wsClient, d ChatSendData) ChatMessageData {
	t.Helper()
	c.send(TypeChatSend, d)
	return recvChat(t, c)
}

// assertMentionIDs 断言 mentions 为给定的十进制 ID（升序比较）。
func assertMentionIDs(t *testing.T, got []string, want ...string) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("mentions = %v，期望 %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("mentions = %v，期望 %v", got, want)
		}
	}
}

// TestChatSendReplyAndMentions 服务器频道：回复 + 提及的广播字段、去重、
// 软删目标预览、nonce 幂等回显从库回填（不信任重放请求）。
func TestChatSendReplyAndMentions(t *testing.T) {
	f := newChatFixture(t)
	chID := strconv.FormatInt(f.ch.ID, 10)

	a := f.dial("1", "alice")
	b := f.dial("2", "bob")

	orig := sendChat(t, a, ChatSendData{ChannelID: chID, Type: "text", Content: "原消息"})
	if orig.ID == "" {
		t.Fatal("原消息缺 id")
	}
	// b 也收到了原消息：先取走，后续断言才对准各自的下一条广播。
	if got := recvChat(t, b); got.ID != orig.ID {
		t.Fatalf("b 未收到原消息: %+v", got)
	}

	// 回复 + 提及（重复项去重）：双方收到的广播都带 reply_to/reply_preview/mentions。
	b.send(TypeChatSend, ChatSendData{
		ChannelID: chID, Type: "text", Content: "回复你",
		ReplyTo: orig.ID, Mentions: []string{"1", "1"},
	})
	for _, c := range []*wsClient{b, a} {
		msg := recvChat(t, c)
		if msg.Content != "回复你" || msg.ReplyTo != orig.ID {
			t.Fatalf("%s 回复广播不符: %+v", c.user, msg)
		}
		if msg.ReplyPreview == nil {
			t.Fatalf("%s 缺 reply_preview", c.user)
		}
		p := msg.ReplyPreview
		if p.ID != orig.ID || p.UserID != "1" || p.Nickname != "alice" ||
			p.Content != "原消息" || p.Deleted {
			t.Fatalf("%s reply_preview 不符: %+v", c.user, p)
		}
		assertMentionIDs(t, msg.Mentions, "1")
	}

	// 被引用消息软删后仍可回复：预览正文为空、deleted=true。
	gone := sendChat(t, a, ChatSendData{ChannelID: chID, Type: "text", Content: "将被删除"})
	recvChat(t, b) // b 的副本
	goneID, _ := strconv.ParseInt(gone.ID, 10, 64)
	if err := f.st.SoftDeleteMessage(context.Background(), goneID, time.Now()); err != nil {
		t.Fatalf("SoftDeleteMessage: %v", err)
	}
	late := sendChat(t, b, ChatSendData{
		ChannelID: chID, Type: "text", Content: "迟到的回复", ReplyTo: gone.ID,
	})
	recvChat(t, a) // a 的副本
	if late.ReplyPreview == nil || !late.ReplyPreview.Deleted || late.ReplyPreview.Content != "" {
		t.Fatalf("软删目标预览不符: %+v", late.ReplyPreview)
	}

	// nonce 幂等：重放请求体只改 content、不带 reply/mentions，回显仍从库回填。
	first := sendChat(t, b, ChatSendData{
		ChannelID: chID, Type: "text", Content: "幂等回复",
		Nonce: "rp-1", ReplyTo: orig.ID, Mentions: []string{"1"},
	})
	recvChat(t, a) // a 的副本
	replay := sendChat(t, b, ChatSendData{
		ChannelID: chID, Type: "text", Content: "试图替换", Nonce: "rp-1",
	})
	if replay.ID != first.ID || replay.Content != "幂等回复" {
		t.Fatalf("幂等重放应回原消息: %+v（原 id=%s）", replay, first.ID)
	}
	if replay.ReplyTo != orig.ID || replay.ReplyPreview == nil || replay.ReplyPreview.ID != orig.ID {
		t.Fatalf("幂等重放应回填 reply_preview: %+v", replay)
	}
	assertMentionIDs(t, replay.Mentions, "1")

	// 重放不再广播给其他成员：a 的下一条 chat.message 必须是新消息。
	a.send(TypeChatSend, ChatSendData{ChannelID: chID, Type: "text", Content: "下一条"})
	if next := recvChat(t, a); next.Content != "下一条" {
		t.Fatalf("a 的下一条应是新消息: %+v", next)
	}
	if next := recvChat(t, b); next.Content != "下一条" {
		t.Fatalf("b 收到了重放广播（应只回显给发送者）: %+v", next)
	}

	// 落库的回复/提及可回读。
	msgs, err := f.st.MessagesBefore(context.Background(), f.ch.ID, 0, 20)
	if err != nil {
		t.Fatalf("MessagesBefore: %v", err)
	}
	var stored *store.Message
	for i := range msgs {
		if strconv.FormatInt(msgs[i].ID, 10) == first.ID {
			stored = &msgs[i]
		}
	}
	if stored == nil || stored.ReplyTo == nil || strconv.FormatInt(*stored.ReplyTo, 10) != orig.ID {
		t.Fatalf("落库消息缺 reply_to: %+v", stored)
	}
	mentions, err := f.st.MentionsOfMessages(context.Background(), []int64{stored.ID})
	if err != nil || len(mentions[stored.ID]) != 1 || mentions[stored.ID][0] != 1 {
		t.Fatalf("落库提及不符: %v %v", mentions, err)
	}
}

// TestChatSendReplyMentionValidation 跨频道/不存在/非法 ID 的回复与提及：
// 统一 bad_reply / bad_mention，且不落库。
func TestChatSendReplyMentionValidation(t *testing.T) {
	f := newChatFixture(t)
	ctx := context.Background()
	chID := strconv.FormatInt(f.ch.ID, 10)

	// 另一服务器频道里的消息：不能作为本频道的回复目标。
	ch2 := &store.Channel{ServerID: f.ch.ServerID, Name: "other", Type: store.ChannelText}
	if err := f.st.CreateChannel(ctx, ch2); err != nil {
		t.Fatalf("CreateChannel: %v", err)
	}
	other := &store.Message{ChannelID: ch2.ID, UserID: 1, Content: "别的频道的消息"}
	if err := f.st.CreateMessage(ctx, other); err != nil {
		t.Fatalf("CreateMessage: %v", err)
	}

	b := f.dial("2", "bob")
	cases := []struct {
		name string
		data ChatSendData
		code string
	}{
		{"跨频道回复", ChatSendData{ReplyTo: strconv.FormatInt(other.ID, 10)}, ErrCodeBadReply},
		{"回复不存在", ChatSendData{ReplyTo: "999999999"}, ErrCodeBadReply},
		{"回复 0", ChatSendData{ReplyTo: "0"}, ErrCodeBadReply},
		{"回复非数字", ChatSendData{ReplyTo: "abc"}, ErrCodeBadReply},
		{"提及非成员", ChatSendData{Mentions: []string{"8"}}, ErrCodeBadMention},
		{"提及 0", ChatSendData{Mentions: []string{"0"}}, ErrCodeBadMention},
		{"提及非数字", ChatSendData{Mentions: []string{"x"}}, ErrCodeBadMention},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d := tc.data
			d.ChannelID, d.Type, d.Content = chID, "text", "坏请求"
			b.send(TypeChatSend, d)
			wantError(t, b, tc.code)
		})
	}

	// 全部被拒，不应落库（频道内没有任何消息）。
	msgs, err := f.st.MessagesBefore(ctx, f.ch.ID, 0, 10)
	if err != nil {
		t.Fatalf("MessagesBefore: %v", err)
	}
	if len(msgs) != 0 {
		t.Fatalf("非法请求不应落库，实际 %d 条: %+v", len(msgs), msgs)
	}
}

// TestChatConversationReplyMentions 私聊：成员提及通过、非成员提及 bad_mention、
// 跨频道回复 bad_reply，正常回复字段投递双方。
func TestChatConversationReplyMentions(t *testing.T) {
	f := newChatFixture(t)
	ctx := context.Background()

	conv := &store.Channel{Scope: store.ChannelScopeDirect}
	if err := f.st.CreateConversation(ctx, conv, []int64{1, 8}); err != nil {
		t.Fatalf("CreateConversation: %v", err)
	}
	convID := strconv.FormatInt(conv.ID, 10)

	a := f.dial("1", "alice")
	e := f.dial("8", "eve")

	orig := sendChat(t, a, ChatSendData{ChannelID: convID, Type: "text", Content: "私聊原消息"})
	if got := recvChat(t, e); got.ID != orig.ID {
		t.Fatalf("eve 未收到原消息: %+v", got)
	}

	// eve 提及 alice 并回复原消息：双方收到完整字段。
	e.send(TypeChatSend, ChatSendData{
		ChannelID: convID, Type: "text", Content: "收到",
		ReplyTo: orig.ID, Mentions: []string{"1"},
	})
	for _, c := range []*wsClient{e, a} {
		msg := recvChat(t, c)
		if msg.ReplyTo != orig.ID || msg.ReplyPreview == nil ||
			msg.ReplyPreview.Nickname != "alice" || msg.ReplyPreview.Content != "私聊原消息" {
			t.Fatalf("%s 私聊回复字段不符: %+v", c.user, msg)
		}
		assertMentionIDs(t, msg.Mentions, "1")
	}

	// bob(2) 不是会话成员 → bad_mention。
	e.send(TypeChatSend, ChatSendData{
		ChannelID: convID, Type: "text", Content: "提错人", Mentions: []string{"2"},
	})
	wantError(t, e, ErrCodeBadMention)

	// 服务器频道里的消息不能作为私聊的回复目标 → bad_reply。
	other := &store.Message{ChannelID: f.ch.ID, UserID: 1, Content: "频道消息"}
	if err := f.st.CreateMessage(ctx, other); err != nil {
		t.Fatalf("CreateMessage: %v", err)
	}
	e.send(TypeChatSend, ChatSendData{
		ChannelID: convID, Type: "text", Content: "错频道回复",
		ReplyTo: strconv.FormatInt(other.ID, 10),
	})
	wantError(t, e, ErrCodeBadReply)
}

// TestMessageEventsBroadcast message.edited / message.deleted 只投递给指定 uid，
// payload 字段正确，并写入目标会话的事件环。
func TestMessageEventsBroadcast(t *testing.T) {
	f := newChatFixture(t)

	a := f.dial("1", "alice")
	b := f.dial("2", "bob")
	m := f.dial("9", "mallory")

	f.hub.BroadcastMessageEdited([]string{"1", "2"}, "77", "88", "编辑后的正文", 1700000000123, []string{"1", "2"})
	for _, c := range []*wsClient{a, b} {
		var ev MessageEditedData
		if err := c.recvUntil(TypeMessageEdited).Decode(&ev); err != nil {
			t.Fatalf("%s decode message.edited: %v", c.user, err)
		}
		if ev.ChannelID != "77" || ev.MessageID != "88" || ev.Content != "编辑后的正文" ||
			ev.EditedAt != 1700000000123 {
			t.Fatalf("%s message.edited 字段不符: %+v", c.user, ev)
		}
		assertMentionIDs(t, ev.Mentions, "1", "2")
	}
	// mallory 不在目标内：下一条必须是 deleted（若 edited 泄漏会先读到它）。
	f.hub.BroadcastMessageDeleted([]string{"9"}, "77", "88")
	var del MessageDeletedData
	if err := m.recvUntil(TypeMessageDeleted).Decode(&del); err != nil {
		t.Fatalf("decode message.deleted: %v", err)
	}
	if del.ChannelID != "77" || del.MessageID != "88" {
		t.Fatalf("message.deleted 字段不符: %+v", del)
	}

	// 事件环：目标会话留痕，非目标会话没有 edited。
	if events, _ := f.hub.evlog.Replay(defaultTestSid("1"), 0); !hasEventType(events, TypeMessageEdited) {
		t.Fatalf("uid=1 事件环缺 message.edited: %+v", events)
	}
	if events, _ := f.hub.evlog.Replay(defaultTestSid("9"), 0); hasEventType(events, TypeMessageEdited) {
		t.Fatalf("uid=9 事件环不应有 message.edited: %+v", events)
	}

	// 空目标列表不 panic、不产生事件。
	f.hub.BroadcastMessageEdited(nil, "77", "88", "x", 1, nil)
	f.hub.BroadcastMessageDeleted([]string{""}, "77", "88")
}

// hasEventType 事件环里是否存在指定类型。
func hasEventType(events []Event, typ string) bool {
	for _, ev := range events {
		if ev.Type == typ {
			return true
		}
	}
	return false
}
