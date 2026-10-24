package api

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// TestGetMessageDecodesReplyFields #39：单条消息的编辑/软删时间、回复目标、
// 回复预览与提及全部可解码；路径与鉴权头正确。
func TestGetMessageDecodesReplyFields(t *testing.T) {
	const payload = `{
		"id": "42", "channel_id": "7", "user_id": "3",
		"content": "新正文", "created_at": "2026-10-24T10:00:00Z",
		"sender_nickname": "甲", "sender_tag": "0001",
		"edited_at": "2026-10-24T10:01:00Z",
		"reply_to": "41",
		"reply_preview": {"id": "41", "user_id": "4", "nickname": "乙", "content": "", "deleted": true},
		"mentions": ["4", "5"]
	}`
	var gotPath, gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotAuth = r.URL.Path, r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(payload))
	}))
	defer srv.Close()

	c := NewAPIClient(srv.URL)
	c.SetToken("tok")
	m, err := c.GetMessage(7, 42)
	if err != nil {
		t.Fatalf("GetMessage: %v", err)
	}
	if gotPath != "/api/v1/channels/7/messages/42" {
		t.Fatalf("路径 = %q", gotPath)
	}
	if gotAuth != "Bearer tok" {
		t.Fatalf("鉴权头 = %q", gotAuth)
	}
	if m.ID.String() != "42" || m.ChannelID.String() != "7" || m.UserID.String() != "3" {
		t.Fatalf("ID 解码异常: %+v", m)
	}
	if m.Content != "新正文" || m.SenderNickname != "甲" {
		t.Fatalf("正文/昵称异常: %+v", m)
	}
	if m.EditedAt == nil || !m.EditedAt.Equal(time.Date(2026, 10, 24, 10, 1, 0, 0, time.UTC)) {
		t.Fatalf("edited_at 解码异常: %v", m.EditedAt)
	}
	if m.DeletedAt != nil {
		t.Fatalf("未删除消息 deleted_at 应为 nil，得到 %v", m.DeletedAt)
	}
	if m.ReplyTo == nil || m.ReplyTo.String() != "41" {
		t.Fatalf("reply_to 解码异常: %v", m.ReplyTo)
	}
	if m.ReplyPreview == nil || m.ReplyPreview.ID.String() != "41" ||
		m.ReplyPreview.UserID.String() != "4" || m.ReplyPreview.Nickname != "乙" ||
		m.ReplyPreview.Content != "" || !m.ReplyPreview.Deleted {
		t.Fatalf("reply_preview 解码异常: %+v", m.ReplyPreview)
	}
	if len(m.Mentions) != 2 || m.Mentions[0].String() != "4" || m.Mentions[1].String() != "5" {
		t.Fatalf("mentions 解码异常: %+v", m.Mentions)
	}
}

// TestGetMessageDeletedStillReturned 软删消息仍返回（content 空、deleted_at 有值）。
func TestGetMessageDeletedStillReturned(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"42","channel_id":"7","user_id":"3","content":"",` +
			`"created_at":"2026-10-24T10:00:00Z","deleted_at":"2026-10-24T10:02:00Z"}`))
	}))
	defer srv.Close()

	c := NewAPIClient(srv.URL)
	m, err := c.GetMessage(7, 42)
	if err != nil {
		t.Fatalf("GetMessage: %v", err)
	}
	if m.DeletedAt == nil || !m.DeletedAt.Equal(time.Date(2026, 10, 24, 10, 2, 0, 0, time.UTC)) {
		t.Fatalf("deleted_at 解码异常: %v", m.DeletedAt)
	}
	if m.Content != "" {
		t.Fatalf("软删消息 content 应为空串，得到 %q", m.Content)
	}
}

// TestEditMessageSendsMentionsAlways #39：PATCH 体总是带 mentions——
// nil 归一为空数组（清除全部提及），不能让服务端把 null 当「保持原提及」。
func TestEditMessageSendsMentionsAlways(t *testing.T) {
	cases := []struct {
		name     string
		mentions []string
		wantLen  int
	}{
		{"清除提及 nil", nil, 0},
		{"清除提及 空数组", []string{}, 0},
		{"替换提及", []string{"4", "5"}, 2},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var gotMethod, gotPath string
			var body map[string]any
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				gotMethod, gotPath = r.Method, r.URL.Path
				raw, _ := io.ReadAll(r.Body)
				if err := json.Unmarshal(raw, &body); err != nil {
					t.Errorf("请求体非法: %v", err)
				}
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{"id":"42","channel_id":"7","user_id":"3","content":"改后",` +
					`"created_at":"2026-10-24T10:00:00Z","edited_at":"2026-10-24T10:05:00Z","mentions":["4","5"]}`))
			}))
			defer srv.Close()

			c := NewAPIClient(srv.URL)
			m, err := c.EditMessage(7, 42, "改后", tc.mentions)
			if err != nil {
				t.Fatalf("EditMessage: %v", err)
			}
			if gotMethod != http.MethodPatch || gotPath != "/api/v1/channels/7/messages/42" {
				t.Fatalf("请求 = %s %s", gotMethod, gotPath)
			}
			if body["content"] != "改后" {
				t.Fatalf("content = %v", body["content"])
			}
			ms, ok := body["mentions"].([]any)
			if !ok {
				t.Fatalf("mentions 必须是数组（null 会被服务端当成缺省），实际 %#v", body["mentions"])
			}
			if len(ms) != tc.wantLen {
				t.Fatalf("mentions 长度 = %d, want %d", len(ms), tc.wantLen)
			}
			if m.EditedAt == nil {
				t.Fatal("响应 edited_at 未解码")
			}
		})
	}
}

