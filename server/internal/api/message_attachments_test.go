// 消息引用附件（#41 B3）：REST 发送/视图/软删/nonce 重放与附件校验。
package api

import (
	"context"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"testing"

	"github.com/OpenALC/LiteTalk/server/internal/store"
)

// seedMessageAttachment 直接写内存 store 的附件元数据（跳过上传链路，
// 测试聚焦消息关联与视图）。
func seedMessageAttachment(t *testing.T, mem *store.Mem, uploaderID int64, name string, thumb bool) *store.Attachment {
	t.Helper()
	a := &store.Attachment{
		UploaderID:  uploaderID,
		Filename:    name,
		ContentType: "image/png",
		Size:        1234,
		Width:       320,
		Height:      200,
		StorageKey:  "storage-" + name,
		AccessKey:   "access-" + name,
		SHA256:      strings.Repeat("0", 64),
	}
	if thumb {
		a.ThumbKey = "thumb-" + name
	}
	if err := mem.CreateAttachment(context.Background(), a); err != nil {
		t.Fatalf("CreateAttachment(%s): %v", name, err)
	}
	return a
}

type messageAttachmentFixture struct {
	mem    *store.Mem
	h      http.Handler
	access string
	uid    int64
	chID   int64
}

func newMessageAttachmentFixture(t *testing.T) *messageAttachmentFixture {
	t.Helper()
	srv, mem := newTestServer(t)
	h := srv.Handler()
	access, _ := registerUser(t, h, mem, "msgatt@ex.com", "msgatt", "password123")
	uid := asID(getJSON(t, h, "/api/v1/users/me", access, http.StatusOK).(map[string]any)["id"])
	srvResp := postJSON(t, h, "/api/v1/servers", access, map[string]string{
		"name": "附件消息服", "icon_name": "music", "icon_color": "#4FA4F3",
	}, http.StatusCreated)
	chResp := postJSON(t, h, fmt.Sprintf("/api/v1/servers/%d/channels", asID(srvResp["id"])), access,
		map[string]string{"name": "附件频道", "type": "text"}, http.StatusCreated)
	return &messageAttachmentFixture{mem: mem, h: h, access: access, uid: uid, chID: asID(chResp["id"])}
}

// send 发一条消息（body 可带 attachment_ids），按 wantStatus 断言。
func (f *messageAttachmentFixture) send(t *testing.T, body map[string]any, wantStatus int) map[string]any {
	t.Helper()
	return postJSON(t, f.h, fmt.Sprintf("/api/v1/channels/%d/messages", f.chID), f.access, body, wantStatus)
}

func idStrings(ids ...int64) []string {
	out := make([]string, 0, len(ids))
	for _, id := range ids {
		out = append(out, strconv.FormatInt(id, 10))
	}
	return out
}

func attachmentList(t *testing.T, msg map[string]any) []any {
	t.Helper()
	raw, ok := msg["attachments"]
	if !ok {
		return nil
	}
	list, ok := raw.([]any)
	if !ok {
		t.Fatalf("attachments 不是数组: %T", raw)
	}
	return list
}

// TestCreateMessageWithAttachments 新建消息引用 1–2 个附件：201 视图含附件元数据、
// 能力 URL、缩略图 URL，且附件在库里被认领到该消息。
func TestCreateMessageWithAttachments(t *testing.T) {
	f := newMessageAttachmentFixture(t)
	a1 := seedMessageAttachment(t, f.mem, f.uid, "photo-1.png", true)
	a2 := seedMessageAttachment(t, f.mem, f.uid, "photo-2.png", false)

	resp := f.send(t, map[string]any{
		"content":        "看这两张",
		"attachment_ids": idStrings(a1.ID, a2.ID),
	}, http.StatusCreated)
	if resp["content"] != "看这两张" {
		t.Fatalf("content = %v", resp["content"])
	}
	list := attachmentList(t, resp)
	if len(list) != 2 {
		t.Fatalf("attachments 应 2 个，实际 %d: %v", len(list), resp)
	}
	first := list[0].(map[string]any)
	second := list[1].(map[string]any)
	if asID(first["id"]) != a1.ID || asID(second["id"]) != a2.ID {
		t.Fatalf("附件应按 ID 升序：%v", list)
	}
	if first["filename"] != "photo-1.png" || first["content_type"] != "image/png" ||
		int(first["size"].(float64)) != 1234 || int(first["width"].(float64)) != 320 ||
		int(first["height"].(float64)) != 200 {
		t.Fatalf("附件字段不符: %v", first)
	}
	wantURL := fmt.Sprintf("/api/v1/attachments/%d?key=access-photo-1.png", a1.ID)
	if first["url"] != wantURL {
		t.Fatalf("url = %v，期望 %s", first["url"], wantURL)
	}
	wantThumb := fmt.Sprintf("/api/v1/attachments/%d/thumbnail?key=access-photo-1.png", a1.ID)
	if first["thumbnail_url"] != wantThumb {
		t.Fatalf("thumbnail_url = %v，期望 %s", first["thumbnail_url"], wantThumb)
	}
	if _, ok := second["thumbnail_url"]; ok {
		t.Fatalf("无缩略图的附件不应带 thumbnail_url: %v", second)
	}

	// 附件已认领到该消息。
	for _, a := range []*store.Attachment{a1, a2} {
		got, err := f.mem.AttachmentByID(context.Background(), a.ID)
		if err != nil {
			t.Fatalf("AttachmentByID(%d): %v", a.ID, err)
		}
		if got.MessageID != asID(resp["id"]) {
			t.Fatalf("附件 %d 认领消息 = %d，期望 %d", a.ID, got.MessageID, asID(resp["id"]))
		}
	}
}

