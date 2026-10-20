package store

import (
	"context"
	"errors"
	"os"
	"testing"

	mysqldrv "github.com/go-sql-driver/mysql"
)

// runNicknameCaseContract 三个 Store 共用的昵称大小写契约（#24 P2）：
// nickname 逐字节比较，大小写变体 + 同 tag 是两个独立账号。
func runNicknameCaseContract(t *testing.T, st Store) {
	t.Helper()
	ctx := context.Background()

	t.Run("大小写变体同 tag 可共存且按精确大小写命中", func(t *testing.T) {
		upper := &User{Email: "case-upper@ex.com", Nickname: "Alice", Tag: "9001"}
		lower := &User{Email: "case-lower@ex.com", Nickname: "alice", Tag: "9001"}
		if err := st.CreateUser(ctx, upper); err != nil {
			t.Fatalf("创建 Alice: %v", err)
		}
		if err := st.CreateUser(ctx, lower); err != nil {
			t.Fatalf("创建 alice: %v", err)
		}
		if upper.ID == lower.ID {
			t.Fatalf("两个大小写变体应得到不同 id: %d", upper.ID)
		}
		for _, tc := range []struct {
			nickname string
			wantID   int64
		}{
			{"Alice", upper.ID},
			{"alice", lower.ID},
		} {
			got, err := st.UserByNicknameTag(ctx, tc.nickname, "9001")
			if err != nil {
				t.Fatalf("UserByNicknameTag(%s): %v", tc.nickname, err)
			}
			if got.ID != tc.wantID || got.Nickname != tc.nickname {
				t.Fatalf("UserByNicknameTag(%s) 命中错误账号: %+v", tc.nickname, got)
			}
		}
		// 既不是 Alice 也不是 alice 的第三种大小写不命中；tag 不同也不命中。
		if _, err := st.UserByNicknameTag(ctx, "ALICE", "9001"); !errors.Is(err, ErrNotFound) {
			t.Fatalf("ALICE 不应命中任何账号，实际 %v", err)
		}
		if _, err := st.UserByNicknameTag(ctx, "Alice", "9002"); !errors.Is(err, ErrNotFound) {
			t.Fatalf("tag 不同不应命中，实际 %v", err)
		}
	})

	t.Run("同大小写重复仍冲突", func(t *testing.T) {
		first := &User{Email: "dup-first@ex.com", Nickname: "Bob", Tag: "9010"}
		if err := st.CreateUser(ctx, first); err != nil {
			t.Fatalf("创建 Bob: %v", err)
		}
		again := &User{Email: "dup-second@ex.com", Nickname: "Bob", Tag: "9010"}
		if err := st.CreateUser(ctx, again); !errors.Is(err, ErrConflict) {
			t.Fatalf("同大小写重复应 ErrConflict，实际 %v", err)
		}
		if _, err := st.UserByNicknameTag(ctx, "Bob", "9010"); err != nil {
			t.Fatalf("原账号应仍可查: %v", err)
		}
	})
}

func TestNicknameCaseContractMem(t *testing.T) {
	runNicknameCaseContract(t, NewMem())
}

func TestNicknameCaseContractSQLite(t *testing.T) {
	m := openTestSQLite(t)
	if err := m.Migrate(context.Background(), findMigrationsSQLite(t)); err != nil {
		t.Fatalf("迁移失败: %v", err)
	}
	runNicknameCaseContract(t, m)
}

// nicknameTestDB MySQL 专用昵称契约库：每次用例重建，避免与同包其它用例互相污染。
const nicknameTestDB = "litetalk_nickname_test"

