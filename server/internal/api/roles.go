package api

import (
	"context"
	"errors"
	"math"
	"net/http"
	"strconv"

	"github.com/OpenALC/LiteTalk/server/internal/store"
)

// #35 B4：角色管理 REST。层级防提权在 API 层强制——Store 只保 builtin 不可改删，
// 「低职位不能动高职位」必须在这里拦。

// roleRank 操作者在角色层级中的位置（#35 B4）。
type roleRank struct {
	owner bool // servers.owner_id == uid：不受层级限制
	max   int  // 最高角色 position；owner 视为 MaxInt
}

// roleRankOf 计算操作者的层级基准：owner 不受限；非 owner 取其角色最高 position
// （RolesOfMember 含 @everyone；空列表按 legacy member=0/admin=1/owner=2 回退）。
func (s *Server) roleRankOf(ctx context.Context, serverID, userID int64) (roleRank, *apiErr) {
	srv, err := s.store.ServerByID(ctx, serverID)
	if err != nil {
		return roleRank{}, s.writeStoreErr(ctx, err)
	}
	if srv.OwnerID == userID {
		return roleRank{owner: true, max: math.MaxInt}, nil
	}
	roles, err := s.store.RolesOfMember(ctx, serverID, userID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return roleRank{}, errf(http.StatusForbidden, "forbidden", "不是服务器成员")
		}
		return roleRank{}, s.writeStoreErr(ctx, err)
	}
	max, e := s.maxPosition(ctx, serverID, userID, roles)
	if e != nil {
		return roleRank{}, e
	}
	return roleRank{max: max}, nil
}

// targetMaxRank 目标成员当前角色的最高 position。非成员 404——目标侧的错误口径
// 与操作者侧（403）不同：跨服/移出成员不出存在性（#35 B4 追加层级闸）。
func (s *Server) targetMaxRank(ctx context.Context, serverID, userID int64) (int, *apiErr) {
	roles, err := s.store.RolesOfMember(ctx, serverID, userID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return 0, errf(http.StatusNotFound, "not_found", "资源不存在")
		}
		return 0, s.writeStoreErr(ctx, err)
	}
	return s.maxPosition(ctx, serverID, userID, roles)
}

// maxPosition 角色列表的最高 position；空列表按 legacy member=0/admin=1/owner=2 回退
// （B1 之后正常数据不会走到；0010 未回填的旧库可能）。
func (s *Server) maxPosition(ctx context.Context, serverID, userID int64, roles []store.Role) (int, *apiErr) {
	max := -1
	for _, r := range roles {
		if r.Position > max {
			max = r.Position
		}
	}
	if len(roles) == 0 {
		mem, err := s.store.Member(ctx, serverID, userID)
		if err != nil {
			return 0, s.writeStoreErr(ctx, err)
		}
		switch mem.Role {
		case store.RoleOwner:
			max = 2
		case store.RoleAdmin:
			max = 1
		default:
			max = 0
		}
	}
	return max, nil
}

// guardRoleBody 防提权（#35 B4）：非 owner 的 position 必须严格小于自己最高位，
// 权限位必须是操作者权限的子集（逐位 CanServer，Administrator/owner 隐含全过）。
func (s *Server) guardRoleBody(ctx context.Context, rr roleRank, serverID, userID int64, position int, perms store.Permission) *apiErr {
	if rr.owner {
		return nil
	}
	if position >= rr.max {
		return errf(http.StatusForbidden, "forbidden", "不能创建或改动不低于自己的角色")
	}
	return s.guardPermSubset(ctx, userID, serverID, perms)
}

// guardPermSubset 请求的每个权限位都要操作者自己有（防越权授予）。
func (s *Server) guardPermSubset(ctx context.Context, userID, serverID int64, perms store.Permission) *apiErr {
	for p := store.Permission(1); p <= store.AllPermissions; p <<= 1 {
		if perms&p == 0 {
			continue
		}
		ok, err := s.authz.CanServer(ctx, userID, serverID, p)
		if err != nil {
			return s.writeStoreErr(ctx, err)
		}
		if !ok {
			return errf(http.StatusForbidden, "forbidden", "不能授予自己不具备的权限")
		}
	}
	return nil
}

// requireManageInOwnerServer 隐式服务器资源的权限门（#35 B4）：
// 不是该服成员 → 404（跨服资源不泄露存在性）；成员但权限不足 → 403。
func (s *Server) requireManageInOwnerServer(ctx context.Context, serverID, userID int64, perm store.Permission) *apiErr {
	ok, err := s.authz.IsMember(ctx, userID, serverID)
	if err != nil {
		return s.writeStoreErr(ctx, err)
	}
	if !ok {
		return errf(http.StatusNotFound, "not_found", "资源不存在")
	}
	return s.requirePerm(ctx, serverID, userID, perm)
}

