package authz

import (
	"time"

	"github.com/OpenALC/LiteTalk/server/internal/store"
)

// serverPolicy 一个服务器的权限快照：加载完成后不可变，可无锁并发读。
// 权限判断全程只读快照，Store 访问集中在 loadFromStore。
type serverPolicy struct {
	ownerID int64
	roles   map[int64]store.Role
	// builtin 三值 member/admin/owner → 角色 id；0 表示缺（legacy 数据）。
	builtin map[string]int64
	// memberRoles uid → 角色 id 列表（含 @everyone，B1 不变量）。
	memberRoles map[int64][]int64
	// members 键存在即成员；值带 legacy role——角色表缺失时回退静态模板要用它。
	// （规范草案写的是 map[int64]bool，这里换成 MemberRole 只为少一次查询。）
	members    map[int64]store.MemberRole
	overwrites map[int64][]store.ChannelOverwrite
	loadedAt   time.Time
}

// isMember 成员判断（IsMember 用；owner 不在此旁路，成员资格只看成员表）。
func (s *serverPolicy) isMember(uid int64) bool {
	_, ok := s.members[uid]
	return ok
}

// basePermissions 角色并集（含 @everyone）。角色表缺 @everyone、或该成员无分配、
// 或分配的角色全部失联时，回退 server_members.role 的静态模板（legacy 数据兼容）。
// 任何缺失路径都不允许 panic。
func (s *serverPolicy) basePermissions(uid int64) store.Permission {
	if s.builtin[store.BuiltinKeyMember] == 0 {
		return staticTemplate(s.members[uid])
	}
	ids := s.memberRoles[uid]
	var p store.Permission
	resolved := 0
	for _, id := range ids {
		if r, ok := s.roles[id]; ok {
			p |= r.Permissions
			resolved++
		}
	}
	if resolved == 0 {
		return staticTemplate(s.members[uid])
	}
	return p
}

// staticTemplate legacy 静态模板：owner/admin 全开，member 默认频道/语音位。
// 与 store.BuiltinRoleTemplates 的 owner/admin=32767 同源。
func staticTemplate(legacy store.MemberRole) store.Permission {
	switch legacy {
	case store.RoleOwner, store.RoleAdmin:
		return store.AllPermissions
	default:
		return store.DefaultMemberPerms
	}
}

// mergeChannel 频道级有效权限（纯函数，独立可测）。
// 分层由宽到窄：@everyone 覆盖 → 成员其它角色覆盖（OR 合并为一层）→ 成员个人覆盖；
// 每层 (p | allow) &^ deny：同层 deny 胜，更窄层可翻转更宽层的 deny。
// 只有 ChannelPermissionMask 内的位参与覆盖；管理位永远来自 base（角色并集），
// 覆盖里混进管理位一律忽略（B1 已拒绝写入，这里防的是历史脏数据）。
// roleIDs 只含成员持有的角色——没持有的角色其覆盖不得生效。
func mergeChannel(base store.Permission, uid, everyoneRoleID int64, roleIDs []int64, overwrites []store.ChannelOverwrite) store.Permission {
	inRoles := make(map[int64]bool, len(roleIDs))
	for _, id := range roleIDs {
		if id != everyoneRoleID {
			inRoles[id] = true
		}
	}
	var everyoneAllow, everyoneDeny, roleAllow, roleDeny, memberAllow, memberDeny store.Permission
	for _, o := range overwrites {
		allow := o.Allow & store.ChannelPermissionMask
		deny := o.Deny & store.ChannelPermissionMask
		switch {
		case o.TargetType == store.OverwriteTargetMember && o.TargetID == uid:
			memberAllow |= allow
			memberDeny |= deny
		case o.TargetType == store.OverwriteTargetRole && everyoneRoleID != 0 && o.TargetID == everyoneRoleID:
			everyoneAllow |= allow
			everyoneDeny |= deny
		case o.TargetType == store.OverwriteTargetRole && inRoles[o.TargetID]:
			roleAllow |= allow
			roleDeny |= deny
		}
	}
	p := (base | everyoneAllow) &^ everyoneDeny
	p = (p | roleAllow) &^ roleDeny
	p = (p | memberAllow) &^ memberDeny
	return p
}

// effective 判定 uid 对 perm 的有效权限（纯合并，不接触 Store）。
// owner 旁路先于成员判断：owner_id 是归属权威，成员行缺失时不能把服主锁死在门外。
// 非成员（非 owner）false：由 handler 统一 403。
func (s *serverPolicy) effective(uid, channelID int64, perm store.Permission) bool {
	if s.ownerID == uid {
		return true
	}
	if !s.isMember(uid) {
		return false
	}
	base := s.basePermissions(uid)
	if base.Has(store.Administrator) {
		return true
	}
	if channelID == 0 {
		return base.Has(perm)
	}
	p := mergeChannel(base, uid, s.builtin[store.BuiltinKeyMember], s.memberRoles[uid], s.overwrites[channelID])
	return p.Has(perm)
}
