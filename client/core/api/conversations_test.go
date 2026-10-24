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

// conversationJSON 会话响应 wire 契约：id 为字符串，成员可带数字 ID（#31 兼容），
// 不含 email。
const conversationJSON = `{
  "id": "9001", "kind": "group", "name": "开黑",
  "members": [
    {"user_id": 1, "nickname": "甲", "tag": "0001", "role": "owner", "joined_at": "2026-10-20T10:00:00Z"},
    {"user_id": "2", "nickname": "乙", "tag": "0002", "role": "member", "joined_at": "2026-10-20T10:01:00Z"}
  ],
  "created_at": "2026-10-20T10:00:00Z"
}`

type recordedConversationRequest struct {
	method string
	path   string
	body   []byte
}

// newConversationStub 返回固定状态/响应体，并记录最后一次请求（方法/路径/body）。
// 所有会话端点都要求 Bearer 令牌，桩里一并断言。
func newConversationStub(t *testing.T, status int, response string) (*httptest.Server, func() recordedConversationRequest) {
	t.Helper()
	var mu sync.Mutex
	var last recordedConversationRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		mu.Lock()
		last = recordedConversationRequest{method: r.Method, path: r.URL.Path, body: body}
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
	return srv, func() recordedConversationRequest {
		mu.Lock()
		defer mu.Unlock()
		return last
	}
}

func assertConversation(t *testing.T, c *ConversationDTO) {
	t.Helper()
	if c == nil {
		t.Fatal("会话为 nil")
	}
	if c.ID.String() != "9001" || c.Kind != "group" || c.Name != "开黑" {
		t.Fatalf("会话字段错误: %+v", c)
	}
	if len(c.Members) != 2 {
		t.Fatalf("成员数 = %d, want 2: %+v", len(c.Members), c.Members)
	}
	if c.Members[0].UserID.String() != "1" || c.Members[0].Nickname != "甲" ||
		c.Members[0].Tag != "0001" || c.Members[0].Role != "owner" {
		t.Fatalf("owner 成员字段错误: %+v", c.Members[0])
	}
	if c.Members[1].UserID.String() != "2" || c.Members[1].Role != "member" {
		t.Fatalf("member 成员字段错误: %+v", c.Members[1])
	}
	if c.CreatedAt.IsZero() || c.Members[0].JoinedAt.IsZero() {
		t.Fatalf("时间字段未解析: created=%v joined=%v", c.CreatedAt, c.Members[0].JoinedAt)
	}

	// #31：ID 在 wire 上必须是字符串（Snowflake 超出 JS Number 精度）。
	raw, err := json.Marshal(c)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var wire map[string]any
	if err := json.Unmarshal(raw, &wire); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if _, ok := wire["id"].(string); !ok {
		t.Fatalf("id 必须以字符串输出: %s", raw)
	}
	members, _ := wire["members"].([]any)
	if len(members) != 2 {
		t.Fatalf("wire members 异常: %s", raw)
	}
	if uid, ok := members[0].(map[string]any)["user_id"].(string); !ok || uid != "1" {
		t.Fatalf("user_id 必须以字符串输出: %s", raw)
	}
}

// bodyJSON 解析请求体；坏 JSON 直接判失败。
func bodyJSON(t *testing.T, raw []byte) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("请求体不是 JSON: %v (%s)", err, raw)
	}
	return m
}

// userIDs 取 user_ids 数组并归一为 []string。
func userIDs(t *testing.T, body map[string]any) []string {
	t.Helper()
	raw, ok := body["user_ids"].([]any)
	if !ok {
		t.Fatalf("user_ids 缺失或类型错误: %v", body)
	}
	out := make([]string, len(raw))
	for i, v := range raw {
		s, ok := v.(string)
		if !ok {
			t.Fatalf("user_ids[%d] 不是字符串: %v", i, v)
		}
		out[i] = s
	}
	return out
}