// roleView store 角色 → wire（permissions 十进制字符串）。
func roleView(r *store.Role) Role {
	return Role{
		Id:          idOf(r.ID),
		ServerId:    idOf(r.ServerID),
		Name:        r.Name,
		Color:       r.Color,
		Position:    r.Position,
		Permissions: strconv.FormatUint(uint64(r.Permissions), 10),
		Builtin:     r.IsBuiltin(),
		CreatedAt:   timeOf(r.CreatedAt),
		UpdatedAt:   timeOf(r.UpdatedAt),
	}
}

func roleViews(list []store.Role) []Role {
	out := make([]Role, 0, len(list))
	for i := range list {
		out = append(out, roleView(&list[i]))
	}
	return out
}

// parsePerm 读十进制权限位串；非法串与未定义位拒 400（body 非法，不是权限问题）。
func parsePerm(s string) (store.Permission, *apiErr) {
	if s == "" {
		return 0, errf(http.StatusBadRequest, "bad_permissions", "权限位串不能为空")
	}
	v, err := strconv.ParseUint(s, 10, 64)
	if err != nil {
		return 0, errf(http.StatusBadRequest, "bad_permissions", "权限位串非法")
	}
	p := store.Permission(v)
	if !p.Valid() {
		return 0, errf(http.StatusBadRequest, "bad_permissions", "含未定义权限位")
	}
	return p, nil
}

// formatPerm 权限位 → 十进制串（wire 形态）。
func formatPerm(p store.Permission) string {
	return strconv.FormatUint(uint64(p), 10)
}

// parseOverwritePerm 覆盖位只允许频道位（管理位属于角色）。
func parseOverwritePerm(s string) (store.Permission, *apiErr) {
	p, e := parsePerm(s)
	if e != nil {
		return 0, e
	}
	if p&^store.ChannelPermissionMask != 0 {
		return 0, errf(http.StatusBadRequest, "bad_permissions", "覆盖只允许频道位")
	}
	return p, nil
}

// validRoleName 角色名 1–32 字符。
func validRoleName(s string) bool {
	n := len([]rune(s))
	return n >= 1 && n <= 32
}

// validRolePosition position 必须在 DB 列范围内（INT32）：越界写库报错会变成 500，
// 在这里挡成 400。
func validRolePosition(p int) *apiErr {
	if p < 0 || p > math.MaxInt32 {
		return errf(http.StatusBadRequest, "bad_position", "position 超出范围")
	}
	return nil
}

// ListRoles GET /api/v1/servers/{id}/roles（成员可读）
func (s *Server) ListRoles(ctx context.Context, request ListRolesRequestObject) (ListRolesResponseObject, error) {
	if e := checkID(request.Id); e != nil {
		return nil, e
	}
	c := currentUser(ctx)
	if e := s.requireMember(ctx, request.Id, c.UserID); e != nil {
		return nil, e
	}
	roles, err := s.store.ListRoles(ctx, request.Id)
	if err != nil {
		return nil, s.writeStoreErr(ctx, err)
	}
	return ListRoles200JSONResponse(roleViews(roles)), nil
}

// CreateRole POST /api/v1/servers/{id}/roles（需 ManageRoles）
func (s *Server) CreateRole(ctx context.Context, request CreateRoleRequestObject) (CreateRoleResponseObject, error) {
	if e := checkID(request.Id); e != nil {
		return nil, e
	}
	c := currentUser(ctx)
	if e := s.requirePerm(ctx, request.Id, c.UserID, store.ManageRoles); e != nil {
		return nil, e
	}
	if request.Body == nil {
		return nil, bodyMissing()
	}
	req := request.Body
	if !validRoleName(req.Name) {
		return nil, errf(http.StatusBadRequest, "bad_name", "角色名需为 1–32 个字符")
	}
	color := ""
	if req.Color != nil {
		if !validIconColor(*req.Color) {
			return nil, errf(http.StatusBadRequest, "bad_color", "颜色不合法")
		}
		color = *req.Color
	}
	perms, e := parsePerm(req.Permissions)
	if e != nil {
		return nil, e
	}
	position := 0
	if req.Position != nil {
		position = *req.Position
	}
	if e := validRolePosition(position); e != nil {
		return nil, e
	}
	rr, e := s.roleRankOf(ctx, request.Id, c.UserID)
	if e != nil {
		return nil, e
	}
	if e := s.guardRoleBody(ctx, rr, request.Id, c.UserID, position, perms); e != nil {
		return nil, e
	}
	role := &store.Role{
		ServerID: request.Id, Name: req.Name, Color: color,
		Position: position, Permissions: perms,
	}
	if err := s.store.CreateRole(ctx, role); err != nil {
		return nil, s.writeStoreErr(ctx, err)
	}
	// 角色并集参与权限判定：快照作废。
	s.authz.InvalidateServer(request.Id)
	s.audit(ctx, store.AuditLog{
		ServerID: request.Id, ActorID: c.UserID, Action: "role.create",
		TargetType: "role", TargetID: &role.ID,
		Detail: auditDetail(map[string]any{
			"name": role.Name, "position": role.Position, "permissions": req.Permissions,
		}),
	})
	return CreateRole201JSONResponse(roleView(role)), nil
}

