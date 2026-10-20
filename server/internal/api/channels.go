package api

import (
	"context"
	"net/http"
	"strconv"

	"github.com/OpenALC/LiteTalk/server/internal/store"
)

func validChannelName(s string) bool {
	n := len([]rune(s))
	return n >= 1 && n <= 64
}

// CreateChannel POST /api/v1/servers/{id}/channels（需 ManageChannels，#35 B3）
func (s *Server) CreateChannel(ctx context.Context, request CreateChannelRequestObject) (CreateChannelResponseObject, error) {
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
	req := request.Body
	if !validChannelName(req.Name) {
		return nil, errf(http.StatusBadRequest, "bad_name", "频道名需为 1–64 个字符")
	}
	chType := store.ChannelType(req.Type)
	if chType != store.ChannelText && chType != store.ChannelVoice {
		return nil, errf(http.StatusBadRequest, "bad_type", "类型必须是 text 或 voice")
	}
	// 分类归属校验（与 PATCH 同规则）：存在且属于本服务器，防止跨服挂载/悬空 id。
	// 0 = 显式无分类，落库前归一成 NULL——否则 0 与 NULL 两种「无分类」并存，
	// 而 PATCH 那侧一律落 NULL（见 PatchChannel）。
	var catID *int64
	if req.CategoryId != nil && *req.CategoryId != 0 {
		cat, err := s.store.CategoryByID(ctx, int64(*req.CategoryId))
		if err != nil || cat.ServerID != request.Id {
			return nil, errf(http.StatusBadRequest, "bad_category", "分类不存在或不属于该服务器")
		}
		v := int64(*req.CategoryId)
		catID = &v
	}
	ch := &store.Channel{
		ServerID:   request.Id,
		Name:       req.Name,
		Type:       chType,
		CategoryID: catID,
	}
	if req.Position != nil {
		ch.Position = *req.Position
	}
	if err := s.store.CreateChannel(ctx, ch); err != nil {
		return nil, s.writeStoreErr(ctx, err)
	}
	s.audit(ctx, store.AuditLog{
		ServerID: request.Id, ActorID: c.UserID, Action: "channel.create",
		TargetType: "channel", TargetID: ptrID(ch.ID), ChannelID: ptrID(ch.ID),
		Detail: auditDetail(map[string]any{"name": ch.Name, "type": string(ch.Type)}),
	})
	return CreateChannel201JSONResponse(channelView(ch)), nil
}

// PatchChannel PATCH /api/v1/channels/{id}（需 ManageChannels，#35 B3）
func (s *Server) PatchChannel(ctx context.Context, request PatchChannelRequestObject) (PatchChannelResponseObject, error) {
	if e := checkID(request.Id); e != nil {
		return nil, e
	}
	c := currentUser(ctx)
	// 资源→权限→body（#35 B3）：先解析频道（404），再权限（403），最后字段校验（400）。
	ch, err := s.store.ChannelByID(ctx, request.Id)
	if err != nil {
		return nil, s.writeStoreErr(ctx, err)
	}
	if e := s.requirePerm(ctx, ch.ServerID, c.UserID, store.ManageChannels); e != nil {
		return nil, e
	}
	req := patchBody(request.JSONBody, request.ApplicationMergePatchPlusJSONBody)
	if req == nil {
		return nil, bodyMissing()
	}
	// Merge Patch（#31）：缺省不改，null = 清空（name/type 不可清 → 校验拒 400；
	// category_id 的 null = 移出分类；position 的 null 拒 400）。
	// 审计只记变更字段（#35 B4）。
	detail := map[string]any{}
	if req.Name.Set {
		if !validChannelName(req.Name.Val) {
			return nil, errf(http.StatusBadRequest, "bad_name", "频道名需为 1–64 个字符")
		}
		ch.Name = req.Name.Val
		detail["name"] = req.Name.Val
	}
	if req.Type.Set {
		t := store.ChannelType(req.Type.Val)
		if t != store.ChannelText && t != store.ChannelVoice {
			return nil, errf(http.StatusBadRequest, "bad_type", "类型必须是 text 或 voice")
		}
		ch.Type = t
		detail["type"] = string(t)
	}
	if req.CategoryID.Set {
		if req.CategoryID.Clearing() || req.CategoryID.Val == 0 {
			ch.CategoryID = nil // 显式移出分类（null 或 0，#31）
			detail["category_id"] = nil
		} else {
			// 校验分类归属本服务器，防止跨服挂载。
			cat, err := s.store.CategoryByID(ctx, int64(req.CategoryID.Val))
			if err != nil || cat.ServerID != ch.ServerID {
				return nil, errf(http.StatusBadRequest, "bad_category", "分类不存在或不属于该服务器")
			}
			v := int64(req.CategoryID.Val)
			ch.CategoryID = &v
			detail["category_id"] = req.CategoryID.Val
		}
	}
	if req.Position.Set {
		if req.Position.Clearing() {
			return nil, errf(http.StatusBadRequest, "bad_request", "position 不可清除")
		}
		ch.Position = req.Position.Val
		detail["position"] = req.Position.Val
	}
	if err := s.store.UpdateChannel(ctx, ch); err != nil {
		return nil, s.writeStoreErr(ctx, err)
	}
	s.audit(ctx, store.AuditLog{
		ServerID: ch.ServerID, ActorID: c.UserID, Action: "channel.update",
		TargetType: "channel", TargetID: ptrID(ch.ID), ChannelID: ptrID(ch.ID),
		Detail: auditDetail(detail),
	})
	return PatchChannel200JSONResponse(channelView(ch)), nil
}

