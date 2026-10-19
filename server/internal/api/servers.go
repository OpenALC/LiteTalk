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
	// 简单校验 #RRGGBB。
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

// PatchServer PATCH /api/v1/servers/{id}（仅 owner/admin）
func (s *Server) PatchServer(ctx context.Context, request PatchServerRequestObject) (PatchServerResponseObject, error) {
	if e := checkID(request.Id); e != nil {
		return nil, e
	}
	req := patchBody(request.JSONBody, request.ApplicationMergePatchPlusJSONBody)
	if req == nil {
		return nil, bodyMissing()
	}
	c := currentUser(ctx)
	if e := s.requireRole(ctx, request.Id, c.UserID, store.RoleAdmin); e != nil {
		return nil, e
	}
	srv, err := s.store.ServerByID(ctx, request.Id)
	if err != nil {
		return nil, s.writeStoreErr(ctx, err)
	}
	// Merge Patch（#31）：缺省不改，null = 清空（名称/图标不可清 → 校验拒 400）。
	if req.Name.Set {
		if !validServerName(req.Name.Val) {
			return nil, errf(http.StatusBadRequest, "bad_name", "名称需为 2–32 个字符")
		}
		srv.Name = req.Name.Val
	}
	if req.IconName.Set {
		srv.IconName = req.IconName.Val
	}
	if req.IconColor.Set {
		srv.IconColor = req.IconColor.Val
	}
	if !validIcon(srv.IconName, srv.IconColor) {
		return nil, errf(http.StatusBadRequest, "bad_icon", "图标或颜色不合法")
	}
	if err := s.store.UpdateServer(ctx, srv); err != nil {
		return nil, s.writeStoreErr(ctx, err)
	}
	return PatchServer200JSONResponse(serverView(srv)), nil
}

// DeleteServer DELETE /api/v1/servers/{id}（仅 owner）
func (s *Server) DeleteServer(ctx context.Context, request DeleteServerRequestObject) (DeleteServerResponseObject, error) {
	if e := checkID(request.Id); e != nil {
		return nil, e
	}
	c := currentUser(ctx)
	// 仅 owner 可删（requireRole 的「不是成员 / 权限不足」文案区分保留）。
	if e := s.requireRole(ctx, request.Id, c.UserID, store.RoleOwner); e != nil {
		return nil, e
	}
	// 删前记下频道 ID，删后逐个踢出房间成员（#22）。
	channels, _ := s.store.ChannelsOfServer(ctx, request.Id)
	if err := s.store.DeleteServer(ctx, request.Id); err != nil {
		return nil, s.writeStoreErr(ctx, err)
	}
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
	return JoinServer200JSONResponse(serverView(srv)), nil
}

// ListServerMembers GET /api/v1/servers/{id}/members
func (s *Server) ListServerMembers(ctx context.Context, request ListServerMembersRequestObject) (ListServerMembersResponseObject, error) {
	if e := checkID(request.Id); e != nil {
		return nil, e
	}
	c := currentUser(ctx)
	// 必须是成员才能看成员列表。
	if _, err := s.store.Member(ctx, request.Id, c.UserID); err != nil {
		return nil, errf(http.StatusForbidden, "forbidden", "不是服务器成员")
	}
	members, err := s.store.MembersOfServer(ctx, request.Id)
	if err != nil {
		return nil, s.writeStoreErr(ctx, err)
	}
	// ETag 协商（#31）：内容哈希弱标签，命中 If-None-Match 回 304。
	view := memberViews(members)
	etag := contentETag(view)
	setETag(ctx, etag)
	if etagMatch(ifNoneMatch(ctx), etag) {
		return ListServerMembers304Response{}, nil
	}
	return ListServerMembers200JSONResponse(view), nil
}

// CreateInvite POST /api/v1/servers/{id}/invites
func (s *Server) CreateInvite(ctx context.Context, request CreateInviteRequestObject) (CreateInviteResponseObject, error) {
	if e := checkID(request.Id); e != nil {
		return nil, e
	}
	if request.Body == nil {
		return nil, bodyMissing()
	}
	c := currentUser(ctx)
	if e := s.requireRole(ctx, request.Id, c.UserID, store.RoleAdmin); e != nil {
		return nil, e
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
	return CreateInvite201JSONResponse(inviteView(inv)), nil
}

// requireRole 校验成员角色；返回 nil 表示通过（错误文案区分「不是成员/权限不足」）。
func (s *Server) requireRole(ctx context.Context, serverID, userID int64, min store.MemberRole) *apiErr {
	mem, err := s.store.Member(ctx, serverID, userID)
	if err != nil {
		return errf(http.StatusForbidden, "forbidden", "不是服务器成员")
	}
	rank := map[store.MemberRole]int{store.RoleMember: 1, store.RoleAdmin: 2, store.RoleOwner: 3}
	if rank[mem.Role] < rank[min] {
		return errf(http.StatusForbidden, "forbidden", "权限不足")
	}
	return nil
}

// randomCode 生成 n 字节随机 hex 邀请码。
func randomCode(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}
