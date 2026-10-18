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

	nextID map[string]int64

	*MemKV // KV 部分（Get/Set/Del/Incr/IncrCap/Eval 由 MemKV 提供）
}

// NewMem 创建内存存储。
func NewMem() *Mem {
	return &Mem{
		users:       map[int64]*User{},
		friendships: map[int64]*Friendship{},
		servers:     map[int64]*Server{},
		members:     map[int64]map[int64]*ServerMember{},
		channels:    map[int64]*Channel{},
		categories:  map[int64]*Category{},
		messages:    map[int64]*Message{},
		invites:     map[string]*Invite{},
		sessions:    map[int64]map[string]*UserSession{},
		nextID:      map[string]int64{},
		MemKV:       NewMemKV(),
	}
}

func (m *Mem) next(kind string) int64 {
	m.nextID[kind]++
	return m.nextID[kind]
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
	u.ID = m.next("user")
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
	f.ID = m.next("friendship")
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
	s.ID = m.next("server")
	s.CreatedAt = time.Now()
	cp := *s
	m.servers[s.ID] = &cp
	return nil
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
	mem.JoinedAt = time.Now()
	cp := *mem
	m.members[mem.ServerID][mem.UserID] = &cp
	return nil
}

func (m *Mem) RemoveMember(_ context.Context, serverID, userID int64) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.members[serverID][userID]; !ok {
		return ErrNotFound
	}
	delete(m.members[serverID], userID)
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
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.servers[c.ServerID]; !ok {
		return ErrNotFound
	}
	c.ID = m.next("channel")
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
		if c.ServerID == serverID {
			out = append(out, *c)
		}
	}
	return out, nil
}

func (m *Mem) UpdateChannel(_ context.Context, c *Channel) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.channels[c.ID]; !ok {
		return ErrNotFound
	}
	cp := *c
	m.channels[c.ID] = &cp
	return nil
}

func (m *Mem) DeleteChannel(_ context.Context, id int64) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.channels[id]; !ok {
		return ErrNotFound
	}
	delete(m.channels, id)
	for mid, msg := range m.messages {
		if msg.ChannelID == id {
			delete(m.messages, mid)
		}
	}
	return nil
}

// ---- categories（v0.1.2 迁移 0003；内存实现同步语义） ----

func (m *Mem) CreateCategory(_ context.Context, c *Category) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.servers[c.ServerID]; !ok {
		return ErrNotFound
	}
	c.ID = m.next("category")
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

// DeleteCategoryWithChannels 删除分类及其下全部频道（含各频道消息），
// 与 MySQL 实现语义一致（单操作原子性由 m.mu 保证）。
func (m *Mem) DeleteCategoryWithChannels(_ context.Context, id int64) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	c, ok := m.categories[id]
	if !ok {
		return ErrNotFound
	}
	for cid, ch := range m.channels {
		if ch.CategoryID != nil && *ch.CategoryID == c.ID {
			delete(m.channels, cid)
			for mid, msg := range m.messages {
				if msg.ChannelID == cid {
					delete(m.messages, mid)
				}
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
	msg.ID = m.next("message")
	msg.CreatedAt = time.Now()
	cp := *msg
	m.messages[msg.ID] = &cp
	return nil
}

func (m *Mem) MessagesBefore(_ context.Context, channelID, beforeID int64, limit int) ([]Message, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if limit <= 0 || limit > 50 {
		limit = 50
	}
	// 收集符合条件的消息后按 id 倒序取 limit 条，再反转为正序返回。
	all := []Message{}
	for _, msg := range m.messages {
		if msg.ChannelID != channelID {
			continue
		}
		if beforeID > 0 && msg.ID >= beforeID {
			continue
		}
		all = append(all, *msg)
	}
	// 插入排序（数据量小，limit≤50）。
	for i := 1; i < len(all); i++ {
		for j := i; j > 0 && all[j].ID > all[j-1].ID; j-- {
			all[j], all[j-1] = all[j-1], all[j]
		}
	}
	if len(all) > limit {
		all = all[:limit]
	}
	// 反转为正序（旧→新）。
	out := make([]Message, len(all))
	for i := range all {
		out[i] = all[len(all)-1-i]
		u, ok := m.users[out[i].UserID]
		if ok {
			out[i].SenderNickname, out[i].SenderTag = u.Nickname, u.Tag
		}
	}
	return out, nil
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

func (m *Mem) UseInvite(_ context.Context, code string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	inv, ok := m.invites[code]
	if !ok {
		return ErrNotFound
	}
	// 原子路径含过期校验（#22）。
	if inv.ExpiresAt != nil && time.Now().After(*inv.ExpiresAt) {
		return ErrExpired
	}
	if inv.MaxUses > 0 && inv.Uses >= inv.MaxUses {
		return ErrConflict
	}
	inv.Uses++
	return nil
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
