package store

import (
	"context"
	"database/sql"
	"testing"
)

// 0010 回填验收（#35 B1）：旧迁移建库 → 预置 legacy 数据 → 应用 0010 →
// 断言默认角色/分配/legacy 列不变 → down 撤四表 → 再 up 仍幂等。
// 预置与断言用的 SQL 两方言通用。

// seedLegacy0010 预置 2 服 3 人（member/admin/owner 三种 legacy 角色都有）。
func seedLegacy0010(t *testing.T, db *sql.DB) {
	t.Helper()
	stmts := []string{
		`INSERT INTO users (id, email, nickname, tag, password_hash) VALUES
		 (8101, 'm8101@ex.test', 'm8101', '0001', 'h'),
		 (8102, 'm8102@ex.test', 'm8102', '0001', 'h'),
		 (8103, 'm8103@ex.test', 'm8103', '0001', 'h')`,
		`INSERT INTO servers (id, name, owner_id) VALUES
		 (8201, 'srv-a', 8103), (8202, 'srv-b', 8101)`,
		`INSERT INTO server_members (server_id, user_id, role, joined_at) VALUES
		 (8201, 8101, 'member', '2026-01-02 03:04:05'),
		 (8201, 8102, 'admin',  '2026-01-03 03:04:05'),
		 (8201, 8103, 'owner',  '2026-01-04 03:04:05'),
		 (8202, 8101, 'member', '2026-01-05 03:04:05')`,
	}
	for _, s := range stmts {
		if _, err := db.ExecContext(context.Background(), s); err != nil {
			t.Fatalf("预置失败: %v", err)
		}
	}
}

// assertBackfill0010 断言：每服三条 builtin 值正确、每个成员的分配符合 legacy、
// server_members.role 一字未动。
func assertBackfill0010(t *testing.T, db *sql.DB) {
	t.Helper()
	ctx := context.Background()

	type key struct {
		server, user int64
	}
	want := map[key][]string{
		{8201, 8101}: {"member"},
		{8201, 8102}: {"admin", "member"},
		{8201, 8103}: {"member", "owner"},
		{8202, 8101}: {"member"},
	}

	for _, srv := range []int64{8201, 8202} {
		rows, err := db.QueryContext(ctx,
			`SELECT builtin_key, name, position, permissions FROM roles
			  WHERE server_id = ? ORDER BY position`, srv)
		if err != nil {
			t.Fatal(err)
		}
		type row struct {
			key        string
			name       string
			pos, perms int
		}
		got := []row{}
		for rows.Next() {
			var r row
			if err := rows.Scan(&r.key, &r.name, &r.pos, &r.perms); err != nil {
				rows.Close()
				t.Fatal(err)
			}
			got = append(got, r)
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			t.Fatal(err)
		}
		rows.Close()
		exp := []row{
			{"member", "@everyone", 0, 8207},
			{"admin", "管理员", 1, 32767},
			{"owner", "服主", 2, 32767},
		}
		if len(got) != 3 {
			t.Fatalf("server %d 应 3 条 builtin，实际 %+v", srv, got)
		}
		for i := range exp {
			if got[i] != exp[i] {
				t.Fatalf("server %d builtin[%d] = %+v，期望 %+v", srv, i, got[i], exp[i])
			}
		}
	}

	for k, exp := range want {
		rows, err := db.QueryContext(ctx,
			`SELECT r.builtin_key FROM member_roles mr
			  JOIN roles r ON r.id = mr.role_id
			 WHERE mr.server_id = ? AND mr.user_id = ?
			 ORDER BY r.builtin_key`, k.server, k.user)
		if err != nil {
			t.Fatal(err)
		}
		got := []string{}
		for rows.Next() {
			var s string
			if err := rows.Scan(&s); err != nil {
				rows.Close()
				t.Fatal(err)
			}
			got = append(got, s)
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			t.Fatal(err)
		}
		rows.Close()
		if len(got) != len(exp) {
			t.Fatalf("(%d,%d) 分配 %v，期望 %v", k.server, k.user, got, exp)
		}
		for i := range exp {
			if got[i] != exp[i] {
				t.Fatalf("(%d,%d) 分配 %v，期望 %v", k.server, k.user, got, exp)
			}
		}
	}

	// legacy 列必须一字未动。
	legacy := map[int64]string{8101: "member", 8102: "admin", 8103: "owner"}
	for uid, exp := range legacy {
		var role string
		if err := db.QueryRowContext(ctx,
			`SELECT role FROM server_members WHERE server_id = 8201 AND user_id = ?`, uid).
			Scan(&role); err != nil {
			t.Fatal(err)
		}
		if role != exp {
			t.Fatalf("legacy role 被改动: user %d = %s，期望 %s", uid, role, exp)
		}
	}
}

