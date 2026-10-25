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
	// #37 选项：密码/人数上限仅语音频道；父级校验顺带收口分类归属。
	if req.Password != nil && *req.Password != "" {
		if e := checkChannelPassword(*req.Password, chType); e != nil {
			return nil, e
		}
	}
	if req.UserLimit != nil {
		if e := checkUserLimit(*req.UserLimit, chType); e != nil {
			return nil, e
		}
	}
	// #46 音质选项：仅语音频道可设非零值；text 的 0 值会被 normalizeChannelAudio 归一。
	if req.AudioBitrateKbps != nil {
		if e := checkChannelBitrate(*req.AudioBitrateKbps, chType); e != nil {
			return nil, e
		}
	}
	if req.AudioFrameMs != nil {
		if e := checkChannelFrameMS(*req.AudioFrameMs, chType); e != nil {
			return nil, e
		}
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
	var parentID int64
	if req.ParentId != nil {
		parentID = int64(*req.ParentId)
	}
	// 分类显式给定（含显式 0=无分类）才参与父子一致性校验；缺省随父（#37）。
	_, catID, e := s.resolveParent(ctx, request.Id, 0, parentID, chType, catID, req.CategoryId != nil)
	if e != nil {
		return nil, e
	}
	hash, err := hashChannelPassword(derefString(req.Password))
	if err != nil {
		return nil, s.writeStoreErr(ctx, err)
	}
	ch := &store.Channel{
		ServerID:     request.Id,
		Name:         req.Name,
		Type:         chType,
		CategoryID:   catID,
		PasswordHash: hash,
	}
	if req.Position != nil {
		ch.Position = *req.Position
	}
	if req.UserLimit != nil {
		ch.UserLimit = *req.UserLimit
	}
	if req.AudioBitrateKbps != nil {
		ch.AudioBitrateKbps = *req.AudioBitrateKbps
	}
	if req.AudioFrameMs != nil {
		ch.AudioFrameMS = *req.AudioFrameMs
	}
	normalizeChannelAudio(ch)
	if parentID != 0 {
		ch.ParentID = &parentID
	}
	if err := s.store.CreateChannel(ctx, ch); err != nil {
		return nil, s.writeStoreErr(ctx, err)
	}
	// 审计只记 has_password/user_limit/parent_id/音质（#37/#46）：密码明文与哈希都不进 detail。
	s.audit(ctx, store.AuditLog{
		ServerID: request.Id, ActorID: c.UserID, Action: "channel.create",
		TargetType: "channel", TargetID: ptrID(ch.ID), ChannelID: ptrID(ch.ID),
		Detail: auditDetail(map[string]any{
			"name": ch.Name, "type": string(ch.Type),
			"has_password": hash != "", "user_limit": ch.UserLimit, "parent_id": parentID,
			"audio_bitrate_kbps": ch.AudioBitrateKbps, "audio_frame_ms": ch.AudioFrameMS,
		}),
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
	// 会话作用域不能借服务器频道管理端点修改（#38 B2）。
	if e := serverChannelOnly(ch); e != nil {
		return nil, e
	}
	if e := s.requirePerm(ctx, ch.ServerID, c.UserID, store.ManageChannels); e != nil {
		return nil, e
	}
	req := patchBody(request.JSONBody, request.ApplicationMergePatchPlusJSONBody)
	if req == nil {
		return nil, bodyMissing()
	}
	// 有子频道时禁改 category/type/挂父（#37）：子树的分类/类型/层级跟着父走，
	// 半改会造成树形漂移；先于任何字段应用判，避免半写。
	if req.CategoryID.Set || req.Type.Set || req.ParentID.Set {
		has, err := s.hasChildren(ctx, ch.ServerID, ch.ID)
		if err != nil {
			return nil, s.writeStoreErr(ctx, err)
		}
		if has {
			return nil, errf(http.StatusConflict, "channel_has_children", "频道已有子频道，不能改分类/类型/父级")
		}
	}
	// Merge Patch（#31）：缺省不改，null = 清空（name/type 不可清 → 校验拒 400；
	// category_id 的 null = 移出分类；position 的 null 拒 400）。
	// 审计只记变更字段（#35 B4）；#37 起密码只记 password_changed。
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
	// #37 密码三态：缺省不改、null/空串清除、非空设置（校验用上方定稿的频道类型）。
	if req.Password.Set {
		if req.Password.Clearing() || req.Password.Val == "" {
			ch.PasswordHash = ""
		} else {
			if e := checkChannelPassword(req.Password.Val, ch.Type); e != nil {
				return nil, e
			}
			hash, err := hashChannelPassword(req.Password.Val)
			if err != nil {
				return nil, s.writeStoreErr(ctx, err)
			}
			ch.PasswordHash = hash
		}
		detail["password_changed"] = true
	}
	if req.UserLimit.Set {
		if req.UserLimit.Clearing() {
			return nil, errf(http.StatusBadRequest, "bad_request", "user_limit 不可清除")
		}
		if e := checkUserLimit(req.UserLimit.Val, ch.Type); e != nil {
			return nil, e
		}
		ch.UserLimit = req.UserLimit.Val
		detail["user_limit"] = req.UserLimit.Val
	}
	// #46 音质三态：缺省不改、0 = 清零（跟随设置/默认帧长）、null 拒 400。
	// 校验用上方定稿的频道类型：同一次 PATCH 改成 text 时非零音质会被拒。
	if req.AudioBitrateKbps.Set {
		if req.AudioBitrateKbps.Clearing() {
			return nil, errf(http.StatusBadRequest, "bad_channel_option", "audio_bitrate_kbps 不可清除，0 表示不限制")
		}
		if e := checkChannelBitrate(req.AudioBitrateKbps.Val, ch.Type); e != nil {
			return nil, e
		}
		ch.AudioBitrateKbps = req.AudioBitrateKbps.Val
		detail["audio_bitrate_kbps"] = req.AudioBitrateKbps.Val
	}
	if req.AudioFrameMs.Set {
		if req.AudioFrameMs.Clearing() {
			return nil, errf(http.StatusBadRequest, "bad_channel_option", "audio_frame_ms 不可清除，0 表示默认 20ms")
		}
		if e := checkChannelFrameMS(req.AudioFrameMs.Val, ch.Type); e != nil {
			return nil, e
		}
		ch.AudioFrameMS = req.AudioFrameMs.Val
		detail["audio_frame_ms"] = req.AudioFrameMs.Val
	}
	if req.ParentID.Set {
		if req.ParentID.Clearing() || req.ParentID.Val == 0 {
			ch.ParentID = nil // 提升顶层
			detail["parent_id"] = nil
		} else {
			_, catID, e := s.resolveParent(ctx, ch.ServerID, ch.ID, int64(req.ParentID.Val), ch.Type, ch.CategoryID, req.CategoryID.Set)
			if e != nil {
				return nil, e
			}
			v := int64(req.ParentID.Val)
			ch.ParentID = &v
			ch.CategoryID = catID // 父子同分类：未显式给分类时随父
			detail["parent_id"] = req.ParentID.Val
		}
	}
	// 类型改成 text 时清掉旧音质（text 恒 0/0）；校验已在各字段应用时按最终类型完成。
	normalizeChannelAudio(ch)
	// 子频道自身改 type/category 也必须与父一致（#101 P2）：上方 409 只拦
	// 「有子频道的父频道」，子频道此前可以单方面漂移出父的类型/分类，破坏
	// 两级树的不变量。字段应用完成后统一核对最终值，避免半写。
	// ParentID 被清空（提升顶层）时无父可校验；父频道自身要改 type/category
	// 仍由 hasChildren 409 拦截。
	if ch.ParentID != nil {
		parent, err := s.store.ChannelByID(ctx, *ch.ParentID)
		if err != nil {
			return nil, s.writeStoreErr(ctx, err)
		}
		if parent.ServerID != ch.ServerID {
			return nil, errf(http.StatusBadRequest, "bad_parent", "父频道不存在或不属于该服务器")
		}
		if parent.Type != ch.Type {
			return nil, errf(http.StatusBadRequest, "bad_parent", "父子频道类型必须相同")
		}
		if (ch.CategoryID == nil) != (parent.CategoryID == nil) ||
			(ch.CategoryID != nil && *ch.CategoryID != *parent.CategoryID) {
			return nil, errf(http.StatusBadRequest, "bad_parent", "父子频道分类必须相同")
		}
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
// 级联删除子频道（行级在 Store），并把父与全部语音子频道的房间成员踢出（#37）。
func (s *Server) DeleteChannel(ctx context.Context, request DeleteChannelRequestObject) (DeleteChannelResponseObject, error) {
	if e := checkID(request.Id); e != nil {
		return nil, e
	}
	c := currentUser(ctx)
	ch, err := s.store.ChannelByID(ctx, request.Id)
	if err != nil {
		return nil, s.writeStoreErr(ctx, err)
	}
	// 会话作用域不能借服务器频道管理端点删除（#38 B2）。
	if e := serverChannelOnly(ch); e != nil {
		return nil, e
	}
	if e := s.requirePerm(ctx, ch.ServerID, c.UserID, store.ManageChannels); e != nil {
		return nil, e
	}
	// 先收集子频道（Store 级联后就查不到了），踢房要覆盖父与全部语音子频道。
	children, err := s.channelChildren(ctx, ch.ServerID, ch.ID)
	if err != nil {
		return nil, s.writeStoreErr(ctx, err)
	}
	if err := s.store.DeleteChannel(ctx, request.Id); err != nil {
		return nil, s.writeStoreErr(ctx, err)
	}
	// 频道级覆盖随频道删除：快照作废，避免旧覆盖继续影响同服其它判定。
	s.authz.InvalidateServer(ch.ServerID)
	s.audit(ctx, store.AuditLog{
		ServerID: ch.ServerID, ActorID: c.UserID, Action: "channel.delete",
		TargetType: "channel", TargetID: ptrID(ch.ID), ChannelID: ptrID(ch.ID),
		Detail: auditDetail(map[string]any{
			"name": ch.Name, "type": string(ch.Type), "children": len(children),
		}),
	})
	// 频道删除后踢出房间内成员（#22）；语音子频道一并踢（#37，不留幽灵房）。
	if s.hub != nil {
		if ch.Type == store.ChannelVoice {
			s.hub.KickChannel(strconv.FormatInt(request.Id, 10), "channel_deleted")
		}
		for _, child := range children {
			if child.Type == store.ChannelVoice {
				s.hub.KickChannel(strconv.FormatInt(child.ID, 10), "channel_deleted")
			}
		}
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
