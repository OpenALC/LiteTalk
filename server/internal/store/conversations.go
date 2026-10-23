package store

import "fmt"

// 迁移 0012（#38 B1）共用的会话规范化逻辑。MySQL/SQLite/Mem 三实现都走这里，
// 避免「direct 的 dm_key 怎么排」「group 谁是 owner」在三种存储间漂移。

// isConversationScope 是否私聊/群私聊作用域。
func isConversationScope(s ChannelScope) bool {
	return s == ChannelScopeDirect || s == ChannelScopeGroup
}

// normalizeChannelScope 空串按 server 处理（旧调用方不传 scope 的兼容路径）。
func normalizeChannelScope(s ChannelScope) ChannelScope {
	if s == "" {
		return ChannelScopeServer
	}
	return s
}

// normalizeServerChannel 校验 CreateChannel 入参：只有 server 作用域走这里，
// 且必须有服务器；dm_key 对 server 频道无意义，强制清空。
func normalizeServerChannel(c *Channel) error {
	c.Scope = normalizeChannelScope(c.Scope)
	if c.Scope != ChannelScopeServer || c.ServerID == 0 {
		return ErrForbidden
	}
	c.DMKey = ""
	return nil
}

// normalizeConversation 校验 CreateConversation 入参并补齐会话行默认值：
// 无服务器、文字频道、不参与分类/子频道/密码/人数上限。memberIDs 去重保序后返回。
func normalizeConversation(c *Channel, memberIDs []int64) ([]int64, error) {
	c.Scope = normalizeChannelScope(c.Scope)
	if !isConversationScope(c.Scope) {
		return nil, ErrForbidden
	}
	ids := dedupeIDs(memberIDs)
	switch c.Scope {
	case ChannelScopeDirect:
		if len(ids) != 2 {
			return nil, ErrForbidden
		}
		lo, hi := ids[0], ids[1]
		if lo > hi {
			lo, hi = hi, lo
		}
		c.DMKey = directKey(lo, hi)
	case ChannelScopeGroup:
		if len(ids) == 0 {
			return nil, ErrForbidden
		}
		c.DMKey = ""
	}
	c.ServerID = 0
	c.Type = ChannelText
	c.CategoryID = nil
	c.ParentID = nil
	c.Position = 0
	c.UserLimit = 0
	c.PasswordHash = ""
	return ids, nil
}

// dedupeIDs 去重并保持调用方顺序；0 不是合法用户 ID，直接丢弃。
func dedupeIDs(ids []int64) []int64 {
	seen := make(map[int64]struct{}, len(ids))
	out := make([]int64, 0, len(ids))
	for _, id := range ids {
		if id == 0 {
			continue
		}
		if _, ok := seen[id]; ok {
			continue
		}
		seen[id] = struct{}{}
		out = append(out, id)
	}
	return out
}

// directKey 私聊唯一键：两个 uid 排序后拼接，保证 (a,b) 与 (b,a) 命中同一行。
func directKey(a, b int64) string {
	if a > b {
		a, b = b, a
	}
	return fmt.Sprintf("%d:%d", a, b)
}

// conversationMemberRole group 的首个成员是创建者，记 owner；direct 双方都是 member。
func conversationMemberRole(scope ChannelScope, index int) ChannelMemberRole {
	if scope == ChannelScopeGroup && index == 0 {
		return ChannelMemberOwner
	}
	return ChannelMemberMember
}

// normalizeChannelMemberRole 空串按 member；其余只认 owner/member。
func normalizeChannelMemberRole(role ChannelMemberRole) (ChannelMemberRole, error) {
	switch role {
	case "":
		return ChannelMemberMember, nil
	case ChannelMemberOwner, ChannelMemberMember:
		return role, nil
	default:
		return "", ErrForbidden
	}
}

// nullID 0 写 NULL（channels.server_id 对会话行为 NULL）。
func nullID(id int64) any {
	if id == 0 {
		return nil
	}
	return id
}

// nullString 空串写 NULL（channels.dm_key 对 server/group 行 NULL）。
func nullString(s string) any {
	if s == "" {
		return nil
	}
	return s
}
