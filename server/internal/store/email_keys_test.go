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

	"github.com/OpenALC/LiteTalk/server/migrations"
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
