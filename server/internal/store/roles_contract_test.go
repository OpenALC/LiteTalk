package store

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sync"
	"sync/atomic"
	"testing"

	mysqldrv "github.com/go-sql-driver/mysql"
)

// rolesTestDB MySQL 专用角色契约库：每次用例重建，避免与同包其它用例互相污染。
const rolesTestDB = "litetalk_roles_test"

// rolesSeq 保证同一二进制内用户/服务器的 email、name 唯一。
var rolesSeq atomic.Int64

// seedRolesServer 建 3 个用户 + 1 服 + owner/admin/member 三成员（各子用例独立数据）。
func seedRolesServer(t *testing.T, st Store) (srv *Server, owner, admin, member *User) {
	t.Helper()
	ctx := context.Background()
	n := rolesSeq.Add(1)
	mk := func(kind string) *User {
		u := &User{
			Email:        fmt.Sprintf("roles-%d-%s@ex.test", n, kind),
			Nickname:     fmt.Sprintf("r%d%s", n%100000, kind),
			Tag:          "0001",
			PasswordHash: "h",
		}
		if err := st.CreateUser(ctx, u); err != nil {
			t.Fatalf("CreateUser(%s): %v", kind, err)
		}
		return u
	}
	owner, admin, member = mk("o"), mk("a"), mk("m")
	srv = &Server{Name: fmt.Sprintf("roles-%d", n), OwnerID: owner.ID, IconName: "music", IconColor: "#4FA4F3"}
	if err := st.CreateServer(ctx, srv); err != nil {
		t.Fatalf("CreateServer: %v", err)
	}
	if err := st.AddMember(ctx, &ServerMember{ServerID: srv.ID, UserID: owner.ID, Role: RoleOwner}); err != nil {
		t.Fatalf("AddMember(owner): %v", err)
	}
	if err := st.AddMember(ctx, &ServerMember{ServerID: srv.ID, UserID: admin.ID, Role: RoleAdmin}); err != nil {
		t.Fatalf("AddMember(admin): %v", err)
	}
	if err := st.AddMember(ctx, &ServerMember{ServerID: srv.ID, UserID: member.ID, Role: RoleMember}); err != nil {
		t.Fatalf("AddMember(member): %v", err)
	}
	return srv, owner, admin, member
}

// wantKeys 取分配里出现的 builtin key，排序后比较（含自定义时传 customNames）。
func roleBuiltinKeys(t *testing.T, st Store, serverID, userID int64) []string {
	t.Helper()
	list, err := st.RolesOfMember(context.Background(), serverID, userID)
	if err != nil {
		t.Fatalf("RolesOfMember: %v", err)
	}
	out := []string{}
	for _, r := range list {
		if r.IsBuiltin() {
			out = append(out, r.BuiltinKey)
		}
	}
	return out
}

