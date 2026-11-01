package api

import (
	"context"
	"encoding/json"
	"net/http"

	"github.com/OpenALC/LiteTalk/server/internal/store"
	"github.com/OpenALC/LiteTalk/server/internal/token"
)

// #35 B4：审计查询与写入。写入 best-effort——失败只记日志，绝不影响业务响应。

// audit 写审计行（#35 B4）。detail 只放变更字段的 JSON，
// 禁止邮箱/消息内容/令牌等隐私数据（对外可读面是 ManageServer）。
func (s *Server) audit(ctx context.Context, e store.AuditLog) {
	if err := s.store.WriteAudit(ctx, &e); err != nil {
		s.log.Printf("[audit] 写入失败 action=%s server=%d: %v", e.Action, e.ServerID, err)
	}
}

// auditDetail 变更字段 → JSON 字符串；序列化失败回空对象（不阻塞业务）。
func auditDetail(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		return "{}"
	}
	return string(b)
}

// auditView store 审计行 → wire（可空列缺省不回）。
func auditView(a *store.AuditLog) AuditLogEntry {
	out := AuditLogEntry{
		Id:        idOf(a.ID),
		ServerId:  idOf(a.ServerID),
		ActorId:   idOf(a.ActorID),
		Action:    a.Action,
		Detail:    a.Detail,
		CreatedAt: timeOf(a.CreatedAt),
	}
	if a.TargetType != "" {
		t := a.TargetType
		out.TargetType = &t
	}
	if a.TargetID != nil {
		v := ID(*a.TargetID)
		out.TargetId = &v
	}
	if a.ChannelID != nil {
		v := ID(*a.ChannelID)
		out.ChannelId = &v
	}
	return out
}

// ListAuditLog GET /api/v1/servers/{id}/audit-log（P1-2 双判定）。
// scope 腿：JWT 主体不受 scope 上限约束（行为不变）；令牌主体必须显式持有
// admin.read——审计面是 admin.read 的消费端点，guild.manage 不再顺带可读。
// 角色腿：纯角色 ManageServer（authz.RoleCanInServer，不做 scope 交集）。
// 两腿缺一不可：admin.read 归零后借位过门的路已断，只读令牌拿不到审计，
// 角色不够的管理令牌同样拿不到。
func (s *Server) ListAuditLog(ctx context.Context, request ListAuditLogRequestObject) (ListAuditLogResponseObject, error) {
	if e := checkID(request.Id); e != nil {
		return nil, e
	}
	if !token.ScopeAllows(ctx, token.ScopeAdminRead) {
		return nil, errf(http.StatusForbidden, "forbidden", "权限不足")
	}
	c := currentUser(ctx)
	if e := s.requireRolePerm(ctx, request.Id, c.UserID, store.ManageServer); e != nil {
		return nil, e
	}
	before := int64(0)
	if request.Params.Before != nil {
		if *request.Params.Before < 0 {
			return nil, errf(http.StatusBadRequest, "bad_cursor", "游标须为非负 ID")
		}
		before = *request.Params.Before
	}
	// 显式越界即 400（与消息分页同口径，不静默夹紧）。
	limit := 50
	if request.Params.Limit != nil {
		if *request.Params.Limit < 1 || *request.Params.Limit > 50 {
			return nil, errf(http.StatusBadRequest, "bad_limit", "limit 须为 1–50")
		}
		limit = *request.Params.Limit
	}
	logs, err := s.store.AuditLogBefore(ctx, request.Id, before, limit)
	if err != nil {
		return nil, s.writeStoreErr(ctx, err)
	}
	out := make([]AuditLogEntry, 0, len(logs))
	for i := range logs {
		out = append(out, auditView(&logs[i]))
	}
	return ListAuditLog200JSONResponse(out), nil
}
