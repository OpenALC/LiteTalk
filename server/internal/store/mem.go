package store

import (
	"context"
	"sort"
	"sync"
	"time"
)

// Mem 是 Store + KV 的内存实现。供 API 层测试与无数据库环境使用；
// 语义与 MySQL 实现一致（唯一约束、外键式删除级联在 API 层处理）。
// KV 部分经嵌入 MemKV 提供（拆分后 NewMemKV 可单独作为进程内 KV）。
type Mem struct {
	mu sync.Mutex

	users       map[int64]*User
	friendships map[int64]*Friendship
	servers     map[int64]*Server
	members     map[int64]map[int64]*ServerMember // serverID → userID → member
	channels    map[int64]*Channel
	categories  map[int64]*Category
	messages    map[int64]*Message
	invites     map[string]*Invite
	sessions    map[int64]map[string]*UserSession // userID → deviceType → session
	// channelMembers 会话成员（迁移 0012，#38）：channelID → userID → 成员。
	channelMembers map[int64]map[int64]*ChannelMember

	roles       map[int64]*Role                                     // roleID → role（迁移 0010）
	memberRoles map[int64]map[int64]map[int64]*MemberRoleAssignment // serverID → userID → roleID → 分配
	overwrites  map[overwriteKey]*ChannelOverwrite                  // (channel,targetType,targetID) → 覆盖
	auditLogs   map[int64]*AuditLog                                 // id → 审计行

	*MemKV // KV 部分（Get/Set/Del/Incr/IncrCap/Eval 由 MemKV 提供）
}

// overwriteKey 覆盖行主键（channel_id, target_type, target_id）。
type overwriteKey struct {
	channelID  int64
	targetType OverwriteTargetType
	targetID   int64
}

// NewMem 创建内存存储。
func NewMem() *Mem {
	return &Mem{
		users:          map[int64]*User{},
		friendships:    map[int64]*Friendship{},
		servers:        map[int64]*Server{},
		members:        map[int64]map[int64]*ServerMember{},
		channels:       map[int64]*Channel{},
		categories:     map[int64]*Category{},
		messages:       map[int64]*Message{},
		invites:        map[string]*Invite{},
		sessions:       map[int64]map[string]*UserSession{},
		channelMembers: map[int64]map[int64]*ChannelMember{},
		roles:          map[int64]*Role{},
		memberRoles:    map[int64]map[int64]map[int64]*MemberRoleAssignment{},
		overwrites:     map[overwriteKey]*ChannelOverwrite{},
		auditLogs:      map[int64]*AuditLog{},
		MemKV:          NewMemKV(),
	}
}

// ---- users ----

func (m *Mem) CreateUser(_ context.Context, u *User) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, x := range m.users {
		if x.Email == u.Email {
			return ErrConflict
		}
		if x.Nickname == u.Nickname && x.Tag == u.Tag {
			return ErrConflict
		}
	}
	now := time.Now()
	u.ID = newID(u.ID)
	u.CreatedAt, u.UpdatedAt = now, now
	cp := *u
	m.users[u.ID] = &cp
	return nil
}

func (m *Mem) UserByID(_ context.Context, id int64) (*User, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	u, ok := m.users[id]
	if !ok {
		return nil, ErrNotFound
	}
	cp := *u
	return &cp, nil
}

func (m *Mem) UserByEmail(_ context.Context, email string) (*User, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, u := range m.users {
		if u.Email == email {
			cp := *u
			return &cp, nil
		}
	}
	return nil, ErrNotFound
}

func (m *Mem) UserByNicknameTag(_ context.Context, nickname, tag string) (*User, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, u := range m.users {
		if u.Nickname == nickname && u.Tag == tag {
			cp := *u
			return &cp, nil
		}
	}
	return nil, ErrNotFound
}

func (m *Mem) UpdateUser(_ context.Context, u *User) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	old, ok := m.users[u.ID]
	if !ok {
		return ErrNotFound
	}
	if u.Nickname != old.Nickname || u.Tag != old.Tag {
		for _, x := range m.users {
			if x.ID != u.ID && x.Nickname == u.Nickname && x.Tag == u.Tag {
				return ErrConflict
			}
		}
	}
	u.UpdatedAt = time.Now()
	cp := *u
	m.users[u.ID] = &cp
	return nil
}

// HasUsers 库内是否已有用户（首启探测；UserByID(1) 随 Snowflake 失效）。
func (m *Mem) HasUsers(_ context.Context) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.users) > 0, nil
}

// HasServers 库内是否已有服务器（#27：实例是否已被认领的持久判据）。
func (m *Mem) HasServers(_ context.Context) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.servers) > 0, nil
}

// ---- friendships ----

func (m *Mem) CreateFriendship(_ context.Context, f *Friendship) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, x := range m.friendships {
		if (x.UserID == f.UserID && x.FriendID == f.FriendID) ||
			(x.UserID == f.FriendID && x.FriendID == f.UserID) {
			return ErrConflict
		}
	}
	f.ID = newID(f.ID)
	f.CreatedAt = time.Now()
	cp := *f
	m.friendships[f.ID] = &cp
	return nil
}

func (m *Mem) FriendshipByID(_ context.Context, id int64) (*Friendship, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	f, ok := m.friendships[id]
	if !ok {
		return nil, ErrNotFound
	}
	cp := *f
	return &cp, nil
}

