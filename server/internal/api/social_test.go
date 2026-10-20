package api

import (
	"context"
	"fmt"
	"sync"
	"testing"

	"github.com/OpenALC/LiteTalk/server/internal/store"
)

// assertNoEmail 断言 friendView 的 user 子对象不含 email 键。
func assertNoEmail(t *testing.T, item map[string]any) {
	t.Helper()
	u, ok := item["user"].(map[string]any)
	if !ok {
		t.Fatalf("user 字段缺失或类型错误: %+v", item)
	}
	if _, leak := u["email"]; leak {
		t.Fatalf("好友响应泄露 email: %+v", u)
	}
	if u["nickname"] == "" || u["tag"] == "" {
		t.Fatalf("公开视图缺 nickname/tag: %+v", u)
	}
}

// TestFriendsResponseNoEmail 好友相关响应一律 PublicUser 视图：对端 email 不外泄（P1）。
// 昵称#tag 只有 1 万种组合可枚举，带 email 等于公开任意用户邮箱。
func TestFriendsResponseNoEmail(t *testing.T) {
	srv, mem := newTestServer(t)
	h := srv.Handler()

	aTok, _ := registerUser(t, h, mem, "pa@ex.com", "隐私A", "password123")
	bTok, _ := registerUser(t, h, mem, "pb@ex.com", "隐私B", "password123")

	// A 加 B（请求响应里的 user 是对端）。
	bu, err := mem.UserByEmail(t.Context(), "pb@ex.com")
	if err != nil {
		t.Fatalf("UserByEmail: %v", err)
	}
	fr := postJSON(t, h, "/api/v1/friends/requests", aTok,
		map[string]string{"nickname": fmt.Sprintf("隐私B#%s", bu.Tag)}, 201)
	assertNoEmail(t, fr)

	fid := int(asID(fr["id"]))

	// B 接受（响应对 A 视角）。
	acc := postJSON(t, h, fmt.Sprintf("/api/v1/friends/requests/%d/accept", fid), bTok, nil, 200)
	assertNoEmail(t, acc)

	// 双方列表（对端 user）。
	for _, tok := range []string{aTok, bTok} {
		list := getJSON(t, h, "/api/v1/friends", tok, 200).([]any)
		if len(list) != 1 {
			t.Fatalf("好友列表应有 1 条，实际 %d", len(list))
		}
		assertNoEmail(t, list[0].(map[string]any))
	}

	// PATCH 备注响应对端也无 email。
	patched := patchJSON(t, h, fmt.Sprintf("/api/v1/friends/%d", fid), aTok,
		map[string]string{"remark": "同事"}, 200)
	assertNoEmail(t, patched)
}

// TestSelfResponseKeepsEmail 本人 /me 与注册自响应仍带 email（视图只约束「他人」）。
func TestSelfResponseKeepsEmail(t *testing.T) {
	srv, mem := newTestServer(t)
	h := srv.Handler()

	access, _ := registerUser(t, h, mem, "self@ex.com", "本人视图", "password123")
	me := getJSON(t, h, "/api/v1/users/me", access, 200).(map[string]any)
	if me["email"] != "self@ex.com" {
		t.Fatalf("/me 应保留本人 email，实际 %v", me["email"])
	}
}

