// Package token 实现 api_token（#51 #54）：个人访问令牌（pat）与机器人令牌（bot）
// 是同一种东西——一张表（api_tokens）、一个校验服务、一套 scope 白名单。
// REST 中间件与 WS 握手共用这里的同一份实现（一条实现，不两套）。
//
// 明文形态 ltk_<kind>_<prefix>_<secret>：prefix 8 位小写 hex（检索与 UI 展示用，
// 有意公开，不是秘密也不做鉴权要素），secret 32 字节 base64url（真正的凭据）。
// 明文只在 POST /api/v1/tokens 的响应里出现一次，之后任何接口都不再回传。
package token

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"log"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/OpenALC/LiteTalk/server/internal/auth"
	"github.com/OpenALC/LiteTalk/server/internal/store"
)

// 令牌种类。JWT 主体也用 KindJWT 标注在同一个 Principal 上，三类同构（契约 §0.1）。
const (
	KindJWT = "jwt"
	KindPAT = "pat"
	KindBot = "bot"
)

// PlainPrefix 明文总前缀（Bearer ltk_... 据此分流，先于 JWT 解析判定）。
const PlainPrefix = "ltk_"

const (
	// SecretBytes secret 随机字节数：256bit 熵，base64url 编码 43 字符。
	SecretBytes = 32
	// lastUsedInterval last_used_at 落库节流窗口：每 token 最多 60s 写一次库
	//（UPDATE 锁行，校验热路径绝不每请求写库）。
	lastUsedInterval = 60 * time.Second
)

// ErrInvalidToken 令牌格式错/不存在/哈希不符/已吊销/已过期/属主被停用。
// 全部失败原因折叠成同一错误：调用方只回 401，不区分「哪一步失败」，避免泄露探测信息。
var ErrInvalidToken = errors.New("api token invalid")

// Plain 新签发的令牌明文三要素。Token 仅创建响应返回一次；Prefix/Hash 落库。
type Plain struct {
	Token  string // 完整明文 ltk_<kind>_<prefix>_<secret>
	Prefix string // 8 位小写 hex
	Hash   string // SHA-256(secret) 十六进制（64 字符）
	Kind   string // pat | bot
}

// plainRe 明文形态：kind 固定 pat/bot；prefix 恰 8 位小写 hex（与建索引写入口径一致，
// 杜绝大小写旁路）；secret 是 32 字节 base64url 的 43 字符。
var plainRe = regexp.MustCompile(`^ltk_(pat|bot)_([0-9a-f]{8})_([A-Za-z0-9_-]{43})$`)

// NewPlain 生成新令牌明文。kind 只接受 pat/bot（jwt 不是可签发种类）。
func NewPlain(kind string) (*Plain, error) {
	if kind != KindPAT && kind != KindBot {
		return nil, ErrInvalidToken
	}
	secretRaw := make([]byte, SecretBytes)
	prefixRaw := make([]byte, 4)
	if _, err := rand.Read(secretRaw); err != nil {
		return nil, err
	}
	if _, err := rand.Read(prefixRaw); err != nil {
		return nil, err
	}
	prefix := hex.EncodeToString(prefixRaw)
	secret := base64.RawURLEncoding.EncodeToString(secretRaw)
	return &Plain{
		Token:  PlainPrefix + kind + "_" + prefix + "_" + secret,
		Prefix: prefix,
		Hash:   HashSecret(secret),
		Kind:   kind,
	}, nil
}

// HashSecret SHA-256(secret) 十六进制。
//
// 为什么是 SHA-256 而不是 bcrypt：令牌是 256bit 的高熵随机串，不是人选的密码，
// 不存在字典/爆破攻击面，慢哈希只增加成本不增加安全；校验必须先按 8 位前缀走
// 唯一索引定位唯一行，bcrypt 的盐内嵌在哈希里无法按前缀建索引，且它约 200ms 的
// 单次耗时会让每个 Bearer 请求都付出一遍、直接拖垮整个 REST 面。抗预计算由
// secret 本身的 32 字节随机性保证（等价于每令牌独立盐），与哈希快慢无关。
func HashSecret(secret string) string {
	sum := sha256.Sum256([]byte(secret))
	return hex.EncodeToString(sum[:])
}

// Principal 鉴权主体：JWT 与 api_token 走同一个类型（契约 §0.1）。
//   - JWT 路径：Claims 为解析出的令牌声明，Kind=jwt，TokenID=0，Scopes=nil
//     （权限只由角色决定，不受 scope 上限约束）。
//   - 令牌路径：Claims 为属主用户的身份声明（Sid/Dev 恒空——令牌没有会话概念，
//     吊销即失效），Kind=pat|bot，TokenID 为行主键，Scopes 为令牌授予的 scope。
//
// 不做任何缓存：Principal 每次校验实时构建，吊销立即生效。
type Principal struct {
	*auth.Claims
	Kind    string
	TokenID int64
	Scopes  []string
}

// HasScope 显式 scope 判定，handler 按需调用；**不在中间件做路径级 scope 匹配**
// （路径与 scope 的对应关系会演化，散在中间件里会失控）。
func (p *Principal) HasScope(name string) bool {
	for _, s := range p.Scopes {
		if s == name {
			return true
		}
	}
	return false
}

// TokenStore Service 依赖的最小存储面（*store.Mem / *store.MySQL / *store.SQLite 均满足）。
type TokenStore interface {
	APITokenByPrefix(ctx context.Context, prefix string) (*store.APIToken, error)
	TouchAPITokenLastUsed(ctx context.Context, id int64, at time.Time) error
	UserByID(ctx context.Context, id int64) (*store.User, error)
}