func (m *Mem) FriendshipsOfUser(_ context.Context, userID int64) ([]Friendship, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := []Friendship{}
	for _, f := range m.friendships {
		if f.UserID == userID || f.FriendID == userID {
			out = append(out, *f)
		}
	}
	return out, nil
}

func (m *Mem) FriendshipBetween(_ context.Context, a, b int64) (*Friendship, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, f := range m.friendships {
		if (f.UserID == a && f.FriendID == b) || (f.UserID == b && f.FriendID == a) {
			cp := *f
			return &cp, nil
		}
	}
	return nil, ErrNotFound
}

func (m *Mem) UpdateFriendship(_ context.Context, f *Friendship) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.friendships[f.ID]; !ok {
		return ErrNotFound
	}
	cp := *f
	m.friendships[f.ID] = &cp
	return nil
}

func (m *Mem) DeleteFriendship(_ context.Context, id int64) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.friendships[id]; !ok {
		return ErrNotFound
	}
	delete(m.friendships, id)
	return nil
}

// ---- servers ----

func (m *Mem) CreateServer(_ context.Context, s *Server) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	s.ID = newID(s.ID)
	s.CreatedAt = time.Now()
	cp := *s
	m.servers[s.ID] = &cp
	// 三条 builtin 角色同锁写入（#35）：缺任一条后续 AddMember/SetMemberRoles 会断不变量。
	m.insertBuiltinRolesLocked(s.ID)
	return nil
}

// insertBuiltinRolesLocked 按模板写入 member/admin/owner 三条 builtin（调用方持 m.mu）。
func (m *Mem) insertBuiltinRolesLocked(serverID int64) {
	now := time.Now()
	for _, tpl := range BuiltinRoleTemplates {
		r := &Role{
			ID: newID(0), ServerID: serverID, Name: tpl.Name, Color: DefaultRoleColor,
			Position: tpl.Position, Permissions: tpl.Permissions, BuiltinKey: tpl.BuiltinKey,
			CreatedAt: now, UpdatedAt: now,
		}
		m.roles[r.ID] = r
	}
}

// builtinRoleIDLocked 取该服指定 builtin_key 的角色 id；缺失返回 0（不应发生，
// CreateServer/迁移回填保证三条齐全，缺失属数据损坏）。
func (m *Mem) builtinRoleIDLocked(serverID int64, key string) int64 {
	for _, r := range m.roles {
		if r.ServerID == serverID && r.BuiltinKey == key {
			return r.ID
		}
	}
	return 0
}

func (m *Mem) ServerByID(_ context.Context, id int64) (*Server, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	s, ok := m.servers[id]
	if !ok {
		return nil, ErrNotFound
	}
	cp := *s
	return &cp, nil
}

func (m *Mem) ServersOfUser(_ context.Context, userID int64) ([]Server, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := []Server{}
	for sid, members := range m.members {
		if _, ok := members[userID]; ok {
			if s, ok := m.servers[sid]; ok {
				out = append(out, *s)
			}
		}
	}
	return out, nil
}

func (m *Mem) UpdateServer(_ context.Context, s *Server) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.servers[s.ID]; !ok {
		return ErrNotFound
	}
	cp := *s
	m.servers[s.ID] = &cp
	return nil
}

func (m *Mem) DeleteServer(_ context.Context, id int64) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.servers[id]; !ok {
		return ErrNotFound
	}
	delete(m.servers, id)
	delete(m.members, id)
	delete(m.memberRoles, id)
	for rid, r := range m.roles {
		if r.ServerID == id {
			delete(m.roles, rid)
		}
	}
	for k, o := range m.overwrites {
		if c, ok := m.channels[o.ChannelID]; ok && c.ServerID == id {
			delete(m.overwrites, k)
		}
	}
	for aid, a := range m.auditLogs {
		if a.ServerID == id {
			delete(m.auditLogs, aid)
		}
	}
	for catID, cat := range m.categories {
		if cat.ServerID == id {
			delete(m.categories, catID)
		}
	}
	for cid, c := range m.channels {
		if c.ServerID == id {
			delete(m.channels, cid)
			for mid, msg := range m.messages {
				if msg.ChannelID == cid {
					delete(m.messages, mid)
				}
			}
		}
	}
	for code, inv := range m.invites {
		if inv.ServerID == id {
			delete(m.invites, code)
		}
	}
	return nil
}

func (m *Mem) AddMember(_ context.Context, mem *ServerMember) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.servers[mem.ServerID]; !ok {
		return ErrNotFound
	}
	if m.members[mem.ServerID] == nil {
		m.members[mem.ServerID] = map[int64]*ServerMember{}
	}
	if _, ok := m.members[mem.ServerID][mem.UserID]; ok {
		return ErrConflict
	}
	// 先解析默认角色再落笔：builtin 缺失时不留孤儿成员行（与 SQL 事务同语义）。
	keys := defaultRoleKeys(mem.Role)
	ids := make([]int64, 0, len(keys))
	for _, key := range keys {
		roleID := m.builtinRoleIDLocked(mem.ServerID, key)
		if roleID == 0 {
			return ErrNotFound
		}
		ids = append(ids, roleID)
	}
	mem.JoinedAt = time.Now()
	cp := *mem
	m.members[mem.ServerID][mem.UserID] = &cp
	now := mem.JoinedAt
	for _, roleID := range ids {
		m.putAssignmentLocked(mem.ServerID, mem.UserID, roleID, 0, now)
	}
	return nil
}