// TestPatchFriendRemark 备注/分组：发起方可改并随列表返回，被请求方 403。
func TestPatchFriendRemark(t *testing.T) {
	srv, mem := newTestServer(t)
	h := srv.Handler()

	aTok, _ := registerUser(t, h, mem, "fa@ex.com", "好友A", "password123")
	bTok, _ := registerUser(t, h, mem, "fb@ex.com", "好友B", "password123")

	// A 加 B（tag 随机生成，从 mem 取真实值拼 昵称#tag）。
	bu, err := mem.UserByEmail(t.Context(), "fb@ex.com")
	if err != nil {
		t.Fatalf("UserByEmail: %v", err)
	}
	req := postJSON(t, h, "/api/v1/friends/requests", aTok,
		map[string]string{"nickname": fmt.Sprintf("好友B#%s", bu.Tag)}, 201)
	fid := asID(req["id"])
	postJSON(t, h, fmt.Sprintf("/api/v1/friends/requests/%d/accept", int(fid)), bTok, nil, 200)

	// A（发起方）设置备注与分组。
	patchJSON(t, h, fmt.Sprintf("/api/v1/friends/%d", int(fid)), aTok,
		map[string]string{"remark": "同事小王", "group": "同事"}, 200)

	// A 列表带出备注/分组。
	list := getJSON(t, h, "/api/v1/friends", aTok, 200).([]any)
	if len(list) != 1 {
		t.Fatalf("A 好友数 = %d", len(list))
	}
	v := list[0].(map[string]any)
	if v["remark"] != "同事小王" || v["group"] != "同事" {
		t.Fatalf("备注/分组未带回: %+v", v)
	}

	// B（被请求方）改同一条行 → 403（备注是发起方视角）。
	patchJSON(t, h, fmt.Sprintf("/api/v1/friends/%d", int(fid)), bTok,
		map[string]string{"remark": "x"}, 403)

	// 超长备注 → 400。
	patchJSON(t, h, fmt.Sprintf("/api/v1/friends/%d", int(fid)), aTok,
		map[string]string{"remark": string(make([]byte, 65))}, 400)
}

// TestJoinServerInviteExhausted 邀请码次数用尽返回 409 code_exhausted；
// 已在服务器内的用户重复请求按幂等成功返回（#22：消费邀请码与加入成员同事务）。
func TestJoinServerInviteExhausted(t *testing.T) {
	srv, mem := newTestServer(t)
	h := srv.Handler()

	aTok, _ := registerUser(t, h, mem, "jca@ex.com", "邀请A", "password123")
	bTok, _ := registerUser(t, h, mem, "jcb@ex.com", "邀请B", "password123")
	cTok, _ := registerUser(t, h, mem, "jcc@ex.com", "邀请C", "password123")

	sv := postJSON(t, h, "/api/v1/servers", aTok, map[string]string{"name": "邀请服"}, 201)
	sid := int(asID(sv["id"]))
	inv := postJSON(t, h, fmt.Sprintf("/api/v1/servers/%d/invites", sid), aTok,
		map[string]any{"max_uses": 1}, 201)
	code := inv["code"].(string)

	postJSON(t, h, "/api/v1/servers/join", bTok, map[string]string{"code": code}, 200)
	// 已加入的用户重复请求：幂等成功，不应返回 code_exhausted。
	postJSON(t, h, "/api/v1/servers/join", bTok, map[string]string{"code": code}, 200)
	// 次数已用尽：第三个用户 409。
	out := postJSON(t, h, "/api/v1/servers/join", cTok, map[string]string{"code": code}, 409)
	if out["code"] != "code_exhausted" {
		t.Fatalf("期望 code_exhausted，实际 %+v", out)
	}
}

