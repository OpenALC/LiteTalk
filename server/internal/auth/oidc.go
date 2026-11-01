// oidc.go — 最小 OIDC 依赖方（RP，#54）：标准授权码 + PKCE(S256) 登录。
// 刻意不引入第三方 OIDC 大包：discovery 拉取、JWKS 缓存与 ID Token（RS256）
// 验签全部用标准库实现（约两百行），只依赖 golang.org/x/oauth2 做 token 端点
// 交互。只支持 RS256 签名的 ID Token——其余 alg 一律拒绝，杜绝 alg 混淆类攻击。
//
// 未配置 = 功能关闭且零副作用：构造 Provider 本身不做任何 I/O，discovery 与
// JWKS 都是首次登录时惰性拉取并带 TTL 缓存，没有后台 goroutine。
package auth

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"math/big"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"

	"golang.org/x/oauth2"
)

// OIDCConfig LITETALK_OIDC_* 的解析结果。五项语义见 deploy/env/litetalk.env.example。
type OIDCConfig struct {
	Issuer       string // IdP 签发者标识（必须与 discovery 文档的 issuer 逐字一致）
	ClientID     string
	ClientSecret string
	RedirectURL  string // 部署方前端回调页（页面把 code+state POST 回 /auth/oidc/callback）
	Scopes       []string
}

// DefaultOIDCScopes scopes 未配置时的默认集（OIDC 标准三件）。
func DefaultOIDCScopes() []string { return []string{"openid", "profile", "email"} }

// Complete 配置是否完整。缺任何一项都按未配置处理（fail-closed：功能关闭，
// 授权端点 404）；cmd/server 的启动校验会对显式写了一半的配置直接报错，
// 所以这里不需要再区分「写错」与「没写」。
func (c OIDCConfig) Complete() bool {
	return c.Issuer != "" && c.ClientID != "" && c.ClientSecret != "" && c.RedirectURL != ""
}

// OIDCClaims ID Token 里本实现消费的声明。
type OIDCClaims struct {
	Sub               string
	Email             string
	EmailVerified     bool
	Name              string
	PreferredUsername string
}

// 错误分类：调用方据此映射不同 HTTP 语义。
//   - ErrOIDCUpstream：IdP 侧故障（discovery/JWKS/token 不可达或响应异常）；
//   - ErrOIDCCode：授权码被 IdP 拒绝或令牌响应不合规（用户重来一次即可）；
//   - ErrOIDCToken：ID Token 校验失败（签名/iss/aud/exp/sub/nonce 不符）。
var (
	ErrOIDCUpstream = errors.New("oidc: identity provider error")
	ErrOIDCCode     = errors.New("oidc: authorization code rejected")
	ErrOIDCToken    = errors.New("oidc: id token invalid")
)

const (
	oidcHTTPTimeout   = 10 * time.Second
	oidcCacheTTL      = 15 * time.Minute // discovery 与 JWKS 共用同一缓存窗口
	oidcClockSkew     = time.Minute      // exp/iat 判定的时钟偏差容忍
	oidcRespLimit     = 1 << 20          // IdP 响应体上限（discovery/JWKS 都远小于此）
	oidcVerifierBytes = 32               // PKCE verifier 熵（RFC 7636 要求 ≥256bit）
	// oidcIATMaxAge 签发时间的陈旧上限：OIDC 不规定 iat 与 exp 的间隔，正常 IdP
	// 签发的 ID Token 生命周期是分钟到小时级。上限只用于拒绝「exp 仍有效但签发
	// 已远超合理时长」的令牌（被盗令牌长期重放/IdP 时钟异常），不参与常规过期判定。
	oidcIATMaxAge = 24 * time.Hour
)

// NewOIDCPKCE 生成 code_verifier（43 字符 base64url = 256bit 熵）与 S256 摘要。
// verifier 明文只进服务端 state 存储，不出服务端——授权 URL 只带 S256 摘要。
func NewOIDCPKCE() (verifier, challenge string, err error) {
	raw := make([]byte, oidcVerifierBytes)
	if _, err = rand.Read(raw); err != nil {
		return "", "", err
	}
	verifier = base64.RawURLEncoding.EncodeToString(raw)
	sum := sha256.Sum256([]byte(verifier))
	return verifier, base64.RawURLEncoding.EncodeToString(sum[:]), nil
}