// putAssignmentLocked 幂等写入一条分配（调用方持 m.mu）。
func (m *Mem) putAssignmentLocked(serverID, userID, roleID, assignedBy int64, at time.Time) {
	if m.memberRoles[serverID] == nil {
		m.memberRoles[serverID] = map[int64]map[int64]*MemberRoleAssignment{}
	}
	if m.memberRoles[serverID][userID] == nil {
		m.memberRoles[serverID][userID] = map[int64]*MemberRoleAssignment{}
	}
	if _, ok := m.memberRoles[serverID][userID][roleID]; ok {
		return
	}
	m.memberRoles[serverID][userID][roleID] = &MemberRoleAssignment{
		ServerID: serverID, UserID: userID, RoleID: roleID, AssignedAt: at, AssignedBy: assignedBy,
	}
}

func (m *Mem) RemoveMember(_ context.Context, serverID, userID int64) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.members[serverID][userID]; !ok {
		return ErrNotFound
	}
	delete(m.members[serverID], userID)
	// 成员的角色分配与该服内以该成员为目标的频道覆盖一起清（#35）。
	delete(m.memberRoles[serverID], userID)
	for k, o := range m.overwrites {
		if o.TargetType == OverwriteTargetMember && o.TargetID == userID {
			if c, ok := m.channels[o.ChannelID]; ok && c.ServerID == serverID {
				delete(m.overwrites, k)
			}
		}
	}
	return nil
}

func (m *Mem) Member(_ context.Context, serverID, userID int64) (*ServerMember, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	mem, ok := m.members[serverID][userID]
	if !ok {
		return nil, ErrNotFound
	}
	cp := *mem
	if u, ok := m.users[userID]; ok {
		cp.Nickname, cp.Tag = u.Nickname, u.Tag
	}
	return &cp, nil
}

func (m *Mem) MembersOfServer(_ context.Context, serverID int64) ([]ServerMember, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := []ServerMember{}
	for _, mem := range m.members[serverID] {
		cp := *mem
		if u, ok := m.users[mem.UserID]; ok {
			cp.Nickname, cp.Tag = u.Nickname, u.Tag
		}
		out = append(out, cp)
	}
	return out, nil
}

// ---- channels ----

func (m *Mem) CreateChannel(_ context.Context, c *Channel) error {
	if err := normalizeServerChannel(c); err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.servers[c.ServerID]; !ok {
		return ErrNotFound
	}
	c.ID = newID(c.ID)
	c.CreatedAt = time.Now()
	cp := *c
	m.channels[c.ID] = &cp
	return nil
}

func (m *Mem) ChannelByID(_ context.Context, id int64) (*Channel, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	c, ok := m.channels[id]
	if !ok {
		return nil, ErrNotFound
	}
	cp := *c
	return &cp, nil
}

func (m *Mem) ChannelsOfServer(_ context.Context, serverID int64) ([]Channel, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := []Channel{}
	for _, c := range m.channels {
		if c.ServerID == serverID && c.Scope == ChannelScopeServer {
			out = append(out, *c)
		}
	}
	// 与 MySQL/SQLite 的 ORDER BY position, id 对齐（#37）：map 遍历序随机，
	// 不排序会让频道列表/父子收集的顺序三实现不一致。
	sort.Slice(out, func(i, j int) bool {
		if out[i].Position != out[j].Position {
			return out[i].Position < out[j].Position
		}
		return out[i].ID < out[j].ID
	})
	return out, nil
}

func (m *Mem) UpdateChannel(_ context.Context, c *Channel) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.channels[c.ID]; !ok {
		return ErrNotFound
	}
	scope := normalizeChannelScope(c.Scope)
	if scope != ChannelScopeServer && !isConversationScope(scope) {
		return ErrForbidden
	}
	cp := *c
	cp.Scope = scope
	if scope == ChannelScopeServer {
		cp.DMKey = ""
	}
	m.channels[c.ID] = &cp
	return nil
}

func (m *Mem) DeleteChannel(_ context.Context, id int64) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.channels[id]; !ok {
		return ErrNotFound
	}
	// 子频道行级联（#37）：消息/覆盖/会话成员一并清，与 SQL 实现同语义。
	for cid, c := range m.channels {
		if c.ParentID != nil && *c.ParentID == id {
			delete(m.channels, cid)
			m.deleteOverwritesOfChannelLocked(cid)
			delete(m.channelMembers, cid)
			for mid, msg := range m.messages {
				if msg.ChannelID == cid {
					delete(m.messages, mid)
				}
			}
		}
	}
	delete(m.channels, id)
	m.deleteOverwritesOfChannelLocked(id)
	delete(m.channelMembers, id)
	for mid, msg := range m.messages {
		if msg.ChannelID == id {
			delete(m.messages, mid)
		}
	}
	return nil
}

// ---- conversations（迁移 0012，#38 B1） ----

func (m *Mem) CreateConversation(_ context.Context, c *Channel, memberIDs []int64) error {
	ids, err := normalizeConversation(c, memberIDs)
	if err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if c.Scope == ChannelScopeDirect {
		for _, x := range m.channels {
			if x.Scope == ChannelScopeDirect && x.DMKey == c.DMKey {
				return ErrConflict
			}
		}
	}
	c.ID = newID(c.ID)
	now := time.Now()
	c.CreatedAt = now
	cp := *c
	m.channels[c.ID] = &cp
	members := make(map[int64]*ChannelMember, len(ids))
	for i, uid := range ids {
		members[uid] = &ChannelMember{
			ChannelID: c.ID,
			UserID:    uid,
			Role:      conversationMemberRole(c.Scope, i),
			JoinedAt:  now,
		}
	}
	m.channelMembers[c.ID] = members
	return nil
}

