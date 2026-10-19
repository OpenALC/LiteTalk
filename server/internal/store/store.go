// Package store 是 M3 持久化层：领域类型 + 数据访问接口。
// MySQL 实现（mysql.go）与内存实现（mem.go）同接口；测试用内存实现，
// 真库测试在 DSN 不可用时 t.Skip（LITETALK_MYSQL_DSN 注入）。
package store

import (
	"context"
	"errors"
	"time"

	"github.com/OpenALC/LiteTalk/server/internal/snowflake"
)

// 领域错误（API 层据此映射 HTTP 状态码）。
var (
	ErrNotFound  = errors.New("not found")
	ErrConflict  = errors.New("conflict")
	ErrForbidden = errors.New("forbidden")
	ErrExpired   = errors.New("expired")
)

// newID 新行主键（#31）：调用方未预置时取 Snowflake，显式赋值优先（测试夹具用）。
// 各 Create 显式带 id 列插入，不再依赖 AUTO_INCREMENT；列上的 AUTO_INCREMENT 保留不动。
func newID(id int64) int64 {
	if id != 0 {
		return id
	}
	return snowflake.Next()
}

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
// remark/group_name 为发起方视角的本地备注与分组（v0.1.2 迁移 0003）。
type Friendship struct {
	ID        int64            `json:"id" db:"id"`
	UserID    int64            `json:"user_id" db:"user_id"`
	FriendID  int64            `json:"friend_id" db:"friend_id"`
	Status    FriendshipStatus `json:"status" db:"status"`
	Remark    string           `json:"remark" db:"remark"`
	GroupName string           `json:"group_name" db:"group_name"`
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

// Channel 频道。category_id 指向分类表（迁移 0003），NULL = 未分类。
type Channel struct {
	ID         int64       `json:"id" db:"id"`
	ServerID   int64       `json:"server_id" db:"server_id"`
	Name       string      `json:"name" db:"name"`
	Type       ChannelType `json:"type" db:"type"`
	CategoryID *int64      `json:"category_id" db:"category_id"`
	Position   int         `json:"position" db:"position"`
	CreatedAt  time.Time   `json:"created_at" db:"created_at"`
}

// Category 频道分类（v0.1.2 迁移 0003）。空分类可持久化，删除可连带频道。
type Category struct {
	ID        int64     `json:"id" db:"id"`
	ServerID  int64     `json:"server_id" db:"server_id"`
	Name      string    `json:"name" db:"name"`
	Position  int       `json:"position" db:"position"`
	CreatedAt time.Time `json:"created_at" db:"created_at"`
}

// Message 文字频道消息。
// Nonce 客户端幂等键（#31）：空 = 不去重；(user_id, nonce) 唯一索引在 0005 迁移。
type Message struct {
	ID        int64     `json:"id" db:"id"`
	ChannelID int64     `json:"channel_id" db:"channel_id"`
	UserID    int64     `json:"user_id" db:"user_id"`
	Content   string    `json:"content" db:"content"`
	Nonce     string    `json:"nonce,omitempty" db:"nonce"`
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

// UserSession 用户会话（v1.1.0 迁移 0004）：每设备一个会话，登录/注册/游客成功时覆盖写入
// （覆盖即吊销旧会话）。sid 是 32 hex 随机串，写入时由调用方生成。
type UserSession struct {
	UserID     int64     `json:"user_id" db:"user_id"`
	DeviceType string    `json:"device_type" db:"device_type"`
	Sid        string    `json:"sid" db:"sid"`
	IP         string    `json:"ip" db:"ip"`
	CreatedAt  time.Time `json:"created_at" db:"created_at"`
	UpdatedAt  time.Time `json:"updated_at" db:"updated_at"`
}

// Store 数据访问接口（MySQL 与内存实现共同契约）。
type Store interface {
	// users
	CreateUser(ctx context.Context, u *User) error
	UserByID(ctx context.Context, id int64) (*User, error)
	UserByEmail(ctx context.Context, email string) (*User, error)
	UserByNicknameTag(ctx context.Context, nickname, tag string) (*User, error)
	UpdateUser(ctx context.Context, u *User) error
	// HasUsers 库内是否已有用户（首启探测用；UserByID(1) 随 Snowflake 失效）。
	HasUsers(ctx context.Context) (bool, error)
	// HasServers 库内是否已有服务器（#27：实例是否已被认领的持久判据，
	// 默认进程内 KV 重启丢标记，不能只看消费标记）。
	HasServers(ctx context.Context) (bool, error)

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

	// categories（v0.1.2 迁移 0003）
	CreateCategory(ctx context.Context, c *Category) error
	CategoryByID(ctx context.Context, id int64) (*Category, error)
	CategoriesOfServer(ctx context.Context, serverID int64) ([]Category, error)
	// DeleteCategoryWithChannels 单事务删除分类及其下全部频道（含各频道消息）。
	DeleteCategoryWithChannels(ctx context.Context, id int64) error
	// UpdateCategory 全量写 name/position（不动 created_at/所属服务器）。
	UpdateCategory(ctx context.Context, c *Category) error
	// ReorderCategories 把 id 移到 position（0-based，越界夹到首/尾），同服务器
	// 其它分类相对顺序保持并重排为 0..n-1。
	ReorderCategories(ctx context.Context, serverID, id int64, position int) error

	// messages
	// CreateMessage 带 nonce 且撞唯一索引时返回 ErrConflict（调用方回读原消息）。
	CreateMessage(ctx context.Context, m *Message) error
	// MessagesBefore 倒序分页：beforeID=0 取最新 limit 条（按 id 正序返回，旧→新）。
	MessagesBefore(ctx context.Context, channelID, beforeID int64, limit int) ([]Message, error)
	// MessagesAfter 正序分页（向前翻更新）：afterID=0 取最旧 limit 条；取 id 大于
	// afterID 的最早 limit 条，按 id 正序返回。limit 越界回落 50（与 Before 同口径）。
	MessagesAfter(ctx context.Context, channelID, afterID int64, limit int) ([]Message, error)
	// MessageByNonce 幂等回读：按 (user_id, nonce) 找原消息；无命中 ErrNotFound。
	MessageByNonce(ctx context.Context, userID int64, nonce string) (*Message, error)

	// invites
	CreateInvite(ctx context.Context, inv *Invite) error
	InviteByCode(ctx context.Context, code string) (*Invite, error)
	// UseInvite 自增使用次数（超过 max_uses 返回 ErrConflict）。
	UseInvite(ctx context.Context, code string) error
	// UseInviteAndAddMember 原子地消费邀请码并把 userID 加入邀请码所属服务器（#22）：
	// 邀请码校验（存在/未过期/次数未用尽）与成员插入在同一事务里完成，成员已存在
	// 时返回 ErrConflict 且不消耗邀请码。服务器 ID 取自 invitation 行，避免调用方传错。
	UseInviteAndAddMember(ctx context.Context, code string, userID int64, role MemberRole) (*ServerMember, error)

	// user_sessions（v1.1.0 迁移 0004，多设备会话）
	UpsertUserSession(ctx context.Context, s *UserSession) error
	// GetUserSession 返回该设备当前会话；未命中返回 ErrNotFound。
	GetUserSession(ctx context.Context, userID int64, deviceType string) (*UserSession, error)
	// DeleteUserSession 删除指定设备会话；未命中返回 ErrNotFound。
	DeleteUserSession(ctx context.Context, userID int64, deviceType string) error
	// DeleteAllUserSessions 删除该用户全部设备会话（重置密码用）。
	DeleteAllUserSessions(ctx context.Context, userID int64) error
}

// KV 键值抽象（Redis、SQLite 与内存实现共同契约；auth 的验证码/刷新令牌/限流使用）。
type KV interface {
	Get(ctx context.Context, key string) (string, error)
	Set(ctx context.Context, key, value string, ttl time.Duration) error
	Del(ctx context.Context, keys ...string) error
	// Incr 自增并返回新值；key 不存在时从 0 开始并设置 ttl。
	Incr(ctx context.Context, key string, ttl time.Duration) (int64, error)
	// IncrCap 原子自增并限幅（#22）：新值 > cap 时不增长、返回 cap+1（调用方据此判超限）；
	// key 不存在时从 1 开始并设置 ttl。用于替换「Incr 后再比较」的竞态。
	IncrCap(ctx context.Context, key string, ttl time.Duration, cap int64) (int64, error)
	// Eval 执行 Lua 脚本（Redis）或锁内等价模拟（Mem）。
	// 当前唯一脚本是 CASScript；实现方不认识脚本应报错而非猜语义（fail-closed）。
	Eval(ctx context.Context, script string, keys []string, args ...string) (any, error)
}

// CASScript 比较交换（compare-and-swap）脚本：
// KEYS[1] 当前值与 ARGV[1] 相等时替换为 ARGV[2]、TTL 重置为 ARGV[3] 秒并返回 1，否则返回 0。
// Redis 单线程执行 EVAL，GET 比对与 SET 之间插不进其他写——auth 的刷新令牌旋转靠它
// 杜绝并发重放（同一令牌被多个 goroutine 同时旋转时只有一个成功）。
// Mem.Eval 在全局锁内模拟同一语义（GET+SET 排队执行，等价原子）。
const CASScript = `
local cur = redis.call('GET', KEYS[1])
if cur == ARGV[1] then
  redis.call('SET', KEYS[1], ARGV[2], 'EX', ARGV[3])
  return 1
end
return 0
`

// ErrKVNotFound Get 未命中（区别于其他错误）。
var ErrKVNotFound = errors.New("kv: key not found")
