package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io/fs"
	"strings"
	"sync"
	"testing"
	"testing/fstest"

	"github.com/OpenALC/LiteTalk/server/migrations"
	mysqldrv "github.com/go-sql-driver/mysql"
)

func seedOldEmailUser(t *testing.T, db *sql.DB, id int64, email string) {
	t.Helper()
	if _, err := db.ExecContext(context.Background(),
		`INSERT INTO users (id, email, nickname, tag, password_hash) VALUES (?, ?, ?, ?, '')`,
		id, email, "legacy", fmt.Sprintf("%04d", id%10000)); err != nil {
		t.Fatal(err)
	}
}

func assertEmailKeyCounts(t *testing.T, db *sql.DB, want int) {
	t.Helper()
	var users, keys, missing int
	if err := db.QueryRow(`SELECT COUNT(*) FROM users`).Scan(&users); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow(`SELECT COUNT(*) FROM user_email_keys`).Scan(&keys); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow(`SELECT COUNT(*) FROM users u LEFT JOIN user_email_keys k ON k.user_id = u.id
		WHERE k.user_id IS NULL`).Scan(&missing); err != nil {
		t.Fatal(err)
	}
	if users != want || keys != want || missing != 0 {
		t.Fatalf("users=%d keys=%d missing=%d, want %d", users, keys, missing, want)
	}
}

func assertEmailCreateRace(t *testing.T, st Store, db *sql.DB) {
	t.Helper()
	ctx := context.Background()
	var wg sync.WaitGroup
	start := make(chan struct{})
	errs := make([]error, 2)
	for i, email := range []string{"Δ.Test@Example.COM", "δ.test@example.com"} {
		wg.Add(1)
		go func(i int, email string) {
			defer wg.Done()
			<-start
			errs[i] = st.CreateUser(ctx, &User{Email: email, Nickname: "race", Tag: string(rune('1' + i)), PasswordHash: "h"})
		}(i, email)
	}
	close(start)
	wg.Wait()
	if !(errs[0] == nil && errors.Is(errs[1], ErrConflict) ||
		errs[1] == nil && errors.Is(errs[0], ErrConflict)) {
		t.Fatalf("同键并发应一胜一冲突: %v / %v", errs[0], errs[1])
	}
	assertEmailKeyCounts(t, db, 1)
	if got, err := st.UserByEmail(ctx, "δ.test@example.com"); err != nil || got == nil {
		t.Fatalf("规范键查询失败: %v", err)
	}
}

func assertEmailUpdateRace(t *testing.T, st Store, db *sql.DB) {
	t.Helper()
	ctx := context.Background()
	before, err := st.UserByEmail(ctx, "δ.test@example.com")
	if err != nil {
		t.Fatal(err)
	}
	update := *before
	update.Email = "other@example.com"
	var wg sync.WaitGroup
	var updateErr, createErr error
	wg.Add(2)
	go func() { defer wg.Done(); updateErr = st.UpdateUser(ctx, &update) }()
	go func() {
		defer wg.Done()
		createErr = st.CreateUser(ctx, &User{Email: "other@example.com", Nickname: "other", Tag: "1000"})
	}()
	wg.Wait()
	if updateErr != nil || createErr != nil {
		t.Fatalf("UpdateUser/注册并发失败: %v / %v", updateErr, createErr)
	}
	assertEmailKeyCounts(t, db, 2)
	if after, err := st.UserByEmail(ctx, before.Email); err != nil || after.ID != before.ID || after.Email != before.Email {
		t.Fatalf("UpdateUser 不应改变旧邮箱键: %+v %v", after, err)
	}
}

