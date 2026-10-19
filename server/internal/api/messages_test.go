package api

import (
	"fmt"
	"net/http"
	"testing"
)

// sendMsg 发一条消息并返回其 ID。
func sendMsg(t *testing.T, h http.Handler, token string, chID int64, content string) int64 {
	t.Helper()
	resp := postJSON(t, h, fmt.Sprintf("/api/v1/channels/%d/messages", chID), token,
		map[string]string{"content": content}, http.StatusCreated)
	return asID(resp["id"])
}

// TestMessageCursorPagination 游标分页（#31）：before 取更旧、after 取更新、
// 旧别名 before_id 等价 before、负游标 400、禁 offset（不存在的参数直接忽略）。
func TestMessageCursorPagination(t *testing.T) {
	srv, mem := newTestServer(t)
	h := srv.Handler()
	aAccess, _ := registerUser(t, h, mem, "pg-a@ex.com", "pgA", "password123")

	serverResp := postJSON(t, h, "/api/v1/servers", aAccess,
		map[string]string{"name": "分页服务器", "icon_name": "music", "icon_color": "#4FA4F3"}, http.StatusCreated)
	serverID := asID(serverResp["id"])
	chResp := postJSON(t, h, fmt.Sprintf("/api/v1/servers/%d/channels", serverID), aAccess,
		map[string]string{"name": "闲聊", "type": "text"}, http.StatusCreated)
	chID := asID(chResp["id"])

	// 发 5 条，记录 ID（Snowflake 单调递增）。
	ids := make([]int64, 5)
	for i := range ids {
		ids[i] = sendMsg(t, h, aAccess, chID, fmt.Sprintf("第%d条", i+1))
	}

	// 最新一页（无游标）：5 条全量，旧→新正序。
	all := getJSON(t, h, fmt.Sprintf("/api/v1/channels/%d/messages", chID), aAccess, http.StatusOK).([]any)
	if len(all) != 5 {
		t.Fatalf("全量应 5 条，实际 %d", len(all))
	}
	for i, it := range all {
		if asID(it.(map[string]any)["id"]) != ids[i] {
			t.Fatalf("第%d条应为 %d，实际 %v", i, ids[i], it.(map[string]any)["id"])
		}
	}

	// before=ids[3] → 只取更旧的 3 条（ids[0..2]），正序。
	page := getJSON(t, h, fmt.Sprintf("/api/v1/channels/%d/messages?before=%d", chID, ids[3]), aAccess, http.StatusOK).([]any)
	if len(page) != 3 {
		t.Fatalf("before 页应 3 条，实际 %d", len(page))
	}
	if asID(page[0].(map[string]any)["id"]) != ids[0] || asID(page[2].(map[string]any)["id"]) != ids[2] {
		t.Fatalf("before 页顺序错误：%v", page)
	}

	// before_id 旧别名与 before 等价（兼容窗口，#31 裁定10）。
	alias := getJSON(t, h, fmt.Sprintf("/api/v1/channels/%d/messages?before_id=%d", chID, ids[3]), aAccess, http.StatusOK).([]any)
	if len(alias) != 3 || asID(alias[0].(map[string]any)["id"]) != ids[0] {
		t.Fatalf("before_id 别名应等价 before，实际 %d 条", len(alias))
	}

	// after=ids[1] → 取更新的 ids[2..4]，正序。
	fwd := getJSON(t, h, fmt.Sprintf("/api/v1/channels/%d/messages?after=%d", chID, ids[1]), aAccess, http.StatusOK).([]any)
	if len(fwd) != 3 || asID(fwd[0].(map[string]any)["id"]) != ids[2] || asID(fwd[2].(map[string]any)["id"]) != ids[4] {
		t.Fatalf("after 页应为 ids[2..4]，实际 %d 条", len(fwd))
	}

	// limit 截断（越界回落 50 的口径下，合法值生效）。
	two := getJSON(t, h, fmt.Sprintf("/api/v1/channels/%d/messages?limit=2", chID), aAccess, http.StatusOK).([]any)
	if len(two) != 2 || asID(two[0].(map[string]any)["id"]) != ids[3] {
		t.Fatalf("limit=2 应取最新2条，实际 %d 条", len(two))
	}

	// 负游标拒绝。
	getJSON(t, h, fmt.Sprintf("/api/v1/channels/%d/messages?before=-1", chID), aAccess, http.StatusBadRequest)
}