func (m *Mem) FindDirectChannel(_ context.Context, a, b int64) (*Channel, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	key := directKey(a, b)
	for _, c := range m.channels {
		if c.Scope == ChannelScopeDirect && c.DMKey == key {
			cp := *c
			return &cp, nil
		}
	}
	return nil, ErrNotFound
}

func (m *Mem) ListConversations(_ context.Context, userID int64) ([]Channel, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := []Channel{}
	for id, c := range m.channels {
		if !isConversationScope(c.Scope) {
			continue
		}
		if _, ok := m.channelMembers[id][userID]; !ok {
			continue
		}
		out = append(out, *c)
	}
	// 与 SQL 的 ORDER BY created_at DESC, id DESC 对齐：map 遍历序随机，必须定序。
	sort.Slice(out, func(i, j int) bool {
		if !out[i].CreatedAt.Equal(out[j].CreatedAt) {
			return out[i].CreatedAt.After(out[j].CreatedAt)
		}
		return out[i].ID > out[j].ID
	})
	return out, nil
}

func (m *Mem) ChannelMembers(_ context.Context, channelID int64) ([]ChannelMember, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := []ChannelMember{}
	for _, cm := range m.channelMembers[channelID] {
		out = append(out, m.channelMemberCopyLocked(cm))
	}
	sortChannelMembers(out)
	return out, nil
}

// ChannelMembersOfChannels 批量成员（#38 B2）：按 channel_id 分组，组内排序与
// ChannelMembers 一致；没有任何成员的频道不出现在结果里。
func (m *Mem) ChannelMembersOfChannels(_ context.Context, channelIDs []int64) (map[int64][]ChannelMember, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make(map[int64][]ChannelMember, len(channelIDs))
	for _, id := range channelIDs {
		for _, cm := range m.channelMembers[id] {
			out[id] = append(out[id], m.channelMemberCopyLocked(cm))
		}
	}
	for id := range out {
		sortChannelMembers(out[id])
	}
	return out, nil
}

// channelMemberCopyLocked 复制成员行并内联用户昵称/tag（调用方持 m.mu）。
func (m *Mem) channelMemberCopyLocked(cm *ChannelMember) ChannelMember {
	cp := *cm
	if u, ok := m.users[cm.UserID]; ok {
		cp.Nickname, cp.Tag = u.Nickname, u.Tag
	}
	return cp
}

// sortChannelMembers 与 SQL 的 ORDER BY joined_at, user_id 对齐。
func sortChannelMembers(list []ChannelMember) {
	sort.Slice(list, func(i, j int) bool {
		if !list[i].JoinedAt.Equal(list[j].JoinedAt) {
			return list[i].JoinedAt.Before(list[j].JoinedAt)
		}
		return list[i].UserID < list[j].UserID
	})
}

func (m *Mem) ChannelMember(_ context.Context, channelID, userID int64) (*ChannelMember, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	cm, ok := m.channelMembers[channelID][userID]
	if !ok {
		return nil, ErrNotFound
	}
	cp := m.channelMemberCopyLocked(cm)
	return &cp, nil
}

func (m *Mem) SetChannelMemberRole(_ context.Context, channelID, userID int64, role ChannelMemberRole) error {
	role, err := normalizeChannelMemberRole(role)
	if err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	c, ok := m.channels[channelID]
	if !ok {
		return ErrNotFound
	}
	if c.Scope != ChannelScopeGroup {
		return ErrForbidden
	}
	cm, ok := m.channelMembers[channelID][userID]
	if !ok {
		return ErrNotFound
	}
	cm.Role = role
	return nil
}

func (m *Mem) AddChannelMember(_ context.Context, channelID, userID int64, role ChannelMemberRole) error {
	role, err := normalizeChannelMemberRole(role)
	if err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	c, ok := m.channels[channelID]
	if !ok {
		return ErrNotFound
	}
	if c.Scope != ChannelScopeGroup {
		return ErrForbidden
	}
	if m.channelMembers[channelID] == nil {
		m.channelMembers[channelID] = map[int64]*ChannelMember{}
	}
	if _, exists := m.channelMembers[channelID][userID]; exists {
		return ErrConflict
	}
	m.channelMembers[channelID][userID] = &ChannelMember{
		ChannelID: channelID, UserID: userID, Role: role, JoinedAt: time.Now(),
	}
	return nil
}

func (m *Mem) RemoveChannelMember(_ context.Context, channelID, userID int64) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	c, ok := m.channels[channelID]
	if !ok {
		return ErrNotFound
	}
	if c.Scope != ChannelScopeGroup {
		return ErrForbidden
	}
	if _, ok := m.channelMembers[channelID][userID]; !ok {
		return ErrNotFound
	}
	delete(m.channelMembers[channelID], userID)
	return nil
}

// deleteOverwritesOfChannelLocked 删指定频道的全部覆盖（调用方持 m.mu）。
func (m *Mem) deleteOverwritesOfChannelLocked(channelID int64) {
	for k, o := range m.overwrites {
		if o.ChannelID == channelID {
			delete(m.overwrites, k)
		}
	}
}

// ---- categories（v0.1.2 迁移 0003；内存实现同步语义） ----

