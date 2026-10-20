package api

import (
	"context"
	"errors"
	"net/http"
	"strconv"

	"github.com/OpenALC/LiteTalk/server/internal/signal"
	"github.com/OpenALC/LiteTalk/server/internal/store"
)

// #36：服务端强制闭麦 REST。SFU 侧包级丢弃（track 不摘）由 Hub→SFU 完成，
// 这里只做资源/权限/形态校验与审计。

// SetServerMuted PUT /api/v1/channels/{id}/members/{user_id}/server-mute（需 MuteMembers）
func (s *Server) SetServerMuted(ctx context.Context, request SetServerMutedRequestObject) (SetServerMutedResponseObject, error) {
	if e := checkID(request.Id); e != nil {
		return nil, e
	}
	if e := checkID(request.UserId); e != nil {
		return nil, e
	}
	c := currentUser(ctx)
	// 资源→权限→body（#35 B3 口径）：非成员/无权 403，不泄露频道内容。
	ch, err := s.store.ChannelByID(ctx, request.Id)
	if err != nil {
		return nil, s.writeStoreErr(ctx, err)
	}
	if e := s.requirePerm(ctx, ch.ServerID, c.UserID, store.MuteMembers); e != nil {
		return nil, e
	}
	if request.Body == nil {
		return nil, bodyMissing()
	}
	if ch.Type != store.ChannelVoice {
		return nil, errf(http.StatusBadRequest, "bad_channel", "只能对语音频道成员强制闭麦")
	}
	// 自我操作无意义（管理自己闭麦用本地开关），拒 400 钉住语义。
	if request.UserId == c.UserID {
		return nil, errf(http.StatusBadRequest, "bad_request", "不能对自己操作")
	}
	muted := request.Body.Muted
	if s.hub == nil {
		// 无信令面（纯 REST 测试/降级）：没有语音房，目标必然不在房。
		return nil, errf(http.StatusNotFound, "not_found", "目标不在该频道中")
	}
	if err := s.hub.SetServerMuted(strconv.FormatInt(ch.ID, 10), strconv.FormatInt(request.UserId, 10), muted); err != nil {
		// 目标不在该频道语音房（或 SFU 侧同义失败）→ 404；其余按 500。
		if errors.Is(err, signal.ErrNotInChannel) {
			return nil, errf(http.StatusNotFound, "not_found", "目标不在该频道中")
		}
		s.log.Printf("[500] %s: server-mute: %v", reqPath(ctx), err)
		return nil, errf(http.StatusInternalServerError, "internal", "服务器内部错误")
	}
	action := "voice.server_mute"
	if !muted {
		action = "voice.server_unmute"
	}
	s.audit(ctx, store.AuditLog{
		ServerID: ch.ServerID, ActorID: c.UserID, Action: action,
		TargetType: "member", TargetID: ptrID(request.UserId), ChannelID: ptrID(ch.ID),
		Detail: auditDetail(map[string]any{"target": request.UserId, "muted": muted}),
	})
	return SetServerMuted200JSONResponse{Status: "ok"}, nil
}
