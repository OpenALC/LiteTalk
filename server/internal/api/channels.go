package api

import (
	"net/http"

	"github.com/OpenALC/LiteTalk/server/internal/store"
)

type createChannelReq struct {
	Name       string            `json:"name"`
	Type       store.ChannelType `json:"type"`
	CategoryID *int64            `json:"category_id"` // v0.1.2 分类归属（可空）
	Position   *int              `json:"position"`
}

func validChannelName(s string) bool {
	n := len([]rune(s))
	return n >= 1 && n <= 64
}

// handleCreateChannel POST /api/v1/servers/{id}/channels（owner/admin）
func (s *Server) handleCreateChannel(w http.ResponseWriter, r *http.Request) {
	c := currentUser(r)
	serverID, ok := pathID(w, r, "id")
	if !ok {
		return
	}
	if !s.requireRole(w, r, serverID, c.UserID, store.RoleAdmin) {
		return
	}
	var req createChannelReq
	if !decodeJSON(w, r, &req) {
		return
	}
	if !validChannelName(req.Name) {
		writeErr(w, http.StatusBadRequest, "bad_name", "频道名需为 1–64 个字符")
		return
	}
	if req.Type != store.ChannelText && req.Type != store.ChannelVoice {
		writeErr(w, http.StatusBadRequest, "bad_type", "类型必须是 text 或 voice")
		return
	}
	// 分类归属校验（与 PATCH 同规则）：存在且属于本服务器，防止跨服挂载/悬空 id。
	if req.CategoryID != nil && *req.CategoryID != 0 {
		cat, err := s.store.CategoryByID(r.Context(), *req.CategoryID)
		if err != nil || cat.ServerID != serverID {
			writeErr(w, http.StatusBadRequest, "bad_category", "分类不存在或不属于该服务器")
			return
		}
	}
	ch := &store.Channel{
		ServerID:   serverID,
		Name:       req.Name,
		Type:       req.Type,
		CategoryID: req.CategoryID,
	}
	if req.Position != nil {
		ch.Position = *req.Position
	}
	if err := s.store.CreateChannel(r.Context(), ch); err != nil {
		s.writeStoreErr(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, ch)
}

type patchChannelReq struct {
	Name       *string            `json:"name"`
	Type       *store.ChannelType `json:"type"`
	CategoryID *int64             `json:"category_id"` // v0.1.2 改分类归属（null 显式移出分类）
	Position   *int               `json:"position"`
}

// handlePatchChannel PATCH /api/v1/channels/{id}（owner/admin）
func (s *Server) handlePatchChannel(w http.ResponseWriter, r *http.Request) {
	c := currentUser(r)
	id, ok := pathID(w, r, "id")
	if !ok {
		return
	}
	ch, err := s.store.ChannelByID(r.Context(), id)
	if err != nil {
		s.writeStoreErr(w, r, err)
		return
	}
	if !s.requireRole(w, r, ch.ServerID, c.UserID, store.RoleAdmin) {
		return
	}
	var req patchChannelReq
	if !decodeJSON(w, r, &req) {
		return
	}
	if req.Name != nil {
		if !validChannelName(*req.Name) {
			writeErr(w, http.StatusBadRequest, "bad_name", "频道名需为 1–64 个字符")
			return
		}
		ch.Name = *req.Name
	}
	if req.Type != nil {
		if *req.Type != store.ChannelText && *req.Type != store.ChannelVoice {
			writeErr(w, http.StatusBadRequest, "bad_type", "类型必须是 text 或 voice")
			return
		}
		ch.Type = *req.Type
	}
	if req.CategoryID != nil {
		if *req.CategoryID == 0 {
			ch.CategoryID = nil // 显式移出分类（0 = 无分类）
		} else {
			// 校验分类归属本服务器，防止跨服挂载。
			cat, err := s.store.CategoryByID(r.Context(), *req.CategoryID)
			if err != nil || cat.ServerID != ch.ServerID {
				writeErr(w, http.StatusBadRequest, "bad_category", "分类不存在或不属于该服务器")
				return
			}
			ch.CategoryID = req.CategoryID
		}
	}
	if req.Position != nil {
		ch.Position = *req.Position
	}
	if err := s.store.UpdateChannel(r.Context(), ch); err != nil {
		s.writeStoreErr(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, ch)
}

// handleDeleteChannel DELETE /api/v1/channels/{id}（owner/admin）
func (s *Server) handleDeleteChannel(w http.ResponseWriter, r *http.Request) {
	c := currentUser(r)
	id, ok := pathID(w, r, "id")
	if !ok {
		return
	}
	ch, err := s.store.ChannelByID(r.Context(), id)
	if err != nil {
		s.writeStoreErr(w, r, err)
		return
	}
	if !s.requireRole(w, r, ch.ServerID, c.UserID, store.RoleAdmin) {
		return
	}
	if err := s.store.DeleteChannel(r.Context(), id); err != nil {
		s.writeStoreErr(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// handleListChannels GET /api/v1/servers/{id}/channels（v0.1.2 补缺口）。
// 返回分类与频道两个数组，频道带 category_id；前端按分类归组渲染。
// 任意服务器成员可读。
func (s *Server) handleListChannels(w http.ResponseWriter, r *http.Request) {
	c := currentUser(r)
	serverID, ok := pathID(w, r, "id")
	if !ok {
		return
	}
	if _, err := s.store.Member(r.Context(), serverID, c.UserID); err != nil {
		writeErr(w, http.StatusForbidden, "forbidden", "不是该服务器成员")
		return
	}
	channels, err := s.store.ChannelsOfServer(r.Context(), serverID)
	if err != nil {
		s.writeStoreErr(w, r, err)
		return
	}
	categories, err := s.store.CategoriesOfServer(r.Context(), serverID)
	if err != nil {
		s.writeStoreErr(w, r, err)
		return
	}
	if channels == nil {
		channels = []store.Channel{}
	}
	if categories == nil {
		categories = []store.Category{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"categories": categories, "channels": channels})
}
