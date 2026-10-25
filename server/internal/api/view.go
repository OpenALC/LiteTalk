package api

import (
	"context"
	"sort"
	"strconv"

	"github.com/OpenALC/LiteTalk/server/internal/store"
)

// 视图转换（#31）：store 结构体是存储层，不得直接序列化进 REST——ID 类型、
// 时间格式、隐私字段都由这里的 DTO 收口。openapi.yaml 为唯一事实来源，
// 生成的 User/ServerInfo/... 结构体即 wire 形状。

// PublicUser 由 openapi.yaml 生成（api.gen.go）：对外用户视图，不含 email。
// 昵称#tag 只有 1 万种组合可枚举，好友/成员等「返回他人信息」的响应必须用它；
// 只有本人 /me 与登录/注册等自响应可以带 email（User）。

// publicUser 从完整用户记录投影出公开视图；nil 安全（好友对端查询失败时透传 nil）。
func publicUser(u *store.User) *PublicUser {
	if u == nil {
		return nil
	}
	return &PublicUser{
		Id:            idOf(u.ID),
		Nickname:      u.Nickname,
		Tag:           u.Tag,
		EmailVerified: u.EmailVerified,
		IsGuest:       u.IsGuest,
		Status:        int32(u.Status),
		CreatedAt:     timeOf(u.CreatedAt),
		UpdatedAt:     timeOf(u.UpdatedAt),
	}
}

// userSelf 自响应视图（含 email）；密码哈希等字段不过 DTO。
func userSelf(u *store.User) User {
	return User{
		Id:            idOf(u.ID),
		Email:         u.Email,
		Nickname:      u.Nickname,
		Tag:           u.Tag,
		EmailVerified: u.EmailVerified,
		IsGuest:       u.IsGuest,
		Status:        int32(u.Status),
		CreatedAt:     timeOf(u.CreatedAt),
		UpdatedAt:     timeOf(u.UpdatedAt),
	}
}

func serverView(sv *store.Server) ServerInfo {
	return ServerInfo{
		Id:        idOf(sv.ID),
		Name:      sv.Name,
		OwnerId:   idOf(sv.OwnerID),
		IconName:  sv.IconName,
		IconColor: sv.IconColor,
		CreatedAt: timeOf(sv.CreatedAt),
	}
}

func channelView(c *store.Channel) ChannelInfo {
	// #37：has_password/user_limit 恒回（指针置位），password 哈希本身永不出；
	// parent_id 顶层为 nil（omitempty 缺省省略，语义等价 null）。
	// #46：音质两字段同样恒回，0 表示跟随/默认。
	hasPassword := c.PasswordHash != ""
	limit := c.UserLimit
	bitrate := c.AudioBitrateKbps
	frameMS := c.AudioFrameMS
	out := ChannelInfo{
		Id:               idOf(c.ID),
		ServerId:         idOf(c.ServerID),
		Name:             c.Name,
		Type:             ChannelInfoType(c.Type),
		Position:         c.Position,
		CreatedAt:        timeOf(c.CreatedAt),
		HasPassword:      &hasPassword,
		UserLimit:        &limit,
		AudioBitrateKbps: &bitrate,
		AudioFrameMs:     &frameMS,
	}
	if c.CategoryID != nil {
		out.CategoryId = idPtr(*c.CategoryID)
	}
	if c.ParentID != nil {
		out.ParentId = idPtr(*c.ParentID)
	}
	return out
}

func categoryView(c *store.Category) CategoryInfo {
	return CategoryInfo{
		Id:        idOf(c.ID),
		ServerId:  idOf(c.ServerID),
		Name:      c.Name,
		Position:  c.Position,
		CreatedAt: timeOf(c.CreatedAt),
	}
}

// memberView 成员行视图；online 由调用方从 presence:{uid} 读取后传入（#11）。
func memberView(m *store.ServerMember, online bool) MemberInfo {
	out := MemberInfo{
		ServerId: idOf(m.ServerID),
		UserId:   idOf(m.UserID),
		Role:     MemberInfoRole(m.Role),
		JoinedAt: timeOf(m.JoinedAt),
		Online:   online,
	}
	if m.Nickname != "" {
		n := m.Nickname
		out.Nickname = &n
	}
	if m.Tag != "" {
		t := m.Tag
		out.Tag = &t
	}
	return out
}

