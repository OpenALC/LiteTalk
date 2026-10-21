package api

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// #37 B1：频道密码/人数上限/子频道树的 REST 回归。

// channelOptsWorld 建好 owner 的服 + 语音/文字频道夹具。
type channelOptsWorld struct {
	*permWorld
	hub *fakeHub
}

func newChannelOptsWorld(t *testing.T) *channelOptsWorld {
	t.Helper()
	w := newPermWorld(t)
	h := &fakeHub{notIn: map[string]bool{}}
	w.srv.hub = h
	return &channelOptsWorld{permWorld: w, hub: h}
}

// createCh 建频道并返回响应体。
func (w *channelOptsWorld) createCh(t *testing.T, body map[string]any) map[string]any {
	t.Helper()
	resp := postJSON(t, w.h, fmt.Sprintf("/api/v1/servers/%d/channels", w.serverID), w.owner, body, http.StatusCreated)
	return resp
}

// TestChannelPasswordOptions 密码三态与 has_password；明文/哈希不回显；user_limit 边界。
func TestChannelPasswordOptions(t *testing.T) {
	w := newChannelOptsWorld(t)
	rt := w.h

	// 文字频道设密码/上限 → 400。
	doJSON(t, rt, http.MethodPost, fmt.Sprintf("/api/v1/servers/%d/channels", w.serverID), w.owner,
		map[string]any{"name": "文", "type": "text", "password": "x"}, http.StatusBadRequest)
	doJSON(t, rt, http.MethodPost, fmt.Sprintf("/api/v1/servers/%d/channels", w.serverID), w.owner,
		map[string]any{"name": "文", "type": "text", "user_limit": 5}, http.StatusBadRequest)

	// 语音频道带密码创建：has_password=true，响应无 password/hash 字段。
	const secret = "pw-secret-1"
	ch := w.createCh(t, map[string]any{"name": "锁", "type": "voice", "password": secret, "user_limit": 5})
	chID := asID(ch["id"])
	if ch["has_password"] != true {
		t.Fatalf("has_password 应 true: %v", ch)
	}
	if ch["password"] != nil || ch["password_hash"] != nil {
		t.Fatalf("响应不得回显密码/哈希: %v", ch)
	}
	if ch["user_limit"] != float64(5) {
		t.Fatalf("user_limit = %v", ch["user_limit"])
	}

	// 密码长度边界：72 字节放行、73 拒。
	ok72 := strings.Repeat("a", 72)
	w.createCh(t, map[string]any{"name": "72", "type": "voice", "password": ok72})
	doJSON(t, rt, http.MethodPost, fmt.Sprintf("/api/v1/servers/%d/channels", w.serverID), w.owner,
		map[string]any{"name": "73", "type": "voice", "password": strings.Repeat("a", 73)}, http.StatusBadRequest)

	// PATCH 设置/清除（null 与空串都清）；user_limit null 拒。
	upd := doJSON(t, rt, http.MethodPatch, fmt.Sprintf("/api/v1/channels/%d", chID), w.owner,
		map[string]any{"password": "pw-2", "user_limit": 7}, http.StatusOK)
	if upd["has_password"] != true || upd["user_limit"] != float64(7) {
		t.Fatalf("PATCH 后选项不符: %v", upd)
	}
	cleared := doJSON(t, rt, http.MethodPatch, fmt.Sprintf("/api/v1/channels/%d", chID), w.owner,
		map[string]any{"password": nil}, http.StatusOK)
	if cleared["has_password"] != false {
		t.Fatalf("null 清密码后 has_password 应 false: %v", cleared)
	}
	w.createCh(t, map[string]any{"name": "锁2", "type": "voice", "password": "x"})
	ch2 := asID(postJSON(t, rt, fmt.Sprintf("/api/v1/servers/%d/channels", w.serverID), w.owner,
		map[string]any{"name": "锁3", "type": "voice", "password": "y"}, http.StatusCreated)["id"])
	_ = ch2
	doJSON(t, rt, http.MethodPatch, fmt.Sprintf("/api/v1/channels/%d", chID), w.owner,
		map[string]any{"user_limit": nil}, http.StatusBadRequest)

	// 审计：channel.update 带 password_changed，detail 无明文。
	logs := getJSON(t, rt, fmt.Sprintf("/api/v1/servers/%d/audit-log", w.serverID), w.owner, http.StatusOK).([]any)
	sawChanged := false
	for _, raw := range logs {
		e := raw.(map[string]any)
		detail, _ := e["detail"].(string)
		if strings.Contains(detail, secret) || strings.Contains(detail, "pw-2") || strings.Contains(detail, "password_hash") {
			t.Fatalf("审计 detail 泄密: %s", detail)
		}
		if action, _ := e["action"].(string); action == "channel.update" && strings.Contains(detail, "password_changed") {
			sawChanged = true
		}
	}
	if !sawChanged {
		t.Fatalf("审计应有 password_changed 标记: %v", logs)
	}
}

