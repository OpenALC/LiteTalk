package api

import (
	"net/http"

	"github.com/OpenALC/LiteTalk/server/internal/store"
)

// handleMe GET /api/v1/users/me
func (s *Server) handleMe(w http.ResponseWriter, r *http.Request) {
	c := currentUser(r)
	user, err := s.store.UserByID(r.Context(), c.UserID)
	if err != nil {
		s.writeStoreErr(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, user)
}

type patchMeReq struct {
	Nickname *string `json:"nickname"`
}

// handlePatchMe PATCH /api/v1/users/me（昵称可改；无头像字段）
func (s *Server) handlePatchMe(w http.ResponseWriter, r *http.Request) {
	c := currentUser(r)
	var req patchMeReq
	if !decodeJSON(w, r, &req) {
		return
	}
	user, err := s.store.UserByID(r.Context(), c.UserID)
	if err != nil {
		s.writeStoreErr(w, r, err)
		return
	}
	if req.Nickname != nil {
		if !validNickname(*req.Nickname) {
			writeErr(w, http.StatusBadRequest, "bad_nickname", "昵称需为 2–16 个字符")
			return
		}
		if *req.Nickname != user.Nickname {
			// 昵称变更后 tag 需重新分配保证 (nickname,tag) 不冲突。
			user.Nickname = *req.Nickname
			for i := 0; i < 5; i++ {
				user.Tag = fmtTag(int(user.ID*7919+int64(i*104729)) % 10000)
				err = s.store.UpdateUser(r.Context(), user)
				if err == nil {
					break
				}
				if err != store.ErrConflict {
					s.writeStoreErr(w, r, err)
					return
				}
			}
		}
	}
	writeJSON(w, http.StatusOK, user)
}