// TestCategoriesCRUD 分类创建/列频道/删除：非空未带参 409，with_channels=1 级联。
func TestCategoriesCRUD(t *testing.T) {
	srv, mem := newTestServer(t)
	h := srv.Handler()

	aTok, _ := registerUser(t, h, mem, "ca@ex.com", "分类A", "password123")
	bTok, _ := registerUser(t, h, mem, "cb@ex.com", "分类B", "password123")

	// A 建服务器。
	sv := postJSON(t, h, "/api/v1/servers", aTok, map[string]string{"name": "测试服"}, 201)
	sid := int(asID(sv["id"]))
	// B 经邀请加入（成员可读频道）。
	inv := postJSON(t, h, fmt.Sprintf("/api/v1/servers/%d/invites", sid), aTok, map[string]any{}, 201)
	postJSON(t, h, "/api/v1/servers/join", bTok, map[string]string{"code": inv["code"].(string)}, 200)

	// 创建分类。
	cat := postJSON(t, h, fmt.Sprintf("/api/v1/servers/%d/categories", sid), aTok,
		map[string]string{"name": "语音分组"}, 201)
	catID := int(asID(cat["id"]))

	// 在分类下建频道（先建频道再 PATCH 归属，createChannelReq 无 category 字段）。
	ch := postJSON(t, h, fmt.Sprintf("/api/v1/servers/%d/channels", sid), aTok,
		map[string]any{"name": "开黑房", "type": "voice"}, 201)
	chID := int(asID(ch["id"]))
	patchJSON(t, h, fmt.Sprintf("/api/v1/channels/%d", chID), aTok,
		map[string]any{"category_id": catID}, 200)

	// GET channels 返回 categories + 频道带 category_id。
	out := getJSON(t, h, fmt.Sprintf("/api/v1/servers/%d/channels", sid), bTok, 200).(map[string]any)
	cats := out["categories"].([]any)
	chans := out["channels"].([]any)
	if len(cats) != 1 || cats[0].(map[string]any)["name"] != "语音分组" {
		t.Fatalf("categories = %+v", cats)
	}
	found := false
	for _, c := range chans {
		m := c.(map[string]any)
		if m["category_id"] != nil && int(asID(m["category_id"])) == catID {
			found = true
		}
	}
	if !found {
		t.Fatalf("频道未带 category_id: %+v", chans)
	}

	// 非空未带参删除 → 409。
	delJSON(t, h, fmt.Sprintf("/api/v1/categories/%d", catID), aTok, 409)
	// with_channels=1 级联删除。
	delJSON(t, h, fmt.Sprintf("/api/v1/categories/%d?with_channels=1", catID), aTok, 200)
	// 频道一并消失。
	out = getJSON(t, h, fmt.Sprintf("/api/v1/servers/%d/channels", sid), aTok, 200).(map[string]any)
	if len(out["channels"].([]any)) != 0 || len(out["categories"].([]any)) != 0 {
		t.Fatalf("级联删除后仍有残留: %+v", out)
	}
}

// TestCategoryForbidden 非 owner/admin 创建分类 → 403。
func TestCategoryForbidden(t *testing.T) {
	srv, mem := newTestServer(t)
	h := srv.Handler()

	aTok, _ := registerUser(t, h, mem, "da@ex.com", "权限A", "password123")
	bTok, _ := registerUser(t, h, mem, "db@ex.com", "权限B", "password123")
	sv := postJSON(t, h, "/api/v1/servers", aTok, map[string]string{"name": "权限服"}, 201)
	sid := int(asID(sv["id"]))
	inv := postJSON(t, h, fmt.Sprintf("/api/v1/servers/%d/invites", sid), aTok, map[string]any{}, 201)
	postJSON(t, h, "/api/v1/servers/join", bTok, map[string]string{"code": inv["code"].(string)}, 200)

	postJSON(t, h, fmt.Sprintf("/api/v1/servers/%d/categories", sid), bTok,
		map[string]string{"name": "越权"}, 403)
}