func TestListConversations(t *testing.T) {
	t.Run("有会话", func(t *testing.T) {
		srv, last := newConversationStub(t, http.StatusOK, "["+conversationJSON+"]")
		c := NewAPIClient(srv.URL)
		c.SetToken("tok")

		list, err := c.ListConversations()
		if err != nil {
			t.Fatalf("ListConversations: %v", err)
		}
		if len(list) != 1 {
			t.Fatalf("会话数 = %d, want 1", len(list))
		}
		assertConversation(t, &list[0])
		if r := last(); r.method != http.MethodGet || r.path != "/api/v1/conversations" {
			t.Fatalf("请求 = %s %s", r.method, r.path)
		}
	})

	t.Run("空列表非 nil", func(t *testing.T) {
		srv, _ := newConversationStub(t, http.StatusOK, "[]")
		c := NewAPIClient(srv.URL)
		c.SetToken("tok")

		list, err := c.ListConversations()
		if err != nil {
			t.Fatalf("ListConversations: %v", err)
		}
		if list == nil || len(list) != 0 {
			t.Fatalf("空列表应返回非 nil 空切片: %#v", list)
		}
	})
}

func TestCreateDirectConversation(t *testing.T) {
	// 200 = 幂等命中已有会话，201 = 新建；两者都算成功。
	for _, status := range []int{http.StatusOK, http.StatusCreated} {
		status := status
		t.Run(http.StatusText(status), func(t *testing.T) {
			srv, last := newConversationStub(t, status, conversationJSON)
			c := NewAPIClient(srv.URL)
			c.SetToken("tok")

			got, err := c.CreateDirectConversation("7")
			if err != nil {
				t.Fatalf("CreateDirectConversation: %v", err)
			}
			assertConversation(t, got)

			r := last()
			if r.method != http.MethodPost || r.path != "/api/v1/conversations" {
				t.Fatalf("请求 = %s %s", r.method, r.path)
			}
			body := bodyJSON(t, r.body)
			if body["kind"] != "direct" {
				t.Fatalf("kind = %v, want direct", body["kind"])
			}
			if ids := userIDs(t, body); !reflect.DeepEqual(ids, []string{"7"}) {
				t.Fatalf("user_ids = %v, want [7]", ids)
			}
			if _, ok := body["name"]; ok {
				t.Fatalf("direct 不应携带 name: %v", body)
			}
		})
	}
}

func TestCreateGroupConversation(t *testing.T) {
	t.Run("带名称", func(t *testing.T) {
		srv, last := newConversationStub(t, http.StatusCreated, conversationJSON)
		c := NewAPIClient(srv.URL)
		c.SetToken("tok")

		got, err := c.CreateGroupConversation("开黑", []string{"7", "8"})
		if err != nil {
			t.Fatalf("CreateGroupConversation: %v", err)
		}
		assertConversation(t, got)

		r := last()
		if r.method != http.MethodPost || r.path != "/api/v1/conversations" {
			t.Fatalf("请求 = %s %s", r.method, r.path)
		}
		body := bodyJSON(t, r.body)
		if body["kind"] != "group" || body["name"] != "开黑" {
			t.Fatalf("请求体错误: %v", body)
		}
		if ids := userIDs(t, body); !reflect.DeepEqual(ids, []string{"7", "8"}) {
			t.Fatalf("user_ids = %v, want [7 8]", ids)
		}
	})

	t.Run("空名称不发送", func(t *testing.T) {
		srv, last := newConversationStub(t, http.StatusCreated, conversationJSON)
		c := NewAPIClient(srv.URL)
		c.SetToken("tok")

		if _, err := c.CreateGroupConversation("", []string{"7", "8"}); err != nil {
			t.Fatalf("CreateGroupConversation: %v", err)
		}
		if _, ok := bodyJSON(t, last().body)["name"]; ok {
			t.Fatalf("空名称不应出现在请求体: %s", last().body)
		}
	})
}

func TestGetConversation(t *testing.T) {
	srv, last := newConversationStub(t, http.StatusOK, conversationJSON)
	c := NewAPIClient(srv.URL)
	c.SetToken("tok")

	got, err := c.GetConversation("9001")
	if err != nil {
		t.Fatalf("GetConversation: %v", err)
	}
	assertConversation(t, got)
	if r := last(); r.method != http.MethodGet || r.path != "/api/v1/conversations/9001" {
		t.Fatalf("请求 = %s %s", r.method, r.path)
	}
}

