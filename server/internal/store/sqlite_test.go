package store

import (
	"context"
	"io/fs"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/OpenALC/LiteTalk/server/migrations"
)

// openTestSQLite 打开临时文件库。SQLite 路径无条件跑（不准 t.Skip），
// 外部服务零依赖——这正是 #24 的验收前提。
func openTestSQLite(t *testing.T) *SQLite {
	t.Helper()
	t.Setenv("LITETALK_SQLITE_PATH", "") // 避免 env 覆盖 TempDir 路径
	sq, err := OpenSQLite(filepath.Join(t.TempDir(), "litetalk.db"))
	if err != nil {
		t.Fatalf("OpenSQLite: %v", err)
	}
	t.Cleanup(func() { _ = sq.Close() })
	return sq
}

// findMigrationsSQLite 定位 SQLite 方言迁移（sqlite/ 子目录；磁盘覆盖模式同布局）。
func findMigrationsSQLite(t *testing.T) fs.FS {
	t.Helper()
	if dir := os.Getenv("LITETALK_MIGRATIONS_DIR"); dir != "" {
		return os.DirFS(filepath.Join(dir, "sqlite"))
	}
	sub, err := fs.Sub(migrations.FS, "sqlite")
	if err != nil {
		t.Fatalf("fs.Sub sqlite: %v", err)
	}
	return sub
}

// TestSQLiteMigrateAndCRUD 迁移矩阵：Migrate → 再 Migrate（幂等）→ 逐个
// RollbackLast 撤到空库 → 断言 users 消失 → 再 Migrate → CRUD 冒烟。
func TestSQLiteMigrateAndCRUD(t *testing.T) {
	m := openTestSQLite(t)
	ctx := context.Background()
	migFS := findMigrationsSQLite(t)

	if err := m.Migrate(ctx, migFS); err != nil {
		t.Fatalf("迁移失败: %v", err)
	}
	if err := m.Migrate(ctx, migFS); err != nil {
		t.Fatalf("重复迁移应幂等: %v", err)
	}

	// 迁移是叠放的，RollbackLast 只撤最新一个——逐个回滚到空库再断言。
	for {
		var left int
		if err := m.DB().QueryRowContext(ctx, `SELECT COUNT(*) FROM schema_migrations`).Scan(&left); err != nil {
			t.Fatal(err)
		}
		if left == 0 {
			break
		}
		if err := m.RollbackLast(ctx, migFS); err != nil {
			t.Fatalf("回滚失败: %v", err)
		}
	}
	var cnt int
	if err := m.DB().QueryRowContext(ctx,
		`SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name='users'`).Scan(&cnt); err != nil || cnt != 0 {
		t.Fatalf("回滚后 users 表应消失 cnt=%d err=%v", cnt, err)
	}
	if err := m.Migrate(ctx, migFS); err != nil {
		t.Fatalf("二次迁移失败: %v", err)
	}

	// CRUD 冒烟（与 mysql_test 同结构，两边语义漂移会直接测挂）。
	u := &User{Email: "t@ex.com", Nickname: "tester", Tag: "0001", PasswordHash: "h"}
	if err := m.CreateUser(ctx, u); err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	got, err := m.UserByEmail(ctx, "t@ex.com")
	if err != nil || got.Nickname != "tester" {
		t.Fatalf("UserByEmail: %v %v", got, err)
	}
	if err := m.CreateUser(ctx, &User{Email: "t@ex.com", Nickname: "x", Tag: "0002"}); err != ErrConflict {
		t.Fatalf("邮箱唯一约束未生效: %v", err)
	}

	sv := &Server{Name: "cat-srv", OwnerID: u.ID, IconName: "music", IconColor: "#4FA4F3"}
	if err := m.CreateServer(ctx, sv); err != nil {
		t.Fatalf("CreateServer: %v", err)
	}
	catIDs := []int64{}
	for i, name := range []string{"a", "b", "c"} {
		c := &Category{ServerID: sv.ID, Name: name, Position: i}
		if err := m.CreateCategory(ctx, c); err != nil {
			t.Fatalf("CreateCategory: %v", err)
		}
		catIDs = append(catIDs, c.ID)
	}
	upd := &Category{ID: catIDs[0], ServerID: sv.ID, Name: "a2", Position: 0}
	if err := m.UpdateCategory(ctx, upd); err != nil {
		t.Fatalf("UpdateCategory: %v", err)
	}
	// 原样保存（值全同）必须成功，不存在的 id 仍须 ErrNotFound。
	if err := m.UpdateCategory(ctx, upd); err != nil {
		t.Fatalf("原样保存应成功: %v", err)
	}
	miss := *upd
	miss.ID = 999999
	if err := m.UpdateCategory(ctx, &miss); err != ErrNotFound {
		t.Fatalf("不存在 id 应 ErrNotFound: %v", err)
	}
	if err := m.ReorderCategories(ctx, sv.ID, catIDs[2], 0); err != nil {
		t.Fatalf("ReorderCategories: %v", err)
	}
	list, err := m.CategoriesOfServer(ctx, sv.ID)
	if err != nil || len(list) != 3 {
		t.Fatalf("CategoriesOfServer: %v %v", list, err)
	}
	want := []int64{catIDs[2], catIDs[0], catIDs[1]}
	for i, c := range list {
		if c.ID != want[i] || c.Position != i {
			t.Fatalf("重排后[%d] = %+v，期望 id=%d pos=%d", i, c, want[i], i)
		}
	}
	if list[1].Name != "a2" {
		t.Fatalf("改名未生效: %+v", list[1])
	}

	assertChannelCategoryPersist(t, m)
}

