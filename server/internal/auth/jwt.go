package auth

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/OpenALC/LiteTalk/server/internal/store"
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

// RefreshStore 刷新令牌存取（KV 抽象，rt:{uid}:{dev} 存哈希）。
type RefreshStore struct {
	kv kvStore
}

type kvStore interface {
	Get(ctx context.Context, key string) (string, error)
	Set(ctx context.Context, key, value string, ttl time.Duration) error
	Del(ctx context.Context, keys ...string) error
	Incr(ctx context.Context, key string, ttl time.Duration) (int64, error)
	IncrCap(ctx context.Context, key string, ttl time.Duration, cap int64) (int64, error)
	Eval(ctx context.Context, script string, keys []string, args ...string) (any, error)
}

// NewRefreshStore 构造刷新令牌存储。
func NewRefreshStore(kv kvStore) *RefreshStore { return &RefreshStore{kv: kv} }

// DeviceDesktop 桌面端设备类型（旧格式 refresh 令牌无 dev 段，一律按此解析，R1）。
const DeviceDesktop = "desktop"

// DeviceTypes 设备类型白名单（P2-2）：登录入参校验（api.deviceType）、refresh 令牌解析、
// RevokeAll/DeleteAll 枚举共用这一份；新增设备类型只需改这里并补迁移文档。
var DeviceTypes = map[string]struct{}{
	"desktop": {},
	"mobile":  {},
}

// IsKnownDevice 是否为白名单内的设备类型。
func IsKnownDevice(dev string) bool {
	_, ok := DeviceTypes[dev]
	return ok
}

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

// IssueWithUID 签发带前缀的刷新令牌并存储哈希（R1：令牌自描述设备与会话）：
//   - dev 非空：新格式 "<uid>.<dev>.<sid>.<rand>"（sid 为空时退化为 "<uid>.<dev>.<rand>"）；
//   - dev 为空：旧格式 "<uid>.<sid>.<rand>"（sid 为空时再退化为 "<uid>.<rand>"）。
//
// 客户端 refresh/logout 时无需额外携带设备与会话信息，服务端从令牌本身解析。
func (s *RefreshStore) IssueWithUID(ctx context.Context, userID int64, dev, sid string) (string, error) {
	plain, err := s.Issue(ctx, userID, dev, sid)
	if err != nil {
		return "", err
	}
	return formatRefreshToken(userID, dev, sid, plain), nil
}

// formatRefreshToken 拼装 refresh 明文（不落库；普通签发与 CAS 旋转共用）。
func formatRefreshToken(userID int64, dev, sid, plain string) string {
	var b strings.Builder
	b.WriteString(itoa(userID))
	if dev != "" {
		b.WriteByte('.')
		b.WriteString(dev)
	}
	if sid != "" {
		b.WriteByte('.')
		b.WriteString(sid)
	}
	b.WriteByte('.')
	b.WriteString(plain)
	return b.String()
}