// Service api_token 校验服务。REST 与 WS 共用同一实例。
type Service struct {
	st  TokenStore
	log *log.Logger
	now func() time.Time // 测试注入时钟

	// lastUsedMu 守护节流表：tokenID → 下次允许落库时刻。决策同步做（纯内存），
	// 落库在独立 goroutine（校验路径不等 UPDATE）。表只增不减：token 数量级小，
	// 每行 24B，清理不值得复杂度。
	lastUsedMu   sync.Mutex
	lastUsedNext map[int64]time.Time
	wg           sync.WaitGroup // 在途落库 goroutine（WaitIdle 供测试收口）
}

// NewService 构造校验服务。
func NewService(st TokenStore, lg *log.Logger) *Service {
	if lg == nil {
		lg = log.Default()
	}
	return &Service{
		st:           st,
		log:          lg,
		now:          time.Now,
		lastUsedNext: map[int64]time.Time{},
	}
}

// Verify 校验明文令牌并产出与 JWT 同构的 Principal。
// 流程（契约 §0.1）：拆 kind/prefix → 按 prefix 查行 → 常数时间比对哈希 →
// 检查 revoked_at/expires_at → 校验 users.status → 异步节流更新 last_used_at。
func (s *Service) Verify(ctx context.Context, plain string) (*Principal, error) {
	kind, prefix, secret, ok := parsePlain(plain)
	if !ok {
		return nil, ErrInvalidToken
	}
	row, err := s.st.APITokenByPrefix(ctx, prefix)
	if err != nil {
		// 查无此行与查询故障一律按无效处理（fail-closed，不给探测信息）。
		return nil, ErrInvalidToken
	}
	// 哈希恒 64 hex 字符，长度相等时 ConstantTimeCompare 即常数时间。
	if subtle.ConstantTimeCompare([]byte(HashSecret(secret)), []byte(row.SecretHash)) != 1 {
		return nil, ErrInvalidToken
	}
	now := s.now()
	if row.RevokedAt != nil && !now.Before(*row.RevokedAt) {
		return nil, ErrInvalidToken
	}
	if row.ExpiresAt != nil && !now.Before(*row.ExpiresAt) {
		return nil, ErrInvalidToken
	}
	user, err := s.st.UserByID(ctx, row.UserID)
	if err != nil || user.Status != 0 {
		// 属主被删/停用 → 令牌失效（status 非 0 即非正常态，与 JWT 路径同一口径：
		// 主体不存在即无效）。
		return nil, ErrInvalidToken
	}
	p := &Principal{
		Claims:  &auth.Claims{UserID: user.ID, Nickname: user.Nickname, Tag: user.Tag},
		Kind:    kind,
		TokenID: row.ID,
		Scopes:  strings.Fields(row.Scopes),
	}
	s.scheduleTouch(row.ID, now)
	return p, nil
}

// scheduleTouch 同步完成节流决策，DB 写交给 goroutine。
// 决策必须在请求路径同步：两个并发请求只有第一个能通过节流窗口。
func (s *Service) scheduleTouch(id int64, now time.Time) {
	s.lastUsedMu.Lock()
	if next, ok := s.lastUsedNext[id]; ok && now.Before(next) {
		s.lastUsedMu.Unlock()
		return
	}
	s.lastUsedNext[id] = now.Add(lastUsedInterval)
	s.lastUsedMu.Unlock()
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		// 落库用独立 context：请求 ctx 在响应后取消，不能带进后台 goroutine。
		if err := s.st.TouchAPITokenLastUsed(context.Background(), id, now); err != nil {
			s.log.Printf("[token] last_used_at 更新失败 token=%d: %v", id, err)
		}
	}()
}

// WaitIdle 等待全部在途 last_used 落库完成（测试收口用；生产路径不调）。
func (s *Service) WaitIdle() { s.wg.Wait() }

// parsePlain 拆解明文；形态不对一律拒绝。
func parsePlain(plain string) (kind, prefix, secret string, ok bool) {
	m := plainRe.FindStringSubmatch(plain)
	if m == nil {
		return "", "", "", false
	}
	return m[1], m[2], m[3], true
}

// ---- 上下文 ----

type principalCtxKey struct{}

// WithPrincipal 把 Principal 放进请求上下文（auth 中间件注入）。
func WithPrincipal(ctx context.Context, p *Principal) context.Context {
	return context.WithValue(ctx, principalCtxKey{}, p)
}

// PrincipalFromContext 取中间件注入的 Principal；无则 nil。
func PrincipalFromContext(ctx context.Context) *Principal {
	p, _ := ctx.Value(principalCtxKey{}).(*Principal)
	return p
}

// ScopeLimitFromContext 供 authz 的集中判定点取 scope 上限：
//   - ctx 无 Principal（WS 旧路径 / dev 令牌）→ (0, false)：不受限；
//   - JWT 主体 → (0, false)：不受限（权限只由角色决定）；
//   - 令牌主体 → (scope 映射位并集, true)：检查的权限位必须落在并集内；
//     空 scope 的令牌返回 (0, true) = 无任何权限（绝不当全权限，fail-closed）。
func ScopeLimitFromContext(ctx context.Context) (store.Permission, bool) {
	p := PrincipalFromContext(ctx)
	if p == nil || p.Kind == KindJWT {
		return 0, false
	}
	return ScopeBits(p.Scopes), true
}
