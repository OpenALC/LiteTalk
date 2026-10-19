package store

import (
	"context"
	"errors"
	"time"

	"github.com/redis/go-redis/v9"
)

// RedisKV 是 KV 的 Redis 实现（go-redis v9）。
type RedisKV struct {
	rdb *redis.Client
}

// OpenRedis 连接 Redis。addr 空时取环境变量 LITETALK_REDIS_ADDR（默认 127.0.0.1:6379）。
func OpenRedis(addr string) (*RedisKV, error) {
	if addr == "" {
		addr = "127.0.0.1:6379"
	}
	rdb := redis.NewClient(&redis.Options{Addr: addr})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := rdb.Ping(ctx).Err(); err != nil {
		_ = rdb.Close()
		return nil, err
	}
	return &RedisKV{rdb: rdb}, nil
}

// Close 关闭连接。
func (r *RedisKV) Close() error { return r.rdb.Close() }

func (r *RedisKV) Get(ctx context.Context, key string) (string, error) {
	v, err := r.rdb.Get(ctx, key).Result()
	if errors.Is(err, redis.Nil) {
		return "", ErrKVNotFound
	}
	return v, err
}

func (r *RedisKV) Set(ctx context.Context, key, value string, ttl time.Duration) error {
	return r.rdb.Set(ctx, key, value, ttl).Err()
}

func (r *RedisKV) Del(ctx context.Context, keys ...string) error {
	return r.rdb.Del(ctx, keys...).Err()
}

// incrScript 原子自增 + 首次设置 TTL（#22）：INCR 与 EXPIRE 分两条命令时，
// 进程若在两者之间退出会留下永不过期的计数键（验证码每日/错误计数）。
// Redis 单线程执行 EVAL，计数与过期要么一起生效、要么都不生效。
const incrScript = `
local n = redis.call('INCR', KEYS[1])
if n == 1 then
  redis.call('EXPIRE', KEYS[1], ARGV[1])
end
return n
`

// Incr 自增计数：首次创建时设置 ttl，之后沿用原 TTL（与内存实现一致）。
func (r *RedisKV) Incr(ctx context.Context, key string, ttl time.Duration) (int64, error) {
	return r.rdb.Eval(ctx, incrScript, []string{key}, int(ttl.Seconds())).Int64()
}

// Eval 执行 Lua 脚本（go-redis EVAL）。CAS 的原子性由 Redis 单线程执行模型保证。
func (r *RedisKV) Eval(ctx context.Context, script string, keys []string, args ...string) (any, error) {
	argv := make([]any, len(args))
	for i, a := range args {
		argv[i] = a
	}
	return r.rdb.Eval(ctx, script, keys, argv...).Result()
}

// incrCapScript Lua 原子自增限幅（#22）：新值 > cap 时不增长、返回 cap+1；
// key 不存在时从 1 开始并设置 ttl（已有 key 沿用原 TTL）。Redis 单线程执行保证原子。
const incrCapScript = `
local cur = redis.call('GET', KEYS[1])
local n
if cur then
  n = tonumber(cur) + 1
else
  n = 1
end
if n > tonumber(ARGV[2]) then
  return tonumber(ARGV[2]) + 1
end
if cur then
  redis.call('INCR', KEYS[1])
else
  redis.call('SET', KEYS[1], n, 'EX', ARGV[1])
end
return n
`

// IncrCap 原子自增并限幅（#22）。
func (r *RedisKV) IncrCap(ctx context.Context, key string, ttl time.Duration, cap int64) (int64, error) {
	n, err := r.rdb.Eval(ctx, incrCapScript, []string{key},
		int(ttl.Seconds()), cap).Int64()
	if err != nil {
		return 0, err
	}
	return n, nil
}
