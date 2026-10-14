// Package store 是 M3 持久化层：领域类型 + 数据访问接口。
// MySQL 实现（mysql.go）与内存实现（mem.go）同接口；测试用内存实现，
// 真库测试在 DSN 不可用时 t.Skip（LITETALK_MYSQL_DSN 注入）。
package store

import (
	"context"
	"errors"
	"time"
)

// 领域错误（API 层据此映射 HTTP 状态码）。
var (
	ErrNotFound  = errors.New("not found")
	ErrConflict  = errors.New("conflict")
	ErrForbidden = errors.New("forbidden")
)

// User 用户（§6.1 users 表）。无头像字段。
type User struct {
	ID            int64     `json:"id" db:"id"`
	Email         string    `json:"email" db:"email"`
	Nickname      string    `json:"nickname" db:"nickname"`
	Tag           string    `json:"tag" db:"tag"`
	PasswordHash  string    `json:"-" db:"password_hash"`
	EmailVerified bool      `json:"email_verified" db:"email_verified"`
	IsGuest       bool      `json:"is_guest" db:"is_guest"`
	Status        int8      `json:"status" db:"status"`
	CreatedAt     time.Time `json:"created_at" db:"created_at"`
	UpdatedAt     time.Time `json:"updated_at" db:"updated_at"`
}

// FriendshipStatus 好友关系状态。
type FriendshipStatus string

const (
	FriendPending  FriendshipStatus = "pending"
	FriendAccepted FriendshipStatus = "accepted"
	FriendBlocked  FriendshipStatus = "blocked"
)

// Friendship 好友关系（单行表示一对，user_id < friend_id 由 API 层保证顺序）。
type Friendship struct {
	ID        int64            `json:"id" db:"id"`
	UserID    int64            `json:"user_id" db:"user_id"`
	FriendID  int64            `json:"friend_id" db:"friend_id"`
	Status    FriendshipStatus `json:"status" db:"status"`
	CreatedAt time.Time        `json:"created_at" db:"created_at"`
}

// Server 服务器（群组）。图标为预设 Lucide 图标名 + 颜色，无图片。
type Server struct {
	ID        int64     `json:"id" db:"id"`
	Name      string    `json:"name" db:"name"`
	OwnerID   int64     `json:"owner_id" db:"owner_id"`
	IconName  string    `json:"icon_name" db:"icon_name"`
	IconColor string    `json:"icon_color" db:"icon_color"`
	CreatedAt time.Time `json:"created_at" db:"created_at"`
}

// MemberRole 成员角色。
type MemberRole string

const (
	RoleOwner  MemberRole = "owner"
	RoleAdmin  MemberRole = "admin"
	RoleMember MemberRole = "member"
)

// ServerMember 服务器成员。
type ServerMember struct {
	ServerID int64      `json:"server_id" db:"server_id"`
	UserID   int64      `json:"user_id" db:"user_id"`
	Role     MemberRole `json:"role" db:"role"`
	JoinedAt time.Time  `json:"joined_at" db:"joined_at"`
	// JoinedUser 供成员列表响应内联（内存实现直接填充）。
	Nickname string `json:"nickname,omitempty" db:"-"`
	Tag      string `json:"tag,omitempty" db:"-"`
}

// ChannelType 频道类型。
type ChannelType string

const (
	ChannelText  ChannelType = "text"
	ChannelVoice ChannelType = "voice"
)

// Channel 频道。
type Channel struct {
	ID        int64       `json:"id" db:"id"`
	ServerID  int64       `json:"server_id" db:"server_id"`
	Name      string      `json:"name" db:"name"`
	Type      ChannelType `json:"type" db:"type"`
	Position  int         `json:"position" db:"position"`
	CreatedAt time.Time   `json:"created_at" db:"created_at"`
}

