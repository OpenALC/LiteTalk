package api

import (
	"context"
	"fmt"
	"net/http"
	"testing"

	"github.com/OpenALC/LiteTalk/server/internal/store"
)

// #35 B3：REST 鉴权接入的回归与权限矩阵。角色数据直接走 store 夹具（角色管理
// API 是 B4），但每次直写 store 后必须 InvalidateServer——Authorizer 按服缓存
// （TTL 10s），不失效会拿旧快照裁决，测试与生产同此约束。

// meID 取当前令牌用户的数字 ID。
func meID(t *testing.T, h http.Handler, token string) int64 {
	t.Helper()
	me := getJSON(t, h, "/api/v1/users/me", token, http.StatusOK).(map[string]any)
	return asID(me["id"])
}

// permWorld 一套权限矩阵夹具：owner 建服，admin 挂 admin 角色，member 邀请入服。
type permWorld struct {
	t        *testing.T
	srv      *Server
	h        http.Handler
	mem      *store.Mem
	serverID int64
	owner    string
	admin    string
	member   string
	stranger string
}

func newPermWorld(t *testing.T) *permWorld {
	t.Helper()
	srv, mem := newTestServer(t)
	h := srv.Handler()
	ctx := context.Background()
	w := &permWorld{
		t: t, srv: srv, h: h, mem: mem,
		owner:    mustReg(t, h, mem, "pw-o@ex.com", "pwOwner"),
		admin:    mustReg(t, h, mem, "pw-a@ex.com", "pwAdmin"),
		member:   mustReg(t, h, mem, "pw-m@ex.com", "pwMember"),
		stranger: mustReg(t, h, mem, "pw-x@ex.com", "pwStranger"),
	}
	created := postJSON(t, h, "/api/v1/servers", w.owner,
		map[string]string{"name": "权限矩阵服", "icon_name": "music", "icon_color": "#4FA4F3"}, http.StatusCreated)
	w.serverID = asID(created["id"])

	// admin 角色（B4 前无角色管理 API，直写 store）。
	adminID := meID(t, h, w.admin)
	if err := mem.AddMember(ctx, &store.ServerMember{ServerID: w.serverID, UserID: adminID, Role: store.RoleAdmin}); err != nil {
		t.Fatalf("AddMember(admin): %v", err)
	}
	srv.authz.InvalidateServer(w.serverID)

	// member 经邀请入服（真实入服路径）。
	inv := postJSON(t, h, fmt.Sprintf("/api/v1/servers/%d/invites", w.serverID), w.owner,
		map[string]any{}, http.StatusCreated)
	postJSON(t, h, "/api/v1/servers/join", w.member,
		map[string]string{"code": inv["code"].(string)}, http.StatusOK)
	return w
}

func mustReg(t *testing.T, h http.Handler, mem *store.Mem, email, nickname string) string {
	t.Helper()
	tok, _ := registerUser(t, h, mem, email, nickname, "password123")
	return tok
}

// everyoneID 取该服 @everyone 角色 ID（覆盖夹具用）。
func (w *permWorld) everyoneID() int64 {
	w.t.Helper()
	roles, err := w.mem.ListRoles(context.Background(), w.serverID)
	if err != nil {
		w.t.Fatal(err)
	}
	for _, r := range roles {
		if r.BuiltinKey == store.BuiltinKeyMember {
			return r.ID
		}
	}
	w.t.Fatal("缺 @everyone 角色")
	return 0
}

// denyEveryone 直写 @everyone 覆盖并失效缓存（B3 无覆盖管理 API，B4 补）。
func (w *permWorld) denyEveryone(channelID int64, perm store.Permission) {
	w.t.Helper()
	if err := w.mem.SetChannelOverwrite(context.Background(), &store.ChannelOverwrite{
		ChannelID: channelID, TargetType: store.OverwriteTargetRole,
		TargetID: w.everyoneID(), Deny: perm,
	}); err != nil {
		w.t.Fatal(err)
	}
	w.srv.authz.InvalidateServer(w.serverID)
}

