package authz

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/OpenALC/LiteTalk/server/internal/store"
)

var fixtureSeq atomic.Int64

// fixture 标准测试服：owner/admin/两个普通成员/局外人 + 文字频道。
type fixture struct {
	st                   *store.Mem
	srv                  *store.Server
	ch                   *store.Channel
	owner, admin, member *store.User
	member2              *store.User
	stranger             *store.User
	everyoneID           int64
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	ctx := context.Background()
	st := store.NewMem()
	n := fixtureSeq.Add(1)
	mk := func(kind string) *store.User {
		u := &store.User{
			Email: fmt.Sprintf("authz-%d-%s@ex.test", n, kind),
			// 昵称带序号保证同 tag 下全局唯一。
			Nickname:     fmt.Sprintf("z%d%s", n%100000, kind),
			Tag:          "0001",
			PasswordHash: "h",
		}
		if err := st.CreateUser(ctx, u); err != nil {
			t.Fatalf("CreateUser: %v", err)
		}
		return u
	}
	f := &fixture{
		st:       st,
		owner:    mk("o"),
		admin:    mk("a"),
		member:   mk("m"),
		member2:  mk("n"),
		stranger: mk("x"),
	}
	f.srv = &store.Server{Name: fmt.Sprintf("authz-%d", n), OwnerID: f.owner.ID, IconName: "music", IconColor: "#4FA4F3"}
	if err := st.CreateServer(ctx, f.srv); err != nil {
		t.Fatalf("CreateServer: %v", err)
	}
	f.ch = &store.Channel{ServerID: f.srv.ID, Name: "general", Type: store.ChannelText}
	if err := st.CreateChannel(ctx, f.ch); err != nil {
		t.Fatalf("CreateChannel: %v", err)
	}
	for _, m := range []struct {
		u *store.User
		r store.MemberRole
	}{
		{f.owner, store.RoleOwner}, {f.admin, store.RoleAdmin},
		{f.member, store.RoleMember}, {f.member2, store.RoleMember},
	} {
		if err := st.AddMember(ctx, &store.ServerMember{ServerID: f.srv.ID, UserID: m.u.ID, Role: m.r}); err != nil {
			t.Fatalf("AddMember: %v", err)
		}
	}
	roles, err := st.ListRoles(ctx, f.srv.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range roles {
		if r.BuiltinKey == store.BuiltinKeyMember {
			f.everyoneID = r.ID
		}
	}
	if f.everyoneID == 0 {
		t.Fatal("缺 @everyone 角色")
	}
	return f
}

// yes 断言调用得 (true, nil)。多值返回只能整体作实参，故收一个闭包。
func yes(t *testing.T, what string, call func() (bool, error)) {
	t.Helper()
	ok, err := call()
	if err != nil || !ok {
		t.Fatalf("%s: 得到 (%v, %v)，期望 (true, nil)", what, ok, err)
	}
}

// no 断言调用得 (false, nil)——非成员/无权限路径，不得带 error。
func no(t *testing.T, what string, call func() (bool, error)) {
	t.Helper()
	ok, err := call()
	if err != nil || ok {
		t.Fatalf("%s: 得到 (%v, %v)，期望 (false, nil)", what, ok, err)
	}
}

// TestDefaultTemplates owner/admin/member 默认模板：管理位、频道位、语音位。
func TestDefaultTemplates(t *testing.T) {
	f := newFixture(t)
	a := New(f.st, Options{})
	ctx := context.Background()

	// member：频道/语音位有，管理位无。
	yes(t, "member 语音位", func() (bool, error) { return a.Can(ctx, f.member.ID, f.ch.ID, store.Speak) })
	yes(t, "member 发言位", func() (bool, error) { return a.Can(ctx, f.member.ID, f.ch.ID, store.SendMessages) })
	no(t, "member 管理位", func() (bool, error) { return a.CanServer(ctx, f.member.ID, f.srv.ID, store.ManageRoles) })
	no(t, "member 踢人位", func() (bool, error) { return a.CanServer(ctx, f.member.ID, f.srv.ID, store.KickMembers) })
	// admin：模板全开（非服主，走 Administrator 并集）。
	yes(t, "admin 管理位", func() (bool, error) { return a.CanServer(ctx, f.admin.ID, f.srv.ID, store.ManageRoles) })
	yes(t, "admin 踢人位", func() (bool, error) { return a.CanServer(ctx, f.admin.ID, f.srv.ID, store.KickMembers) })
	yes(t, "admin 频道位", func() (bool, error) { return a.Can(ctx, f.admin.ID, f.ch.ID, store.ViewChannel) })
	// owner：全开。
	yes(t, "owner 管理位", func() (bool, error) { return a.CanServer(ctx, f.owner.ID, f.srv.ID, store.ManageServer) })
	yes(t, "owner 频道位", func() (bool, error) { return a.Can(ctx, f.owner.ID, f.ch.ID, store.PrioritySpeaker) })
}

// TestOwnerAdminBypassOverwrites owner 与 Administrator 绕过频道覆盖。
func TestOwnerAdminBypassOverwrites(t *testing.T) {
	f := newFixture(t)
	a := New(f.st, Options{})
	ctx := context.Background()
	all := store.ChannelPermissionMask
	if err := f.st.SetChannelOverwrite(ctx, &store.ChannelOverwrite{
		ChannelID: f.ch.ID, TargetType: store.OverwriteTargetRole, TargetID: f.everyoneID, Deny: all,
	}); err != nil {
		t.Fatal(err)
	}
	if err := f.st.SetChannelOverwrite(ctx, &store.ChannelOverwrite{
		ChannelID: f.ch.ID, TargetType: store.OverwriteTargetMember, TargetID: f.admin.ID, Deny: all,
	}); err != nil {
		t.Fatal(err)
	}
	no(t, "member 覆盖后", func() (bool, error) { return a.Can(ctx, f.member.ID, f.ch.ID, store.ViewChannel) })
	yes(t, "admin 个人 deny 拦不住 Administrator", func() (bool, error) { return a.Can(ctx, f.admin.ID, f.ch.ID, store.ViewChannel) })
	yes(t, "owner 绕过覆盖", func() (bool, error) { return a.Can(ctx, f.owner.ID, f.ch.ID, store.ViewChannel) })
}

// TestOverwriteLayering 端到端三层覆盖：@everyone deny → 角色 allow → 个人 deny。
func TestOverwriteLayering(t *testing.T) {
	f := newFixture(t)
	a := New(f.st, Options{})
	ctx := context.Background()

	if err := f.st.SetChannelOverwrite(ctx, &store.ChannelOverwrite{
		ChannelID: f.ch.ID, TargetType: store.OverwriteTargetRole, TargetID: f.everyoneID, Deny: store.ViewChannel,
	}); err != nil {
		t.Fatal(err)
	}
	no(t, "everyone deny 生效", func() (bool, error) { return a.Can(ctx, f.member.ID, f.ch.ID, store.ViewChannel) })

	// 自定义角色 + 该频道 allow → 持有者可见；无该角色的 member2 仍不可见。
	role := &store.Role{ServerID: f.srv.ID, Name: "可看", Position: 9, Permissions: store.Speak}
	if err := f.st.CreateRole(ctx, role); err != nil {
		t.Fatal(err)
	}
	if err := f.st.SetChannelOverwrite(ctx, &store.ChannelOverwrite{
		ChannelID: f.ch.ID, TargetType: store.OverwriteTargetRole, TargetID: role.ID, Allow: store.ViewChannel,
	}); err != nil {
		t.Fatal(err)
	}
	if err := f.st.SetMemberRoles(ctx, f.srv.ID, f.member.ID, []int64{role.ID}, f.owner.ID); err != nil {
		t.Fatal(err)
	}
	// 改完 Store 必须失效缓存——Authorizer 只在 TTL 到期/失效时重读。
	a.InvalidateAll()
	yes(t, "角色 allow 覆盖 everyone deny", func() (bool, error) { return a.Can(ctx, f.member.ID, f.ch.ID, store.ViewChannel) })
	no(t, "无该角色的成员", func() (bool, error) { return a.Can(ctx, f.member2.ID, f.ch.ID, store.ViewChannel) })

	// 成员个人 deny 再覆盖角色 allow。
	if err := f.st.SetChannelOverwrite(ctx, &store.ChannelOverwrite{
		ChannelID: f.ch.ID, TargetType: store.OverwriteTargetMember, TargetID: f.member.ID, Deny: store.ViewChannel,
	}); err != nil {
		t.Fatal(err)
	}
	a.InvalidateAll()
	no(t, "个人 deny 覆盖角色 allow", func() (bool, error) { return a.Can(ctx, f.member.ID, f.ch.ID, store.ViewChannel) })
}

// TestCanServerIgnoresOverwrites 服务器级判断不看频道覆盖。
func TestCanServerIgnoresOverwrites(t *testing.T) {
	f := newFixture(t)
	a := New(f.st, Options{})
	ctx := context.Background()
	if err := f.st.SetChannelOverwrite(ctx, &store.ChannelOverwrite{
		ChannelID: f.ch.ID, TargetType: store.OverwriteTargetRole, TargetID: f.everyoneID, Deny: store.ViewChannel,
	}); err != nil {
		t.Fatal(err)
	}
	no(t, "频道级受覆盖影响", func() (bool, error) { return a.Can(ctx, f.member.ID, f.ch.ID, store.ViewChannel) })
	yes(t, "服务器级不看覆盖", func() (bool, error) { return a.CanServer(ctx, f.member.ID, f.srv.ID, store.ViewChannel) })
	yes(t, "CanInServer(0)", func() (bool, error) { return a.CanInServer(ctx, f.member.ID, f.srv.ID, 0, store.ViewChannel) })
}

// TestCanResolvesChannel Can 自己解析频道；不存在返回 ErrNotFound。
func TestCanResolvesChannel(t *testing.T) {
	f := newFixture(t)
	a := New(f.st, Options{})
	ctx := context.Background()
	yes(t, "真实频道", func() (bool, error) { return a.Can(ctx, f.member.ID, f.ch.ID, store.Speak) })
	if _, err := a.Can(ctx, f.member.ID, 4242424242, store.Speak); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("频道不存在应 ErrNotFound，实际 %v", err)
	}
	if _, err := a.Can(ctx, f.member.ID, 0, store.Speak); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("Can 传 0 应 ErrNotFound（服务器级用 CanServer），实际 %v", err)
	}
}

