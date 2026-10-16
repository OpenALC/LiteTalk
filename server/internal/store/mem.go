package store

import (
	"context"
	"sort"
	"sync"
	"time"
)

// Mem 是 Store + KV 的内存实现。供 API 层测试与无数据库环境使用；
// 语义与 MySQL 实现一致（唯一约束、外键式删除级联在 API 层处理）。
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

	nextID map[string]int64

	// KV 部分
	kv      map[string]kvEntry
	kvTimed map[string]*time.Timer
}

type kvEntry struct {
	value string
	exp   time.Time // zero = 永不过期
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
		nextID:      map[string]int64{},
		kv:          map[string]kvEntry{},
		kvTimed:     map[string]*time.Timer{},
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
	if inv.MaxUses > 0 && inv.Uses >= inv.MaxUses {
		return ErrConflict
	}
	inv.Uses++
	return nil
}

// ---- KV ----

func (m *Mem) Get(_ context.Context, key string) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	e, ok := m.kv[key]
	if !ok {
		return "", ErrKVNotFound
	}
	if !e.exp.IsZero() && time.Now().After(e.exp) {
		delete(m.kv, key)
		return "", ErrKVNotFound
	}
	return e.value, nil
}

func (m *Mem) Set(_ context.Context, key, value string, ttl time.Duration) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.setLocked(key, value, ttl)
	return nil
}

func (m *Mem) setLocked(key, value string, ttl time.Duration) {
	e := kvEntry{value: value}
	if ttl > 0 {
		e.exp = time.Now().Add(ttl)
		if t, ok := m.kvTimed[key]; ok {
			t.Stop()
		}
		m.kvTimed[key] = time.AfterFunc(ttl, func() {
			m.mu.Lock()
			defer m.mu.Unlock()
			delete(m.kv, key)
			delete(m.kvTimed, key)
		})
	}
	m.kv[key] = e
}

func (m *Mem) Del(_ context.Context, keys ...string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, k := range keys {
		delete(m.kv, k)
		if t, ok := m.kvTimed[k]; ok {
			t.Stop()
			delete(m.kvTimed, k)
		}
	}
	return nil
}

func (m *Mem) Incr(_ context.Context, key string, ttl time.Duration) (int64, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var n int64
	if e, ok := m.kv[key]; ok && (e.exp.IsZero() || time.Now().Before(e.exp)) {
		// 解析旧值（Incr 只用于纯数字计数）。
		for _, c := range e.value {
			if c < '0' || c > '9' {
				n = 0
				break
			}
			n = n*10 + int64(c-'0')
		}
	}
	n++
	// 首次创建才设置 TTL（与 Redis INCR+EX 语义对齐）。
	if _, ok := m.kvTimed[key]; !ok {
		m.setLocked(key, itoa(n), ttl)
	} else {
		e := m.kv[key]
		e.value = itoa(n)
		m.kv[key] = e
	}
	return n, nil
}

func itoa(n int64) string {
	if n == 0 {
		return "0"
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	return string(b[i:])
}