func (m *Mem) CreateCategory(_ context.Context, c *Category) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.servers[c.ServerID]; !ok {
		return ErrNotFound
	}
	c.ID = newID(c.ID)
	c.CreatedAt = time.Now()
	cp := *c
	m.categories[c.ID] = &cp
	return nil
}

func (m *Mem) CategoryByID(_ context.Context, id int64) (*Category, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	c, ok := m.categories[id]
	if !ok {
		return nil, ErrNotFound
	}
	cp := *c
	return &cp, nil
}

func (m *Mem) CategoriesOfServer(_ context.Context, serverID int64) ([]Category, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := []Category{}
	for _, c := range m.categories {
		if c.ServerID == serverID {
			out = append(out, *c)
		}
	}
	// 与 MySQL 版 ORDER BY position, id 对齐——map 遍历序随机，不排序会导致
	// GET /servers/{id}/channels 的分类顺序不确定（review S5）。
	sort.Slice(out, func(i, j int) bool {
		if out[i].Position != out[j].Position {
			return out[i].Position < out[j].Position
		}
		return out[i].ID < out[j].ID
	})
	return out, nil
}

// DeleteCategoryWithChannels 删除分类及其下全部频道（含各频道消息与覆盖），
// 与 MySQL 实现语义一致（单操作原子性由 m.mu 保证）。子频道同分类是 API 不变量，
// 按 parent_id 再兜一圈防数据漂移漏行（#37）。
func (m *Mem) DeleteCategoryWithChannels(_ context.Context, id int64) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	c, ok := m.categories[id]
	if !ok {
		return ErrNotFound
	}
	inCat := map[int64]bool{}
	for cid, ch := range m.channels {
		if ch.ServerID == c.ServerID && ch.CategoryID != nil && *ch.CategoryID == c.ID {
			inCat[cid] = true
		}
	}
	for cid, ch := range m.channels {
		if ch.ParentID != nil && inCat[*ch.ParentID] {
			inCat[cid] = true
		}
	}
	for cid := range inCat {
		delete(m.channels, cid)
		m.deleteOverwritesOfChannelLocked(cid)
		for mid, msg := range m.messages {
			if msg.ChannelID == cid {
				delete(m.messages, mid)
			}
		}
	}
	delete(m.categories, id)
	return nil
}

func (m *Mem) UpdateCategory(_ context.Context, c *Category) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	old, ok := m.categories[c.ID]
	if !ok {
		return ErrNotFound
	}
	cp := *c
	// created_at 不可变；不允许挪服务器（跨服务器移动不在范围）。
	cp.CreatedAt = old.CreatedAt
	cp.ServerID = old.ServerID
	m.categories[c.ID] = &cp
	return nil
}

// ReorderCategories 把 id 移到 position（越界夹到首/尾）并重排同服务器分类为 0..n-1，
// 与 MySQL 事务版语义一致（原子性由 m.mu 保证）。
func (m *Mem) ReorderCategories(_ context.Context, serverID, id int64, position int) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	target, ok := m.categories[id]
	if !ok || target.ServerID != serverID {
		return ErrNotFound
	}
	list := []*Category{}
	for _, c := range m.categories {
		if c.ServerID == serverID {
			list = append(list, c)
		}
	}
	sort.Slice(list, func(i, j int) bool {
		if list[i].Position != list[j].Position {
			return list[i].Position < list[j].Position
		}
		return list[i].ID < list[j].ID
	})
	rest := make([]*Category, 0, len(list))
	for _, c := range list {
		if c.ID != id {
			rest = append(rest, c)
		}
	}
	if position < 0 {
		position = 0
	}
	if position > len(rest) {
		position = len(rest)
	}
	out := make([]*Category, 0, len(list))
	out = append(out, rest[:position]...)
	out = append(out, target)
	out = append(out, rest[position:]...)
	for i, c := range out {
		c.Position = i
	}
	return nil
}

// ---- messages ----

func (m *Mem) CreateMessage(_ context.Context, msg *Message) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.channels[msg.ChannelID]; !ok {
		return ErrNotFound
	}
	// nonce 幂等：同频道同用户同 nonce 只留一条（唯一索引语义，空 nonce 不去重）。
	if msg.Nonce != "" {
		for _, x := range m.messages {
			if x.ChannelID == msg.ChannelID && x.UserID == msg.UserID && x.Nonce == msg.Nonce {
				return ErrConflict
			}
		}
	}
	msg.ID = newID(msg.ID)
	msg.CreatedAt = time.Now()
	cp := *msg
	m.messages[msg.ID] = &cp
	return nil
}

// messageMatch 收集频道消息并按 id 升序排序（分页共用）。
func (m *Mem) messageMatch(channelID int64) []Message {
	all := []Message{}
	for _, msg := range m.messages {
		if msg.ChannelID == channelID {
			all = append(all, *msg)
		}
	}
	// 插入排序（数据量小）。
	for i := 1; i < len(all); i++ {
		for j := i; j > 0 && all[j].ID < all[j-1].ID; j-- {
			all[j], all[j-1] = all[j-1], all[j]
		}
	}
	return all
}