// TestSQLiteCRUD 全量表抽样 + 冲突路径（唯一键、UseInvite 超限/过期）。
func TestSQLiteCRUD(t *testing.T) {
	m := openTestSQLite(t)
	ctx := context.Background()
	if err := m.Migrate(ctx, findMigrationsSQLite(t)); err != nil {
		t.Fatalf("迁移失败: %v", err)
	}

	// users：双唯一键冲突。
	u1 := &User{Email: "a@ex.com", Nickname: "甲", Tag: "0001", PasswordHash: "h"}
	if err := m.CreateUser(ctx, u1); err != nil {
		t.Fatal(err)
	}
	u2 := &User{Email: "b@ex.com", Nickname: "乙", Tag: "0001", PasswordHash: "h"}
	if err := m.CreateUser(ctx, u2); err != nil {
		t.Fatal(err)
	}
	if err := m.CreateUser(ctx, &User{Email: "a@ex.com", Nickname: "丙", Tag: "0002"}); err != ErrConflict {
		t.Fatalf("email 冲突应 ErrConflict: %v", err)
	}
	if err := m.CreateUser(ctx, &User{Email: "c@ex.com", Nickname: "甲", Tag: "0001"}); err != ErrConflict {
		t.Fatalf("nickname+tag 冲突应 ErrConflict: %v", err)
	}
	upd := *u1
	upd.Nickname = "甲改"
	if err := m.UpdateUser(ctx, &upd); err != nil {
		t.Fatalf("UpdateUser: %v", err)
	}
	// 原样保存（值全同）不许误报 404。
	if err := m.UpdateUser(ctx, &upd); err != nil {
		t.Fatalf("原样保存应成功: %v", err)
	}
	if err := m.UpdateUser(ctx, &User{ID: 999999, Nickname: "z", Tag: "zzzz"}); err != ErrNotFound {
		t.Fatalf("不存在用户应 ErrNotFound: %v", err)
	}

	// friendships：双向查询 + 成对唯一。CreateFriendship 与 MySQL 同款只写
	// user_id/friend_id/status（备注经 UpdateFriendship 写，API 真实路径如此）。
	f := &Friendship{UserID: u1.ID, FriendID: u2.ID, Status: FriendPending}
	if err := m.CreateFriendship(ctx, f); err != nil {
		t.Fatal(err)
	}
	if err := m.CreateFriendship(ctx, &Friendship{UserID: u1.ID, FriendID: u2.ID, Status: FriendPending}); err != ErrConflict {
		t.Fatalf("成对唯一未生效: %v", err)
	}
	f.Status = FriendAccepted
	f.Remark = "小乙"
	f.GroupName = "队友"
	if err := m.UpdateFriendship(ctx, f); err != nil {
		t.Fatalf("UpdateFriendship: %v", err)
	}
	btw, err := m.FriendshipBetween(ctx, u2.ID, u1.ID)
	if err != nil || btw.Remark != "小乙" || btw.GroupName != "队友" {
		t.Fatalf("反向查询应命中并带回备注: %+v %v", btw, err)
	}
	if got, err := m.FriendshipByID(ctx, f.ID); err != nil || got.Status != FriendAccepted {
		t.Fatalf("状态更新未生效: %+v %v", got, err)
	}

	// servers + members。
	sv := &Server{Name: "服", OwnerID: u1.ID, IconName: "music", IconColor: "#4FA4F3"}
	if err := m.CreateServer(ctx, sv); err != nil {
		t.Fatal(err)
	}
	mem := &ServerMember{ServerID: sv.ID, UserID: u1.ID, Role: RoleOwner}
	if err := m.AddMember(ctx, mem); err != nil {
		t.Fatal(err)
	}
	if err := m.AddMember(ctx, &ServerMember{ServerID: sv.ID, UserID: u1.ID, Role: RoleMember}); err != ErrConflict {
		t.Fatalf("成员唯一未生效: %v", err)
	}
	if err := m.AddMember(ctx, &ServerMember{ServerID: sv.ID, UserID: u2.ID, Role: RoleMember}); err != nil {
		t.Fatal(err)
	}
	if got, err := m.Member(ctx, sv.ID, u1.ID); err != nil || got.Nickname != "甲改" {
		t.Fatalf("Member 应内联用户信息: %+v %v", got, err)
	}
	if list, err := m.MembersOfServer(ctx, sv.ID); err != nil || len(list) != 2 {
		t.Fatalf("MembersOfServer: %v %v", list, err)
	}
	if list, err := m.ServersOfUser(ctx, u2.ID); err != nil || len(list) != 1 || list[0].ID != sv.ID {
		t.Fatalf("ServersOfUser: %v %v", list, err)
	}

	// channels + messages。
	ch := &Channel{ServerID: sv.ID, Name: "文字", Type: ChannelText, Position: 0}
	if err := m.CreateChannel(ctx, ch); err != nil {
		t.Fatal(err)
	}
	msg := &Message{ChannelID: ch.ID, UserID: u1.ID, Content: "第一条"}
	if err := m.CreateMessage(ctx, msg); err != nil {
		t.Fatal(err)
	}
	if err := m.CreateMessage(ctx, &Message{ChannelID: ch.ID, UserID: u2.ID, Content: "第二条"}); err != nil {
		t.Fatal(err)
	}
	// beforeID=0 取最新，倒序收集后正序返回。
	list, err := m.MessagesBefore(ctx, ch.ID, 0, 50)
	if err != nil || len(list) != 2 || list[0].Content != "第一条" || list[1].Content != "第二条" {
		t.Fatalf("MessagesBefore 顺序: %v %v", list, err)
	}
	if list[0].SenderNickname != "甲改" || list[0].SenderTag != "0001" {
		t.Fatalf("消息应内联发送者: %+v", list[0])
	}
	// 分页：beforeID 取第二条之前。
	page, err := m.MessagesBefore(ctx, ch.ID, list[1].ID, 50)
	if err != nil || len(page) != 1 || page[0].Content != "第一条" {
		t.Fatalf("MessagesBefore 分页: %v %v", page, err)
	}

	// invites：超限与过期路径。
	inv := &Invite{Code: "inv001", ServerID: sv.ID, CreatorID: u1.ID, MaxUses: 1}
	if err := m.CreateInvite(ctx, inv); err != nil {
		t.Fatal(err)
	}
	if err := m.CreateInvite(ctx, &Invite{Code: "inv001", ServerID: sv.ID, CreatorID: u1.ID}); err != ErrConflict {
		t.Fatalf("邀请码唯一未生效: %v", err)
	}
	if err := m.UseInvite(ctx, "inv001"); err != nil {
		t.Fatalf("首次使用应成功: %v", err)
	}
	if err := m.UseInvite(ctx, "inv001"); err != ErrConflict {
		t.Fatalf("超限应 ErrConflict: %v", err)
	}
	if err := m.UseInvite(ctx, "missing"); err != ErrNotFound {
		t.Fatalf("不存在应 ErrNotFound: %v", err)
	}
	past := time.Now().Add(-time.Hour).UTC().Truncate(time.Second)
	if err := m.CreateInvite(ctx, &Invite{Code: "exp001", ServerID: sv.ID, CreatorID: u1.ID, ExpiresAt: &past}); err != nil {
		t.Fatal(err)
	}
	if err := m.UseInvite(ctx, "exp001"); err != ErrExpired {
		t.Fatalf("过期应 ErrExpired: %v", err)
	}
	got, err := m.InviteByCode(ctx, "exp001")
	if err != nil || got.ExpiresAt == nil || !got.ExpiresAt.Equal(past) {
		t.Fatalf("expires_at 往返失真: %+v %v", got, err)
	}

	// user_sessions：upsert 覆盖 + 时间往返。
	sess := &UserSession{UserID: u1.ID, DeviceType: "desktop", Sid: "s1", IP: "1.2.3.4"}
	if err := m.UpsertUserSession(ctx, sess); err != nil {
		t.Fatal(err)
	}
	g1, err := m.GetUserSession(ctx, u1.ID, "desktop")
	if err != nil || g1.Sid != "s1" || g1.CreatedAt.IsZero() || g1.UpdatedAt.IsZero() {
		t.Fatalf("GetUserSession: %+v %v", g1, err)
	}
	// 同值 upsert（值全同）也必须成功，且保留 CreatedAt。
	if err := m.UpsertUserSession(ctx, sess); err != nil {
		t.Fatalf("同值 upsert: %v", err)
	}
	g2, err := m.GetUserSession(ctx, u1.ID, "desktop")
	if err != nil || g2.Sid != "s1" || !g2.CreatedAt.Equal(g1.CreatedAt) {
		t.Fatalf("覆盖不应重置 CreatedAt: %+v %v", g2, err)
	}
	// 覆盖写：sid 变化，设备维度覆盖即吊销旧会话。
	sess.Sid = "s2"
	if err := m.UpsertUserSession(ctx, sess); err != nil {
		t.Fatal(err)
	}
	if g3, err := m.GetUserSession(ctx, u1.ID, "desktop"); err != nil || g3.Sid != "s2" {
		t.Fatalf("覆盖写失败: %+v %v", g3, err)
	}
	if err := m.DeleteUserSession(ctx, u1.ID, "desktop"); err != nil {
		t.Fatalf("DeleteUserSession: %v", err)
	}
	if _, err := m.GetUserSession(ctx, u1.ID, "desktop"); err != ErrNotFound {
		t.Fatalf("删除后应 ErrNotFound: %v", err)
	}
	if err := m.DeleteAllUserSessions(ctx, u1.ID); err != nil {
		t.Fatalf("DeleteAllUserSessions: %v", err)
	}

	// 删除链路：删频道连带消息，删服务器连带子资源。
	if err := m.DeleteChannel(ctx, ch.ID); err != nil {
		t.Fatalf("DeleteChannel: %v", err)
	}
	if _, err := m.ChannelByID(ctx, ch.ID); err != ErrNotFound {
		t.Fatalf("频道应消失: %v", err)
	}
	if list, err := m.MessagesBefore(ctx, ch.ID, 0, 50); err != nil || len(list) != 0 {
		t.Fatalf("频道删除应连带消息: %v %v", list, err)
	}
	if err := m.DeleteServer(ctx, sv.ID); err != nil {
		t.Fatalf("DeleteServer: %v", err)
	}
	if _, err := m.ServerByID(ctx, sv.ID); err != ErrNotFound {
		t.Fatalf("服务器应消失: %v", err)
	}
	if err := m.DeleteFriendship(ctx, f.ID); err != nil {
		t.Fatalf("DeleteFriendship: %v", err)
	}
}