// TestCreateMessageAttachmentOnly 纯附件消息：带附件时 content 允许空串；
// 无附件的空内容仍被拒（bad_content）。
func TestCreateMessageAttachmentOnly(t *testing.T) {
	f := newMessageAttachmentFixture(t)
	a := seedMessageAttachment(t, f.mem, f.uid, "only.png", false)

	resp := f.send(t, map[string]any{
		"content":        "",
		"attachment_ids": idStrings(a.ID),
	}, http.StatusCreated)
	if resp["content"] != "" {
		t.Fatalf("纯附件消息 content 应为空串，实际 %v", resp["content"])
	}
	if list := attachmentList(t, resp); len(list) != 1 {
		t.Fatalf("纯附件消息应含 1 个附件，实际 %v", resp)
	}

	body := f.send(t, map[string]any{"content": ""}, http.StatusBadRequest)
	if body["code"] != "bad_content" {
		t.Fatalf("无附件空内容 code = %v", body["code"])
	}
}

// TestCreateMessageAttachmentValidation 附件校验：超限/重复/非法 ID/不存在/
// 非本人上传/已被引用一律 400 bad_attachment，且不落库。
func TestCreateMessageAttachmentValidation(t *testing.T) {
	f := newMessageAttachmentFixture(t)
	own := seedMessageAttachment(t, f.mem, f.uid, "own.png", false)
	foreign := seedMessageAttachment(t, f.mem, f.uid+1, "foreign.png", false)
	claimed := seedMessageAttachment(t, f.mem, f.uid, "claimed.png", false)

	// 先正常引用 claimed，制造「已被引用」状态。
	f.send(t, map[string]any{"content": "认领", "attachment_ids": idStrings(claimed.ID)}, http.StatusCreated)

	eleven := make([]string, 11)
	for i := range eleven {
		eleven[i] = strconv.Itoa(900000 + i)
	}
	cases := []struct {
		name string
		ids  []string
	}{
		{"超过 10 个", eleven},
		{"重复", idStrings(own.ID, own.ID)},
		{"非正 ID", []string{"0", "-1"}},
		{"无法解析", []string{"abc"}},
		{"不存在", []string{"999999999"}},
		{"非本人上传", idStrings(foreign.ID)},
		{"已被引用", idStrings(claimed.ID)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			body := f.send(t, map[string]any{"content": "附件不合法", "attachment_ids": tc.ids}, http.StatusBadRequest)
			if body["code"] != "bad_attachment" {
				t.Fatalf("code = %v，期望 bad_attachment", body["code"])
			}
		})
	}

	// 全部失败请求都不应落库：只有先前认领的那一条。
	msgs, err := f.mem.MessagesBefore(context.Background(), f.chID, 0, 10)
	if err != nil {
		t.Fatalf("MessagesBefore: %v", err)
	}
	if len(msgs) != 1 {
		t.Fatalf("失败请求不应落库，实际 %d 条: %+v", len(msgs), msgs)
	}
}

