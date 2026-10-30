package api

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/OpenALC/LiteTalk/server/internal/signal"
	"github.com/OpenALC/LiteTalk/server/internal/store"
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

// TestPatchChildChannelInvariants 子频道 PATCH 不得破坏父子同类型/同分类（#101 P2）：
// 父频道有子频道时的 409 拦截不变；子频道自身向父看齐放行，单方面漂移拒绝且不落库。
func TestPatchChildChannelInvariants(t *testing.T) {
	w := newChannelOptsWorld(t)
	rt := w.h
	ctx := context.Background()

	catA := asID(postJSON(t, rt, fmt.Sprintf("/api/v1/servers/%d/categories", w.serverID), w.owner,
		map[string]string{"name": "甲"}, http.StatusCreated)["id"])
	catB := asID(postJSON(t, rt, fmt.Sprintf("/api/v1/servers/%d/categories", w.serverID), w.owner,
		map[string]string{"name": "乙"}, http.StatusCreated)["id"])

	parent := w.createCh(t, map[string]any{"name": "父", "type": "voice", "category_id": fmt.Sprint(catA)})
	parentID := asID(parent["id"])
	child := w.createCh(t, map[string]any{"name": "子", "type": "voice", "parent_id": fmt.Sprint(parentID)})
	childID := asID(child["id"])

	patch := func(body map[string]any, want int) map[string]any {
		t.Helper()
		return doJSON(t, rt, http.MethodPatch, fmt.Sprintf("/api/v1/channels/%d", childID), w.owner, body, want)
	}

	// 改类型与父不同 → 400 bad_parent，库内仍是 voice。
	if resp := patch(map[string]any{"type": "text"}, http.StatusBadRequest); resp["code"] != "bad_parent" {
		t.Fatalf("子频道改类型应 bad_parent: %v", resp)
	}
	if got, err := w.mem.ChannelByID(ctx, childID); err != nil || got.Type != store.ChannelVoice {
		t.Fatalf("拒绝后类型不应落库: %+v err=%v", got, err)
	}

	// 改分类与父不同（含显式清空）→ 400 bad_parent，库内分类仍是 catA。
	if resp := patch(map[string]any{"category_id": fmt.Sprint(catB)}, http.StatusBadRequest); resp["code"] != "bad_parent" {
		t.Fatalf("子频道改分类应 bad_parent: %v", resp)
	}
	if resp := patch(map[string]any{"category_id": nil}, http.StatusBadRequest); resp["code"] != "bad_parent" {
		t.Fatalf("子频道清分类应 bad_parent: %v", resp)
	}
	if got, err := w.mem.ChannelByID(ctx, childID); err != nil || got.CategoryID == nil || *got.CategoryID != catA {
		t.Fatalf("拒绝后分类不应落库: %+v err=%v", got, err)
	}

	// 与父一致的类型+分类：放行。
	ok := patch(map[string]any{"type": "voice", "category_id": fmt.Sprint(catA)}, http.StatusOK)
	if ok["type"] != "voice" || asID(ok["category_id"]) != catA {
		t.Fatalf("与父一致的改动应放行: %v", ok)
	}

	// 父频道有子频道时改类型/分类仍 409（回归，不允许靠父侧漂移绕过不变量）。
	if resp := doJSON(t, rt, http.MethodPatch, fmt.Sprintf("/api/v1/channels/%d", parentID), w.owner,
		map[string]any{"type": "text"}, http.StatusConflict); resp["code"] != "channel_has_children" {
		t.Fatalf("父频道改类型应 409: %v", resp)
	}
	if resp := doJSON(t, rt, http.MethodPatch, fmt.Sprintf("/api/v1/channels/%d", parentID), w.owner,
		map[string]any{"category_id": fmt.Sprint(catB)}, http.StatusConflict); resp["code"] != "channel_has_children" {
		t.Fatalf("父频道改分类应 409: %v", resp)
	}

	// 顶层频道不受影响（回归）：可自由改类型与分类。
	top := w.createCh(t, map[string]any{"name": "顶层", "type": "voice"})
	up := doJSON(t, rt, http.MethodPatch, fmt.Sprintf("/api/v1/channels/%d", asID(top["id"])), w.owner,
		map[string]any{"type": "text", "category_id": fmt.Sprint(catB)}, http.StatusOK)
	if up["type"] != "text" || asID(up["category_id"]) != catB {
		t.Fatalf("顶层频道改类型/分类不应受限: %v", up)
	}
}

