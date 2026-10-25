package api

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/OpenALC/LiteTalk/server/internal/store"
)

// 服务器名称与图标校验（§5.4：图标为预设 Lucide 图标+颜色）。
var allowedIcons = map[string]bool{
	"messages-square": true, "gamepad-2": true, "music": true, "palette": true,
	"book-open": true, "coffee": true, "mic": true, "graduation-cap": true,
	"heart": true, "rocket": true, "camera": true, "dumbbell": true,
}

func validServerName(s string) bool {
	n := len([]rune(s))
	return n >= 2 && n <= 32
}

func validIcon(name, color string) bool {
	if !allowedIcons[name] {
		return false
	}
	return validIconColor(color)
}

// validIconColor 校验 #RRGGBB（角色/图标共用）。
func validIconColor(color string) bool {
	if len(color) != 7 || color[0] != '#' {
		return false
	}
	for i := 1; i < 7; i++ {
		c := color[i]
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f' || c >= 'A' && c <= 'F') {
			return false
		}
	}
	return true
}

// CreateServer POST /api/v1/servers
func (s *Server) CreateServer(ctx context.Context, request CreateServerRequestObject) (CreateServerResponseObject, error) {
	if request.Body == nil {
		return nil, bodyMissing()
	}
	c := currentUser(ctx)
	req := request.Body
	if !validServerName(req.Name) {
		return nil, errf(http.StatusBadRequest, "bad_name", "名称需为 2–32 个字符")
	}
	name, color := req.Name, "#4FA4F3"
	if req.IconColor != nil && *req.IconColor != "" {
		color = *req.IconColor
	}
	icon := "messages-square"
	if req.IconName != nil && *req.IconName != "" {
		icon = *req.IconName
	}
	if !validIcon(icon, color) {
		return nil, errf(http.StatusBadRequest, "bad_icon", "图标或颜色不合法")
	}
	srv := &store.Server{
		Name:      name,
		OwnerID:   c.UserID,
		IconName:  icon,
		IconColor: color,
	}
	if err := s.store.CreateServer(ctx, srv); err != nil {
		return nil, s.writeStoreErr(ctx, err)
	}
	// 创建者自动成为 owner 成员。
	mem := &store.ServerMember{ServerID: srv.ID, UserID: c.UserID, Role: store.RoleOwner}
	if err := s.store.AddMember(ctx, mem); err != nil {
		return nil, s.writeStoreErr(ctx, err)
	}
	return CreateServer201JSONResponse(serverView(srv)), nil
}

// ListServers GET /api/v1/servers
func (s *Server) ListServers(ctx context.Context, request ListServersRequestObject) (ListServersResponseObject, error) {
	c := currentUser(ctx)
	servers, err := s.store.ServersOfUser(ctx, c.UserID)
	if err != nil {
		return nil, s.writeStoreErr(ctx, err)
	}
	return ListServers200JSONResponse(serverViews(servers)), nil
}

// PatchServer PATCH /api/v1/servers/{id}（需 ManageServer，#35 B3）
func (s *Server) PatchServer(ctx context.Context, request PatchServerRequestObject) (PatchServerResponseObject, error) {
	if e := checkID(request.Id); e != nil {
		return nil, e
	}
	c := currentUser(ctx)
	// 资源→权限→body 的顺序（#35 B3）：无权者拿不到字段规则的形状反馈。
	if e := s.requirePerm(ctx, request.Id, c.UserID, store.ManageServer); e != nil {
		return nil, e
	}
	req := patchBody(request.JSONBody, request.ApplicationMergePatchPlusJSONBody)
	if req == nil {
		return nil, bodyMissing()
	}
	srv, err := s.store.ServerByID(ctx, request.Id)
	if err != nil {
		return nil, s.writeStoreErr(ctx, err)
	}
	// Merge Patch（#31）：缺省不改，null = 清空（名称/图标不可清 → 校验拒 400）。
	// 审计只记变更字段（#35 B4）。
	detail := map[string]any{}
	if req.Name.Set {
		if !validServerName(req.Name.Val) {
			return nil, errf(http.StatusBadRequest, "bad_name", "名称需为 2–32 个字符")
		}
		srv.Name = req.Name.Val
		detail["name"] = req.Name.Val
	}
	if req.IconName.Set {
		srv.IconName = req.IconName.Val
		detail["icon_name"] = req.IconName.Val
	}
	if req.IconColor.Set {
		srv.IconColor = req.IconColor.Val
		detail["icon_color"] = req.IconColor.Val
	}
	if !validIcon(srv.IconName, srv.IconColor) {
		return nil, errf(http.StatusBadRequest, "bad_icon", "图标或颜色不合法")
	}
	if err := s.store.UpdateServer(ctx, srv); err != nil {
		return nil, s.writeStoreErr(ctx, err)
	}
	s.audit(ctx, store.AuditLog{
		ServerID: request.Id, ActorID: c.UserID, Action: "server.update",
		TargetType: "server", TargetID: ptrID(request.Id), Detail: auditDetail(detail),
	})
	return PatchServer200JSONResponse(serverView(srv)), nil
}

