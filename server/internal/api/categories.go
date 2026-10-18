package api

import (
	"context"
	"net/http"

	"github.com/OpenALC/LiteTalk/server/internal/store"
)

func validCategoryName(s string) bool {
	n := len([]rune(s))
	return n >= 1 && n <= 32
}

// CreateCategory POST /api/v1/servers/{id}/categories（owner/admin，v0.1.2）。
// 分类允许为空（创建后暂未归入频道），position 按当前数量追加。
func (s *Server) CreateCategory(ctx context.Context, request CreateCategoryRequestObject) (CreateCategoryResponseObject, error) {
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
	if !validCategoryName(request.Body.Name) {
		return nil, errf(http.StatusBadRequest, "bad_name", "分类名需为 1–32 个字符")
	}
	existing, err := s.store.CategoriesOfServer(ctx, request.Id)
	if err != nil {
		return nil, s.writeStoreErr(ctx, err)
	}
	cat := &store.Category{ServerID: request.Id, Name: request.Body.Name, Position: len(existing)}
	if err := s.store.CreateCategory(ctx, cat); err != nil {
		return nil, s.writeStoreErr(ctx, err)
	}
	return CreateCategory201JSONResponse(*cat), nil
}

// DeleteCategory DELETE /api/v1/categories/{id}（owner/admin，v0.1.2）。
// 默认仅删空分类：分类下仍有频道且未带 with_channels=1 → 409 category_not_empty，
// 防止误删；显式带参才级联删除分类及其下全部频道（含消息，单事务）。
func (s *Server) DeleteCategory(ctx context.Context, request DeleteCategoryRequestObject) (DeleteCategoryResponseObject, error) {
	if e := checkID(request.Id); e != nil {
		return nil, e
	}
	c := currentUser(ctx)
	cat, err := s.store.CategoryByID(ctx, request.Id)
	if err != nil {
		return nil, s.writeStoreErr(ctx, err)
	}
	if e := s.requireRole(ctx, cat.ServerID, c.UserID, store.RoleAdmin); e != nil {
		return nil, e
	}
	withChannels := 0
	if request.Params.WithChannels != nil {
		withChannels = *request.Params.WithChannels
	}
	if withChannels != 1 {
		channels, err := s.store.ChannelsOfServer(ctx, cat.ServerID)
		if err != nil {
			return nil, s.writeStoreErr(ctx, err)
		}
		for _, ch := range channels {
			if ch.CategoryID != nil && *ch.CategoryID == request.Id {
				return nil, errf(http.StatusConflict, "category_not_empty", "分类下仍有频道，带 with_channels=1 可连频道一并删除")
			}
		}
	}
	if err := s.store.DeleteCategoryWithChannels(ctx, request.Id); err != nil {
		return nil, s.writeStoreErr(ctx, err)
	}
	return DeleteCategory200JSONResponse{Status: "ok"}, nil
}

// PatchCategory PATCH /api/v1/categories/{id}（owner/admin，0.1.2）。
// name/position 都可选但至少给一个；position 为目标序（0-based），服务端把该分类
// 移到此位置并重排同服务器其它分类为 0..n-1（相对顺序保持）。分类名复用
// validCategoryName（与 POST 同一口径），不单独收紧。
func (s *Server) PatchCategory(ctx context.Context, request PatchCategoryRequestObject) (PatchCategoryResponseObject, error) {
	if e := checkID(request.Id); e != nil {
		return nil, e
	}
	if request.Body == nil {
		return nil, bodyMissing()
	}
	c := currentUser(ctx)
	cat, err := s.store.CategoryByID(ctx, request.Id)
	if err != nil {
		return nil, s.writeStoreErr(ctx, err)
	}
	// 权限按「分类所属服务器」的 owner/admin 判定：成员只读，不够改。
	if e := s.requireRole(ctx, cat.ServerID, c.UserID, store.RoleAdmin); e != nil {
		return nil, e
	}
	req := request.Body
	if req.Name == nil && req.Position == nil {
		return nil, errf(http.StatusBadRequest, "bad_request", "name 与 position 至少给一个")
	}
	if req.Name != nil {
		if !validCategoryName(*req.Name) {
			return nil, errf(http.StatusBadRequest, "bad_name", "分类名需为 1–32 个字符")
		}
		cat.Name = *req.Name
		if err := s.store.UpdateCategory(ctx, cat); err != nil {
			return nil, s.writeStoreErr(ctx, err)
		}
	}
	if req.Position != nil {
		if err := s.store.ReorderCategories(ctx, cat.ServerID, request.Id, *req.Position); err != nil {
			return nil, s.writeStoreErr(ctx, err)
		}
		// 重排后重新读回，响应里的 position 是最终序号。
		if updated, err := s.store.CategoryByID(ctx, request.Id); err == nil {
			cat = updated
		}
	}
	return PatchCategory200JSONResponse(*cat), nil
}
