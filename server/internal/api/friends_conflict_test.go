package api

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
)

// friendReqResult 并发好友请求的单次结果（带发起方与目标，便于定位胜者）。
type friendReqResult struct {
	status int
	body   map[string]any
	token  string
	target string
}

// postFriendRequestRaw 不走 postJSON：后者用 t.Fatalf，不能在并发 goroutine 里调用。
func postFriendRequestRaw(h http.Handler, token, nicknameTag string) friendReqResult {
	var buf bytes.Buffer
	_ = json.NewEncoder(&buf).Encode(map[string]string{"nickname": nicknameTag})
	req := httptest.NewRequest(http.MethodPost, "/api/v1/friends/requests", &buf)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(clientVersionHeader, testClientVersion)
	req.Header.Set("Authorization", "Bearer "+token)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	out := map[string]any{}
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	return friendReqResult{status: rec.Code, body: out, token: token, target: nicknameTag}
}

// userTag 读当前用户的 4 位 tag（组 昵称#tag 用）。
func userTag(t *testing.T, h http.Handler, token string) string {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/users/me", nil)
	req.Header.Set(clientVersionHeader, testClientVersion)
	req.Header.Set("Authorization", "Bearer "+token)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /users/me 期望 200 实际 %d", rec.Code)
	}
	var me map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &me)
	tag, _ := me["tag"].(string)
	if tag == "" {
		t.Fatalf("用户 tag 为空: %v", me)
	}
	return tag
}

// TestFriendRequestConcurrentNoDuplicate #76：A→B 与 B→A 并发请求只落一行，
// 冲突方得到 409；重复请求不再新增行，列表、接受后的状态都不重复。
func TestFriendRequestConcurrentNoDuplicate(t *testing.T) {
	srv, mem := newTestServer(t)
	h := srv.Handler()
	aTok, _ := registerUser(t, h, mem, "cf-a@ex.com", "并发甲", "password123")
	bTok, _ := registerUser(t, h, mem, "cf-b@ex.com", "并发乙", "password123")
	aTarget := "并发乙#" + userTag(t, h, bTok)
	bTarget := "并发甲#" + userTag(t, h, aTok)

	results := make(chan friendReqResult, 2)
	start := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); <-start; results <- postFriendRequestRaw(h, aTok, aTarget) }()
	go func() { defer wg.Done(); <-start; results <- postFriendRequestRaw(h, bTok, bTarget) }()
	close(start)
	wg.Wait()
	close(results)

	var winner friendReqResult
	created, conflicts := 0, 0
	for r := range results {
		switch r.status {
		case http.StatusCreated:
			created++
			winner = r
		case http.StatusConflict:
			conflicts++
			// 冲突可能来自 API 预检（request_exists/already_friends）或唯一索引
			// （conflict）：并发场景两者都合法，但必须有明确的业务码。
			code, _ := r.body["code"].(string)
			switch code {
			case "conflict", "request_exists", "already_friends":
			default:
				t.Fatalf("冲突响应 code 不明确: %v", r.body)
			}
		default:
			t.Fatalf("并发好友请求出现意外状态 %d: %v", r.status, r.body)
		}
	}
	if created != 1 || conflicts != 1 {
		t.Fatalf("并发反向请求应一成功一冲突，实际 201=%d 409=%d", created, conflicts)
	}
	fid := asID(winner.body["id"])

	// 双方列表都只有一行（同一 id），不因并发请求出现重复项。
	for _, tok := range []string{aTok, bTok} {
		items := getJSON(t, h, "/api/v1/friends", tok, http.StatusOK).([]any)
		if len(items) != 1 {
			t.Fatalf("好友列表应 1 条，实际 %d: %v", len(items), items)
		}
		if got := asID(items[0].(map[string]any)["id"]); got != fid {
			t.Fatalf("列表项 id=%d，期望 %d", got, fid)
		}
	}

	// 同一方向重复请求：按既有约定返回 409（request_exists），列表仍一行。
	again := postFriendRequestRaw(h, winner.token, winner.target)
	if again.status != http.StatusConflict {
		t.Fatalf("重复好友请求应 409，实际 %d: %v", again.status, again.body)
	}
	if code, _ := again.body["code"].(string); code != "request_exists" {
		t.Fatalf("重复请求 code 应为 request_exists，实际 %v: %v", code, again.body)
	}
	for _, tok := range []string{aTok, bTok} {
		if items := getJSON(t, h, "/api/v1/friends", tok, http.StatusOK).([]any); len(items) != 1 {
			t.Fatalf("重复请求后好友列表应仍为 1 条，实际 %d", len(items))
		}
	}

	// 接受后双方各一行 accepted，不产生第二条。
	recipient := aTok
	if winner.token == aTok {
		recipient = bTok
	}
	postJSON(t, h, fmt.Sprintf("/api/v1/friends/requests/%d/accept", fid), recipient, nil, http.StatusOK)
	for _, tok := range []string{aTok, bTok} {
		items := getJSON(t, h, "/api/v1/friends", tok, http.StatusOK).([]any)
		if len(items) != 1 {
			t.Fatalf("接受后好友列表应 1 条，实际 %d", len(items))
		}
		if st := items[0].(map[string]any)["status"]; st != "accepted" {
			t.Fatalf("接受后状态应为 accepted，实际 %v", st)
		}
	}
}
