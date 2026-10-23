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

// Friendship 好友关系（单行表示一对，user_id 是发起方，方向有语义）。
// remark/group_name 为发起方视角的本地备注与分组（v0.1.2 迁移 0003）；
// 迁移 0008 起数据库按无序对唯一，反向重复插入返回 ErrConflict。
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

// ChannelScope 频道作用域（迁移 0012，#38 B1）：
// server = 服务器频道；direct/group = 私聊/群私聊，没有所属服务器。
type ChannelScope string

const (
	ChannelScopeServer ChannelScope = "server"
	ChannelScopeDirect ChannelScope = "direct"
	ChannelScopeGroup  ChannelScope = "group"
)

// Channel 频道。category_id 指向分类表（迁移 0003），NULL = 未分类。
type Channel struct {
	ID int64 `json:"id" db:"id"`
	// ServerID 所属服务器；0 表示无服务器（direct/group，DB 存 NULL）。
	ServerID int64        `json:"server_id" db:"server_id"`
	Name     string       `json:"name" db:"name"`
	Type     ChannelType  `json:"type" db:"type"`
	Scope    ChannelScope `json:"scope" db:"scope"`
	// DMKey 私聊去重键 "<minUID>:<maxUID>"，只有 direct 有值（server/group 存 NULL）。
	// 不对外：客户端用会话 ID 与成员列表识别私聊，键里只是双方 uid。
	DMKey      string    `json:"-" db:"dm_key"`
	CategoryID *int64    `json:"category_id" db:"category_id"`
	Position   int       `json:"position" db:"position"`
	CreatedAt  time.Time `json:"created_at" db:"created_at"`
	// #37 B1 频道选项：密码（bcrypt）、人数上限（0=不限）、子频道父级（NULL=顶层）。
	// PasswordHash 不出 JSON——wire 只暴露 has_password，明文/哈希都不回显。
	// 更新是全行写：读到的 hash 必须原样写回，清除密码写空串。
	ParentID     *int64 `json:"parent_id" db:"parent_id"`
	UserLimit    int    `json:"user_limit" db:"user_limit"`
	PasswordHash string `json:"-" db:"password_hash"`
}

// ChannelMemberRole 会话成员角色。owner 只用于 group（首个成员即创建者）。
type ChannelMemberRole string

const (
	ChannelMemberOwner  ChannelMemberRole = "owner"
	ChannelMemberMember ChannelMemberRole = "member"
)

