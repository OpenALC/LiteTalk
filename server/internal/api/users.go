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

// tagFromID 由用户 ID 与重试序号派生 4 位 tag（#22）。
// Snowflake ID 已接近 int64 上限，原实现 ID*7919 会溢出为负数（约半数样本
// 得到 "-7952" 这类值，MySQL CHAR(4) 直接 500、SQLite 存进去后无法按
// 昵称#tag 查找）。splitmix64 混合在 uint64 上定义明确（溢出回绕），结果
// 恒为非负；attempt 参与混合，冲突重试因此得到不同 tag。
func tagFromID(id int64, attempt int) string {
	x := uint64(id) + uint64(attempt)*0x9E3779B97F4A7C15
	x ^= x >> 30
	x *= 0xBF58476D1CE4E5B9
	x ^= x >> 27
	x *= 0x94D049BB133111EB
	x ^= x >> 31
	return fmtTag(int(x % 10000))
}

// PatchMe PATCH /api/v1/users/me（昵称可改；无头像字段）
// Merge Patch（#31）：缺省不改，null 清空（昵称不可清 → 校验拒 400）。
func (s *Server) PatchMe(ctx context.Context, request PatchMeRequestObject) (PatchMeResponseObject, error) {
	req := patchBody(request.JSONBody, request.ApplicationMergePatchPlusJSONBody)
	if req == nil {
		return nil, bodyMissing()
	}
	c := currentUser(ctx)
	user, err := s.store.UserByID(ctx, c.UserID)
	if err != nil {
		return nil, s.writeStoreErr(ctx, err)
	}
	if req.Nickname.Set {
		name := req.Nickname.Val
		if !validNickname(name) {
			return nil, errf(http.StatusBadRequest, "bad_nickname", "昵称需为 2–16 个字符")
		}
		if name != user.Nickname {
			// 昵称变更后 tag 需重新分配保证 (nickname,tag) 不冲突。
			user.Nickname = name
			for i := 0; i < 5; i++ {
				user.Tag = tagFromID(user.ID, i)
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
