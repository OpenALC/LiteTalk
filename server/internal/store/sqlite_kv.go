package store

import (
	"context"
	"database/sql"
	"errors"
	"strconv"
	"strings"
	"time"
)

// SQLiteKV 是 KV 的 SQLite 实现（#83）：与 SQLite Store 共用同一个 *sql.DB
// （同一个库文件、同一个连接池），不额外打开数据库文件。
//
// 过期语义与 Redis/MemKV 对齐：Get 命中过期键按未命中处理并惰性删除；
// Incr/IncrCap/Eval 在事务内把过期键当作不存在，首次写入时设置 TTL、
// 已存在的键沿用原 expires_at。SQLite 单写者 + 库 DSN 的 BEGIN IMMEDIATE
// 使事务串行，CAS 的「读-比对-写」不会被其他写插入。
type SQLiteKV struct {
	db *sql.DB
}

// NewSQLiteKV 基于已打开的 SQLite 连接池构造 KV。db 由调用方持有并负责关闭。
func NewSQLiteKV(db *sql.DB) *SQLiteKV { return &SQLiteKV{db: db} }

// sqKVUpsert 覆盖写。expires_at 为 NULL 表示永不过期。
const sqKVUpsert = `INSERT INTO kv (kv_key, kv_value, expires_at) VALUES (?, ?, ?)
ON CONFLICT(kv_key) DO UPDATE SET kv_value = excluded.kv_value, expires_at = excluded.expires_at`

// kvExpiry ttl > 0 时返回 UTC 到期时刻，否则返回 nil（永不过期）。
// 统一写 UTC：MySQL DATETIME 与 SQLite DATETIME 都不带时区，比较依赖两边同为 UTC。
func kvExpiry(ttl time.Duration) any {
	if ttl > 0 {
		return time.Now().Add(ttl).UTC()
	}
	return nil
}

func (k *SQLiteKV) Get(ctx context.Context, key string) (string, error) {
	var value string
	var exp sql.NullTime
	err := k.db.QueryRowContext(ctx,
		`SELECT kv_value, expires_at FROM kv WHERE kv_key = ?`, key).Scan(&value, &exp)
	if errors.Is(err, sql.ErrNoRows) {
		return "", ErrKVNotFound
	}
	if err != nil {
		return "", err
	}
	if kvExpired(exp) {
		// 只删「仍处于过期状态」的行：并发 Set 刷新出的新行不能被这次清理误删。
		_, _ = k.db.ExecContext(ctx,
			`DELETE FROM kv WHERE kv_key = ? AND expires_at IS NOT NULL AND expires_at <= ?`,
			key, time.Now().UTC())
		return "", ErrKVNotFound
	}
	return value, nil
}

func (k *SQLiteKV) Set(ctx context.Context, key, value string, ttl time.Duration) error {
	_, err := k.db.ExecContext(ctx, sqKVUpsert, key, value, kvExpiry(ttl))
	return err
}

func (k *SQLiteKV) Del(ctx context.Context, keys ...string) error {
	if len(keys) == 0 {
		return nil
	}
	ph := strings.TrimSuffix(strings.Repeat("?,", len(keys)), ",")
	args := make([]any, len(keys))
	for i, key := range keys {
		args[i] = key
	}
	_, err := k.db.ExecContext(ctx, `DELETE FROM kv WHERE kv_key IN (`+ph+`)`, args...)
	return err
}

func (k *SQLiteKV) Incr(ctx context.Context, key string, ttl time.Duration) (int64, error) {
	return k.incr(ctx, key, ttl, 0, false)
}

func (k *SQLiteKV) IncrCap(ctx context.Context, key string, ttl time.Duration, cap int64) (int64, error) {
	return k.incr(ctx, key, ttl, cap, true)
}

// incr 事务内原子自增：键不存在（含已过期）时从 1 开始并写入 ttl；
// 已存在时只改值、不动 expires_at，沿用首次设置的 TTL。
// capped 时新值超过 cap 不落库，返回 cap+1 供调用方判超限。
func (k *SQLiteKV) incr(ctx context.Context, key string, ttl time.Duration, cap int64, capped bool) (int64, error) {
	tx, err := k.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer func() { _ = tx.Rollback() }()

	var n int64
	value, _, found, err := sqKVReadTx(ctx, tx, key)
	if err != nil {
		return 0, err
	}
	if found {
		n = parseKVInt(value) + 1
	} else {
		n = 1
	}
	if capped && n > cap {
		return cap + 1, nil
	}
	if !found {
		if _, err := tx.ExecContext(ctx, sqKVUpsert, key, strconv.FormatInt(n, 10), kvExpiry(ttl)); err != nil {
			return 0, err
		}
	} else {
		if _, err := tx.ExecContext(ctx,
			`UPDATE kv SET kv_value = ? WHERE kv_key = ?`, strconv.FormatInt(n, 10), key); err != nil {
			return 0, err
		}
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return n, nil
}

// Eval 仅支持 CASScript（fail-closed：未知脚本直接报错，不猜语义）。
// 事务内完成「读当前值-比对-覆盖写并重置 TTL」，与 Redis 单线程 EVAL 等价原子。
func (k *SQLiteKV) Eval(ctx context.Context, script string, keys []string, args ...string) (any, error) {
	if script != CASScript {
		return nil, errors.New("sqlite kv: 仅支持 CASScript")
	}
	if len(keys) != 1 || len(args) != 3 {
		return nil, errors.New("sqlite kv: CASScript 需要 1 个 key 与 3 个参数")
	}
	ttl, err := time.ParseDuration(args[2] + "s")
	if err != nil {
		return nil, err
	}

	tx, err := k.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()

	value, _, found, err := sqKVReadTx(ctx, tx, keys[0])
	if err != nil {
		return nil, err
	}
	// 键不存在、已过期或旧值不匹配都视为交换失败（与 Lua 中 GET 为 false 同理）。
	if !found || value != args[0] {
		return int64(0), nil
	}
	if _, err := tx.ExecContext(ctx, sqKVUpsert, keys[0], args[1], kvExpiry(ttl)); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return int64(1), nil
}

// sqKVReadTx 事务内读键：不存在或已过期都返回 found=false。
// 过期行不在这里删——由 Get 惰性清理或后续写覆盖，避免在写事务里多一次 DELETE。
func sqKVReadTx(ctx context.Context, tx *sql.Tx, key string) (string, sql.NullTime, bool, error) {
	var value string
	var exp sql.NullTime
	err := tx.QueryRowContext(ctx,
		`SELECT kv_value, expires_at FROM kv WHERE kv_key = ?`, key).Scan(&value, &exp)
	if errors.Is(err, sql.ErrNoRows) {
		return "", sql.NullTime{}, false, nil
	}
	if err != nil {
		return "", sql.NullTime{}, false, err
	}
	if kvExpired(exp) {
		return "", sql.NullTime{}, false, nil
	}
	return value, exp, true, nil
}

func kvExpired(exp sql.NullTime) bool {
	return exp.Valid && !time.Now().Before(exp.Time)
}

// parseKVInt 按 MemKV 的口径解析计数：非纯数字一律当 0（KV 的 Incr 只用于计数）。
func parseKVInt(s string) int64 {
	var n int64
	for _, c := range s {
		if c < '0' || c > '9' {
			return 0
		}
		n = n*10 + int64(c-'0')
	}
	return n
}

// 编译期断言：SQLiteKV 是 KV 的完整实现。
var _ KV = (*SQLiteKV)(nil)
