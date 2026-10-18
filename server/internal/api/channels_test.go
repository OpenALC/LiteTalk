package api

import (
	"fmt"
	"net/http"
	"testing"
)

// TestCreateChannelCategory 新建频道显式归入分类（#72）：category_id 必须落库并
// 在 ListChannels 里原样回来，否则归组只能等重启才「对上」。0 归一成无分类；
// 跨服 / 悬空分类 id 一律拒绝（防止挂到别的服务器的分类下）。
func TestCreateChannelCategory(t *testing.T) {
	srv, mem := newTestServer(t)
	h := srv.Handler()
	aAccess, _ := registerUser(t, h, mem, "cat-a@ex.com", "catA", "password123")

	mk := func(name string) int64 {
		resp := postJSON(t, h, "/api/v1/servers", aAccess, map[string]string{
			"name": name, "icon_name": "music", "icon_color": "#4FA4F3",
		}, http.StatusCreated)
		return int64(resp["id"].(float64))
	}
	s1 := mk("服一")
	s2 := mk("服二")

	catResp := postJSON(t, h, fmt.Sprintf("/api/v1/servers/%d/categories", s1), aAccess,
		map[string]string{"name": "甲组"}, http.StatusCreated)
	catID := int64(catResp["id"].(float64))
	cat2Resp := postJSON(t, h, fmt.Sprintf("/api/v1/servers/%d/categories", s2), aAccess,
		map[string]string{"name": "乙组"}, http.StatusCreated)
	cat2ID := int64(cat2Resp["id"].(float64))

	// 显式归入分类：落库后 ListChannels 原样带回。
	postJSON(t, h, fmt.Sprintf("/api/v1/servers/%d/channels", s1), aAccess,
		map[string]any{"name": "开黑", "type": "voice", "category_id": catID}, http.StatusCreated)
	list := getJSON(t, h, fmt.Sprintf("/api/v1/servers/%d/channels", s1), aAccess, http.StatusOK).(map[string]any)
	chans := list["channels"].([]any)
	if len(chans) != 1 {
		t.Fatalf("频道数应为 1，实际 %d", len(chans))
	}
	got := chans[0].(map[string]any)
	if got["category_id"] != float64(catID) {
		t.Fatalf("category_id 应为 %d，实际 %v", catID, got["category_id"])
	}

	// 0 = 显式无分类：归一成 NULL，不能落 0（与 PATCH 同语义）。
	postJSON(t, h, fmt.Sprintf("/api/v1/servers/%d/channels", s1), aAccess,
		map[string]any{"name": "散装", "type": "text", "category_id": 0}, http.StatusCreated)
	list = getJSON(t, h, fmt.Sprintf("/api/v1/servers/%d/channels", s1), aAccess, http.StatusOK).(map[string]any)
	for _, it := range list["channels"].([]any) {
		m := it.(map[string]any)
		if m["name"] == "散装" && m["category_id"] != nil {
			t.Fatalf("category_id=0 应落 NULL，实际 %v", m["category_id"])
		}
	}

	// 缺省（不带字段）同样无分类。
	postJSON(t, h, fmt.Sprintf("/api/v1/servers/%d/channels", s1), aAccess,
		map[string]any{"name": "野队", "type": "voice"}, http.StatusCreated)

	// 跨服分类：拒绝，不能挂到别的服务器的分类下。
	postJSON(t, h, fmt.Sprintf("/api/v1/servers/%d/channels", s1), aAccess,
		map[string]any{"name": "越界", "type": "voice", "category_id": cat2ID}, http.StatusBadRequest)
	// 悬空分类：拒绝。
	postJSON(t, h, fmt.Sprintf("/api/v1/servers/%d/channels", s1), aAccess,
		map[string]any{"name": "幽灵", "type": "voice", "category_id": 999999}, http.StatusBadRequest)

	// 被拒绝的两条不应落库。
	list = getJSON(t, h, fmt.Sprintf("/api/v1/servers/%d/channels", s1), aAccess, http.StatusOK).(map[string]any)
	if n := len(list["channels"].([]any)); n != 3 {
		t.Fatalf("落库频道数应为 3，实际 %d", n)
	}
}