// DeleteChannel DELETE /api/v1/channels/{id}（需 ManageChannels，#35 B3）
func (s *Server) DeleteChannel(ctx context.Context, request DeleteChannelRequestObject) (DeleteChannelResponseObject, error) {
	if e := checkID(request.Id); e != nil {
		return nil, e
	}
	c := currentUser(ctx)
	ch, err := s.store.ChannelByID(ctx, request.Id)
	if err != nil {
		return nil, s.writeStoreErr(ctx, err)
	}
	if e := s.requirePerm(ctx, ch.ServerID, c.UserID, store.ManageChannels); e != nil {
		return nil, e
	}
	if err := s.store.DeleteChannel(ctx, request.Id); err != nil {
		return nil, s.writeStoreErr(ctx, err)
	}
	// 频道级覆盖随频道删除：快照作废，避免旧覆盖继续影响同服其它判定。
	s.authz.InvalidateServer(ch.ServerID)
	s.audit(ctx, store.AuditLog{
		ServerID: ch.ServerID, ActorID: c.UserID, Action: "channel.delete",
		TargetType: "channel", TargetID: ptrID(ch.ID), ChannelID: ptrID(ch.ID),
		Detail: auditDetail(map[string]any{"name": ch.Name, "type": string(ch.Type)}),
	})
	// 频道删除后踢出该房间内成员（#22，单实例语义）。
	if s.hub != nil {
		s.hub.KickChannel(strconv.FormatInt(request.Id, 10), "channel_deleted")
	}
	return DeleteChannel200JSONResponse{Status: "ok"}, nil
}

// ListChannels GET /api/v1/servers/{id}/channels（v0.1.2 补缺口）。
// 返回分类与频道两个数组，频道带 category_id；前端按分类归组渲染。
// 成员可读，但只返回其有 ViewChannel 的频道（#35 B3）；categories 不因空分类过滤。
func (s *Server) ListChannels(ctx context.Context, request ListChannelsRequestObject) (ListChannelsResponseObject, error) {
	if e := checkID(request.Id); e != nil {
		return nil, e
	}
	c := currentUser(ctx)
	if e := s.requireMember(ctx, request.Id, c.UserID); e != nil {
		return nil, e
	}
	channels, err := s.store.ChannelsOfServer(ctx, request.Id)
	if err != nil {
		return nil, s.writeStoreErr(ctx, err)
	}
	// 空结果落 [] 而非 null（契约 ServerChannels）。
	if channels == nil {
		channels = []store.Channel{}
	}
	channels, err = s.visibleChannels(ctx, c.UserID, request.Id, channels)
	if err != nil {
		return nil, s.writeStoreErr(ctx, err)
	}
	categories, err := s.store.CategoriesOfServer(ctx, request.Id)
	if err != nil {
		return nil, s.writeStoreErr(ctx, err)
	}
	if categories == nil {
		categories = []store.Category{}
	}
	// ETag 协商（#31）：内容哈希弱标签，命中 If-None-Match 回 304。
	out := ServerChannels{Categories: categoryViews(categories), Channels: channelViews(channels)}
	etag := contentETag(out)
	setETag(ctx, etag)
	if etagMatch(ifNoneMatch(ctx), etag) {
		return ListChannels304Response{}, nil
	}
	return ListChannels200JSONResponse(out), nil
}
