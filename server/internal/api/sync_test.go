package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestSyncSnapshotShape GET /api/v1/sync：服务器（含分类/频道）+ 好友（含在线态）
// 的聚合快照形状；空数组必须是 [] 而不是 null；对端用户视图不得带 email。
func TestSyncSnapshotShape(t *testing.T) {
	srv, mem := newTestServer(t)
	h := srv.Handler()

	aTok, _ := registerUser(t, h, mem, "sync-a@ex.com", "同步A", "password123")
	bTok, _ := registerUser(t, h, mem, "sync-b@ex.com", "同步B", "password123")
	au, err := mem.UserByEmail(t.Context(), "sync-a@ex.com")
	if err != nil {
		t.Fatalf("UserByEmail A: %v", err)
	}
	bu, err := mem.UserByEmail(t.Context(), "sync-b@ex.com")
	if err != nil {
		t.Fatalf("UserByEmail B: %v", err)
	}

	// A 建服务器 + 分类 + 频道；再建一个无频道服务器，验证空数组形状。
	created := postJSON(t, h, "/api/v1/servers", aTok, map[string]string{"name": "同步服"}, http.StatusCreated)
	sid := asID(created["id"])
	cat := postJSON(t, h, fmt.Sprintf("/api/v1/servers/%d/categories", sid), aTok,
		map[string]string{"name": "甲组"}, http.StatusCreated)
	catID := asID(cat["id"])
	ch := postJSON(t, h, fmt.Sprintf("/api/v1/servers/%d/channels", sid), aTok, map[string]any{
		"name": "闲聊", "type": "text", "category_id": fmt.Sprintf("%d", catID),
	}, http.StatusCreated)
	chID := asID(ch["id"])
	empty := postJSON(t, h, "/api/v1/servers", aTok, map[string]string{"name": "空频道服"}, http.StatusCreated)
	emptyID := asID(empty["id"])

	// A、B 成为好友，B 上线。
	postJSON(t, h, "/api/v1/friends/requests", aTok,
		map[string]string{"nickname": fmt.Sprintf("同步B#%s", bu.Tag)}, http.StatusCreated)
	rel := getJSON(t, h, "/api/v1/friends", aTok, http.StatusOK).([]any)
	if len(rel) != 1 {
		t.Fatalf("好友关系应 1 条，实际 %d", len(rel))
	}
	fid := asID(rel[0].(map[string]any)["id"])
	postJSON(t, h, fmt.Sprintf("/api/v1/friends/requests/%d/accept", fid), bTok, nil, http.StatusOK)
	if err := mem.Set(t.Context(), presenceKeyOf(bu.ID), "1", 0); err != nil {
		t.Fatalf("Set presence B: %v", err)
	}
	// B 也建一个服务器：A 的（以及 B 的）快照都不能越权带出非成员服务器。
	bSrv := postJSON(t, h, "/api/v1/servers", bTok, map[string]string{"name": "B的服"}, http.StatusCreated)
	bSid := asID(bSrv["id"])

	body := getJSON(t, h, "/api/v1/sync", aTok, http.StatusOK).(map[string]any)
	servers, ok := body["servers"].([]any)
	if !ok || servers == nil {
		t.Fatalf("servers 应为数组，实际 %T %v", body["servers"], body["servers"])
	}
	if len(servers) != 2 {
		t.Fatalf("A 应看到自己 2 个服务器，实际 %d: %v", len(servers), servers)
	}
	byID := map[int64]map[string]any{}
	for _, raw := range servers {
		sm := raw.(map[string]any)
		byID[asID(sm["id"])] = sm
		if asID(sm["id"]) == bSid {
			t.Fatalf("非成员服务器 %d 不应出现在 A 的快照里", bSid)
		}
	}
	sm := byID[sid]
	if sm == nil {
		t.Fatalf("快照缺少 A 的服务器 %d", sid)
	}
	if sm["name"] != "同步服" || sm["icon_name"] != "messages-square" || sm["owner_id"] == nil {
		t.Fatalf("服务器字段不全: %v", sm)
	}
	cats := sm["categories"].([]any)
	chans := sm["channels"].([]any)
	if len(cats) != 1 || asID(cats[0].(map[string]any)["id"]) != catID {
		t.Fatalf("分类不正确: %v", cats)
	}
	if len(chans) != 1 || asID(chans[0].(map[string]any)["id"]) != chID {
		t.Fatalf("频道不正确: %v", chans)
	}
	if got := byID[emptyID]; got == nil {
		t.Fatalf("快照缺少空频道服务器 %d", emptyID)
	} else {
		// 空服务器必须落 [] 而非 null。
		if got["categories"] == nil || got["channels"] == nil {
			t.Fatalf("空服务器的 categories/channels 应为 [] 而非 null: %v", got)
		}
		if len(got["categories"].([]any)) != 0 || len(got["channels"].([]any)) != 0 {
			t.Fatalf("空服务器不应有分类/频道: %v", got)
		}
	}

	friends := body["friends"].([]any)
	if len(friends) != 1 {
		t.Fatalf("好友应 1 条，实际 %d", len(friends))
	}
	fu := friends[0].(map[string]any)["user"].(map[string]any)
	if asID(fu["id"]) != bu.ID {
		t.Fatalf("好友对端应为 B(%d)，实际 %v", bu.ID, fu["id"])
	}
	if fu["online"] != true {
		t.Fatalf("B 已上线，online 应为 true: %v", fu)
	}
	if _, has := fu["email"]; has {
		t.Fatalf("好友用户视图不得包含 email: %v", fu)
	}
	if fu["nickname"] != "同步B" {
		t.Fatalf("昵称不对: %v", fu["nickname"])
	}

	// 原始响应体里也不得出现 email 字段（email_verified 是另一字段）。
	req := httptest.NewRequest(http.MethodGet, "/api/v1/sync", nil)
	req.Header.Set(clientVersionHeader, testClientVersion)
	req.Header.Set("Authorization", "Bearer "+aTok)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("sync 期望 200 实际 %d body=%s", rec.Code, rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), `"email":`) {
		t.Fatalf("快照响应不得包含 email: %s", rec.Body.String())
	}

	// B 的快照只含 B 自己的服务器；A 的服务器不可见。
	bbody := getJSON(t, h, "/api/v1/sync", bTok, http.StatusOK).(map[string]any)
	bServers := bbody["servers"].([]any)
	if len(bServers) != 1 || asID(bServers[0].(map[string]any)["id"]) != bSid {
		t.Fatalf("B 的快照应只含自己的服务器 %d: %v", bSid, bServers)
	}
	// B 视角好友是 A（关系行对端），同样不含 email。
	if fs := bbody["friends"].([]any); len(fs) != 1 || asID(fs[0].(map[string]any)["user"].(map[string]any)["id"]) != au.ID {
		t.Fatalf("B 的快照好友应为 A(%d): %v", au.ID, fs)
	}
}

