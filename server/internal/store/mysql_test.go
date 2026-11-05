package store

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"io/fs"
	"math"
	"os"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/OpenALC/LiteTalk/server/migrations"
	mysqldrv "github.com/go-sql-driver/mysql"
)

// 没有配置 DSN 的本地测试可跳过；CI 必须显式要求真库。
func testMySQLDSN(t *testing.T) string {
	t.Helper()
	dsn := os.Getenv("LITETALK_MYSQL_DSN")
	if dsn == "" {
		if os.Getenv("LITETALK_MYSQL_REQUIRED") == "1" {
			t.Fatal("真库测试要求 LITETALK_MYSQL_DSN")
		}
		t.Skip("未配置 LITETALK_MYSQL_DSN，跳过可选真库测试")
	}
	if _, err := mysqldrv.ParseDSN(dsn); err != nil {
		t.Fatalf("解析 MySQL DSN 失败: %v", err)
	}
	return dsn
}

// openTestMySQL 为每个调用方提供独立真库，保留既有辅助函数签名。
func openTestMySQL(t *testing.T) *MySQL {
	t.Helper()
	return openTestMySQLSchema(t, "default")
}

// openTestMySQLSchema 为单个测试创建随机库；连接池的 DSN 显式带库名。
// 不经 OpenMySQL，避免生产读取环境变量的覆盖规则把连接导向基准库。
func openTestMySQLSchema(t *testing.T, schemaHint string) *MySQL {
	t.Helper()
	if !regexp.MustCompile(`^[a-z][a-z0-9_]{0,24}$`).MatchString(schemaHint) {
		t.Fatalf("不安全的测试库名前缀: %q", schemaHint)
	}
	cfg, err := mysqldrv.ParseDSN(testMySQLDSN(t))
	if err != nil {
		t.Fatalf("解析 MySQL DSN 失败: %v", err)
	}
	var nonce [8]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		t.Fatalf("生成临时库名失败: %v", err)
	}
	schema := "litetalk_ci_" + schemaHint + "_" + hex.EncodeToString(nonce[:])
	cfg.DBName = ""
	admin, err := sql.Open("mysql", cfg.FormatDSN())
	if err != nil {
		t.Fatalf("打开 MySQL 连接失败: %v", err)
	}
	defer admin.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := admin.PingContext(ctx); err != nil {
		t.Fatalf("MySQL Ping 失败: %v", err)
	}
	if _, err := admin.ExecContext(ctx, "CREATE DATABASE `"+schema+"` CHARACTER SET utf8mb4 COLLATE utf8mb4_unicode_ci"); err != nil {
		t.Fatalf("创建临时库失败: %v", err)
	}
	cfg.DBName = schema
	db, err := sql.Open("mysql", cfg.FormatDSN())
	if err == nil {
		err = db.PingContext(ctx)
	}
	if err != nil {
		if db != nil {
			_ = db.Close()
		}
		_, _ = admin.ExecContext(ctx, "DROP DATABASE `"+schema+"`")
		t.Fatalf("连接临时库失败: %v", err)
	}
	db.SetMaxOpenConns(20)
	db.SetMaxIdleConns(5)
	m := &MySQL{db: db}
	t.Cleanup(func() {
		_ = m.Close()
		cfg.DBName = ""
		cleanup, err := sql.Open("mysql", cfg.FormatDSN())
		if err != nil {
			t.Errorf("打开临时库清理连接失败: %v", err)
			return
		}
		defer cleanup.Close()
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cleanupCancel()
		if _, err := cleanup.ExecContext(cleanupCtx, "DROP DATABASE `"+schema+"`"); err != nil {
			t.Errorf("清理临时库失败: %v", err)
		}
	})
	return m
}

