package api

import (
	"context"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"testing"

	"github.com/OpenALC/LiteTalk/server/internal/store"
)

// #35 B4：角色/覆盖管理与审计的 REST 回归。层级防提权、builtin 保护、
// 覆盖越界位、缓存失效与审计可见性都钉在这里。

// permWorld2 在 permWorld 之上加一个「granter」用户：持自定义角色
// ManageRoles|Speak|ViewChannel|SendMessages（无 Administrator），用于层级/
// 权限子集用例——admin 的 Administrator 会把子集检查全放行，测不出越权授予。
type permWorld2 struct {
	*permWorld
	granter   string // 令牌
	granterID int64
	grantRole *store.Role
}

func newPermWorld2(t *testing.T) *permWorld2 {
	t.Helper()
	w := newPermWorld(t)
	ctx := context.Background()
	w2 := &permWorld2{permWorld: w, granter: mustReg(t, w.h, w.mem, "pw-g@ex.com", "pwGranter")}
	w2.granterID = meID(t, w.h, w2.granter)
	// granter 先入服（成员才有角色可分配）。
	inv := postJSON(t, w.h, fmt.Sprintf("/api/v1/servers/%d/invites", w.serverID), w.owner,
		map[string]any{}, http.StatusCreated)
	postJSON(t, w.h, "/api/v1/servers/join", w2.granter,
		map[string]string{"code": inv["code"].(string)}, http.StatusOK)
	// 服主建高位角色（position=9）并授予 granter；granter 最高位=9。
	r := &store.Role{
		ServerID: w.serverID, Name: "授权者", Position: 9,
		Permissions: store.ManageRoles | store.Speak | store.ViewChannel | store.SendMessages,
	}
	if err := w.mem.CreateRole(ctx, r); err != nil {
		t.Fatalf("CreateRole(grant): %v", err)
	}
	if err := w.mem.SetMemberRoles(ctx, w.serverID, w2.granterID, []int64{r.ID}, 0); err != nil {
		t.Fatalf("SetMemberRoles(grant): %v", err)
	}
	w.srv.authz.InvalidateServer(w.serverID)
	w2.grantRole = r
	return w2
}

// permStr 权限位 → wire 十进制串。
func permStr(p store.Permission) string { return strconv.FormatUint(uint64(p), 10) }

// TestRolesListAndBuiltinProtection 角色列表成员可读；builtin 改/删 403；
// 创建/更新/删除需 ManageRoles。
func TestRolesListAndBuiltinProtection(t *testing.T) {
	w := newPermWorld(t)
	h := w.h

	// 成员可读（含 3 条 builtin）。
	list := getJSON(t, h, fmt.Sprintf("/api/v1/servers/%d/roles", w.serverID), w.member, http.StatusOK).([]any)
	if len(list) != 3 {
		t.Fatalf("应 3 条 builtin，实际 %d: %v", len(list), list)
	}
	if list[0].(map[string]any)["name"] != "@everyone" {
		t.Fatalf("排序应 @everyone 在前: %v", list[0])
	}
	// 非成员 403。
	getJSON(t, h, fmt.Sprintf("/api/v1/servers/%d/roles", w.serverID), w.stranger, http.StatusForbidden)

	// member 无 ManageRoles：创建 403。
	postJSON(t, h, fmt.Sprintf("/api/v1/servers/%d/roles", w.serverID), w.member,
		map[string]string{"name": "x", "permissions": "3"}, http.StatusForbidden)

	// builtin 改/删 403（admin 也一样）。
	everyone := list[0].(map[string]any)
	everyoneID := asID(everyone["id"])
	doJSON(t, h, http.MethodPatch, fmt.Sprintf("/api/v1/roles/%d", everyoneID), w.admin,
		map[string]any{"name": "改名", "permissions": "1"}, http.StatusForbidden)
	doJSON(t, h, http.MethodDelete, fmt.Sprintf("/api/v1/roles/%d", everyoneID), w.admin, nil, http.StatusForbidden)
	// owner builtin 同样。
	doJSON(t, h, http.MethodDelete, fmt.Sprintf("/api/v1/roles/%d", asID(list[2].(map[string]any)["id"])), w.owner, nil, http.StatusForbidden)

	// 资源不存在 404。
	doJSON(t, h, http.MethodPatch, "/api/v1/roles/4242424242", w.owner,
		map[string]any{"name": "x", "permissions": "1"}, http.StatusNotFound)
	getJSON(t, h, "/api/v1/servers/4242424242/roles", w.owner, http.StatusNotFound)
}