// oidcDiscovery /.well-known/openid-configuration 中本实现消费的字段。
type oidcDiscovery struct {
	Issuer                string `json:"issuer"`
	AuthorizationEndpoint string `json:"authorization_endpoint"`
	TokenEndpoint         string `json:"token_endpoint"`
	JWKSURI               string `json:"jwks_uri"`
}

// OIDCProvider OIDC 依赖方。登录是低频动作：缓存命中路径无 I/O；缓存未命中
// 时 HTTP 拉取在锁外进行（并发首拉最多重复请求一次，结果幂等互覆，无一致性问题）。
type OIDCProvider struct {
	cfg  OIDCConfig
	log  *log.Logger
	http *http.Client
	now  func() time.Time

	mu          sync.Mutex
	discovery   *oidcDiscovery
	discoveryAt time.Time
	jwks        map[string]*rsa.PublicKey // kid → key
	jwksAt      time.Time
}

// NewOIDCProvider 构造依赖方。scopes 为空补默认集。
func NewOIDCProvider(cfg OIDCConfig, lg *log.Logger) *OIDCProvider {
	if lg == nil {
		lg = log.Default()
	}
	if len(cfg.Scopes) == 0 {
		cfg.Scopes = DefaultOIDCScopes()
	}
	return &OIDCProvider{
		cfg:  cfg,
		log:  lg,
		http: &http.Client{Timeout: oidcHTTPTimeout},
		now:  time.Now,
	}
}

// Issuer 配置的签发者标识（oidc_identities 绑定键的 issuer 半边）。
func (p *OIDCProvider) Issuer() string { return p.cfg.Issuer }

// oauthConf 组装 oauth2.Config（endpoint 取自 discovery）。
func (p *OIDCProvider) oauthConf(d *oidcDiscovery) *oauth2.Config {
	return &oauth2.Config{
		ClientID:     p.cfg.ClientID,
		ClientSecret: p.cfg.ClientSecret,
		RedirectURL:  p.cfg.RedirectURL,
		Scopes:       p.cfg.Scopes,
		Endpoint: oauth2.Endpoint{
			AuthURL:  d.AuthorizationEndpoint,
			TokenURL: d.TokenEndpoint,
		},
	}
}

// endpoints 拉取（或读缓存）discovery 文档。issuer 必须与配置逐字一致
// （OIDC 规范要求），防把授权/令牌请求发到伪装端点。
func (p *OIDCProvider) endpoints(ctx context.Context) (*oidcDiscovery, error) {
	p.mu.Lock()
	if p.discovery != nil && p.now().Sub(p.discoveryAt) < oidcCacheTTL {
		d := p.discovery
		p.mu.Unlock()
		return d, nil
	}
	p.mu.Unlock()

	url := strings.TrimSuffix(p.cfg.Issuer, "/") + "/.well-known/openid-configuration"
	var d oidcDiscovery
	if err := p.getJSON(ctx, url, &d); err != nil {
		return nil, err
	}
	if d.Issuer != p.cfg.Issuer {
		return nil, fmt.Errorf("%w: discovery issuer %q 与配置 %q 不一致", ErrOIDCUpstream, d.Issuer, p.cfg.Issuer)
	}
	if d.AuthorizationEndpoint == "" || d.TokenEndpoint == "" || d.JWKSURI == "" {
		return nil, fmt.Errorf("%w: discovery 缺少 authorization_endpoint/token_endpoint/jwks_uri", ErrOIDCUpstream)
	}
	p.mu.Lock()
	p.discovery, p.discoveryAt = &d, p.now()
	p.mu.Unlock()
	return &d, nil
}