// TestNonMemberAndPermEdge 非成员 false/nil；perm==0 放行；未定义位 ErrForbidden。
func TestNonMemberAndPermEdge(t *testing.T) {
	f := newFixture(t)
	a := New(f.st, Options{})
	ctx := context.Background()

	no(t, "局外人频道级", func() (bool, error) { return a.Can(ctx, f.stranger.ID, f.ch.ID, store.Speak) })
	no(t, "局外人服务器级", func() (bool, error) { return a.CanServer(ctx, f.stranger.ID, f.srv.ID, store.Speak) })
	if ok, err := a.IsMember(ctx, f.stranger.ID, f.srv.ID); err != nil || ok {
		t.Fatalf("局外人应非成员: %v %v", ok, err)
	}
	if ok, err := a.IsMember(ctx, f.member.ID, f.srv.ID); err != nil || !ok {
		t.Fatalf("成员应是成员: %v %v", ok, err)
	}

	// perm == 0：无要求直接放行（与成员身份无关）。
	yes(t, "perm=0 成员", func() (bool, error) { return a.Can(ctx, f.member.ID, f.ch.ID, 0) })
	yes(t, "perm=0 局外人", func() (bool, error) { return a.Can(ctx, f.stranger.ID, f.ch.ID, 0) })
	// 未定义位：统一 ErrForbidden（owner 也一样，先于任何旁路）。
	for _, u := range []*store.User{f.member, f.owner} {
		if ok, err := a.Can(ctx, u.ID, f.ch.ID, store.AllPermissions+1); ok || !errors.Is(err, store.ErrForbidden) {
			t.Fatalf("未定义位应 (false, ErrForbidden)，实际 (%v, %v)", ok, err)
		}
	}
}

