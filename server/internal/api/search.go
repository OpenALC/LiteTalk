package api

import (
	"context"
	"net/http"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

// #42 B1：消息搜索 REST 面。
//
// 可见性口径与未读快照一致：服务器频道逐个复核 ViewChannel，direct/group 会话
// 逐个复核 channel_members；store 只按服务端算出的频道集合过滤，客户端无法把
// 频道 ID 塞进搜索范围。结果复用消息视图装配（附件、提及、回复预览）。

// 搜索频控：30 次/分钟/用户。
const searchRateLimit = 30

// SearchMessages GET /api/v1/search?q=&limit=（#42 B1）
func (s *Server) SearchMessages(ctx context.Context, request SearchMessagesRequestObject) (SearchMessagesResponseObject, error) {
	c := currentUser(ctx)
	q := strings.TrimSpace(request.Params.Q)
	if q == "" || utf8.RuneCountInString(q) > 100 {
		return nil, errf(http.StatusBadRequest, "bad_query", "搜索词需为 1–100 字符")
	}
	// 与消息分页同口径：显式 limit 越界 400，不静默回落缺省值。
	limit := 20
	if request.Params.Limit != nil {
		if *request.Params.Limit < 1 || *request.Params.Limit > 50 {
			return nil, errf(http.StatusBadRequest, "bad_limit", "limit 须为 1–50")
		}
		limit = *request.Params.Limit
	}
	if e := s.allowRate(ctx, "search:"+strconv.FormatInt(c.UserID, 10),
		searchRateLimit, time.Minute, "搜索过于频繁"); e != nil {
		return nil, e
	}
	channelIDs, _, _, err := s.visibleChannelSet(ctx, c.UserID)
	if err != nil {
		return nil, s.writeStoreErr(ctx, err)
	}
	// 无可见频道时 store 直接返回空结果；不做特判，避免两处口径漂移。
	msgs, err := s.store.SearchMessages(ctx, channelIDs, q, limit)
	if err != nil {
		return nil, s.writeStoreErr(ctx, err)
	}
	views, e := s.messageViews(ctx, msgs)
	if e != nil {
		return nil, e
	}
	return SearchMessages200JSONResponse(views), nil
}