// AuthorizeURL 生成 IdP 授权页 URL（授权码 + PKCE S256 + nonce）。state 与
// nonce 由调用方生成并存入 OIDCStates；失败（IdP discovery 不可达）返回
// ErrOIDCUpstream。
func (p *OIDCProvider) AuthorizeURL(ctx context.Context, state, codeVerifier, nonce string) (string, error) {
	d, err := p.endpoints(ctx)
	if err != nil {
		return "", err
	}
	return p.oauthConf(d).AuthCodeURL(state,
		oauth2.S256ChallengeOption(codeVerifier),
		oauth2.SetAuthURLParam("nonce", nonce),
	), nil
}

// Exchange 授权码换令牌并校验 ID Token。nonce 必须与发起授权时一致
// （正常流程恒非空；空 nonce 仅兼容没有存 nonce 的历史 state）。
//
// 超时（PR118 P2-4）：x/oauth2 的 Exchange 不看 provider 的 client 配置，缺省退回
// http.DefaultClient（无超时）——IdP token 端点挂住时，回调 handler 的 goroutine
// 被永久占用。修法双层：把与 discovery/JWKS 同一条带超时的 client 注入 ctx，再给
// 整次交换（含 id_token 校验的 JWKS 拉取）套同值 deadline，client 超时与 ctx 截止
// 先到者生效。deadline 取 p.http.Timeout（测试注入短超时 client 时整条链路同步
// 缩短）；client 未设超时（异常构造）回落 oidcHTTPTimeout，保证恒有界。
func (p *OIDCProvider) Exchange(ctx context.Context, code, verifier, nonce string) (*OIDCClaims, error) {
	d, err := p.endpoints(ctx)
	if err != nil {
		return nil, err
	}
	ctx = context.WithValue(ctx, oauth2.HTTPClient, p.http)
	timeout := p.http.Timeout
	if timeout <= 0 {
		timeout = oidcHTTPTimeout
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	tok, err := p.oauthConf(d).Exchange(ctx, code, oauth2.VerifierOption(verifier))
	if err != nil {
		var re *oauth2.RetrieveError
		if errors.As(err, &re) {
			// IdP 明确拒绝（授权码无效/过期/verifier 不符）。
			return nil, fmt.Errorf("%w: %v", ErrOIDCCode, err)
		}
		return nil, fmt.Errorf("%w: token 请求失败: %v", ErrOIDCUpstream, err)
	}
	rawID, _ := tok.Extra("id_token").(string)
	if rawID == "" {
		return nil, fmt.Errorf("%w: 令牌响应缺少 id_token（scope 必须含 openid）", ErrOIDCCode)
	}
	return p.verifyIDToken(ctx, rawID, nonce)
}

// oidcIDTokenHeader ID Token 的 JOSE 头（只消费 alg/kid）。
type oidcIDTokenHeader struct {
	Alg string `json:"alg"`
	Kid string `json:"kid"`
}

// flexBool 布尔或字符串布尔的双形态解码（个别 IdP 的 email_verified 是字符串）。
type flexBool bool

func (b *flexBool) UnmarshalJSON(raw []byte) error {
	var v bool
	if err := json.Unmarshal(raw, &v); err == nil {
		*b = flexBool(v)
		return nil
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		*b = flexBool(strings.EqualFold(s, "true"))
		return nil
	}
	return fmt.Errorf("布尔字段类型异常: %s", raw)
}

// oidcIDToken ID Token 载荷。
type oidcIDToken struct {
	Issuer            string   `json:"iss"`
	Subject           string   `json:"sub"`
	Audience          any      `json:"aud"` // 单串或数组
	AuthorizedParty   string   `json:"azp"`
	IssuedAt          int64    `json:"iat"`
	Expires           int64    `json:"exp"`
	Nonce             string   `json:"nonce"`
	Email             string   `json:"email"`
	EmailVerified     flexBool `json:"email_verified"`
	Name              string   `json:"name"`
	PreferredUsername string   `json:"preferred_username"`
}

// audiences aud 声明归一：单值或数组，剔除空串与非字符串项。
func (t *oidcIDToken) audiences() []string {
	switch a := t.Audience.(type) {
	case string:
		if a != "" {
			return []string{a}
		}
	case []any:
		out := make([]string, 0, len(a))
		for _, v := range a {
			if s, ok := v.(string); ok && s != "" {
				out = append(out, s)
			}
		}
		return out
	}
	return nil
}

// authorizedPartyOK azp 校验（OIDC Core §3.1.3.7）：azp 出现时必须等于本
// client_id；多个 aud 时 azp 必须出现（区分「发给谁」与「为谁签发」）。
// 单 aud 且无 azp 是合规形态，不额外要求——这条取舍避免误伤只回单串 aud 的 IdP。
func (t *oidcIDToken) authorizedPartyOK(clientID string, audCount int) bool {
	if t.AuthorizedParty == "" {
		return audCount <= 1
	}
	return t.AuthorizedParty == clientID
}

// verifyIDToken 拆头 → JWKS 取钥 → RS256 验签 → iss/aud/exp/sub/nonce 校验。
func (p *OIDCProvider) verifyIDToken(ctx context.Context, raw, nonce string) (*OIDCClaims, error) {
	parts := strings.Split(raw, ".")
	if len(parts) != 3 {
		return nil, fmt.Errorf("%w: 形态不是 JWS", ErrOIDCToken)
	}
	hb, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		return nil, fmt.Errorf("%w: 头解码失败", ErrOIDCToken)
	}
	var h oidcIDTokenHeader
	if err := json.Unmarshal(hb, &h); err != nil {
		return nil, fmt.Errorf("%w: 头解析失败", ErrOIDCToken)
	}
	// 只认 RS256：RS384/512 与 ES 系列不支持也不试探（fail-closed）。
	if h.Alg != "RS256" {
		return nil, fmt.Errorf("%w: 不支持的签名算法 %q", ErrOIDCToken, h.Alg)
	}
	key, err := p.publicKey(ctx, h.Kid)
	if err != nil {
		return nil, err
	}
	sig, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		return nil, fmt.Errorf("%w: 签名解码失败", ErrOIDCToken)
	}
	digest := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	if err := rsa.VerifyPKCS1v15(key, crypto.SHA256, digest[:], sig); err != nil {
		return nil, fmt.Errorf("%w: 签名校验失败", ErrOIDCToken)
	}
	pb, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return nil, fmt.Errorf("%w: 载荷解码失败", ErrOIDCToken)
	}
	var t oidcIDToken
	if err := json.Unmarshal(pb, &t); err != nil {
		return nil, fmt.Errorf("%w: 载荷解析失败", ErrOIDCToken)
	}
	now := p.now()
	auds := t.audiences()
	switch {
	case t.Issuer != p.cfg.Issuer:
		return nil, fmt.Errorf("%w: iss 不符", ErrOIDCToken)
	case !slices.Contains(auds, p.cfg.ClientID):
		return nil, fmt.Errorf("%w: aud 不符", ErrOIDCToken)
	case !t.authorizedPartyOK(p.cfg.ClientID, len(auds)):
		return nil, fmt.Errorf("%w: azp 不符", ErrOIDCToken)
	case t.Expires == 0 || now.After(time.Unix(t.Expires, 0).Add(oidcClockSkew)):
		return nil, fmt.Errorf("%w: 已过期", ErrOIDCToken)
	// iat 缺失即不合规（OIDC Core 要求必填）；未来时间只容忍时钟偏差，
	// 过旧上限见 oidcIATMaxAge 的注释。
	case t.IssuedAt == 0:
		return nil, fmt.Errorf("%w: 缺少 iat", ErrOIDCToken)
	case time.Unix(t.IssuedAt, 0).After(now.Add(oidcClockSkew)):
		return nil, fmt.Errorf("%w: iat 在未来", ErrOIDCToken)
	case now.Sub(time.Unix(t.IssuedAt, 0)) > oidcIATMaxAge:
		return nil, fmt.Errorf("%w: iat 过旧", ErrOIDCToken)
	case t.Subject == "":
		return nil, fmt.Errorf("%w: 缺少 sub", ErrOIDCToken)
	case nonce != "" && t.Nonce != nonce:
		return nil, fmt.Errorf("%w: nonce 不符", ErrOIDCToken)
	}
	return &OIDCClaims{
		Sub:               t.Subject,
		Email:             t.Email,
		EmailVerified:     bool(t.EmailVerified),
		Name:              t.Name,
		PreferredUsername: t.PreferredUsername,
	}, nil
}