// TestStoreErrorPropagates Store 故障必须向上返回 error，不能伪装成 403。
func TestStoreErrorPropagates(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	boom := errors.New("db down")
	w := &wrapperStore{Store: f.st, membersFn: func(context.Context, int64) ([]store.ServerMember, error) {
		return nil, boom
	}}
	a := New(w, Options{})
	if ok, err := a.Can(ctx, f.member.ID, f.ch.ID, store.Speak); ok || !errors.Is(err, boom) {
		t.Fatalf("故障应原样上抛，实际 (%v, %v)", ok, err)
	}
	w2 := &wrapperStore{Store: f.st, listRolesFn: func(context.Context, int64) ([]store.Role, error) {
		return nil, boom
	}}
	a2 := New(w2, Options{})
	if ok, err := a2.CanServer(ctx, f.member.ID, f.srv.ID, store.Speak); ok || !errors.Is(err, boom) {
		t.Fatalf("ListRoles 故障应上抛，实际 (%v, %v)", ok, err)
	}
	// 服务器不存在：ErrNotFound 上抛。
	if ok, err := a.CanServer(ctx, f.member.ID, 4242424242, store.Speak); ok || !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("服务器不存在应 ErrNotFound，实际 (%v, %v)", ok, err)
	}
}

// TestLegacyFallback 角色数据缺失（空/ErrNotFound/缺 @everyone）时回退静态模板，
// 行为与 B1 之前的 legacy 权限一致，且不许 panic。
func TestLegacyFallback(t *testing.T) {
	ctx := context.Background()
	t.Run("空角色表", func(t *testing.T) {
		f := newFixture(t)
		w := &wrapperStore{Store: f.st,
			listRolesFn: func(context.Context, int64) ([]store.Role, error) { return nil, nil },
			assignsFn:   func(context.Context, int64) ([]store.MemberRoleAssignment, error) { return nil, nil },
		}
		assertLegacyBehavior(t, New(w, Options{}), f, ctx)
	})
	t.Run("ListRoles/Assignments 返回 ErrNotFound", func(t *testing.T) {
		f := newFixture(t)
		w := &wrapperStore{Store: f.st,
			listRolesFn: func(context.Context, int64) ([]store.Role, error) {
				return nil, store.ErrNotFound
			},
			assignsFn: func(context.Context, int64) ([]store.MemberRoleAssignment, error) {
				return nil, store.ErrNotFound
			},
		}
		assertLegacyBehavior(t, New(w, Options{}), f, ctx)
	})
	t.Run("角色表缺 @everyone", func(t *testing.T) {
		f := newFixture(t)
		// 成员持有带管理位的自定义角色，但角色表缺 @everyone：模板模式必须无视分配。
		role := &store.Role{ServerID: f.srv.ID, Name: "有管理位", Position: 9, Permissions: store.ManageRoles | store.MuteMembers}
		if err := f.st.CreateRole(ctx, role); err != nil {
			t.Fatal(err)
		}
		if err := f.st.SetMemberRoles(ctx, f.srv.ID, f.member.ID, []int64{role.ID}, f.owner.ID); err != nil {
			t.Fatal(err)
		}
		w := &wrapperStore{Store: f.st,
			listRolesFn: func(context.Context, int64) ([]store.Role, error) {
				return []store.Role{*role}, nil // 只有自定义角色
			},
		}
		a := New(w, Options{})
		no(t, "模板模式无视分配权限", func() (bool, error) { return a.CanServer(ctx, f.member.ID, f.srv.ID, store.ManageRoles) })
		yes(t, "模板模式保留默认位", func() (bool, error) { return a.CanServer(ctx, f.member.ID, f.srv.ID, store.Speak) })
	})
}

