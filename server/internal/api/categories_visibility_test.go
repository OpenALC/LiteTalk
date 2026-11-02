package api

import (
	"fmt"
	"net/http"
	"testing"

	"github.com/OpenALC/LiteTalk/server/internal/store"
)

// #102 分类名过滤：ListChannels / GetSync 只返回至少有一个可见频道的分类，
// 与频道过滤同一口径。

// TestListChannelsHidesEmptyCategories 成员的可见频道被撤掉后：
// 分类里没有可见频道就把分类名一起隐藏；持 ManageChannels 的服主仍能看到
// 空分类（否则新分类在列表里不存在，无法往里挂频道）。
func TestListChannelsHidesEmptyCategories(t *testing.T) {
	w := newPermWorld(t)
	memberID := meID(t, w.h, w.member)

	mkCat := func(name string) int64 {
		resp := postJSON(t, w.h, fmt.Sprintf("/api/v1/servers/%d/categories", w.serverID), w.owner,
			map[string]string{"name": name}, http.StatusCreated)
		return asID(resp["id"])
	}
	mkCh := func(name string, catID int64) int64 {
		resp := postJSON(t, w.h, fmt.Sprintf("/api/v1/servers/%d/channels", w.serverID), w.owner,
			map[string]any{"name": name, "type": "text", "category_id": catID}, http.StatusCreated)
		return asID(resp["id"])
	}
	secretCat := mkCat("内鬼指挥部")
	openCat := mkCat("公开区")
	secretCh := mkCh("机密", secretCat)
	openCh := mkCh("闲聊", openCat)

	list := getJSON(t, w.h, fmt.Sprintf("/api/v1/servers/%d/channels", w.serverID), w.member,
		http.StatusOK).(map[string]any)
	if got := len(list["categories"].([]any)); got != 2 {
		t.Fatalf("初始分类数 = %d，want 2", got)
	}

	// 撤销成员对机密频道的 ViewChannel：该分类下再无可见频道。
	doJSON(t, w.h, http.MethodPut,
		fmt.Sprintf("/api/v1/channels/%d/overwrites/member/%d", secretCh, memberID), w.owner,
		map[string]string{"allow": "0", "deny": permStr(store.ViewChannel)}, http.StatusOK)

	list = getJSON(t, w.h, fmt.Sprintf("/api/v1/servers/%d/channels", w.serverID), w.member,
		http.StatusOK).(map[string]any)
	cats := list["categories"].([]any)
	if len(cats) != 1 {
		t.Fatalf("无可见频道的分类应被隐藏：分类数 = %d（%v）", len(cats), cats)
	}
	if name := cats[0].(map[string]any)["name"]; name != "公开区" {
		t.Fatalf("留下的分类应为「公开区」，实际 %v", name)
	}
	chans := list["channels"].([]any)
	if len(chans) != 1 || asID(chans[0].(map[string]any)["id"]) != openCh {
		t.Fatalf("频道过滤不符：%v", chans)
	}

	// 服主持 ManageChannels：空分类照常返回（管理视角）。
	ownerList := getJSON(t, w.h, fmt.Sprintf("/api/v1/servers/%d/channels", w.serverID), w.owner,
		http.StatusOK).(map[string]any)
	if got := len(ownerList["categories"].([]any)); got != 2 {
		t.Fatalf("服主应看到 2 个分类，实际 %d", got)
	}
}

// TestSyncHidesEmptyCategories GetSync 与 ListChannels 同一口径。
func TestSyncHidesEmptyCategories(t *testing.T) {
	w := newPermWorld(t)
	memberID := meID(t, w.h, w.member)

	cat := postJSON(t, w.h, fmt.Sprintf("/api/v1/servers/%d/categories", w.serverID), w.owner,
		map[string]string{"name": "机密区"}, http.StatusCreated)
	catID := asID(cat["id"])
	ch := postJSON(t, w.h, fmt.Sprintf("/api/v1/servers/%d/channels", w.serverID), w.owner,
		map[string]any{"name": "机密频道", "type": "text", "category_id": catID}, http.StatusCreated)
	chID := asID(ch["id"])

	// 成员初始可见。
	snap := getJSON(t, w.h, "/api/v1/sync", w.member, http.StatusOK).(map[string]any)
	srv := findSyncServer(t, snap, w.serverID)
	if got := len(srv["categories"].([]any)); got != 1 {
		t.Fatalf("初始分类数 = %d，want 1", got)
	}

	// 撤销 ViewChannel 后：频道与分类同时消失。
	doJSON(t, w.h, http.MethodPut,
		fmt.Sprintf("/api/v1/channels/%d/overwrites/member/%d", chID, memberID), w.owner,
		map[string]string{"allow": "0", "deny": permStr(store.ViewChannel)}, http.StatusOK)

	snap = getJSON(t, w.h, "/api/v1/sync", w.member, http.StatusOK).(map[string]any)
	srv = findSyncServer(t, snap, w.serverID)
	if got := len(srv["categories"].([]any)); got != 0 {
		t.Fatalf("无可见频道时分类应为空，实际 %d", got)
	}
	if got := len(srv["channels"].([]any)); got != 0 {
		t.Fatalf("无可见频道时频道应为空，实际 %d", got)
	}
}

// findSyncServer 从 /sync 快照里取出指定服务器。
func findSyncServer(t *testing.T, snap map[string]any, serverID int64) map[string]any {
	t.Helper()
	for _, it := range snap["servers"].([]any) {
		m := it.(map[string]any)
		if asID(m["id"]) == serverID {
			return m
		}
	}
	t.Fatalf("快照里没有服务器 %d", serverID)
	return nil
}
