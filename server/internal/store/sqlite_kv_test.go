package store

import (
	"context"
	"database/sql"
	"errors"
	"io/fs"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/OpenALC/LiteTalk/server/migrations"
)

// openTestSQLiteKV 打开已迁移的临时 SQLite 库并与 Store 共用一个连接池。
func openTestSQLiteKV(t *testing.T) (*SQLite, *SQLiteKV) {
	t.Helper()
	sq := openTestSQLite(t)
	if err := sq.Migrate(context.Background(), findMigrationsSQLite(t)); err != nil {
		t.Fatalf("迁移失败: %v", err)
	}
	return sq, NewSQLiteKV(sq.DB())
}

// TestSQLiteKVSetGetDel Get/Set/Del 基本语义：未命中、覆盖写、批量删、幂等删。
func TestSQLiteKVSetGetDel(t *testing.T) {
	_, kv := openTestSQLiteKV(t)
	ctx := context.Background()

	if _, err := kv.Get(ctx, "missing"); !errors.Is(err, ErrKVNotFound) {
		t.Fatalf("未命中应 ErrKVNotFound，实际 %v", err)
	}
	if err := kv.Set(ctx, "a", "1", 0); err != nil {
		t.Fatalf("Set: %v", err)
	}
	if err := kv.Set(ctx, "b", "2", time.Hour); err != nil {
		t.Fatalf("Set: %v", err)
	}
	if got, err := kv.Get(ctx, "a"); err != nil || got != "1" {
		t.Fatalf("Get(a) = %q/%v，want 1/nil", got, err)
	}
	if err := kv.Set(ctx, "a", "3", 0); err != nil {
		t.Fatalf("覆盖写: %v", err)
	}
	if got, err := kv.Get(ctx, "a"); err != nil || got != "3" {
		t.Fatalf("覆盖后 Get(a) = %q/%v，want 3/nil", got, err)
	}
	if err := kv.Del(ctx, "a", "b"); err != nil {
		t.Fatalf("Del: %v", err)
	}
	for _, k := range []string{"a", "b"} {
		if _, err := kv.Get(ctx, k); !errors.Is(err, ErrKVNotFound) {
			t.Fatalf("删除后 Get(%s) 应未命中，实际 %v", k, err)
		}
	}
	if err := kv.Del(ctx); err != nil {
		t.Fatalf("空删除应为 no-op: %v", err)
	}
	if err := kv.Del(ctx, "a", "b"); err != nil {
		t.Fatalf("重复删除应幂等: %v", err)
	}
}

// TestSQLiteKVTTL ttl>0 写 expires_at，ttl=0 永不过期；真实过期后 Get 未命中并惰性删行。
func TestSQLiteKVTTL(t *testing.T) {
	sq, kv := openTestSQLiteKV(t)
	ctx := context.Background()

	if err := kv.Set(ctx, "forever", "v", 0); err != nil {
		t.Fatal(err)
	}
	if exp := readKVExpiry(t, sq, "forever"); exp.Valid {
		t.Fatalf("ttl=0 应写 NULL，实际 %v", exp.Time)
	}

	before := time.Now()
	if err := kv.Set(ctx, "hour", "v", time.Hour); err != nil {
		t.Fatal(err)
	}
	exp := readKVExpiry(t, sq, "hour")
	if !exp.Valid || exp.Time.Before(before.Add(59*time.Minute)) || exp.Time.After(before.Add(61*time.Minute)) {
		t.Fatalf("ttl=1h 的 expires_at 不在预期区间: %v", exp)
	}

	// SQLite 驱动的时间格式只到秒，短 TTL 会被截断到整秒——这里用 1s TTL 再等过
	// 整秒，验证的是真实的过期路径而不是手工回填的过期行。
	if err := kv.Set(ctx, "short", "v", time.Second); err != nil {
		t.Fatal(err)
	}
	time.Sleep(1200 * time.Millisecond)
	if _, err := kv.Get(ctx, "short"); !errors.Is(err, ErrKVNotFound) {
		t.Fatalf("过期后 Get 应未命中，实际 %v", err)
	}
	var n int
	if err := sq.DB().QueryRowContext(ctx, `SELECT COUNT(*) FROM kv WHERE kv_key = 'short'`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("过期键应被惰性删除，实际仍剩 %d 行", n)
	}
	// 永久键不受影响。
	if got, err := kv.Get(ctx, "forever"); err != nil || got != "v" {
		t.Fatalf("永久键应仍在: %q/%v", got, err)
	}
}

