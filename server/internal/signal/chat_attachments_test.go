// chat.send 引用附件（#41 B3）：chat.message 下发附件、校验与 nonce 幂等回显。
package signal

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"testing"

	"github.com/OpenALC/LiteTalk/server/internal/store"
)

// seedChatAttachment 直接写内存 store 的附件元数据（#41 B3 只测消息关联）。
func seedChatAttachment(t *testing.T, st *store.Mem, uploaderID int64, name string, thumb bool) *store.Attachment {
	t.Helper()
	a := &store.Attachment{
		UploaderID:  uploaderID,
		Filename:    name,
		ContentType: "image/png",
		Size:        2048,
		Width:       640,
		Height:      480,
		StorageKey:  "storage-" + name,
		AccessKey:   "access-" + name,
		SHA256:      strings.Repeat("0", 64),
	}
	if thumb {
		a.ThumbKey = "thumb-" + name
	}
	if err := st.CreateAttachment(context.Background(), a); err != nil {
		t.Fatalf("CreateAttachment(%s): %v", name, err)
	}
	return a
}

func chatAttachmentIDs(list []AttachmentInfo) []int64 {
	out := make([]int64, 0, len(list))
	for _, a := range list {
		id, err := strconv.ParseInt(a.ID, 10, 64)
		if err != nil {
			return nil
		}
		out = append(out, id)
	}
	return out
}

// TestChatSendAttachmentBroadcast 发送带附件（纯附件消息，content 为空）：
// 发送者与接收者都收到 chat.message.attachments，字段与能力 URL 正确。
func TestChatSendAttachmentBroadcast(t *testing.T) {
	f := newChatFixture(t)
	chID := strconv.FormatInt(f.ch.ID, 10)
	a1 := seedChatAttachment(t, f.st, 1, "chat-1.png", true)
	a2 := seedChatAttachment(t, f.st, 1, "chat-2.png", false)

	a := f.dial("1", "alice")
	b := f.dial("2", "bob")
	a.send(TypeChatSend, ChatSendData{
		ChannelID: chID, Type: "text", Content: "",
		AttachmentIDS: []string{strconv.FormatInt(a1.ID, 10), strconv.FormatInt(a2.ID, 10)},
	})

	for _, c := range []*wsClient{a, b} {
		var msg ChatMessageData
		if err := c.recvUntil(TypeChatMessage).Decode(&msg); err != nil {
			t.Fatalf("%s decode chat.message: %v", c.user, err)
		}
		if msg.Content != "" {
			t.Fatalf("%s 纯附件消息 content = %q", c.user, msg.Content)
		}
		if len(msg.Attachments) != 2 {
			t.Fatalf("%s attachments 应 2 个，实际 %+v", c.user, msg.Attachments)
		}
		first, second := msg.Attachments[0], msg.Attachments[1]
		if first.ID != strconv.FormatInt(a1.ID, 10) || second.ID != strconv.FormatInt(a2.ID, 10) {
			t.Fatalf("%s 附件应按 ID 升序: %+v", c.user, msg.Attachments)
		}
		if first.Filename != "chat-1.png" || first.Size != 2048 || first.ContentType != "image/png" ||
			first.Width != 640 || first.Height != 480 {
			t.Fatalf("%s 附件字段不符: %+v", c.user, first)
		}
		wantURL := fmt.Sprintf("/api/v1/attachments/%d?key=access-chat-1.png", a1.ID)
		if first.URL != wantURL {
			t.Fatalf("%s url = %q，期望 %q", c.user, first.URL, wantURL)
		}
		wantThumb := fmt.Sprintf("/api/v1/attachments/%d/thumbnail?key=access-chat-1.png", a1.ID)
		if first.ThumbnailURL != wantThumb {
			t.Fatalf("%s thumbnail_url = %q，期望 %q", c.user, first.ThumbnailURL, wantThumb)
		}
		if second.ThumbnailURL != "" {
			t.Fatalf("%s 无缩略图附件不应带 thumbnail_url: %+v", c.user, second)
		}
	}
}

// TestChatSendNoAttachmentsOmitsKey 无附件时 chat.message 不带 attachments 键（omitempty）。
func TestChatSendNoAttachmentsOmitsKey(t *testing.T) {
	f := newChatFixture(t)
	chID := strconv.FormatInt(f.ch.ID, 10)
	a := f.dial("1", "alice")
	a.send(TypeChatSend, ChatSendData{ChannelID: chID, Type: "text", Content: "无附件"})

	env := a.recvUntil(TypeChatMessage)
	var raw map[string]any
	if err := json.Unmarshal(env.Data, &raw); err != nil {
		t.Fatalf("decode raw chat.message: %v", err)
	}
	if _, ok := raw["attachments"]; ok {
		t.Fatalf("无附件时不应下发 attachments: %v", raw)
	}
}

