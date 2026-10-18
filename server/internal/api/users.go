package api

import (
	"context"
	"errors"
	"net/http"

	"github.com/OpenALC/LiteTalk/server/internal/store"
)

// GetMe GET /api/v1/users/me（自响应含 email）
func (s *Server) GetMe(ctx context.Context, request GetMeRequestObject) (GetMeResponseObject, error) {
	c := currentUser(ctx)
	user, err := s.store.UserByID(ctx, c.UserID)
	if err != nil {
		return nil, s.writeStoreErr(ctx, err)
	}
	return GetMe200JSONResponse(userSelf(user)), nil
}

// PatchMe PATCH /api/v1/users/me（昵称可改；无头像字段）
func (s *Server) PatchMe(ctx context.Context, request PatchMeRequestObject) (PatchMeResponseObject, error) {
	if request.Body == nil {
		return nil, bodyMissing()
	}
	c := currentUser(ctx)
	user, err := s.store.UserByID(ctx, c.UserID)
	if err != nil {
		return nil, s.writeStoreErr(ctx, err)
	}
	if req := request.Body; req.Nickname != nil {
		if !validNickname(*req.Nickname) {
			return nil, errf(http.StatusBadRequest, "bad_nickname", "昵称需为 2–16 个字符")
		}
		if *req.Nickname != user.Nickname {
			// 昵称变更后 tag 需重新分配保证 (nickname,tag) 不冲突。
			user.Nickname = *req.Nickname
			for i := 0; i < 5; i++ {
				user.Tag = fmtTag(int(user.ID*7919+int64(i*104729)) % 10000)
				err = s.store.UpdateUser(ctx, user)
				if err == nil {
					break
				}
				if !errors.Is(err, store.ErrConflict) {
					return nil, s.writeStoreErr(ctx, err)
				}
			}
			// 5 次重试仍冲突 → 409（#22）。
			if err != nil {
				return nil, s.writeStoreErr(ctx, err)
			}
		}
	}
	return PatchMe200JSONResponse(userSelf(user)), nil
}