// TestRoleHierarchy 防提权：低职位不能建/改/删不低于自己的角色，也不能越权授予权限位。
func TestRoleHierarchy(t *testing.T) {
	w := newPermWorld2(t)
	h := w.h
	g := w.granter

	// granter 最高位 9：position 0–8 可建，9 起 403。
	ok := postJSON(t, h, fmt.Sprintf("/api/v1/servers/%d/roles", w.serverID), g,
		map[string]any{"name": "低位", "position": 8, "permissions": permStr(store.Speak)}, http.StatusCreated)
	okID := asID(ok["id"])
	postJSON(t, h, fmt.Sprintf("/api/v1/servers/%d/roles", w.serverID), g,
		map[string]any{"name": "同位", "position": 9, "permissions": permStr(store.Speak)}, http.StatusForbidden)
	postJSON(t, h, fmt.Sprintf("/api/v1/servers/%d/roles", w.serverID), g,
		map[string]any{"name": "高位", "position": 10, "permissions": permStr(store.Speak)}, http.StatusForbidden)

	// 越权授予自己没有的权限位（granter 无 KickMembers）。
	postJSON(t, h, fmt.Sprintf("/api/v1/servers/%d/roles", w.serverID), g,
		map[string]any{"name": "越权", "position": 1, "permissions": permStr(store.KickMembers)}, http.StatusForbidden)
	// 未定义位 400。
	postJSON(t, h, fmt.Sprintf("/api/v1/servers/%d/roles", w.serverID), g,
		map[string]any{"name": "未定义位", "position": 1, "permissions": "99999"}, http.StatusBadRequest)
	// 非法串 400。
	postJSON(t, h, fmt.Sprintf("/api/v1/servers/%d/roles", w.serverID), g,
		map[string]any{"name": "坏串", "position": 1, "permissions": "abc"}, http.StatusBadRequest)
	// 非法名/负 position 400。
	postJSON(t, h, fmt.Sprintf("/api/v1/servers/%d/roles", w.serverID), g,
		map[string]any{"name": "", "position": 1, "permissions": "8"}, http.StatusBadRequest)
	postJSON(t, h, fmt.Sprintf("/api/v1/servers/%d/roles", w.serverID), g,
		map[string]any{"name": "负位", "position": -1, "permissions": "8"}, http.StatusBadRequest)

	// 更新：改不低于自己的角色 403（grantRole position=9）；低位角色可改。
	doJSON(t, h, http.MethodPatch, fmt.Sprintf("/api/v1/roles/%d", w.grantRole.ID), g,
		map[string]any{"name": "篡位", "permissions": permStr(store.Speak)}, http.StatusForbidden)
	doJSON(t, h, http.MethodPatch, fmt.Sprintf("/api/v1/roles/%d", okID), g,
		map[string]any{"name": "低位改", "position": 7, "permissions": permStr(store.Speak | store.ViewChannel)}, http.StatusOK)
	// 抬到自己同位 403。
	doJSON(t, h, http.MethodPatch, fmt.Sprintf("/api/v1/roles/%d", okID), g,
		map[string]any{"name": "低位改", "position": 9, "permissions": permStr(store.Speak)}, http.StatusForbidden)
	// 删除同理。
	doJSON(t, h, http.MethodDelete, fmt.Sprintf("/api/v1/roles/%d", w.grantRole.ID), g, nil, http.StatusForbidden)
	doJSON(t, h, http.MethodDelete, fmt.Sprintf("/api/v1/roles/%d", okID), g, nil, http.StatusOK)

	// owner 不受层级限制：position 100 + 全权限。
	postJSON(t, h, fmt.Sprintf("/api/v1/servers/%d/roles", w.serverID), w.owner,
		map[string]any{"name": "服主的", "position": 100, "permissions": permStr(store.AllPermissions)}, http.StatusCreated)

	// admin 最高位=1：position 0 可建、position >= 1 拒（与规范「admin 不能创建
	// position>=1 的角色」对齐）。
	postJSON(t, h, fmt.Sprintf("/api/v1/servers/%d/roles", w.serverID), w.admin,
		map[string]any{"name": "admin低位", "position": 0, "permissions": permStr(store.Speak)}, http.StatusCreated)
	postJSON(t, h, fmt.Sprintf("/api/v1/servers/%d/roles", w.serverID), w.admin,
		map[string]any{"name": "admin同位", "position": 1, "permissions": permStr(store.Speak)}, http.StatusForbidden)
}