// TestChatSendAttachmentValidation 附件校验：超限/重复/非法/不存在/非本人/
// 已被引用一律 error bad_attachment，且不落库；无附件空内容仍拒收。
func TestChatSendAttachmentValidation(t *testing.T) {
	f := newChatFixture(t)
	chID := strconv.FormatInt(f.ch.ID, 10)
	own := seedChatAttachment(t, f.st, 1, "own.png", false)
	foreign := seedChatAttachment(t, f.st, 2, "foreign.png", false)
	claimed := seedChatAttachment(t, f.st, 1, "claimed.png", false)

	a := f.dial("1", "alice")

	// 无附件 + 空内容 → bad_message（内容门槛不因附件改动而放宽）。
	a.send(TypeChatSend, ChatSendData{ChannelID: chID, Type: "text", Content: ""})
	var e ErrorData
	if err := a.recvUntil(TypeError).Decode(&e); err != nil || e.Code != ErrCodeBadMessage {
		t.Fatalf("无附件空内容应 bad_message：%+v %v", e, err)
	}

	eleven := make([]string, 11)
	for i := range eleven {
		eleven[i] = strconv.Itoa(800000 + i)
	}
	cases := []struct {
		name string
		ids  []string
	}{
		{"超过 10 个", eleven},
		{"重复", []string{strconv.FormatInt(own.ID, 10), strconv.FormatInt(own.ID, 10)}},
		{"非正 ID", []string{"0"}},
		{"无法解析", []string{"abc"}},
		{"不存在", []string{"999999999"}},
		{"非本人上传", []string{strconv.FormatInt(foreign.ID, 10)}},
	}
	for _, tc := range cases {
		a.send(TypeChatSend, ChatSendData{ChannelID: chID, Type: "text", Content: "附件不合法", AttachmentIDS: tc.ids})
		if err := a.recvUntil(TypeError).Decode(&e); err != nil || e.Code != ErrCodeBadAttachment {
			t.Fatalf("%s 应 bad_attachment：%+v %v", tc.name, e, err)
		}
	}

	// 正常认领 claimed 后，再次引用 → bad_attachment（已被引用）。
	a.send(TypeChatSend, ChatSendData{
		ChannelID: chID, Type: "text", Content: "认领",
		AttachmentIDS: []string{strconv.FormatInt(claimed.ID, 10)},
	})
	var first ChatMessageData
	if err := a.recvUntil(TypeChatMessage).Decode(&first); err != nil {
		t.Fatalf("decode 认领消息: %v", err)
	}
	a.send(TypeChatSend, ChatSendData{
		ChannelID: chID, Type: "text", Content: "重复认领",
		AttachmentIDS: []string{strconv.FormatInt(claimed.ID, 10)},
	})
	if err := a.recvUntil(TypeError).Decode(&e); err != nil || e.Code != ErrCodeBadAttachment {
		t.Fatalf("已被引用应 bad_attachment：%+v %v", e, err)
	}

	msgs, err := f.st.MessagesBefore(context.Background(), f.ch.ID, 0, 10)
	if err != nil {
		t.Fatalf("MessagesBefore: %v", err)
	}
	if len(msgs) != 1 {
		t.Fatalf("被拒消息不应落库，实际 %d 条: %+v", len(msgs), msgs)
	}
}

// TestChatSendNonceReplayKeepsAttachments nonce 幂等回显原消息附件；
// 重放请求里的附件不得被二次认领。
func TestChatSendNonceReplayKeepsAttachments(t *testing.T) {
	f := newChatFixture(t)
	chID := strconv.FormatInt(f.ch.ID, 10)
	a1 := seedChatAttachment(t, f.st, 1, "nonce-1.png", false)
	a2 := seedChatAttachment(t, f.st, 1, "nonce-2.png", false)

	a := f.dial("1", "alice")
	a.send(TypeChatSend, ChatSendData{
		ChannelID: chID, Type: "text", Content: "原消息", Nonce: "att-n1",
		AttachmentIDS: []string{strconv.FormatInt(a1.ID, 10)},
	})
	var first ChatMessageData
	if err := a.recvUntil(TypeChatMessage).Decode(&first); err != nil {
		t.Fatalf("decode 首发: %v", err)
	}
	if ids := chatAttachmentIDs(first.Attachments); len(ids) != 1 || ids[0] != a1.ID {
		t.Fatalf("首发附件不符: %+v", first.Attachments)
	}

	a.send(TypeChatSend, ChatSendData{
		ChannelID: chID, Type: "text", Content: "重放", Nonce: "att-n1",
		AttachmentIDS: []string{strconv.FormatInt(a2.ID, 10)},
	})
	var replay ChatMessageData
	if err := a.recvUntil(TypeChatMessage).Decode(&replay); err != nil {
		t.Fatalf("decode 重放: %v", err)
	}
	if replay.ID != first.ID || replay.Content != "原消息" {
		t.Fatalf("重放应回原消息：%+v（原 id=%s）", replay, first.ID)
	}
	if ids := chatAttachmentIDs(replay.Attachments); len(ids) != 1 || ids[0] != a1.ID {
		t.Fatalf("重放应回原附件: %+v", replay.Attachments)
	}

	// a2 未被重放请求认领：新消息可正常引用。
	a.send(TypeChatSend, ChatSendData{
		ChannelID: chID, Type: "text", Content: "第二条",
		AttachmentIDS: []string{strconv.FormatInt(a2.ID, 10)},
	})
	var second ChatMessageData
	if err := a.recvUntil(TypeChatMessage).Decode(&second); err != nil {
		t.Fatalf("decode 第二条: %v", err)
	}
	if ids := chatAttachmentIDs(second.Attachments); len(ids) != 1 || ids[0] != a2.ID {
		t.Fatalf("a2 应可被新消息认领: %+v", second.Attachments)
	}

	msgs, err := f.st.MessagesBefore(context.Background(), f.ch.ID, 0, 10)
	if err != nil {
		t.Fatalf("MessagesBefore: %v", err)
	}
	if len(msgs) != 2 {
		t.Fatalf("重放不应重复落库，实际 %d 条", len(msgs))
	}
}