// ParseRefreshToken 解析 refresh 令牌，返回 uid、设备类型与会话 ID（R1：自描述，无需反查）。
// 支持四种格式：
//   - "<uid>.<dev>.<sid>.<rand>"：新格式，dev 段须命中 DeviceTypes 白名单；
//   - "<uid>.<dev>.<rand>"：sid 为空的退化新格式（按 dev 段与旧格式区分，sid 恒为 32hex 不会撞设备名）；
//   - "<uid>.<sid>.<rand>" / "<uid>.<rand>"：v1.1.0 前的旧格式，dev 一律按 desktop 解析。
func ParseRefreshToken(token string) (uid int64, dev, sid string, ok bool) {
	dot1 := strings.IndexByte(token, '.')
	if dot1 <= 0 {
		return 0, "", "", false
	}
	uid = parseInto(token[:dot1])
	if uid <= 0 {
		return 0, "", "", false
	}
	rest := token[dot1+1:]
	dot2 := strings.IndexByte(rest, '.')
	if dot2 < 0 {
		return uid, DeviceDesktop, "", true // 旧格式 uid.rand
	}
	seg, tail := rest[:dot2], rest[dot2+1:]
	if IsKnownDevice(seg) {
		// 新格式：tail 为 "<sid>.<rand>"，sid 为空时退化为 "<rand>"。
		d := strings.IndexByte(tail, '.')
		if d < 0 {
			return uid, seg, "", true
		}
		if strings.IndexByte(tail[d+1:], '.') >= 0 {
			return 0, "", "", false // rand 为 hex 不含点，再含点即畸形
		}
		return uid, seg, tail[:d], true
	}
	// 旧格式 uid.sid.rand：rand 是 hex 不含点，tail 再含点即畸形令牌。
	if strings.IndexByte(tail, '.') >= 0 {
		return 0, "", "", false
	}
	return uid, DeviceDesktop, seg, true
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

// ErrSessionReplaced 令牌 sid 与当前会话不符（另一会话已接管该设备槽位）。
// 当前会话只存在于 SessionIndex，RefreshStore 无权访问，故由 API 层握手前比对后返回。
var ErrSessionReplaced = errors.New("session replaced")

// ErrSessionReplayed 旧令牌重放（CAS 比对失败：并发旋转被抢先，或旋转后的旧值再次使用）。
var ErrSessionReplayed = errors.New("session replayed")

// Rotate 旋转：校验旧令牌匹配后换发新令牌并吊销旧值。
// dev/sid 全部从令牌解析（R1），调用方无从传错设备类型——P0-2（mobile 刷新必败）即因
// 调用方写死 desktop 所致。token 为 IssueWithUID 签发的完整格式。
//
// 新键 rt:{uid}:{dev} 走 Lua CAS（store.CASScript）：Redis 单线程保证 GET 比对与 SET
// 原子，并发重放同一令牌只有一个 goroutine 成功，其余 ErrSessionReplayed。
// 新键无值时兼容旧版单槽位 rt:{uid}（v1.1.0 前的旧格式令牌）：命中即迁移到新键再旋转。
func (s *RefreshStore) Rotate(ctx context.Context, userID int64, oldPlain string) (string, error) {
	uid, dev, sid, ok := ParseRefreshToken(oldPlain)
	if !ok || uid != userID {
		return "", ErrInvalidToken
	}
	raw := oldPlainStrip(oldPlain)
	key := rtKey(userID, dev)

	// 新键无值：旧格式令牌读 legacy 单槽位，哈希命中则迁移（带上令牌自带 sid）到新键。
	if _, err := s.kv.Get(ctx, key); err != nil {
		oldVal, err2 := s.kv.Get(ctx, rtKeyLegacy(userID))
		if err2 == nil && oldVal == HashToken(raw) {
			plain, err := s.IssueWithUID(ctx, userID, dev, sid)
			if err != nil {
				return "", err
			}
			_ = s.kv.Del(ctx, rtKeyLegacy(userID)) // 迁移成功后删除旧键
			return plain, nil
		}
		return "", ErrInvalidToken
	}

	// 真 CAS：期望值=旧令牌哈希。不匹配（被抢先旋转/旧值重放）→ 0 → ErrSessionReplayed。
	newPlain, err := RandomToken(32)
	if err != nil {
		return "", err
	}
	res, err := s.kv.Eval(ctx, store.CASScript, []string{key},
		HashToken(raw), HashToken(newPlain), itoa(int64(RefreshTokenTTL/time.Second)))
	if err != nil {
		return "", err
	}
	if n, _ := res.(int64); n != 1 {
		return "", ErrSessionReplayed
	}
	return formatRefreshToken(userID, dev, sid, newPlain), nil
}

// Revoke 吊销刷新令牌（logout）：dev 从令牌解析（R1），连 legacy 单槽位键一并删除——
// 迁移窗口内只删新键会让旧格式令牌在 logout 后依然有效。
func (s *RefreshStore) Revoke(ctx context.Context, userID int64, plain string) error {
	uid, dev, _, ok := ParseRefreshToken(plain)
	if !ok || uid != userID {
		return ErrInvalidToken
	}
	return s.kv.Del(ctx, rtKey(userID, dev), rtKeyLegacy(userID))
}

// RevokeAll 吊销该用户全部设备的刷新令牌（重置密码用）。
func (s *RefreshStore) RevokeAll(ctx context.Context, userID int64) error {
	// 逐设备删除（KV 抽象无 SCAN；设备类型枚举自 DeviceTypes 白名单）。
	keys := make([]string, 0, len(DeviceTypes)+1)
	for d := range DeviceTypes {
		keys = append(keys, rtKey(userID, d))
	}
	keys = append(keys, rtKeyLegacy(userID)) // 兼容旧键
	return s.kv.Del(ctx, keys...)
}

// Match 校验刷新令牌与存储哈希一致（logout 吊销前用：不校验则伪造 "<uid>.x" 可吊销他人令牌）。
// dev 从令牌解析（R1）；任何取数错误都按不匹配处理（fail-closed）。
func (s *RefreshStore) Match(ctx context.Context, userID int64, plain string) bool {
	uid, dev, _, ok := ParseRefreshToken(plain)
	if !ok || uid != userID {
		return false
	}
	stored, err := s.kv.Get(ctx, rtKey(userID, dev))
	if err == nil {
		return stored == HashToken(oldPlainStrip(plain))
	}
	// 新键无值：兼容旧版单槽位 rt:{uid}（旧格式令牌）。
	if oldVal, err := s.kv.Get(ctx, rtKeyLegacy(userID)); err == nil {
		return oldVal == HashToken(oldPlainStrip(plain))
	}
	return false
}

// oldPlainStrip 从完整 refresh 令牌中取真正的随机段（末段）：Issue 存的是 HashToken(rand)，
// 比对时也必须只对 rand 做哈希。rand 为 hex 不含点，故末段切分对全部格式成立。
func oldPlainStrip(plain string) string {
	if i := strings.LastIndexByte(plain, '.'); i >= 0 {
		return plain[i+1:]
	}
	return plain
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
