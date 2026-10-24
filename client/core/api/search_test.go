package api

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// TestSearchMessagesQueryAndLimit 请求路径与查询参数：#42 的 q 含中文/空格/&
// 时必须正确编码，limit 在客户端夹到 [1,50]（<=0 按默认 20）。
func TestSearchMessagesQueryAndLimit(t *testing.T) {
	cases := []struct {
		name      string
		limit     int
		wantLimit string
	}{
		{"默认", 0, "20"},
		{"负数为默认", -5, "20"},
		{"契约内", 10, "10"},
		{"上限", 50, "50"},
		{"越界上限", 60, "50"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var gotPath, gotQ, gotLimit string
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				gotPath = r.URL.Path
				gotQ = r.URL.Query().Get("q")
				gotLimit = r.URL.Query().Get("limit")
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`[]`))
			}))
			defer srv.Close()

			c := NewAPIClient(srv.URL)
			c.SetToken("tok")
			if _, err := c.SearchMessages("  你好 & world  ", tc.limit); err != nil {
				t.Fatalf("SearchMessages: %v", err)
			}
			if gotPath != "/api/v1/search" {
				t.Fatalf("路径 = %q，期望 /api/v1/search", gotPath)
			}
			// q 先 trim：首尾空白不上送，中间的 & 必须作为值编码而非参数分隔符。
			if gotQ != "你好 & world" {
				t.Fatalf("q = %q，期望 %q", gotQ, "你好 & world")
			}
			if gotLimit != tc.wantLimit {
				t.Fatalf("limit = %q，期望 %q", gotLimit, tc.wantLimit)
			}
		})
	}
}

// TestSearchMessagesDecode 200 响应解析为 MessageDTO（ID 字符串化、附件齐全），
// 空响应体 [] 归一为非 nil 空切片。
func TestSearchMessagesDecode(t *testing.T) {
	created := time.Date(2026, 10, 25, 9, 30, 0, 0, time.UTC)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`[{"id":"9001","channel_id":"123","user_id":"7",
			"content":"你好世界","created_at":"` + created.Format(time.RFC3339Nano) + `",
			"sender_nickname":"甲","sender_tag":"0001","reply_to":"9000",
			"mentions":["8"],
			"attachments":[{"id":"501","filename":"a.txt","size":12,"content_type":"text/plain",
				"width":0,"height":0,"url":"/api/v1/attachments/501?key=k"}]}]`))
	}))
	defer srv.Close()

	c := NewAPIClient(srv.URL)
	list, err := c.SearchMessages("你好", 20)
	if err != nil {
		t.Fatalf("SearchMessages: %v", err)
	}
	if len(list) != 1 {
		t.Fatalf("条数 = %d，期望 1", len(list))
	}
	m := list[0]
	if m.ID.String() != "9001" || m.ChannelID.String() != "123" || m.UserID.String() != "7" {
		t.Fatalf("ID 解码异常: %+v", m)
	}
	if m.Content != "你好世界" || m.SenderNickname != "甲" {
		t.Fatalf("正文/昵称异常: %+v", m)
	}
	if m.ReplyTo == nil || m.ReplyTo.String() != "9000" {
		t.Fatalf("reply_to 解码异常: %v", m.ReplyTo)
	}
	if len(m.Mentions) != 1 || m.Mentions[0].String() != "8" {
		t.Fatalf("mentions 解码异常: %+v", m.Mentions)
	}
	if m.Attachments == nil || len(*m.Attachments) != 1 ||
		(*m.Attachments)[0].ID.String() != "501" || (*m.Attachments)[0].URL != "/api/v1/attachments/501?key=k" {
		t.Fatalf("附件解码异常: %+v", m.Attachments)
	}

	// 空数组：非 nil，调用方可直接 range / len，序列化到前端不会是 null。
	srvEmpty := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`[]`))
	}))
	defer srvEmpty.Close()
	c2 := NewAPIClient(srvEmpty.URL)
	empty, err := c2.SearchMessages("x", 20)
	if err != nil {
		t.Fatalf("SearchMessages 空结果: %v", err)
	}
	if empty == nil || len(empty) != 0 {
		t.Fatalf("空结果应为非 nil 空切片，得到 %#v", empty)
	}
}

// TestSearchMessagesErrors 非 200 走 decodeError：机器码与状态码保留；
// 429 的 Retry-After 在传输层解析（搜索频控 30 次/分）。
func TestSearchMessagesErrors(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/problem+json")
		w.Header().Set("Retry-After", "7")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"type":"about:blank","title":"too_many_requests","status":429,"code":"rate_limited","detail":"搜索过于频繁"}`))
	}))
	defer srv.Close()

	c := NewAPIClient(srv.URL)
	_, err := c.SearchMessages("你好", 20)
	if err == nil {
		t.Fatal("429 应返回错误")
	}
	var ae *APIError
	if !errors.As(err, &ae) {
		t.Fatalf("错误类型 = %T，期望 *APIError", err)
	}
	if ae.Status != http.StatusTooManyRequests || ae.Code != "rate_limited" || ae.Message != "搜索过于频繁" {
		t.Fatalf("错误解析异常: %+v", ae)
	}
	if ae.RetryAfter != 7 {
		t.Fatalf("RetryAfter = %d，期望 7", ae.RetryAfter)
	}
}

// TestSearchMessagesBlankQuery 空白搜索词不发请求（服务端必回 400 bad_query）。
func TestSearchMessagesBlankQuery(t *testing.T) {
	requests := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`[]`))
	}))
	defer srv.Close()

	c := NewAPIClient(srv.URL)
	out, err := c.SearchMessages("   ", 20)
	if err != nil {
		t.Fatalf("空白搜索词不应报错: %v", err)
	}
	if len(out) != 0 || out == nil {
		t.Fatalf("空白搜索词应返回非 nil 空切片，得到 %#v", out)
	}
	if requests != 0 {
		t.Fatalf("空白搜索词不应发请求，实际 %d 次", requests)
	}
}