// UpdateRole PATCH /api/v1/roles/{id}（需 ManageRoles；builtin 403）
func (s *Server) UpdateRole(ctx context.Context, request UpdateRoleRequestObject) (UpdateRoleResponseObject, error) {
	if e := checkID(request.Id); e != nil {
		return nil, e
	}
	c := currentUser(ctx)
	role, err := s.store.RoleByID(ctx, request.Id)
	if err != nil {
		return nil, s.writeStoreErr(ctx, err)
	}
	// 跨服资源不出存在性（#35 B4）；builtin 的存在性不敏感，直接 403。
	if role.IsBuiltin() {
		return nil, errf(http.StatusForbidden, "forbidden", "内置角色不可修改")
	}
	if e := s.requireManageInOwnerServer(ctx, role.ServerID, c.UserID, store.ManageRoles); e != nil {
		return nil, e
	}
	if request.Body == nil {
		return nil, bodyMissing()
	}
	req := request.Body
	if !validRoleName(req.Name) {
		return nil, errf(http.StatusBadRequest, "bad_name", "角色名需为 1–32 个字符")
	}
	// 缺省字段沿用现值（Store 全量写，这里补缺再落笔）。
	upd := *role
	upd.Name = req.Name
	if req.Color != nil {
		if !validIconColor(*req.Color) {
			return nil, errf(http.StatusBadRequest, "bad_color", "颜色不合法")
		}
		upd.Color = *req.Color
	}
	if req.Position != nil {
		if e := validRolePosition(*req.Position); e != nil {
			return nil, e
		}
		upd.Position = *req.Position
	}
	perms, e := parsePerm(req.Permissions)
	if e != nil {
		return nil, e
	}
	upd.Permissions = perms
	// 层级对「现位置」与「目标位置」都生效：改不动比自己高的角色，也不能把自己抬上去。
	rr, e := s.roleRankOf(ctx, role.ServerID, c.UserID)
	if e != nil {
		return nil, e
	}
	if e := s.guardRoleBody(ctx, rr, role.ServerID, c.UserID, role.Position, perms); e != nil {
		return nil, e
	}
	if e := s.guardRoleBody(ctx, rr, role.ServerID, c.UserID, upd.Position, perms); e != nil {
		return nil, e
	}
	if err := s.store.UpdateRole(ctx, &upd); err != nil {
		return nil, s.writeStoreErr(ctx, err)
	}
	s.authz.InvalidateServer(role.ServerID)
	s.audit(ctx, store.AuditLog{
		ServerID: role.ServerID, ActorID: c.UserID, Action: "role.update",
		TargetType: "role", TargetID: &upd.ID,
		Detail: auditDetail(map[string]any{
			"name": upd.Name, "position": upd.Position, "permissions": req.Permissions,
		}),
	})
	return UpdateRole200JSONResponse(roleView(&upd)), nil
}

// DeleteRole DELETE /api/v1/roles/{id}（需 ManageRoles；builtin 403）
func (s *Server) DeleteRole(ctx context.Context, request DeleteRoleRequestObject) (DeleteRoleResponseObject, error) {
	if e := checkID(request.Id); e != nil {
		return nil, e
	}
	c := currentUser(ctx)
	role, err := s.store.RoleByID(ctx, request.Id)
	if err != nil {
		return nil, s.writeStoreErr(ctx, err)
	}
	if role.IsBuiltin() {
		return nil, errf(http.StatusForbidden, "forbidden", "内置角色不可删除")
	}
	if e := s.requireManageInOwnerServer(ctx, role.ServerID, c.UserID, store.ManageRoles); e != nil {
		return nil, e
	}
	rr, e := s.roleRankOf(ctx, role.ServerID, c.UserID)
	if e != nil {
		return nil, e
	}
	if e := s.guardRoleBody(ctx, rr, role.ServerID, c.UserID, role.Position, 0); e != nil {
		return nil, e
	}
	if err := s.store.DeleteRole(ctx, role.ID); err != nil {
		return nil, s.writeStoreErr(ctx, err)
	}
	// 分配与覆盖级联删除：快照作废。
	s.authz.InvalidateServer(role.ServerID)
	s.audit(ctx, store.AuditLog{
		ServerID: role.ServerID, ActorID: c.UserID, Action: "role.delete",
		TargetType: "role", TargetID: &role.ID,
		Detail: auditDetail(map[string]any{"name": role.Name, "position": role.Position}),
	})
	return DeleteRole200JSONResponse{Status: "ok"}, nil
}