// DeleteServer DELETE /api/v1/servers/{id}（仅 owner）。
// 不走 ManageServer：管理员只是「代管」，拆服权只属于服主（#35 B3 保持 owner-only）。
func (s *Server) DeleteServer(ctx context.Context, request DeleteServerRequestObject) (DeleteServerResponseObject, error) {
	if e := checkID(request.Id); e != nil {
		return nil, e
	}
	c := currentUser(ctx)
	srv, err := s.store.ServerByID(ctx, request.Id)
	if err != nil {
		return nil, s.writeStoreErr(ctx, err)
	}
	if srv.OwnerID != c.UserID {
		return nil, errf(http.StatusForbidden, "forbidden", "仅服主可删除服务器")
	}
	// 删前记下频道 ID，删后逐个踢出房间成员（#22）。
	channels, _ := s.store.ChannelsOfServer(ctx, request.Id)
	if err := s.store.DeleteServer(ctx, request.Id); err != nil {
		return nil, s.writeStoreErr(ctx, err)
	}
	// 服务器没了，权限快照同步作废（否则后续查询吃到陈旧成员/角色快照）。
	s.authz.InvalidateServer(request.Id)
	if s.hub != nil {
		for _, ch := range channels {
			s.hub.KickChannel(strconv.FormatInt(ch.ID, 10), "server_deleted")
		}
	}
	return DeleteServer200JSONResponse{Status: "ok"}, nil
}

// JoinServer POST /api/v1/servers/join（邀请码）
func (s *Server) JoinServer(ctx context.Context, request JoinServerRequestObject) (JoinServerResponseObject, error) {
	if request.Body == nil {
		return nil, bodyMissing()
	}
	c := currentUser(ctx)
	if request.Body.Code == "" {
		return nil, errf(http.StatusBadRequest, "bad_request", "缺少邀请码")
	}
	inv, err := s.store.InviteByCode(ctx, request.Body.Code)
	if err != nil {
		return nil, errf(http.StatusNotFound, "bad_code", "邀请码无效")
	}
	if inv.ExpiresAt != nil && time.Now().After(*inv.ExpiresAt) {
		return nil, errf(http.StatusGone, "code_expired", "邀请码已过期")
	}
	// 已是成员直接返回。
	if _, err := s.store.Member(ctx, inv.ServerID, c.UserID); err == nil {
		srv, _ := s.store.ServerByID(ctx, inv.ServerID)
		return JoinServer200JSONResponse(serverView(srv)), nil
	}
	// 消费邀请码与加入成员同一事务（#22）：中途失败时邀请码次数不增长。
	if _, err := s.store.UseInviteAndAddMember(ctx, inv.Code, c.UserID, store.RoleMember); err != nil {
		switch {
		case errors.Is(err, store.ErrConflict):
			// 邀请码用尽；也可能是同一用户的并发请求已先一步加入，后者按幂等成功返回。
			if _, e := s.store.Member(ctx, inv.ServerID, c.UserID); e == nil {
				srv, e := s.store.ServerByID(ctx, inv.ServerID)
				if e != nil {
					return nil, s.writeStoreErr(ctx, e)
				}
				// 并发加入也算入服：快照里可能还是「非成员」，同步作废。
				s.authz.InvalidateServer(inv.ServerID)
				// 新成员以当前最新消息为已读水位，避免把全部历史看成未读（#40）。
				s.initServerReadStates(ctx, c.UserID, inv.ServerID)
				return JoinServer200JSONResponse(serverView(srv)), nil
			}
			return nil, errf(http.StatusConflict, "code_exhausted", "邀请码使用次数已达上限")
		case errors.Is(err, store.ErrExpired):
			return nil, errf(http.StatusGone, "code_expired", "邀请码已过期")
		case errors.Is(err, store.ErrNotFound):
			return nil, errf(http.StatusNotFound, "bad_code", "邀请码无效")
		}
		return nil, s.writeStoreErr(ctx, err)
	}
	srv, err := s.store.ServerByID(ctx, inv.ServerID)
	if err != nil {
		return nil, s.writeStoreErr(ctx, err)
	}
	// 入服改变成员/角色：权限快照立即作废，下一次请求即按新身份裁决。
	s.authz.InvalidateServer(inv.ServerID)
	// 新成员以当前最新消息为已读水位，避免把全部历史看成未读（#40）。
	s.initServerReadStates(ctx, c.UserID, inv.ServerID)
	return JoinServer200JSONResponse(serverView(srv)), nil
}

// ListServerMembers GET /api/v1/servers/{id}/members
func (s *Server) ListServerMembers(ctx context.Context, request ListServerMembersRequestObject) (ListServerMembersResponseObject, error) {
	if e := checkID(request.Id); e != nil {
		return nil, e
	}
	c := currentUser(ctx)
	// 必须是成员才能看成员列表。
	if e := s.requireMember(ctx, request.Id, c.UserID); e != nil {
		return nil, e
	}
	members, err := s.store.MembersOfServer(ctx, request.Id)
	if err != nil {
		return nil, s.writeStoreErr(ctx, err)
	}
	// ETag 协商（#31）：内容哈希弱标签，命中 If-None-Match 回 304。
	// 在线态是视图的一部分：presence 变化会让 ETag 变化（#11）。
	view := s.serverMemberViews(ctx, members)
	etag := contentETag(view)
	setETag(ctx, etag)
	if etagMatch(ifNoneMatch(ctx), etag) {
		return ListServerMembers304Response{}, nil
	}
	return ListServerMembers200JSONResponse(view), nil
}

