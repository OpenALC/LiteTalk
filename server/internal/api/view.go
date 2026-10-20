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
	out := ChannelInfo{
		Id:        idOf(c.ID),
		ServerId:  idOf(c.ServerID),
		Name:      c.Name,
		Type:      ChannelInfoType(c.Type),
		Position:  c.Position,
		CreatedAt: timeOf(c.CreatedAt),
	}
	if c.CategoryID != nil {
		out.CategoryId = idPtr(*c.CategoryID)
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

func messageView(m *store.Message) Message {
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
	return out
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

// messageViews 批量转换（分页列表）。
func messageViews(msgs []store.Message) []Message {
	out := make([]Message, 0, len(msgs))
	for i := range msgs {
		out = append(out, messageView(&msgs[i]))
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

// online presence:{uid} 存在 = 在线；无 KV 依赖时恒离线。
func (s *Server) online(ctx context.Context, uid int64) bool {
	if s.presence == nil {
		return false
	}
	_, err := s.presence.Get(ctx, "presence:"+strconv.FormatInt(uid, 10))
	return err == nil
}
