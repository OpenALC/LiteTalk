package api

import (
	"context"
	"errors"
	"sort"
	"strconv"

	"github.com/OpenALC/LiteTalk/server/internal/store"
)

// #40 B2：未读与提及计数的 REST 面。
//
// 读快照只信「可见性复核后的频道集合」：服务器频道逐个走 visibleChannels
// （ViewChannel），会话逐个复核 channel_members，不能只依赖 store 的列表过滤。
// 写水位（mark read）与消息发送同一鉴权口径：会话非成员 404（不泄露存在性），
// 服务器频道无 ViewChannel 403。

// ListUnreads GET /api/v1/unreads（#40）
// 返回当前用户全部可见频道的未读/提及计数与按服务器聚合的计数；两类数组都
// 含 0 值项并按 ID 升序。服务器聚合只统计服务器文字频道，不含 direct/group。
func (s *Server) ListUnreads(ctx context.Context, _ ListUnreadsRequestObject) (ListUnreadsResponseObject, error) {
	c := currentUser(ctx)
	channelIDs, channelServer, serverIDs, err := s.visibleChannelSet(ctx, c.UserID)
	if err != nil {
		return nil, s.writeStoreErr(ctx, err)
	}

	serverTotals := make(map[int64]store.Unread, len(serverIDs))
	for _, id := range serverIDs {
		serverTotals[id] = store.Unread{}
	}
	counts, err := s.store.UnreadCounts(ctx, c.UserID, channelIDs)
	if err != nil {
		return nil, s.writeStoreErr(ctx, err)
	}
	sort.Slice(channelIDs, func(i, j int) bool { return channelIDs[i] < channelIDs[j] })

	channels := make([]UnreadChannel, 0, len(channelIDs))
	for _, id := range channelIDs {
		u := counts[id]
		channels = append(channels, unreadChannelView(id, u))
		if serverID := channelServer[id]; serverID > 0 {
			total := serverTotals[serverID]
			total.UnreadCount += u.UnreadCount
			total.MentionCount += u.MentionCount
			serverTotals[serverID] = total
		}
	}

	sort.Slice(serverIDs, func(i, j int) bool { return serverIDs[i] < serverIDs[j] })
	serverViews := make([]UnreadServer, 0, len(serverIDs))
	for _, id := range serverIDs {
		u := serverTotals[id]
		serverViews = append(serverViews, UnreadServer{
			ServerId:     idOf(id),
			UnreadCount:  u.UnreadCount,
			MentionCount: u.MentionCount,
		})
	}
	return ListUnreads200JSONResponse{Channels: channels, Servers: serverViews}, nil
}

// visibleChannelSet 收集当前用户全部可见的文字频道（#40 未读口径，/search 复用）：
// 服务器频道逐个走 visibleChannels（ViewChannel），direct/group 会话逐个复核
// channel_members。返回频道 ID（按发现顺序去重）、channelID→服务器 ID（会话为 0）
// 以及用户所在服务器 ID。非成员服务器/会话只记日志跳过，不泄露存在性。
func (s *Server) visibleChannelSet(ctx context.Context, uid int64) ([]int64, map[int64]int64, []int64, error) {
	servers, err := s.store.ServersOfUser(ctx, uid)
	if err != nil {
		return nil, nil, nil, err
	}
	channelServer := make(map[int64]int64)
	channelIDs := make([]int64, 0)
	serverIDs := make([]int64, 0, len(servers))

	for i := range servers {
		srv := &servers[i]
		if _, err := s.store.Member(ctx, srv.ID, uid); err != nil {
			if errors.Is(err, store.ErrNotFound) {
				// 理论上 ServersOfUser 已按成员过滤；实现变动时也不能泄露。
				s.log.Printf("[visible] 跳过非成员服务器 %d（用户 %d）", srv.ID, uid)
				continue
			}
			return nil, nil, nil, err
		}
		serverIDs = append(serverIDs, srv.ID)

		channels, err := s.store.ChannelsOfServer(ctx, srv.ID)
		if err != nil {
			return nil, nil, nil, err
		}
		textChannels := make([]store.Channel, 0, len(channels))
		for _, ch := range channels {
			if ch.Type == store.ChannelText && ch.Scope == store.ChannelScopeServer {
				textChannels = append(textChannels, ch)
			}
		}
		visible, err := s.visibleChannels(ctx, uid, srv.ID, textChannels)
		if err != nil {
			return nil, nil, nil, err
		}
		for _, ch := range visible {
			if _, dup := channelServer[ch.ID]; dup {
				continue
			}
			channelServer[ch.ID] = srv.ID
			channelIDs = append(channelIDs, ch.ID)
		}
	}

	conversations, err := s.store.ListConversations(ctx, uid)
	if err != nil {
		return nil, nil, nil, err
	}
	for i := range conversations {
		conv := &conversations[i]
		if !conversationScoped(conv) {
			continue
		}
		if _, err := s.store.ChannelMember(ctx, conv.ID, uid); err != nil {
			if errors.Is(err, store.ErrNotFound) {
				s.log.Printf("[visible] 跳过非成员会话 %d（用户 %d）", conv.ID, uid)
				continue
			}
			return nil, nil, nil, err
		}
		if _, dup := channelServer[conv.ID]; dup {
			continue
		}
		channelServer[conv.ID] = 0
		channelIDs = append(channelIDs, conv.ID)
	}
	return channelIDs, channelServer, serverIDs, nil
}