// oidcJWKS jwks_uri 响应。只消费 RSA 密钥（kty=RSA）。
type oidcJWKS struct {
	Keys []struct {
		Kty string `json:"kty"`
		Kid string `json:"kid"`
		N   string `json:"n"`
		E   string `json:"e"`
	} `json:"keys"`
}

// publicKey 按 kid 取验签公钥：缓存命中且未过期直接返回；未知 kid 或缓存过期
// 时重拉 JWKS（支持 IdP 换钥）。重拉失败但缓存里仍有该 kid 时退回旧钥
// （可用性优先——IdP 抖动不应放大成全部用户登录失败）。
func (p *OIDCProvider) publicKey(ctx context.Context, kid string) (*rsa.PublicKey, error) {
	p.mu.Lock()
	key, haveKey := p.jwks[kid]
	fresh := p.now().Sub(p.jwksAt) < oidcCacheTTL
	p.mu.Unlock()
	if haveKey && fresh {
		return key, nil
	}

	keys, err := p.fetchJWKS(ctx)
	if err != nil {
		if haveKey {
			p.log.Printf("[oidc] JWKS 刷新失败（退回缓存旧钥）: %v", err)
			return key, nil
		}
		return nil, err
	}
	p.mu.Lock()
	p.jwks, p.jwksAt = keys, p.now()
	p.mu.Unlock()
	if k, ok := keys[kid]; ok {
		return k, nil
	}
	return nil, fmt.Errorf("%w: JWKS 中不存在 kid %q", ErrOIDCUpstream, kid)
}

