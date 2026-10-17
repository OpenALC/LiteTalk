package store

import (
	"context"
	"os"
	"testing"
)

// openTestMySQL 连接真库；DSN 不可用时返回 nil（测试 t.Skip）。
func openTestMySQL(t *testing.T) *MySQL {
	t.Helper()
	dsn := os.Getenv("LITETALK_MYSQL_DSN")
	if dsn == "" {
		dsn = "root:@tcp(127.0.0.1:3306)/?parseTime=true&multiStatements=true"
	}
	m, err := OpenMySQL(dsn)
	if err != nil {
		t.Skipf("MySQL 不可用，跳过真库测试: %v", err)
	}
	return m
}

// TestMySQLMigrateAndCRUD 真库：建库→迁移→回滚→再迁移→基础 CRUD。
func TestMySQLMigrateAndCRUD(t *testing.T) {
	m := openTestMySQL(t)
	defer m.Close()
	ctx := context.Background()

	// 建独立测试库，避免污染。
	if _, err := m.DB().ExecContext(ctx, "CREATE DATABASE IF NOT EXISTS litetalk_test"); err != nil {
		t.Fatalf("建测试库失败: %v", err)
	}
	if _, err := m.DB().ExecContext(ctx, "USE litetalk_test"); err != nil {
		t.Fatal(err)
	}
	// 清理历史。
	for _, tbl := range []string{"invites", "messages", "channels", "categories", "server_members", "servers", "friendships", "users", "schema_migrations"} {
		_, _ = m.DB().ExecContext(ctx, "DROP TABLE IF EXISTS "+tbl)
	}

	migDir := findMigrations(t)
	if err := m.Migrate(ctx, migDir); err != nil {
		t.Fatalf("迁移失败: %v", err)
	}
	// 幂等：重复执行应跳过。
	if err := m.Migrate(ctx, migDir); err != nil {
		t.Fatalf("重复迁移应幂等: %v", err)
	}

	// 回滚验证 down 迁移。
	if err := m.RollbackLast(ctx, migDir); err != nil {
		t.Fatalf("回滚失败: %v", err)
	}
	var cnt int
	if err := m.DB().QueryRowContext(ctx, "SELECT COUNT(*) FROM information_schema.tables WHERE table_schema='litetalk_test' AND table_name='users'").Scan(&cnt); err != nil || cnt != 0 {
		t.Fatalf("回滚后 users 表应消失 cnt=%d err=%v", cnt, err)
	}
	// 再迁移回来。
	if err := m.Migrate(ctx, migDir); err != nil {
		t.Fatalf("二次迁移失败: %v", err)
	}

	// CRUD 冒烟。
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

	// 分类改名/重排（0.1.2 PATCH）：MySQL 事务路径与 Mem 同语义。
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
	// 末位移到最前，重排为 0..n-1。
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
}

// findMigrations 定位 migrations 目录（相对本包 ../../migrations）。
func findMigrations(t *testing.T) string {
	t.Helper()
	for _, p := range []string{"../../migrations", "../migrations", "migrations"} {
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}
	t.Fatal("找不到 migrations 目录")
	return ""
}