// TestChannelAudioOptions 频道音质（#46）：创建/更新往返、校验矩阵、text 归零。
func TestChannelAudioOptions(t *testing.T) {
	w := newChannelOptsWorld(t)
	rt := w.h

	// 语音频道创建带音质：响应恒含两字段，ListChannels 也一致。
	ch := w.createCh(t, map[string]any{
		"name": "音质", "type": "voice", "audio_bitrate_kbps": 16, "audio_frame_ms": 10,
	})
	chID := asID(ch["id"])
	if ch["audio_bitrate_kbps"] != float64(16) || ch["audio_frame_ms"] != float64(10) {
		t.Fatalf("创建响应音质不符: %v", ch)
	}
	list := getJSON(t, rt, fmt.Sprintf("/api/v1/servers/%d/channels", w.serverID), w.owner, http.StatusOK).(map[string]any)
	found := false
	for _, raw := range list["channels"].([]any) {
		m := raw.(map[string]any)
		if asID(m["id"]) == chID {
			found = true
			if m["audio_bitrate_kbps"] != float64(16) || m["audio_frame_ms"] != float64(10) {
				t.Fatalf("列表音质不符: %v", m)
			}
		}
	}
	if !found {
		t.Fatalf("列表里找不到新建频道: %v", list)
	}

	// 校验矩阵：码率 0/8/128 放行；7/129/-1 拒。帧长 0/10/20 放行；15 拒。
	patch := func(body map[string]any, want int) map[string]any {
		t.Helper()
		return doJSON(t, rt, http.MethodPatch, fmt.Sprintf("/api/v1/channels/%d", chID), w.owner, body, want)
	}
	for _, kbps := range []int{0, 8, 128} {
		resp := patch(map[string]any{"audio_bitrate_kbps": kbps}, http.StatusOK)
		if resp["audio_bitrate_kbps"] != float64(kbps) {
			t.Fatalf("码率 %d 更新不符: %v", kbps, resp)
		}
	}
	for _, kbps := range []int{7, 129, -1} {
		resp := patch(map[string]any{"audio_bitrate_kbps": kbps}, http.StatusBadRequest)
		if resp["code"] != "bad_channel_option" {
			t.Fatalf("码率 %d 应 bad_channel_option: %v", kbps, resp)
		}
	}
	for _, ms := range []int{0, 10, 20} {
		resp := patch(map[string]any{"audio_frame_ms": ms}, http.StatusOK)
		if resp["audio_frame_ms"] != float64(ms) {
			t.Fatalf("帧长 %d 更新不符: %v", ms, resp)
		}
	}
	for _, ms := range []int{15, 5, -10} {
		resp := patch(map[string]any{"audio_frame_ms": ms}, http.StatusBadRequest)
		if resp["code"] != "bad_channel_option" {
			t.Fatalf("帧长 %d 应 bad_channel_option: %v", ms, resp)
		}
	}
	// null 不表示清除：用 0 清零。缺省字段不改动另一字段。
	if resp := patch(map[string]any{"audio_bitrate_kbps": nil}, http.StatusBadRequest); resp["code"] != "bad_channel_option" {
		t.Fatalf("码率 null 应 400: %v", resp)
	}
	cur := patch(map[string]any{"audio_bitrate_kbps": 48}, http.StatusOK)
	if cur["audio_frame_ms"] != float64(20) {
		t.Fatalf("未提供的帧长应保持不变: %v", cur)
	}

	// text 频道：非零音质 400；显式 0 放行且响应保持 0/0。
	text := w.createCh(t, map[string]any{"name": "文", "type": "text", "audio_bitrate_kbps": 0, "audio_frame_ms": 0})
	if text["audio_bitrate_kbps"] != float64(0) || text["audio_frame_ms"] != float64(0) {
		t.Fatalf("text 音质应恒为 0/0: %v", text)
	}
	textID := asID(text["id"])
	textPatch := func(body map[string]any) map[string]any {
		t.Helper()
		return doJSON(t, rt, http.MethodPatch, fmt.Sprintf("/api/v1/channels/%d", textID), w.owner, body, http.StatusBadRequest)
	}
	if resp := textPatch(map[string]any{"audio_bitrate_kbps": 16}); resp["code"] != "bad_channel_option" {
		t.Fatalf("text 设码率应 400: %v", resp)
	}
	if resp := textPatch(map[string]any{"audio_frame_ms": 10}); resp["code"] != "bad_channel_option" {
		t.Fatalf("text 设帧长应 400: %v", resp)
	}
	doJSON(t, rt, http.MethodPost, fmt.Sprintf("/api/v1/servers/%d/channels", w.serverID), w.owner,
		map[string]any{"name": "文2", "type": "text", "audio_bitrate_kbps": 32}, http.StatusBadRequest)
	doJSON(t, rt, http.MethodPost, fmt.Sprintf("/api/v1/servers/%d/channels", w.serverID), w.owner,
		map[string]any{"name": "文3", "type": "text", "audio_frame_ms": 10}, http.StatusBadRequest)

	// voice→text 未显式改音质：旧值清零，不能把语音参数留在文字频道上。
	up := patch(map[string]any{"type": "text"}, http.StatusOK)
	if up["audio_bitrate_kbps"] != float64(0) || up["audio_frame_ms"] != float64(0) {
		t.Fatalf("改为 text 后音质应清零: %v", up)
	}
	// text→voice 同一次请求带音质：允许。
	if resp := patch(map[string]any{"type": "voice", "audio_bitrate_kbps": 24, "audio_frame_ms": 10}, http.StatusOK); resp["audio_bitrate_kbps"] != float64(24) {
		t.Fatalf("改回 voice 并设音质应放行: %v", resp)
	}

	// 权限/存在性：普通成员 403，不存在频道 404，非成员 403。
	doJSON(t, rt, http.MethodPatch, fmt.Sprintf("/api/v1/channels/%d", chID), w.member,
		map[string]any{"audio_bitrate_kbps": 16}, http.StatusForbidden)
	doJSON(t, rt, http.MethodPatch, "/api/v1/channels/4242424242", w.owner,
		map[string]any{"audio_bitrate_kbps": 16}, http.StatusNotFound)
	doJSON(t, rt, http.MethodPatch, fmt.Sprintf("/api/v1/channels/%d", chID), w.stranger,
		map[string]any{"audio_bitrate_kbps": 16}, http.StatusForbidden)
}