// CreateInvite POST /api/v1/servers/{id}/invites（需 ManageServer，#35 B3）
func (s *Server) CreateInvite(ctx context.Context, request CreateInviteRequestObject) (CreateInviteResponseObject, error) {
	if e := checkID(request.Id); e != nil {
		return nil, e
	}
	c := currentUser(ctx)
	// 资源→权限→body（#35 B3）。
	if e := s.requirePerm(ctx, request.Id, c.UserID, store.ManageServer); e != nil {
		return nil, e
	}
	if request.Body == nil {
		return nil, bodyMissing()
	}
	req := request.Body
	// 邀请参数校验（#22）：max_uses 不可为负。
	if req.MaxUses != nil && *req.MaxUses < 0 {
		return nil, errf(http.StatusBadRequest, "bad_request", "max_uses 不可为负")
	}
	code, err := randomCode(12)
	if err != nil {
		return nil, s.writeStoreErr(ctx, err)
	}
	inv := &store.Invite{Code: code, ServerID: request.Id, CreatorID: c.UserID}
	if req.MaxUses != nil {
		inv.MaxUses = *req.MaxUses
	}
	if req.ExpiresIn != nil && *req.ExpiresIn > 0 {
		t := time.Now().Add(time.Duration(*req.ExpiresIn) * time.Second)
		inv.ExpiresAt = &t
	}
	if err := s.store.CreateInvite(ctx, inv); err != nil {
		return nil, s.writeStoreErr(ctx, err)
	}
	// 审计不记邀请码本身（凭据类，#35 B4）。
	s.audit(ctx, store.AuditLog{
		ServerID: request.Id, ActorID: c.UserID, Action: "invite.create",
		Detail: auditDetail(map[string]any{"max_uses": inv.MaxUses, "expires_in": req.ExpiresIn}),
	})
	return CreateInvite201JSONResponse(inviteView(inv)), nil
}

// requirePerm 服务器级权限门（#35 B3）：资源不存在 404（CanServer 透传 ErrNotFound），
// 非成员/权限不足 403，Store 故障 500——故障绝不伪装成 403。
func (s *Server) requirePerm(ctx context.Context, serverID, userID int64, perm store.Permission) *apiErr {
	if s.authz == nil {
		return errf(http.StatusInternalServerError, "internal", "权限服务未就绪")
	}
	ok, err := s.authz.CanServer(ctx, userID, serverID, perm)
	if err != nil {
		return s.writeStoreErr(ctx, err)
	}
	if !ok {
		return errf(http.StatusForbidden, "forbidden", "权限不足")
	}
	return nil
}

// requireMember 成员门（#35 B3）：服务器不存在 404，非成员 403，故障 500。
func (s *Server) requireMember(ctx context.Context, serverID, userID int64) *apiErr {
	if s.authz == nil {
		return errf(http.StatusInternalServerError, "internal", "权限服务未就绪")
	}
	ok, err := s.authz.IsMember(ctx, userID, serverID)
	if err != nil {
		return s.writeStoreErr(ctx, err)
	}
	if !ok {
		return errf(http.StatusForbidden, "forbidden", "不是服务器成员")
	}
	return nil
}

// requireChannelPerm 频道级权限门（#35 B3）：ch 由调用方先行解析（404 先于此发生）。
func (s *Server) requireChannelPerm(ctx context.Context, ch *store.Channel, userID int64, perm store.Permission) *apiErr {
	if s.authz == nil {
		return errf(http.StatusInternalServerError, "internal", "权限服务未就绪")
	}
	ok, err := s.authz.CanInServer(ctx, userID, ch.ServerID, ch.ID, perm)
	if err != nil {
		return s.writeStoreErr(ctx, err)
	}
	if !ok {
		return errf(http.StatusForbidden, "forbidden", "权限不足")
	}
	return nil
}

// visibleChannels 过滤出 uid 有 ViewChannel 的频道（#35 B3；categories 不过滤）。
// 单频道判定故障直接上抛 500——静默丢频道会把可用性问题伪装成权限问题。
func (s *Server) visibleChannels(ctx context.Context, uid, serverID int64, chans []store.Channel) ([]store.Channel, error) {
	if s.authz == nil {
		return nil, errf(http.StatusInternalServerError, "internal", "权限服务未就绪")
	}
	out := make([]store.Channel, 0, len(chans))
	for _, ch := range chans {
		ok, err := s.authz.CanInServer(ctx, uid, serverID, ch.ID, store.ViewChannel)
		if err != nil {
			return nil, err
		}
		if ok {
			out = append(out, ch)
		}
	}
	return out, nil
}

// randomCode 生成 n 字节随机 hex 邀请码。
func randomCode(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}