// TestMessageNonceIdempotent nonce 幂等（#31）：同 nonce 重放回原消息 200，
// 回显原样返回；无 nonce 不去重。
func TestMessageNonceIdempotent(t *testing.T) {
	srv, mem := newTestServer(t)
	h := srv.Handler()
	aAccess, _ := registerUser(t, h, mem, "nc-a@ex.com", "ncA", "password123")

	serverResp := postJSON(t, h, "/api/v1/servers", aAccess,
		map[string]string{"name": "幂等服务器", "icon_name": "music", "icon_color": "#4FA4F3"}, http.StatusCreated)
	serverID := asID(serverResp["id"])
	chResp := postJSON(t, h, fmt.Sprintf("/api/v1/servers/%d/channels", serverID), aAccess,
		map[string]string{"name": "闲聊", "type": "text"}, http.StatusCreated)
	chID := asID(chResp["id"])

	// 首发 201，nonce 原样回显。
	first := postJSON(t, h, fmt.Sprintf("/api/v1/channels/%d/messages", chID), aAccess,
		map[string]string{"content": "你好", "nonce": "n-001"}, http.StatusCreated)
	if first["nonce"] != "n-001" {
		t.Fatalf("nonce 应原样回显，实际 %v", first["nonce"])
	}
	id := asID(first["id"])

	// 同 nonce 重放 → 200 + 原消息体（内容不同也回原消息，防对账分叉）。
	replay := postJSON(t, h, fmt.Sprintf("/api/v1/channels/%d/messages", chID), aAccess,
		map[string]string{"content": "被替换的内容", "nonce": "n-001"}, http.StatusOK)
	if asID(replay["id"]) != id || replay["content"] != "你好" {
		t.Fatalf("重放应回原消息：%v", replay)
	}

	// 不同 nonce 是新消息；无 nonce 不去重。
	other := postJSON(t, h, fmt.Sprintf("/api/v1/channels/%d/messages", chID), aAccess,
		map[string]string{"content": "你好", "nonce": "n-002"}, http.StatusCreated)
	if asID(other["id"]) == id {
		t.Fatal("不同 nonce 应为新消息")
	}
	postJSON(t, h, fmt.Sprintf("/api/v1/channels/%d/messages", chID), aAccess,
		map[string]string{"content": "无幂等"}, http.StatusCreated)
	postJSON(t, h, fmt.Sprintf("/api/v1/channels/%d/messages", chID), aAccess,
		map[string]string{"content": "无幂等"}, http.StatusCreated)

	msgs := getJSON(t, h, fmt.Sprintf("/api/v1/channels/%d/messages", chID), aAccess, http.StatusOK).([]any)
	if len(msgs) != 4 {
		t.Fatalf("应 4 条（n-001/n-002 + 两条无 nonce 各落一条），实际 %d", len(msgs))
	}

	// 超长 nonce 拒绝。
	postJSON(t, h, fmt.Sprintf("/api/v1/channels/%d/messages", chID), aAccess,
		map[string]string{"content": "x", "nonce": string(make([]byte, 65))}, http.StatusBadRequest)
}

// TestMessageNonceChannelScoped 去重键含频道（#31）：同一 nonce 跨频道各自独立；
// 幂等判断先于频控——配额耗尽后重放仍回原消息（200），而不是 429。
func TestMessageNonceChannelScoped(t *testing.T) {
	srv, mem := newTestServer(t)
	h := srv.Handler()
	aAccess, _ := registerUser(t, h, mem, "nc-scope@ex.com", "ncScope", "password123")

	serverResp := postJSON(t, h, "/api/v1/servers", aAccess,
		map[string]string{"name": "跨频道", "icon_name": "music", "icon_color": "#4FA4F3"}, http.StatusCreated)
	serverID := asID(serverResp["id"])
	mkChannel := func(name string) int64 {
		t.Helper()
		resp := postJSON(t, h, fmt.Sprintf("/api/v1/servers/%d/channels", serverID), aAccess,
			map[string]string{"name": name, "type": "text"}, http.StatusCreated)
		return asID(resp["id"])
	}
	ch1, ch2 := mkChannel("一"), mkChannel("二")

	first := postJSON(t, h, fmt.Sprintf("/api/v1/channels/%d/messages", ch1), aAccess,
		map[string]string{"content": "频道一", "nonce": "same"}, http.StatusCreated)
	second := postJSON(t, h, fmt.Sprintf("/api/v1/channels/%d/messages", ch2), aAccess,
		map[string]string{"content": "频道二", "nonce": "same"}, http.StatusCreated)
	if asID(first["id"]) == asID(second["id"]) {
		t.Fatal("跨频道同 nonce 不应互相命中")
	}

	// 各自重放回各自频道的原消息。
	replay1 := postJSON(t, h, fmt.Sprintf("/api/v1/channels/%d/messages", ch1), aAccess,
		map[string]string{"content": "重放一", "nonce": "same"}, http.StatusOK)
	if asID(replay1["id"]) != asID(first["id"]) || replay1["content"] != "频道一" {
		t.Fatalf("频道一重放应回原消息：%v", replay1)
	}
	replay2 := postJSON(t, h, fmt.Sprintf("/api/v1/channels/%d/messages", ch2), aAccess,
		map[string]string{"content": "重放二", "nonce": "same"}, http.StatusOK)
	if asID(replay2["id"]) != asID(second["id"]) || replay2["content"] != "频道二" {
		t.Fatalf("频道二重放应回原消息：%v", replay2)
	}

	// 打满 20 条/分配额（上面已计 2 条），再断言：新 nonce 429，重放旧 nonce 仍 200。
	for i := 0; i < 18; i++ {
		postJSON(t, h, fmt.Sprintf("/api/v1/channels/%d/messages", ch1), aAccess,
			map[string]string{"content": fmt.Sprintf("填充%d", i), "nonce": fmt.Sprintf("fill-%d", i)}, http.StatusCreated)
	}
	postJSON(t, h, fmt.Sprintf("/api/v1/channels/%d/messages", ch1), aAccess,
		map[string]string{"content": "超限", "nonce": "fill-over"}, http.StatusTooManyRequests)
	afterLimit := postJSON(t, h, fmt.Sprintf("/api/v1/channels/%d/messages", ch1), aAccess,
		map[string]string{"content": "重放", "nonce": "same"}, http.StatusOK)
	if asID(afterLimit["id"]) != asID(first["id"]) {
		t.Fatalf("配额耗尽后的幂等重放应回原消息：%v", afterLimit)
	}
}
