package authz

import (
	"testing"

	"github.com/OpenALC/LiteTalk/server/internal/store"
)

// TestStaticTemplate legacy 静态模板：owner/admin 全开、member 默认位。
func TestStaticTemplate(t *testing.T) {
	if staticTemplate(store.RoleOwner) != store.AllPermissions {
		t.Fatal("owner 模板应 AllPermissions")
	}
	if staticTemplate(store.RoleAdmin) != store.AllPermissions {
		t.Fatal("admin 模板应 AllPermissions")
	}
	if staticTemplate(store.RoleMember) != store.DefaultMemberPerms {
		t.Fatal("member 模板应 DefaultMemberPerms")
	}
	// 未知值按 member 收敛（fail-closed 到最小集）。
	if staticTemplate("guest") != store.DefaultMemberPerms {
		t.Fatal("未知 legacy 角色应按 member 处理")
	}
}

// TestMergeChannelLayers 分层由宽到窄：@everyone → 成员角色 → 个人；
// 更窄层可翻转更宽层的 deny。
func TestMergeChannelLayers(t *testing.T) {
	base := store.DefaultMemberPerms // 含 ViewChannel
	const uid, everyoneID, roleID = int64(7), int64(1), int64(2)

	// @everyone deny ViewChannel → 普通成员不可见。
	ows := []store.ChannelOverwrite{
		{TargetType: store.OverwriteTargetRole, TargetID: everyoneID, Deny: store.ViewChannel},
	}
	if p := mergeChannel(base, uid, everyoneID, nil, ows); p.Has(store.ViewChannel) {
		t.Fatal("@everyone deny 应生效")
	}
	// 成员角色 allow 可覆盖 @everyone deny。
	ows = append(ows, store.ChannelOverwrite{
		TargetType: store.OverwriteTargetRole, TargetID: roleID, Allow: store.ViewChannel,
	})
	p := mergeChannel(base, uid, everyoneID, []int64{everyoneID, roleID}, ows)
	if !p.Has(store.ViewChannel) {
		t.Fatal("角色 allow 应覆盖 @everyone deny")
	}
	// 成员个人 deny 再覆盖角色 allow。
	ows = append(ows, store.ChannelOverwrite{
		TargetType: store.OverwriteTargetMember, TargetID: uid, Deny: store.ViewChannel,
	})
	p = mergeChannel(base, uid, everyoneID, []int64{everyoneID, roleID}, ows)
	if p.Has(store.ViewChannel) {
		t.Fatal("个人 deny 应覆盖角色 allow")
	}
	// 个人 allow 可翻转上层 deny（个人层此时只有 allow）。
	owsFlip := append(ows[:2:2], store.ChannelOverwrite{
		TargetType: store.OverwriteTargetMember, TargetID: uid, Allow: store.ViewChannel})
	if p = mergeChannel(base, uid, everyoneID, []int64{everyoneID, roleID}, owsFlip); !p.Has(store.ViewChannel) {
		t.Fatal("个人 allow 应覆盖 @everyone deny")
	}
	// 个人层自己 allow+deny 并存：deny 胜。
	owsBoth := append(owsFlip, store.ChannelOverwrite{
		TargetType: store.OverwriteTargetMember, TargetID: uid, Deny: store.ViewChannel})
	if p = mergeChannel(base, uid, everyoneID, []int64{everyoneID, roleID}, owsBoth); p.Has(store.ViewChannel) {
		t.Fatal("同层 allow+deny 应 deny 胜")
	}
}

// TestMergeChannelSameLayerDenyWins 同层 allow 与 deny 并存时 deny 胜。
func TestMergeChannelSameLayerDenyWins(t *testing.T) {
	base := store.DefaultMemberPerms
	const uid, everyoneID = int64(7), int64(1)
	ows := []store.ChannelOverwrite{
		{TargetType: store.OverwriteTargetRole, TargetID: everyoneID, Allow: store.Speak, Deny: store.Speak},
	}
	if p := mergeChannel(base, uid, everyoneID, nil, ows); p.Has(store.Speak) {
		t.Fatal("同层 deny 应胜过 allow")
	}
	// 角色层同理。
	ows = []store.ChannelOverwrite{
		{TargetType: store.OverwriteTargetRole, TargetID: 9, Allow: store.Speak, Deny: store.Speak},
	}
	if p := mergeChannel(base, uid, everyoneID, []int64{9}, ows); p.Has(store.Speak) {
		t.Fatal("角色层 deny 应胜过 allow")
	}
}