// TestEditMessageErrorKeepsCode PATCH 的业务错误码（message_deleted）必须保留。
func TestEditMessageErrorKeepsCode(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"code":"message_deleted","detail":"消息已删除"}`))
	}))
	defer srv.Close()

	c := NewAPIClient(srv.URL)
	_, err := c.EditMessage(7, 42, "x", nil)
	var ae *APIError
	if !errors.As(err, &ae) {
		t.Fatalf("应返回 *APIError，得到 %v", err)
	}
	if ae.Status != http.StatusNotFound || ae.Code != "message_deleted" {
		t.Fatalf("错误解码异常: %+v", ae)
	}
}

// TestDeleteMessageStatus 204 成功；404 message_deleted 走 decodeError 保留机器码。
func TestDeleteMessageStatus(t *testing.T) {
	t.Run("204", func(t *testing.T) {
		var gotMethod, gotPath string
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			gotMethod, gotPath = r.Method, r.URL.Path
			w.WriteHeader(http.StatusNoContent)
		}))
		defer srv.Close()

		c := NewAPIClient(srv.URL)
		if err := c.DeleteMessage(7, 42); err != nil {
			t.Fatalf("DeleteMessage: %v", err)
		}
		if gotMethod != http.MethodDelete || gotPath != "/api/v1/channels/7/messages/42" {
			t.Fatalf("请求 = %s %s", gotMethod, gotPath)
		}
	})

	t.Run("404 message_deleted", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"code":"message_deleted","detail":"消息已删除"}`))
		}))
		defer srv.Close()

		c := NewAPIClient(srv.URL)
		err := c.DeleteMessage(7, 42)
		var ae *APIError
		if !errors.As(err, &ae) {
			t.Fatalf("应返回 *APIError，得到 %v", err)
		}
		if ae.Status != http.StatusNotFound || ae.Code != "message_deleted" {
			t.Fatalf("错误解码异常: %+v", ae)
		}
	})
}

// TestListMentionCandidates 提及候选解码（顺序由服务端保证，客户端原样保留）。
func TestListMentionCandidates(t *testing.T) {
	var gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`[{"user_id":"4","nickname":"乙","tag":"0002"},` +
			`{"user_id":"5","nickname":"丙","tag":"0003"}]`))
	}))
	defer srv.Close()

	c := NewAPIClient(srv.URL)
	list, err := c.ListMentionCandidates(7)
	if err != nil {
		t.Fatalf("ListMentionCandidates: %v", err)
	}
	if gotPath != "/api/v1/channels/7/mention-candidates" {
		t.Fatalf("路径 = %q", gotPath)
	}
	if len(list) != 2 || list[0].UserID.String() != "4" || list[0].Nickname != "乙" ||
		list[0].Tag != "0002" || list[1].UserID.String() != "5" {
		t.Fatalf("候选解码异常: %+v", list)
	}
}