// messageView 消息 wire 视图（#39）：mentions 与回复目标由调用方批量装配后传入
// （nil 表示无提及 / 目标不存在）。#41：attachments 由调用方批量加载后传入；
// 软删消息即使库里仍有附件行也不下发。
func messageView(m *store.Message, mentions []int64, reply *store.Message, attachments []store.Attachment) Message {
	out := Message{
		Id:        idOf(m.ID),
		ChannelId: idOf(m.ChannelID),
		UserId:    idOf(m.UserID),
		Content:   m.Content,
		CreatedAt: timeOf(m.CreatedAt),
	}
	if m.Nonce != "" {
		n := m.Nonce
		out.Nonce = &n
	}
	if m.SenderNickname != "" {
		n := m.SenderNickname
		out.SenderNickname = &n
	}
	if m.SenderTag != "" {
		t := m.SenderTag
		out.SenderTag = &t
	}
	if m.EditedAt != nil {
		t := timeOf(*m.EditedAt)
		out.EditedAt = &t
	}
	if m.DeletedAt != nil {
		t := timeOf(*m.DeletedAt)
		out.DeletedAt = &t
	}
	if m.ReplyTo != nil {
		out.ReplyTo = idPtr(*m.ReplyTo)
	}
	if len(mentions) > 0 {
		ids := make([]ID, 0, len(mentions))
		for _, uid := range mentions {
			ids = append(ids, idOf(uid))
		}
		out.Mentions = &ids
	}
	if reply != nil {
		out.ReplyPreview = replyPreviewView(reply)
	}
	if m.DeletedAt == nil && len(attachments) > 0 {
		list := make([]Attachment, 0, len(attachments))
		for i := range attachments {
			list = append(list, attachmentView(&attachments[i]))
		}
		out.Attachments = &list
	}
	return out
}

// replyPreviewView 回复预览（#39）：目标已软删时 content 为空、deleted=true，
// 只暴露跳转与展示所需字段；昵称取目标消息发送时的快照。
func replyPreviewView(m *store.Message) *ReplyPreview {
	content := m.Content
	if m.DeletedAt != nil {
		content = ""
	}
	return &ReplyPreview{
		Id:       idOf(m.ID),
		UserId:   idOf(m.UserID),
		Nickname: m.SenderNickname,
		Content:  content,
		Deleted:  m.DeletedAt != nil,
	}
}

// messageExtras 一批消息的 #39/#41 附加字段：提及用户、回复目标与附件。
type messageExtras struct {
	mentions    map[int64][]int64
	replies     map[int64]*store.Message
	attachments map[int64][]store.Attachment
}

// loadMessageExtras 批量装配提及、回复目标与附件：一次 MentionsOfMessages +
// 一次 MessagesByIDs + 一次 AttachmentsByMessageIDs，列表不做 N+1。回复目标
// 不存在（历史脏数据）时该条不出 reply_preview，不阻断其它消息。
func (s *Server) loadMessageExtras(ctx context.Context, msgs []store.Message) (*messageExtras, *apiErr) {
	ids := make([]int64, 0, len(msgs))
	replyIDs := make([]int64, 0, len(msgs))
	seen := make(map[int64]struct{}, len(msgs))
	for i := range msgs {
		ids = append(ids, msgs[i].ID)
		if msgs[i].ReplyTo == nil {
			continue
		}
		rid := *msgs[i].ReplyTo
		if _, ok := seen[rid]; ok {
			continue
		}
		seen[rid] = struct{}{}
		replyIDs = append(replyIDs, rid)
	}
	mentions, err := s.store.MentionsOfMessages(ctx, ids)
	if err != nil {
		return nil, s.writeStoreErr(ctx, err)
	}
	replies, err := s.store.MessagesByIDs(ctx, replyIDs)
	if err != nil {
		return nil, s.writeStoreErr(ctx, err)
	}
	attachments, err := s.store.AttachmentsByMessageIDs(ctx, ids)
	if err != nil {
		return nil, s.writeStoreErr(ctx, err)
	}
	return &messageExtras{mentions: mentions, replies: replies, attachments: attachments}, nil
}