func TestEmailKeysSQLiteConflictAborts(t *testing.T) {
	ctx := context.Background()
	sq := openTestSQLite(t)
	full := findMigrationsSQLite(t)
	if err := sq.Migrate(ctx, migrationsBefore(t, full, "0023")); err != nil {
		t.Fatal(err)
	}
	seedOldEmailUser(t, sq.DB(), 91001, "Δ@Example.COM")
	seedOldEmailUser(t, sq.DB(), 91002, "δ@example.com")
	err := sq.Migrate(ctx, full)
	if !errors.Is(err, ErrEmailKeyConflict) || !strings.Contains(err.Error(), "91001") ||
		!strings.Contains(err.Error(), "91002") || strings.Contains(err.Error(), "Example") {
		t.Fatalf("冲突错误只应列内部 ID: %v", err)
	}
	var n int
	if err := sq.DB().QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name='user_email_keys'`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("冲突后不应建表: %d %v", n, err)
	}
	if err := sq.DB().QueryRow(`SELECT COUNT(*) FROM schema_migrations WHERE version = ?`, emailKeysVersion).Scan(&n); err != nil || n != 0 {
		t.Fatalf("冲突后不应标记迁移: %d %v", n, err)
	}
	if err := sq.DB().QueryRow(`SELECT COUNT(*) FROM users WHERE id IN (91001, 91002)`).Scan(&n); err != nil || n != 2 {
		t.Fatalf("冲突后旧账号不应变化: %d %v", n, err)
	}
}

func TestEmailKeysSQLiteUpgradeRollbackAndIntegrity(t *testing.T) {
	ctx := context.Background()
	sq := openTestSQLite(t)
	full := findMigrationsSQLite(t)
	if err := sq.Migrate(ctx, migrationsBefore(t, full, "0023")); err != nil {
		t.Fatal(err)
	}
	seedOldEmailUser(t, sq.DB(), 92001, "Δ.Test@Example.COM")
	if err := sq.Migrate(ctx, full); err != nil {
		t.Fatal(err)
	}
	assertEmailKeyCounts(t, sq.DB(), 1)
	got, err := sq.UserByEmail(ctx, "δ.test@example.com")
	if err != nil || got.ID != 92001 || got.Email != "Δ.Test@Example.COM" {
		t.Fatalf("旧邮箱原值或规范键查询改变: %+v %v", got, err)
	}
	if err := sq.RollbackLast(ctx, full); err != nil {
		t.Fatal(err)
	}
	if sq.emailKeysReady.Load() {
		t.Fatal("down 后仍启用键表")
	}
	if err := sq.Migrate(ctx, full); err != nil {
		t.Fatal(err)
	}
	assertEmailKeyCounts(t, sq.DB(), 1)
	if _, err := sq.DB().Exec(`DROP TABLE user_email_keys`); err != nil {
		t.Fatal(err)
	}
	if err := sq.Migrate(ctx, full); err == nil {
		t.Fatal("0023 已标记但键表缺失必须失败")
	}
}

func TestEmailKeysSQLiteConcurrentCreateAndUpdate(t *testing.T) {
	ctx := context.Background()
	sq := openTestSQLite(t)
	if err := sq.Migrate(ctx, findMigrationsSQLite(t)); err != nil {
		t.Fatal(err)
	}
	assertEmailCreateRace(t, sq, sq.DB())
	assertEmailUpdateRace(t, sq, sq.DB())
	if _, err := sq.DB().ExecContext(ctx, `DELETE FROM users WHERE email = ?`, "other@example.com"); err != nil {
		t.Fatal(err)
	}
	assertEmailKeyCounts(t, sq.DB(), 1)
	if err := sq.CreateUser(ctx, &User{Email: "other@example.com", Nickname: "reused", Tag: "2000"}); err != nil {
		t.Fatalf("删除用户后规范键应随 FK 释放: %v", err)
	}
}

func TestEmailKeysMemUpdateKeepsEmail(t *testing.T) {
	ctx := context.Background()
	mem := NewMem()
	u := &User{Email: "Δ@Example.COM", Nickname: "legacy", Tag: "0001"}
	if err := mem.CreateUser(ctx, u); err != nil {
		t.Fatal(err)
	}
	if err := mem.CreateUser(ctx, &User{Email: "δ@example.com", Nickname: "dup", Tag: "0002"}); !errors.Is(err, ErrConflict) {
		t.Fatalf("Mem 应按同一规范键去重: %v", err)
	}
	u.Email = "other@example.com"
	if err := mem.UpdateUser(ctx, u); err != nil {
		t.Fatal(err)
	}
	got, err := mem.UserByEmail(ctx, "δ@example.com")
	if err != nil || got.Email != "Δ@Example.COM" {
		t.Fatalf("Mem UpdateUser 应保留旧邮箱: %+v %v", got, err)
	}
}

func TestEmailKeysMySQLReentryAndRollback(t *testing.T) {
	ctx := context.Background()
	m := openRolesMySQL(t)
	full := findMigrations(t)
	if err := m.Migrate(ctx, migrationsBefore(t, full, "0023")); err != nil {
		t.Fatal(err)
	}
	seedOldEmailUser(t, m.DB(), 93001, "Legacy.Mixed@Example.COM")
	// 模拟 MySQL DDL 已提交、回填事务尚未提交时中断。
	up, err := fs.ReadFile(migrations.FS, "0023_user_email_keys.up.sql")
	if err != nil {
		t.Fatal(err)
	}
	stmts, err := splitSQL(string(up))
	if err != nil || len(stmts) != 1 {
		t.Fatalf("0023 必须只有一条可重入 DDL: %d %v", len(stmts), err)
	}
	if _, err := m.DB().ExecContext(ctx, stmts[0]); err != nil {
		t.Fatal(err)
	}
	if err := m.Migrate(ctx, full); err != nil {
		t.Fatal(err)
	}
	assertEmailKeyCounts(t, m.DB(), 1)
	if err := m.RollbackLast(ctx, full); err != nil {
		t.Fatal(err)
	}
	if err := m.Migrate(ctx, full); err != nil {
		t.Fatal(err)
	}
	assertEmailKeyCounts(t, m.DB(), 1)
}

func TestEmailKeysMySQLConcurrentCreate(t *testing.T) {
	ctx := context.Background()
	m := openRolesMySQL(t)
	if err := m.Migrate(ctx, findMigrations(t)); err != nil {
		t.Fatal(err)
	}
	var oldIndex int
	if err := m.DB().QueryRowContext(ctx, `SELECT COUNT(*) FROM information_schema.statistics
		WHERE table_schema = DATABASE() AND table_name = 'users' AND index_name = 'uk_users_email'`).Scan(&oldIndex); err != nil || oldIndex == 0 {
		t.Fatalf("原有 MySQL 邮箱唯一索引不得放松: %d %v", oldIndex, err)
	}
	assertEmailCreateRace(t, m, m.DB())
	assertEmailUpdateRace(t, m, m.DB())
	if _, err := m.DB().ExecContext(ctx, `DELETE FROM users WHERE email = ?`, "other@example.com"); err != nil {
		t.Fatal(err)
	}
	assertEmailKeyCounts(t, m.DB(), 1)
	if err := m.CreateUser(ctx, &User{Email: "other@example.com", Nickname: "reused", Tag: "2000"}); err != nil {
		t.Fatalf("删除用户后规范键应随 FK 释放: %v", err)
	}
}

func TestEmailKeysMySQLConflictAborts(t *testing.T) {
	ctx := context.Background()
	m := openRolesMySQL(t)
	full := findMigrations(t)
	if err := m.Migrate(ctx, migrationsBefore(t, full, "0023")); err != nil {
		t.Fatal(err)
	}
	seedOldEmailUser(t, m.DB(), 94001, "A B@example.com")
	seedOldEmailUser(t, m.DB(), 94002, "ab@example.com")
	err := m.Migrate(ctx, full)
	if !errors.Is(err, ErrEmailKeyConflict) || !strings.Contains(err.Error(), "94001") ||
		!strings.Contains(err.Error(), "94002") || strings.Contains(err.Error(), "example.com") {
		t.Fatalf("冲突错误只应列内部 ID: %v", err)
	}
	var n int
	if err := m.DB().QueryRow(`SELECT COUNT(*) FROM information_schema.tables
		WHERE table_schema = DATABASE() AND table_name = 'user_email_keys'`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("冲突后不应建表: %d %v", n, err)
	}
	if err := m.DB().QueryRow(`SELECT COUNT(*) FROM users WHERE id IN (94001, 94002)`).Scan(&n); err != nil || n != 2 {
		t.Fatalf("冲突后账号不应变化: %d %v", n, err)
	}
}

func TestEmailKeysSQLiteRejectsSameNameWrongSchema(t *testing.T) {
	cases := map[string]string{
		"缺主键": `CREATE TABLE user_email_keys (
			email_key BLOB NOT NULL, user_id INTEGER NOT NULL UNIQUE REFERENCES users(id) ON DELETE CASCADE)`,
		"缺用户唯一键": `CREATE TABLE user_email_keys (
			email_key BLOB NOT NULL PRIMARY KEY, user_id INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE)`,
		"缺外键": `CREATE TABLE user_email_keys (
			email_key BLOB NOT NULL PRIMARY KEY, user_id INTEGER NOT NULL UNIQUE)`,
		"非二进制列": `CREATE TABLE user_email_keys (
			email_key TEXT NOT NULL PRIMARY KEY, user_id INTEGER NOT NULL UNIQUE REFERENCES users(id) ON DELETE CASCADE)`,
	}
	for name, ddl := range cases {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			sq := openTestSQLite(t)
			full := findMigrationsSQLite(t)
			if err := sq.Migrate(ctx, migrationsBefore(t, full, "0023")); err != nil {
				t.Fatal(err)
			}
			if _, err := sq.DB().ExecContext(ctx, ddl); err != nil {
				t.Fatal(err)
			}
			if err := sq.Migrate(ctx, full); err == nil || !strings.Contains(err.Error(), "结构不符") {
				t.Fatalf("同名错表应拒绝: %v", err)
			}
			var n int
			if err := sq.DB().QueryRowContext(ctx,
				`SELECT COUNT(*) FROM schema_migrations WHERE version = ?`, emailKeysVersion).Scan(&n); err != nil || n != 0 {
				t.Fatalf("错表不应标记版本: %d %v", n, err)
			}
		})
	}
}

func TestEmailKeysSQLiteRejectsAppliedCorruption(t *testing.T) {
	for name, mutation := range map[string]string{
		"缺表": `DROP TABLE user_email_keys`,
		"无键": `DELETE FROM user_email_keys`,
		"错键": `UPDATE user_email_keys SET email_key = x'77726f6e67'`,
	} {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			sq := openTestSQLite(t)
			full := findMigrationsSQLite(t)
			if err := sq.Migrate(ctx, full); err != nil {
				t.Fatal(err)
			}
			id := int64(97001)
			seed := &User{ID: id, Email: "Name@Example.com", Nickname: "name", Tag: "0001"}
			if err := sq.CreateUser(ctx, seed); err != nil {
				t.Fatal(err)
			}
			if _, err := sq.DB().ExecContext(ctx, mutation); err != nil {
				t.Fatal(err)
			}
			err := sq.Migrate(ctx, full)
			if err == nil || name != "缺表" && !strings.Contains(err.Error(), "97001") {
				t.Fatalf("0023 已标记但内容损坏应拒绝且只报 ID: %v", err)
			}
			if strings.Contains(err.Error(), "Name@") || strings.Contains(err.Error(), "name@") {
				t.Fatalf("损坏错误不得暴露邮箱: %v", err)
			}
			if name == "缺表" {
				createErr := sq.CreateUser(ctx, &User{Email: "secret@example.com", Nickname: "new", Tag: "0002"})
				if createErr == nil || !strings.Contains(createErr.Error(), "sqlite code=") ||
					strings.Contains(createErr.Error(), "secret") {
					t.Fatalf("写键失败应保留安全错误号且不泄露邮箱: %v", createErr)
				}
				var count int
				if err := sq.DB().QueryRowContext(ctx, `SELECT COUNT(*) FROM users`).Scan(&count); err != nil || count != 1 {
					t.Fatalf("写键失败应回滚用户插入: %d %v", count, err)
				}
			}
		})
	}
}

func TestEmailKeysMySQLRejectsSameNameWrongSchema(t *testing.T) {
	cases := map[string]string{
		"缺主键": `CREATE TABLE user_email_keys (
			email_key VARBINARY(1020) NOT NULL, user_id BIGINT NOT NULL UNIQUE,
			FOREIGN KEY (user_id) REFERENCES users(id) ON DELETE CASCADE) ENGINE=InnoDB`,
		"缺用户唯一键": `CREATE TABLE user_email_keys (
			email_key VARBINARY(1020) NOT NULL PRIMARY KEY, user_id BIGINT NOT NULL,
			FOREIGN KEY (user_id) REFERENCES users(id) ON DELETE CASCADE) ENGINE=InnoDB`,
		"缺外键": `CREATE TABLE user_email_keys (
			email_key VARBINARY(1020) NOT NULL PRIMARY KEY, user_id BIGINT NOT NULL UNIQUE) ENGINE=InnoDB`,
		"非二进制列": `CREATE TABLE user_email_keys (
			email_key VARCHAR(255) NOT NULL PRIMARY KEY, user_id BIGINT NOT NULL UNIQUE,
			FOREIGN KEY (user_id) REFERENCES users(id) ON DELETE CASCADE) ENGINE=InnoDB`,
	}
	for name, ddl := range cases {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			m := openRolesMySQL(t)
			full := findMigrations(t)
			if err := m.Migrate(ctx, migrationsBefore(t, full, "0023")); err != nil {
				t.Fatal(err)
			}
			if _, err := m.DB().ExecContext(ctx, ddl); err != nil {
				t.Fatal(err)
			}
			if err := m.Migrate(ctx, full); err == nil || !strings.Contains(err.Error(), "结构不符") {
				t.Fatalf("同名错表应拒绝: %v", err)
			}
			var n int
			if err := m.DB().QueryRowContext(ctx,
				`SELECT COUNT(*) FROM schema_migrations WHERE version = ?`, emailKeysVersion).Scan(&n); err != nil || n != 0 {
				t.Fatalf("错表不应标记版本: %d %v", n, err)
			}
		})
	}
}

func TestEmailKeysMySQLRejectsAppliedCorruption(t *testing.T) {
	for name, mutation := range map[string]string{
		"缺表": `DROP TABLE user_email_keys`,
		"无键": `DELETE FROM user_email_keys`,
		"错键": `UPDATE user_email_keys SET email_key = x'77726f6e67'`,
	} {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			m := openRolesMySQL(t)
			full := findMigrations(t)
			if err := m.Migrate(ctx, full); err != nil {
				t.Fatal(err)
			}
			if err := m.CreateUser(ctx, &User{ID: 98001, Email: "Name@Example.com", Nickname: "name", Tag: "0001"}); err != nil {
				t.Fatal(err)
			}
			if _, err := m.DB().ExecContext(ctx, mutation); err != nil {
				t.Fatal(err)
			}
			err := m.Migrate(ctx, full)
			if err == nil || name != "缺表" && !strings.Contains(err.Error(), "98001") {
				t.Fatalf("0023 已标记但内容损坏应拒绝且只报 ID: %v", err)
			}
			if strings.Contains(err.Error(), "Name@") || strings.Contains(err.Error(), "name@") {
				t.Fatalf("损坏错误不得暴露邮箱: %v", err)
			}
			if name == "缺表" {
				createErr := m.CreateUser(ctx, &User{Email: "secret@example.com", Nickname: "new", Tag: "0002"})
				if createErr == nil || !strings.Contains(createErr.Error(), "mysql errno=") ||
					strings.Contains(createErr.Error(), "secret") {
					t.Fatalf("写键失败应保留安全错误号且不泄露邮箱: %v", createErr)
				}
				var count int
				if err := m.DB().QueryRowContext(ctx, `SELECT COUNT(*) FROM users`).Scan(&count); err != nil || count != 1 {
					t.Fatalf("写键失败应回滚用户插入: %d %v", count, err)
				}
			}
		})
	}
}

func TestSafeEmailDBErrorOmitsDuplicateKey(t *testing.T) {
	err := &mysqldrv.MySQLError{Number: 1062, Message: "duplicate email secret@example.com"}
	got := safeEmailDBError(err)
	if !strings.Contains(got, "1062") || strings.Contains(got, "secret") || strings.Contains(got, "example.com") {
		t.Fatalf("诊断只允许错误号/SQLSTATE: %q", got)
	}
}

func emailKeySentinelMigrations() fs.FS {
	return fstest.MapFS{
		"0024_sentinel.up.sql": &fstest.MapFile{Data: []byte(`CREATE TABLE email_migration_sentinel (id INTEGER PRIMARY KEY);
			INSERT INTO email_migration_sentinel (id) VALUES (1);`)},
		"0024_sentinel.down.sql": &fstest.MapFile{Data: []byte(`DROP TABLE email_migration_sentinel;`)},
	}
}

func TestEmailKeysSQLiteCorruptionBlocksLaterMigration(t *testing.T) {
	ctx := context.Background()
	sq := openTestSQLite(t)
	if err := sq.Migrate(ctx, findMigrationsSQLite(t)); err != nil {
		t.Fatal(err)
	}
	if err := sq.CreateUser(ctx, &User{ID: 99001, Email: "Name@Example.com", Nickname: "name", Tag: "0001"}); err != nil {
		t.Fatal(err)
	}
	if _, err := sq.DB().ExecContext(ctx, `UPDATE user_email_keys SET email_key = x'77726f6e67'`); err != nil {
		t.Fatal(err)
	}
	err := sq.Migrate(ctx, emailKeySentinelMigrations())
	if err == nil || !strings.Contains(err.Error(), "99001") || strings.Contains(err.Error(), "Example") {
		t.Fatalf("应在 0024 前拒绝 0023 错键，且只报 ID: %v", err)
	}
	var n int
	if err := sq.DB().QueryRowContext(ctx,
		`SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = 'email_migration_sentinel'`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("后续 0024 DDL 不得执行: %d %v", n, err)
	}
	if err := sq.DB().QueryRowContext(ctx,
		`SELECT COUNT(*) FROM schema_migrations WHERE version = '0024_sentinel'`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("后续 0024 不得记版本: %d %v", n, err)
	}
}

func TestEmailKeysMySQLCorruptionBlocksLaterMigration(t *testing.T) {
	ctx := context.Background()
	m := openRolesMySQL(t)
	if err := m.Migrate(ctx, findMigrations(t)); err != nil {
		t.Fatal(err)
	}
	if err := m.CreateUser(ctx, &User{ID: 99002, Email: "Name@Example.com", Nickname: "name", Tag: "0001"}); err != nil {
		t.Fatal(err)
	}
	if _, err := m.DB().ExecContext(ctx, `UPDATE user_email_keys SET email_key = x'77726f6e67'`); err != nil {
		t.Fatal(err)
	}
	err := m.Migrate(ctx, emailKeySentinelMigrations())
	if err == nil || !strings.Contains(err.Error(), "99002") || strings.Contains(err.Error(), "Example") {
		t.Fatalf("应在 0024 前拒绝 0023 错键，且只报 ID: %v", err)
	}
	var n int
	if err := m.DB().QueryRowContext(ctx, `SELECT COUNT(*) FROM information_schema.tables
		WHERE table_schema = DATABASE() AND table_name = 'email_migration_sentinel'`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("后续 0024 DDL 不得执行: %d %v", n, err)
	}
	if err := m.DB().QueryRowContext(ctx,
		`SELECT COUNT(*) FROM schema_migrations WHERE version = '0024_sentinel'`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("后续 0024 不得记版本: %d %v", n, err)
	}
}
