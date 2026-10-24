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
// 参数 fail-closed：显式 limit 越界 400 bad_limit，显式游标 <= 0 400 bad_cursor。
func (s *Server) ListMessages(ctx context.Context, request ListMessagesRequestObject) (ListMessagesResponseObject, error) {
	if e := checkID(request.Id); e != nil {
		return nil, e
	}
	c := currentUser(ctx)
	ch, err := s.store.ChannelByID(ctx, request.Id)
	if err != nil {
		return nil, s.writeStoreErr(ctx, err)
	}
	// 会话频道（#38 B2）：成员即拥有 View，跳过服务器 authz；非成员按 404
	// 处理，不泄露会话是否存在。服务器频道仍走权限门（#35 B3）。
	if conversationScoped(ch) {
		if _, err := s.store.ChannelMember(ctx, ch.ID, c.UserID); err != nil {
			if errors.Is(err, store.ErrNotFound) {
				return nil, errf(http.StatusNotFound, "not_found", "资源不存在")
			}
			return nil, s.writeStoreErr(ctx, err)
		}
	} else if e := s.requireChannelPerm(ctx, ch, c.UserID, store.ViewChannel); e != nil {
		return nil, e
	}
	// 游标互斥：before（含旧别名 before_id）优先于 after。
	// 显式给出的游标必须 > 0：0 不是合法消息 ID，不能当作「无游标」静默降级成
	// 最新一页（契约 minimum:1，客户端传 0 说明它的游标状态已坏，应尽早暴露）。
	var beforeID, afterID int64
	if v := request.Params.Before; v != nil {
		if *v <= 0 {
			return nil, errf(http.StatusBadRequest, "bad_cursor", "游标须为正 ID")
		}
		beforeID = *v
	} else if v := request.Params.BeforeId; v != nil {
		if *v <= 0 {
			return nil, errf(http.StatusBadRequest, "bad_cursor", "游标须为正 ID")
		}
		beforeID = *v // 旧参数别名（#31 兼容窗口，一个版本后移除）
	} else if v := request.Params.After; v != nil {
		if *v <= 0 {
			return nil, errf(http.StatusBadRequest, "bad_cursor", "游标须为正 ID")
		}
		afterID = *v
	}
	// limit 显式越界即 400，不再静默回落 50：契约（minimum:1, maximum:50）与
	// 行为必须一致，否则调用方以为拿到了请求的条数。
	limit := 50
	if v := request.Params.Limit; v != nil {
		if *v < 1 || *v > 50 {
			return nil, errf(http.StatusBadRequest, "bad_limit", "limit 须为 1–50")
		}
		limit = *v
	}
	if afterID > 0 {
		msgs, err := s.store.MessagesAfter(ctx, ch.ID, afterID, limit)
		if err != nil {
			return nil, s.writeStoreErr(ctx, err)
		}
		views, e := s.messageViews(ctx, msgs)
		if e != nil {
			return nil, e
		}
		return ListMessages200JSONResponse(views), nil
	}
	msgs, err := s.store.MessagesBefore(ctx, ch.ID, beforeID, limit)
	if err != nil {
		return nil, s.writeStoreErr(ctx, err)
	}
	views, e := s.messageViews(ctx, msgs)
	if e != nil {
		return nil, e
	}
	return ListMessages200JSONResponse(views), nil
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
	// VIEW+SEND 双门（#35 B3），且在 nonce 幂等回读之前——无权者连回显都拿不到。
	// 会话频道（#38 B2）：成员同时拥有 View/Send，跳过服务器 authz；非成员 404。
	if conversationScoped(ch) {
		if _, err := s.store.ChannelMember(ctx, ch.ID, c.UserID); err != nil {
			if errors.Is(err, store.ErrNotFound) {
				return nil, errf(http.StatusNotFound, "not_found", "资源不存在")
			}
			return nil, s.writeStoreErr(ctx, err)
		}
	} else {
		if e := s.requireChannelPerm(ctx, ch, c.UserID, store.ViewChannel); e != nil {
			return nil, e
		}
		if e := s.requireChannelPerm(ctx, ch, c.UserID, store.SendMessages); e != nil {
			return nil, e
		}
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
	// #39：回复目标必须同频道（软删目标允许）；提及用户必须是该频道可见成员。
	// 校验先于幂等回读：无效请求不应因 nonce 命中而拿到 200。
	replyTo, e := s.validateReply(ctx, ch, request.Body.ReplyTo)
	if e != nil {
		return nil, e
	}
	var mentions []int64
	if request.Body.Mentions != nil {
		mentions, e = s.resolveMentions(ctx, ch, *request.Body.Mentions)
		if e != nil {
			return nil, e
		}
	}
	// 幂等命中直接回原消息，不消耗频控配额（弱网重试先于配额判断）。
	if nonce != "" {
		orig, err := s.store.MessageByNonce(ctx, ch.ID, c.UserID, nonce)
		if err == nil {
			return s.createdMessage(ctx, orig, http.StatusOK)
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
		ReplyTo:   replyTo,
		Mentions:  mentions,
	}
	if err := s.store.CreateMessage(ctx, msg); err != nil {
		// 撞 (channel_id, user_id, nonce) 唯一索引 = 幂等重放：回读原消息返回 200。
		if nonce != "" && errors.Is(err, store.ErrConflict) {
			orig, rerr := s.store.MessageByNonce(ctx, ch.ID, c.UserID, nonce)
			if rerr != nil {
				return nil, s.writeStoreErr(ctx, rerr)
			}
			return s.createdMessage(ctx, orig, http.StatusOK)
		}
		return nil, s.writeStoreErr(ctx, err)
	}
	msg.SenderNickname, msg.SenderTag = c.Nickname, c.Tag
	return s.createdMessage(ctx, msg, http.StatusCreated)
}

// createdMessage 组装发送/幂等回读的响应：幂等命中按 200，新建按 201。
// 两条路径都经 messageViewFor 回填库里的 mentions 与 reply_preview（不用请求入参，
// 重放时以原消息为准）。
func (s *Server) createdMessage(ctx context.Context, msg *store.Message, status int) (CreateMessageResponseObject, error) {
	v, e := s.messageViewFor(ctx, msg)
	if e != nil {
		return nil, e
	}
	if status == http.StatusOK {
		return CreateMessage200JSONResponse(v), nil
	}
	return CreateMessage201JSONResponse(v), nil
}

// validateReply 校验回复目标（#39）：存在且与消息同频道；目标已软删也允许
// （预览显示已删除）。不合法统一 400 bad_reply。
func (s *Server) validateReply(ctx context.Context, ch *store.Channel, replyTo *ID) (*int64, *apiErr) {
	if replyTo == nil {
		return nil, nil
	}
	id := int64(*replyTo)
	if id <= 0 {
		return nil, errf(http.StatusBadRequest, "bad_reply", "回复目标不合法")
	}
	target, err := s.store.MessageByID(ctx, id)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return nil, errf(http.StatusBadRequest, "bad_reply", "回复的消息不存在")
		}
		return nil, s.writeStoreErr(ctx, err)
	}
	if target.ChannelID != ch.ID {
		return nil, errf(http.StatusBadRequest, "bad_reply", "不能回复其它频道的消息")
	}
	return &id, nil
}

