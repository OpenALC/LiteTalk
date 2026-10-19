package api

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/OpenALC/LiteTalk/server/internal/store"
)

// ListMessages GET /api/v1/channels/{id}/messages?before=&after=&limit=（游标分页 50 条，#31）
// 游标语义：before 取更旧一页（ID 小于游标最新 limit 条）、after 取更新一页，
// 均按旧→新正序返回。禁 offset。before_id 是 before 的旧别名（兼容一个版本）。
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
	// 游标互斥：before（含旧别名 before_id）优先于 after。
	var beforeID, afterID int64
	if v := request.Params.Before; v != nil {
		beforeID = *v
	} else if v := request.Params.BeforeId; v != nil {
		beforeID = *v // 旧参数别名（#31 兼容窗口，一个版本后移除）
	} else if v := request.Params.After; v != nil {
		afterID = *v
	}
	if beforeID < 0 || afterID < 0 {
		return nil, errf(http.StatusBadRequest, "bad_cursor", "游标须为非负 ID")
	}
	limit := 50
	if v := request.Params.Limit; v != nil && *v > 0 && *v <= 50 {
		limit = *v
	}
	if afterID > 0 {
		msgs, err := s.store.MessagesAfter(ctx, ch.ID, afterID, limit)
		if err != nil {
			return nil, s.writeStoreErr(ctx, err)
		}
		return ListMessages200JSONResponse(messageViews(msgs)), nil
	}
	msgs, err := s.store.MessagesBefore(ctx, ch.ID, beforeID, limit)
	if err != nil {
		return nil, s.writeStoreErr(ctx, err)
	}
	return ListMessages200JSONResponse(messageViews(msgs)), nil
}

// 消息发送频控：20 条/分钟/用户。
const msgRateLimit = 20

// CreateMessage POST /api/v1/channels/{id}/messages（≤2000 字 + 频控 + nonce 幂等，#31）
// nonce 幂等：同频道同用户同 nonce 只落一条，重复提交回读原消息 200（弱网重试对账）。
// 幂等判断先于频控：重试撞上配额时也必须拿回原消息，而不是 429（审查 P2）。
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
	var nonce string
	if request.Body.Nonce != nil {
		nonce = *request.Body.Nonce
		if len(nonce) > 64 {
			return nil, errf(http.StatusBadRequest, "bad_nonce", "nonce 需为 0–64 字符")
		}
	}
	// 幂等命中直接回原消息，不消耗频控配额（弱网重试先于配额判断）。
	if nonce != "" {
		orig, err := s.store.MessageByNonce(ctx, ch.ID, c.UserID, nonce)
		if err == nil {
			return CreateMessage200JSONResponse(messageView(orig)), nil
		}
		if !errors.Is(err, store.ErrNotFound) {
			return nil, s.writeStoreErr(ctx, err)
		}
	}
	if e := s.allowRate(ctx, "msg:"+strconv.FormatInt(c.UserID, 10), msgRateLimit, time.Minute, "发送过于频繁"); e != nil {
		return nil, e
	}
	msg := &store.Message{
		ChannelID: ch.ID,
		UserID:    c.UserID,
		Content:   content,
		Nonce:     nonce,
	}
	if err := s.store.CreateMessage(ctx, msg); err != nil {
		// 撞 (channel_id, user_id, nonce) 唯一索引 = 幂等重放：回读原消息返回 200。
		if nonce != "" && errors.Is(err, store.ErrConflict) {
			orig, rerr := s.store.MessageByNonce(ctx, ch.ID, c.UserID, nonce)
			if rerr != nil {
				return nil, s.writeStoreErr(ctx, rerr)
			}
			return CreateMessage200JSONResponse(messageView(orig)), nil
		}
		return nil, s.writeStoreErr(ctx, err)
	}
	msg.SenderNickname, msg.SenderTag = c.Nickname, c.Tag
	return CreateMessage201JSONResponse(messageView(msg)), nil
}