// TestSQLiteKVIncrTTL 首次自增设置 TTL，后续沿用；过期键按不存在重新从 1 开始。
func TestSQLiteKVIncrTTL(t *testing.T) {
	sq, kv := openTestSQLiteKV(t)
	ctx := context.Background()

	n, err := kv.Incr(ctx, "count", time.Hour)
	if err != nil || n != 1 {
		t.Fatalf("首次 Incr = %d/%v，want 1/nil", n, err)
	}
	first := readKVExpiry(t, sq, "count")
	if !first.Valid {
		t.Fatal("首次 Incr 应设置 TTL")
	}
	if n, err = kv.Incr(ctx, "count", time.Millisecond); err != nil || n != 2 {
		t.Fatalf("第二次 Incr = %d/%v，want 2/nil", n, err)
	}
	if second := readKVExpiry(t, sq, "count"); !second.Valid || !second.Time.Equal(first.Time) {
		t.Fatalf("已有键的 Incr 不应改 TTL：first=%v second=%v", first.Time, second.Time)
	}

	// 手工把 expires_at 回填到过去：过期键视作不存在，重新从 1 开始并重设 TTL。
	if _, err := sq.DB().ExecContext(ctx,
		`UPDATE kv SET expires_at = ? WHERE kv_key = 'count'`, time.Now().Add(-time.Minute).UTC()); err != nil {
		t.Fatal(err)
	}
	if n, err = kv.Incr(ctx, "count", time.Hour); err != nil || n != 1 {
		t.Fatalf("过期后 Incr = %d/%v，want 1/nil", n, err)
	}
	if exp := readKVExpiry(t, sq, "count"); !exp.Valid || exp.Time.Before(time.Now()) {
		t.Fatalf("过期后 Incr 应重设未来 TTL，实际 %v", exp)
	}

	// 非数字值按 0 处理（与 MemKV 口径一致）。
	if err := kv.Set(ctx, "junk", "abc", 0); err != nil {
		t.Fatal(err)
	}
	if n, err = kv.Incr(ctx, "junk", 0); err != nil || n != 1 {
		t.Fatalf("非数字值 Incr = %d/%v，want 1/nil", n, err)
	}
}

// TestSQLiteKVIncrCap 限幅边界：新值超过 cap 不落库，返回 cap+1。
func TestSQLiteKVIncrCap(t *testing.T) {
	_, kv := openTestSQLiteKV(t)
	ctx := context.Background()

	want := []int64{1, 2, 3, 4, 4}
	for i, w := range want {
		n, err := kv.IncrCap(ctx, "rl", time.Hour, 3)
		if err != nil || n != w {
			t.Fatalf("第 %d 次 IncrCap = %d/%v，want %d/nil", i+1, n, err, w)
		}
	}
	got, err := kv.Get(ctx, "rl")
	if err != nil || got != "3" {
		t.Fatalf("超限后计数应停在 3，实际 %q/%v", got, err)
	}
	if capZero, err := kv.IncrCap(ctx, "zero", 0, 0); err != nil || capZero != 1 {
		t.Fatalf("cap=0 时 IncrCap = %d/%v，want 1/nil", capZero, err)
	}
}

// TestSQLiteKVEvalCAS CAS 语义：值匹配才替换并重置 TTL；不匹配/不存在/过期均为 0。
func TestSQLiteKVEvalCAS(t *testing.T) {
	sq, kv := openTestSQLiteKV(t)
	ctx := context.Background()

	if err := kv.Set(ctx, "rt", "old", time.Hour); err != nil {
		t.Fatal(err)
	}
	res, err := kv.Eval(ctx, CASScript, []string{"rt"}, "old", "new", "60")
	if err != nil {
		t.Fatalf("Eval: %v", err)
	}
	if n, _ := res.(int64); n != 1 {
		t.Fatalf("CAS 命中应返回 1，实际 %v", res)
	}
	if got, err := kv.Get(ctx, "rt"); err != nil || got != "new" {
		t.Fatalf("CAS 后 Get = %q/%v，want new/nil", got, err)
	}
	// TTL 重置为 60 秒。
	exp := readKVExpiry(t, sq, "rt")
	if !exp.Valid || exp.Time.Before(time.Now().Add(50*time.Second)) || exp.Time.After(time.Now().Add(70*time.Second)) {
		t.Fatalf("CAS 应把 TTL 重置为约 60s，实际 %v", exp)
	}

	res, err = kv.Eval(ctx, CASScript, []string{"rt"}, "old", "again", "60")
	if err != nil {
		t.Fatal(err)
	}
	if n, _ := res.(int64); n != 0 {
		t.Fatalf("旧值已不匹配，CAS 应返回 0，实际 %v", res)
	}
	if got, _ := kv.Get(ctx, "rt"); got != "new" {
		t.Fatalf("CAS 失败不应改值，实际 %q", got)
	}

	// 不存在与过期键：比对失败。
	for _, key := range []string{"absent", "expired"} {
		if key == "expired" {
			if err := kv.Set(ctx, key, "old", time.Hour); err != nil {
				t.Fatal(err)
			}
			if _, err := sq.DB().ExecContext(ctx,
				`UPDATE kv SET expires_at = ? WHERE kv_key = ?`, time.Now().Add(-time.Minute).UTC(), key); err != nil {
				t.Fatal(err)
			}
		}
		res, err = kv.Eval(ctx, CASScript, []string{key}, "old", "new", "60")
		if err != nil {
			t.Fatal(err)
		}
		if n, _ := res.(int64); n != 0 {
			t.Fatalf("%s 的 CAS 应返回 0，实际 %v", key, res)
		}
	}

	// fail-closed：未知脚本与参数个数错误都报错。
	if _, err := kv.Eval(ctx, "return redis.call('DEL', KEYS[1])", []string{"rt"}, "x"); err == nil {
		t.Fatal("未知脚本应报错")
	}
	if _, err := kv.Eval(ctx, CASScript, []string{"rt", "rt2"}, "a", "b", "1"); err == nil {
		t.Fatal("key 个数错误应报错")
	}
	if _, err := kv.Eval(ctx, CASScript, []string{"rt"}, "a", "b"); err == nil {
		t.Fatal("参数个数错误应报错")
	}
	if _, err := kv.Eval(ctx, CASScript, []string{"rt"}, "a", "b", "not-a-number"); err == nil {
		t.Fatal("非法 TTL 应报错")
	}
}