// assertTablesGone0010 四张新表消失（down 只动新表）。
func assertTablesGone0010(t *testing.T, check func(table string) bool) {
	t.Helper()
	for _, tbl := range []string{"roles", "member_roles", "channel_overwrites", "audit_log"} {
		if check(tbl) {
			t.Fatalf("down 后 %s 应消失", tbl)
		}
	}
	// 旧表不动。
	if !check("server_members") || !check("servers") {
		t.Fatal("down 不得动旧表")
	}
}

// TestRolesMigrationSQLite 0010 回填 + down/up（SQLite 无条件跑）。
func TestRolesMigrationSQLite(t *testing.T) {
	ctx := context.Background()
	m := openTestSQLite(t)
	full := findMigrationsSQLite(t)
	if err := m.Migrate(ctx, migrationsBefore(t, full, "0010")); err != nil {
		t.Fatalf("迁移到 0009 失败: %v", err)
	}
	seedLegacy0010(t, m.DB())
	if err := m.Migrate(ctx, full); err != nil {
		t.Fatalf("应用 0010 失败: %v", err)
	}
	assertBackfill0010(t, m.DB())

	// down：四张新表消失、旧表与 legacy 数据保留。
	if err := m.RollbackLast(ctx, full); err != nil {
		t.Fatalf("down 失败: %v", err)
	}
	assertTablesGone0010(t, func(table string) bool {
		var n int
		if err := m.DB().QueryRowContext(ctx,
			`SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name=?`, table).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n > 0
	})

	// 再 up：重新回填（新行 id 由 rowid 分配，按 builtin_key 断言）。
	if err := m.Migrate(ctx, full); err != nil {
		t.Fatalf("再 up 失败: %v", err)
	}
	assertBackfill0010(t, m.DB())
}

// TestRolesMigrationMySQL 0010 回填 + down/up + 非事务重跑幂等（DSN 不可用时跳过）。
func TestRolesMigrationMySQL(t *testing.T) {
	ctx := context.Background()
	m := openRolesMySQL(t)
	full := findMigrations(t)
	if err := m.Migrate(ctx, migrationsBefore(t, full, "0010")); err != nil {
		t.Fatalf("迁移到 0009 失败: %v", err)
	}
	seedLegacy0010(t, m.DB())
	if err := m.Migrate(ctx, full); err != nil {
		t.Fatalf("应用 0010 失败: %v", err)
	}
	assertBackfill0010(t, m.DB())

	// 非事务重入：抹掉 0010 记账再跑整文件（CREATE IF NOT EXISTS + WHERE NOT EXISTS），
	// 回填不得写重、分配不得翻倍。
	if _, err := m.DB().ExecContext(ctx,
		`DELETE FROM schema_migrations WHERE version = '0010_roles_permissions'`); err != nil {
		t.Fatal(err)
	}
	if err := m.Migrate(ctx, full); err != nil {
		t.Fatalf("重跑 0010 失败: %v", err)
	}
	var roleCnt, assignCnt int
	if err := m.DB().QueryRowContext(ctx,
		`SELECT COUNT(*) FROM roles WHERE server_id = 8201`).Scan(&roleCnt); err != nil {
		t.Fatal(err)
	}
	if err := m.DB().QueryRowContext(ctx,
		`SELECT COUNT(*) FROM member_roles WHERE server_id = 8201`).Scan(&assignCnt); err != nil {
		t.Fatal(err)
	}
	// 8201 的分配：8101→member 1 条、8102→admin+member 2 条、8103→member+owner 2 条。
	if roleCnt != 3 || assignCnt != 5 {
		t.Fatalf("重跑应幂等: roles=%d member_roles=%d", roleCnt, assignCnt)
	}
	assertBackfill0010(t, m.DB())

	// down：四张新表消失、旧表保留。
	if err := m.RollbackLast(ctx, full); err != nil {
		t.Fatalf("down 失败: %v", err)
	}
	assertTablesGone0010(t, func(table string) bool {
		var n int
		if err := m.DB().QueryRowContext(ctx,
			`SELECT COUNT(*) FROM information_schema.tables
			  WHERE table_schema = DATABASE() AND table_name = ?`, table).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n > 0
	})

	// 再 up。
	if err := m.Migrate(ctx, full); err != nil {
		t.Fatalf("再 up 失败: %v", err)
	}
	assertBackfill0010(t, m.DB())
}