func (m *Mem) MessagesBefore(_ context.Context, channelID, beforeID int64, limit int) ([]Message, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if limit <= 0 || limit > 50 {
		limit = 50
	}
	// 升序全集 → 截取 beforeID 之前最新 limit 条（仍按升序返回，与 SQL 实现一致）。
	all := m.messageMatch(channelID)
	if beforeID > 0 {
		cut := all[:0]
		for _, msg := range all {
			if msg.ID < beforeID {
				cut = append(cut, msg)
			}
		}
		all = cut
	}
	if len(all) > limit {
		all = all[len(all)-limit:]
	}
	out := make([]Message, len(all))
	for i := range all {
		out[i] = all[i]
		u, ok := m.users[out[i].UserID]
		if ok {
			out[i].SenderNickname, out[i].SenderTag = u.Nickname, u.Tag
		}
	}
	return out, nil
}

// MessagesAfter 正序分页（向前翻）：id > afterID 的最早 limit 条。
func (m *Mem) MessagesAfter(_ context.Context, channelID, afterID int64, limit int) ([]Message, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if limit <= 0 || limit > 50 {
		limit = 50
	}
	all := m.messageMatch(channelID)
	if afterID > 0 {
		cut := all[:0]
		for _, msg := range all {
			if msg.ID > afterID {
				cut = append(cut, msg)
			}
		}
		all = cut
	}
	if len(all) > limit {
		all = all[:limit]
	}
	out := make([]Message, len(all))
	for i := range all {
		out[i] = all[i]
		u, ok := m.users[out[i].UserID]
		if ok {
			out[i].SenderNickname, out[i].SenderTag = u.Nickname, u.Tag
		}
	}
	return out, nil
}

// MessageByNonce 幂等回读原消息（频道维度隔离：同 nonce 在不同频道各自独立）。
func (m *Mem) MessageByNonce(_ context.Context, channelID, userID int64, nonce string) (*Message, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if nonce == "" {
		return nil, ErrNotFound
	}
	for _, x := range m.messages {
		if x.ChannelID == channelID && x.UserID == userID && x.Nonce == nonce {
			cp := *x
			if u, ok := m.users[cp.UserID]; ok {
				cp.SenderNickname, cp.SenderTag = u.Nickname, u.Tag
			}
			return &cp, nil
		}
	}
	return nil, ErrNotFound
}

// ---- invites ----

func (m *Mem) CreateInvite(_ context.Context, inv *Invite) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.servers[inv.ServerID]; !ok {
		return ErrNotFound
	}
	if _, ok := m.invites[inv.Code]; ok {
		return ErrConflict
	}
	cp := *inv
	m.invites[inv.Code] = &cp
	return nil
}

func (m *Mem) InviteByCode(_ context.Context, code string) (*Invite, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	inv, ok := m.invites[code]
	if !ok {
		return nil, ErrNotFound
	}
	cp := *inv
	return &cp, nil
}

// checkInviteLocked 邀请码有效性校验（调用方持 m.mu）：存在、未过期、次数未用尽。
func (m *Mem) checkInviteLocked(code string) (*Invite, error) {
	inv, ok := m.invites[code]
	if !ok {
		return nil, ErrNotFound
	}
	// 原子路径含过期校验（#22）。
	if inv.ExpiresAt != nil && time.Now().After(*inv.ExpiresAt) {
		return nil, ErrExpired
	}
	if inv.MaxUses > 0 && inv.Uses >= inv.MaxUses {
		return nil, ErrConflict
	}
	return inv, nil
}

func (m *Mem) UseInvite(_ context.Context, code string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	inv, err := m.checkInviteLocked(code)
	if err != nil {
		return err
	}
	inv.Uses++
	return nil
}

// UseInviteAndAddMember 原子消费邀请码并加入服务器（#22）：同一把锁内完成校验、
// 自增、成员插入与默认角色分配，任何一步失败都不消耗邀请码、不留分配。
func (m *Mem) UseInviteAndAddMember(_ context.Context, code string, userID int64, role MemberRole) (*ServerMember, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	inv, err := m.checkInviteLocked(code)
	if err != nil {
		return nil, err
	}
	if _, ok := m.servers[inv.ServerID]; !ok {
		return nil, ErrNotFound
	}
	if m.members[inv.ServerID] != nil {
		if _, ok := m.members[inv.ServerID][userID]; ok {
			return nil, ErrConflict
		}
	} else {
		m.members[inv.ServerID] = map[int64]*ServerMember{}
	}
	// 先解析默认角色再落笔：缺角色时不消耗邀请码、不留分配（与 SQL 事务同语义）。
	keys := defaultRoleKeys(role)
	ids := make([]int64, 0, len(keys))
	for _, key := range keys {
		roleID := m.builtinRoleIDLocked(inv.ServerID, key)
		if roleID == 0 {
			return nil, ErrNotFound
		}
		ids = append(ids, roleID)
	}
	inv.Uses++
	mem := &ServerMember{ServerID: inv.ServerID, UserID: userID, Role: role, JoinedAt: time.Now()}
	cp := *mem
	m.members[inv.ServerID][userID] = &cp
	for _, roleID := range ids {
		m.putAssignmentLocked(inv.ServerID, userID, roleID, 0, mem.JoinedAt)
	}
	return mem, nil
}

// ---- user_sessions（v1.1.0 迁移 0004） ----

func (m *Mem) UpsertUserSession(_ context.Context, s *UserSession) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	now := time.Now()
	if m.sessions[s.UserID] == nil {
		m.sessions[s.UserID] = map[string]*UserSession{}
	}
	if old, ok := m.sessions[s.UserID][s.DeviceType]; ok {
		s.CreatedAt = old.CreatedAt
	} else {
		s.CreatedAt = now
	}
	s.UpdatedAt = now
	cp := *s
	m.sessions[s.UserID][s.DeviceType] = &cp
	return nil
}

