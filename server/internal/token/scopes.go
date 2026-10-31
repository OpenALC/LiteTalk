package token

import (
	"github.com/OpenALC/LiteTalk/server/internal/store"
)

// scope 白名单（批次 6 §0.2）：对外契约。字符串一旦发布不可改名、不可改语义，
// 只能新增——改名等于破坏所有已签发令牌的兼容。
const (
	ScopeMessagesRead     = "messages.read"
	ScopeMessagesWrite    = "messages.write"
	ScopeAttachmentsWrite = "attachments.write"
	ScopeVoiceConnect     = "voice.connect"
	ScopeGuildManage      = "guild.manage"
	ScopeAdminRead        = "admin.read"
)

// knownScopes scope → 权限位映射。**scope 只是上限，绝不是授予**：最终权限 =
// Authorizer 算出的角色权限 ∩ 此处映射位（authz.CanInServer 集中取交集），
// 角色被降级后令牌自动失去对应权限。
var knownScopes = map[string]store.Permission{
	ScopeMessagesRead:     store.ViewChannel,
	ScopeMessagesWrite:    store.ViewChannel | store.SendMessages,
	ScopeAttachmentsWrite: store.AttachFiles,
	ScopeVoiceConnect:     store.ViewChannel | store.ConnectVoice | store.Speak,
	ScopeGuildManage:      store.ManageChannels | store.ManageRoles | store.ManageServer,
	ScopeAdminRead:        store.ManageServer,
}

// IsKnownScope 是否在白名单内（创建端拒绝未知值与空列表）。
func IsKnownScope(name string) bool {
	_, ok := knownScopes[name]
	return ok
}

// ValidScopes 创建入口的 scope 校验：至少一个、全部在白名单内、无重复。
func ValidScopes(scopes []string) bool {
	if len(scopes) == 0 {
		return false
	}
	seen := make(map[string]bool, len(scopes))
	for _, s := range scopes {
		if !IsKnownScope(s) || seen[s] {
			return false
		}
		seen[s] = true
	}
	return true
}

// ScopeBits scope 列表映射出的权限位并集。未知 scope 贡献 0 位——创建端已拒绝，
// 这里兜底不放大权限。
func ScopeBits(scopes []string) store.Permission {
	var out store.Permission
	for _, s := range scopes {
		out |= knownScopes[s]
	}
	return out
}
