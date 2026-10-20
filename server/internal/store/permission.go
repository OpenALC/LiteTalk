package store

// Permission 权限位（#35）。DB 列是有符号 BIGINT 但只用低 15 位：读库先扫 int64
// 再转 Permission，直接扫进 uint64 在值超 int63 时会被驱动按负数处理。
type Permission uint64

// 权限位定义（位序即协议位序，改值等于改协议）。
const (
	ViewChannel     Permission = 1 << 0  // 1
	SendMessages    Permission = 1 << 1  // 2
	ConnectVoice    Permission = 1 << 2  // 4
	Speak           Permission = 1 << 3  // 8
	PrioritySpeaker Permission = 1 << 4  // 16
	MuteMembers     Permission = 1 << 5  // 32
	MoveMembers     Permission = 1 << 6  // 64
	KickMembers     Permission = 1 << 7  // 128
	BanMembers      Permission = 1 << 8  // 256
	ManageChannels  Permission = 1 << 9  // 512
	ManageRoles     Permission = 1 << 10 // 1024
	ManageServer    Permission = 1 << 11 // 2048
	MentionEveryone Permission = 1 << 12 // 4096
	AttachFiles     Permission = 1 << 13 // 8192
	Administrator   Permission = 1 << 14 // 16384
)

// 权限组合。
const (
	// AllPermissions 低 15 位全开。
	AllPermissions Permission = 32767
	// ChannelPermissionMask 频道覆盖（allow/deny）只作用于这些位；管理类位只属于角色。
	ChannelPermissionMask Permission = ViewChannel | SendMessages | ConnectVoice | Speak |
		PrioritySpeaker | MentionEveryone | AttachFiles // 12319
	// DefaultMemberPerms @everyone 默认：进出、发言、传文件。
	DefaultMemberPerms Permission = ViewChannel | SendMessages | ConnectVoice | Speak | AttachFiles // 8207
)

// 内置角色 key（roles.builtin_key；自定义角色为空）。
const (
	BuiltinKeyMember = "member"
	BuiltinKeyAdmin  = "admin"
	BuiltinKeyOwner  = "owner"
)

// DefaultRoleColor 角色默认色（主题主色）。
const DefaultRoleColor = "#4FA4F3"

// RoleTemplate 内置角色模板。CreateServer 与迁移 0010 回填共用这组值，
// 两侧必须同源——改这里要同步迁移里的 INSERT 常量。
type RoleTemplate struct {
	BuiltinKey  string
	Name        string
	Position    int
	Permissions Permission
}

// BuiltinRoleTemplates 每个 server 固定三条 builtin：member/@everyone、admin/管理员、owner/服主。
var BuiltinRoleTemplates = [...]RoleTemplate{
	{BuiltinKey: BuiltinKeyMember, Name: "@everyone", Position: 0, Permissions: DefaultMemberPerms},
	{BuiltinKey: BuiltinKeyAdmin, Name: "管理员", Position: 1, Permissions: AllPermissions},
	{BuiltinKey: BuiltinKeyOwner, Name: "服主", Position: 2, Permissions: AllPermissions},
}

// Valid 未定义位拒绝：DB 只存低 15 位，带高位入参会与读回值不一致（写入被截断的假象）。
func (p Permission) Valid() bool { return p&^AllPermissions == 0 }

// Has 是否包含 flag 的全部位。
func (p Permission) Has(flag Permission) bool { return p&flag == flag }
