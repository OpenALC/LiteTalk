package api

import (
	"context"

	"github.com/OpenALC/LiteTalk/server/internal/store"
)

// GetSync GET /api/v1/sync（#78）：事件续传失败（gateway.invalid_session）后的
// 聚合快照，一次请求恢复服务器/频道/好友视图。
//
// 归属校验：ServersOfUser 本身按 server_members 关联过滤，这里仍逐服复核
// Member——store 实现变动或未来加缓存时，不能把非成员服务器/频道泄露出去。
// 非成员（理论不可达）直接跳过并记日志，而不是让整个快照失败。
//
// 性能：store 没有批量取分类/频道的接口，这里是逐服 N+1；本端点只在续传
// 失败时低频触发，先接受，后续可随 store 批量接口一起收口。
// recent_messages 未包含在快照内：按「用户全部服务器的文字频道」逐频道取
// 最近消息的响应体积不可控（频道数 × 每频道条数 × 2000 字），而客户端已有
// seenChans 范围的消息补拉，聚合端点只解决服务器/频道/好友视图。
func (s *Server) GetSync(ctx context.Context, request GetSyncRequestObject) (GetSyncResponseObject, error) {
	c := currentUser(ctx)
	servers, err := s.store.ServersOfUser(ctx, c.UserID)
	if err != nil {
		return nil, s.writeStoreErr(ctx, err)
	}
	out := make([]SyncServer, 0, len(servers))
	for i := range servers {
		srv := &servers[i]
		if _, err := s.store.Member(ctx, srv.ID, c.UserID); err != nil {
			s.log.Printf("[sync] 跳过非成员服务器 %d（用户 %d）: %v", srv.ID, c.UserID, err)
			continue
		}
		channels, err := s.store.ChannelsOfServer(ctx, srv.ID)
		if err != nil {
			return nil, s.writeStoreErr(ctx, err)
		}
		categories, err := s.store.CategoriesOfServer(ctx, srv.ID)
		if err != nil {
			return nil, s.writeStoreErr(ctx, err)
		}
		// 空结果落 [] 而非 null（契约 SyncServer）。
		if channels == nil {
			channels = []store.Channel{}
		}
		if categories == nil {
			categories = []store.Category{}
		}
		base := serverView(srv)
		out = append(out, SyncServer{
			Id:         base.Id,
			Name:       base.Name,
			OwnerId:    base.OwnerId,
			IconName:   base.IconName,
			IconColor:  base.IconColor,
			CreatedAt:  base.CreatedAt,
			Categories: categoryViews(categories),
			Channels:   channelViews(channels),
		})
	}
	friends, err := s.friendEntries(ctx, c.UserID)
	if err != nil {
		return nil, s.writeStoreErr(ctx, err)
	}
	return GetSync200JSONResponse(SyncSnapshot{Servers: out, Friends: friends}), nil
}
