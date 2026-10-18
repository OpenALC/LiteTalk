package store

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	_ "modernc.org/sqlite"
)

// openProbeDB 打开临时库（探针专用，直接摸 database/sql 层）：DSN 与 OpenSQLite 同款。
func openProbeDB(t *testing.T) *sql.DB {
	t.Helper()
	dsn := sqliteDSN(filepath.Join(t.TempDir(), "probe.db"))
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		t.Fatal(err)
	}
	// SQLite 单写者：连接池 >1 会放大 SQLITE_BUSY。
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = db.Close() })
	return db
}

// TestSQLiteTimeProbe modernc 时间行为探测：DATETIME 列的 time.Time 往返、
// CURRENT_TIMESTAMP 文本可解析、以及 UPDATE/upsert 的 RowsAffected 语义。
// 结论固化为断言——驱动升级改变行为会在这里先炸。
func TestSQLiteTimeProbe(t *testing.T) {
	db := openProbeDB(t)
	ctx := context.Background()
	if _, err := db.ExecContext(ctx, `CREATE TABLE probe (
		id INTEGER PRIMARY KEY, ts DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
		nt DATETIME NULL, v INT NOT NULL DEFAULT 0)`); err != nil {
		t.Fatal(err)
	}
	// 1) Go time.Time 写入 → time.Time 读回。
	now := time.Now().UTC().Truncate(time.Second)
	if _, err := db.ExecContext(ctx, `INSERT INTO probe (id, ts) VALUES (1, ?)`, now); err != nil {
		t.Fatal(err)
	}
	var got time.Time
	if err := db.QueryRowContext(ctx, `SELECT ts FROM probe WHERE id = 1`).Scan(&got); err != nil {
		t.Fatalf("DATETIME 扫描 time.Time: %v", err)
	}
	if !got.Equal(now) {
		t.Fatalf("时间往返失真: 写 %v 读 %v", now, got)
	}
	// 2) DEFAULT CURRENT_TIMESTAMP 文本也能扫描成 time.Time。
	if _, err := db.ExecContext(ctx, `INSERT INTO probe (id) VALUES (2)`); err != nil {
		t.Fatal(err)
	}
	var def time.Time
	if err := db.QueryRowContext(ctx, `SELECT ts FROM probe WHERE id = 2`).Scan(&def); err != nil {
		t.Fatalf("CURRENT_TIMESTAMP 扫描: %v", err)
	}
	if time.Since(def) > time.Minute || time.Since(def) < -time.Minute {
		t.Fatalf("DEFAULT CURRENT_TIMESTAMP 距今异常: %v", def)
	}
	// 3) NULL 扫描 sql.NullTime。
	var nt sql.NullTime
	if err := db.QueryRowContext(ctx, `SELECT nt FROM probe WHERE id = 1`).Scan(&nt); err != nil || nt.Valid {
		t.Fatalf("NULL 扫描: valid=%v err=%v", nt.Valid, err)
	}
	// 4) 同值 UPDATE 的 RowsAffected（SQLite 与 MySQL 不同，SQLite 计匹配行）。
	res, err := db.ExecContext(ctx, `UPDATE probe SET v = 0 WHERE id = 1`)
	if err != nil {
		t.Fatal(err)
	}
	n, _ := res.RowsAffected()
	t.Logf("同值 UPDATE RowsAffected=%d", n)
	if n != 1 {
		t.Fatalf("SQLite 同值 UPDATE 应计 1 行受影响，实际 %d", n)
	}
	// 5) ON CONFLICT DO UPDATE 同值时的 RowsAffected。
	if _, err := db.ExecContext(ctx, `CREATE TABLE up (a INT NOT NULL, b INT NOT NULL, PRIMARY KEY (a, b))`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO up (a, b) VALUES (1, 1)`); err != nil {
		t.Fatal(err)
	}
	res, err = db.ExecContext(ctx,
		`INSERT INTO up (a, b) VALUES (1, 1) ON CONFLICT(a, b) DO UPDATE SET b = excluded.b`)
	if err != nil {
		t.Fatal(err)
	}
	n, _ = res.RowsAffected()
	t.Logf("upsert 同值 RowsAffected=%d", n)
	if n != 1 {
		t.Fatalf("upsert 同值 DO UPDATE 应计 1 行，实际 %d", n)
	}
	// 6) 唯一冲突的错误文案（isDupSQLite 判定依据）。
	_, err = db.ExecContext(ctx, `INSERT INTO up (a, b) VALUES (1, 1)`)
	if err == nil {
		t.Fatal("期望唯一冲突")
	}
	t.Logf("dup err=%v", err)
	if !strings.Contains(err.Error(), "UNIQUE constraint failed") {
		t.Fatalf("冲突文案不含 UNIQUE constraint failed: %v", err)
	}
	// 7) _txlock=immediate：BeginTx 即 BEGIN IMMEDIATE（迁移互斥依赖它）。
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	// 第二个连接并发写应被 busy_timeout 挡住或立即拿到锁失败——这里只验证事务本身可开。
	if err := tx.Rollback(); err != nil && !errors.Is(err, sql.ErrTxDone) {
		t.Fatal(err)
	}
}
