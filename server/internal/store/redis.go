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

// Incr 自增计数：首次创建时设置 ttl，之后沿用原 TTL（与内存实现一致）。
func (r *RedisKV) Incr(ctx context.Context, key string, ttl time.Duration) (int64, error) {
	n, err := r.rdb.Incr(ctx, key).Result()
	if err != nil {
		return 0, err
	}
	if n == 1 {
		if err := r.rdb.Expire(ctx, key, ttl).Err(); err != nil {
			return 0, err
		}
	}
	return n, nil
}

// Eval 执行 Lua 脚本（go-redis EVAL）。CAS 的原子性由 Redis 单线程执行模型保证。
func (r *RedisKV) Eval(ctx context.Context, script string, keys []string, args ...string) (any, error) {
	argv := make([]any, len(args))
	for i, a := range args {
		argv[i] = a
	}
	return r.rdb.Eval(ctx, script, keys, argv...).Result()
}