// runRolesContract 三实现共用的角色/权限契约（#35 B1）。
func runRolesContract(t *testing.T, st Store) {
	t.Helper()
	ctx := context.Background()

	t.Run("建服自动三条 builtin", func(t *testing.T) {
		srv, _, _, _ := seedRolesServer(t, st)
		list, err := st.ListRoles(ctx, srv.ID)
		if err != nil {
			t.Fatalf("ListRoles: %v", err)
		}
		if len(list) != 3 {
			t.Fatalf("应恰 3 条 builtin，实际 %d: %+v", len(list), list)
		}
		// 顺序按 position：member/admin/owner。
		want := []RoleTemplate{
			{BuiltinKey: BuiltinKeyMember, Name: "@everyone", Position: 0, Permissions: DefaultMemberPerms},
			{BuiltinKey: BuiltinKeyAdmin, Name: "管理员", Position: 1, Permissions: AllPermissions},
			{BuiltinKey: BuiltinKeyOwner, Name: "服主", Position: 2, Permissions: AllPermissions},
		}
		for i, w := range want {
			got := list[i]
			if got.BuiltinKey != w.BuiltinKey || got.Name != w.Name ||
				got.Position != w.Position || got.Permissions != w.Permissions {
				t.Fatalf("builtin[%d] = %+v，期望 %+v", i, got, w)
			}
			if got.Color != DefaultRoleColor {
				t.Fatalf("builtin[%d] 颜色应为 %s，实际 %s", i, DefaultRoleColor, got.Color)
			}
			if got.ID == 0 || got.CreatedAt.IsZero() || got.UpdatedAt.IsZero() {
				t.Fatalf("builtin[%d] 应有 id/时间: %+v", i, got)
			}
		}
	})

	t.Run("入会自动默认角色", func(t *testing.T) {
		srv, owner, admin, member := seedRolesServer(t, st)
		for _, c := range []struct {
			u    *User
			want []string
		}{
			{member, []string{BuiltinKeyMember}},
			{admin, []string{BuiltinKeyAdmin, BuiltinKeyMember}},
			{owner, []string{BuiltinKeyMember, BuiltinKeyOwner}},
		} {
			got := roleBuiltinKeys(t, st, srv.ID, c.u.ID)
			set := map[string]bool{}
			for _, k := range got {
				set[k] = true
			}
			if len(set) != len(c.want) {
				t.Fatalf("用户 %d 角色集 %v，期望 %v", c.u.ID, got, c.want)
			}
			for _, k := range c.want {
				if !set[k] {
					t.Fatalf("用户 %d 缺角色 %s：实际 %v", c.u.ID, k, got)
				}
			}
			// 分配行必须真实存在（RolesOfMember 会兜底 @everyone）。
			as, err := st.RoleAssignmentsOfServer(ctx, srv.ID)
			if err != nil {
				t.Fatalf("RoleAssignmentsOfServer: %v", err)
			}
			n := 0
			for _, a := range as {
				if a.UserID == c.u.ID {
					n++
				}
			}
			if n != len(c.want) {
				t.Fatalf("用户 %d 分配行应 %d 条，实际 %d", c.u.ID, len(c.want), n)
			}
		}
	})

	t.Run("邀请入会带默认角色且冲突不消耗不留分配", func(t *testing.T) {
		srv, owner, _, _ := seedRolesServer(t, st)
		u := &User{Email: fmt.Sprintf("join-%d@ex.test", rolesSeq.Add(1)), Nickname: "joiner", Tag: "0001", PasswordHash: "h"}
		if err := st.CreateUser(ctx, u); err != nil {
			t.Fatalf("CreateUser: %v", err)
		}
		code := fmt.Sprintf("inv-%d", rolesSeq.Add(1))
		if err := st.CreateInvite(ctx, &Invite{Code: code, ServerID: srv.ID, CreatorID: owner.ID, MaxUses: 0}); err != nil {
			t.Fatalf("CreateInvite: %v", err)
		}
		if _, err := st.UseInviteAndAddMember(ctx, code, u.ID, RoleAdmin); err != nil {
			t.Fatalf("UseInviteAndAddMember: %v", err)
		}
		got := roleBuiltinKeys(t, st, srv.ID, u.ID)
		set := map[string]bool{}
		for _, k := range got {
			set[k] = true
		}
		if !set[BuiltinKeyMember] || !set[BuiltinKeyAdmin] || len(set) != 2 {
			t.Fatalf("邀请入会（admin）应有 everyone+admin，实际 %v", got)
		}
		// 同一用户二次加入：ErrConflict，不增 uses、不添 owner 分配。
		if _, err := st.UseInviteAndAddMember(ctx, code, u.ID, RoleOwner); !errors.Is(err, ErrConflict) {
			t.Fatalf("重复加入应 ErrConflict，实际 %v", err)
		}
		inv, err := st.InviteByCode(ctx, code)
		if err != nil || inv.Uses != 1 {
			t.Fatalf("冲突不得消耗邀请码: uses=%v err=%v", inv, err)
		}
		got = roleBuiltinKeys(t, st, srv.ID, u.ID)
		for _, k := range got {
			if k == BuiltinKeyOwner {
				t.Fatalf("冲突路径不得留下 owner 分配: %v", got)
			}
		}
	})

	t.Run("自定义角色 CRUD 与排序", func(t *testing.T) {
		srv, _, _, _ := seedRolesServer(t, st)
		r5 := &Role{ServerID: srv.ID, Name: "甲", Color: "#111111", Position: 5, Permissions: SendMessages | Speak}
		if err := st.CreateRole(ctx, r5); err != nil {
			t.Fatalf("CreateRole(r5): %v", err)
		}
		if r5.ID == 0 || r5.CreatedAt.IsZero() || r5.BuiltinKey != "" {
			t.Fatalf("创建后应有 id/时间且非 builtin: %+v", r5)
		}
		r3 := &Role{ServerID: srv.ID, Name: "乙", Position: 3, Permissions: ViewChannel}
		if err := st.CreateRole(ctx, r3); err != nil {
			t.Fatalf("CreateRole(r3): %v", err)
		}
		if r3.Color != DefaultRoleColor {
			t.Fatalf("空色应回落默认色，实际 %s", r3.Color)
		}
		list, err := st.ListRoles(ctx, srv.ID)
		if err != nil || len(list) != 5 {
			t.Fatalf("ListRoles: %v %v", list, err)
		}
		// position 升序：0/1/2 builtin 后是 3、5。
		if list[3].ID != r3.ID || list[4].ID != r5.ID {
			t.Fatalf("排序应按 position: %+v", list)
		}
		// 更新全量写。
		r3.Name, r3.Color, r3.Position, r3.Permissions = "丙", "#222222", 4, ManageRoles|SendMessages
		if err := st.UpdateRole(ctx, r3); err != nil {
			t.Fatalf("UpdateRole: %v", err)
		}
		got, err := st.RoleByID(ctx, r3.ID)
		if err != nil || got.Name != "丙" || got.Color != "#222222" || got.Position != 4 ||
			got.Permissions != ManageRoles|SendMessages {
			t.Fatalf("更新后读回不一致: %+v %v", got, err)
		}
		// 删除自定义角色。
		if err := st.DeleteRole(ctx, r5.ID); err != nil {
			t.Fatalf("DeleteRole: %v", err)
		}
		if _, err := st.RoleByID(ctx, r5.ID); !errors.Is(err, ErrNotFound) {
			t.Fatalf("删除后应 ErrNotFound，实际 %v", err)
		}
	})

	t.Run("SetMemberRoles 替换与强制保留", func(t *testing.T) {
		srv, owner, admin, member := seedRolesServer(t, st)
		rA := &Role{ServerID: srv.ID, Name: "A", Position: 9, Permissions: Speak}
		rB := &Role{ServerID: srv.ID, Name: "B", Position: 8, Permissions: SendMessages}
		if err := st.CreateRole(ctx, rA); err != nil {
			t.Fatalf("CreateRole(A): %v", err)
		}
		if err := st.CreateRole(ctx, rB); err != nil {
			t.Fatalf("CreateRole(B): %v", err)
		}
		roles, err := st.ListRoles(ctx, srv.ID)
		if err != nil {
			t.Fatal(err)
		}
		var adminID, ownerID int64
		for _, r := range roles {
			switch r.BuiltinKey {
			case BuiltinKeyAdmin:
				adminID = r.ID
			case BuiltinKeyOwner:
				ownerID = r.ID
			}
		}

		// 替换：member 原只有 @everyone，换为 [B] 后仍是 everyone+B。
		if err := st.SetMemberRoles(ctx, srv.ID, member.ID, []int64{rB.ID}, admin.ID); err != nil {
			t.Fatalf("SetMemberRoles: %v", err)
		}
		set := map[string]bool{}
		names := []string{}
		list, err := st.RolesOfMember(ctx, srv.ID, member.ID)
		if err != nil {
			t.Fatal(err)
		}
		for _, r := range list {
			set[r.BuiltinKey] = true
			names = append(names, r.Name)
		}
		if !set[BuiltinKeyMember] {
			t.Fatalf("@everyone 强制保留失效: %v", names)
		}
		if len(names) != 2 { // @everyone + B
			t.Fatalf("替换后应 2 条，实际 %v", names)
		}

		// 投影：member 仍为 member。
		mem, err := st.Member(ctx, srv.ID, member.ID)
		if err != nil || mem.Role != RoleMember {
			t.Fatalf("投影应 member: %+v %v", mem, err)
		}

		// 分配 assignedBy 落库。
		as, err := st.RoleAssignmentsOfServer(ctx, srv.ID)
		if err != nil {
			t.Fatal(err)
		}
		for _, a := range as {
			if a.UserID == member.ID && a.RoleID == rB.ID && a.AssignedBy != admin.ID {
				t.Fatalf("assignedBy 应为 %d，实际 %d", admin.ID, a.AssignedBy)
			}
		}

		// 有 admin 角色 → 投影 admin。
		if err := st.SetMemberRoles(ctx, srv.ID, member.ID, []int64{adminID, rA.ID}, owner.ID); err != nil {
			t.Fatalf("SetMemberRoles(admin): %v", err)
		}
		mem, err = st.Member(ctx, srv.ID, member.ID)
		if err != nil || mem.Role != RoleAdmin {
			t.Fatalf("投影应 admin: %+v %v", mem, err)
		}
		// 摘掉 admin → 回落 member。
		if err := st.SetMemberRoles(ctx, srv.ID, member.ID, []int64{rA.ID}, owner.ID); err != nil {
			t.Fatalf("SetMemberRoles(去 admin): %v", err)
		}
		mem, err = st.Member(ctx, srv.ID, member.ID)
		if err != nil || mem.Role != RoleMember {
			t.Fatalf("投影应回落 member: %+v %v", mem, err)
		}

		// owner 强制保留 owner 投影：服主摘 owner 角色不生效。
		if err := st.SetMemberRoles(ctx, srv.ID, owner.ID, []int64{rA.ID}, owner.ID); err != nil {
			t.Fatalf("SetMemberRoles(owner): %v", err)
		}
		list, err = st.RolesOfMember(ctx, srv.ID, owner.ID)
		if err != nil {
			t.Fatal(err)
		}
		hasOwner := false
		for _, r := range list {
			if r.ID == ownerID {
				hasOwner = true
			}
		}
		if !hasOwner {
			t.Fatal("owner 的 owner 角色应被强制保留")
		}
		mem, err = st.Member(ctx, srv.ID, owner.ID)
		if err != nil || mem.Role != RoleOwner {
			t.Fatalf("owner 投影应保持 owner: %+v %v", mem, err)
		}

		// 非成员 / 跨服 / 未知角色拒绝。
		if err := st.SetMemberRoles(ctx, srv.ID, 424242, []int64{rA.ID}, owner.ID); !errors.Is(err, ErrNotFound) {
			t.Fatalf("非成员应 ErrNotFound，实际 %v", err)
		}
		if err := st.SetMemberRoles(ctx, srv.ID, member.ID, []int64{999999999}, owner.ID); !errors.Is(err, ErrNotFound) {
			t.Fatalf("未知角色应 ErrNotFound，实际 %v", err)
		}
		other, _, _, _ := seedRolesServer(t, st)
		otherRoles, err := st.ListRoles(ctx, other.ID)
		if err != nil || len(otherRoles) == 0 {
			t.Fatal(err)
		}
		if err := st.SetMemberRoles(ctx, srv.ID, member.ID, []int64{otherRoles[0].ID}, owner.ID); !errors.Is(err, ErrNotFound) {
			t.Fatalf("跨服角色应 ErrNotFound，实际 %v", err)
		}
	})

	t.Run("非 owner 不得持有 owner 角色", func(t *testing.T) {
		srv, owner, admin, member := seedRolesServer(t, st)
		roles, err := st.ListRoles(ctx, srv.ID)
		if err != nil {
			t.Fatal(err)
		}
		var ownerID int64
		for _, r := range roles {
			if r.BuiltinKey == BuiltinKeyOwner {
				ownerID = r.ID
			}
		}
		if ownerID == 0 {
			t.Fatal("缺 owner builtin")
		}
		// 普通成员经 roleIDs 自授 owner → ErrForbidden。
		if err := st.SetMemberRoles(ctx, srv.ID, member.ID, []int64{ownerID}, member.ID); !errors.Is(err, ErrForbidden) {
			t.Fatalf("成员自授 owner 应 ErrForbidden，实际 %v", err)
		}
		// admin 同样无权持有 owner builtin。
		if err := st.SetMemberRoles(ctx, srv.ID, admin.ID, []int64{ownerID}, admin.ID); !errors.Is(err, ErrForbidden) {
			t.Fatalf("admin 持有 owner 应 ErrForbidden，实际 %v", err)
		}
		// 被拒不得半写：成员仍是原分配（@everyone），没有 owner。
		list, err := st.RolesOfMember(ctx, srv.ID, member.ID)
		if err != nil {
			t.Fatal(err)
		}
		if len(list) != 1 || list[0].BuiltinKey != BuiltinKeyMember {
			t.Fatalf("拒绝后不得改动分配: %+v", list)
		}
		mem, err := st.Member(ctx, srv.ID, member.ID)
		if err != nil || mem.Role != RoleMember {
			t.Fatalf("拒绝后投影应不变: %+v %v", mem, err)
		}
		// 服主自己保留/指定 owner 角色仍成功。
		if err := st.SetMemberRoles(ctx, srv.ID, owner.ID, []int64{ownerID}, owner.ID); err != nil {
			t.Fatalf("服主保留 owner 角色应成功: %v", err)
		}
		list, err = st.RolesOfMember(ctx, srv.ID, owner.ID)
		if err != nil {
			t.Fatal(err)
		}
		hasOwner := false
		for _, r := range list {
			if r.ID == ownerID {
				hasOwner = true
			}
		}
		if !hasOwner {
			t.Fatalf("服主应持有 owner 角色: %+v", list)
		}
	})

	t.Run("builtin 不可改删且禁造", func(t *testing.T) {
		srv, _, _, _ := seedRolesServer(t, st)
		list, err := st.ListRoles(ctx, srv.ID)
		if err != nil {
			t.Fatal(err)
		}
		everyone := list[0]
		if err := st.UpdateRole(ctx, &Role{ID: everyone.ID, ServerID: srv.ID, Name: "改名", Permissions: ViewChannel}); !errors.Is(err, ErrForbidden) {
			t.Fatalf("改 builtin 应 ErrForbidden，实际 %v", err)
		}
		if err := st.DeleteRole(ctx, everyone.ID); !errors.Is(err, ErrForbidden) {
			t.Fatalf("删 builtin 应 ErrForbidden，实际 %v", err)
		}
		if err := st.CreateRole(ctx, &Role{ServerID: srv.ID, Name: "伪", BuiltinKey: BuiltinKeyAdmin, Permissions: ViewChannel}); !errors.Is(err, ErrForbidden) {
			t.Fatalf("造 builtin 应 ErrForbidden，实际 %v", err)
		}
		if err := st.CreateRole(ctx, &Role{ServerID: srv.ID, Name: "高位", Permissions: AllPermissions + 1}); !errors.Is(err, ErrForbidden) {
			t.Fatalf("未定义位应 ErrForbidden，实际 %v", err)
		}
		if err := st.UpdateRole(ctx, &Role{ID: 4242424242, ServerID: srv.ID, Name: "x", Permissions: ViewChannel}); !errors.Is(err, ErrNotFound) {
			t.Fatalf("更新不存在角色应 ErrNotFound，实际 %v", err)
		}
		if err := st.CreateRole(ctx, &Role{ServerID: 4242424242, Name: "x", Permissions: ViewChannel}); !errors.Is(err, ErrNotFound) {
			t.Fatalf("建到不存在服务器应 ErrNotFound，实际 %v", err)
		}
	})

	t.Run("DeleteRole 级联分配与角色覆盖", func(t *testing.T) {
		srv, _, _, member := seedRolesServer(t, st)
		ch := &Channel{ServerID: srv.ID, Name: "c", Type: ChannelText}
		if err := st.CreateChannel(ctx, ch); err != nil {
			t.Fatalf("CreateChannel: %v", err)
		}
		r := &Role{ServerID: srv.ID, Name: "可删", Position: 7, Permissions: Speak}
		if err := st.CreateRole(ctx, r); err != nil {
			t.Fatalf("CreateRole: %v", err)
		}
		if err := st.SetMemberRoles(ctx, srv.ID, member.ID, []int64{r.ID}, 0); err != nil {
			t.Fatalf("SetMemberRoles: %v", err)
		}
		if err := st.SetChannelOverwrite(ctx, &ChannelOverwrite{
			ChannelID: ch.ID, TargetType: OverwriteTargetRole, TargetID: r.ID, Allow: Speak,
		}); err != nil {
			t.Fatalf("SetChannelOverwrite: %v", err)
		}
		if err := st.DeleteRole(ctx, r.ID); err != nil {
			t.Fatalf("DeleteRole: %v", err)
		}
		if _, err := st.RoleByID(ctx, r.ID); !errors.Is(err, ErrNotFound) {
			t.Fatalf("删除后应 ErrNotFound，实际 %v", err)
		}
		as, err := st.RoleAssignmentsOfServer(ctx, srv.ID)
		if err != nil {
			t.Fatal(err)
		}
		for _, a := range as {
			if a.RoleID == r.ID {
				t.Fatalf("删除角色后分配应级联清空: %+v", a)
			}
		}
		ows, err := st.ChannelOverwrites(ctx, ch.ID)
		if err != nil {
			t.Fatal(err)
		}
		if len(ows) != 0 {
			t.Fatalf("删除角色后角色覆盖应级联清空: %+v", ows)
		}
	})

	t.Run("RemoveMember 级联分配与成员覆盖", func(t *testing.T) {
		srv, _, _, member := seedRolesServer(t, st)
		ch := &Channel{ServerID: srv.ID, Name: "c", Type: ChannelText}
		if err := st.CreateChannel(ctx, ch); err != nil {
			t.Fatal(err)
		}
		r := &Role{ServerID: srv.ID, Name: "R", Position: 6, Permissions: Speak}
		if err := st.CreateRole(ctx, r); err != nil {
			t.Fatal(err)
		}
		if err := st.SetMemberRoles(ctx, srv.ID, member.ID, []int64{r.ID}, 0); err != nil {
			t.Fatal(err)
		}
		// 成员覆盖 + 角色覆盖各一。
		if err := st.SetChannelOverwrite(ctx, &ChannelOverwrite{
			ChannelID: ch.ID, TargetType: OverwriteTargetMember, TargetID: member.ID, Deny: ViewChannel,
		}); err != nil {
			t.Fatal(err)
		}
		if err := st.SetChannelOverwrite(ctx, &ChannelOverwrite{
			ChannelID: ch.ID, TargetType: OverwriteTargetRole, TargetID: r.ID, Allow: Speak,
		}); err != nil {
			t.Fatal(err)
		}
		if err := st.RemoveMember(ctx, srv.ID, member.ID); err != nil {
			t.Fatalf("RemoveMember: %v", err)
		}
		as, err := st.RoleAssignmentsOfServer(ctx, srv.ID)
		if err != nil {
			t.Fatal(err)
		}
		for _, a := range as {
			if a.UserID == member.ID {
				t.Fatalf("退服后分配应清空: %+v", a)
			}
		}
		ows, err := st.ChannelOverwrites(ctx, ch.ID)
		if err != nil {
			t.Fatal(err)
		}
		if len(ows) != 1 || ows[0].TargetType != OverwriteTargetRole {
			t.Fatalf("只应留角色覆盖: %+v", ows)
		}
		if err := st.RemoveMember(ctx, srv.ID, member.ID); !errors.Is(err, ErrNotFound) {
			t.Fatalf("二次退服应 ErrNotFound，实际 %v", err)
		}
	})

	t.Run("覆盖 upsert 校验与排序", func(t *testing.T) {
		srv, _, _, member := seedRolesServer(t, st)
		ch := &Channel{ServerID: srv.ID, Name: "c", Type: ChannelText}
		if err := st.CreateChannel(ctx, ch); err != nil {
			t.Fatal(err)
		}
		r := &Role{ServerID: srv.ID, Name: "R", Position: 6, Permissions: Speak}
		if err := st.CreateRole(ctx, r); err != nil {
			t.Fatal(err)
		}
		if err := st.SetChannelOverwrite(ctx, &ChannelOverwrite{
			ChannelID: ch.ID, TargetType: OverwriteTargetRole, TargetID: r.ID, Allow: Speak, Deny: SendMessages,
		}); err != nil {
			t.Fatalf("SetChannelOverwrite: %v", err)
		}
		// upsert 覆盖同键。
		if err := st.SetChannelOverwrite(ctx, &ChannelOverwrite{
			ChannelID: ch.ID, TargetType: OverwriteTargetRole, TargetID: r.ID, Allow: ConnectVoice,
		}); err != nil {
			t.Fatalf("upsert: %v", err)
		}
		ows, err := st.ChannelOverwrites(ctx, ch.ID)
		if err != nil || len(ows) != 1 {
			t.Fatalf("upsert 后应单行: %+v %v", ows, err)
		}
		if ows[0].Allow != ConnectVoice || ows[0].Deny != 0 {
			t.Fatalf("upsert 应全量替换 allow/deny: %+v", ows[0])
		}
		// 成员覆盖 + 排序：member < role（字典序），同类型按 target_id。
		if err := st.SetChannelOverwrite(ctx, &ChannelOverwrite{
			ChannelID: ch.ID, TargetType: OverwriteTargetMember, TargetID: member.ID, Allow: ViewChannel,
		}); err != nil {
			t.Fatal(err)
		}
		ows, err = st.ChannelOverwrites(ctx, ch.ID)
		if err != nil || len(ows) != 2 {
			t.Fatalf("应 2 行: %+v %v", ows, err)
		}
		if ows[0].TargetType != OverwriteTargetMember || ows[1].TargetType != OverwriteTargetRole {
			t.Fatalf("排序应 member 在前: %+v", ows)
		}
		// ChannelOverwritesOfServer 经频道关联。
		ows2, err := st.ChannelOverwritesOfServer(ctx, srv.ID)
		if err != nil || len(ows2) != 2 {
			t.Fatalf("OfServer: %+v %v", ows2, err)
		}

		// 校验：非法类型 / 频道不存在 / 跨服目标 / 非成员目标 / 未定义位。
		if err := st.SetChannelOverwrite(ctx, &ChannelOverwrite{
			ChannelID: ch.ID, TargetType: "group", TargetID: r.ID,
		}); !errors.Is(err, ErrForbidden) {
			t.Fatalf("非法 target_type 应 ErrForbidden，实际 %v", err)
		}
		if err := st.SetChannelOverwrite(ctx, &ChannelOverwrite{
			ChannelID: 4242424242, TargetType: OverwriteTargetRole, TargetID: r.ID,
		}); !errors.Is(err, ErrNotFound) {
			t.Fatalf("频道不存在应 ErrNotFound，实际 %v", err)
		}
		other, _, otherAdmin, _ := seedRolesServer(t, st)
		otherRoles, _ := st.ListRoles(ctx, other.ID)
		if err := st.SetChannelOverwrite(ctx, &ChannelOverwrite{
			ChannelID: ch.ID, TargetType: OverwriteTargetRole, TargetID: otherRoles[0].ID,
		}); !errors.Is(err, ErrNotFound) {
			t.Fatalf("跨服角色目标应 ErrNotFound，实际 %v", err)
		}
		if err := st.SetChannelOverwrite(ctx, &ChannelOverwrite{
			ChannelID: ch.ID, TargetType: OverwriteTargetMember, TargetID: otherAdmin.ID,
		}); !errors.Is(err, ErrNotFound) {
			t.Fatalf("非本服成员目标应 ErrNotFound，实际 %v", err)
		}
		if err := st.SetChannelOverwrite(ctx, &ChannelOverwrite{
			ChannelID: ch.ID, TargetType: OverwriteTargetRole, TargetID: r.ID, Allow: AllPermissions + 1,
		}); !errors.Is(err, ErrForbidden) {
			t.Fatalf("未定义位应 ErrForbidden，实际 %v", err)
		}
		// 频道覆盖只收频道位：管理类位是脏数据，Store 拒绝。
		if err := st.SetChannelOverwrite(ctx, &ChannelOverwrite{
			ChannelID: ch.ID, TargetType: OverwriteTargetRole, TargetID: r.ID, Allow: ManageRoles,
		}); !errors.Is(err, ErrForbidden) {
			t.Fatalf("Allow 含 ManageRoles 应 ErrForbidden，实际 %v", err)
		}
		if err := st.SetChannelOverwrite(ctx, &ChannelOverwrite{
			ChannelID: ch.ID, TargetType: OverwriteTargetRole, TargetID: r.ID, Deny: Administrator,
		}); !errors.Is(err, ErrForbidden) {
			t.Fatalf("Deny 含 Administrator 应 ErrForbidden，实际 %v", err)
		}

		// 删除覆盖。
		if err := st.DeleteChannelOverwrite(ctx, ch.ID, OverwriteTargetRole, r.ID); err != nil {
			t.Fatalf("DeleteChannelOverwrite: %v", err)
		}
		if err := st.DeleteChannelOverwrite(ctx, ch.ID, OverwriteTargetRole, r.ID); !errors.Is(err, ErrNotFound) {
			t.Fatalf("二次删除应 ErrNotFound，实际 %v", err)
		}
		ows, err = st.ChannelOverwrites(ctx, ch.ID)
		if err != nil || len(ows) != 1 || ows[0].TargetType != OverwriteTargetMember {
			t.Fatalf("删除后应只剩成员覆盖: %+v %v", ows, err)
		}
	})

	t.Run("DeleteChannel 与分类删除级联覆盖", func(t *testing.T) {
		srv, _, _, member := seedRolesServer(t, st)
		cat := &Category{ServerID: srv.ID, Name: "cat"}
		if err := st.CreateCategory(ctx, cat); err != nil {
			t.Fatal(err)
		}
		ch1 := &Channel{ServerID: srv.ID, Name: "c1", Type: ChannelText, CategoryID: &cat.ID}
		ch2 := &Channel{ServerID: srv.ID, Name: "c2", Type: ChannelVoice, CategoryID: &cat.ID}
		ch3 := &Channel{ServerID: srv.ID, Name: "c3", Type: ChannelText}
		for _, c := range []*Channel{ch1, ch2, ch3} {
			if err := st.CreateChannel(ctx, c); err != nil {
				t.Fatal(err)
			}
			if err := st.SetChannelOverwrite(ctx, &ChannelOverwrite{
				ChannelID: c.ID, TargetType: OverwriteTargetMember, TargetID: member.ID, Allow: ViewChannel,
			}); err != nil {
				t.Fatal(err)
			}
		}
		// 删单频道：只清自己的覆盖。
		if err := st.DeleteChannel(ctx, ch3.ID); err != nil {
			t.Fatalf("DeleteChannel: %v", err)
		}
		if ows, err := st.ChannelOverwrites(ctx, ch3.ID); err != nil || len(ows) != 0 {
			t.Fatalf("频道删除后覆盖应清空: %+v %v", ows, err)
		}
		if ows, err := st.ChannelOverwritesOfServer(ctx, srv.ID); err != nil || len(ows) != 2 {
			t.Fatalf("其它频道覆盖应保留: %+v %v", ows, err)
		}
		// 删分类：清子频道覆盖。
		if err := st.DeleteCategoryWithChannels(ctx, cat.ID); err != nil {
			t.Fatalf("DeleteCategoryWithChannels: %v", err)
		}
		if ows, err := st.ChannelOverwritesOfServer(ctx, srv.ID); err != nil || len(ows) != 0 {
			t.Fatalf("分类删除后子频道覆盖应清空: %+v %v", ows, err)
		}
	})

	t.Run("DeleteServer 级联全部新表", func(t *testing.T) {
		srv, owner, _, member := seedRolesServer(t, st)
		ch := &Channel{ServerID: srv.ID, Name: "c", Type: ChannelText}
		if err := st.CreateChannel(ctx, ch); err != nil {
			t.Fatal(err)
		}
		r := &Role{ServerID: srv.ID, Name: "R", Position: 6, Permissions: Speak}
		if err := st.CreateRole(ctx, r); err != nil {
			t.Fatal(err)
		}
		if err := st.SetMemberRoles(ctx, srv.ID, member.ID, []int64{r.ID}, owner.ID); err != nil {
			t.Fatal(err)
		}
		if err := st.SetChannelOverwrite(ctx, &ChannelOverwrite{
			ChannelID: ch.ID, TargetType: OverwriteTargetMember, TargetID: member.ID, Allow: Speak,
		}); err != nil {
			t.Fatal(err)
		}
		tid := member.ID
		if err := st.WriteAudit(ctx, &AuditLog{ServerID: srv.ID, ActorID: owner.ID, Action: "role.create",
			TargetType: "role", TargetID: &tid, ChannelID: &ch.ID, Detail: "{}"}); err != nil {
			t.Fatal(err)
		}
		if err := st.DeleteServer(ctx, srv.ID); err != nil {
			t.Fatalf("DeleteServer: %v", err)
		}
		if list, err := st.ListRoles(ctx, srv.ID); err != nil || len(list) != 0 {
			t.Fatalf("roles 应清空: %+v %v", list, err)
		}
		if as, err := st.RoleAssignmentsOfServer(ctx, srv.ID); err != nil || len(as) != 0 {
			t.Fatalf("member_roles 应清空: %+v %v", as, err)
		}
		if ows, err := st.ChannelOverwritesOfServer(ctx, srv.ID); err != nil || len(ows) != 0 {
			t.Fatalf("channel_overwrites 应清空: %+v %v", ows, err)
		}
		if logs, err := st.AuditLogBefore(ctx, srv.ID, 0, 50); err != nil || len(logs) != 0 {
			t.Fatalf("audit_log 应清空: %+v %v", logs, err)
		}
	})

	t.Run("审计写入与游标", func(t *testing.T) {
		srv, owner, _, _ := seedRolesServer(t, st)
		ch := &Channel{ServerID: srv.ID, Name: "c", Type: ChannelText}
		if err := st.CreateChannel(ctx, ch); err != nil {
			t.Fatal(err)
		}
		// 预置 ID + 零值回填两种写法。
		tid := owner.ID
		a1 := &AuditLog{ID: 0, ServerID: srv.ID, ActorID: owner.ID, Action: "member.kick",
			TargetType: "user", TargetID: &tid, Detail: "d1"}
		if err := st.WriteAudit(ctx, a1); err != nil {
			t.Fatalf("WriteAudit: %v", err)
		}
		if a1.ID == 0 || a1.CreatedAt.IsZero() {
			t.Fatalf("应有回填 id/时间: %+v", a1)
		}
		a2 := &AuditLog{ServerID: srv.ID, ActorID: owner.ID, Action: "channel.create",
			ChannelID: &ch.ID, Detail: "d2"}
		a3 := &AuditLog{ServerID: srv.ID, ActorID: owner.ID, Action: "role.update", Detail: "d3"}
		for _, a := range []*AuditLog{a2, a3} {
			if err := st.WriteAudit(ctx, a); err != nil {
				t.Fatalf("WriteAudit: %v", err)
			}
		}
		// 倒序游标：最新 2 条（新→旧）。
		page, err := st.AuditLogBefore(ctx, srv.ID, 0, 2)
		if err != nil || len(page) != 2 {
			t.Fatalf("第一页: %+v %v", page, err)
		}
		if page[0].ID != a3.ID || page[1].ID != a2.ID {
			t.Fatalf("应新→旧: %+v", page)
		}
		page2, err := st.AuditLogBefore(ctx, srv.ID, a2.ID, 2)
		if err != nil || len(page2) != 1 || page2[0].ID != a1.ID {
			t.Fatalf("游标翻页: %+v %v", page2, err)
		}
		// 可空列回读。
		if page2[0].TargetType != "user" || page2[0].TargetID == nil || *page2[0].TargetID != owner.ID {
			t.Fatalf("target 回读不一致: %+v", page2[0])
		}
		if page2[0].ChannelID != nil {
			t.Fatalf("channel_id 应为 NULL: %+v", page2[0])
		}
		if page[1].ChannelID == nil || *page[1].ChannelID != ch.ID {
			t.Fatalf("channel_id 回读不一致: %+v", page[1])
		}
		// limit 越界回落 50（写 3 条全返回）。
		all, err := st.AuditLogBefore(ctx, srv.ID, 0, 0)
		if err != nil || len(all) != 3 {
			t.Fatalf("limit 夹紧: %+v %v", all, err)
		}
		// 其它服务器隔离。
		other, _, _, _ := seedRolesServer(t, st)
		if logs, err := st.AuditLogBefore(ctx, other.ID, 0, 50); err != nil || len(logs) != 0 {
			t.Fatalf("跨服隔离: %+v %v", logs, err)
		}
	})

	t.Run("并发 SetMemberRoles 不半写", func(t *testing.T) {
		srv, owner, _, member := seedRolesServer(t, st)
		rA := &Role{ServerID: srv.ID, Name: "cA", Position: 9, Permissions: Speak}
		rB := &Role{ServerID: srv.ID, Name: "cB", Position: 9, Permissions: SendMessages}
		if err := st.CreateRole(ctx, rA); err != nil {
			t.Fatal(err)
		}
		if err := st.CreateRole(ctx, rB); err != nil {
			t.Fatal(err)
		}
		start := make(chan struct{})
		var wg sync.WaitGroup
		errs := make(chan error, 2)
		for _, set := range [][]int64{{rA.ID}, {rB.ID}} {
			wg.Add(1)
			go func(ids []int64) {
				defer wg.Done()
				<-start
				errs <- st.SetMemberRoles(ctx, srv.ID, member.ID, ids, owner.ID)
			}(set)
		}
		close(start)
		wg.Wait()
		close(errs)
		for err := range errs {
			if err != nil {
				t.Fatalf("并发 SetMemberRoles: %v", err)
			}
		}
		list, err := st.RolesOfMember(ctx, srv.ID, member.ID)
		if err != nil {
			t.Fatal(err)
		}
		// 终态必须是某一组完整结果：everyone + A 或 everyone + B，不能混装。
		set := map[int64]bool{}
		for _, r := range list {
			set[r.ID] = true
		}
		hasA, hasB := set[rA.ID], set[rB.ID]
		if len(list) != 2 || !set[list[0].ID] {
			t.Fatalf("终态应恰 2 条，实际 %+v", list)
		}
		if hasA == hasB {
			t.Fatalf("应恰好一个自定义角色生效: A=%v B=%v %+v", hasA, hasB, list)
		}
	})

	t.Run("并发覆盖 upsert 单行", func(t *testing.T) {
		srv, _, _, member := seedRolesServer(t, st)
		ch := &Channel{ServerID: srv.ID, Name: "c", Type: ChannelText}
		if err := st.CreateChannel(ctx, ch); err != nil {
			t.Fatal(err)
		}
		start := make(chan struct{})
		var wg sync.WaitGroup
		errs := make(chan error, 2)
		for _, allow := range []Permission{Speak, SendMessages} {
			wg.Add(1)
			go func(allow Permission) {
				defer wg.Done()
				<-start
				errs <- st.SetChannelOverwrite(ctx, &ChannelOverwrite{
					ChannelID: ch.ID, TargetType: OverwriteTargetMember, TargetID: member.ID, Allow: allow,
				})
			}(allow)
		}
		close(start)
		wg.Wait()
		close(errs)
		for err := range errs {
			if err != nil {
				t.Fatalf("并发 SetChannelOverwrite: %v", err)
			}
		}
		ows, err := st.ChannelOverwrites(ctx, ch.ID)
		if err != nil || len(ows) != 1 {
			t.Fatalf("并发后应单行，实际 %+v %v", ows, err)
		}
		if ows[0].Allow != Speak && ows[0].Allow != SendMessages {
			t.Fatalf("allow 应为两值之一: %+v", ows[0])
		}
	})
}

