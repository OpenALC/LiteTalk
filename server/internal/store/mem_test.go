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