// TestPermissionsOwnerAdminMember 三档默认模板回归：admin 仍可管理，member 可看可发，
// 非成员 403，资源不存在 404。
func TestPermissionsOwnerAdminMember(t *testing.T) {
	w := newPermWorld(t)
	h := w.h
	ch := postJSON(t, h, fmt.Sprintf("/api/v1/servers/%d/channels", w.serverID), w.owner,
		map[string]string{"name": "大厅", "type": "text"}, http.StatusCreated)
	chID := asID(ch["id"])

	// admin：改服务器 / 建邀请 / 建频道 / 建分类 / 改分类。
	doJSON(t, h, http.MethodPatch, fmt.Sprintf("/api/v1/servers/%d", w.serverID), w.admin,
		map[string]string{"name": "改名成功"}, http.StatusOK)
	postJSON(t, h, fmt.Sprintf("/api/v1/servers/%d/invites", w.serverID), w.admin,
		map[string]any{}, http.StatusCreated)
	adminCh := postJSON(t, h, fmt.Sprintf("/api/v1/servers/%d/channels", w.serverID), w.admin,
		map[string]string{"name": "管理建的", "type": "text"}, http.StatusCreated)
	cat := postJSON(t, h, fmt.Sprintf("/api/v1/servers/%d/categories", w.serverID), w.admin,
		map[string]string{"name": "甲组"}, http.StatusCreated)
	doJSON(t, h, http.MethodPatch, fmt.Sprintf("/api/v1/categories/%d", asID(cat["id"])), w.admin,
		map[string]any{"name": "乙组"}, http.StatusOK)
	_ = adminCh

	// member：读频道/消息、发消息都可以。
	getJSON(t, h, fmt.Sprintf("/api/v1/servers/%d/channels", w.serverID), w.member, http.StatusOK)
	sendMsg(t, h, w.member, chID, "成员发言")
	getJSON(t, h, fmt.Sprintf("/api/v1/channels/%d/messages", chID), w.member, http.StatusOK)

	// member：管理类一律 403。
	for _, c := range []struct {
		method, path string
		body         any
	}{
		{http.MethodPatch, fmt.Sprintf("/api/v1/servers/%d", w.serverID), map[string]string{"name": "不够格"}},
		{http.MethodPost, fmt.Sprintf("/api/v1/servers/%d/invites", w.serverID), map[string]any{}},
		{http.MethodPost, fmt.Sprintf("/api/v1/servers/%d/channels", w.serverID), map[string]string{"name": "x", "type": "text"}},
		{http.MethodPatch, fmt.Sprintf("/api/v1/channels/%d", chID), map[string]string{"name": "y"}},
		{http.MethodDelete, fmt.Sprintf("/api/v1/channels/%d", chID), nil},
		{http.MethodPost, fmt.Sprintf("/api/v1/servers/%d/categories", w.serverID), map[string]string{"name": "x"}},
		{http.MethodPatch, fmt.Sprintf("/api/v1/categories/%d", asID(cat["id"])), map[string]any{"name": "z"}},
		{http.MethodDelete, fmt.Sprintf("/api/v1/categories/%d", asID(cat["id"])), nil},
	} {
		doJSON(t, h, c.method, c.path, w.member, c.body, http.StatusForbidden)
	}

	// 非成员：成员读口也 403。
	getJSON(t, h, fmt.Sprintf("/api/v1/servers/%d/channels", w.serverID), w.stranger, http.StatusForbidden)
	getJSON(t, h, fmt.Sprintf("/api/v1/servers/%d/members", w.serverID), w.stranger, http.StatusForbidden)
	getJSON(t, h, fmt.Sprintf("/api/v1/channels/%d/messages", chID), w.stranger, http.StatusForbidden)
	doJSON(t, h, http.MethodPatch, fmt.Sprintf("/api/v1/servers/%d", w.serverID), w.stranger,
		map[string]string{"name": "x"}, http.StatusForbidden)

	// 资源不存在 404（不是 403——存在性与权限分开报）。
	doJSON(t, h, http.MethodPatch, "/api/v1/servers/4242424242", w.owner,
		map[string]string{"name": "x"}, http.StatusNotFound)
	getJSON(t, h, "/api/v1/channels/4242424242/messages", w.owner, http.StatusNotFound)
	getJSON(t, h, "/api/v1/servers/4242424242/channels", w.owner, http.StatusNotFound)
}