// TestMessageViewsIncludeAttachments 列表批量装配附件：有附件的消息带 attachments，
// 无附件的消息缺省；软删后列表与单条都不再返回附件。
func TestMessageViewsIncludeAttachments(t *testing.T) {
	f := newMessageAttachmentFixture(t)
	a1 := seedMessageAttachment(t, f.mem, f.uid, "list-1.png", false)
	a2 := seedMessageAttachment(t, f.mem, f.uid, "list-2.png", false)
	m1 := asID(f.send(t, map[string]any{"content": "第一条", "attachment_ids": idStrings(a1.ID)}, http.StatusCreated)["id"])
	asID(f.send(t, map[string]any{"content": "第二条"}, http.StatusCreated)["id"])
	m3 := asID(f.send(t, map[string]any{"content": "第三条", "attachment_ids": idStrings(a2.ID)}, http.StatusCreated)["id"])

	list := getJSON(t, f.h, fmt.Sprintf("/api/v1/channels/%d/messages", f.chID), f.access, http.StatusOK).([]any)
	if len(list) != 3 {
		t.Fatalf("列表应 3 条，实际 %d", len(list))
	}
	byID := make(map[int64]map[string]any, len(list))
	for _, it := range list {
		msg := it.(map[string]any)
		byID[asID(msg["id"])] = msg
	}
	if atts := attachmentList(t, byID[m1]); len(atts) != 1 || asID(atts[0].(map[string]any)["id"]) != a1.ID {
		t.Fatalf("m1 attachments 不符: %v", byID[m1])
	}
	if _, ok := byID[m3]["attachments"]; !ok {
		t.Fatalf("m3 应带 attachments: %v", byID[m3])
	}
	for _, it := range list {
		msg := it.(map[string]any)
		if asID(msg["id"]) != m1 && asID(msg["id"]) != m3 {
			if _, ok := msg["attachments"]; ok {
				t.Fatalf("无附件消息不应带 attachments: %v", msg)
			}
		}
	}

	// 软删 m1：列表与单条都不返回附件（即使库里附件行仍在）。
	delJSON(t, f.h, fmt.Sprintf("/api/v1/channels/%d/messages/%d", f.chID, m1), f.access, http.StatusNoContent)
	single := getJSON(t, f.h, fmt.Sprintf("/api/v1/channels/%d/messages/%d", f.chID, m1), f.access, http.StatusOK).(map[string]any)
	if _, ok := single["attachments"]; ok {
		t.Fatalf("软删消息不应返回 attachments: %v", single)
	}
	after := getJSON(t, f.h, fmt.Sprintf("/api/v1/channels/%d/messages", f.chID), f.access, http.StatusOK).([]any)
	for _, it := range after {
		msg := it.(map[string]any)
		if asID(msg["id"]) == m1 {
			if _, ok := msg["attachments"]; ok {
				t.Fatalf("软删消息在列表里不应返回 attachments: %v", msg)
			}
		}
	}
}

// TestCreateMessageNonceReplayKeepsAttachments nonce 重放回原消息（含原附件）；
// 重放请求里的附件不得被二次认领，原消息为准。
func TestCreateMessageNonceReplayKeepsAttachments(t *testing.T) {
	f := newMessageAttachmentFixture(t)
	a1 := seedMessageAttachment(t, f.mem, f.uid, "nonce-1.png", false)
	a2 := seedMessageAttachment(t, f.mem, f.uid, "nonce-2.png", false)

	first := f.send(t, map[string]any{
		"content": "原消息", "nonce": "att-n1", "attachment_ids": idStrings(a1.ID),
	}, http.StatusCreated)
	replay := f.send(t, map[string]any{
		"content": "", "nonce": "att-n1", "attachment_ids": idStrings(a2.ID),
	}, http.StatusOK)
	if asID(replay["id"]) != asID(first["id"]) || replay["content"] != "原消息" {
		t.Fatalf("重放应回原消息: %v", replay)
	}
	list := attachmentList(t, replay)
	if len(list) != 1 || asID(list[0].(map[string]any)["id"]) != a1.ID {
		t.Fatalf("重放应回原附件: %v", replay)
	}
	// a2 未被重放请求认领：新消息可正常引用。
	f.send(t, map[string]any{"content": "第二条", "attachment_ids": idStrings(a2.ID)}, http.StatusCreated)

	msgs, err := f.mem.MessagesBefore(context.Background(), f.chID, 0, 10)
	if err != nil {
		t.Fatalf("MessagesBefore: %v", err)
	}
	if len(msgs) != 2 {
		t.Fatalf("重放不应重复落库，实际 %d 条", len(msgs))
	}
}
