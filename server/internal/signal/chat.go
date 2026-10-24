// chat 文字聊天：chat.send（C→S）→ 校验/幂等/频控/权限 → 落库 → chat.message 广播（S→C）。
// wire 契约与客户端 client/core/signal/protocol_gen.go 对齐（ChatSendData / ChatMessageData）。
package signal

import (
	"context"
	"errors"
	"sort"
	"strconv"
	"time"

	"github.com/OpenALC/LiteTalk/server/internal/store"
)

// ChatStore chat 持久化最小接口（store.Store 满足；nil 时仅广播不落库）。
type ChatStore interface {
	CreateMessage(ctx context.Context, msg *store.Message) error
	// MessageByNonce 幂等回读：按 (channel_id, user_id, nonce) 找原消息（#31）。
	MessageByNonce(ctx context.Context, channelID, userID int64, nonce string) (*store.Message, error)
	// MessageByID 单条回读（#39）：回复目标校验与预览取昵称；含软删行。
	MessageByID(ctx context.Context, id int64) (*store.Message, error)
	// MentionsOfMessages 批量读提及（#39）：幂等回显从库回填，不信任重放请求。
	MentionsOfMessages(ctx context.Context, ids []int64) (map[int64][]int64, error)
	ChannelByID(ctx context.Context, id int64) (*store.Channel, error)
	Member(ctx context.Context, serverID, userID int64) (*store.ServerMember, error)
	// MembersOfServer 全量成员（#32 事件面广播定向：成员的全部会话进环，
	// 不再只推在线连接——断线续传窗口内的会话要能补发）。
	MembersOfServer(ctx context.Context, serverID int64) ([]store.ServerMember, error)
	// ChannelMembers 会话成员（#38）：私聊/群聊的可见性与广播范围都以成员表为准。
	ChannelMembers(ctx context.Context, channelID int64) ([]store.ChannelMember, error)
	// UnreadCountsForUsers 单频道多用户未读/提及计数（#40）：chat.message 广播后
	// 给每个收件人推自己的 channel.unread。store.Store 满足。
	UnreadCountsForUsers(ctx context.Context, channelID int64, userIDs []int64) (map[int64]store.Unread, error)
}

// RateLimiter 发送频控接口（auth.RateLimit 满足；nil 时不限流）。
type RateLimiter interface {
	Allow(ctx context.Context, key string, limit int, window time.Duration) (bool, error)
}

// 聊天发送限制：2000 字、20 条/分（key 与 REST messages 共用 msg: 前缀，用户级统一配额）。
const (
	chatMaxRunes   = 2000
	chatMaxNonce   = 64
	chatRateLimit  = 20
	chatRateWindow = time.Minute
)

