package store

import (
	"context"
	"testing"
)

// TestUserSession 多设备会话 CRUD（v1.1.0 迁移 0004）。
func TestUserSession(t *testing.T) {
	ctx := context.Background()
	m := NewMem()

	// 未命中时返回 ErrNotFound。
	if _, err := m.GetUserSession(ctx, 1, "desktop"); err != ErrNotFound {
		t.Fatalf("未命中应 ErrNotFound，实际 %v", err)
	}

	// upsert 创建会话。
	s := &UserSession{UserID: 1, DeviceType: "desktop", Sid: "s1", IP: "10.0.0.1"}
	if err := m.UpsertUserSession(ctx, s); err != nil {
		t.Fatal(err)
	}
	got, err := m.GetUserSession(ctx, 1, "desktop")
	if err != nil {
		t.Fatal(err)
	}
	if got.Sid != "s1" || got.CreatedAt.IsZero() || got.UpdatedAt.IsZero() {
		t.Fatalf("读回错误: %+v", got)
	}

	// upsert 覆盖同设备会话，保留 CreatedAt。
	s2 := &UserSession{UserID: 1, DeviceType: "desktop", Sid: "s2"}
	if err := m.UpsertUserSession(ctx, s2); err != nil {
		t.Fatal(err)
	}
	got2, _ := m.GetUserSession(ctx, 1, "desktop")
	if got2.Sid != "s2" {
		t.Fatalf("应覆盖为新 sid，实际 %q", got2.Sid)
	}
	if !got2.CreatedAt.Equal(got.CreatedAt) {
		t.Fatalf("覆盖不应重置 CreatedAt")
	}

	// 异设备共存。
	if err := m.UpsertUserSession(ctx, &UserSession{UserID: 1, DeviceType: "mobile", Sid: "m1"}); err != nil {
		t.Fatal(err)
	}
	got3, _ := m.GetUserSession(ctx, 1, "mobile")
	if got3.Sid != "m1" {
		t.Fatalf("mobile 会话应独立")
	}

	// 删单个设备会话。
	if err := m.DeleteUserSession(ctx, 1, "mobile"); err != nil {
		t.Fatal(err)
	}
	if _, err := m.GetUserSession(ctx, 1, "mobile"); err != ErrNotFound {
		t.Fatalf("删后应 ErrNotFound")
	}

	// 删全部设备会话。
	if err := m.DeleteAllUserSessions(ctx, 1); err != nil {
		t.Fatal(err)
	}
	if _, err := m.GetUserSession(ctx, 1, "desktop"); err != ErrNotFound {
		t.Fatalf("全删后应 ErrNotFound")
	}

	// 再删应 ErrNotFound（幂等校验，防盲删）。
	if err := m.DeleteUserSession(ctx, 1, "desktop"); err != ErrNotFound {
		t.Fatalf("重复删应 ErrNotFound，实际 %v", err)
	}
}