// view 单条转换（回复目标缺失时 reply_preview 缺省）。
func (x *messageExtras) view(m *store.Message) Message {
	var reply *store.Message
	if m.ReplyTo != nil {
		reply = x.replies[*m.ReplyTo]
	}
	// 软删消息不返回附件（附件行仍在库里，但随消息一起失效）。
	var attachments []store.Attachment
	if m.DeletedAt == nil {
		attachments = x.attachments[m.ID]
	}
	return messageView(m, x.mentions[m.ID], reply, attachments)
}

// messageViews 批量转换（分页列表）。
func (s *Server) messageViews(ctx context.Context, msgs []store.Message) ([]Message, *apiErr) {
	x, e := s.loadMessageExtras(ctx, msgs)
	if e != nil {
		return nil, e
	}
	out := make([]Message, 0, len(msgs))
	for i := range msgs {
		out = append(out, x.view(&msgs[i]))
	}
	return out, nil
}

// messageViewFor 单条消息视图（新建/幂等回读/编辑后与 GetMessage 共用）。
func (s *Server) messageViewFor(ctx context.Context, m *store.Message) (Message, *apiErr) {
	x, e := s.loadMessageExtras(ctx, []store.Message{*m})
	if e != nil {
		return Message{}, e
	}
	return x.view(m), nil
}

// mentionCandidateView 提及候选视图（#39）：只出昵称/tag，不含 email。
func mentionCandidateView(uid int64, nickname, tag string) MentionCandidate {
	return MentionCandidate{UserId: idOf(uid), Nickname: nickname, Tag: tag}
}

// sortMentionCandidates 按 nickname、user_id 升序（稳定且确定的 wire 顺序）。
func sortMentionCandidates(list []MentionCandidate) {
	sort.Slice(list, func(i, j int) bool {
		if list[i].Nickname != list[j].Nickname {
			return list[i].Nickname < list[j].Nickname
		}
		return list[i].UserId < list[j].UserId
	})
}

func inviteView(inv *store.Invite) Invite {
	out := Invite{
		Code:      inv.Code,
		ServerId:  idOf(inv.ServerID),
		CreatorId: idOf(inv.CreatorID),
		MaxUses:   inv.MaxUses,
		Uses:      inv.Uses,
	}
	if inv.ExpiresAt != nil {
		t := timeOf(*inv.ExpiresAt)
		out.ExpiresAt = &t
	}
	return out
}

func serverViews(list []store.Server) []ServerInfo {
	out := make([]ServerInfo, 0, len(list))
	for i := range list {
		out = append(out, serverView(&list[i]))
	}
	return out
}

func channelViews(list []store.Channel) []ChannelInfo {
	out := make([]ChannelInfo, 0, len(list))
	for i := range list {
		out = append(out, channelView(&list[i]))
	}
	return out
}

func categoryViews(list []store.Category) []CategoryInfo {
	out := make([]CategoryInfo, 0, len(list))
	for i := range list {
		out = append(out, categoryView(&list[i]))
	}
	return out
}

// serverMemberViews 成员列表视图（#11）：逐成员查 presence:{uid} 填 online。
// 成员数量通常不大，且该端点只在打开成员列表时拉取，先接受逐个 GET。
// role_ids（#35 B4）：一次 RoleAssignmentsOfServer + 一次 ListRoles 填齐，不做 N+1。
func (s *Server) serverMemberViews(ctx context.Context, list []store.ServerMember) []MemberInfo {
	roleIDs := s.memberRoleIDs(ctx, list)
	out := make([]MemberInfo, 0, len(list))
	for i := range list {
		v := memberView(&list[i], s.online(ctx, list[i].UserID))
		if ids, ok := roleIDs[list[i].UserID]; ok {
			v.RoleIds = &ids
		}
		out = append(out, v)
	}
	return out
}