// TestDeleteServerOwnerOnly 删服只属服主：admin 403（ManageServer 不等于拆服权）。
func TestDeleteServerOwnerOnly(t *testing.T) {
	w := newPermWorld(t)
	h := w.h
	// member 与 admin 都不能删。
	doJSON(t, h, http.MethodDelete, fmt.Sprintf("/api/v1/servers/%d", w.serverID), w.member, nil, http.StatusForbidden)
	doJSON(t, h, http.MethodDelete, fmt.Sprintf("/api/v1/servers/%d", w.serverID), w.admin, nil, http.StatusForbidden)
	// 非成员同 403。
	doJSON(t, h, http.MethodDelete, fmt.Sprintf("/api/v1/servers/%d", w.serverID), w.stranger, nil, http.StatusForbidden)
	// 服主可以。
	doJSON(t, h, http.MethodDelete, fmt.Sprintf("/api/v1/servers/%d", w.serverID), w.owner, nil, http.StatusOK)
}

// TestChannelVisibilityFilters ListChannels/GetSync 隐藏无 ViewChannel 的频道；
// categories 不因隐藏频道而变；消息读写按 VIEW/SEND 双门 403。
func TestChannelVisibilityFilters(t *testing.T) {
	w := newPermWorld(t)
	h := w.h

	cat := postJSON(t, h, fmt.Sprintf("/api/v1/servers/%d/categories", w.serverID), w.owner,
		map[string]string{"name": "可见组"}, http.StatusCreated)
	catID := asID(cat["id"])
	ch1 := postJSON(t, h, fmt.Sprintf("/api/v1/servers/%d/channels", w.serverID), w.owner,
		map[string]string{"name": "公开", "type": "text", "category_id": fmt.Sprintf("%d", catID)}, http.StatusCreated)
	ch2 := postJSON(t, h, fmt.Sprintf("/api/v1/servers/%d/channels", w.serverID), w.owner,
		map[string]string{"name": "隐藏", "type": "text", "category_id": fmt.Sprintf("%d", catID)}, http.StatusCreated)
	visID, hidID := asID(ch1["id"]), asID(ch2["id"])

	// member 隐藏 ch2（@everyone deny ViewChannel）。
	w.denyEveryone(hidID, store.ViewChannel)

	list := getJSON(t, h, fmt.Sprintf("/api/v1/servers/%d/channels", w.serverID), w.member, http.StatusOK).(map[string]any)
	chans := list["channels"].([]any)
	if len(chans) != 1 || asID(chans[0].(map[string]any)["id"]) != visID {
		t.Fatalf("member 应只见公开频道，实际 %v", chans)
	}
	// categories 原样（不因频道隐藏而过滤分类）。
	if cats := list["categories"].([]any); len(cats) != 1 || asID(cats[0].(map[string]any)["id"]) != catID {
		t.Fatalf("categories 不应被过滤: %v", list["categories"])
	}
	// 服主不受影响（owner 旁路）。
	all := getJSON(t, h, fmt.Sprintf("/api/v1/servers/%d/channels", w.serverID), w.owner, http.StatusOK).(map[string]any)
	if len(all["channels"].([]any)) != 2 {
		t.Fatalf("服主应见 2 个频道: %v", all["channels"])
	}

	// 隐藏频道：读 403、写 403（VIEW 先于 SEND 裁决）。
	getJSON(t, h, fmt.Sprintf("/api/v1/channels/%d/messages", hidID), w.member, http.StatusForbidden)
	postJSON(t, h, fmt.Sprintf("/api/v1/channels/%d/messages", hidID), w.member,
		map[string]string{"content": "看不见还发"}, http.StatusForbidden)
	// 公开频道读写正常。
	sendMsg(t, h, w.member, visID, "公开可见")

	// sync 快照同样过滤。
	body := getJSON(t, h, "/api/v1/sync", w.member, http.StatusOK).(map[string]any)
	servers := body["servers"].([]any)
	var sm map[string]any
	for _, raw := range servers {
		if asID(raw.(map[string]any)["id"]) == w.serverID {
			sm = raw.(map[string]any)
		}
	}
	if sm == nil {
		t.Fatalf("sync 缺服务器 %d", w.serverID)
	}
	schans := sm["channels"].([]any)
	if len(schans) != 1 || asID(schans[0].(map[string]any)["id"]) != visID {
		t.Fatalf("sync 应隐藏无权频道，实际 %v", schans)
	}
	if scats := sm["categories"].([]any); len(scats) != 1 {
		t.Fatalf("sync categories 不应被过滤: %v", scats)
	}

	// SEND 单独被拒：VIEW 还在时读得到、发不出。
	w.denyEveryone(visID, store.SendMessages)
	getJSON(t, h, fmt.Sprintf("/api/v1/channels/%d/messages", visID), w.member, http.StatusOK)
	postJSON(t, h, fmt.Sprintf("/api/v1/channels/%d/messages", visID), w.member,
		map[string]string{"content": "被禁言"}, http.StatusForbidden)

	// 语音频道发消息维持现状 400（B3 不改 REST 消息类型校验）。
	voice := postJSON(t, h, fmt.Sprintf("/api/v1/servers/%d/channels", w.serverID), w.owner,
		map[string]string{"name": "语音", "type": "voice"}, http.StatusCreated)
	postJSON(t, h, fmt.Sprintf("/api/v1/channels/%d/messages", asID(voice["id"])), w.owner,
		map[string]string{"content": "x"}, http.StatusBadRequest)
}