// resolveMentions 校验并归一 @提及（#39）：去重、元素须 > 0，每个用户必须是
// 该频道可见成员（服务器频道要求 ViewChannel；会话要求 channel_members）。
// 任一不合法 400 bad_mention；空列表表示无提及。
func (s *Server) resolveMentions(ctx context.Context, ch *store.Channel, raw []ID) ([]int64, *apiErr) {
	if len(raw) == 0 {
		return nil, nil
	}
	seen := make(map[int64]struct{}, len(raw))
	out := make([]int64, 0, len(raw))
	for _, id := range raw {
		uid := int64(id)
		if uid <= 0 {
			return nil, errf(http.StatusBadRequest, "bad_mention", "提及用户不合法")
		}
		if _, dup := seen[uid]; dup {
			continue
		}
		seen[uid] = struct{}{}
		ok, e := s.canMention(ctx, ch, uid)
		if e != nil {
			return nil, e
		}
		if !ok {
			return nil, errf(http.StatusBadRequest, "bad_mention", "提及用户不在该频道")
		}
		out = append(out, uid)
	}
	return out, nil
}

// canMention 目标用户是否可见该频道（与 ViewChannel 同口径；会话看成员关系）。
func (s *Server) canMention(ctx context.Context, ch *store.Channel, uid int64) (bool, *apiErr) {
	if conversationScoped(ch) {
		if _, err := s.store.ChannelMember(ctx, ch.ID, uid); err != nil {
			if errors.Is(err, store.ErrNotFound) {
				return false, nil
			}
			return false, s.writeStoreErr(ctx, err)
		}
		return true, nil
	}
	if s.authz == nil {
		return false, errf(http.StatusInternalServerError, "internal", "权限服务未就绪")
	}
	ok, err := s.authz.CanInServer(ctx, uid, ch.ServerID, ch.ID, store.ViewChannel)
	if err != nil {
		return false, s.writeStoreErr(ctx, err)
	}
	return ok, nil
}