// handleChatSend 文字消息发送：内容/类型/长度校验 → 权限 → 幂等 → 频控 → 落库 → 广播。
// 广播范围（#32 事件面 + #35 B3 过滤）：该频道所属服务器**有 ViewChannel 的成员会话**——
// 在线实时投递，断线续传窗口内的进事件环补发；无权限成员两路都不收。
// 私聊/群聊频道（#38）以 channel_members 为唯一权限口径：成员即可见可发，广播目标
// 即全部成员（含发送者），不查服务器权限。dev 内置房间回退为房内广播。
// 文字消息只进文字频道（语音频道 bad_message，#35 B3）。
//
// nonce 幂等（#31）：同频道同用户同 nonce 只落一条、只广播一次；重复请求把原消息
// 定向回显给发起者（不进事件环、不再打扰其他成员），弱网重试据此对账。
func (h *Hub) handleChatSend(c *client, d ChatSendData) {
	if d.ChannelID == "" {
		h.sendError(c, ErrCodeBadMessage, "chat.send 需要 channel_id")
		return
	}
	// 消息类型校验（§0.12）：本期仅 text。
	// 缺省字段按 text 处理——v1.0.x 客户端不带 type，服务端升级后不能直接拒收（P1-3 兼容）。
	if d.Type == "" {
		d.Type = "text"
	}
	if d.Type != "text" {
		h.sendError(c, ErrCodeBadMessage, "chat.send 仅支持 type=text")
		return
	}
	if n := len([]rune(d.Content)); n == 0 || n > chatMaxRunes {
		h.sendError(c, ErrCodeBadMessage, "消息长度需为 1–2000 字符")
		return
	}
	if len(d.Nonce) > chatMaxNonce {
		h.sendError(c, ErrCodeBadMessage, "nonce 需为 0–64 字符")
		return
	}
	ctx := context.Background()

	// 权限二选一：当前在该房间（M2 dev 房间场景）；
	// 或 channel_id 为数字频道且本人有该频道的发送资格（服务器文字频道 / 会话成员）。
	inRoom := c.roomIDLocked() == d.ChannelID
	var (
		chID, uid int64
		serverID  int64
		persist   bool // 有 store 频道实体：落库 + 幂等 + 事件面广播
		// convMembers 非 nil = 私聊/群聊频道：成员表同时是发送资格与广播范围，
		// 跳过服务器 authz（成员即拥有 View/Send）。
		convMembers []store.ChannelMember
	)
	if id, err := strconv.ParseInt(d.ChannelID, 10, 64); err == nil && h.chatStore != nil {
		if ch, err := h.chatStore.ChannelByID(ctx, id); err == nil {
			u, uerr := strconv.ParseInt(c.userID, 10, 64)
			if isConversationChannel(ch) {
				// 会话频道（#38）：非成员与非会话一律 not_found，不泄露会话是否存在。
				if uerr != nil {
					h.sendError(c, ErrCodeNotFound, "会话不存在")
					return
				}
				members, merr := h.chatStore.ChannelMembers(ctx, id)
				if merr != nil {
					h.log.Printf("chat: 查会话 %d 成员失败: %v", id, merr)
					h.sendError(c, ErrCodeInternal, "消息发送失败")
					return
				}
				if !hasChannelMember(members, u) {
					h.sendError(c, ErrCodeNotFound, "会话不存在")
					return
				}
				// 类型检查放在成员判定之后——非成员统一 not_found，不泄露频道类型。
				if ch.Type != store.ChannelText {
					h.sendError(c, ErrCodeBadMessage, "会话不支持文字消息")
					return
				}
				chID, uid, persist, convMembers = id, u, true, members
			} else {
				serverID = ch.ServerID
				if uerr == nil {
					if _, err := h.chatStore.Member(ctx, ch.ServerID, u); err == nil {
						chID, uid, persist = id, u, true
						// 频道类型检查（#35 B3）：文字消息只进文字频道，语音频道 bad_message。
						// 放在成员判定之后——非成员统一 forbidden，不泄露频道类型。
						if ch.Type != store.ChannelText {
							h.sendError(c, ErrCodeBadMessage, "语音频道不支持文字消息")
							return
						}
					}
				}
			}
		}
	}
	// dev 内置房间（无 store 实体）才走 inRoom；真实会话频道已在上面判定，
	// 非成员不会被房间归属绕过。
	if !inRoom && !persist {
		h.sendError(c, ErrCodeForbidden, "无权向该频道发送消息")
		return
	}
	// B3 权限：store 服务器频道需 ViewChannel+SendMessages（在幂等回读之前）；
	// 会话频道成员即拥有权限；dev 内置房间维持原语义（无频道实体，进房即可见）。
	if persist && convMembers == nil {
		for _, perm := range []store.Permission{store.ViewChannel, store.SendMessages} {
			if code, msg := h.checkPerm(ctx, uid, serverID, chID, perm, "无权向该频道发送消息"); code != "" {
				h.sendError(c, code, msg)
				return
			}
		}
	}

	// 回复与提及（#39）：校验先于幂等回读——无效请求不能因 nonce 命中拿到原消息
	// （与 REST CreateMessage 同口径）。内置房间没有消息实体，两个字段一律拒收，
	// 避免静默丢弃。
	var (
		replyTo      *int64
		replyPreview *ReplyPreview
		mentionIDs   []int64
	)
	if !persist {
		if d.ReplyTo != "" || len(d.Mentions) > 0 {
			h.sendError(c, ErrCodeBadMessage, "内置房间不支持回复或提及")
			return
		}
	} else {
		var code, msg string
		replyTo, replyPreview, code, msg = h.resolveChatReply(ctx, chID, d.ReplyTo)
		if code != "" {
			h.sendError(c, code, msg)
			return
		}
		mentionIDs, code, msg = h.resolveChatMentions(ctx, chID, serverID, convMembers, d.Mentions)
		if code != "" {
			h.sendError(c, code, msg)
			return
		}
	}

	// 幂等判断先于频控：弱网重试撞上配额时也必须拿回原消息，而不是 rate_limited。
	if persist && d.Nonce != "" {
		orig, err := h.chatStore.MessageByNonce(ctx, chID, uid, d.Nonce)
		switch {
		case err == nil:
			out, rerr := h.chatMessageFromStore(ctx, orig, c)
			if rerr != nil {
				h.log.Printf("chat: 幂等回显补全失败: %v", rerr)
				h.sendError(c, ErrCodeInternal, "消息保存失败")
				return
			}
			h.sendJSON(c, TypeChatMessage, out)
			return
		case !errors.Is(err, store.ErrNotFound):
			h.log.Printf("chat: nonce 幂等回读失败: %v", err)
			h.sendError(c, ErrCodeInternal, "消息保存失败")
			return
		}
	}

	if h.limits != nil {
		ok, err := h.limits.Allow(ctx, "msg:"+c.userID, chatRateLimit, chatRateWindow)
		if err != nil || !ok {
			h.sendError(c, ErrCodeRateLimited, "发送过于频繁")
			return
		}
	}

	out := ChatMessageData{
		ChannelID: d.ChannelID,
		Content:   d.Content,
		TS:        time.Now().UnixMilli(),
		User:      UserBrief{ID: c.userID, Nickname: c.username},
		Nonce:     d.Nonce, // 仅发送方带 nonce 时回显（omitempty）
	}
	if replyTo != nil {
		out.ReplyTo = strconv.FormatInt(*replyTo, 10)
		out.ReplyPreview = replyPreview
	}
	out.Mentions = formatMentionIDs(mentionIDs)
	if persist {
		msg := &store.Message{
			ChannelID: chID, UserID: uid, Content: d.Content, Nonce: d.Nonce,
			ReplyTo: replyTo, Mentions: mentionIDs,
		}
		if err := h.chatStore.CreateMessage(ctx, msg); err != nil {
			// 并发同 nonce：前一条已落库，回读原消息定向回显，不再广播。
			if d.Nonce != "" && errors.Is(err, store.ErrConflict) {
				if orig, rerr := h.chatStore.MessageByNonce(ctx, chID, uid, d.Nonce); rerr == nil {
					echo, cerr := h.chatMessageFromStore(ctx, orig, c)
					if cerr != nil {
						h.log.Printf("chat: 幂等回显补全失败: %v", cerr)
						h.sendError(c, ErrCodeInternal, "消息保存失败")
						return
					}
					h.sendJSON(c, TypeChatMessage, echo)
					return
				}
			}
			h.sendError(c, ErrCodeInternal, "消息保存失败")
			return
		}
		out.ID = strconv.FormatInt(msg.ID, 10)
		out.TS = msg.CreatedAt.UnixMilli()
	} else {
		// dev 房间（无 store 频道）：仅广播，id 用时间戳占位。
		out.ID = strconv.FormatInt(out.TS, 10)
	}

	// 广播（#32 事件面）：会话频道 → 全部成员（含发送者，成员表即权限表）；
	// 服务器频道 → 该服务器有 ViewChannel 的成员会话（逐收件人过滤（#35 B3）：
	// 无权限者不进事件环、不收实时投递，单个收件人查询失败只记日志跳过——
	// 不能把整条消息拖成 500）；dev 房间 → 房内成员。
	// 发送者恒在目标内（前端以广播替换本地回显，不另做回执）。
	uids := map[string]bool{c.userID: true}
	switch {
	case convMembers != nil:
		for _, m := range convMembers {
			uids[formatUID(m.UserID)] = true
		}
	case persist:
		members, err := h.chatStore.MembersOfServer(ctx, serverID)
		if err != nil {
			h.log.Printf("chat: 查服务器 %d 成员失败: %v", serverID, err)
		}
		for _, m := range members {
			if m.UserID == uid {
				continue // 发送者已恒在目标内
			}
			ok, err := h.permCheckQuiet(ctx, m.UserID, serverID, chID, store.ViewChannel)
			if err != nil {
				h.log.Printf("chat: 收件人 %d 权限查询失败，跳过: %v", m.UserID, err)
				continue
			}
			if ok {
				uids[formatUID(m.UserID)] = true
			}
		}
	default:
		h.mu.Lock()
		if room := h.rooms[d.ChannelID]; room != nil {
			for _, m := range room.members {
				uids[m.userID] = true
			}
		}
		h.mu.Unlock()
	}
	h.broadcastEvent(uids, TypeChatMessage, out)
	// 未读计数（#40）：消息事件之后给每个收件人推自己的精确计数。失败只记日志，
	// 不影响已经广播/落库的消息。
	if persist {
		h.broadcastChatUnread(ctx, chID, uids)
	}
}