// TestPatchChannel10msLegacyGate PATCH 启用 10ms 帧长（#116）：房内有 proto<3
// 客户端时 409 且不落库；检查与 join 竞态（预检查通过、落库后复查命中）回滚
// 帧长再 409；无旧客户端或降到 20ms 时放行。
func TestPatchChannel10msLegacyGate(t *testing.T) {
	w := newChannelOptsWorld(t)
	rt := w.h
	ch := w.createCh(t, map[string]any{"name": "10ms", "type": "voice"})
	voiceID := asID(ch["id"])
	voicePath := fmt.Sprintf("/api/v1/channels/%d", voiceID)
	key := strconv.FormatInt(voiceID, 10)
	frameOf := func() int {
		t.Helper()
		got, err := w.mem.ChannelByID(context.Background(), voiceID)
		if err != nil {
			t.Fatalf("ChannelByID: %v", err)
		}
		return got.AudioFrameMS
	}
	wantConflict := func(resp map[string]any) {
		t.Helper()
		if resp["code"] != signal.ErrCodeProtocolUnsupported {
			t.Fatalf("409 code 应为 %q，实际 %v", signal.ErrCodeProtocolUnsupported, resp)
		}
	}

	// 预检查命中：房内有旧客户端 → 409，帧长保持 0（不落库、不广播）。
	w.hub.setRoomLegacy(key, true, 0)
	wantConflict(doJSON(t, rt, http.MethodPatch, voicePath, w.owner,
		map[string]any{"audio_frame_ms": 10}, http.StatusConflict))
	if got := frameOf(); got != 0 {
		t.Fatalf("被拒后库值应保持 0，实际 %d", got)
	}
	if got := len(w.hub.audioLog()); got != 0 {
		t.Fatalf("被拒时不应广播 channel.audio，实际 %d", got)
	}

	// 竞态分支：预检查放行，落库后复查命中 → 409 且帧长回滚为修改前的 0。
	w.hub.setRoomLegacy(key, true, 1)
	wantConflict(doJSON(t, rt, http.MethodPatch, voicePath, w.owner,
		map[string]any{"audio_frame_ms": 10}, http.StatusConflict))
	if got := frameOf(); got != 0 {
		t.Fatalf("复查命中后帧长应回滚为 0，实际 %d", got)
	}
	if got := w.hub.legacyChecks(); got != 2 {
		t.Fatalf("竞态分支应检查两次（预检查 + 复查），实际 %d", got)
	}
	if got := len(w.hub.audioLog()); got != 0 {
		t.Fatalf("回滚时不应广播错误配置，实际 %d", got)
	}

	// 无旧客户端：启用 10ms 放行并落库、广播。
	w.hub.setRoomLegacy(key, false, 0)
	resp := doJSON(t, rt, http.MethodPatch, voicePath, w.owner,
		map[string]any{"audio_frame_ms": 10}, http.StatusOK)
	if resp["audio_frame_ms"] != float64(10) || frameOf() != 10 {
		t.Fatalf("无旧客户端应启用 10ms: resp=%v db=%d", resp, frameOf())
	}
	if got := len(w.hub.audioLog()); got != 1 {
		t.Fatalf("成功后应广播一次 channel.audio，实际 %d", got)
	}

	// 10→20 不检查：即使房内标记有旧客户端也放行。
	w.hub.setRoomLegacy(key, true, 0)
	if resp := doJSON(t, rt, http.MethodPatch, voicePath, w.owner,
		map[string]any{"audio_frame_ms": 20}, http.StatusOK); resp["audio_frame_ms"] != float64(20) {
		t.Fatalf("10→20 不应受能力闸影响: %v", resp)
	}
	if got := frameOf(); got != 20 {
		t.Fatalf("10→20 应落库 20，实际 %d", got)
	}
	if got := w.hub.legacyChecks(); got != 0 {
		t.Fatalf("10→20 不应检查旧客户端，实际检查 %d 次", got)
	}

	// 值不变（10 再设 10）不触发检查；恢复 10 后房内标记旧客户端再设 10 仍放行。
	w.hub.setRoomLegacy(key, false, 0)
	doJSON(t, rt, http.MethodPatch, voicePath, w.owner, map[string]any{"audio_frame_ms": 10}, http.StatusOK)
	w.hub.setRoomLegacy(key, true, 0)
	if resp := doJSON(t, rt, http.MethodPatch, voicePath, w.owner,
		map[string]any{"audio_frame_ms": 10}, http.StatusOK); resp["audio_frame_ms"] != float64(10) {
		t.Fatalf("值不变不应受能力闸影响: %v", resp)
	}
	if got := w.hub.legacyChecks(); got != 0 {
		t.Fatalf("值不变不应检查旧客户端，实际检查 %d 次", got)
	}
}