// TestPatchCategory 分类重命名与排序（0.1.2）：owner/admin 才可改，成员只读；
// name/position 至少给一个；position 重排同服务器分类为 0..n-1。
func TestPatchCategory(t *testing.T) {
	srv, mem := newTestServer(t)
	h := srv.Handler()

	aTok, _ := registerUser(t, h, mem, "pa@ex.com", "改名A", "password123")
	bTok, _ := registerUser(t, h, mem, "pb@ex.com", "改名B", "password123")
	cTok, _ := registerUser(t, h, mem, "pc@ex.com", "路人C", "password123")

	sv := postJSON(t, h, "/api/v1/servers", aTok, map[string]string{"name": "改名服"}, 201)
	sid := int(asID(sv["id"]))
	inv := postJSON(t, h, fmt.Sprintf("/api/v1/servers/%d/invites", sid), aTok, map[string]any{}, 201)
	postJSON(t, h, "/api/v1/servers/join", bTok, map[string]string{"code": inv["code"].(string)}, 200)

	// 三个分类：甲 乙 丙（position 0 1 2）。
	catIDs := []int{}
	for _, name := range []string{"甲", "乙", "丙"} {
		cat := postJSON(t, h, fmt.Sprintf("/api/v1/servers/%d/categories", sid), aTok,
			map[string]string{"name": name}, 201)
		catIDs = append(catIDs, int(asID(cat["id"])))
	}

	// 未登录 → 401。
	patchJSON(t, h, fmt.Sprintf("/api/v1/categories/%d", catIDs[0]), "",
		map[string]any{"name": "x"}, 401)
	// 都不给 → 400 bad_request。
	out := patchJSON(t, h, fmt.Sprintf("/api/v1/categories/%d", catIDs[0]), aTok,
		map[string]any{}, 400)
	if out["code"] != "bad_request" {
		t.Fatalf("code = %v", out["code"])
	}
	// 非法名 → 400 bad_name。
	out = patchJSON(t, h, fmt.Sprintf("/api/v1/categories/%d", catIDs[0]), aTok,
		map[string]any{"name": ""}, 400)
	if out["code"] != "bad_name" {
		t.Fatalf("code = %v", out["code"])
	}
	// 成员只读 → 403；非成员 → 403。
	patchJSON(t, h, fmt.Sprintf("/api/v1/categories/%d", catIDs[0]), bTok,
		map[string]any{"name": "越权改"}, 403)
	patchJSON(t, h, fmt.Sprintf("/api/v1/categories/%d", catIDs[0]), cTok,
		map[string]any{"name": "路人改"}, 403)

	// owner 改名 → 200，响应回带最终字段。
	out = patchJSON(t, h, fmt.Sprintf("/api/v1/categories/%d", catIDs[0]), aTok,
		map[string]any{"name": "甲改"}, 200)
	if out["name"] != "甲改" || int(asID(out["id"])) != catIDs[0] {
		t.Fatalf("改名响应 = %+v", out)
	}

	// 丙移到最前 → 顺序 丙 甲改 乙，position 重排 0 1 2。
	patchJSON(t, h, fmt.Sprintf("/api/v1/categories/%d", catIDs[2]), aTok,
		map[string]any{"position": 0}, 200)
	chans := getJSON(t, h, fmt.Sprintf("/api/v1/servers/%d/channels", sid), aTok, 200).(map[string]any)
	cats := chans["categories"].([]any)
	want := []string{"丙", "甲改", "乙"}
	for i, raw := range cats {
		m := raw.(map[string]any)
		if m["name"] != want[i] || int(m["position"].(float64)) != i {
			t.Fatalf("重排后[%d] = %+v，期望 %s/%d", i, m, want[i], i)
		}
	}

	// 同一请求既改名又重排：先重排、读回、再改名，响应带最终 position。
	out = patchJSON(t, h, fmt.Sprintf("/api/v1/categories/%d", catIDs[1]), aTok,
		map[string]any{"name": "乙改", "position": 2}, 200)
	if out["name"] != "乙改" || int(out["position"].(float64)) != 2 {
		t.Fatalf("改名+重排响应 = %+v，期望 乙改/2", out)
	}
	chans = getJSON(t, h, fmt.Sprintf("/api/v1/servers/%d/channels", sid), aTok, 200).(map[string]any)
	cats = chans["categories"].([]any)
	want = []string{"丙", "甲改", "乙改"}
	for i, raw := range cats {
		if raw.(map[string]any)["name"] != want[i] {
			t.Fatalf("改名+重排后[%d] = %+v，期望 %s", i, raw, want[i])
		}
	}

	// 不存在的分类 → 404。
	patchJSON(t, h, "/api/v1/categories/9999", aTok, map[string]any{"name": "x"}, 404)

	// 原样保存（name 未变）→ 200：对话框不改内容直接保存是常规操作，不是 404。
	out = patchJSON(t, h, fmt.Sprintf("/api/v1/categories/%d", catIDs[1]), aTok,
		map[string]any{"name": "乙改"}, 200)
	if out["name"] != "乙改" {
		t.Fatalf("原样保存响应 = %+v", out)
	}
	// name 未变 + 同请求带 position：重排不得被 no-op 更新挡下。
	patchJSON(t, h, fmt.Sprintf("/api/v1/categories/%d", catIDs[1]), aTok,
		map[string]any{"name": "乙改", "position": 0}, 200)
	chans = getJSON(t, h, fmt.Sprintf("/api/v1/servers/%d/channels", sid), aTok, 200).(map[string]any)
	cats = chans["categories"].([]any)
	if first := cats[0].(map[string]any)["name"]; first != "乙改" {
		t.Fatalf("no-op 改名后重排未生效，首位 = %v", first)
	}
}