// TestSQLiteTimeRoundTrip 时间往返专测（modernc 与 go-sql-driver 行为不同，
// 探针结论固化在 TestSQLiteTimeProbe，这里验 Store 层整链路）。
func TestSQLiteTimeRoundTrip(t *testing.T) {
	m := openTestSQLite(t)
	ctx := context.Background()
	if err := m.Migrate(ctx, findMigrationsSQLite(t)); err != nil {
		t.Fatalf("迁移失败: %v", err)
	}

	before := time.Now().Add(-time.Second)
	u := &User{Email: "time@ex.com", Nickname: "时", Tag: "0001", PasswordHash: "h"}
	if err := m.CreateUser(ctx, u); err != nil {
		t.Fatal(err)
	}
	after := time.Now().Add(time.Second)
	got, err := m.UserByID(ctx, u.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.CreatedAt.Before(before) || got.CreatedAt.After(after) {
		t.Fatalf("CreatedAt 越界: %v", got.CreatedAt)
	}
	if got.UpdatedAt.IsZero() {
		t.Fatalf("UpdatedAt 不应为零值: %+v", got)
	}
	// 时区归一：UTC 存储读回应可与 UTC 时刻直接比较。
	if !got.CreatedAt.Equal(got.CreatedAt.UTC()) && got.CreatedAt.Location() != time.UTC {
		t.Fatalf("时间应按 UTC 解释: loc=%v", got.CreatedAt.Location())
	}

	sess := &UserSession{UserID: u.ID, DeviceType: "desktop", Sid: "t1", IP: ""}
	if err := m.UpsertUserSession(ctx, sess); err != nil {
		t.Fatal(err)
	}
	s1, err := m.GetUserSession(ctx, u.ID, "desktop")
	if err != nil {
		t.Fatal(err)
	}
	if s1.CreatedAt.Before(before) || s1.CreatedAt.After(after) {
		t.Fatalf("会话 CreatedAt 越界: %v", s1.CreatedAt)
	}
	// 更新后 UpdatedAt 不应早于 CreatedAt。
	sess.Sid = "t2"
	if err := m.UpsertUserSession(ctx, sess); err != nil {
		t.Fatal(err)
	}
	s2, err := m.GetUserSession(ctx, u.ID, "desktop")
	if err != nil {
		t.Fatal(err)
	}
	if s2.UpdatedAt.Before(s2.CreatedAt) {
		t.Fatalf("UpdatedAt 早于 CreatedAt: up=%v cre=%v", s2.UpdatedAt, s2.CreatedAt)
	}
}

// TestSQLiteHasServers #27：引导邀请码的持久判据（空库 false，建服务器后 true）。
func TestSQLiteHasServers(t *testing.T) {
	ctx := context.Background()
	m := openTestSQLite(t)
	if err := m.Migrate(ctx, findMigrationsSQLite(t)); err != nil {
		t.Fatalf("迁移失败: %v", err)
	}
	ok, err := m.HasServers(ctx)
	if err != nil || ok {
		t.Fatalf("空库 HasServers = %v/%v，want false/nil", ok, err)
	}
	u := &User{Email: "owner@ex.com", Nickname: "owner", Tag: "0001"}
	if err := m.CreateUser(ctx, u); err != nil {
		t.Fatal(err)
	}
	if err := m.CreateServer(ctx, &Server{Name: "默认服务器", OwnerID: u.ID}); err != nil {
		t.Fatal(err)
	}
	ok, err = m.HasServers(ctx)
	if err != nil || !ok {
		t.Fatalf("建服务器后 HasServers = %v/%v，want true/nil", ok, err)
	}
}
