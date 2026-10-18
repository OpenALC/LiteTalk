package store

import (
	"context"
	"io/fs"
	"os"
	"testing"

	"github.com/OpenALC/LiteTalk/server/migrations"
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
	// 清理历史。必须覆盖每条迁移建过的表（否则跨运行不幂等：上一轮留下的表会让
	// 下一轮 CREATE TABLE 直接 1050）——新增迁移建表要同步这张清单。
	for _, tbl := range []string{
		"invites", "messages", "channels", "categories", "server_members",
		"servers", "friendships", "users", "user_sessions", "schema_migrations",
	} {
		_, _ = m.DB().ExecContext(ctx, "DROP TABLE IF EXISTS "+tbl)
	}

	migFS := findMigrations(t)
	if err := m.Migrate(ctx, migFS); err != nil {
		t.Fatalf("迁移失败: %v", err)
	}
	// 幂等：重复执行应跳过。
	if err := m.Migrate(ctx, migFS); err != nil {
		t.Fatalf("重复迁移应幂等: %v", err)
	}

	// 回滚验证 down 迁移。迁移是叠放的，RollbackLast 只撤最新一个——只回滚一次
	// 就断言 users 消失，是只有 0001_init 的年代写的，0002+ 落地后恒失败。
	// 这里逐个回滚到空库，再验 users 确实没了。
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
	if err := m.DB().QueryRowContext(ctx, "SELECT COUNT(*) FROM information_schema.tables WHERE table_schema='litetalk_test' AND table_name='users'").Scan(&cnt); err != nil || cnt != 0 {
		t.Fatalf("回滚后 users 表应消失 cnt=%d err=%v", cnt, err)
	}
	// 再迁移回来。
	if err := m.Migrate(ctx, migFS); err != nil {
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
	// 原样保存（值全同）必须成功：MySQL 无 clientFoundRows 时报 0 行受影响，
	// 不能误判不存在（与 Mem 对齐）；不存在的 id 仍须 ErrNotFound。
	if err := m.UpdateCategory(ctx, upd); err != nil {
		t.Fatalf("原样保存应成功: %v", err)
	}
	miss := *upd
	miss.ID = 999999
	if err := m.UpdateCategory(ctx, &miss); err != ErrNotFound {
		t.Fatalf("不存在 id 应 ErrNotFound: %v", err)
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

// findMigrations 定位迁移文件：优先 embed（migrations.FS），回退磁盘目录。
func findMigrations(t *testing.T) fs.FS {
	t.Helper()
	// 磁盘目录覆盖测试（LITETALK_MIGRATIONS_DIR 指定时验证 os.DirFS 路径）。
	if dir := os.Getenv("LITETALK_MIGRATIONS_DIR"); dir != "" {
		return os.DirFS(dir)
	}
	return migrations.FS
}