// TestCacheInvalidationAfterMutations 写后失效立即生效：入服后立刻可见，
// 删服后立刻 404（陈旧快照会给出 200/403 的旧答案）。
func TestCacheInvalidationAfterMutations(t *testing.T) {
	w := newPermWorld(t)
	h := w.h

	// 第二次入服视角：stranger 先探一次（把「非成员」写进缓存），入服后立刻可见频道。
	getJSON(t, h, fmt.Sprintf("/api/v1/servers/%d/channels", w.serverID), w.stranger, http.StatusForbidden)
	inv := postJSON(t, h, fmt.Sprintf("/api/v1/servers/%d/invites", w.serverID), w.owner,
		map[string]any{}, http.StatusCreated)
	postJSON(t, h, "/api/v1/servers/join", w.stranger,
		map[string]string{"code": inv["code"].(string)}, http.StatusOK)
	getJSON(t, h, fmt.Sprintf("/api/v1/servers/%d/channels", w.serverID), w.stranger, http.StatusOK)

	// 删服后成员立刻 404：不失效时旧快照仍判成员、回 200 空列表。
	getJSON(t, h, fmt.Sprintf("/api/v1/servers/%d/channels", w.serverID), w.member, http.StatusOK)
	doJSON(t, h, http.MethodDelete, fmt.Sprintf("/api/v1/servers/%d", w.serverID), w.owner, nil, http.StatusOK)
	getJSON(t, h, fmt.Sprintf("/api/v1/servers/%d/channels", w.serverID), w.member, http.StatusNotFound)
}