// SetMemberRoles PUT /api/v1/servers/{id}/members/{user_id}/roles（需 ManageRoles）
func (s *Server) SetMemberRoles(ctx context.Context, request SetMemberRolesRequestObject) (SetMemberRolesResponseObject, error) {
	if e := checkID(request.Id); e != nil {
		return nil, e
	}
	if e := checkID(request.UserId); e != nil {
		return nil, e
	}
	c := currentUser(ctx)
	if e := s.requirePerm(ctx, request.Id, c.UserID, store.ManageRoles); e != nil {
		return nil, e
	}
	if request.Body == nil {
		return nil, bodyMissing()
	}
	// 先把该服角色表拉齐：目标角色的归属/层级/权限位都在这里裁决。
	all, err := s.store.ListRoles(ctx, request.Id)
	if err != nil {
		return nil, s.writeStoreErr(ctx, err)
	}
	byID := make(map[int64]store.Role, len(all))
	ownerBuiltinID := int64(0)
	for i := range all {
		byID[all[i].ID] = all[i]
		if all[i].BuiltinKey == store.BuiltinKeyOwner {
			ownerBuiltinID = all[i].ID
		}
	}
	rr, e := s.roleRankOf(ctx, request.Id, c.UserID)
	if e != nil {
		return nil, e
	}
	// 目标层级闸（#35 验收追加）：非 owner 只能动比自己低的成员——否则同级 admin
	// 提交 role_ids: [] 就能把另一个 admin 降级（Store 只强制 @everyone）。
	// 目标非成员 404；owner 操作者不受限（Store 对 owner 目标另有强制保留兜底）。
	if !rr.owner {
		tRank, e := s.targetMaxRank(ctx, request.Id, request.UserId)
		if e != nil {
			return nil, e
		}
		if tRank >= rr.max {
			return nil, errf(http.StatusForbidden, "forbidden", "不能改动不低于自己的成员")
		}
	}
	want := make([]int64, 0, len(request.Body.RoleIds))
	seen := map[int64]bool{}
	for _, id := range request.Body.RoleIds {
		rid := int64(id)
		if e := checkID(rid); e != nil {
			return nil, e
		}
		if seen[rid] {
			continue
		}
		seen[rid] = true
		r, ok := byID[rid]
		if !ok {
			// 不存在或跨服角色：统一不出存在性。
			return nil, errf(http.StatusNotFound, "not_found", "角色不存在")
		}
		// owner builtin 不可分配（owner 身份只由 servers.owner_id 决定）。
		if ownerBuiltinID != 0 && rid == ownerBuiltinID {
			return nil, errf(http.StatusForbidden, "forbidden", "owner 角色不可分配")
		}
		// @everyone（member builtin）强制保留、不参与层级判定；其余角色须低于自己。
		if r.BuiltinKey != store.BuiltinKeyMember {
			if e := s.guardRoleBody(ctx, rr, request.Id, c.UserID, r.Position, r.Permissions); e != nil {
				return nil, e
			}
		}
		want = append(want, rid)
	}
	if err := s.store.SetMemberRoles(ctx, request.Id, request.UserId, want, c.UserID); err != nil {
		return nil, s.writeStoreErr(ctx, err)
	}
	s.authz.InvalidateServer(request.Id)
	s.audit(ctx, store.AuditLog{
		ServerID: request.Id, ActorID: c.UserID, Action: "member.roles.update",
		TargetType: "member", TargetID: ptrID(request.UserId),
		Detail: auditDetail(map[string]any{"role_ids": request.Body.RoleIds}),
	})
	return SetMemberRoles200JSONResponse{Status: "ok"}, nil
}

// ptrID 审计 target 用（wire ID 指针）。
func ptrID(v int64) *int64 {
	p := v
	return &p
}