// 编辑/删除频控：各 20 次/分钟/用户（#39）。
const (
	msgEditRateLimit   = 20
	msgDeleteRateLimit = 20
)

// visibleChannelFor 取频道并校验当前用户可见（#39）：会话频道要求成员，非成员
// 404（不泄露存在性）；服务器频道要求 ViewChannel（403）。口径与 ListMessages 一致。
func (s *Server) visibleChannelFor(ctx context.Context, channelID, uid int64) (*store.Channel, *apiErr) {
	ch, err := s.store.ChannelByID(ctx, channelID)
	if err != nil {
		return nil, s.writeStoreErr(ctx, err)
	}
	if conversationScoped(ch) {
		if _, err := s.store.ChannelMember(ctx, ch.ID, uid); err != nil {
			if errors.Is(err, store.ErrNotFound) {
				return nil, errf(http.StatusNotFound, "not_found", "资源不存在")
			}
			return nil, s.writeStoreErr(ctx, err)
		}
		return ch, nil
	}
	if e := s.requireChannelPerm(ctx, ch, uid, store.ViewChannel); e != nil {
		return nil, e
	}
	return ch, nil
}

// messageInChannel 读取路径频道内的消息；不存在或不属于该频道统一 404
// （跨频道消息 ID 不泄露存在性）。
func (s *Server) messageInChannel(ctx context.Context, ch *store.Channel, messageID int64) (*store.Message, *apiErr) {
	msg, err := s.store.MessageByID(ctx, messageID)
	if err != nil {
		return nil, s.writeStoreErr(ctx, err)
	}
	if msg.ChannelID != ch.ID {
		return nil, errf(http.StatusNotFound, "not_found", "资源不存在")
	}
	return msg, nil
}

// errMessageDeleted 已删除消息统一 404 message_deleted（编辑与重复删除同口径）。
func errMessageDeleted() *apiErr {
	return errf(http.StatusNotFound, "message_deleted", "消息已删除")
}

// GetMessage GET /api/v1/channels/{id}/messages/{message_id}（#39）
// 软删消息仍返回（content 为空、deleted_at 有值），供回复跳转。
func (s *Server) GetMessage(ctx context.Context, request GetMessageRequestObject) (GetMessageResponseObject, error) {
	if e := checkID(request.Id); e != nil {
		return nil, e
	}
	if e := checkID(request.MessageId); e != nil {
		return nil, e
	}
	ch, e := s.visibleChannelFor(ctx, request.Id, currentUser(ctx).UserID)
	if e != nil {
		return nil, e
	}
	msg, e := s.messageInChannel(ctx, ch, request.MessageId)
	if e != nil {
		return nil, e
	}
	v, e := s.messageViewFor(ctx, msg)
	if e != nil {
		return nil, e
	}
	return GetMessage200JSONResponse(v), nil
}

