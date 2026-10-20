package api

import (
	"context"
	"net/http"

	"github.com/OpenALC/LiteTalk/server/internal/store"
)

// #35 B4：频道覆盖管理 REST。覆盖只作用于频道位；管理位属于角色，越界位 400
// （Store 侧还有一次校验兜底，ErrForbidden → 403）。

// overwriteView store 覆盖 → wire。
func overwriteView(o *store.ChannelOverwrite) ChannelOverwrite {
	return ChannelOverwrite{
		ChannelId:  idOf(o.ChannelID),
		TargetType: ChannelOverwriteTargetType(o.TargetType),
		TargetId:   idOf(o.TargetID),
		Allow:      formatPerm(o.Allow),
		Deny:       formatPerm(o.Deny),
		UpdatedAt:  timeOf(o.UpdatedAt),
	}
}

func overwriteViews(list []store.ChannelOverwrite) []ChannelOverwrite {
	out := make([]ChannelOverwrite, 0, len(list))
	for i := range list {
		out = append(out, overwriteView(&list[i]))
	}
	return out
}

// ListChannelOverwrites GET /api/v1/channels/{id}/overwrites（需 ManageChannels）
func (s *Server) ListChannelOverwrites(ctx context.Context, request ListChannelOverwritesRequestObject) (ListChannelOverwritesResponseObject, error) {
	if e := checkID(request.Id); e != nil {
		return nil, e
	}
	c := currentUser(ctx)
	ch, err := s.store.ChannelByID(ctx, request.Id)
	if err != nil {
		return nil, s.writeStoreErr(ctx, err)
	}
	if e := s.requireManageInOwnerServer(ctx, ch.ServerID, c.UserID, store.ManageChannels); e != nil {
		return nil, e
	}
	ows, err := s.store.ChannelOverwrites(ctx, ch.ID)
	if err != nil {
		return nil, s.writeStoreErr(ctx, err)
	}
	return ListChannelOverwrites200JSONResponse(overwriteViews(ows)), nil
}

// SetChannelOverwrite PUT /api/v1/channels/{id}/overwrites/{target_type}/{target_id}
func (s *Server) SetChannelOverwrite(ctx context.Context, request SetChannelOverwriteRequestObject) (SetChannelOverwriteResponseObject, error) {
	if e := checkID(request.Id); e != nil {
		return nil, e
	}
	if e := checkID(request.TargetId); e != nil {
		return nil, e
	}
	tt, e := overwriteTargetType(string(request.TargetType))
	if e != nil {
		return nil, e
	}
	c := currentUser(ctx)
	ch, err := s.store.ChannelByID(ctx, request.Id)
	if err != nil {
		return nil, s.writeStoreErr(ctx, err)
	}
	if e := s.requireManageInOwnerServer(ctx, ch.ServerID, c.UserID, store.ManageChannels); e != nil {
		return nil, e
	}
	if request.Body == nil {
		return nil, bodyMissing()
	}
	allow, e := parseOverwritePerm(request.Body.Allow)
	if e != nil {
		return nil, e
	}
	deny, e := parseOverwritePerm(request.Body.Deny)
	if e != nil {
		return nil, e
	}
	ow := &store.ChannelOverwrite{
		ChannelID:  ch.ID,
		TargetType: tt,
		TargetID:   request.TargetId,
		Allow:      allow,
		Deny:       deny,
	}
	// 目标同服校验在 Store（跨服/不存在统一 ErrNotFound → 404）。
	if err := s.store.SetChannelOverwrite(ctx, ow); err != nil {
		return nil, s.writeStoreErr(ctx, err)
	}
	// 覆盖参与频道可见性：快照作废，下一次列表/同步立即反映。
	s.authz.InvalidateServer(ch.ServerID)
	s.audit(ctx, store.AuditLog{
		ServerID: ch.ServerID, ActorID: c.UserID, Action: "channel.overwrite.set",
		TargetType: string(tt), TargetID: ptrID(request.TargetId), ChannelID: ptrID(ch.ID),
		Detail: auditDetail(map[string]any{
			"target_type": string(tt), "target_id": request.TargetId,
			"allow": request.Body.Allow, "deny": request.Body.Deny,
		}),
	})
	return SetChannelOverwrite200JSONResponse(overwriteView(ow)), nil
}

// DeleteChannelOverwrite DELETE /api/v1/channels/{id}/overwrites/{target_type}/{target_id}
func (s *Server) DeleteChannelOverwrite(ctx context.Context, request DeleteChannelOverwriteRequestObject) (DeleteChannelOverwriteResponseObject, error) {
	if e := checkID(request.Id); e != nil {
		return nil, e
	}
	if e := checkID(request.TargetId); e != nil {
		return nil, e
	}
	tt, e := overwriteTargetType(string(request.TargetType))
	if e != nil {
		return nil, e
	}
	c := currentUser(ctx)
	ch, err := s.store.ChannelByID(ctx, request.Id)
	if err != nil {
		return nil, s.writeStoreErr(ctx, err)
	}
	if e := s.requireManageInOwnerServer(ctx, ch.ServerID, c.UserID, store.ManageChannels); e != nil {
		return nil, e
	}
	if err := s.store.DeleteChannelOverwrite(ctx, ch.ID, tt, request.TargetId); err != nil {
		return nil, s.writeStoreErr(ctx, err)
	}
	s.authz.InvalidateServer(ch.ServerID)
	s.audit(ctx, store.AuditLog{
		ServerID: ch.ServerID, ActorID: c.UserID, Action: "channel.overwrite.delete",
		TargetType: string(tt), TargetID: ptrID(request.TargetId), ChannelID: ptrID(ch.ID),
		Detail: auditDetail(map[string]any{"target_type": string(tt), "target_id": request.TargetId}),
	})
	return DeleteChannelOverwrite200JSONResponse{Status: "ok"}, nil
}

// overwriteTargetType 路径枚举再校验（绑定层可能已拦，handler 不依赖它）。
func overwriteTargetType(s string) (store.OverwriteTargetType, *apiErr) {
	switch store.OverwriteTargetType(s) {
	case store.OverwriteTargetRole, store.OverwriteTargetMember:
		return store.OverwriteTargetType(s), nil
	}
	return "", errf(http.StatusBadRequest, "bad_request", "target_type 必须是 role 或 member")
}