// fetchJWKS 拉取并解析 JWKS（HTTP 在锁外；jwks_uri 来自 discovery 缓存）。
func (p *OIDCProvider) fetchJWKS(ctx context.Context) (map[string]*rsa.PublicKey, error) {
	d, err := p.endpoints(ctx)
	if err != nil {
		return nil, err
	}
	var doc oidcJWKS
	if err := p.getJSON(ctx, d.JWKSURI, &doc); err != nil {
		return nil, err
	}
	keys := make(map[string]*rsa.PublicKey, len(doc.Keys))
	for _, k := range doc.Keys {
		if k.Kty != "RSA" || k.Kid == "" || k.N == "" {
			continue
		}
		n, err := base64.RawURLEncoding.DecodeString(k.N)
		if err != nil {
			continue
		}
		e, err := base64.RawURLEncoding.DecodeString(k.E)
		if err != nil || len(e) == 0 || len(e) > 8 {
			continue
		}
		exp := 0
		for _, b := range e {
			exp = exp<<8 | int(b)
		}
		keys[k.Kid] = &rsa.PublicKey{N: new(big.Int).SetBytes(n), E: exp}
	}
	if len(keys) == 0 {
		return nil, fmt.Errorf("%w: JWKS 无可用 RSA 密钥", ErrOIDCUpstream)
	}
	return keys, nil
}

// getJSON 带上限的 GET + JSON 解码（错误统一折叠成 ErrOIDCUpstream）。
func (p *OIDCProvider) getJSON(ctx context.Context, url string, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrOIDCUpstream, err)
	}
	resp, err := p.http.Do(req)
	if err != nil {
		return fmt.Errorf("%w: 请求 %s 失败: %v", ErrOIDCUpstream, url, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("%w: %s 返回 HTTP %d", ErrOIDCUpstream, url, resp.StatusCode)
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, oidcRespLimit)).Decode(out); err != nil {
		return fmt.Errorf("%w: %s 响应解析失败: %v", ErrOIDCUpstream, url, err)
	}
	return nil
}