// openNicknameMySQL 建一个专用、干净的 MySQL 测试库（做法同 openFriendshipMySQL）。
// DSN 里带库名而不是靠 USE：连接池会新开连接，USE 只影响执行它的那条连接。
func openNicknameMySQL(t *testing.T) *MySQL {
	t.Helper()
	base := openTestMySQL(t) // DSN 不可用时跳过
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
	if _, err := base.DB().ExecContext(ctx, "DROP DATABASE IF EXISTS "+nicknameTestDB); err != nil {
		base.Close()
		t.Fatalf("清理专用测试库失败: %v", err)
	}
	if _, err := base.DB().ExecContext(ctx,
		"CREATE DATABASE "+nicknameTestDB+" CHARACTER SET utf8mb4 COLLATE utf8mb4_unicode_ci"); err != nil {
		base.Close()
		t.Fatalf("建专用测试库失败: %v", err)
	}
	base.Close()

	cfg.DBName = nicknameTestDB
	// OpenMySQL 优先读环境变量；t.Setenv 让后续连接也指向专用库，用例结束自动还原。
	t.Setenv("LITETALK_MYSQL_DSN", cfg.FormatDSN())
	m, err := OpenMySQL(cfg.FormatDSN())
	if err != nil {
		t.Fatalf("连接专用测试库失败: %v", err)
	}
	t.Cleanup(func() { _ = m.Close() })
	return m
}

func TestNicknameCaseContractMySQL(t *testing.T) {
	m := openNicknameMySQL(t)
	if err := m.Migrate(context.Background(), findMigrations(t)); err != nil {
		t.Fatalf("迁移失败: %v", err)
	}
	runNicknameCaseContract(t, m)
}

// TestNicknameCaseMigrationSQLite 0009 在 SQLite 是注释型 no-op：
// 老库里本来就存在的大小写变体升级后原样可读，回滚 0009 也不动数据。
func TestNicknameCaseMigrationSQLite(t *testing.T) {
	ctx := context.Background()
	m := openTestSQLite(t)
	full := findMigrationsSQLite(t)
	if err := m.Migrate(ctx, migrationsBefore(t, full, "0009")); err != nil {
		t.Fatalf("迁移到 0008 失败: %v", err)
	}
	// 0009 之前 SQLite 就允许大小写变体同 tag 共存，预置两行模拟老库。
	if _, err := m.DB().ExecContext(ctx,
		`INSERT INTO users (id, email, nickname, tag, password_hash) VALUES
		 (8001, 'mig-up@ex.com', 'Alice', '0001', ''), (8002, 'mig-low@ex.com', 'alice', '0001', '')`); err != nil {
		t.Fatalf("预置大小写变体失败: %v", err)
	}
	if err := m.Migrate(ctx, full); err != nil {
		t.Fatalf("应用 0009 失败: %v", err)
	}
	assertNicknameMigrationState(t, m, "0009_users_nickname_bin", true)

	if err := m.RollbackLast(ctx, full); err != nil {
		t.Fatalf("回滚 0009 失败: %v", err)
	}
	assertNicknameMigrationState(t, m, "0009_users_nickname_bin", false)
	// 回滚后再迁移一次，确认注释型迁移可重入。
	if err := m.Migrate(ctx, full); err != nil {
		t.Fatalf("回滚后再迁移失败: %v", err)
	}
	assertNicknameMigrationState(t, m, "0009_users_nickname_bin", true)
}

// assertNicknameMigrationState 两行老数据在 0009 前后都可读；SQLite 语义始终敏感。
func assertNicknameMigrationState(t *testing.T, m *SQLite, version string, applied bool) {
	t.Helper()
	ctx := context.Background()
	var n int
	if err := m.DB().QueryRowContext(ctx,
		`SELECT COUNT(*) FROM schema_migrations WHERE version = ?`, version).Scan(&n); err != nil {
		t.Fatalf("读 schema_migrations: %v", err)
	}
	if applied != (n == 1) {
		t.Fatalf("%s 应用状态不符（期望 applied=%v，实际 %d 条记录）", version, applied, n)
	}
	for _, tc := range []struct {
		nickname string
		wantID   int64
	}{{"Alice", 8001}, {"alice", 8002}} {
		got, err := m.UserByNicknameTag(ctx, tc.nickname, "0001")
		if err != nil || got.ID != tc.wantID {
			t.Fatalf("老数据 %s#0001 应可读: %+v %v", tc.nickname, got, err)
		}
	}
	if _, err := m.UserByNicknameTag(ctx, "ALICE", "0001"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("SQLite 大小写变体不应命中，实际 %v", err)
	}
}