// TestMemStore 内存实现基本 CRUD（语义与 MySQL 对齐的回归保障）。
func TestMemStore(t *testing.T) {
	ctx := context.Background()
	m := NewMem()

	// users 唯一性。
	u1 := &User{Email: "a@ex.com", Nickname: "alice", Tag: "0123", PasswordHash: "h"}
	if err := m.CreateUser(ctx, u1); err != nil {
		t.Fatal(err)
	}
	if err := m.CreateUser(ctx, &User{Email: "a@ex.com", Nickname: "x", Tag: "9999"}); err != ErrConflict {
		t.Fatalf("邮箱冲突应报 ErrConflict，实际 %v", err)
	}
	if err := m.CreateUser(ctx, &User{Email: "b@ex.com", Nickname: "alice", Tag: "0123"}); err != ErrConflict {
		t.Fatalf("(nickname,tag) 冲突应报 ErrConflict，实际 %v", err)
	}

	// friendships 双向唯一。
	f := &Friendship{UserID: u1.ID, FriendID: 2, Status: FriendPending}
	if err := m.CreateFriendship(ctx, f); err != nil {
		t.Fatal(err)
	}
	if err := m.CreateFriendship(ctx, &Friendship{UserID: 2, FriendID: u1.ID}); err != ErrConflict {
		t.Fatalf("反向关系应冲突，实际 %v", err)
	}

	// servers + members + channels + messages 级联删除。
	s := &Server{Name: "s", OwnerID: u1.ID, IconName: "music", IconColor: "#4FA4F3"}
	if err := m.CreateServer(ctx, s); err != nil {
		t.Fatal(err)
	}
	if err := m.AddMember(ctx, &ServerMember{ServerID: s.ID, UserID: u1.ID, Role: RoleOwner}); err != nil {
		t.Fatal(err)
	}
	ch := &Channel{ServerID: s.ID, Name: "c", Type: ChannelText}
	if err := m.CreateChannel(ctx, ch); err != nil {
		t.Fatal(err)
	}
	if err := m.CreateMessage(ctx, &Message{ChannelID: ch.ID, UserID: u1.ID, Content: "hi"}); err != nil {
		t.Fatal(err)
	}
	if err := m.DeleteServer(ctx, s.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := m.ChannelByID(ctx, ch.ID); err != ErrNotFound {
		t.Fatalf("删服后频道应消失，实际 %v", err)
	}

	// invites 用尽。
	inv := &Invite{Code: "abc", ServerID: 99, CreatorID: 1, MaxUses: 1}
	// 手动注入一个 server 供 invite 外键语义。
	s2 := &Server{Name: "s2", OwnerID: 1}
	_ = m.CreateServer(ctx, s2)
	inv.ServerID = s2.ID
	if err := m.CreateInvite(ctx, inv); err != nil {
		t.Fatal(err)
	}
	if err := m.UseInvite(ctx, "abc"); err != nil {
		t.Fatal(err)
	}
	if err := m.UseInvite(ctx, "abc"); err != ErrConflict {
		t.Fatalf("用尽应 ErrConflict，实际 %v", err)
	}

	// KV：Incr 语义。
	n, err := m.Incr(ctx, "k", 0)
	if err != nil || n != 1 {
		t.Fatalf("Incr 首次 = %d %v", n, err)
	}
	n, _ = m.Incr(ctx, "k", 0)
	if n != 2 {
		t.Fatalf("Incr 第二次 = %d", n)
	}
}

// TestCategoryUpdateAndReorder 分类改名/重排（0.1.2 PATCH 的 store 语义）：
// 重排后相对顺序保持并重排 0..n-1；改名不动 created_at；不存在的 id 报 ErrNotFound。
func TestCategoryUpdateAndReorder(t *testing.T) {
	ctx := context.Background()
	m := NewMem()
	s := &Server{Name: "cat-srv", OwnerID: 1, IconName: "music", IconColor: "#4FA4F3"}
	if err := m.CreateServer(ctx, s); err != nil {
		t.Fatal(err)
	}
	ids := []int64{}
	for i, name := range []string{"甲", "乙", "丙"} {
		c := &Category{ServerID: s.ID, Name: name, Position: i}
		if err := m.CreateCategory(ctx, c); err != nil {
			t.Fatal(err)
		}
		ids = append(ids, c.ID)
	}

	// UpdateCategory 改名；created_at 不可变、不许挪服务器。
	first, err := m.CategoryByID(ctx, ids[0])
	if err != nil {
		t.Fatal(err)
	}
	first.Name = "甲改"
	if err := m.UpdateCategory(ctx, first); err != nil {
		t.Fatal(err)
	}
	got, _ := m.CategoryByID(ctx, ids[0])
	if got.Name != "甲改" || !got.CreatedAt.Equal(first.CreatedAt) {
		t.Fatalf("改名后 = %+v", got)
	}
	missing := *got
	missing.ID = 9999
	if err := m.UpdateCategory(ctx, &missing); err != ErrNotFound {
		t.Fatalf("不存在 id 应 ErrNotFound，实际 %v", err)
	}
	// 原样保存（值全同）必须成功：用户在对话框里不改内容直接保存是常规操作，
	// 不是 404（MySQL 无 clientFoundRows 时同样报 0 行，两边语义须一致）。
	if err := m.UpdateCategory(ctx, got); err != nil {
		t.Fatalf("原样保存应成功，实际 %v", err)
	}

	// 末尾移到最前：丙 甲 乙 → 位置 0 1 2。
	if err := m.ReorderCategories(ctx, s.ID, ids[2], 0); err != nil {
		t.Fatal(err)
	}
	list, _ := m.CategoriesOfServer(ctx, s.ID)
	want := []int64{ids[2], ids[0], ids[1]}
	for i, c := range list {
		if c.ID != want[i] || c.Position != i {
			t.Fatalf("重排后[%d] = %+v，期望 id=%d pos=%d", i, c, want[i], i)
		}
	}

	// 最前移到末尾：甲 乙 丙（越界 position 夹到尾）。
	if err := m.ReorderCategories(ctx, s.ID, ids[2], 99); err != nil {
		t.Fatal(err)
	}
	list, _ = m.CategoriesOfServer(ctx, s.ID)
	want = []int64{ids[0], ids[1], ids[2]}
	for i, c := range list {
		if c.ID != want[i] || c.Position != i {
			t.Fatalf("夹尾后[%d] = %+v，期望 id=%d pos=%d", i, c, want[i], i)
		}
	}

	// 不存在/跨服务器 id 报 ErrNotFound，不影响现有顺序。
	other := &Server{Name: "other", OwnerID: 1, IconName: "music", IconColor: "#4FA4F3"}
	if err := m.CreateServer(ctx, other); err != nil {
		t.Fatal(err)
	}
	if err := m.ReorderCategories(ctx, other.ID, ids[0], 0); err != ErrNotFound {
		t.Fatalf("跨服务器应 ErrNotFound，实际 %v", err)
	}
	if err := m.ReorderCategories(ctx, s.ID, 9999, 0); err != ErrNotFound {
		t.Fatalf("不存在 id 应 ErrNotFound，实际 %v", err)
	}
}
