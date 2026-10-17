package api

import (
	"net/http"
	"strconv"

	"github.com/OpenALC/LiteTalk/server/internal/store"
)

type createCategoryReq struct {
	Name string `json:"name"`
}

func validCategoryName(s string) bool {
	n := len([]rune(s))
	return n >= 1 && n <= 32
}

// handleCreateCategory POST /api/v1/servers/{id}/categories（owner/admin，v0.1.2）。
// 分类允许为空（创建后暂未归入频道），position 按当前数量追加。
func (s *Server) handleCreateCategory(w http.ResponseWriter, r *http.Request) {
	c := currentUser(r)
	serverID, ok := pathID(w, r, "id")
	if !ok {
		return
	}
	if !s.requireRole(w, r, serverID, c.UserID, store.RoleAdmin) {
		return
	}
	var req createCategoryReq
	if !decodeJSON(w, r, &req) {
		return
	}
	if !validCategoryName(req.Name) {
		writeErr(w, http.StatusBadRequest, "bad_name", "分类名需为 1–32 个字符")
		return
	}
	existing, err := s.store.CategoriesOfServer(r.Context(), serverID)
	if err != nil {
		s.writeStoreErr(w, r, err)
		return
	}
	cat := &store.Category{ServerID: serverID, Name: req.Name, Position: len(existing)}
	if err := s.store.CreateCategory(r.Context(), cat); err != nil {
		s.writeStoreErr(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, cat)
}

// handleDeleteCategory DELETE /api/v1/categories/{id}（owner/admin，v0.1.2）。
// 默认仅删空分类：分类下仍有频道且未带 with_channels=1 → 409 category_not_empty，
// 防止误删；显式带参才级联删除分类及其下全部频道（含消息，单事务）。
func (s *Server) handleDeleteCategory(w http.ResponseWriter, r *http.Request) {
	c := currentUser(r)
	id, ok := pathID(w, r, "id")
	if !ok {
		return
	}
	cat, err := s.store.CategoryByID(r.Context(), id)
	if err != nil {
		s.writeStoreErr(w, r, err)
		return
	}
	if !s.requireRole(w, r, cat.ServerID, c.UserID, store.RoleAdmin) {
		return
	}
	withChannels, _ := strconv.Atoi(r.URL.Query().Get("with_channels"))
	if withChannels != 1 {
		channels, err := s.store.ChannelsOfServer(r.Context(), cat.ServerID)
		if err != nil {
			s.writeStoreErr(w, r, err)
			return
		}
		for _, ch := range channels {
			if ch.CategoryID != nil && *ch.CategoryID == id {
				writeErr(w, http.StatusConflict, "category_not_empty", "分类下仍有频道，带 with_channels=1 可连频道一并删除")
				return
			}
		}
	}
	if err := s.store.DeleteCategoryWithChannels(r.Context(), id); err != nil {
		s.writeStoreErr(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}
