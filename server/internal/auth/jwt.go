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

// SignAccess 签发访问令牌（2h）。
func (j *JWT) SignAccess(userID int64, nickname, tag string) (string, error) {
	now := time.Now()
	claims := Claims{
		UserID:   userID,
		Nickname: nickname,
		Tag:      tag,
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

func rtKey(userID int64) string { return "rt:" + itoa(userID) }

// Issue 签发新刷新令牌：返回明文（响应给客户端），存储哈希（30d TTL）。
func (s *RefreshStore) Issue(ctx context.Context, userID int64) (plain string, err error) {
	plain, err = RandomToken(32)
	if err != nil {
		return "", err
	}
	if err := s.kv.Set(ctx, rtKey(userID), HashToken(plain), RefreshTokenTTL); err != nil {
		return "", err
	}
	return plain, nil
}

// IssueWithUID 签发带用户 ID 前缀的刷新令牌（"<uid>.<random>"），
// 客户端 refresh 时无需额外携带 user_id，服务端可直接反查。
func (s *RefreshStore) IssueWithUID(ctx context.Context, userID int64) (string, error) {
	plain, err := s.Issue(ctx, userID)
	if err != nil {
		return "", err
	}
	return itoa(userID) + "." + plain, nil
}

// ParseRefreshUID 从 "<uid>.<random>" 格式解析用户 ID。
func ParseRefreshUID(token string) (int64, bool) {
	for i := 0; i < len(token); i++ {
		if token[i] == '.' {
			uid := parseInto(token[:i])
			if uid <= 0 {
				return 0, false
			}
			return uid, true
		}
	}
	return 0, false
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

// Rotate 旋转：校验旧令牌匹配后换发新令牌并吊销旧值。
// token 为完整格式（含 uid 前缀）或裸随机串（兼容 Issue 直发）。
func (s *RefreshStore) Rotate(ctx context.Context, userID int64, oldPlain string) (string, error) {
	// 剥离 uid 前缀取真正的随机段做哈希比对。
	raw := oldPlain
	if uid, ok := ParseRefreshUID(oldPlain); ok && uid == userID {
		raw = oldPlain[len(itoa(userID))+1:]
	}
	stored, err := s.kv.Get(ctx, rtKey(userID))
	if err != nil || stored != HashToken(raw) {
		return "", ErrInvalidToken
	}
	plain, err := s.IssueWithUID(ctx, userID) // Set 覆盖旧哈希 = 吊销旧令牌
	if err != nil {
		return "", err
	}
	return plain, nil
}

// Revoke 吊销（logout）。
func (s *RefreshStore) Revoke(ctx context.Context, userID int64) error {
	return s.kv.Del(ctx, rtKey(userID))
}

// Match 校验 "<uid>.<random>" 刷新令牌与存储哈希一致（logout 吊销前用：
// 不校验的话伪造 "<uid>.x" 即可吊销他人令牌）。
// 任何取数错误都按不匹配处理（与 Rotate 的 fail-closed 语义一致）。
func (s *RefreshStore) Match(ctx context.Context, userID int64, plain string) bool {
	raw := plain
	if uid, ok := ParseRefreshUID(plain); ok && uid == userID {
		raw = plain[len(itoa(userID))+1:]
	}
	stored, err := s.kv.Get(ctx, rtKey(userID))
	if err != nil {
		return false
	}
	return stored == HashToken(raw)
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