// assertLegacyBehavior 三类 legacy 行为：owner/admin 全开、member 仅默认位。
func assertLegacyBehavior(t *testing.T, a *Authorizer, f *fixture, ctx context.Context) {
	t.Helper()
	yes(t, "legacy owner 全开", func() (bool, error) { return a.CanServer(ctx, f.owner.ID, f.srv.ID, store.ManageServer) })
	yes(t, "legacy admin 全开", func() (bool, error) { return a.CanServer(ctx, f.admin.ID, f.srv.ID, store.KickMembers) })
	yes(t, "legacy member 有语音位", func() (bool, error) { return a.Can(ctx, f.member.ID, f.ch.ID, store.ConnectVoice) })
	no(t, "legacy member 无管理位", func() (bool, error) { return a.CanServer(ctx, f.member.ID, f.srv.ID, store.ManageRoles) })
	no(t, "legacy member 无踢人位", func() (bool, error) { return a.CanServer(ctx, f.member.ID, f.srv.ID, store.KickMembers) })
	no(t, "legacy 局外人拒绝", func() (bool, error) { return a.Can(ctx, f.stranger.ID, f.ch.ID, store.Speak) })
}

// wrapperStore 嵌入 store.Store 按需拦截：计数（缓存测试）、注入故障、
// 模拟 legacy 旧库（角色数据缺失）。
type wrapperStore struct {
	store.Store
	mu        sync.Mutex
	calls     map[string]int
	perServer map[string]map[int64]int

	listRolesFn func(ctx context.Context, serverID int64) ([]store.Role, error)
	assignsFn   func(ctx context.Context, serverID int64) ([]store.MemberRoleAssignment, error)
	membersFn   func(ctx context.Context, serverID int64) ([]store.ServerMember, error)

	// blockList 非 nil 时 ListRoles 先等它（并发/重载时序测试用）。
	blockList   chan struct{}
	startedList chan struct{} // 非 nil 时进入 ListRoles 发一个信号
}