// TestNicknameCaseMigrationMySQL 真库验证 0009：旧库可读、列排序规则切换、
// 迁移后大小写变体可共存、down 在有变体时失败、处理后回滚并再次迁移。
func TestNicknameCaseMigrationMySQL(t *testing.T) {
	ctx := context.Background()
	m := openNicknameMySQL(t)
	full := findMigrations(t)
	if err := m.Migrate(ctx, migrationsBefore(t, full, "0009")); err != nil {
		t.Fatalf("迁移到 0008 失败: %v", err)
	}
	if _, err := m.DB().ExecContext(ctx,
		`INSERT INTO users (id, email, nickname, tag, password_hash) VALUES
		 (8101, 'mig-mysql@ex.com', 'Alice', '0001', '')`); err != nil {
		t.Fatalf("预置旧数据失败: %v", err)
	}
	// 旧语义：alice 命中 Alice，证明测试起点确实是 unicode_ci。
	if u, err := m.UserByNicknameTag(ctx, "alice", "0001"); err != nil || u.ID != 8101 {
		t.Fatalf("迁移前 alice 应命中 Alice: %+v %v", u, err)
	}

	if err := m.Migrate(ctx, full); err != nil {
		t.Fatalf("应用 0009 失败: %v", err)
	}
	if coll := nicknameCollation(t, m); coll != "utf8mb4_bin" {
		t.Fatalf("迁移后 nickname 排序规则应为 utf8mb4_bin，实际 %s", coll)
	}
	if u, err := m.UserByNicknameTag(ctx, "Alice", "0001"); err != nil || u.ID != 8101 {
		t.Fatalf("迁移后旧数据应可读: %+v %v", u, err)
	}
	if _, err := m.UserByNicknameTag(ctx, "alice", "0001"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("迁移后大小写变体不应命中，实际 %v", err)
	}

	// 迁移后大小写变体 + 同 tag 可以共存。
	lower := &User{Email: "mig-lower@ex.com", Nickname: "alice", Tag: "0001"}
	if err := m.CreateUser(ctx, lower); err != nil {
		t.Fatalf("迁移后创建 alice 失败: %v", err)
	}
	if u, err := m.UserByNicknameTag(ctx, "alice", "0001"); err != nil || u.ID != lower.ID {
		t.Fatalf("精确 alice 应命中新账号: %+v %v", u, err)
	}

	// down 在存在大小写变体时按文档失败（唯一索引重建撞 Duplicate entry）。
	if err := m.RollbackLast(ctx, full); !isDup(err) {
		t.Fatalf("存在大小写变体时回滚 0009 应因 Duplicate entry 失败，实际 %v", err)
	}
	// 处理后回滚成功，排序规则回到 unicode_ci，旧语义恢复。
	if _, err := m.DB().ExecContext(ctx,
		`DELETE FROM users WHERE nickname = ? AND tag = ?`, "alice", "0001"); err != nil {
		t.Fatalf("清理大小写变体失败: %v", err)
	}
	if err := m.RollbackLast(ctx, full); err != nil {
		t.Fatalf("回滚 0009 失败: %v", err)
	}
	if coll := nicknameCollation(t, m); coll != "utf8mb4_unicode_ci" {
		t.Fatalf("回滚后 nickname 排序规则应为 utf8mb4_unicode_ci，实际 %s", coll)
	}
	if u, err := m.UserByNicknameTag(ctx, "alice", "0001"); err != nil || u.ID != 8101 {
		t.Fatalf("回滚后应恢复大小写不敏感语义: %+v %v", u, err)
	}
	// 再次迁移确认可重入。
	if err := m.Migrate(ctx, full); err != nil {
		t.Fatalf("回滚后再迁移失败: %v", err)
	}
	if coll := nicknameCollation(t, m); coll != "utf8mb4_bin" {
		t.Fatalf("二次迁移后排序规则应为 utf8mb4_bin，实际 %s", coll)
	}
}

// nicknameCollation 读 users.nickname 的当前排序规则。
func nicknameCollation(t *testing.T, m *MySQL) string {
	t.Helper()
	var coll string
	if err := m.DB().QueryRow(
		`SELECT COLLATION_NAME FROM information_schema.COLUMNS
		  WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME = 'users' AND COLUMN_NAME = 'nickname'`).Scan(&coll); err != nil {
		t.Fatalf("读 nickname 排序规则失败: %v", err)
	}
	return coll
}