// ChannelMember 会话成员（迁移 0012 channel_members 表）。
// Nickname/Tag 供成员列表响应内联（与 ServerMember 同口径，SQL 侧 JOIN users）。
type ChannelMember struct {
	ChannelID int64             `json:"channel_id" db:"channel_id"`
	UserID    int64             `json:"user_id" db:"user_id"`
	Role      ChannelMemberRole `json:"role" db:"role"`
	JoinedAt  time.Time         `json:"joined_at" db:"joined_at"`
	Nickname  string            `json:"nickname,omitempty" db:"-"`
	Tag       string            `json:"tag,omitempty" db:"-"`
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
// Nonce 客户端幂等键（#31）：空 = 不去重；(channel_id, user_id, nonce) 唯一索引见迁移 0007。
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

// Role 角色（迁移 0010）。BuiltinKey 三值 member/admin/owner 为内置（不可改删、
// 每服各一条），空串 = 自定义角色（DB 存 NULL）。Permissions 只用低 15 位。
type Role struct {
	ID          int64      `json:"id" db:"id"`
	ServerID    int64      `json:"server_id" db:"server_id"`
	Name        string     `json:"name" db:"name"`
	Color       string     `json:"color" db:"color"`
	Position    int        `json:"position" db:"position"`
	Permissions Permission `json:"permissions" db:"permissions"`
	BuiltinKey  string     `json:"builtin_key,omitempty" db:"builtin_key"`
	CreatedAt   time.Time  `json:"created_at" db:"created_at"`
	UpdatedAt   time.Time  `json:"updated_at" db:"updated_at"`
}

// IsBuiltin 是否内置角色。
func (r *Role) IsBuiltin() bool { return r.BuiltinKey != "" }

// MemberRoleAssignment 成员角色分配（member_roles 表）。
type MemberRoleAssignment struct {
	ServerID   int64     `json:"server_id" db:"server_id"`
	UserID     int64     `json:"user_id" db:"user_id"`
	RoleID     int64     `json:"role_id" db:"role_id"`
	AssignedAt time.Time `json:"assigned_at" db:"assigned_at"`
	AssignedBy int64     `json:"assigned_by" db:"assigned_by"`
}

// OverwriteTargetType 覆盖目标类型。
type OverwriteTargetType string

const (
	OverwriteTargetRole   OverwriteTargetType = "role"
	OverwriteTargetMember OverwriteTargetType = "member"
)

// ChannelOverwrite 频道权限覆盖（channel_overwrites 表）。Allow/Deny 只允许
// ChannelPermissionMask 内的位——管理类位属于角色，越界位由 Store 拒绝入参。
type ChannelOverwrite struct {
	ChannelID  int64               `json:"channel_id" db:"channel_id"`
	TargetType OverwriteTargetType `json:"target_type" db:"target_type"`
	TargetID   int64               `json:"target_id" db:"target_id"`
	Allow      Permission          `json:"allow" db:"allow"`
	Deny       Permission          `json:"deny" db:"deny"`
	UpdatedAt  time.Time           `json:"updated_at" db:"updated_at"`
}

// AuditLog 审计日志（audit_log 表）。ID 由调用方 Snowflake 预置或 WriteAudit 回填；
// TargetType 空串、TargetID/ChannelID 为 nil 时存 NULL。
type AuditLog struct {
	ID         int64     `json:"id" db:"id"`
	ServerID   int64     `json:"server_id" db:"server_id"`
	ActorID    int64     `json:"actor_id" db:"actor_id"`
	Action     string    `json:"action" db:"action"`
	TargetType string    `json:"target_type,omitempty" db:"target_type"`
	TargetID   *int64    `json:"target_id,omitempty" db:"target_id"`
	ChannelID  *int64    `json:"channel_id,omitempty" db:"channel_id"`
	Detail     string    `json:"detail" db:"detail"`
	CreatedAt  time.Time `json:"created_at" db:"created_at"`
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
	// CreateFriendship 写入 remark/group_name；同一无序对（含反向）重复返回 ErrConflict。
	CreateFriendship(ctx context.Context, f *Friendship) error
	FriendshipByID(ctx context.Context, id int64) (*Friendship, error)
	// FriendshipsOfUser 返回该用户参与的全部关系（两个方向）。
	FriendshipsOfUser(ctx context.Context, userID int64) ([]Friendship, error)
	FriendshipBetween(ctx context.Context, a, b int64) (*Friendship, error)
	UpdateFriendship(ctx context.Context, f *Friendship) error
	DeleteFriendship(ctx context.Context, id int64) error

	// servers
	// CreateServer 同一事务/锁内写 server + 三条 builtin 角色（#35）。
	CreateServer(ctx context.Context, s *Server) error
	ServerByID(ctx context.Context, id int64) (*Server, error)
	// ServersOfUser 返回用户所在服务器（经 server_members 关联）。
	ServersOfUser(ctx context.Context, userID int64) ([]Server, error)
	UpdateServer(ctx context.Context, s *Server) error
	// DeleteServer 级联删除该服 roles/member_roles/频道覆盖/audit_log 及既有子资源。
	DeleteServer(ctx context.Context, id int64) error
	// AddMember 同一事务/锁内写成员 + @everyone 分配，并按 legacy role 追加 admin/owner builtin。
	AddMember(ctx context.Context, m *ServerMember) error
	// RemoveMember 删成员行 + 其角色分配 + 该服内以该成员为目标的频道覆盖。
	RemoveMember(ctx context.Context, serverID, userID int64) error
	Member(ctx context.Context, serverID, userID int64) (*ServerMember, error)
	MembersOfServer(ctx context.Context, serverID int64) ([]ServerMember, error)

	// channels
	// CreateChannel 只建服务器频道：scope 空串按 server 处理，非 server 或 server_id 为 0
	// 返回 ErrForbidden。私聊/群私聊走 CreateConversation。
	CreateChannel(ctx context.Context, c *Channel) error
	ChannelByID(ctx context.Context, id int64) (*Channel, error)
	// ChannelsOfServer 只返回该服的 server 作用域频道（不含 direct/group）。
	ChannelsOfServer(ctx context.Context, serverID int64) ([]Channel, error)
	// UpdateChannel 全行写；调用方必须读-改-写以保留 scope/dm_key，
	// 私聊行写回时 ServerID=0 落 NULL、DMKey 空串落 NULL。
	UpdateChannel(ctx context.Context, c *Channel) error
	// DeleteChannel 同事务删消息、该频道覆盖、会话成员与频道行。
	DeleteChannel(ctx context.Context, id int64) error

	// conversations（迁移 0012，#38 B1）：私聊/群私聊是 server_id NULL 的文字频道。
	// CreateConversation 同一事务写频道行 + channel_members；direct 必须恰两个不同成员，
	// 双方都是 member，dm_key 由成员 ID 规范化为 "<minUID>:<maxUID>"，重复创建返回 ErrConflict；
	// group 至少一个成员，memberIDs[0] 视为创建者记 owner，其余 member。
	// scope 非 direct/group 或成员数不合法返回 ErrForbidden。
	CreateConversation(ctx context.Context, c *Channel, memberIDs []int64) error
	// FindDirectChannel 按规范化 dm_key 找私聊；无命中 ErrNotFound。
	FindDirectChannel(ctx context.Context, a, b int64) (*Channel, error)
	// ListConversations 用户参与的 direct/group 会话，按创建时间、ID 倒序。
	ListConversations(ctx context.Context, userID int64) ([]Channel, error)
	// ChannelMembers 成员列表，按 joined_at、user_id 升序；频道不存在返回空列表。
	ChannelMembers(ctx context.Context, channelID int64) ([]ChannelMember, error)
	// ChannelMember 单个成员；非成员或频道不存在返回 ErrNotFound。
	ChannelMember(ctx context.Context, channelID, userID int64) (*ChannelMember, error)
	// AddChannelMember 只接受 group 频道；role 空串按 member 处理，
	// direct/server 频道或非法角色返回 ErrForbidden，重复成员返回 ErrConflict。
	// direct 成员固定为双方，增删留给 B2 的 API 语义，不在 Store 放开。
	AddChannelMember(ctx context.Context, channelID, userID int64, role ChannelMemberRole) error
	// RemoveChannelMember 只接受 group 频道；direct/server 返回 ErrForbidden，
	// 频道不存在或成员未命中返回 ErrNotFound。
	RemoveChannelMember(ctx context.Context, channelID, userID int64) error

	// categories（v0.1.2 迁移 0003）
	CreateCategory(ctx context.Context, c *Category) error
	CategoryByID(ctx context.Context, id int64) (*Category, error)
	CategoriesOfServer(ctx context.Context, serverID int64) ([]Category, error)
	// DeleteCategoryWithChannels 单事务删除分类及其下全部频道（含各频道消息与覆盖）。
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
	// MessageByNonce 幂等回读：按 (channel_id, user_id, nonce) 找原消息；无命中 ErrNotFound。
	MessageByNonce(ctx context.Context, channelID, userID int64, nonce string) (*Message, error)

	// invites
	CreateInvite(ctx context.Context, inv *Invite) error
	InviteByCode(ctx context.Context, code string) (*Invite, error)
	// UseInvite 自增使用次数（超过 max_uses 返回 ErrConflict）。
	UseInvite(ctx context.Context, code string) error
	// UseInviteAndAddMember 原子地消费邀请码并把 userID 加入邀请码所属服务器（#22）：
	// 邀请码校验（存在/未过期/次数未用尽）与成员插入在同一事务里完成，成员已存在
	// 时返回 ErrConflict 且不消耗邀请码、不留角色分配。默认角色分配随成员同事务写入（#35）。
	// 服务器 ID 取自 invitation 行，避免调用方传错。
	UseInviteAndAddMember(ctx context.Context, code string, userID int64, role MemberRole) (*ServerMember, error)

	// user_sessions（v1.1.0 迁移 0004，多设备会话）
	UpsertUserSession(ctx context.Context, s *UserSession) error
	// GetUserSession 返回该设备当前会话；未命中返回 ErrNotFound。
	GetUserSession(ctx context.Context, userID int64, deviceType string) (*UserSession, error)
	// DeleteUserSession 删除指定设备会话；未命中返回 ErrNotFound。
	DeleteUserSession(ctx context.Context, userID int64, deviceType string) error
	// DeleteAllUserSessions 删除该用户全部设备会话（重置密码用）。
	DeleteAllUserSessions(ctx context.Context, userID int64) error

	// roles / member_roles / channel_overwrites / audit_log（迁移 0010，#35 B1）。
	// 「不属于该服务器」与「不存在」统一返回 ErrNotFound，不泄露跨服资源是否存在。

	// ListRoles 按 position、id 升序。
	ListRoles(ctx context.Context, serverID int64) ([]Role, error)
	RoleByID(ctx context.Context, id int64) (*Role, error)
	// CreateRole 仅自定义角色：builtin_key 必须空，否则 ErrForbidden；
	// 未定义权限位拒绝入参。ID 未预置时取 Snowflake。
	CreateRole(ctx context.Context, r *Role) error
	// UpdateRole 全量写 name/color/position/permissions；builtin 返回 ErrForbidden。
	UpdateRole(ctx context.Context, r *Role) error
	// DeleteRole builtin 返回 ErrForbidden；级联删除其分配与以它为目标的频道覆盖。
	DeleteRole(ctx context.Context, id int64) error
	// RolesOfMember 成员的有效角色（含 @everyone，即使分配行缺失也强制带上），
	// 按 position、id 升序；成员不存在返回 ErrNotFound。
	RolesOfMember(ctx context.Context, serverID, userID int64) ([]Role, error)
	// SetMemberRoles 事务替换该成员的分配：roleIDs 去重后逐个校验同服；
	// 非 owner 不得持有 owner builtin（含经 roleIDs 自授，返回 ErrForbidden）；
	// 强制保留 @everyone，server owner（或 legacy role=owner）强制保留 owner builtin；
	// 同步 server_members.role 投影（有 owner 角色→owner；有 admin→admin；否则 member）。
	SetMemberRoles(ctx context.Context, serverID, userID int64, roleIDs []int64, assignedBy int64) error
	// RoleAssignmentsOfServer 全服分配，按 user_id、role_id 升序。
	RoleAssignmentsOfServer(ctx context.Context, serverID int64) ([]MemberRoleAssignment, error)
	// ChannelOverwrites 按 target_type、target_id 升序。
	ChannelOverwrites(ctx context.Context, channelID int64) ([]ChannelOverwrite, error)
	// ChannelOverwritesOfServer 经 channels 关联取全服覆盖，按 channel_id、target_type、target_id 升序。
	ChannelOverwritesOfServer(ctx context.Context, serverID int64) ([]ChannelOverwrite, error)
	// SetChannelOverwrite upsert；校验 target_type 合法、Allow/Deny 在频道掩码内、
	// 频道存在、目标（角色/成员）同服。
	SetChannelOverwrite(ctx context.Context, o *ChannelOverwrite) error
	// DeleteChannelOverwrite 删除指定目标的覆盖；未命中返回 ErrNotFound。
	DeleteChannelOverwrite(ctx context.Context, channelID int64, targetType OverwriteTargetType, targetID int64) error
	// WriteAudit 落审计行，回填 ID（未预置时 Snowflake）与 CreatedAt。
	WriteAudit(ctx context.Context, a *AuditLog) error
	// AuditLogBefore 倒序游标（新→旧返回）：beforeID=0 取最新 limit 条；
	// limit 越界回落 50（与 MessagesBefore 同口径）。
	AuditLogBefore(ctx context.Context, serverID, beforeID int64, limit int) ([]AuditLog, error)
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