// TestMergeChannelIgnoresManagementBits 覆盖里的管理位不生效：
// allow 不能凭覆盖发管理位，deny 不能用覆盖砍角色并集里的管理位。
func TestMergeChannelIgnoresManagementBits(t *testing.T) {
	const uid, everyoneID = int64(7), int64(1)
	// allow 混入管理位：base 没有的位不能出现。
	ows := []store.ChannelOverwrite{
		{TargetType: store.OverwriteTargetMember, TargetID: uid,
			Allow: store.ManageRoles | store.Administrator | store.ManageServer},
	}
	if p := mergeChannel(store.DefaultMemberPerms, uid, everyoneID, nil, ows); p.Has(store.ManageRoles) {
		t.Fatal("覆盖 allow 不得发放管理位")
	}
	// deny 混入管理位：base 里的管理位不能被砍。
	if p := mergeChannel(store.AllPermissions, uid, everyoneID, nil, ows); !p.Has(store.Administrator) {
		t.Fatal("覆盖 deny 不得砍掉管理位")
	}
}

// TestMergeChannelUnheldRoleIgnored 没持有的角色，其覆盖不得生效。
func TestMergeChannelUnheldRoleIgnored(t *testing.T) {
	base := store.DefaultMemberPerms
	const uid, everyoneID, heldID, unheldID = int64(7), int64(1), int64(2), int64(3)
	ows := []store.ChannelOverwrite{
		{TargetType: store.OverwriteTargetRole, TargetID: unheldID, Deny: store.ViewChannel},
		{TargetType: store.OverwriteTargetRole, TargetID: heldID, Allow: store.PrioritySpeaker},
	}
	p := mergeChannel(base, uid, everyoneID, []int64{everyoneID, heldID}, ows)
	if !p.Has(store.ViewChannel) {
		t.Fatal("未持有角色的 deny 不得生效")
	}
	if !p.Has(store.PrioritySpeaker) {
		t.Fatal("持有角色的 allow 应生效")
	}
	// 成员个人覆盖不受角色持有与否影响。
	ows = append(ows, store.ChannelOverwrite{
		TargetType: store.OverwriteTargetMember, TargetID: uid, Deny: store.ViewChannel})
	p = mergeChannel(base, uid, everyoneID, []int64{everyoneID, heldID}, ows)
	if p.Has(store.ViewChannel) {
		t.Fatal("个人 deny 应生效")
	}
}