// TestChannelParentOptions 父级校验矩阵、分类继承、有子频道禁改、提升顶层。
func TestChannelParentOptions(t *testing.T) {
	w := newChannelOptsWorld(t)
	rt := w.h

	cat := postJSON(t, rt, fmt.Sprintf("/api/v1/servers/%d/categories", w.serverID), w.owner,
		map[string]string{"name": "树"}, http.StatusCreated)
	catID := asID(cat["id"])
	parent := w.createCh(t, map[string]any{
		"name": "父", "type": "voice", "category_id": fmt.Sprint(catID), "user_limit": 3,
	})
	parentID := asID(parent["id"])

	// 子频道省略 category → 继承父分类。
	child := w.createCh(t, map[string]any{"name": "子", "type": "voice", "parent_id": fmt.Sprint(parentID)})
	childID := asID(child["id"])
	if child["parent_id"] == nil || asID(child["parent_id"]) != parentID {
		t.Fatalf("parent_id 落库不符: %v", child)
	}
	if child["category_id"] == nil || asID(child["category_id"]) != catID {
		t.Fatalf("子频道应继承父分类: %v", child)
	}

	// 校验矩阵：父不存在/跨服/自己/父是子频道/类型不同/分类不同 → 400 bad_parent。
	badParent := func(t *testing.T, body map[string]any) {
		t.Helper()
		resp := doJSON(t, rt, http.MethodPost, fmt.Sprintf("/api/v1/servers/%d/channels", w.serverID), w.owner,
			body, http.StatusBadRequest)
		if resp["code"] != "bad_parent" {
			t.Fatalf("应 bad_parent: %v (body=%v)", resp, body)
		}
	}
	badParent(t, map[string]any{"name": "x", "type": "voice", "parent_id": "4242424242"})
	other := postJSON(t, rt, "/api/v1/servers", w.owner, map[string]string{"name": "外服"}, http.StatusCreated)
	otherParent := w.createCh2(t, asID(other["id"]), map[string]any{"name": "外父", "type": "voice"})
	badParent(t, map[string]any{"name": "x", "type": "voice", "parent_id": fmt.Sprint(asID(otherParent["id"]))})
	badParent(t, map[string]any{"name": "x", "type": "voice", "parent_id": fmt.Sprint(childID)}) // 父是子频道
	badParent(t, map[string]any{"name": "x", "type": "text", "parent_id": fmt.Sprint(parentID)}) // 类型不同
	badParent(t, map[string]any{"name": "x", "type": "voice", "parent_id": fmt.Sprint(parentID),
		"category_id": "0"}) // 分类不同（父在 cat，显式无分类）

	// 有子频道：改分类/类型/挂父 → 409 channel_has_children。
	conflict := func(body map[string]any) map[string]any {
		t.Helper()
		return doJSON(t, rt, http.MethodPatch, fmt.Sprintf("/api/v1/channels/%d", parentID), w.owner,
			body, http.StatusConflict)
	}
	if resp := conflict(map[string]any{"category_id": nil}); resp["code"] != "channel_has_children" {
		t.Fatalf("改分类应 409: %v", resp)
	}
	if resp := conflict(map[string]any{"type": "text"}); resp["code"] != "channel_has_children" {
		t.Fatalf("改类型应 409: %v", resp)
	}
	if resp := conflict(map[string]any{"parent_id": fmt.Sprint(childID)}); resp["code"] != "channel_has_children" {
		t.Fatalf("挂父应 409: %v", resp)
	}
	// 无子频道的改名不受限。
	doJSON(t, rt, http.MethodPatch, fmt.Sprintf("/api/v1/channels/%d", parentID), w.owner,
		map[string]any{"name": "父改名"}, http.StatusOK)

	// 子频道提升顶层（null 与 0 都行）。
	up := doJSON(t, rt, http.MethodPatch, fmt.Sprintf("/api/v1/channels/%d", childID), w.owner,
		map[string]any{"parent_id": nil}, http.StatusOK)
	if up["parent_id"] != nil {
		t.Fatalf("提升顶层后 parent_id 应空: %v", up)
	}
	// 挂回 + 自己挂自己 400。
	doJSON(t, rt, http.MethodPatch, fmt.Sprintf("/api/v1/channels/%d", childID), w.owner,
		map[string]any{"parent_id": fmt.Sprint(parentID)}, http.StatusOK)
	doJSON(t, rt, http.MethodPatch, fmt.Sprintf("/api/v1/channels/%d", childID), w.owner,
		map[string]any{"parent_id": fmt.Sprint(childID)}, http.StatusBadRequest)
}

