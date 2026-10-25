package voice

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/OpenALC/LiteTalk/client/core/api"
	"github.com/OpenALC/LiteTalk/client/core/signal"
)

// TestSendChatWithAttachmentsCarriesIDs chat.send 上行携带 attachment_ids；
// content 可为空（纯附件消息），回复/提及仍按各自字段下发。
// SendChatWithMeta 保持兼容：不带附件时 attachment_ids 缺省。
func TestSendChatWithAttachmentsCarriesIDs(t *testing.T) {
	stub := newChatWireStub(t, serverHelloV2())
	s := newWireSession(t, stub)

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) && s.ProtocolVersion() < 2 {
		time.Sleep(10 * time.Millisecond)
	}
	if s.ProtocolVersion() < 2 {
		t.Fatalf("协议未协商到 v2（protoVer=%d）", s.ProtocolVersion())
	}

	if err := s.SendChatWithAttachments("ch1", "", "m0", []string{"u2"}, []string{"501", "502"}); err != nil {
		t.Fatalf("SendChatWithAttachments: %v", err)
	}
	got := stub.waitChatSends(t, 1)[0]
	if got.ChannelID != "ch1" || got.Type != "text" || got.Content != "" {
		t.Fatalf("基础字段错误: %+v", got)
	}
	if got.ReplyTo != "m0" || len(got.Mentions) != 1 || got.Mentions[0] != "u2" {
		t.Fatalf("回复/提及错误: %+v", got)
	}
	if len(got.AttachmentIDS) != 2 || got.AttachmentIDS[0] != "501" || got.AttachmentIDS[1] != "502" {
		t.Fatalf("attachment_ids 错误: %+v", got.AttachmentIDS)
	}

	// 兼容路径：SendChatWithMeta 不带附件字段。
	if err := s.SendChatWithMeta("ch1", "普通", "", nil); err != nil {
		t.Fatalf("SendChatWithMeta: %v", err)
	}
	sends := stub.waitChatSends(t, 2)
	if len(sends[1].AttachmentIDS) != 0 {
		t.Fatalf("无附件消息不应带 attachment_ids: %+v", sends[1].AttachmentIDS)
	}
}

// TestSendChatWithAttachmentsOfflineQueue 断线窗口入队的附件 ID 随重连重放
// 原样带回（弱网下附件消息不能掉附件）；入队时复制切片，调用方后续修改不影响。
func TestSendChatWithAttachmentsOfflineQueue(t *testing.T) {
	stub := newChatWireStub(t, serverHelloV2())
	s := NewSession(api.NewAPIClient(stub.wsURL()), "tok", "n", Events{}, nil)
	s.ws = signal.NewClient(signal.Config{
		URL:          stub.wsURL(),
		ReconnectMin: 20 * time.Millisecond,
		ReconnectMax: 100 * time.Millisecond,
		OnConnect:    s.handleSignalConnect,
	})
	s.registerHandlers()
	// 不启动 Run：模拟断线窗口，消息连同附件 ID 入队。
	ids := []string{"501", "502"}
	if err := s.SendChatWithAttachments("ch1", "", "m0", []string{"u2"}, ids); err != nil {
		t.Fatalf("断线时 SendChatWithAttachments 应入队: %v", err)
	}
	ids[0] = "mutated" // 入队快照不应受影响
	s.chatMu.Lock()
	if len(s.pendingChats) != 1 {
		n := len(s.pendingChats)
		s.chatMu.Unlock()
		t.Fatalf("待发队列应有 1 条，实际 %d", n)
	}
	queued := s.pendingChats[0]
	s.chatMu.Unlock()
	if len(queued.attachmentIDs) != 2 || queued.attachmentIDs[0] != "501" || queued.attachmentIDs[1] != "502" {
		t.Fatalf("入队附件 ID 错误: %+v", queued.attachmentIDs)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go s.ws.Run(ctx)
	defer s.ws.Close()

	sends := stub.waitChatSends(t, 1)
	if len(sends[0].AttachmentIDS) != 2 || sends[0].AttachmentIDS[0] != "501" || sends[0].AttachmentIDS[1] != "502" {
		t.Fatalf("重放附件 ID 错误: %+v", sends[0].AttachmentIDS)
	}
	if sends[0].Content != "" || sends[0].ReplyTo != "m0" {
		t.Fatalf("重放字段错误: %+v", sends[0])
	}
}

// TestChatMessageDataFromDTOAttachments REST 补拉回灌：附件元数据随消息保留
// （相对 url 原样交给 app 层拼基址）；软删消息即使带了附件也不下发。
func TestChatMessageDataFromDTOAttachments(t *testing.T) {
	atts := []api.AttachmentDTO{{
		ID: 501, Filename: "pic.png", Size: 2048, ContentType: "image/png",
		Width: 640, Height: 480,
		URL:          "/api/v1/attachments/501?key=k",
		ThumbnailURL: "/api/v1/attachments/501/thumbnail?key=k",
	}}
	m := api.MessageDTO{Content: "", Attachments: &atts}
	d := chatMessageDataFromDTO(m)
	if len(d.Attachments) != 1 {
		t.Fatalf("附件数量 = %d，期望 1", len(d.Attachments))
	}
	got := d.Attachments[0]
	if got.ID != "501" || got.Filename != "pic.png" || got.Size != 2048 ||
		got.ContentType != "image/png" || got.Width != 640 || got.Height != 480 ||
		got.URL != "/api/v1/attachments/501?key=k" ||
		got.ThumbnailURL != "/api/v1/attachments/501/thumbnail?key=k" {
		t.Fatalf("附件映射错误: %+v", got)
	}

	deletedAt := time.Now()
	m.DeletedAt = &deletedAt
	if d := chatMessageDataFromDTO(m); len(d.Attachments) != 0 {
		t.Fatalf("软删消息不应下发附件: %+v", d.Attachments)
	}
}

// TestChatMessageAttachmentsForwarded chat.message 下行的 attachments 解码后
// 原样交给 ChatMessageHandler（分发层无需额外处理）。
func TestChatMessageAttachmentsForwarded(t *testing.T) {
	stub, _ := newDownlinkStubData(t, signal.Envelope{
		Type: signal.TypeChatMessage, TS: 1,
		Data: json.RawMessage(`{"id":"m1","channel_id":"c1","content":"","ts":123,` +
			`"user":{"id":"u2","nickname":"bob"},"attachments":[` +
			`{"id":"501","filename":"a.txt","size":12,"content_type":"text/plain",` +
			`"width":0,"height":0,"url":"/api/v1/attachments/501?key=k"}]}`),
	})
	got := make(chan signal.ChatMessageData, 1)
	s := NewSession(api.NewAPIClient(stub.wsURL()), "tok", "n", Events{}, nil)
	s.ws = signal.NewClient(signal.Config{URL: stub.wsURL(), ReconnectMin: 20 * time.Millisecond})
	defer s.ws.Close()
	s.registerHandlers()
	s.ChatMessageHandler(func(d signal.ChatMessageData) { got <- d })

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go s.ws.Run(ctx)

	select {
	case d := <-got:
		if len(d.Attachments) != 1 {
			t.Fatalf("附件数量 = %d，期望 1", len(d.Attachments))
		}
		a := d.Attachments[0]
		if a.ID != "501" || a.Filename != "a.txt" || a.Size != 12 ||
			a.ContentType != "text/plain" || a.URL != "/api/v1/attachments/501?key=k" {
			t.Fatalf("附件字段错误: %+v", a)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("未收到 chat.message")
	}
}