func TestRenameConversation(t *testing.T) {
	srv, last := newConversationStub(t, http.StatusOK, conversationJSON)
	c := NewAPIClient(srv.URL)
	c.SetToken("tok")

	got, err := c.RenameConversation("9001", "新名字")
	if err != nil {
		t.Fatalf("RenameConversation: %v", err)
	}
	assertConversation(t, got)

	r := last()
	if r.method != http.MethodPatch || r.path != "/api/v1/conversations/9001" {
		t.Fatalf("请求 = %s %s", r.method, r.path)
	}
	if body := bodyJSON(t, r.body); !reflect.DeepEqual(body, map[string]any{"name": "新名字"}) {
		t.Fatalf("Merge Patch 请求体错误: %v", body)
	}
}

func TestDeleteConversation(t *testing.T) {
	srv, last := newConversationStub(t, http.StatusNoContent, "")
	c := NewAPIClient(srv.URL)
	c.SetToken("tok")

	if err := c.DeleteConversation("9001"); err != nil {
		t.Fatalf("DeleteConversation: %v", err)
	}
	if r := last(); r.method != http.MethodDelete || r.path != "/api/v1/conversations/9001" {
		t.Fatalf("请求 = %s %s", r.method, r.path)
	}
}

func TestAddConversationMembers(t *testing.T) {
	srv, last := newConversationStub(t, http.StatusOK, conversationJSON)
	c := NewAPIClient(srv.URL)
	c.SetToken("tok")

	got, err := c.AddConversationMembers("9001", []string{"7", "8"})
	if err != nil {
		t.Fatalf("AddConversationMembers: %v", err)
	}
	assertConversation(t, got)

	r := last()
	if r.method != http.MethodPost || r.path != "/api/v1/conversations/9001/members" {
		t.Fatalf("请求 = %s %s", r.method, r.path)
	}
	if ids := userIDs(t, bodyJSON(t, r.body)); !reflect.DeepEqual(ids, []string{"7", "8"}) {
		t.Fatalf("user_ids = %v, want [7 8]", ids)
	}
}

func TestRemoveConversationMember(t *testing.T) {
	t.Run("200 返回更新后会话", func(t *testing.T) {
		srv, last := newConversationStub(t, http.StatusOK, conversationJSON)
		c := NewAPIClient(srv.URL)
		c.SetToken("tok")

		got, err := c.RemoveConversationMember("9001", "2")
		if err != nil {
			t.Fatalf("RemoveConversationMember: %v", err)
		}
		assertConversation(t, got)
		if r := last(); r.method != http.MethodDelete ||
			r.path != "/api/v1/conversations/9001/members/2" {
			t.Fatalf("请求 = %s %s", r.method, r.path)
		}
	})

	t.Run("204 最后一人退出", func(t *testing.T) {
		srv, last := newConversationStub(t, http.StatusNoContent, "")
		c := NewAPIClient(srv.URL)
		c.SetToken("tok")

		got, err := c.RemoveConversationMember("9001", "1")
		if err != nil {
			t.Fatalf("RemoveConversationMember: %v", err)
		}
		if got != nil {
			t.Fatalf("204 应返回 nil 会话，实际 %+v", got)
		}
		if r := last(); r.method != http.MethodDelete ||
			r.path != "/api/v1/conversations/9001/members/1" {
			t.Fatalf("请求 = %s %s", r.method, r.path)
		}
	})
}

// TestConversationRESTError 服务端 problem+json 错误要解成 *APIError，
// 供上层按 code 提示（如 direct 改名 not_group）。
func TestConversationRESTError(t *testing.T) {
	srv, _ := newConversationStub(t, http.StatusBadRequest,
		`{"type":"about:blank","title":"bad request","status":400,"code":"not_group","detail":"私聊没有名字"}`)
	c := NewAPIClient(srv.URL)
	c.SetToken("tok")

	_, err := c.RenameConversation("9001", "新名字")
	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("错误类型 = %T, want *APIError: %v", err, err)
	}
	if apiErr.Status != http.StatusBadRequest || apiErr.Code != "not_group" ||
		apiErr.Message != "私聊没有名字" {
		t.Fatalf("APIError 字段错误: %+v", apiErr)
	}
}