// TestSyncUnauthorized 无令牌必须 401（路由自动套 Bearer 鉴权）。
func TestSyncUnauthorized(t *testing.T) {
	srv, _ := newTestServer(t)
	h := srv.Handler()
	getJSON(t, h, "/api/v1/sync", "", http.StatusUnauthorized)
}

// TestSyncEmptySnapshot 新用户：servers/friends 均落 [] 而非 null。
func TestSyncEmptySnapshot(t *testing.T) {
	srv, mem := newTestServer(t)
	h := srv.Handler()
	tok, _ := registerUser(t, h, mem, "sync-empty@ex.com", "空快照", "password123")

	req := httptest.NewRequest(http.MethodGet, "/api/v1/sync", nil)
	req.Header.Set(clientVersionHeader, testClientVersion)
	req.Header.Set("Authorization", "Bearer "+tok)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("期望 200 实际 %d body=%s", rec.Code, rec.Body.String())
	}
	var out struct {
		Servers []json.RawMessage `json:"servers"`
		Friends []json.RawMessage `json:"friends"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("解码: %v", err)
	}
	if out.Servers == nil || out.Friends == nil {
		t.Fatalf("servers/friends 应为 [] 而非 null: %s", rec.Body.String())
	}
	if len(out.Servers) != 0 || len(out.Friends) != 0 {
		t.Fatalf("新用户快照应为空: %s", rec.Body.String())
	}
}
