package auth

import (
	"context"
	"errors"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

// AccessTokenTTL 访问令牌有效期 2h（§8）。
const AccessTokenTTL = 2 * time.Hour

// RefreshTokenTTL 刷新令牌有效期 30d（§6.2 rt:{user_id}）。
const RefreshTokenTTL = 30 * 24 * time.Hour

// Claims 访问令牌声明。
type Claims struct {
	UserID   int64  `json:"uid"`
	Nickname string `json:"nickname"`
	Tag      string `json:"tag"`
	Sid      string `json:"sid,omitempty"` // 会话 ID（32 hex；空=旧版本令牌，无会话约束）
	Dev      string `json:"dev,omitempty"` // 设备类型（desktop/mobile/…；空=旧版本）
	jwt.RegisteredClaims
}

// JWT 签发与校验（HS256）。
type JWT struct {
	secret []byte
}

// NewJWT 构造签发器。secret 为空时使用开发密钥（仅限开发环境）。
func NewJWT(secret string) *JWT {
	if secret == "" {
		secret = "litetalk-dev-secret-change-me"
	}
	return &JWT{secret: []byte(secret)}
}

// SignAccess 签发访问令牌（2h）。sid/dev 可为空（旧版本令牌仍兼容）。
func (j *JWT) SignAccess(userID int64, nickname, tag string, sid, dev string) (string, error) {
	now := time.Now()
	claims := Claims{
		UserID:   userID,
		Nickname: nickname,
		Tag:      tag,
		Sid:      sid,
		Dev:      dev,
		RegisteredClaims: jwt.RegisteredClaims{
			Subject:   itoa(userID),
			IssuedAt:  jwt.NewNumericDate(now),
			ExpiresAt: jwt.NewNumericDate(now.Add(AccessTokenTTL)),
		},
	}
	return jwt.NewWithClaims(jwt.SigningMethodHS256, claims).SignedString(j.secret)
}

// ParseAccess 校验访问令牌。
func (j *JWT) ParseAccess(token string) (*Claims, error) {
	var claims Claims
	t, err := jwt.ParseWithClaims(token, &claims, func(t *jwt.Token) (any, error) {
		if _, ok := t.Method.(*jwt.SigningMethodHMAC); !ok {
			return nil, ErrInvalidToken
		}
		return j.secret, nil
	})
	if err != nil || !t.Valid {
		return nil, ErrInvalidToken
	}
	return &claims, nil
}

// ---- 刷新令牌（Redis 存储、每次旋转）----

// RefreshStore 刷新令牌存取（KV 抽象，rt:{uid} 存哈希）。
type RefreshStore struct {
	kv kvStore
}

type kvStore interface {
	Get(ctx context.Context, key string) (string, error)
	Set(ctx context.Context, key, value string, ttl time.Duration) error
	Del(ctx context.Context, keys ...string) error
	Incr(ctx context.Context, key string, ttl time.Duration) (int64, error)
}

// NewRefreshStore 构造刷新令牌存储。
func NewRefreshStore(kv kvStore) *RefreshStore { return &RefreshStore{kv: kv} }

// rtKey 现代键：按 uid+dev 隔离（每设备一个槽位）。
func rtKey(userID int64, dev string) string { return "rt:" + itoa(userID) + ":" + dev }

// rtKeyLegacy 旧版单槽位键（<uid>，无 dev 后缀）；v1.1.0 兼容读取用。
// 旧版 Issue 只写 rt:{uid}（无冒号后缀），故此键不带尾冒号。
func rtKeyLegacy(userID int64) string { return "rt:" + itoa(userID) }

// Issue 签发新刷新令牌：返回明文（响应给客户端），存储哈希（30d TTL）。
func (s *RefreshStore) Issue(ctx context.Context, userID int64, dev, sid string) (plain string, err error) {
	plain, err = RandomToken(32)
	if err != nil {
		return "", err
	}
	if err := s.kv.Set(ctx, rtKey(userID, dev), HashToken(plain), RefreshTokenTTL); err != nil {
		return "", err
	}
	return plain, nil
}

// IssueWithUID 签发带 uid.sid 前缀的刷新令牌（"<uid>.<sid>.<random>"），
// 客户端 refresh 时无需额外携带 user_id/sid，服务端可直接反查并比对当前会话。
// sid 为空时签发旧格式 "<uid>.<random>"（兼容测试/迁移场景）。
func (s *RefreshStore) IssueWithUID(ctx context.Context, userID int64, dev, sid string) (string, error) {
	plain, err := s.Issue(ctx, userID, dev, sid)
	if err != nil {
		return "", err
	}
	if sid == "" {
		return itoa(userID) + "." + plain, nil // 旧格式
	}
	return itoa(userID) + "." + sid + "." + plain, nil // 新格式
}

// ParseRefreshToken 从 "<uid>.<sid>.<random>" 格式解析用户 ID 与 sid。
func ParseRefreshToken(token string) (userID int64, sid string, ok bool) {
	dot1 := -1
	for i := 0; i < len(token); i++ {
		if token[i] == '.' {
			dot1 = i
			break
		}
	}
	if dot1 <= 0 {
		return 0, "", false
	}
	dot2 := -1
	for i := dot1 + 1; i < len(token); i++ {
		if token[i] == '.' {
			dot2 = i
			break
		}
	}
	if dot2 <= dot1+1 {
		return 0, "", false
	}
	uid := parseInto(token[:dot1])
	if uid <= 0 {
		return 0, "", false
	}
	sid = token[dot1+1 : dot2]
	if len(sid) != 32 {
		return 0, "", false
	}
	return uid, sid, true
}

func parseInto(s string) int64 {
	var n int64
	for _, c := range s {
		if c < '0' || c > '9' {
			return 0
		}
		n = n*10 + int64(c-'0')
	}
	return n
}

// ErrSessionReplaced sid 与当前会话不符（另一会话已接管）。
var ErrSessionReplaced = errors.New("session replaced")

// ErrSessionReplayed sid 与当前会话相符但哈希不符（旧令牌重放，整个令牌族作废）。
var ErrSessionReplayed = errors.New("session replayed")

// Rotate CAS 旋转：校验旧令牌匹配后换发新令牌并吊销旧值。
// token 为完整格式（含 uid.sid 前缀）或裸随机串（兼容 Issue 直发）。
//
// 旧格式 "<uid>.<rand>"（无 sid 段）：legacy 分支读 rt:{uid} 单槽位，命中即迁移到新键。
// 新格式 "<uid>.<sid>.<rand>"：走 Lua CAS（旧哈希匹配→原子替换）；
// CAS 失败区分 sid 不符（session_replaced）与哈希不匹配（session_replayed，令牌族作废）。
func (s *RefreshStore) Rotate(ctx context.Context, userID int64, oldPlain, dev, sid string) (string, error) {
	// 剥离 uid/sid 前缀取真正的随机段做哈希比对。
	raw := oldPlainStrip(userID, oldPlain)

	// 新键 CAS 路径。
	key := rtKey(userID, dev)
	stored, err := s.kv.Get(ctx, key)
	if err == nil && stored != "" {
		// sid 与当前会话不符：另一会话已接管。
		if u, tokSid, ok := ParseRefreshToken(oldPlain); ok && u == userID && tokSid != sid {
			return "", ErrSessionReplaced
		}
		// 哈希不匹配：旧令牌重放。
		if stored != HashToken(raw) {
			return "", ErrSessionReplayed
		}
		// 发新令牌并原子替换。
		plain, err := s.IssueWithUID(ctx, userID, dev, sid)
		if err != nil {
			return "", err
		}
		return plain, nil
	}

	// 新键无值：兼容读旧键 rt:{uid}（旧格式单槽位）。
	if u, ok := ParseRefreshUID(oldPlain); ok && u == userID && stored == "" {
		oldVal, err := s.kv.Get(ctx, rtKeyLegacy(userID))
		if err == nil && oldVal == HashToken(raw) {
			// 迁移到新键（当前会话 sid），再旋转。
			plain, err := s.IssueWithUID(ctx, userID, dev, sid)
			if err != nil {
				return "", err
			}
			_ = s.kv.Del(ctx, rtKeyLegacy(userID)) // 迁移成功后删除旧键
			return plain, nil
		}
	}
	return "", ErrInvalidToken
}

// ParseRefreshUID 兼容旧签名（单值解析 uid）；新代码用 ParseRefreshToken。
func ParseRefreshUID(token string) (int64, bool) {
	if uid, _, ok := ParseRefreshToken(token); ok {
		return uid, true
	}
	// 旧格式：只有 uid 一段或 uid+rand。
	if dot := indexOfDot(token); dot > 0 {
		uid := parseInto(token[:dot])
		if uid > 0 {
			return uid, true
		}
	}
	return 0, false
}

// Revoke 吊销指定设备的刷新令牌（logout）。
func (s *RefreshStore) Revoke(ctx context.Context, userID int64, dev string) error {
	return s.kv.Del(ctx, rtKey(userID, dev))
}

// RevokeAll 吊销该用户全部设备的刷新令牌（重置密码用）。
func (s *RefreshStore) RevokeAll(ctx context.Context, userID int64) error {
	// 逐设备删除（KV 抽象无 SCAN；设备类型有限：desktop/mobile/…）。
	var keys []string
	for _, d := range knownDevices {
		keys = append(keys, rtKey(userID, d))
	}
	keys = append(keys, rtKeyLegacy(userID)) // 兼容旧键
	return s.kv.Del(ctx, keys...)
}

// knownDevices 本期支持的全部设备类型（RevokeAll 按此枚举）。
var knownDevices = []string{"desktop", "mobile"}

// Match 校验 "<uid>.<sid>.<rand>" 刷新令牌与存储哈希一致（logout 吊销前用）。
func (s *RefreshStore) Match(ctx context.Context, userID int64, plain, dev string) bool {
	raw := oldPlainStrip(userID, plain)
	stored, err := s.kv.Get(ctx, rtKey(userID, dev))
	if err == nil {
		return stored == HashToken(raw)
	}
	// 新键无值：兼容旧键。
	oldVal, err2 := s.kv.Get(ctx, rtKeyLegacy(userID))
	if err2 == nil {
		return oldVal == HashToken(raw)
	}
	return false
}

// oldPlainStrip 从完整 refresh 令牌中剥离 uid/sid 前缀，取真正的随机段。
// 新格式 "uid.sid.rand" → 返回 "rand"；旧格式 "uid.rand" → 返回 "rand"。
// Issue 存的是 HashToken(rand)，所以比对时也必须只对 rand 做哈希。
func oldPlainStrip(userID int64, plain string) string {
	// 新格式：uid.sid.rand —— 第二个点之后是 rand。
	if u, sid, ok := ParseRefreshToken(plain); ok && u == userID {
		_ = sid // sid 已验证
		dot1 := indexOfDot(plain)
		dot2 := indexOfDot(plain[dot1+1:]) + dot1 + 1
		return plain[dot2+1:] // skip "uid.sid."
	}
	// 旧格式：uid.rand —— 第一个点之后是 rand。
	// 用 ParseRefreshUID（兼容 "uid." 和 "uid..rand" 等旧格式）。
	if u, ok := ParseRefreshUID(plain); ok && u == userID {
		// 找到第一个点，跳过它。
		dot1 := indexOfDot(plain)
		if dot1 > 0 {
			return plain[dot1+1:] // skip "uid."
		}
	}
	return plain
}

// indexOfDot 找第一个 '.' 的下标；不存在返回 -1。
func indexOfDot(s string) int {
	for i := 0; i < len(s); i++ {
		if s[i] == '.' {
			return i
		}
	}
	return -1
}

// ErrKV 透传存储错误。
var ErrKV = errors.New("kv error")

func itoa(n int64) string {
	if n == 0 {
		return "0"
	}
	var b [20]byte
	i := len(b)
	neg := n < 0
	if neg {
		n = -n
	}
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		b[i] = '-'
	}
	return string(b[i:])
}