// createCh2 在指定服务器下建频道（跨服 parent 夹具用）。
func (w *channelOptsWorld) createCh2(t *testing.T, serverID int64, body map[string]any) map[string]any {
	t.Helper()
	return postJSON(t, w.h, fmt.Sprintf("/api/v1/servers/%d/channels", serverID), w.owner, body, http.StatusCreated)
}

// TestChannelDeleteCascadeKick 删父频道级联子频道行，并踢父与语音子频道（#37）；
// 删分类连频道删除时补踢（既有缺口）。
func TestChannelDeleteCascadeKick(t *testing.T) {
	w := newChannelOptsWorld(t)
	rt := w.h
	cat := postJSON(t, rt, fmt.Sprintf("/api/v1/servers/%d/categories", w.serverID), w.owner,
		map[string]string{"name": "待删"}, http.StatusCreated)
	catID := asID(cat["id"])
	parent := w.createCh(t, map[string]any{"name": "父", "type": "voice", "category_id": fmt.Sprint(catID)})
	parentID := asID(parent["id"])
	child := w.createCh(t, map[string]any{"name": "子", "type": "voice", "parent_id": fmt.Sprint(parentID)})
	childID := asID(child["id"])

	doJSON(t, rt, http.MethodDelete, fmt.Sprintf("/api/v1/channels/%d", parentID), w.owner, nil, http.StatusOK)
	// 子频道行级联（API 层 404）。
	getJSON(t, rt, fmt.Sprintf("/api/v1/channels/%d/messages", childID), w.owner, http.StatusNotFound)
	// 父与语音子频道都被踢。
	kicked := w.hub.kicked()
	if len(kicked) != 2 {
		t.Fatalf("应踢父+子两个语音房，实际 %v", kicked)
	}
	// 审计记 children 数量。
	logs := getJSON(t, rt, fmt.Sprintf("/api/v1/servers/%d/audit-log", w.serverID), w.owner, http.StatusOK).([]any)
	found := false
	for _, raw := range logs {
		e := raw.(map[string]any)
		if action, _ := e["action"].(string); action == "channel.delete" {
			if detail, _ := e["detail"].(string); strings.Contains(detail, `"children":1`) {
				found = true
			}
		}
	}
	if !found {
		t.Fatalf("channel.delete 审计应含 children 数量: %v", logs)
	}

	// 删分类（带频道）→ 语音频道补踢（#37 缺口）。
	before := len(w.hub.kicked())
	voice := w.createCh(t, map[string]any{"name": "房内有人", "type": "voice", "category_id": fmt.Sprint(catID)})
	doJSON(t, rt, http.MethodDelete, fmt.Sprintf("/api/v1/categories/%d?with_channels=1", catID), w.owner, nil, http.StatusOK)
	kicked = w.hub.kicked()
	if len(kicked) != before+1 || kicked[len(kicked)-1] != fmt.Sprint(asID(voice["id"])) {
		t.Fatalf("删分类应踢语音频道 %v（追加前 %d 条，实际 %v）", voice["id"], before, kicked)
	}
}

// TestChannelOptionsETag has_password/user_limit/parent_id 变化要改变 ETag。
func TestChannelOptionsETag(t *testing.T) {
	w := newChannelOptsWorld(t)
	rt := w.h
	ch := w.createCh(t, map[string]any{"name": "etag", "type": "voice"})
	chID := asID(ch["id"])
	path := fmt.Sprintf("/api/v1/servers/%d/channels", w.serverID)

	etagOf := func() string {
		req, _ := http.NewRequest(http.MethodGet, path, nil)
		req.Header.Set("Authorization", "Bearer "+w.owner)
		req.Header.Set(clientVersionHeader, testClientVersion)
		rec := httptest.NewRecorder()
		rt.ServeHTTP(rec, req)
		return rec.Header().Get("ETag")
	}
	base := etagOf()
	doJSON(t, rt, http.MethodPatch, fmt.Sprintf("/api/v1/channels/%d", chID), w.owner,
		map[string]any{"user_limit": 4}, http.StatusOK)
	if e := etagOf(); e == base {
		t.Fatal("user_limit 变化应改变 ETag")
	}
	base = etagOf()
	doJSON(t, rt, http.MethodPatch, fmt.Sprintf("/api/v1/channels/%d", chID), w.owner,
		map[string]any{"password": "p1"}, http.StatusOK)
	if e := etagOf(); e == base {
		t.Fatal("has_password 变化应改变 ETag")
	}
	// 换密码值但 has_password 不变：ETag 可不变（不强制）。
}
