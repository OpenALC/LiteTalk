package api

import (
	"context"
	"net/http"
	"strconv"

	"github.com/OpenALC/LiteTalk/server/internal/store"
)

func validCategoryName(s string) bool {
	n := len([]rune(s))
	return n >= 1 && n <= 32
}

// CreateCategory POST /api/v1/servers/{id}/categories（需 ManageChannels，#35 B3）。
// 分类允许为空（创建后暂未归入频道），position 按当前数量追加。
func (s *Server) CreateCategory(ctx context.Context, request CreateCategoryRequestObject) (CreateCategoryResponseObject, error) {
	if e := checkID(request.Id); e != nil {
		return nil, e
	}
	c := currentUser(ctx)
	// 资源→权限→body（#35 B3）。
	if e := s.requirePerm(ctx, request.Id, c.UserID, store.ManageChannels); e != nil {
		return nil, e
	}
	if request.Body == nil {
		return nil, bodyMissing()
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
	s.audit(ctx, store.AuditLog{
		ServerID: request.Id, ActorID: c.UserID, Action: "category.create",
		TargetType: "category", TargetID: ptrID(cat.ID),
		Detail: auditDetail(map[string]any{"name": cat.Name, "position": cat.Position}),
	})
	return CreateCategory201JSONResponse(categoryView(cat)), nil
}

// DeleteCategory DELETE /api/v1/categories/{id}（需 ManageChannels，#35 B3）。
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
	if e := s.requirePerm(ctx, cat.ServerID, c.UserID, store.ManageChannels); e != nil {
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
	// 删除前收集频道（含子频道）：删完行就查不到，踢房要覆盖语音房（#37 补缺口）。
	all, err := s.store.ChannelsOfServer(ctx, cat.ServerID)
	if err != nil {
		return nil, s.writeStoreErr(ctx, err)
	}
	var kickIDs []int64
	inCat := map[int64]bool{}
	for _, ch := range all {
		if ch.CategoryID != nil && *ch.CategoryID == request.Id {
			inCat[ch.ID] = true
		}
	}
	for _, ch := range all {
		if ch.ParentID != nil && inCat[*ch.ParentID] {
			inCat[ch.ID] = true
		}
	}
	for _, ch := range all {
		if inCat[ch.ID] && ch.Type == store.ChannelVoice {
			kickIDs = append(kickIDs, ch.ID)
		}
	}
	if err := s.store.DeleteCategoryWithChannels(ctx, request.Id); err != nil {
		return nil, s.writeStoreErr(ctx, err)
	}
	// 连频道一起删时频道覆盖也随之下葬：快照作废。
	s.authz.InvalidateServer(cat.ServerID)
	s.audit(ctx, store.AuditLog{
		ServerID: cat.ServerID, ActorID: c.UserID, Action: "category.delete",
		TargetType: "category", TargetID: ptrID(cat.ID),
		Detail: auditDetail(map[string]any{"name": cat.Name, "with_channels": withChannels == 1}),
	})
	if s.hub != nil {
		for _, id := range kickIDs {
			s.hub.KickChannel(strconv.FormatInt(id, 10), "channel_deleted")
		}
	}
	return DeleteCategory200JSONResponse{Status: "ok"}, nil
}

// PatchCategory PATCH /api/v1/categories/{id}（需 ManageChannels，#35 B3）。
// name/position 都可选但至少给一个；position 为目标序（0-based），服务端把该分类
// 移到此位置并重排同服务器其它分类为 0..n-1（相对顺序保持）。分类名复用
// validCategoryName（与 POST 同一口径），不单独收紧。
// 同时给 name 与 position 时先重排、读回再改名，避免把旧 position 写回（#16）。
func (s *Server) PatchCategory(ctx context.Context, request PatchCategoryRequestObject) (PatchCategoryResponseObject, error) {
	if e := checkID(request.Id); e != nil {
		return nil, e
	}
	c := currentUser(ctx)
	cat, err := s.store.CategoryByID(ctx, request.Id)
	if err != nil {
		return nil, s.writeStoreErr(ctx, err)
	}
	// 资源→权限→body（#35 B3）：权限按「分类所属服务器」判定。
	if e := s.requirePerm(ctx, cat.ServerID, c.UserID, store.ManageChannels); e != nil {
		return nil, e
	}
	req := patchBody(request.JSONBody, request.ApplicationMergePatchPlusJSONBody)
	if req == nil {
		return nil, bodyMissing()
	}
	// Merge Patch（#31）：缺省不改，null = 清空（name 不可清 → 校验拒 400；
	// position 的 null 拒 400）。至少给一个字段。
	if !req.Name.Set && !req.Position.Set {
		return nil, errf(http.StatusBadRequest, "bad_request", "name 与 position 至少给一个")
	}
	// 两个字段都先校验再写：任一项非法时不应留下半次写入。
	if req.Name.Set && !validCategoryName(req.Name.Val) {
		return nil, errf(http.StatusBadRequest, "bad_name", "分类名需为 1–32 个字符")
	}
	if req.Position.Set && req.Position.Clearing() {
		return nil, errf(http.StatusBadRequest, "bad_request", "position 不可清除")
	}

	// position 必须先重排、再重新读回，最后才改名：UpdateCategory 会全量写
	// name+position，若在重排前调用就会把加载时的旧 position 写回去（#16 P2，
	// 并发重排期间可能覆盖别人的排序）。先重排则改名写入的永远是重排后的序号。
	//
	// 注意：重排与改名仍是两次独立写入，不是同一事务；两者之间若发生并发重排，
	// 最后一次 UpdateCategory 仍可能覆盖它刚读到之后落下的 position。要做到
	// 完全原子需要 store 层新增「改名+重排」单事务方法，本轮不做。
	if req.Position.Set {
		if err := s.store.ReorderCategories(ctx, cat.ServerID, request.Id, req.Position.Val); err != nil {
			return nil, s.writeStoreErr(ctx, err)
		}
		// 重排会把越界 position 夹到首/尾，必须重新读回才能得到最终序号；
		// 读失败时也绝不能拿旧 cat 去改名，否则又写回旧 position。
		updated, err := s.store.CategoryByID(ctx, request.Id)
		if err != nil {
			return nil, s.writeStoreErr(ctx, err)
		}
		cat = updated
	}
	detail := map[string]any{}
	if req.Position.Set {
		detail["position"] = cat.Position
	}
	if req.Name.Set {
		cat.Name = req.Name.Val
		detail["name"] = cat.Name
		if err := s.store.UpdateCategory(ctx, cat); err != nil {
			return nil, s.writeStoreErr(ctx, err)
		}
	}
	s.audit(ctx, store.AuditLog{
		ServerID: cat.ServerID, ActorID: c.UserID, Action: "category.update",
		TargetType: "category", TargetID: ptrID(cat.ID),
		Detail: auditDetail(detail),
	})
	return PatchCategory200JSONResponse(categoryView(cat)), nil
}