// TestChannelAudioBroadcast 音质 PATCH 成功后广播 channel.audio（#46）：
// 只改音质才发；text 频道与仅改名不发。
func TestChannelAudioBroadcast(t *testing.T) {
	w := newChannelOptsWorld(t)
	rt := w.h
	voice := w.createCh(t, map[string]any{"name": "音质广播", "type": "voice"})
	voiceID := asID(voice["id"])
	voicePath := fmt.Sprintf("/api/v1/channels/%d", voiceID)

	doJSON(t, rt, http.MethodPatch, voicePath, w.owner,
		map[string]any{"audio_bitrate_kbps": 32, "audio_frame_ms": 10}, http.StatusOK)
	events := w.hub.audioLog()
	if len(events) != 1 {
		t.Fatalf("应广播一次 channel.audio，实际 %d: %+v", len(events), events)
	}
	ev := events[0]
	if ev.channelID != fmt.Sprint(voiceID) || ev.bitrate != 32 || ev.frameMS != 10 {
		t.Fatalf("广播载荷不符: %+v", ev)
	}
	if len(ev.uids) == 0 {
		t.Fatalf("广播收件人不应为空: %+v", ev)
	}

	// 仅改名不广播。
	doJSON(t, rt, http.MethodPatch, voicePath, w.owner,
		map[string]any{"name": "改名"}, http.StatusOK)
	if got := len(w.hub.audioLog()); got != 1 {
		t.Fatalf("仅改名不应广播，实际累计 %d", got)
	}

	// text 频道设置音质（显式 0）不广播。
	text := w.createCh(t, map[string]any{"name": "文字", "type": "text"})
	doJSON(t, rt, http.MethodPatch, fmt.Sprintf("/api/v1/channels/%d", asID(text["id"])), w.owner,
		map[string]any{"audio_bitrate_kbps": 0}, http.StatusOK)
	if got := len(w.hub.audioLog()); got != 1 {
		t.Fatalf("text 频道不应广播 channel.audio，实际累计 %d", got)
	}
}