// TestMemberRolesAssignment 成员角色分配：owner 不可分配、层级限制、owner 目标
// 强制保留、role_ids/legacy 投影。
func TestMemberRolesAssignment(t *testing.T) {
	w := newPermWorld2(t)
	h := w.h

	// 取 builtin ID。
	roles := getJSON(t, h, fmt.Sprintf("/api/v1/servers/%d/roles", w.serverID), w.owner, http.StatusOK).([]any)
	var everyoneID, adminID, ownerID int64
	for _, raw := range roles {
		r := raw.(map[string]any)
		switch r["name"] {
		case "@everyone":
			everyoneID = asID(r["id"])
		case "管理员":
			adminID = asID(r["id"])
		case "服主":
			ownerID = asID(r["id"])
		}
	}
	memberID := meID(t, h, w.member)

	// owner builtin 不可分配。
	doJSON(t, h, http.MethodPut, fmt.Sprintf("/api/v1/servers/%d/members/%d/roles", w.serverID, memberID), w.owner,
		map[string]any{"role_ids": []string{fmt.Sprint(ownerID)}}, http.StatusForbidden)
	// 非法/跨服 role_ids 404。
	doJSON(t, h, http.MethodPut, fmt.Sprintf("/api/v1/servers/%d/members/%d/roles", w.serverID, memberID), w.owner,
		map[string]any{"role_ids": []string{"4242424242"}}, http.StatusNotFound)
	// 目标非成员 404。
	doJSON(t, h, http.MethodPut, fmt.Sprintf("/api/v1/servers/%d/members/4242424242/roles", w.serverID), w.owner,
		map[string]any{"role_ids": []string{}}, http.StatusNotFound)

	// granter（最高位 9）不能分配 admin builtin（position 1 < 9 可以？——admin 权限位
	// 32767 含 granter 没有的位，子集检查 403）。
	doJSON(t, h, http.MethodPut, fmt.Sprintf("/api/v1/servers/%d/members/%d/roles", w.serverID, memberID), w.granter,
		map[string]any{"role_ids": []string{fmt.Sprint(adminID)}}, http.StatusForbidden)
	// granter 不能分配自己同位的 grantRole（position 9 不小于 9）。
	doJSON(t, h, http.MethodPut, fmt.Sprintf("/api/v1/servers/%d/members/%d/roles", w.serverID, memberID), w.granter,
		map[string]any{"role_ids": []string{fmt.Sprint(w.grantRole.ID)}}, http.StatusForbidden)

	// owner 分配 admin builtin：成功，legacy 投影 → admin，role_ids 含 admin。
	doJSON(t, h, http.MethodPut, fmt.Sprintf("/api/v1/servers/%d/members/%d/roles", w.serverID, memberID), w.owner,
		map[string]any{"role_ids": []string{fmt.Sprint(adminID)}}, http.StatusOK)
	members := getJSON(t, h, fmt.Sprintf("/api/v1/servers/%d/members", w.serverID), w.owner, http.StatusOK).([]any)
	for _, raw := range members {
		m := raw.(map[string]any)
		if asID(m["user_id"]) != memberID {
			continue
		}
		if m["role"] != "admin" {
			t.Fatalf("legacy 投影应 admin，实际 %v", m["role"])
		}
		found := false
		for _, id := range m["role_ids"].([]any) {
			if asID(id) == adminID {
				found = true
			}
		}
		if !found {
			t.Fatalf("role_ids 应含 admin builtin: %v", m["role_ids"])
		}
	}

	// owner 目标：owner builtin 强制保留，不能被降级。
	doJSON(t, h, http.MethodPut, fmt.Sprintf("/api/v1/servers/%d/members/%d/roles", w.serverID, w.ownerIDNum(t)), w.owner,
		map[string]any{"role_ids": []string{fmt.Sprint(everyoneID)}}, http.StatusOK)
	members = getJSON(t, h, fmt.Sprintf("/api/v1/servers/%d/members", w.serverID), w.owner, http.StatusOK).([]any)
	for _, raw := range members {
		m := raw.(map[string]any)
		if asID(m["user_id"]) != w.ownerIDNum(t) {
			continue
		}
		if m["role"] != "owner" {
			t.Fatalf("owner 投影不得降级，实际 %v", m["role"])
		}
		found := false
		for _, id := range m["role_ids"].([]any) {
			if asID(id) == ownerID {
				found = true
			}
		}
		if !found {
			t.Fatalf("owner 的 owner 角色应被强制保留: %v", m["role_ids"])
		}
	}
}