// Message 文字频道消息。
type Message struct {
	ID        int64     `json:"id" db:"id"`
	ChannelID int64     `json:"channel_id" db:"channel_id"`
	UserID    int64     `json:"user_id" db:"user_id"`
	Content   string    `json:"content" db:"content"`
	CreatedAt time.Time `json:"created_at" db:"created_at"`
	// Sender 内联发送者信息（列表响应用）。
	SenderNickname string `json:"sender_nickname,omitempty" db:"-"`
	SenderTag      string `json:"sender_tag,omitempty" db:"-"`
}

// Invite 邀请码。
type Invite struct {
	Code      string     `json:"code" db:"code"`
	ServerID  int64      `json:"server_id" db:"server_id"`
	CreatorID int64      `json:"creator_id" db:"creator_id"`
	MaxUses   int        `json:"max_uses" db:"max_uses"`
	Uses      int        `json:"uses" db:"uses"`
	ExpiresAt *time.Time `json:"expires_at,omitempty" db:"expires_at"`
}

// Store 数据访问接口（MySQL 与内存实现共同契约）。
type Store interface {
	// users
	CreateUser(ctx context.Context, u *User) error
	UserByID(ctx context.Context, id int64) (*User, error)
	UserByEmail(ctx context.Context, email string) (*User, error)
	UserByNicknameTag(ctx context.Context, nickname, tag string) (*User, error)
	UpdateUser(ctx context.Context, u *User) error

	// friendships
	CreateFriendship(ctx context.Context, f *Friendship) error
	FriendshipByID(ctx context.Context, id int64) (*Friendship, error)
	// FriendshipsOfUser 返回该用户参与的全部关系（两个方向）。
	FriendshipsOfUser(ctx context.Context, userID int64) ([]Friendship, error)
	FriendshipBetween(ctx context.Context, a, b int64) (*Friendship, error)
	UpdateFriendship(ctx context.Context, f *Friendship) error
	DeleteFriendship(ctx context.Context, id int64) error

	// servers
	CreateServer(ctx context.Context, s *Server) error
	ServerByID(ctx context.Context, id int64) (*Server, error)
	// ServersOfUser 返回用户所在服务器（经 server_members 关联）。
	ServersOfUser(ctx context.Context, userID int64) ([]Server, error)
	UpdateServer(ctx context.Context, s *Server) error
	DeleteServer(ctx context.Context, id int64) error
	AddMember(ctx context.Context, m *ServerMember) error
	RemoveMember(ctx context.Context, serverID, userID int64) error
	Member(ctx context.Context, serverID, userID int64) (*ServerMember, error)
	MembersOfServer(ctx context.Context, serverID int64) ([]ServerMember, error)

	// channels
	CreateChannel(ctx context.Context, c *Channel) error
	ChannelByID(ctx context.Context, id int64) (*Channel, error)
	ChannelsOfServer(ctx context.Context, serverID int64) ([]Channel, error)
	UpdateChannel(ctx context.Context, c *Channel) error
	DeleteChannel(ctx context.Context, id int64) error

	// messages
	CreateMessage(ctx context.Context, m *Message) error
	// MessagesBefore 倒序分页：beforeID=0 取最新 limit 条（仍按 id 倒序返回）。
	MessagesBefore(ctx context.Context, channelID, beforeID int64, limit int) ([]Message, error)

	// invites
	CreateInvite(ctx context.Context, inv *Invite) error
	InviteByCode(ctx context.Context, code string) (*Invite, error)
	// UseInvite 自增使用次数（超过 max_uses 返回 ErrConflict）。
	UseInvite(ctx context.Context, code string) error
}

// KV 键值抽象（Redis 与内存实现共同契约；auth 的验证码/刷新令牌/限流使用）。
type KV interface {
	Get(ctx context.Context, key string) (string, error)
	Set(ctx context.Context, key, value string, ttl time.Duration) error
	Del(ctx context.Context, keys ...string) error
	// Incr 自增并返回新值；key 不存在时从 0 开始并设置 ttl。
	Incr(ctx context.Context, key string, ttl time.Duration) (int64, error)
}

// ErrKVNotFound Get 未命中（区别于其他错误）。
var ErrKVNotFound = errors.New("kv: key not found")
