package api

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/OpenALC/LiteTalk/server/internal/store"
)

// #42 B1：GET /api/v1/search 的可见性、形状、校验与频控。

// fetchSearch 请求搜索并返回消息对象数组；同时断言空结果是 [] 而不是 null。
func fetchSearch(t *testing.T, h http.Handler, token, query string) []map[string]any {
	t.Helper()
	body := getJSON(t, h, "/api/v1/search?q="+url.QueryEscape(query), token, http.StatusOK)
	arr, ok := body.([]any)
	if !ok || arr == nil {
		t.Fatalf("搜索结果应为非 null 数组，实际 %#v", body)
	}
	out := make([]map[string]any, 0, len(arr))
	for _, raw := range arr {
		obj, ok := raw.(map[string]any)
		if !ok {
			t.Fatalf("搜索结果元素不是对象: %#v", raw)
		}
		out = append(out, obj)
	}
	return out
}

// searchIDSet 结果 ID 集合；顺带断言无重复。
func searchIDSet(t *testing.T, msgs []map[string]any) map[int64]map[string]any {
	t.Helper()
	out := make(map[int64]map[string]any, len(msgs))
	for _, msg := range msgs {
		id := asID(msg["id"])
		if id == 0 {
			t.Fatalf("搜索结果缺少 id: %#v", msg)
		}
		if _, dup := out[id]; dup {
			t.Fatalf("搜索结果重复 id=%d", id)
		}
		out[id] = msg
	}
	return out
}

func assertSearchSet(t *testing.T, msgs []map[string]any, want ...int64) {
	t.Helper()
	got := searchIDSet(t, msgs)
	if len(got) != len(want) {
		t.Fatalf("命中 %d 条，期望 %d 条：%v", len(got), len(want), got)
	}
	for _, id := range want {
		if _, ok := got[id]; !ok {
			t.Fatalf("结果缺少消息 %d：%v", id, got)
		}
	}
}

// TestSearchMessagesVisibility 只返回可见频道；服务器频道、私聊、非成员隔离、
// 软删排除与附件装配。
func TestSearchMessagesVisibility(t *testing.T) {
	w := newConvWorld(t)
	h := w.h
	ctx := context.Background()

	// Alice 建服与频道，Bob 加入；Carol 不是该服成员。
	serverID := asID(postJSON(t, h, "/api/v1/servers", w.alice,
		map[string]string{"name": "搜索服", "icon_name": "music", "icon_color": "#4FA4F3"},
		http.StatusCreated)["id"])
	chID := asID(postJSON(t, h, fmt.Sprintf("/api/v1/servers/%d/channels", serverID), w.alice,
		map[string]string{"name": "搜索频道", "type": "text"}, http.StatusCreated)["id"])
	if err := w.mem.AddMember(ctx, &store.ServerMember{
		ServerID: serverID, UserID: w.bobID, Role: store.RoleMember,
	}); err != nil {
		t.Fatalf("AddMember(bob): %v", err)
	}
	w.srv.authz.InvalidateServer(serverID)

	aliceMsg := sendMsg(t, h, w.alice, chID, "共同关键词 alpha")
	bobMsg := sendMsg(t, h, w.bob, chID, "共同关键词 beta")
	// 预置附件元数据：搜索结果的 Message 视图应带 attachments（#41 复用装配）。
	if err := w.mem.CreateAttachment(ctx, &store.Attachment{
		UploaderID: w.aliceID, MessageID: aliceMsg, Filename: "图.png",
		ContentType: "image/png", Size: 12, Width: 2, Height: 3,
		StorageKey: "sk", AccessKey: strings.Repeat("a", 64), SHA256: strings.Repeat("b", 64),
	}); err != nil {
		t.Fatalf("CreateAttachment: %v", err)
	}

	// Carol 自建服务器与频道：Alice/Bob 都看不到。
	carolServer := asID(postJSON(t, h, "/api/v1/servers", w.carol,
		map[string]string{"name": "Carol 服", "icon_name": "music", "icon_color": "#4FA4F3"},
		http.StatusCreated)["id"])
	carolCh := asID(postJSON(t, h, fmt.Sprintf("/api/v1/servers/%d/channels", carolServer), w.carol,
		map[string]string{"name": "Carol 频道", "type": "text"}, http.StatusCreated)["id"])
	carolMsg := sendMsg(t, h, w.carol, carolCh, "共同关键词 gamma")

	// Alice 与 Bob 的私聊：Carol 不是成员。
	convID := asID(postJSON(t, h, "/api/v1/conversations", w.alice,
		map[string]any{"kind": "direct", "user_ids": uidList(w.bobID)}, http.StatusCreated)["id"])
	dmMsg := sendMsg(t, h, w.bob, convID, "私聊关键词 delta")

	// Alice：服务器频道两条 + 私聊一条，Carol 频道不可见。
	assertSearchSet(t, fetchSearch(t, h, w.alice, "共同关键词"), aliceMsg, bobMsg)
	assertSearchSet(t, fetchSearch(t, h, w.alice, "私聊关键词"), dmMsg)
	// Bob 是服务器与私聊成员，结果同 Alice。
	assertSearchSet(t, fetchSearch(t, h, w.bob, "共同关键词"), aliceMsg, bobMsg)
	assertSearchSet(t, fetchSearch(t, h, w.bob, "私聊关键词"), dmMsg)
	// Carol 只看得到自己服务器里的消息；私聊非成员不泄露。
	assertSearchSet(t, fetchSearch(t, h, w.carol, "共同关键词"), carolMsg)
	assertSearchSet(t, fetchSearch(t, h, w.carol, "私聊关键词"))
	// Erin 与 Dave 没有任何可见频道。
	assertSearchSet(t, fetchSearch(t, h, w.erin, "共同关键词"))
	assertSearchSet(t, fetchSearch(t, h, w.erin, "私聊关键词"))

	// 形状：发送者内联、无邮箱；附件带 url；软删前的正文可见。
	res := searchIDSet(t, fetchSearch(t, h, w.alice, "共同关键词"))
	aliceView := res[aliceMsg]
	if aliceView == nil {
		t.Fatalf("缺少 Alice 的消息: %v", res)
	}
	if aliceView["sender_nickname"] != "cvAlice" || aliceView["sender_tag"] != "0001" {
		t.Fatalf("发送者内联信息不符: %v", aliceView)
	}
	if _, leak := aliceView["email"]; leak {
		t.Fatalf("搜索结果泄露 email: %v", aliceView)
	}
	atts, ok := aliceView["attachments"].([]any)
	if !ok || len(atts) != 1 {
		t.Fatalf("附件应装配到搜索结果: %#v", aliceView["attachments"])
	}
	att := atts[0].(map[string]any)
	if att["filename"] != "图.png" || att["url"] == "" {
		t.Fatalf("附件字段不符: %#v", att)
	}

	// 软删后不再命中。
	delJSON(t, h, fmt.Sprintf("/api/v1/channels/%d/messages/%d", chID, bobMsg),
		w.bob, http.StatusNoContent)
	assertSearchSet(t, fetchSearch(t, h, w.alice, "共同关键词"), aliceMsg)

	// 无匹配 → 空数组（fetchSearch 已断言非 null）。
	assertSearchSet(t, fetchSearch(t, h, w.alice, "不存在的关键词"))

	// 空白搜索词先 trim：前后空格不影响结果。
	assertSearchSet(t, fetchSearch(t, h, w.alice, "  共同关键词  "), aliceMsg)

	// limit=1 取最新一条：先补一条更新的命中消息。
	newest := sendMsg(t, h, w.alice, chID, "共同关键词 omega")
	if got := fetchSearch(t, h, w.alice, "共同关键词"); len(got) != 2 {
		t.Fatalf("limit 前置断言应 2 条，实际 %d", len(got))
	}
	body := getJSON(t, h, "/api/v1/search?q="+url.QueryEscape("共同关键词")+"&limit=1",
		w.alice, http.StatusOK).([]any)
	if len(body) != 1 || asID(body[0].(map[string]any)["id"]) != newest {
		t.Fatalf("limit=1 应只返回最新一条 %d，实际 %v", newest, body)
	}
}