func (m *Mem) GetUserSession(_ context.Context, userID int64, deviceType string) (*UserSession, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	devs, ok := m.sessions[userID]
	if !ok {
		return nil, ErrNotFound
	}
	s, ok := devs[deviceType]
	if !ok {
		return nil, ErrNotFound
	}
	cp := *s
	return &cp, nil
}

func (m *Mem) DeleteUserSession(_ context.Context, userID int64, deviceType string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	devs, ok := m.sessions[userID]
	if !ok {
		return ErrNotFound
	}
	if _, ok := devs[deviceType]; !ok {
		return ErrNotFound
	}
	delete(devs, deviceType)
	return nil
}

func (m *Mem) DeleteAllUserSessions(_ context.Context, userID int64) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.sessions, userID)
	return nil
}

// ---- roles / member_roles / channel_overwrites / audit_log（迁移 0010，#35 B1） ----

func (m *Mem) ListRoles(_ context.Context, serverID int64) ([]Role, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := []Role{}
	for _, r := range m.roles {
		if r.ServerID == serverID {
			out = append(out, *r)
		}
	}
	sortRoles(out)
	return out, nil
}

func (m *Mem) RoleByID(_ context.Context, id int64) (*Role, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	r, ok := m.roles[id]
	if !ok {
		return nil, ErrNotFound
	}
	cp := *r
	return &cp, nil
}

func (m *Mem) CreateRole(_ context.Context, r *Role) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	// builtin 由 CreateServer/迁移回填写入，调用方只能造自定义角色。
	if r.BuiltinKey != "" {
		return ErrForbidden
	}
	if !r.Permissions.Valid() {
		return ErrForbidden
	}
	if _, ok := m.servers[r.ServerID]; !ok {
		return ErrNotFound
	}
	r.ID = newID(r.ID)
	now := time.Now()
	if r.Color == "" {
		r.Color = DefaultRoleColor
	}
	r.CreatedAt, r.UpdatedAt = now, now
	cp := *r
	m.roles[r.ID] = &cp
	return nil
}

func (m *Mem) UpdateRole(_ context.Context, r *Role) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	old, ok := m.roles[r.ID]
	if !ok {
		return ErrNotFound
	}
	if old.IsBuiltin() {
		return ErrForbidden
	}
	if !r.Permissions.Valid() {
		return ErrForbidden
	}
	cp := *old
	cp.Name = r.Name
	cp.Color = r.Color
	if cp.Color == "" {
		cp.Color = DefaultRoleColor
	}
	cp.Position = r.Position
	cp.Permissions = r.Permissions
	cp.UpdatedAt = time.Now()
	m.roles[r.ID] = &cp
	return nil
}

func (m *Mem) DeleteRole(_ context.Context, id int64) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	r, ok := m.roles[id]
	if !ok {
		return ErrNotFound
	}
	if r.IsBuiltin() {
		return ErrForbidden
	}
	delete(m.roles, id)
	// 级联：分配行 + 以该角色为目标的覆盖。
	for _, byUser := range m.memberRoles {
		for _, byRole := range byUser {
			delete(byRole, id)
		}
	}
	for k, o := range m.overwrites {
		if o.TargetType == OverwriteTargetRole && o.TargetID == id {
			delete(m.overwrites, k)
		}
	}
	return nil
}

func (m *Mem) RolesOfMember(_ context.Context, serverID, userID int64) ([]Role, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.members[serverID][userID]; !ok {
		return nil, ErrNotFound
	}
	out := []Role{}
	seen := map[int64]bool{}
	for roleID := range m.memberRoles[serverID][userID] {
		if r, ok := m.roles[roleID]; ok && r.ServerID == serverID {
			out = append(out, *r)
			seen[roleID] = true
		}
	}
	// @everyone 恒在结果里：分配行缺失（历史数据/损坏）也不能丢，权限基线靠它。
	for _, r := range m.roles {
		if r.ServerID == serverID && r.BuiltinKey == BuiltinKeyMember && !seen[r.ID] {
			out = append(out, *r)
			break
		}
	}
	sortRoles(out)
	return out, nil
}