// ownerIDNum 服主的数字 ID。
func (w *permWorld) ownerIDNum(t *testing.T) int64 {
	t.Helper()
	return meID(t, w.h, w.owner)
}

// TestOverwritesAPI 覆盖 set/get/delete、越界位、跨服目标、变更后可见性立即变化。
func TestOverwritesAPI(t *testing.T) {
	w := newPermWorld(t)
	h := w.h
	ch := postJSON(t, h, fmt.Sprintf("/api/v1/servers/%d/channels", w.serverID), w.owner,
		map[string]string{"name": "覆盖频道", "type": "text"}, http.StatusCreated)
	chID := asID(ch["id"])
	roles := getJSON(t, h, fmt.Sprintf("/api/v1/servers/%d/roles", w.serverID), w.owner, http.StatusOK).([]any)
	everyoneID := asID(roles[0].(map[string]any)["id"])
	memberID := meID(t, h, w.member)

	// 无 ManageChannels：列表/写 403。
	getJSON(t, h, fmt.Sprintf("/api/v1/channels/%d/overwrites", chID), w.member, http.StatusForbidden)
	doJSON(t, h, http.MethodPut, fmt.Sprintf("/api/v1/channels/%d/overwrites/member/%d", chID, memberID), w.member,
		map[string]string{"allow": "1", "deny": "0"}, http.StatusForbidden)

	// 越界位/非法串 400。
	doJSON(t, h, http.MethodPut, fmt.Sprintf("/api/v1/channels/%d/overwrites/member/%d", chID, memberID), w.owner,
		map[string]string{"allow": "1024", "deny": "0"}, http.StatusBadRequest) // ManageRoles 不是频道位
	doJSON(t, h, http.MethodPut, fmt.Sprintf("/api/v1/channels/%d/overwrites/member/%d", chID, memberID), w.owner,
		map[string]string{"allow": "abc", "deny": "0"}, http.StatusBadRequest)
	doJSON(t, h, http.MethodPut, fmt.Sprintf("/api/v1/channels/%d/overwrites/member/%d", chID, memberID), w.owner,
		map[string]string{"allow": "99999", "deny": "0"}, http.StatusBadRequest)
	// 非法 target_type 400。
	doJSON(t, h, http.MethodPut, fmt.Sprintf("/api/v1/channels/%d/overwrites/group/%d", chID, memberID), w.owner,
		map[string]string{"allow": "1", "deny": "0"}, http.StatusBadRequest)

	// 跨服目标 404：在另一服建角色，覆盖到本服频道。
	other := postJSON(t, h, "/api/v1/servers", w.owner, map[string]string{"name": "外服"}, http.StatusCreated)
	otherID := asID(other["id"])
	otherRole := postJSON(t, h, fmt.Sprintf("/api/v1/servers/%d/roles", otherID), w.owner,
		map[string]any{"name": "外", "position": 0, "permissions": "8"}, http.StatusCreated)
	doJSON(t, h, http.MethodPut, fmt.Sprintf("/api/v1/channels/%d/overwrites/role/%d", chID, asID(otherRole["id"])), w.owner,
		map[string]string{"allow": "1", "deny": "0"}, http.StatusNotFound)
	// 非成员目标 404。
	doJSON(t, h, http.MethodPut, fmt.Sprintf("/api/v1/channels/%d/overwrites/member/%d", chID, w.ownerIDNum(t)+999999), w.owner,
		map[string]string{"allow": "1", "deny": "0"}, http.StatusNotFound)

	// set + 列表 + 可见性立即变化（缓存失效）。
	doJSON(t, h, http.MethodPut, fmt.Sprintf("/api/v1/channels/%d/overwrites/role/%d", chID, everyoneID), w.owner,
		map[string]string{"allow": "0", "deny": "1"}, http.StatusOK) // deny ViewChannel
	getJSON(t, h, fmt.Sprintf("/api/v1/channels/%d/messages", chID), w.member, http.StatusForbidden)
	ows := getJSON(t, h, fmt.Sprintf("/api/v1/channels/%d/overwrites", chID), w.owner, http.StatusOK).([]any)
	if len(ows) != 1 {
		t.Fatalf("应 1 条覆盖，实际 %v", ows)
	}
	ow := ows[0].(map[string]any)
	if ow["deny"] != "1" || ow["target_type"] != "role" {
		t.Fatalf("覆盖内容不符: %v", ow)
	}

	// delete 后立即恢复。
	doJSON(t, h, http.MethodDelete, fmt.Sprintf("/api/v1/channels/%d/overwrites/role/%d", chID, everyoneID), w.owner, nil, http.StatusOK)
	getJSON(t, h, fmt.Sprintf("/api/v1/channels/%d/messages", chID), w.member, http.StatusOK)
	// 二次删除 404。
	doJSON(t, h, http.MethodDelete, fmt.Sprintf("/api/v1/channels/%d/overwrites/role/%d", chID, everyoneID), w.owner, nil, http.StatusNotFound)
	// 频道不存在 404。
	getJSON(t, h, "/api/v1/channels/4242424242/overwrites", w.owner, http.StatusNotFound)
}