// TestSearchMessagesValidation 搜索词与 limit 的契约校验（fail-closed）。
func TestSearchMessagesValidation(t *testing.T) {
	srv, mem := newTestServer(t)
	h := srv.Handler()
	token, _ := addUser(t, srv, mem, "sv@ex.com", "svUser")

	cases := []struct {
		name   string
		query  string
		status int
		code   string
	}{
		{"缺少 q", "", http.StatusBadRequest, "bad_request"},
		{"空 q", "q=", http.StatusBadRequest, "bad_query"},
		{"纯空白 q", "q=" + url.QueryEscape("   "), http.StatusBadRequest, "bad_query"},
		{"超长 q", "q=" + url.QueryEscape(strings.Repeat("好", 101)), http.StatusBadRequest, "bad_query"},
		{"limit=0", "q=a&limit=0", http.StatusBadRequest, "bad_limit"},
		{"limit=51", "q=a&limit=51", http.StatusBadRequest, "bad_limit"},
	}
	for _, tc := range cases {
		path := "/api/v1/search"
		if tc.query != "" {
			path += "?" + tc.query
		}
		body := getJSON(t, h, path, token, tc.status)
		if code := body.(map[string]any)["code"]; code != tc.code {
			t.Fatalf("%s 应返回 %s，实际 %v", tc.name, tc.code, code)
		}
	}
	// 100 字符边界合法（只校验参数，空结果 200）。
	getJSON(t, h, "/api/v1/search?q="+url.QueryEscape(strings.Repeat("好", 100)),
		token, http.StatusOK)
	// 未登录 401。
	getJSON(t, h, "/api/v1/search?q=a", "", http.StatusUnauthorized)
}

// TestSearchMessagesRateLimit 30 次/分钟/用户，第 31 次 429 rate_limited。
func TestSearchMessagesRateLimit(t *testing.T) {
	srv, mem := newTestServer(t)
	h := srv.Handler()
	token, _ := addUser(t, srv, mem, "srl@ex.com", "srlUser")
	for i := 0; i < 30; i++ {
		getJSON(t, h, "/api/v1/search?q=rate", token, http.StatusOK)
	}
	body := getJSON(t, h, "/api/v1/search?q=rate", token, http.StatusTooManyRequests)
	if code := body.(map[string]any)["code"]; code != "rate_limited" {
		t.Fatalf("超限应返回 rate_limited，实际 %v", code)
	}
}