// memberRoleIDs uid → 角色 ID（按 position、id 升序，与 RolesOfMember 同口径）。
// 查询失败返回空映射：role_ids 是可选字段，缺省不回、不阻塞成员列表。
func (s *Server) memberRoleIDs(ctx context.Context, list []store.ServerMember) map[int64][]ID {
	if len(list) == 0 {
		return nil
	}
	serverID := list[0].ServerID
	as, err := s.store.RoleAssignmentsOfServer(ctx, serverID)
	if err != nil {
		s.log.Printf("[members] 取角色分配失败 server=%d: %v", serverID, err)
		return nil
	}
	roles, err := s.store.ListRoles(ctx, serverID)
	if err != nil {
		s.log.Printf("[members] 取角色列表失败 server=%d: %v", serverID, err)
		return nil
	}
	pos := make(map[int64]int, len(roles))
	for _, r := range roles {
		pos[r.ID] = r.Position
	}
	out := map[int64][]ID{}
	for _, a := range as {
		out[a.UserID] = append(out[a.UserID], ID(a.RoleID))
	}
	for uid, ids := range out {
		sort.Slice(ids, func(i, j int) bool {
			if pos[int64(ids[i])] != pos[int64(ids[j])] {
				return pos[int64(ids[i])] < pos[int64(ids[j])]
			}
			return ids[i] < ids[j]
		})
		out[uid] = ids
	}
	return out
}

// userView 组装对外用户视图并填在线态（#11）。好友数量小，逐个查 presence:{uid} 即可；
// 与 signal.presenceKey 的键格式手工同步（两包互不依赖）。
func (s *Server) userView(ctx context.Context, u *store.User) *PublicUser {
	pub := publicUser(u)
	if pub == nil {
		return nil
	}
	pub.Online = s.online(ctx, u.ID)
	return pub
}

// friendUserView 好友条目对端视图：只有已接受关系才填在线态（#11 P2 隐私）。
// pending/blocked 关系不返回在线与否，避免单方面发出请求即可探测对方在线。
func (s *Server) friendUserView(ctx context.Context, u *store.User, status store.FriendshipStatus) *PublicUser {
	if status != store.FriendAccepted {
		return publicUser(u)
	}
	return s.userView(ctx, u)
}

// fillFriendChannel 给好友条目填当前语音房快照（#92）。隐私口径：
//   - 门槛只看查看者的 ViewChannel，不要求 ConnectVoice；
//   - 无权限、authz 未注入、查询故障、VoicePresence 缺失/失败一律省略字段
//     （channel_id/server_id 键不得出现在原始 JSON），不 500、不记为有权；
//   - 只出频道/服务器 ID，永不携带频道名；dev 内置房间（serverID=0）省略。
//
// viewer 是好友列表的持有者（查看者），subject 是对端（被查看的好友）。
func (s *Server) fillFriendChannel(ctx context.Context, viewer, subject int64, entry *FriendEntry) {
	if s.voice == nil {
		return
	}
	chStr, sidStr, ok := s.voice.VoiceChannelOf(strconv.FormatInt(subject, 10))
	if !ok || sidStr == "" || sidStr == "0" {
		return // 不在语音房 / dev 内置房间（无 store 频道实体）
	}
	chID, errCh := strconv.ParseInt(chStr, 10, 64)
	sid, errSid := strconv.ParseInt(sidStr, 10, 64)
	if errCh != nil || errSid != nil {
		s.log.Printf("api: 好友频道快照省略（ID 非法 user=%d ch=%q srv=%q）", subject, chStr, sidStr)
		return
	}
	if s.authz == nil {
		s.log.Printf("api: 好友频道快照省略（权限服务未就绪） viewer=%d ch=%d", viewer, chID)
		return
	}
	okView, err := s.authz.CanInServer(ctx, viewer, sid, chID, store.ViewChannel)
	if err != nil {
		// 故障不当有权（出字段）也不当无权（伪造 403/假事实）：省略并记日志。
		s.log.Printf("api: 好友频道权限查询失败（viewer=%d ch=%d）: %v", viewer, chID, err)
		return
	}
	if !okView {
		return
	}
	entry.ChannelId, entry.ServerId = idPtr(chID), idPtr(sid)
}

// online presence:{uid} 存在 = 在线；无 KV 依赖时恒离线。
func (s *Server) online(ctx context.Context, uid int64) bool {
	if s.presence == nil {
		return false
	}
	_, err := s.presence.Get(ctx, "presence:"+strconv.FormatInt(uid, 10))
	return err == nil
}