// TestSQLiteKVCASConcurrent 并发 CAS 只有一个成功（刷新令牌旋转的防重放前提）。
func TestSQLiteKVCASConcurrent(t *testing.T) {
	_, kv := openTestSQLiteKV(t)
	ctx := context.Background()
	if err := kv.Set(ctx, "rt", "old", time.Hour); err != nil {
		t.Fatal(err)
	}

	const n = 8
	results := make([]int64, n)
	errs := make([]error, n)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			res, err := kv.Eval(ctx, CASScript, []string{"rt"}, "old", "new", "60")
			errs[i] = err
			if v, ok := res.(int64); ok {
				results[i] = v
			}
		}(i)
	}
	wg.Wait()

	okCount := 0
	for i := range results {
		if errs[i] != nil {
			t.Fatalf("goroutine %d: %v", i, errs[i])
		}
		if results[i] == 1 {
			okCount++
		} else if results[i] != 0 {
			t.Fatalf("goroutine %d 返回 %d，want 0/1", i, results[i])
		}
	}
	if okCount != 1 {
		t.Fatalf("应恰有 1 个 CAS 成功，实际 %d", okCount)
	}
	if got, _ := kv.Get(ctx, "rt"); got != "new" {
		t.Fatalf("并发后值应为 new，实际 %q", got)
	}
}

// TestSQLiteKVMigrationRoundTrip 0006 可正向迁移、可回滚、可再次迁移。
func TestSQLiteKVMigrationRoundTrip(t *testing.T) {
	sq := openTestSQLite(t)
	ctx := context.Background()
	migFS := findMigrationsSQLite(t)
	if err := sq.Migrate(ctx, migFS); err != nil {
		t.Fatalf("迁移失败: %v", err)
	}
	assertKVTable(t, sq, true)

	if err := sq.RollbackLast(ctx, migFS); err != nil {
		t.Fatalf("回滚 0006 失败: %v", err)
	}
	assertKVTable(t, sq, false)

	if err := sq.Migrate(ctx, migFS); err != nil {
		t.Fatalf("再次迁移失败: %v", err)
	}
	assertKVTable(t, sq, true)
}

// TestKVMigrationsDialectAligned 两套方言的 0006 必须成对存在且列一致。
// MySQL 运行期由 TestMySQLMigrateAndCRUD 在有 DSN 时覆盖（CI 无 MySQL 会跳过）；
// 这里做静态比对，防止两套迁移漂移。
func TestKVMigrationsDialectAligned(t *testing.T) {
	read := func(name string) string {
		t.Helper()
		body, err := fs.ReadFile(migrations.FS, name)
		if err != nil {
			t.Fatalf("读取 %s: %v", name, err)
		}
		return string(body)
	}
	myUp := read("0006_kv.up.sql")
	sqUp := read("sqlite/0006_kv.up.sql")
	for _, col := range []string{"kv_key", "kv_value", "expires_at"} {
		if !strings.Contains(myUp, col) || !strings.Contains(sqUp, col) {
			t.Fatalf("两方言都应包含列 %s\nMySQL:\n%s\nSQLite:\n%s", col, myUp, sqUp)
		}
	}
	for _, body := range []string{myUp, sqUp} {
		if !strings.Contains(body, "CREATE TABLE kv") {
			t.Fatalf("up 迁移应建 kv 表:\n%s", body)
		}
	}
	for _, name := range []string{"0006_kv.down.sql", "sqlite/0006_kv.down.sql"} {
		if body := read(name); !strings.Contains(body, "DROP TABLE IF EXISTS kv") {
			t.Fatalf("%s 应删除 kv 表:\n%s", name, body)
		}
	}
}

func readKVExpiry(t *testing.T, sq *SQLite, key string) sql.NullTime {
	t.Helper()
	var exp sql.NullTime
	if err := sq.DB().QueryRow(`SELECT expires_at FROM kv WHERE kv_key = ?`, key).Scan(&exp); err != nil {
		t.Fatalf("读取 %s 的 expires_at: %v", key, err)
	}
	return exp
}

func assertKVTable(t *testing.T, sq *SQLite, want bool) {
	t.Helper()
	var n int
	if err := sq.DB().QueryRow(
		`SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name='kv'`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if got := n > 0; got != want {
		t.Fatalf("kv 表存在 = %v，want %v", got, want)
	}
}
