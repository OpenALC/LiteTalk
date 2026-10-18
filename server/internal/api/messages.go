package api

import (
	"context"
	"net/http"
	"strconv"
	"time"

	"github.com/OpenALC/LiteTalk/server/internal/store"
)

// ListMessages GET /api/v1/channels/{id}/messages?before_id=&limit=（分页 50 条）
func (s *Server) ListMessages(ctx context.Context, request ListMessagesRequestObject) (ListMessagesResponseObject, error) {
	if e := checkID(request.Id); e != nil {
		return nil, e
	}
	c := currentUser(ctx)
	ch, err := s.store.ChannelByID(ctx, request.Id)
	if err != nil {
		return nil, s.writeStoreErr(ctx, err)
	}
	// 成员才能读频道消息。
	if _, err := s.store.Member(ctx, ch.ServerID, c.UserID); err != nil {
		return nil, errf(http.StatusForbidden, "forbidden", "不是服务器成员")
	}
	var beforeID int64
	if v := request.Params.BeforeId; v != nil {
		beforeID = *v
	}
	limit := 50
	if v := request.Params.Limit; v != nil && *v > 0 && *v <= 50 {
		limit = *v
	}
	msgs, err := s.store.MessagesBefore(ctx, ch.ID, beforeID, limit)
	if err != nil {
		return nil, s.writeStoreErr(ctx, err)
	}
	return ListMessages200JSONResponse(msgs), nil
}

// 消息发送频控：20 条/分钟/用户。
const msgRateLimit = 20

// CreateMessage POST /api/v1/channels/{id}/messages（≤2000 字 + 频控）
func (s *Server) CreateMessage(ctx context.Context, request CreateMessageRequestObject) (CreateMessageResponseObject, error) {
	if e := checkID(request.Id); e != nil {
		return nil, e
	}
	if request.Body == nil {
		return nil, bodyMissing()
	}
	c := currentUser(ctx)
	ch, err := s.store.ChannelByID(ctx, request.Id)
	if err != nil {
		return nil, s.writeStoreErr(ctx, err)
	}
	if ch.Type != store.ChannelText {
		return nil, errf(http.StatusBadRequest, "bad_channel", "只能向文字频道发消息")
	}
	if _, err := s.store.Member(ctx, ch.ServerID, c.UserID); err != nil {
		return nil, errf(http.StatusForbidden, "forbidden", "不是服务器成员")
	}
	content := request.Body.Content
	if n := len([]rune(content)); n == 0 || n > 2000 {
		return nil, errf(http.StatusBadRequest, "bad_content", "消息长度需为 1–2000 字符")
	}
	allowed, err := s.limits.Allow(ctx, "msg:"+strconv.FormatInt(c.UserID, 10), msgRateLimit, time.Minute)
	if err != nil || !allowed {
		return nil, errf(http.StatusTooManyRequests, "rate_limited", "发送过于频繁")
	}
	msg := &store.Message{
		ChannelID: ch.ID,
		UserID:    c.UserID,
		Content:   content,
	}
	if err := s.store.CreateMessage(ctx, msg); err != nil {
		return nil, s.writeStoreErr(ctx, err)
	}
	msg.SenderNickname, msg.SenderTag = c.Nickname, c.Tag
	return CreateMessage201JSONResponse(*msg), nil
}
