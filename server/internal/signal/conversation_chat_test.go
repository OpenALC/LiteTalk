package signal

import (
	"context"
	"strconv"
	"testing"

	"github.com/OpenALC/LiteTalk/server/internal/store"
)

// #38 B3：私聊/群聊 WS 消息投递。会话成员表即权限表——成员收消息、也能发送；
// 非成员既收不到也发不进（not_found，不泄露会话存在）。服务器频道广播回归。

// TestChatConversationDelivery 私聊投递：双方都收到（含发送者回显），非成员收不到；
// 非成员发送返回 not_found；非服务器成员的会话成员同样可发。
func TestChatConversationDelivery(t *testing.T) {
	f := newChatFixture(t)
	ctx := context.Background()

	// alice(1) 与 eve(8) 的私聊：eve 不是服务器成员，验证会话权限不走服务器 authz。
	conv := &store.Channel{Scope: store.ChannelScopeDirect}
	if err := f.st.CreateConversation(ctx, conv, []int64{1, 8}); err != nil {
		t.Fatalf("CreateConversation: %v", err)
	}
	convID := strconv.FormatInt(conv.ID, 10)

	a := f.dial("1", "alice")
	e := f.dial("8", "eve")
	b := f.dial("2", "bob") // 服务器成员，但不是会话成员

	a.send(TypeChatSend, ChatSendData{ChannelID: convID, Type: "text", Content: "私聊一"})
	var firstID string
	for _, c := range []*wsClient{a, e} {
		var msg ChatMessageData
		if err := c.recvUntil(TypeChatMessage).Decode(&msg); err != nil {
			t.Fatalf("decode chat.message: %v", err)
		}
		if msg.Content != "私聊一" || msg.ChannelID != convID || msg.User.ID != "1" {
			t.Fatalf("%s 收到的私聊载荷不符: %+v", c.user, msg)
		}
		if msg.ID == "" {
			t.Fatalf("%s 私聊消息缺 id", c.user)
		}
		if firstID == "" {
			firstID = msg.ID
		} else if msg.ID != firstID {
			t.Fatalf("双方应看到同一条消息：%s vs %s", msg.ID, firstID)
		}
	}
	// bob 的事件环也不得留存该会话消息（断线续传同样不能补发）。
	// broadcastEvent 先为全部目标写环再投递，a/e 已收到即代表写环阶段完成。
	if events, _ := f.hub.evlog.Replay(defaultTestSid("2"), 0); len(events) != 0 {
		t.Fatalf("非成员事件环不应有会话消息：%+v", events)
	}

	// bob 的下一条下行必须是「非成员发送」的 not_found——若私聊广播泄漏，
	// 这里会先读到 chat.message 而失败（不依赖超时断言）。
	b.send(TypeChatSend, ChatSendData{ChannelID: convID, Type: "text", Content: "闯入"})
	denied := b.readOne()
	if denied.Type != TypeError {
		t.Fatalf("非成员应先收到 error，实际 %s %s", denied.Type, denied.Data)
	}
	var errData ErrorData
	if err := denied.Decode(&errData); err != nil || errData.Code != ErrCodeNotFound {
		t.Fatalf("非成员发送应 not_found：%+v %v", errData, err)
	}

	// 会话成员发送（eve 非服务器成员）：广播目标仍是全部成员，发送者收到回显。
	e.send(TypeChatSend, ChatSendData{ChannelID: convID, Type: "text", Content: "私聊二"})
	for _, c := range []*wsClient{e, a} {
		var msg ChatMessageData
		if err := c.recvUntil(TypeChatMessage).Decode(&msg); err != nil {
			t.Fatalf("decode chat.message: %v", err)
		}
		if msg.Content != "私聊二" || msg.User.ID != "8" {
			t.Fatalf("%s 回显的私聊载荷不符: %+v", c.user, msg)
		}
	}
	msgs, err := f.st.MessagesBefore(ctx, conv.ID, 0, 10)
	if err != nil {
		t.Fatalf("MessagesBefore: %v", err)
	}
	if len(msgs) != 2 {
		t.Fatalf("私聊应落库 2 条，实际 %d: %+v", len(msgs), msgs)
	}

	// 服务器频道回归：alice 发文字频道，服务器成员 alice/bob 都收到；
	// bob 的这条 chat.message 必须是服务器频道（私聊若泄漏会先出现）。
	chID := strconv.FormatInt(f.ch.ID, 10)
	a.send(TypeChatSend, ChatSendData{ChannelID: chID, Type: "text", Content: "服务器广播"})
	for _, c := range []*wsClient{a, b} {
		var msg ChatMessageData
		if err := c.recvUntil(TypeChatMessage).Decode(&msg); err != nil {
			t.Fatalf("decode chat.message: %v", err)
		}
		if msg.Content != "服务器广播" || msg.ChannelID != chID {
			t.Fatalf("%s 服务器频道广播不符: %+v", c.user, msg)
		}
	}
}