// TestMergeChannelPriorityDenyVsGrant 优先发言授予/撤销的位组合语义（#117）：
// 个人 deny 会让「授予」失效（同层 deny 胜），所以授予必须同时清 deny；
// 撤销只清个人 allow 位时，角色层的授予仍然生效（个人层无位可删）。
func TestMergeChannelPriorityDenyVsGrant(t *testing.T) {
	base := store.DefaultMemberPerms // 不含 PrioritySpeaker
	const uid, everyoneID, roleID = int64(7), int64(1), int64(2)
	roleGrant := store.ChannelOverwrite{
		TargetType: store.OverwriteTargetRole, TargetID: roleID, Allow: store.PrioritySpeaker,
	}
	held := []int64{everyoneID, roleID}

	// 个人 deny + 授权 allow（客户端只补 allow 的旧行为）→ deny 胜，授予无效。
	ows := []store.ChannelOverwrite{
		roleGrant,
		{TargetType: store.OverwriteTargetMember, TargetID: uid, Allow: store.PrioritySpeaker, Deny: store.PrioritySpeaker},
	}
	if p := mergeChannel(base, uid, everyoneID, held, ows); p.Has(store.PrioritySpeaker) {
		t.Fatal("个人 deny 与 allow 并存时 deny 胜，授予不应生效")
	}
	// 授予清掉 personal deny（#117 修复后的位组合）→ 动作重放：角色 allow 生效。
	ows = []store.ChannelOverwrite{
		roleGrant,
		{TargetType: store.OverwriteTargetMember, TargetID: uid, Allow: store.PrioritySpeaker},
	}
	if p := mergeChannel(base, uid, everyoneID, held, ows); !p.Has(store.PrioritySpeaker) {
		t.Fatal("清掉个人 deny 后角色授予应生效")
	}
	// 撤销：只删个人 allow 位，角色 allow 仍在 → 仍然生效（客户端据此提示无法撤销）。
	ows = []store.ChannelOverwrite{roleGrant}
	if p := mergeChannel(base, uid, everyoneID, held, ows); !p.Has(store.PrioritySpeaker) {
		t.Fatal("角色授予在个人位撤销后应保持生效")
	}
	// 没有角色授予时，撤销即失效。
	ows = []store.ChannelOverwrite{
		{TargetType: store.OverwriteTargetRole, TargetID: roleID},
	}
	if p := mergeChannel(base, uid, everyoneID, held, ows); p.Has(store.PrioritySpeaker) {
		t.Fatal("无角色授予时个人位撤销应使权限失效")
	}
}

// TestBasePermissionsFallback 角色数据缺失时回退静态模板，任何路径不许 panic。
func TestBasePermissionsFallback(t *testing.T) {
	const uid = int64(7)
	full := &serverPolicy{
		builtin:     map[string]int64{store.BuiltinKeyMember: 1},
		memberRoles: map[int64][]int64{},
		members:     map[int64]store.MemberRole{uid: store.RoleAdmin},
		roles:       map[int64]store.Role{},
	}
	// 有 @everyone 但该成员无分配 → 模板（admin=全开）。
	if p := full.basePermissions(uid); p != store.AllPermissions {
		t.Fatalf("空分配应走模板，实际 %d", p)
	}
	// 角色表缺 @everyone → 整体模板模式。
	noEveryone := &serverPolicy{
		builtin:     map[string]int64{},
		memberRoles: map[int64][]int64{uid: {1, 2}},
		members:     map[int64]store.MemberRole{uid: store.RoleMember},
		roles:       map[int64]store.Role{1: {ID: 1, Permissions: store.KickMembers}},
	}
	if p := noEveryone.basePermissions(uid); p != store.DefaultMemberPerms {
		t.Fatalf("缺 @everyone 应走模板，实际 %d", p)
	}
	// 分配存在但角色行全部失联 → 模板。
	broken := &serverPolicy{
		builtin:     map[string]int64{store.BuiltinKeyMember: 1},
		memberRoles: map[int64][]int64{uid: {42}},
		members:     map[int64]store.MemberRole{uid: store.RoleMember},
		roles:       map[int64]store.Role{},
	}
	if p := broken.basePermissions(uid); p != store.DefaultMemberPerms {
		t.Fatalf("分配失联应走模板，实际 %d", p)
	}
	// 正常并集：@everyone + 自定义。
	ok := &serverPolicy{
		builtin:     map[string]int64{store.BuiltinKeyMember: 1},
		memberRoles: map[int64][]int64{uid: {1, 2}},
		members:     map[int64]store.MemberRole{uid: store.RoleMember},
		roles: map[int64]store.Role{
			1: {ID: 1, Permissions: store.DefaultMemberPerms},
			2: {ID: 2, Permissions: store.MuteMembers},
		},
	}
	if p := ok.basePermissions(uid); !p.Has(store.MuteMembers) || !p.Has(store.Speak) {
		t.Fatalf("正常并集缺失位: %d", p)
	}
	// 未知成员也走模板（member=DefaultMemberPerms），不 panic。
	if p := ok.basePermissions(999); p != store.DefaultMemberPerms {
		t.Fatalf("未知成员应回退 member 模板，实际 %d", p)
	}
}