func (w *wrapperStore) bump(name string, serverID int64) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.calls == nil {
		w.calls = map[string]int{}
	}
	w.calls[name]++
	if w.perServer == nil {
		w.perServer = map[string]map[int64]int{}
	}
	if w.perServer[name] == nil {
		w.perServer[name] = map[int64]int{}
	}
	w.perServer[name][serverID]++
}

func (w *wrapperStore) count(name string) int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.calls[name]
}

func (w *wrapperStore) countServer(name string, serverID int64) int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.perServer[name][serverID]
}

func (w *wrapperStore) ServerByID(ctx context.Context, id int64) (*store.Server, error) {
	w.bump("ServerByID", id)
	return w.Store.ServerByID(ctx, id)
}

func (w *wrapperStore) ListRoles(ctx context.Context, serverID int64) ([]store.Role, error) {
	w.bump("ListRoles", serverID)
	if w.startedList != nil {
		w.startedList <- struct{}{}
	}
	if w.blockList != nil {
		<-w.blockList
	}
	if w.listRolesFn != nil {
		return w.listRolesFn(ctx, serverID)
	}
	return w.Store.ListRoles(ctx, serverID)
}

func (w *wrapperStore) RoleAssignmentsOfServer(ctx context.Context, serverID int64) ([]store.MemberRoleAssignment, error) {
	w.bump("RoleAssignmentsOfServer", serverID)
	if w.assignsFn != nil {
		return w.assignsFn(ctx, serverID)
	}
	return w.Store.RoleAssignmentsOfServer(ctx, serverID)
}

func (w *wrapperStore) MembersOfServer(ctx context.Context, serverID int64) ([]store.ServerMember, error) {
	w.bump("MembersOfServer", serverID)
	if w.membersFn != nil {
		return w.membersFn(ctx, serverID)
	}
	return w.Store.MembersOfServer(ctx, serverID)
}

func (w *wrapperStore) ChannelOverwritesOfServer(ctx context.Context, serverID int64) ([]store.ChannelOverwrite, error) {
	w.bump("ChannelOverwritesOfServer", serverID)
	return w.Store.ChannelOverwritesOfServer(ctx, serverID)
}

func (w *wrapperStore) ChannelByID(ctx context.Context, id int64) (*store.Channel, error) {
	w.bump("ChannelByID", 0)
	return w.Store.ChannelByID(ctx, id)
}
