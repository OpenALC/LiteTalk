package store

import "testing"

// TestPermissionBits 位值即协议：位序或常量值漂移等于换协议，必须钉死。
func TestPermissionBits(t *testing.T) {
	cases := []struct {
		p    Permission
		want uint64
	}{
		{ViewChannel, 1}, {SendMessages, 2}, {ConnectVoice, 4}, {Speak, 8},
		{PrioritySpeaker, 16}, {MuteMembers, 32}, {MoveMembers, 64}, {KickMembers, 128},
		{BanMembers, 256}, {ManageChannels, 512}, {ManageRoles, 1024}, {ManageServer, 2048},
		{MentionEveryone, 4096}, {AttachFiles, 8192}, {Administrator, 16384},
		{AllPermissions, 32767},
		{ChannelPermissionMask, 12319},
		{DefaultMemberPerms, 8207},
	}
	for _, c := range cases {
		if uint64(c.p) != c.want {
			t.Fatalf("权限位 %d 应为 %d，实际 %d", c.p, c.want, uint64(c.p))
		}
	}
}

// TestPermissionMask 组合值与 Has 语义。
func TestPermissionMask(t *testing.T) {
	const mask = ChannelPermissionMask
	if mask != ViewChannel|SendMessages|ConnectVoice|Speak|PrioritySpeaker|MentionEveryone|AttachFiles {
		t.Fatalf("ChannelPermissionMask 与位并集不一致: %d", mask)
	}
	if mask.Has(ManageRoles) || mask.Has(Administrator) {
		t.Fatal("频道掩码不得含管理类位")
	}
	if !mask.Has(ViewChannel) || !mask.Has(AttachFiles) {
		t.Fatal("频道掩码应含 ViewChannel/AttachFiles")
	}
	// Has 要求 flag 的全部位都在。
	if DefaultMemberPerms.Has(Speak|AttachFiles) != true {
		t.Fatal("DefaultMemberPerms 应含 Speak+AttachFiles")
	}
	if DefaultMemberPerms.Has(Speak | PrioritySpeaker) {
		t.Fatal("DefaultMemberPerms 不含 PrioritySpeaker")
	}
}

// TestPermissionValid 未定义位拒绝入参。
func TestPermissionValid(t *testing.T) {
	if !AllPermissions.Valid() {
		t.Fatal("AllPermissions 应合法")
	}
	if !Permission(0).Valid() {
		t.Fatal("空权限应合法")
	}
	for _, p := range []Permission{AllPermissions + 1, 1 << 15, 1 << 63, ^Permission(0)} {
		if p.Valid() {
			t.Fatalf("带未定义位应拒绝: %d", uint64(p))
		}
	}
}

// TestBuiltinRoleTemplates 默认模板三件套：CreateServer 与迁移 0010 回填同源。
func TestBuiltinRoleTemplates(t *testing.T) {
	if len(BuiltinRoleTemplates) != 3 {
		t.Fatalf("builtin 模板应恰 3 条，实际 %d", len(BuiltinRoleTemplates))
	}
	want := []RoleTemplate{
		{BuiltinKey: "member", Name: "@everyone", Position: 0, Permissions: 8207},
		{BuiltinKey: "admin", Name: "管理员", Position: 1, Permissions: 32767},
		{BuiltinKey: "owner", Name: "服主", Position: 2, Permissions: 32767},
	}
	for i, w := range want {
		got := BuiltinRoleTemplates[i]
		if got != w {
			t.Fatalf("模板[%d] = %+v，期望 %+v", i, got, w)
		}
	}
	if DefaultRoleColor != "#4FA4F3" {
		t.Fatalf("默认角色色应为主题主色，实际 %s", DefaultRoleColor)
	}
}
