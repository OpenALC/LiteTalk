package api

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"sync"
	"testing"
)

type recordedUnreadRequest struct {
	method string
	path   string
	body   []byte
}

// newUnreadStub 固定状态/响应体并记录最后一次请求；未读端点要求 Bearer。
func newUnreadStub(t *testing.T, status int, response string) (*httptest.Server, func() recordedUnreadRequest) {
	t.Helper()
	var mu sync.Mutex
	var last recordedUnreadRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		mu.Lock()
		last = recordedUnreadRequest{method: r.Method, path: r.URL.Path, body: body}
		mu.Unlock()
		if got := r.Header.Get("Authorization"); got != "Bearer tok" {
			t.Errorf("%s %s 未带 Bearer 令牌: %q", r.Method, r.URL.Path, got)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		if response != "" {
			_, _ = w.Write([]byte(response))
		}
	}))
	t.Cleanup(srv.Close)
	return srv, func() recordedUnreadRequest {
		mu.Lock()
		defer mu.Unlock()
		return last
	}
}

func TestListUnreads(t *testing.T) {
	t.Run("解析频道与服务器计数", func(t *testing.T) {
		srv, last := newUnreadStub(t, http.StatusOK, `{
			"channels": [
				{"channel_id": "11", "unread_count": 3, "mention_count": 1},
				{"channel_id": "12", "unread_count": 0, "mention_count": 0}
			],
			"servers": [{"server_id": "1", "unread_count": 3, "mention_count": 1}]
		}`)
		c := NewAPIClient(srv.URL)
		c.SetToken("tok")

		got, err := c.ListUnreads()
		if err != nil {
			t.Fatalf("ListUnreads: %v", err)
		}
		if r := last(); r.method != http.MethodGet || r.path != "/api/v1/unreads" {
			t.Fatalf("请求 = %s %s", r.method, r.path)
		}
		if len(got.Channels) != 2 || len(got.Servers) != 1 {
			t.Fatalf("快照长度错误: %+v", got)
		}
		if got.Channels[0].ChannelID.String() != "11" || got.Channels[0].UnreadCount != 3 ||
			got.Channels[0].MentionCount != 1 {
			t.Fatalf("频道计数错误: %+v", got.Channels[0])
		}
		// 0 值项必须保留，前端按快照整体替换时才能清掉旧计数。
		if got.Channels[1].ChannelID.String() != "12" || got.Channels[1].UnreadCount != 0 ||
			got.Channels[1].MentionCount != 0 {
			t.Fatalf("0 值频道计数错误: %+v", got.Channels[1])
		}
		if got.Servers[0].ServerID.String() != "1" || got.Servers[0].UnreadCount != 3 {
			t.Fatalf("服务器计数错误: %+v", got.Servers[0])
		}

		// #31：ID 在 wire 上必须是字符串。
		raw, err := json.Marshal(got)
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		var wire map[string]any
		if err := json.Unmarshal(raw, &wire); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		channels, _ := wire["channels"].([]any)
		if len(channels) != 2 {
			t.Fatalf("wire channels 异常: %s", raw)
		}
		if id, ok := channels[0].(map[string]any)["channel_id"].(string); !ok || id != "11" {
			t.Fatalf("channel_id 必须以字符串输出: %s", raw)
		}
	})

	t.Run("空数组非 nil", func(t *testing.T) {
		srv, _ := newUnreadStub(t, http.StatusOK, `{"channels":[],"servers":[]}`)
		c := NewAPIClient(srv.URL)
		c.SetToken("tok")

		got, err := c.ListUnreads()
		if err != nil {
			t.Fatalf("ListUnreads: %v", err)
		}
		if got.Channels == nil || len(got.Channels) != 0 {
			t.Fatalf("空 channels 应返回非 nil 空切片: %#v", got.Channels)
		}
		if got.Servers == nil || len(got.Servers) != 0 {
			t.Fatalf("空 servers 应返回非 nil 空切片: %#v", got.Servers)
		}
	})

	t.Run("缺省字段也归一为非 nil", func(t *testing.T) {
		srv, _ := newUnreadStub(t, http.StatusOK, `{}`)
		c := NewAPIClient(srv.URL)
		c.SetToken("tok")

		got, err := c.ListUnreads()
		if err != nil {
			t.Fatalf("ListUnreads: %v", err)
		}
		if got.Channels == nil || got.Servers == nil {
			t.Fatalf("缺省数组应归一为非 nil: %+v", got)
		}
	})
}

func TestMarkChannelRead(t *testing.T) {
	srv, last := newUnreadStub(t, http.StatusOK,
		`{"channel_id":"11","unread_count":0,"mention_count":0}`)
	c := NewAPIClient(srv.URL)
	c.SetToken("tok")

	got, err := c.MarkChannelRead("11", "9001")
	if err != nil {
		t.Fatalf("MarkChannelRead: %v", err)
	}
	if got.ChannelID.String() != "11" || got.UnreadCount != 0 || got.MentionCount != 0 {
		t.Fatalf("响应解析错误: %+v", got)
	}
	r := last()
	if r.method != http.MethodPut || r.path != "/api/v1/channels/11/read" {
		t.Fatalf("请求 = %s %s", r.method, r.path)
	}
	// last_message_id 是契约要求必填的字符串字段。
	if body := bodyJSON(t, r.body); !reflect.DeepEqual(body, map[string]any{"last_message_id": "9001"}) {
		t.Fatalf("请求体错误: %v", body)
	}
}

// TestMarkChannelReadRESTError 400 bad_read 必须保留机器码，供上层静默/重试用。
func TestMarkChannelReadRESTError(t *testing.T) {
	srv, _ := newUnreadStub(t, http.StatusBadRequest,
		`{"type":"about:blank","title":"bad request","status":400,"code":"bad_read","detail":"消息不属于该频道"}`)
	c := NewAPIClient(srv.URL)
	c.SetToken("tok")

	_, err := c.MarkChannelRead("11", "9001")
	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("错误类型 = %T, want *APIError: %v", err, err)
	}
	if apiErr.Status != http.StatusBadRequest || apiErr.Code != "bad_read" ||
		apiErr.Message != "消息不属于该频道" {
		t.Fatalf("APIError 字段错误: %+v", apiErr)
	}
}