func (m *Mem) SetMemberRoles(_ context.Context, serverID, userID int64, roleIDs []int64, assignedBy int64) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	mem, ok := m.members[serverID][userID]
	if !ok {
		return ErrNotFound
	}
	srv, ok := m.servers[serverID]
	if !ok {
		return ErrNotFound
	}
	byID := map[int64]*Role{}
	var everyoneID, adminID, ownerID int64
	for _, r := range m.roles {
		if r.ServerID != serverID {
			continue
		}
		byID[r.ID] = r
		switch r.BuiltinKey {
		case BuiltinKeyMember:
			everyoneID = r.ID
		case BuiltinKeyAdmin:
			adminID = r.ID
		case BuiltinKeyOwner:
			ownerID = r.ID
		}
	}
	if everyoneID == 0 {
		return ErrNotFound // 数据损坏：@everyone 缺失
	}
	isOwner := srv.OwnerID == userID || mem.Role == RoleOwner
	desired := map[int64]bool{}
	for _, id := range roleIDs {
		if _, ok := byID[id]; !ok {
			return ErrNotFound // 不存在或跨服，统一不出存在性
		}
		desired[id] = true
	}
	// owner builtin 只有服主/legacy owner 能持有（#35 硬化）：非 owner 经 roleIDs
	// 自授 owner 角色一律拒绝，防止成员抬权。
	if !isOwner && ownerID != 0 && desired[ownerID] {
		return ErrForbidden
	}
	desired[everyoneID] = true
	// owner 强制保留 owner 投影：服主或 legacy owner 都不可被摘掉 owner builtin。
	if isOwner {
		if ownerID == 0 {
			return ErrNotFound
		}
		desired[ownerID] = true
	}
	// 整组替换（先解析后落笔，半写不可见）。
	if m.memberRoles[serverID] == nil {
		m.memberRoles[serverID] = map[int64]map[int64]*MemberRoleAssignment{}
	}
	delete(m.memberRoles[serverID], userID)
	now := time.Now()
	for id := range desired {
		m.putAssignmentLocked(serverID, userID, id, assignedBy, now)
	}
	// legacy 列是投影：owner→owner；有 admin→admin；否则 member。
	newLegacy := RoleMember
	switch {
	case ownerID != 0 && desired[ownerID]:
		newLegacy = RoleOwner
	case adminID != 0 && desired[adminID]:
		newLegacy = RoleAdmin
	}
	mem.Role = newLegacy
	return nil
}

func (m *Mem) RoleAssignmentsOfServer(_ context.Context, serverID int64) ([]MemberRoleAssignment, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := []MemberRoleAssignment{}
	for _, byRole := range m.memberRoles[serverID] {
		for _, a := range byRole {
			out = append(out, *a)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].UserID != out[j].UserID {
			return out[i].UserID < out[j].UserID
		}
		return out[i].RoleID < out[j].RoleID
	})
	return out, nil
}

func (m *Mem) ChannelOverwrites(_ context.Context, channelID int64) ([]ChannelOverwrite, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := []ChannelOverwrite{}
	for _, o := range m.overwrites {
		if o.ChannelID == channelID {
			out = append(out, *o)
		}
	}
	sortOverwrites(out, false)
	return out, nil
}

func (m *Mem) ChannelOverwritesOfServer(_ context.Context, serverID int64) ([]ChannelOverwrite, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := []ChannelOverwrite{}
	for _, o := range m.overwrites {
		if c, ok := m.channels[o.ChannelID]; ok && c.ServerID == serverID {
			out = append(out, *o)
		}
	}
	sortOverwrites(out, true)
	return out, nil
}

func (m *Mem) SetChannelOverwrite(_ context.Context, o *ChannelOverwrite) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if o.TargetType != OverwriteTargetRole && o.TargetType != OverwriteTargetMember {
		return ErrForbidden
	}
	// 频道覆盖只作用于频道位：管理类位与未定义位都是脏数据，Store 直接拒绝
	// （该检查涵盖 Valid）。
	if o.Allow&^ChannelPermissionMask != 0 || o.Deny&^ChannelPermissionMask != 0 {
		return ErrForbidden
	}
	ch, ok := m.channels[o.ChannelID]
	if !ok {
		return ErrNotFound
	}
	// 目标必须与频道同服：角色跨服/非本服成员都不出存在性。
	switch o.TargetType {
	case OverwriteTargetRole:
		r, ok := m.roles[o.TargetID]
		if !ok || r.ServerID != ch.ServerID {
			return ErrNotFound
		}
	case OverwriteTargetMember:
		if _, ok := m.members[ch.ServerID][o.TargetID]; !ok {
			return ErrNotFound
		}
	}
	o.UpdatedAt = time.Now()
	key := overwriteKey{channelID: o.ChannelID, targetType: o.TargetType, targetID: o.TargetID}
	cp := *o
	m.overwrites[key] = &cp
	return nil
}

func (m *Mem) DeleteChannelOverwrite(_ context.Context, channelID int64, targetType OverwriteTargetType, targetID int64) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	key := overwriteKey{channelID: channelID, targetType: targetType, targetID: targetID}
	if _, ok := m.overwrites[key]; !ok {
		return ErrNotFound
	}
	delete(m.overwrites, key)
	return nil
}

func (m *Mem) WriteAudit(_ context.Context, a *AuditLog) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	a.ID = newID(a.ID)
	if a.CreatedAt.IsZero() {
		a.CreatedAt = time.Now()
	}
	if _, ok := m.auditLogs[a.ID]; ok {
		return ErrConflict
	}
	cp := copyAudit(a)
	m.auditLogs[a.ID] = &cp
	return nil
}

// copyAudit 深拷贝指针字段，避免调用方改 *TargetID/*ChannelID 穿透进内部 map。
func copyAudit(a *AuditLog) AuditLog {
	cp := *a
	if a.TargetID != nil {
		v := *a.TargetID
		cp.TargetID = &v
	}
	if a.ChannelID != nil {
		v := *a.ChannelID
		cp.ChannelID = &v
	}
	return cp
}

func (m *Mem) AuditLogBefore(_ context.Context, serverID, beforeID int64, limit int) ([]AuditLog, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if limit <= 0 || limit > 50 {
		limit = 50
	}
	out := []AuditLog{}
	for _, a := range m.auditLogs {
		if a.ServerID != serverID {
			continue
		}
		if beforeID > 0 && a.ID >= beforeID {
			continue
		}
		out = append(out, copyAudit(a))
	}
	// 倒序游标（新→旧）。
	sort.Slice(out, func(i, j int) bool { return out[i].ID > out[j].ID })
	if len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}