// patchOrderStore 记录分类写入顺序的 store 包装（其余方法透传）。
// 用于断言 PATCH 的执行顺序：重排 → 重新读回 → 改名。
type patchOrderStore struct {
	store.Store
	mu          sync.Mutex
	calls       []string       // 依次记录的 read / reorder / update
	writes      map[string]int // 改名时 UpdateCategory 收到的 position
	reorderHook func(serverID, id int64)
}

func (s *patchOrderStore) CategoryByID(ctx context.Context, id int64) (*store.Category, error) {
	s.mu.Lock()
	s.calls = append(s.calls, "read")
	s.mu.Unlock()
	return s.Store.CategoryByID(ctx, id)
}

func (s *patchOrderStore) ReorderCategories(ctx context.Context, serverID, id int64, position int) error {
	if err := s.Store.ReorderCategories(ctx, serverID, id, position); err != nil {
		return err
	}
	s.mu.Lock()
	s.calls = append(s.calls, "reorder")
	hook := s.reorderHook
	s.reorderHook = nil
	s.mu.Unlock()
	if hook != nil {
		hook(serverID, id)
	}
	return nil
}

func (s *patchOrderStore) UpdateCategory(ctx context.Context, c *store.Category) error {
	s.mu.Lock()
	s.calls = append(s.calls, "update")
	if s.writes == nil {
		s.writes = map[string]int{}
	}
	s.writes[c.Name] = c.Position
	s.mu.Unlock()
	return s.Store.UpdateCategory(ctx, c)
}

// TestPatchCategoryReorderBeforeRename name+position 同时给出时的写入顺序（#16 P2）：
// 必须「重排 → 重新读回 → 改名」，改名携带重排后的 position，而不是加载时的旧值。
// 在重排之后、读回之前插入一次并发重排，确认处理器的改名不会把旧 position 写回。
func TestPatchCategoryReorderBeforeRename(t *testing.T) {
	srv, mem := newTestServer(t)
	rec := &patchOrderStore{Store: mem}
	srv.store = rec
	h := srv.Handler()

	aTok, _ := registerUser(t, h, mem, "po@ex.com", "顺序A", "password123")
	sv := postJSON(t, h, "/api/v1/servers", aTok, map[string]string{"name": "顺序服"}, 201)
	sid := int(asID(sv["id"]))
	ids := map[string]int{}
	for _, name := range []string{"甲", "乙", "丙"} {
		cat := postJSON(t, h, fmt.Sprintf("/api/v1/servers/%d/categories", sid), aTok,
			map[string]string{"name": name}, 201)
		ids[name] = int(asID(cat["id"]))
	}

	// 处理器把乙重排到 0 之后、读回之前，另一路并发把乙挪到 2：
	// 改名应写入读回后的 2，旧值 1 不得回写。
	rec.reorderHook = func(serverID, id int64) {
		done := make(chan error, 1)
		go func() { done <- mem.ReorderCategories(context.Background(), serverID, id, 2) }()
		if err := <-done; err != nil {
			t.Errorf("并发重排失败: %v", err)
		}
	}

	out := patchJSON(t, h, fmt.Sprintf("/api/v1/categories/%d", ids["乙"]), aTok,
		map[string]any{"name": "乙改", "position": 0}, 200)
	if out["name"] != "乙改" {
		t.Fatalf("响应 name = %v，期望 乙改", out["name"])
	}
	if got := int(out["position"].(float64)); got != 2 {
		t.Fatalf("响应 position = %d，期望 2（并发重排后的最终序号）", got)
	}

	rec.mu.Lock()
	calls := append([]string(nil), rec.calls...)
	pos, ok := rec.writes["乙改"]
	rec.mu.Unlock()
	want := []string{"read", "reorder", "read", "update"}
	if fmt.Sprint(calls) != fmt.Sprint(want) {
		t.Fatalf("调用顺序 = %v，期望 %v", calls, want)
	}
	if !ok || pos != 2 {
		t.Fatalf("改名写入 position = %d（存在=%v），期望 2；旧值 1 不得回写", pos, ok)
	}
}