func TestRolesContractMem(t *testing.T) {
	runRolesContract(t, NewMem())
}

func TestRolesContractSQLite(t *testing.T) {
	sq := openTestSQLite(t)
	if err := sq.Migrate(context.Background(), findMigrationsSQLite(t)); err != nil {
		t.Fatalf("迁移失败: %v", err)
	}
	runRolesContract(t, sq)
}

// openRolesMySQL 建专用、干净的 MySQL 测试库（DSN 不可用时 t.Skip）。
func openRolesMySQL(t *testing.T) *MySQL {
	t.Helper()
	base := openTestMySQL(t)
	dsn := os.Getenv("LITETALK_MYSQL_DSN")
	if dsn == "" {
		dsn = "root:@tcp(127.0.0.1:3306)/?parseTime=true&multiStatements=true"
	}
	cfg, err := mysqldrv.ParseDSN(dsn)
	if err != nil {
		base.Close()
		t.Fatalf("解析 MySQL DSN 失败: %v", err)
	}
	ctx := context.Background()
	if _, err := base.DB().ExecContext(ctx, "DROP DATABASE IF EXISTS "+rolesTestDB); err != nil {
		base.Close()
		t.Fatalf("清理专用测试库失败: %v", err)
	}
	if _, err := base.DB().ExecContext(ctx,
		"CREATE DATABASE "+rolesTestDB+" CHARACTER SET utf8mb4 COLLATE utf8mb4_unicode_ci"); err != nil {
		base.Close()
		t.Fatalf("建专用测试库失败: %v", err)
	}
	base.Close()

	cfg.DBName = rolesTestDB
	t.Setenv("LITETALK_MYSQL_DSN", cfg.FormatDSN())
	m, err := OpenMySQL(cfg.FormatDSN())
	if err != nil {
		t.Fatalf("连接专用测试库失败: %v", err)
	}
	t.Cleanup(func() { _ = m.Close() })
	return m
}

func TestRolesContractMySQL(t *testing.T) {
	m := openRolesMySQL(t)
	if err := m.Migrate(context.Background(), findMigrations(t)); err != nil {
		t.Fatalf("迁移失败: %v", err)
	}
	runRolesContract(t, m)
}
