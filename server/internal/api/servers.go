package api

import (
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

type createServerReq struct {
	Name      string `json:"name"`
	IconName  string `json:"icon_name"`
	IconColor string `json:"icon_color"`
}

// handleCreateServer POST /api/v1/servers
func (s *Server) handleCreateServer(w http.ResponseWriter, r *http.Request) {
	c := currentUser(r)
	var req createServerReq
	if !decodeJSON(w, r, &req) {
		return
	}
	if !validServerName(req.Name) {
		writeErr(w, http.StatusBadRequest, "bad_name", "名称需为 2–32 个字符")
		return
	}
	if req.IconName == "" {
		req.IconName = "messages-square"
	}
	if req.IconColor == "" {
		req.IconColor = "#4FA4F3"
	}
	if !validIcon(req.IconName, req.IconColor) {
		writeErr(w, http.StatusBadRequest, "bad_icon", "图标或颜色不合法")
		return
	}
	srv := &store.Server{
		Name:      req.Name,
		OwnerID:   c.UserID,
		IconName:  req.IconName,
		IconColor: req.IconColor,
	}
	if err := s.store.CreateServer(r.Context(), srv); err != nil {
		s.writeStoreErr(w, r, err)
		return
	}
	// 创建者自动成为 owner 成员。
	mem := &store.ServerMember{ServerID: srv.ID, UserID: c.UserID, Role: store.RoleOwner}
	if err := s.store.AddMember(r.Context(), mem); err != nil {
		s.writeStoreErr(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, srv)
}

// handleListServers GET /api/v1/servers
func (s *Server) handleListServers(w http.ResponseWriter, r *http.Request) {
	c := currentUser(r)
	servers, err := s.store.ServersOfUser(r.Context(), c.UserID)
	if err != nil {
		s.writeStoreErr(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, servers)
}

type patchServerReq struct {
	Name      *string `json:"name"`
	IconName  *string `json:"icon_name"`
	IconColor *string `json:"icon_color"`
}

// handlePatchServer PATCH /api/v1/servers/{id}（仅 owner/admin）
func (s *Server) handlePatchServer(w http.ResponseWriter, r *http.Request) {
	c := currentUser(r)
	id, ok := pathID(w, r, "id")
	if !ok {
		return
	}
	if !s.requireRole(w, r, id, c.UserID, store.RoleAdmin) {
		return
	}
	srv, err := s.store.ServerByID(r.Context(), id)
	if err != nil {
		s.writeStoreErr(w, r, err)
		return
	}
	var req patchServerReq
	if !decodeJSON(w, r, &req) {
		return
	}
	if req.Name != nil {
		if !validServerName(*req.Name) {
			writeErr(w, http.StatusBadRequest, "bad_name", "名称需为 2–32 个字符")
			return
		}
		srv.Name = *req.Name
	}
	if req.IconName != nil {
		srv.IconName = *req.IconName
	}
	if req.IconColor != nil {
		srv.IconColor = *req.IconColor
	}
	if !validIcon(srv.IconName, srv.IconColor) {
		writeErr(w, http.StatusBadRequest, "bad_icon", "图标或颜色不合法")
		return
	}
	if err := s.store.UpdateServer(r.Context(), srv); err != nil {
		s.writeStoreErr(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, srv)
}

// handleDeleteServer DELETE /api/v1/servers/{id}（仅 owner）
func (s *Server) handleDeleteServer(w http.ResponseWriter, r *http.Request) {
	c := currentUser(r)
	id, ok := pathID(w, r, "id")
	if !ok {
		return
	}
	if !s.requireRole(w, r, id, c.UserID, store.RoleOwner) {
		return
	}
	// 删前记下频道 ID，删后逐个踢出房间成员（#22）。
	channels, _ := s.store.ChannelsOfServer(r.Context(), id)
	if err := s.store.DeleteServer(r.Context(), id); err != nil {
		s.writeStoreErr(w, r, err)
		return
	}
	if s.hub != nil {
		for _, ch := range channels {
			s.hub.KickChannel(strconv.FormatInt(ch.ID, 10), "server_deleted")
		}
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

type joinServerReq struct {
	Code string `json:"code"`
}

// handleJoinServer POST /api/v1/servers/join（邀请码）
func (s *Server) handleJoinServer(w http.ResponseWriter, r *http.Request) {
	c := currentUser(r)
	var req joinServerReq
	if !decodeJSON(w, r, &req) {
		return
	}
	if req.Code == "" {
		writeErr(w, http.StatusBadRequest, "bad_request", "缺少邀请码")
		return
	}
	inv, err := s.store.InviteByCode(r.Context(), req.Code)
	if err != nil {
		writeErr(w, http.StatusNotFound, "bad_code", "邀请码无效")
		return
	}
	if inv.ExpiresAt != nil && time.Now().After(*inv.ExpiresAt) {
		writeErr(w, http.StatusGone, "code_expired", "邀请码已过期")
		return
	}
	// 已是成员直接返回。
	if _, err := s.store.Member(r.Context(), inv.ServerID, c.UserID); err == nil {
		srv, _ := s.store.ServerByID(r.Context(), inv.ServerID)
		writeJSON(w, http.StatusOK, srv)
		return
	}
	if err := s.store.UseInvite(r.Context(), inv.Code); err != nil {
		if errors.Is(err, store.ErrConflict) {
			writeErr(w, http.StatusConflict, "code_exhausted", "邀请码使用次数已达上限")
			return
		}
		if errors.Is(err, store.ErrExpired) {
			writeErr(w, http.StatusGone, "code_expired", "邀请码已过期")
			return
		}
		s.writeStoreErr(w, r, err)
		return
	}
	mem := &store.ServerMember{ServerID: inv.ServerID, UserID: c.UserID, Role: store.RoleMember}
	if err := s.store.AddMember(r.Context(), mem); err != nil {
		s.writeStoreErr(w, r, err)
		return
	}
	srv, err := s.store.ServerByID(r.Context(), inv.ServerID)
	if err != nil {
		s.writeStoreErr(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, srv)
}

// handleServerMembers GET /api/v1/servers/{id}/members
func (s *Server) handleServerMembers(w http.ResponseWriter, r *http.Request) {
	c := currentUser(r)
	id, ok := pathID(w, r, "id")
	if !ok {
		return
	}
	// 必须是成员才能看成员列表。
	if _, err := s.store.Member(r.Context(), id, c.UserID); err != nil {
		writeErr(w, http.StatusForbidden, "forbidden", "不是服务器成员")
		return
	}
	members, err := s.store.MembersOfServer(r.Context(), id)
	if err != nil {
		s.writeStoreErr(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, members)
}

type createInviteReq struct {
	MaxUses   int   `json:"max_uses"`   // 0 = 不限
	ExpiresIn int64 `json:"expires_in"` // 秒；0 = 不过期
}

// handleCreateInvite POST /api/v1/servers/{id}/invites
func (s *Server) handleCreateInvite(w http.ResponseWriter, r *http.Request) {
	c := currentUser(r)
	id, ok := pathID(w, r, "id")
	if !ok {
		return
	}
	if !s.requireRole(w, r, id, c.UserID, store.RoleAdmin) {
		return
	}
	var req createInviteReq
	if !decodeJSON(w, r, &req) {
		return
	}
	// 邀请参数校验（#22）：max_uses 不可为负。
	if req.MaxUses < 0 {
		writeErr(w, http.StatusBadRequest, "bad_request", "max_uses 不可为负")
		return
	}
	code, err := randomCode(12)
	if err != nil {
		s.writeStoreErr(w, r, err)
		return
	}
	inv := &store.Invite{
		Code:      code,
		ServerID:  id,
		CreatorID: c.UserID,
		MaxUses:   req.MaxUses,
	}
	if req.ExpiresIn > 0 {
		t := time.Now().Add(time.Duration(req.ExpiresIn) * time.Second)
		inv.ExpiresAt = &t
	}
	if err := s.store.CreateInvite(r.Context(), inv); err != nil {
		s.writeStoreErr(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, inv)
}

// requireRole 校验成员角色；不足返回 false 并已写响应。
func (s *Server) requireRole(w http.ResponseWriter, r *http.Request, serverID, userID int64, min store.MemberRole) bool {
	mem, err := s.store.Member(r.Context(), serverID, userID)
	if err != nil {
		writeErr(w, http.StatusForbidden, "forbidden", "不是服务器成员")
		return false
	}
	rank := map[store.MemberRole]int{store.RoleMember: 1, store.RoleAdmin: 2, store.RoleOwner: 3}
	if rank[mem.Role] < rank[min] {
		writeErr(w, http.StatusForbidden, "forbidden", "权限不足")
		return false
	}
	return true
}

// randomCode 生成 n 字节随机 hex 邀请码。
func randomCode(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}
