package api

import (
	"net/http"
	"strconv"
	"time"

	"github.com/OpenALC/LiteTalk/server/internal/store"
)

// handleListMessages GET /api/v1/channels/{id}/messages?before_id=&limit=（分页 50 条）
func (s *Server) handleListMessages(w http.ResponseWriter, r *http.Request) {
	c := currentUser(r)
	id, ok := pathID(w, r, "id")
	if !ok {
		return
	}
	ch, err := s.store.ChannelByID(r.Context(), id)
	if err != nil {
		s.writeStoreErr(w, r, err)
		return
	}
	// 成员才能读频道消息。
	if _, err := s.store.Member(r.Context(), ch.ServerID, c.UserID); err != nil {
		writeErr(w, http.StatusForbidden, "forbidden", "不是服务器成员")
		return
	}
	var beforeID int64
	if v := r.URL.Query().Get("before_id"); v != "" {
		beforeID, _ = strconv.ParseInt(v, 10, 64)
	}
	limit := 50
	if v := r.URL.Query().Get("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 && n <= 50 {
			limit = n
		}
	}
	msgs, err := s.store.MessagesBefore(r.Context(), id, beforeID, limit)
	if err != nil {
		s.writeStoreErr(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, msgs)
}

type createMessageReq struct {
	Content string `json:"content"`
}

// 消息发送频控：20 条/分钟/用户。
const msgRateLimit = 20

// handleCreateMessage POST /api/v1/channels/{id}/messages（≤2000 字 + 频控）
func (s *Server) handleCreateMessage(w http.ResponseWriter, r *http.Request) {
	c := currentUser(r)
	id, ok := pathID(w, r, "id")
	if !ok {
		return
	}
	ch, err := s.store.ChannelByID(r.Context(), id)
	if err != nil {
		s.writeStoreErr(w, r, err)
		return
	}
	if ch.Type != store.ChannelText {
		writeErr(w, http.StatusBadRequest, "bad_channel", "只能向文字频道发消息")
		return
	}
	if _, err := s.store.Member(r.Context(), ch.ServerID, c.UserID); err != nil {
		writeErr(w, http.StatusForbidden, "forbidden", "不是服务器成员")
		return
	}
	var req createMessageReq
	if !decodeJSON(w, r, &req) {
		return
	}
	n := len([]rune(req.Content))
	if n == 0 || n > 2000 {
		writeErr(w, http.StatusBadRequest, "bad_content", "消息长度需为 1–2000 字符")
		return
	}
	allowed, err := s.limits.Allow(r.Context(), "msg:"+strconv.FormatInt(c.UserID, 10), msgRateLimit, time.Minute)
	if err != nil || !allowed {
		writeErr(w, http.StatusTooManyRequests, "rate_limited", "发送过于频繁")
		return
	}
	msg := &store.Message{
		ChannelID: id,
		UserID:    c.UserID,
		Content:   req.Content,
	}
	if err := s.store.CreateMessage(r.Context(), msg); err != nil {
		s.writeStoreErr(w, r, err)
		return
	}
	msg.SenderNickname, msg.SenderTag = c.Nickname, c.Tag
	writeJSON(w, http.StatusCreated, msg)
}