// EditMessage PATCH /api/v1/channels/{id}/messages/{message_id}（#39，仅作者）
// mentions 缺省 = 保持原提及；给出 = 整体替换（空数组清除）。成功后置 edited_at。
func (s *Server) EditMessage(ctx context.Context, request EditMessageRequestObject) (EditMessageResponseObject, error) {
	if e := checkID(request.Id); e != nil {
		return nil, e
	}
	if e := checkID(request.MessageId); e != nil {
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
	msg, e := s.messageInChannel(ctx, ch, request.MessageId)
	if e != nil {
		return nil, e
	}
	if msg.DeletedAt != nil {
		return nil, errMessageDeleted()
	}
	if msg.UserID != c.UserID {
		return nil, errf(http.StatusForbidden, "forbidden", "只能编辑自己的消息")
	}
	content := request.Body.Content
	if n := len([]rune(content)); n == 0 || n > 2000 {
		return nil, errf(http.StatusBadRequest, "bad_content", "消息长度需为 1–2000 字符")
	}
	mentions, e := s.editMentions(ctx, ch, msg, request.Body.Mentions)
	if e != nil {
		return nil, e
	}
	if e := s.allowRate(ctx, "msg:edit:"+strconv.FormatInt(c.UserID, 10),
		msgEditRateLimit, time.Minute, "编辑过于频繁"); e != nil {
		return nil, e
	}
	// 广播目标先于写库算好：读取失败时不能已经改了库再回 500。
	var recipients []string
	if s.hub != nil {
		recipients, e = s.messageRecipients(ctx, ch)
		if e != nil {
			return nil, e
		}
	}
	now := time.Now()
	if err := s.store.UpdateMessageContent(ctx, msg.ID, content, mentions, now); err != nil {
		// 校验后并发被删除：与已删除同口径，不泄露内部时序。
		if errors.Is(err, store.ErrNotFound) {
			return nil, errMessageDeleted()
		}
		return nil, s.writeStoreErr(ctx, err)
	}
	updated, err := s.store.MessageByID(ctx, msg.ID)
	if err != nil {
		return nil, s.writeStoreErr(ctx, err)
	}
	// 编辑广播（#39）：DB 提交成功后推给可见成员；提及取库里的最终值（排序确定），
	// 不用请求入参。
	if s.hub != nil {
		latest, err := s.store.MentionsOfMessages(ctx, []int64{msg.ID})
		if err != nil {
			return nil, s.writeStoreErr(ctx, err)
		}
		editedAt := now.UnixMilli()
		if updated.EditedAt != nil {
			editedAt = updated.EditedAt.UnixMilli()
		}
		s.hub.BroadcastMessageEdited(recipients, strconv.FormatInt(ch.ID, 10),
			strconv.FormatInt(msg.ID, 10), updated.Content, editedAt, formatUIDs(latest[msg.ID]))
	}
	// 编辑可能改变提及，同一收件人列表重算并推送精确计数（#40）；失败只记日志。
	s.broadcastUnreadAfterMessageChange(ctx, ch.ID, recipients)
	v, e := s.messageViewFor(ctx, updated)
	if e != nil {
		return nil, e
	}
	return EditMessage200JSONResponse(v), nil
}

// editMentions 解析编辑后的提及：nil = 保持原提及（读库现值），给出 = 校验后
// 整体替换（空数组返回 nil，Store 清除全部提及）。
func (s *Server) editMentions(ctx context.Context, ch *store.Channel, msg *store.Message, raw *[]ID) ([]int64, *apiErr) {
	if raw == nil {
		cur, err := s.store.MentionsOfMessages(ctx, []int64{msg.ID})
		if err != nil {
			return nil, s.writeStoreErr(ctx, err)
		}
		return cur[msg.ID], nil
	}
	return s.resolveMentions(ctx, ch, *raw)
}

// DeleteMessage DELETE /api/v1/channels/{id}/messages/{message_id}（#39，软删）
// 作者可删自己的消息；服务器频道另有 ManageServer（管理员/服主）可删他人；
// 会话（私聊/群聊）仅作者。行保留供回复引用，重复删除 404 message_deleted。
func (s *Server) DeleteMessage(ctx context.Context, request DeleteMessageRequestObject) (DeleteMessageResponseObject, error) {
	if e := checkID(request.Id); e != nil {
		return nil, e
	}
	if e := checkID(request.MessageId); e != nil {
		return nil, e
	}
	c := currentUser(ctx)
	ch, e := s.visibleChannelFor(ctx, request.Id, c.UserID)
	if e != nil {
		return nil, e
	}
	msg, e := s.messageInChannel(ctx, ch, request.MessageId)
	if e != nil {
		return nil, e
	}
	if msg.DeletedAt != nil {
		return nil, errMessageDeleted()
	}
	if msg.UserID != c.UserID {
		if conversationScoped(ch) {
			return nil, errf(http.StatusForbidden, "forbidden", "只能删除自己的消息")
		}
		// 权限模型没有独立的 ManageMessages 位（低 15 位已固定，新增即改协议）；
		// 管理侧沿用最接近的 ManageServer（管理员/服主）。
		if e := s.requireChannelPerm(ctx, ch, c.UserID, store.ManageServer); e != nil {
			return nil, e
		}
	}
	if e := s.allowRate(ctx, "msg:del:"+strconv.FormatInt(c.UserID, 10),
		msgDeleteRateLimit, time.Minute, "删除过于频繁"); e != nil {
		return nil, e
	}
	// 目标成员/可见性在软删前后不变，先算好再写库，避免写后失败回 500。
	var recipients []string
	if s.hub != nil {
		recipients, e = s.messageRecipients(ctx, ch)
		if e != nil {
			return nil, e
		}
	}
	if err := s.store.SoftDeleteMessage(ctx, msg.ID, time.Now()); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return nil, errMessageDeleted()
		}
		return nil, s.writeStoreErr(ctx, err)
	}
	// 软删广播（#39）：客户端按 message_id 就地显示墓碑。
	if s.hub != nil {
		s.hub.BroadcastMessageDeleted(recipients, strconv.FormatInt(ch.ID, 10),
			strconv.FormatInt(msg.ID, 10))
	}
	// 删除减少未读，同一收件人列表重算并推送计数（#40）；失败只记日志。
	s.broadcastUnreadAfterMessageChange(ctx, ch.ID, recipients)
	return DeleteMessage204Response{}, nil
}