// broadcastChatUnread 计算并推送 chat.send 落库后各收件人的未读/提及计数。
// uids 是 chat.message 的实际收件人集合（十进制字符串），发送者也在内；计数
// 由 store 按「水位之后、非本人、未软删」统一计算，调用方不自行加减。
func (h *Hub) broadcastChatUnread(ctx context.Context, channelID int64, uids map[string]bool) {
	if h.chatStore == nil || len(uids) == 0 {
		return
	}
	ids := make([]int64, 0, len(uids))
	for raw := range uids {
		uid, err := strconv.ParseInt(raw, 10, 64)
		if err != nil || uid <= 0 {
			continue
		}
		ids = append(ids, uid)
	}
	if len(ids) == 0 {
		return
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	counts, err := h.chatStore.UnreadCountsForUsers(ctx, channelID, ids)
	if err != nil {
		h.log.Printf("chat: 计算频道 %d 未读计数失败: %v", channelID, err)
		return
	}
	wire := make(map[string]store.Unread, len(counts))
	for uid, u := range counts {
		wire[formatUID(uid)] = u
	}
	h.BroadcastChannelUnread(strconv.FormatInt(channelID, 10), wire)
}

// isConversationChannel 是否私聊/群聊频道（#38）：无服务器、成员表定义权限。
func isConversationChannel(ch *store.Channel) bool {
	return ch.Scope == store.ChannelScopeDirect || ch.Scope == store.ChannelScopeGroup
}

// hasChannelMember 会话成员表是否包含 uid。
func hasChannelMember(members []store.ChannelMember, uid int64) bool {
	for _, m := range members {
		if m.UserID == uid {
			return true
		}
	}
	return false
}

// BroadcastConversationChanged 会话变更通知（#38 B3）：REST 在 DB 提交成功后调用，
// 目标为相关成员 uid（十进制字符串，调用方已去重）。空列表直接返回。
// 走事件面广播：在线连接实时投递，断线续传窗口内的会话进环补发。
func (h *Hub) BroadcastConversationChanged(uids []string, conversationID, action string) {
	targets := targetSet(uids)
	if len(targets) == 0 {
		return
	}
	h.broadcastEvent(targets, TypeConversationChanged, ConversationChangedData{
		ConversationID: conversationID,
		Action:         action,
	})
}

// BroadcastMessageEdited 消息编辑广播（#39）：REST 编辑成功后调用，目标为该频道
// 可见成员（uids 为十进制字符串，调用方已按 ViewChannel/会话成员过滤）。
// 空列表直接返回；mentions 为空时不下发该字段。
func (h *Hub) BroadcastMessageEdited(uids []string, channelID, messageID, content string, editedAt int64, mentions []string) {
	targets := targetSet(uids)
	if len(targets) == 0 {
		return
	}
	h.broadcastEvent(targets, TypeMessageEdited, MessageEditedData{
		ChannelID: channelID,
		MessageID: messageID,
		Content:   content,
		EditedAt:  editedAt,
		Mentions:  mentions,
	})
}

// BroadcastMessageDeleted 消息软删除广播（#39）：REST 软删成功后调用，
// 目标口径与 BroadcastMessageEdited 相同。
func (h *Hub) BroadcastMessageDeleted(uids []string, channelID, messageID string) {
	targets := targetSet(uids)
	if len(targets) == 0 {
		return
	}
	h.broadcastEvent(targets, TypeMessageDeleted, MessageDeletedData{
		ChannelID: channelID,
		MessageID: messageID,
	})
}

// BroadcastChannelUnread 未读/提及计数广播（#40）：counts 按十进制 uid 携带
// 各接收者自己的精确计数，逐用户走事件面（在线实时 + 断线续传环）。不能合并成
// 一次群发——同一条消息对不同用户的已读水位不同，计数也不同。空 map 不广播。
func (h *Hub) BroadcastChannelUnread(channelID string, counts map[string]store.Unread) {
	if channelID == "" || len(counts) == 0 {
		return
	}
	for uid, u := range counts {
		if uid == "" {
			continue
		}
		h.broadcastEvent(map[string]bool{uid: true}, TypeChannelUnread, ChannelUnreadData{
			ChannelID:    channelID,
			UnreadCount:  int64(u.UnreadCount),
			MentionCount: int64(u.MentionCount),
		})
	}
}

// targetSet 广播目标去重（空 uid 丢弃）。
func targetSet(uids []string) map[string]bool {
	targets := make(map[string]bool, len(uids))
	for _, uid := range uids {
		if uid != "" {
			targets[uid] = true
		}
	}
	return targets
}

// permCheckQuiet 只要结果不翻译错误码（广播过滤用：失败=跳过该收件人）。
func (h *Hub) permCheckQuiet(ctx context.Context, uid, serverID, channelID int64, perm store.Permission) (bool, error) {
	if h.authz == nil {
		return false, errors.New("signal: 权限服务未就绪")
	}
	return h.authz.CanInServer(ctx, uid, serverID, channelID, perm)
}

// resolveChatReply 校验 chat.send 的 reply_to（#39）：必须是同频道内存在的消息；
// 目标已软删也允许（预览 deleted=true）。不合法统一 bad_reply。
func (h *Hub) resolveChatReply(ctx context.Context, chID int64, raw string) (*int64, *ReplyPreview, string, string) {
	if raw == "" {
		return nil, nil, "", ""
	}
	id, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || id <= 0 {
		return nil, nil, ErrCodeBadReply, "回复目标不合法"
	}
	target, err := h.chatStore.MessageByID(ctx, id)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return nil, nil, ErrCodeBadReply, "回复的消息不存在"
		}
		h.log.Printf("chat: 回复目标回读失败: %v", err)
		return nil, nil, ErrCodeInternal, "消息发送失败"
	}
	if target.ChannelID != chID {
		return nil, nil, ErrCodeBadReply, "不能回复其它频道的消息"
	}
	return &id, replyPreviewFromStore(target), "", ""
}

