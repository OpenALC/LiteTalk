package store

import (
	"context"
	"errors"
	"sync"
	"time"
)

// MemKV 是 KV 的进程内实现（从 Mem 拆出，语义与 Redis 对齐）：
// 单进程部署的默认 KV，不跨重启——刷新令牌/验证码/限流在重启后全部丢失。
// 多实例部署必须配置 LITETALK_REDIS_ADDR，否则各实例视图不一致。
type MemKV struct {
	mu      sync.Mutex
	kv      map[string]kvEntry
	kvTimed map[string]*time.Timer
}

type kvEntry struct {
	value string
	exp   time.Time // zero = 永不过期
}

// NewMemKV 创建进程内 KV。
func NewMemKV() *MemKV {
	return &MemKV{
		kv:      map[string]kvEntry{},
		kvTimed: map[string]*time.Timer{},
	}
}

func (m *MemKV) Get(_ context.Context, key string) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	e, ok := m.kv[key]
	if !ok {
		return "", ErrKVNotFound
	}
	if !e.exp.IsZero() && time.Now().After(e.exp) {
		delete(m.kv, key)
		return "", ErrKVNotFound
	}
	return e.value, nil
}

func (m *MemKV) Set(_ context.Context, key, value string, ttl time.Duration) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.setLocked(key, value, ttl)
	return nil
}

func (m *MemKV) setLocked(key, value string, ttl time.Duration) {
	e := kvEntry{value: value}
	if ttl > 0 {
		e.exp = time.Now().Add(ttl)
		if t, ok := m.kvTimed[key]; ok {
			t.Stop()
		}
		m.kvTimed[key] = time.AfterFunc(ttl, func() {
			m.mu.Lock()
			defer m.mu.Unlock()
			delete(m.kv, key)
			delete(m.kvTimed, key)
		})
	}
	m.kv[key] = e
}

func (m *MemKV) Del(_ context.Context, keys ...string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, k := range keys {
		delete(m.kv, k)
		if t, ok := m.kvTimed[k]; ok {
			t.Stop()
			delete(m.kvTimed, k)
		}
	}
	return nil
}

func (m *MemKV) Incr(_ context.Context, key string, ttl time.Duration) (int64, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var n int64
	if e, ok := m.kv[key]; ok && (e.exp.IsZero() || time.Now().Before(e.exp)) {
		// 解析旧值（Incr 只用于纯数字计数）。
		for _, c := range e.value {
			if c < '0' || c > '9' {
				n = 0
				break
			}
			n = n*10 + int64(c-'0')
		}
	}
	n++
	// 首次创建才设置 TTL（与 Redis INCR+EX 语义对齐）。
	if _, ok := m.kvTimed[key]; !ok {
		m.setLocked(key, itoa(n), ttl)
	} else {
		e := m.kv[key]
		e.value = itoa(n)
		m.kv[key] = e
	}
	return n, nil
}

// IncrCap 原子自增并限幅（#22）：全局锁内模拟 Redis Lua 语义。
func (m *MemKV) IncrCap(_ context.Context, key string, ttl time.Duration, cap int64) (int64, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var n int64
	if e, ok := m.kv[key]; ok && (e.exp.IsZero() || time.Now().Before(e.exp)) {
		for _, c := range e.value {
			if c < '0' || c > '9' {
				n = 0
				break
			}
			n = n*10 + int64(c-'0')
		}
	}
	n++
	if n > cap {
		return cap + 1, nil
	}
	if _, ok := m.kvTimed[key]; !ok {
		m.setLocked(key, itoa(n), ttl)
	} else {
		e := m.kv[key]
		e.value = itoa(n)
		m.kv[key] = e
	}
	return n, nil
}

// Eval 在锁内模拟 CASScript 语义：KEYS[1] 当前值等于 ARGV[1] 时替换为 ARGV[2]
// 并把 TTL 重置为 ARGV[3] 秒，返回 int64(1)；否则返回 int64(0)。
// 全部 KV 操作都排在 m.mu 之后，GET 比对与 SET 之间插不进其他写，等价原子，
// 故无需真 Lua。传入其他脚本一律报错（fail-closed，防止新脚本被套上 CAS 语义）。
func (m *MemKV) Eval(_ context.Context, script string, keys []string, args ...string) (any, error) {
	if script != CASScript {
		return nil, errors.New("mem: 仅支持 CASScript")
	}
	if len(keys) != 1 || len(args) != 3 {
		return nil, errors.New("mem: CASScript 需要 1 个 key 与 3 个参数")
	}
	ttl, err := time.ParseDuration(args[2] + "s")
	if err != nil {
		return nil, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	e, ok := m.kv[keys[0]]
	// 键不存在或已过期视作比对失败（与 Lua 中 GET 返回 false 同理）。
	if !ok || (!e.exp.IsZero() && time.Now().After(e.exp)) {
		return int64(0), nil
	}
	if e.value != args[0] {
		return int64(0), nil
	}
	m.setLocked(keys[0], args[1], ttl)
	return int64(1), nil
}

func itoa(n int64) string {
	if n == 0 {
		return "0"
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	return string(b[i:])
}

// 编译期断言：MemKV 是 KV 的完整实现（fail-closed Eval 不变量随类型迁移）。
var _ KV = (*MemKV)(nil)
