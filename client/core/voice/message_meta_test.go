package voice

import (
	"context"
	"encoding/json"
	"sync/atomic"
	"testing"
	"time"

	"github.com/OpenALC/LiteTalk/client/core/api"
	"github.com/OpenALC/LiteTalk/client/core/signal"
)

// TestMessageEditedForwards message.edited（#39）解码后原样转给 OnMessageEdited；
// 缺 ID 的坏包不回调。
func TestMessageEditedForwards(t *testing.T) {
	stub, _ := newDownlinkStubData(t, signal.Envelope{
		Type: signal.TypeMessageEdited, TS: 1,
		Data: json.RawMessage(`{"channel_id":"7","message_id":"42","content":"新正文",` +
			`"edited_at":1761300000000,"mentions":["4","5"]}`),
	})
	got := make(chan signal.MessageEditedData, 1)
	s := NewSession(api.NewAPIClient(stub.wsURL()), "tok", "n", Events{
		OnMessageEdited: func(d signal.MessageEditedData) { got <- d },
	}, nil)
	s.ws = signal.NewClient(signal.Config{URL: stub.wsURL(), ReconnectMin: 20 * time.Millisecond})
	defer s.ws.Close()
	s.registerHandlers()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go s.ws.Run(ctx)

	select {
	case d := <-got:
		if d.ChannelID != "7" || d.MessageID != "42" || d.Content != "新正文" ||
			d.EditedAt != 1761300000000 || len(d.Mentions) != 2 ||
			d.Mentions[0] != "4" || d.Mentions[1] != "5" {
			t.Fatalf("回调载荷错误: %+v", d)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("未收到 OnMessageEdited")
	}
}

// TestMessageDeletedForwards message.deleted（#39）解码后原样转给 OnMessageDeleted。
func TestMessageDeletedForwards(t *testing.T) {
	stub, _ := newDownlinkStubData(t, signal.Envelope{
		Type: signal.TypeMessageDeleted, TS: 1,
		Data: json.RawMessage(`{"channel_id":"7","message_id":"42"}`),
	})
	got := make(chan signal.MessageDeletedData, 1)
	s := NewSession(api.NewAPIClient(stub.wsURL()), "tok", "n", Events{
		OnMessageDeleted: func(d signal.MessageDeletedData) { got <- d },
	}, nil)
	s.ws = signal.NewClient(signal.Config{URL: stub.wsURL(), ReconnectMin: 20 * time.Millisecond})
	defer s.ws.Close()
	s.registerHandlers()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go s.ws.Run(ctx)

	select {
	case d := <-got:
		if d.ChannelID != "7" || d.MessageID != "42" {
			t.Fatalf("回调载荷错误: %+v", d)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("未收到 OnMessageDeleted")
	}
}

// TestChatMessageHandlerRichFields ChatMessageHandler（#39）收到回复目标、
// 回复预览与提及；与旧 ChatHandler 同时设置时只投递富回调一次。
func TestChatMessageHandlerRichFields(t *testing.T) {
	stub, _ := newDownlinkStubData(t, signal.Envelope{
		Type: signal.TypeChatMessage, TS: 1,
		Data: json.RawMessage(`{"id":"m1","channel_id":"c1","content":"回复",` +
			`"ts":123,"nonce":"n-1","user":{"id":"u2","nickname":"bob"},` +
			`"reply_to":"m0","reply_preview":{"id":"m0","user_id":"u3",` +
			`"nickname":"carol","content":"原文","deleted":true},"mentions":["u3","u4"]}`),
	})
	got := make(chan signal.ChatMessageData, 1)
	var legacyCalls atomic.Int32
	s := NewSession(api.NewAPIClient(stub.wsURL()), "tok", "n", Events{}, nil)
	s.ws = signal.NewClient(signal.Config{URL: stub.wsURL(), ReconnectMin: 20 * time.Millisecond})
	defer s.ws.Close()
	s.registerHandlers()
	s.ChatHandler(func(_, _, _, _, _, _, _ string) { legacyCalls.Add(1) })
	s.ChatMessageHandler(func(d signal.ChatMessageData) { got <- d })

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go s.ws.Run(ctx)

	select {
	case d := <-got:
		if d.ID != "m1" || d.ChannelID != "c1" || d.Content != "回复" || d.TS != 123 ||
			d.Nonce != "n-1" || d.User.ID != "u2" || d.User.Nickname != "bob" {
			t.Fatalf("基础字段错误: %+v", d)
		}
		if d.ReplyTo != "m0" {
			t.Fatalf("reply_to = %q", d.ReplyTo)
		}
		if d.ReplyPreview == nil || d.ReplyPreview.ID != "m0" || d.ReplyPreview.UserID != "u3" ||
			d.ReplyPreview.Nickname != "carol" || d.ReplyPreview.Content != "原文" ||
			!d.ReplyPreview.Deleted {
			t.Fatalf("reply_preview 错误: %+v", d.ReplyPreview)
		}
		if len(d.Mentions) != 2 || d.Mentions[0] != "u3" || d.Mentions[1] != "u4" {
			t.Fatalf("mentions 错误: %+v", d.Mentions)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("未收到 ChatMessageHandler 回调")
	}
	// 富回调设置时旧回调不得再收到同一条消息（双投递会让前端消息重复）。
	time.Sleep(100 * time.Millisecond)
	if n := legacyCalls.Load(); n != 0 {
		t.Fatalf("ChatHandler 被重复投递 %d 次", n)
	}
}

// TestSendChatWithMetaCarriesReplyAndMentions chat.send 上行携带 reply_to/
// mentions，且 nonce 仍正常生成（协议 v2）。
func TestSendChatWithMetaCarriesReplyAndMentions(t *testing.T) {
	stub := newChatWireStub(t, serverHelloV2())
	s := newWireSession(t, stub)

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) && s.ProtocolVersion() < 2 {
		time.Sleep(10 * time.Millisecond)
	}
	if s.ProtocolVersion() < 2 {
		t.Fatalf("协议未协商到 v2（protoVer=%d）", s.ProtocolVersion())
	}

	if err := s.SendChatWithMeta("ch1", "回复内容", "m0", []string{"u2", "u3"}); err != nil {
		t.Fatalf("SendChatWithMeta: %v", err)
	}
	got := stub.waitChatSends(t, 1)[0]
	if got.ChannelID != "ch1" || got.Type != "text" || got.Content != "回复内容" {
		t.Fatalf("chat.send 基础字段错误: %+v", got)
	}
	if got.ReplyTo != "m0" {
		t.Fatalf("reply_to = %q", got.ReplyTo)
	}
	if len(got.Mentions) != 2 || got.Mentions[0] != "u2" || got.Mentions[1] != "u3" {
		t.Fatalf("mentions = %+v", got.Mentions)
	}
	if got.Nonce == "" {
		t.Fatal("nonce 缺失")
	}
}

// TestSendChatWithMetaOfflineQueue 断线窗口入队的回复/提及随重连重放保留，
// 且入队时对传入切片做快照（调用方复用切片不影响队列内容）。
func TestSendChatWithMetaOfflineQueue(t *testing.T) {
	stub := newChatWireStub(t, serverHelloV2())
	s := NewSession(api.NewAPIClient(stub.wsURL()), "tok", "n", Events{}, nil)
	s.ws = signal.NewClient(signal.Config{
		URL:          stub.wsURL(),
		ReconnectMin: 20 * time.Millisecond,
		ReconnectMax: 100 * time.Millisecond,
		OnConnect:    s.handleSignalConnect,
	})
	s.registerHandlers()

	mentions := []string{"u2", "u3"}
	// 不启动 Run：模拟断线窗口，消息应连字段一起入队。
	if err := s.SendChatWithMeta("ch1", "离线回复", "m0", mentions); err != nil {
		t.Fatalf("断线时 SendChatWithMeta 应入队: %v", err)
	}
	mentions[0] = "mutated" // 入队快照不应受调用方后续修改影响
	s.chatMu.Lock()
	if len(s.pendingChats) != 1 {
		s.chatMu.Unlock()
		t.Fatalf("待发队列应有 1 条，实际 %d", len(s.pendingChats))
	}
	queued := s.pendingChats[0]
	s.chatMu.Unlock()
	if queued.replyTo != "m0" || len(queued.mentions) != 2 || queued.mentions[0] != "u2" {
		t.Fatalf("入队字段错误: %+v", queued)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go s.ws.Run(ctx)
	defer s.ws.Close()

	sends := stub.waitChatSends(t, 1)
	if sends[0].ReplyTo != "m0" || len(sends[0].Mentions) != 2 || sends[0].Mentions[0] != "u2" {
		t.Fatalf("重放字段错误: %+v", sends[0])
	}
	if sends[0].Content != "离线回复" || sends[0].Nonce != queued.nonce {
		t.Fatalf("重放内容/nonce 错误: %+v", sends[0])
	}
	// 只重发一次。
	time.Sleep(200 * time.Millisecond)
	if n := len(stub.snapshot()); n != 1 {
		t.Fatalf("积压消息应只重发一次，实际 %d 条", n)
	}
}