// resolveChatMentions 校验并归一 chat.send 的 mentions（#39）：去重、元素须为正 ID，
// 每个用户必须可见该频道（会话频道看成员表，服务器频道要求 ViewChannel）。
// 结果按 user_id 升序，保证广播与 REST 视图同序。任一不合法返回 bad_mention。
func (h *Hub) resolveChatMentions(ctx context.Context, chID, serverID int64, convMembers []store.ChannelMember, raw []string) ([]int64, string, string) {
	if len(raw) == 0 {
		return nil, "", ""
	}
	seen := make(map[int64]struct{}, len(raw))
	out := make([]int64, 0, len(raw))
	for _, s := range raw {
		uid, err := strconv.ParseInt(s, 10, 64)
		if err != nil || uid <= 0 {
			return nil, ErrCodeBadMention, "提及用户不合法"
		}
		if _, dup := seen[uid]; dup {
			continue
		}
		seen[uid] = struct{}{}
		var ok bool
		if convMembers != nil {
			ok = hasChannelMember(convMembers, uid)
		} else {
			ok, err = h.permCheckQuiet(ctx, uid, serverID, chID, store.ViewChannel)
			if err != nil {
				h.log.Printf("chat: 提及用户 %d 权限查询失败: %v", uid, err)
				return nil, ErrCodeInternal, "消息发送失败"
			}
		}
		if !ok {
			return nil, ErrCodeBadMention, "提及用户不在该频道"
		}
		out = append(out, uid)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out, "", ""
}

// replyPreviewFromStore 回复预览（#39）：软删目标只给跳转信息，正文清空。
func replyPreviewFromStore(m *store.Message) *ReplyPreview {
	content := m.Content
	if m.DeletedAt != nil {
		content = ""
	}
	return &ReplyPreview{
		ID:       strconv.FormatInt(m.ID, 10),
		UserID:   strconv.FormatInt(m.UserID, 10),
		Nickname: m.SenderNickname,
		Content:  content,
		Deleted:  m.DeletedAt != nil,
	}
}

// formatMentionIDs int64 uid → wire 字符串数组（空列表返回 nil，omitempty 省略）。
func formatMentionIDs(ids []int64) []string {
	if len(ids) == 0 {
		return nil
	}
	out := make([]string, 0, len(ids))
	for _, id := range ids {
		out = append(out, formatUID(id))
	}
	return out
}

// chatMessageFromStore 把落库消息还原成 chat.message 载荷（幂等重放定向回显用）。
// 身份取当前连接：回读按 (channel_id, user_id, nonce) 限定，原消息必属该用户。
// 回复预览与提及同样从库回填——重放请求体不可信（客户端可能已改过字段）。
func (h *Hub) chatMessageFromStore(ctx context.Context, m *store.Message, c *client) (ChatMessageData, error) {
	out := ChatMessageData{
		ID:        strconv.FormatInt(m.ID, 10),
		ChannelID: strconv.FormatInt(m.ChannelID, 10),
		Content:   m.Content,
		TS:        m.CreatedAt.UnixMilli(),
		User:      UserBrief{ID: c.userID, Nickname: c.username},
		Nonce:     m.Nonce,
	}
	if m.ReplyTo != nil {
		out.ReplyTo = strconv.FormatInt(*m.ReplyTo, 10)
		target, err := h.chatStore.MessageByID(ctx, *m.ReplyTo)
		switch {
		case err == nil:
			out.ReplyPreview = replyPreviewFromStore(target)
		case !errors.Is(err, store.ErrNotFound):
			return out, err // 目标已不存在（历史脏数据）只缺预览，不阻断回显
		}
	}
	mentions, err := h.chatStore.MentionsOfMessages(ctx, []int64{m.ID})
	if err != nil {
		return out, err
	}
	out.Mentions = formatMentionIDs(mentions[m.ID])
	return out, nil
}