// TestMySQLMigrateAndCRUD 真库：建库→迁移→回滚→再迁移→基础 CRUD。
func TestMySQLMigrateAndCRUD(t *testing.T) {
	m := openTestMySQLSchema(t, "migrate")
	ctx := context.Background()

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
	if err := m.DB().QueryRowContext(ctx, "SELECT COUNT(*) FROM information_schema.tables WHERE table_schema=DATABASE() AND table_name='users'").Scan(&cnt); err != nil || cnt != 0 {
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
	// 大 Snowflake ID + 4 位 tag（含前导零）写入并按 昵称#tag 查回（#22）。
	uBig := &User{ID: math.MaxInt64 - 7919, Email: "big@ex.com", Nickname: "big-user", Tag: "0042", PasswordHash: "h"}
	if err := m.CreateUser(ctx, uBig); err != nil {
		t.Fatalf("大 ID CreateUser: %v", err)
	}
	if got, err := m.UserByNicknameTag(ctx, "big-user", "0042"); err != nil || got.ID != uBig.ID || got.Tag != "0042" {
		t.Fatalf("大 ID UserByNicknameTag: %+v %v", got, err)
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

	// 邀请码消费与加入成员同一事务（#22）：重复加入不消耗次数。
	uJoin := &User{Email: "join@ex.com", Nickname: "joiner", Tag: "0003", PasswordHash: "h"}
	if err := m.CreateUser(ctx, uJoin); err != nil {
		t.Fatalf("CreateUser(joiner): %v", err)
	}
	if err := m.CreateInvite(ctx, &Invite{Code: "join001", ServerID: sv.ID, CreatorID: u.ID, MaxUses: 1}); err != nil {
		t.Fatalf("CreateInvite: %v", err)
	}
	jm, err := m.UseInviteAndAddMember(ctx, "join001", uJoin.ID, RoleMember)
	if err != nil {
		t.Fatalf("UseInviteAndAddMember: %v", err)
	}
	if jm.ServerID != sv.ID || jm.Role != RoleMember || jm.JoinedAt.IsZero() {
		t.Fatalf("返回成员不符: %+v", jm)
	}
	if _, err := m.UseInviteAndAddMember(ctx, "join001", uJoin.ID, RoleMember); err != ErrConflict {
		t.Fatalf("重复加入应 ErrConflict，实际 %v", err)
	}
	if inv, _ := m.InviteByCode(ctx, "join001"); inv.Uses != 1 {
		t.Fatalf("成员冲突不应消耗邀请码，uses = %d", inv.Uses)
	}
	if _, err := m.UseInviteAndAddMember(ctx, "join001", u.ID, RoleMember); err != ErrConflict {
		t.Fatalf("次数用尽应 ErrConflict，实际 %v", err)
	}

	// 频道分类归属（#72）：与 Mem 共用同一套断言，两边语义漂移会直接测挂。
	assertChannelCategoryPersist(t, m)
}

// TestMySQLUserByEmailCaseInsensitive 真库：utf8mb4_unicode_ci 让存量混合
// 大小写邮箱也能被规范化邮箱命中，且大小写变体重复建号冲突（SQLite 靠
// lower() 兜底，两边语义对齐）。
func TestMySQLUserByEmailCaseInsensitive(t *testing.T) {
	m := openTestMySQLSchema(t, "email")
	ctx := context.Background()
	if err := m.Migrate(ctx, findMigrations(t)); err != nil {
		t.Fatalf("迁移失败: %v", err)
	}
	email := "Legacy.Mixed@Example.COM"
	mixed := &User{Email: email, Nickname: "ci", Tag: "0001", PasswordHash: "h"}
	if err := m.CreateUser(ctx, mixed); err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	got, err := m.UserByEmail(ctx, strings.ToLower(email))
	if err != nil || got.ID != mixed.ID {
		t.Fatalf("规范化邮箱应命中存量混合大小写行: %+v %v", got, err)
	}
	if err := m.CreateUser(ctx, &User{
		Email: strings.ToLower(email), Nickname: "dup", Tag: "0002",
	}); err != ErrConflict {
		t.Fatalf("ci 排序下大小写变体的同一邮箱应 ErrConflict，实际 %v", err)
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
