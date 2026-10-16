// Package auth 的多设备会话索引（v1.1.0 迁移 0004）。
// 三层缓存：进程内 5s TTL → Redis sess:{uid}:{dev}（7 天）→ MySQL user_sessions 回填。
// 会话在登录/注册/游客成功时覆盖写入（覆盖即吊销旧会话）。
package auth

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"sync"
	"time"

	"github.com/OpenALC/LiteTalk/server/internal/store"
)

// sessionCacheTTL 进程内缓存 TTL（5s；足够吸收并发握手，又能让登出/踢线快速生效）。
const sessionCacheTTL = 5 * time.Second

// sessionKVKey Redis sess:{uid}:{dev} 缓存键。
func sessionKVKey(uid int64, dev string) string {
	return "sess:" + itoa(uid) + ":" + dev
}

// SessionIndex 多设备会话索引（进程内缓存 + Redis + MySQL 三层）。
type SessionIndex struct {
	store store.Store
	kv    kvStore
	mu    sync.Mutex
	cache map[string]sessionCacheEntry // key: uid+":"+dev
}

type sessionCacheEntry struct {
	sid string
	exp time.Time
}

// NewSessionIndex 构造会话索引（kv 为可选 Redis 缓存；store 必需）。
func NewSessionIndex(st store.Store, kv kvStore) *SessionIndex {
	return &SessionIndex{store: st, kv: kv, cache: map[string]sessionCacheEntry{}}
}

func sessionCacheKey(uid int64, dev string) string { return itoa(uid) + ":" + dev }

// Get 读取会话：进程内缓存 → Redis → MySQL 回填。
func (x *SessionIndex) Get(ctx context.Context, uid int64, dev string) (string, error) {
	x.mu.Lock()
	if e, ok := x.cache[sessionCacheKey(uid, dev)]; ok && time.Now().Before(e.exp) {
		x.mu.Unlock()
		return e.sid, nil
	}
	x.mu.Unlock()

	// Redis 回填。
	if x.kv != nil {
		if sid, err := x.kv.Get(ctx, sessionKVKey(uid, dev)); err == nil && sid != "" {
			x.cacheSet(sessionCacheKey(uid, dev), sid)
			return sid, nil
		}
	}

	// MySQL 回填。
	if x.store != nil {
		s, err := x.store.GetUserSession(ctx, uid, dev)
		if err == nil {
			x.cacheSet(sessionCacheKey(uid, dev), s.Sid)
			if x.kv != nil {
				_ = x.kv.Set(ctx, sessionKVKey(uid, dev), s.Sid, 7*24*time.Hour)
			}
			return s.Sid, nil
		}
	}
	return "", errors.New("session not found")
}

func (x *SessionIndex) cacheSet(key, sid string) {
	x.mu.Lock()
	x.cache[key] = sessionCacheEntry{sid: sid, exp: time.Now().Add(sessionCacheTTL)}
	x.mu.Unlock()
}

// Upsert 登录/注册/游客成功时生成新 sid：MySQL upsert + Redis 覆盖 + 进程内覆盖。
// 覆盖本身即吊销旧会话。
func (x *SessionIndex) Upsert(ctx context.Context, uid int64, dev, ip string) (string, error) {
	sid, err := randomSid()
	if err != nil {
		return "", err
	}
	if x.store != nil {
		if err := x.store.UpsertUserSession(ctx, &store.UserSession{
			UserID: uid, DeviceType: dev, Sid: sid, IP: ip,
		}); err != nil {
			return "", err
		}
	}
	if x.kv != nil {
		_ = x.kv.Set(ctx, sessionKVKey(uid, dev), sid, 7*24*time.Hour)
	}
	x.mu.Lock()
	x.cache[sessionCacheKey(uid, dev)] = sessionCacheEntry{sid: sid, exp: time.Now().Add(sessionCacheCacheTTLForWrite)}
	x.mu.Unlock()
	return sid, nil
}

// sessionCacheCacheTTLForWrite 写入时缓存 TTL（同 Get）。
const sessionCacheCacheTTLForWrite = sessionCacheTTL

// Delete 删除指定设备会话（登出）：三层同删。
func (x *SessionIndex) Delete(ctx context.Context, uid int64, dev string) error {
	if x.store != nil {
		_ = x.store.DeleteUserSession(ctx, uid, dev)
	}
	if x.kv != nil {
		_ = x.kv.Del(ctx, sessionKVKey(uid, dev))
	}
	x.mu.Lock()
	delete(x.cache, sessionCacheKey(uid, dev))
	x.mu.Unlock()
	return nil
}

// DeleteAll 删除该用户全部设备会话（重置密码）。
func (x *SessionIndex) DeleteAll(ctx context.Context, uid int64) error {
	if x.store != nil {
		_ = x.store.DeleteAllUserSessions(ctx, uid)
	}
	if x.kv != nil {
		for _, d := range knownDevices {
			_ = x.kv.Del(ctx, sessionKVKey(uid, d))
		}
	}
	x.mu.Lock()
	for _, d := range knownDevices {
		delete(x.cache, sessionCacheKey(uid, d))
	}
	x.mu.Unlock()
	return nil
}

// randomSid 生成 32 hex 随机会话 ID。
func randomSid() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}