// messageRecipients 消息事件广播目标（#39）：会话频道 = 全部成员；服务器频道 =
// 对该频道有 ViewChannel 的成员（与 WS chat.message 广播同口径）。uid 为十进制字符串。
func (s *Server) messageRecipients(ctx context.Context, ch *store.Channel) ([]string, *apiErr) {
	if conversationScoped(ch) {
		members, err := s.store.ChannelMembers(ctx, ch.ID)
		if err != nil {
			return nil, s.writeStoreErr(ctx, err)
		}
		return formatUIDs(conversationMemberIDs(members)), nil
	}
	if s.authz == nil {
		return nil, errf(http.StatusInternalServerError, "internal", "权限服务未就绪")
	}
	members, err := s.store.MembersOfServer(ctx, ch.ServerID)
	if err != nil {
		return nil, s.writeStoreErr(ctx, err)
	}
	out := make([]string, 0, len(members))
	for _, m := range members {
		ok, err := s.authz.CanInServer(ctx, m.UserID, ch.ServerID, ch.ID, store.ViewChannel)
		if err != nil {
			return nil, s.writeStoreErr(ctx, err)
		}
		if ok {
			out = append(out, strconv.FormatInt(m.UserID, 10))
		}
	}
	return out, nil
}

// formatUIDs int64 uid → 十进制字符串（空列表返回 nil，wire 上 omitempty 省略）。
func formatUIDs(ids []int64) []string {
	if len(ids) == 0 {
		return nil
	}
	out := make([]string, 0, len(ids))
	for _, id := range ids {
		out = append(out, strconv.FormatInt(id, 10))
	}
	return out
}

// ListMentionCandidates GET /api/v1/channels/{id}/mention-candidates（#39）
// 服务器频道 = 该服对频道有 ViewChannel 的成员；会话频道 = 会话成员。
// 输出按 nickname、user_id 升序，不含 email；空结果为 []。
func (s *Server) ListMentionCandidates(ctx context.Context, request ListMentionCandidatesRequestObject) (ListMentionCandidatesResponseObject, error) {
	if e := checkID(request.Id); e != nil {
		return nil, e
	}
	ch, e := s.visibleChannelFor(ctx, request.Id, currentUser(ctx).UserID)
	if e != nil {
		return nil, e
	}
	if conversationScoped(ch) {
		members, err := s.store.ChannelMembers(ctx, ch.ID)
		if err != nil {
			return nil, s.writeStoreErr(ctx, err)
		}
		out := make([]MentionCandidate, 0, len(members))
		for _, m := range members {
			out = append(out, mentionCandidateView(m.UserID, m.Nickname, m.Tag))
		}
		sortMentionCandidates(out)
		return ListMentionCandidates200JSONResponse(out), nil
	}
	if s.authz == nil {
		return nil, errf(http.StatusInternalServerError, "internal", "权限服务未就绪")
	}
	members, err := s.store.MembersOfServer(ctx, ch.ServerID)
	if err != nil {
		return nil, s.writeStoreErr(ctx, err)
	}
	out := make([]MentionCandidate, 0, len(members))
	for _, m := range members {
		ok, err := s.authz.CanInServer(ctx, m.UserID, ch.ServerID, ch.ID, store.ViewChannel)
		if err != nil {
			return nil, s.writeStoreErr(ctx, err)
		}
		if !ok {
			continue
		}
		out = append(out, mentionCandidateView(m.UserID, m.Nickname, m.Tag))
	}
	sortMentionCandidates(out)
	return ListMentionCandidates200JSONResponse(out), nil
}