// TestAuditLogAPI 审计：操作后可查、权限门、分页。
func TestAuditLogAPI(t *testing.T) {
	w := newPermWorld2(t)
	h := w.h

	// 触发一批审计动作。
	doJSON(t, h, http.MethodPatch, fmt.Sprintf("/api/v1/servers/%d", w.serverID), w.owner,
		map[string]string{"name": "审计服"}, http.StatusOK)
	postJSON(t, h, fmt.Sprintf("/api/v1/servers/%d/roles", w.serverID), w.owner,
		map[string]any{"name": "审计角色", "position": 3, "permissions": "8"}, http.StatusCreated)
	ch := postJSON(t, h, fmt.Sprintf("/api/v1/servers/%d/channels", w.serverID), w.owner,
		map[string]string{"name": "审计频道", "type": "text"}, http.StatusCreated)
	_ = ch

	// 无 ManageServer（granter 只有 ManageRoles 等）→ 403；成员 403。
	getJSON(t, h, fmt.Sprintf("/api/v1/servers/%d/audit-log", w.serverID), w.granter, http.StatusForbidden)
	getJSON(t, h, fmt.Sprintf("/api/v1/servers/%d/audit-log", w.serverID), w.member, http.StatusForbidden)

	logs := getJSON(t, h, fmt.Sprintf("/api/v1/servers/%d/audit-log", w.serverID), w.owner, http.StatusOK).([]any)
	if len(logs) < 3 {
		t.Fatalf("至少 3 条审计，实际 %d: %v", len(logs), logs)
	}
	// 倒序：最新在前（role.create 晚于 server.update）。
	first := logs[0].(map[string]any)
	if first["action"] != "channel.create" {
		t.Fatalf("首条应最新动作 channel.create，实际 %v", first["action"])
	}
	// detail 是 JSON 串且不含隐私字段（抽 server.update 条目验）。
	for _, raw := range logs {
		e := raw.(map[string]any)
		if e["action"] == "server.update" {
			if detail, _ := e["detail"].(string); detail == "" || !strings.Contains(detail, "name") {
				t.Fatalf("server.update detail 应含变更字段: %v", e["detail"])
			}
		}
	}

	// 分页：limit=1 只取最新；before 翻页。
	page := getJSON(t, h, fmt.Sprintf("/api/v1/servers/%d/audit-log?limit=1", w.serverID), w.owner, http.StatusOK).([]any)
	if len(page) != 1 {
		t.Fatalf("limit=1 应 1 条，实际 %v", page)
	}
	before := fmt.Sprint(page[0].(map[string]any)["id"])
	page2 := getJSON(t, h, fmt.Sprintf("/api/v1/servers/%d/audit-log?limit=1&before=%s", w.serverID, before), w.owner, http.StatusOK).([]any)
	if len(page2) != 1 || fmt.Sprint(page2[0].(map[string]any)["id"]) == before {
		t.Fatalf("before 翻页应取更旧条目: %v", page2)
	}
	// limit 越界 400。
	getJSON(t, h, fmt.Sprintf("/api/v1/servers/%d/audit-log?limit=0", w.serverID), w.owner, http.StatusBadRequest)
	getJSON(t, h, fmt.Sprintf("/api/v1/servers/%d/audit-log?limit=51", w.serverID), w.owner, http.StatusBadRequest)
}