// MarkChannelRead PUT /api/v1/channels/{id}/read（#40）
// 水位只前进（store 原子取 max）；消息必须存在且属于路径频道。成功后回最新
// 计数，并经 hub 把 channel.unread 推给当前用户的全部会话（多设备同步）。
func (s *Server) MarkChannelRead(ctx context.Context, request MarkChannelReadRequestObject) (MarkChannelReadResponseObject, error) {
	if e := checkID(request.Id); e != nil {
		return nil, e
	}
	if request.Body == nil {
		return nil, bodyMissing()
	}
	c := currentUser(ctx)
	ch, e := s.visibleChannelFor(ctx, request.Id, c.UserID)
	if e != nil {
		return nil, e
	}
	messageID := int64(request.Body.LastMessageId)
	if messageID <= 0 {
		return nil, errf(400, "bad_read", "最后已读消息不合法")
	}
	msg, err := s.store.MessageByID(ctx, messageID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return nil, errf(400, "bad_read", "最后已读消息不存在")
		}
		return nil, s.writeStoreErr(ctx, err)
	}
	if msg.ChannelID != ch.ID {
		return nil, errf(400, "bad_read", "最后已读消息不属于该频道")
	}
	if err := s.store.SetReadState(ctx, c.UserID, ch.ID, messageID); err != nil {
		return nil, s.writeStoreErr(ctx, err)
	}
	counts, err := s.store.UnreadCounts(ctx, c.UserID, []int64{ch.ID})
	if err != nil {
		return nil, s.writeStoreErr(ctx, err)
	}
	u := counts[ch.ID]
	if s.hub != nil {
		uid := strconv.FormatInt(c.UserID, 10)
		s.hub.BroadcastChannelUnread(strconv.FormatInt(ch.ID, 10), map[string]store.Unread{uid: u})
	}
	return MarkChannelRead200JSONResponse(unreadChannelView(ch.ID, u)), nil
}

// unreadChannelView store 计数 → REST DTO。
func unreadChannelView(channelID int64, u store.Unread) UnreadChannel {
	return UnreadChannel{
		ChannelId:    idOf(channelID),
		UnreadCount:  u.UnreadCount,
		MentionCount: u.MentionCount,
	}
}

// broadcastUnreadAfterMessageChange REST 编辑/软删成功后的计数推送（#40）：
// 用与 message.edited/deleted 相同的收件人列表计算每人自己的计数。失败只记
// 日志——计数是附属信息，不能把已提交的编辑/删除回滚成 500。
func (s *Server) broadcastUnreadAfterMessageChange(ctx context.Context, channelID int64, recipients []string) {
	if s.hub == nil || len(recipients) == 0 {
		return
	}
	uids := make([]int64, 0, len(recipients))
	for _, raw := range recipients {
		uid, err := strconv.ParseInt(raw, 10, 64)
		if err != nil || uid <= 0 {
			continue
		}
		uids = append(uids, uid)
	}
	if len(uids) == 0 {
		return
	}
	counts, err := s.store.UnreadCountsForUsers(ctx, channelID, uids)
	if err != nil {
		s.log.Printf("[unreads] 计算频道 %d 的成员计数失败: %v", channelID, err)
		return
	}
	out := make(map[string]store.Unread, len(counts))
	for uid, u := range counts {
		out[strconv.FormatInt(uid, 10)] = u
	}
	s.hub.BroadcastChannelUnread(strconv.FormatInt(channelID, 10), out)
}

// initServerReadStates 入服初始化（#40）：新成员对每个服务器文字频道以当前
// 最新消息为水位，避免把全部历史看成未读。失败只记日志，不拖垮入服。
func (s *Server) initServerReadStates(ctx context.Context, userID, serverID int64) {
	channels, err := s.store.ChannelsOfServer(ctx, serverID)
	if err != nil {
		s.log.Printf("[unreads] 入服初始化查询服务器 %d 频道失败: %v", serverID, err)
		return
	}
	for _, ch := range channels {
		if ch.Type != store.ChannelText || ch.Scope != store.ChannelScopeServer {
			continue
		}
		s.ensureReadStateAtLatest(ctx, userID, ch.ID)
	}
}

// ensureReadStateAtLatest 以频道当前最新消息为水位补缺失的 read_states 行；
// 已有水位不覆盖。查最新消息或写入失败都只记日志。
func (s *Server) ensureReadStateAtLatest(ctx context.Context, userID, channelID int64) {
	msgs, err := s.store.MessagesBefore(ctx, channelID, 0, 1)
	if err != nil {
		s.log.Printf("[unreads] 查询频道 %d 最新消息失败: %v", channelID, err)
		return
	}
	var latest int64
	if len(msgs) > 0 {
		latest = msgs[len(msgs)-1].ID
	}
	if err := s.store.EnsureReadState(ctx, userID, channelID, latest); err != nil {
		s.log.Printf("[unreads] 初始化用户 %d 在频道 %d 的已读水位失败: %v", userID, channelID, err)
	}
}
