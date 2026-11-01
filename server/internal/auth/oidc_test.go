package auth

// oidc_test.go — 最小 IdP 打桩（httptest）：discovery + token 端点 + JWKS（固定
// 密钥），不依赖真实 IdP（批次 6 §7 验收前提）。覆盖 PKCE 校验、ID Token 签名/
// 过期拒绝与 state 一次性消费。

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/OpenALC/LiteTalk/server/internal/store"
)

// fakeIdP 最小 OIDC 提供方。token 端点强制校验 PKCE（S256(verifier) 必须
// 命中 code 绑定的 challenge）与 client 凭据（Basic 头或表单两种风格都收），
// 是「服务端必须带对 verifier」的判据。
type fakeIdP struct {
	srv          *httptest.Server
	issuer       string // Start 后回填（handler 运行期读取）
	key          *rsa.PrivateKey
	wrongKey     *rsa.PrivateKey // 签名拒绝用例的另一把钥
	kid          string
	clientID     string
	clientSecret string
	redirectURL  string

	mu    sync.Mutex
	codes map[string]fakeCode
	// discoveryIssuer 置非空时 discovery 返回它（issuer 不一致用例）。
	discoveryIssuer string
}

type fakeCode struct {
	challenge     string
	nonce         string
	sub           string
	exp           int64 // 0 = 正常有效期
	signKey       *rsa.PrivateKey
	email         string
	emailVerified bool
	used          bool

	// aud/azp/iat 覆盖载荷声明（零值 = 标准形态：aud=clientID、无 azp、iat=当前）。
	aud     any
	azp     *string
	iat     int64
	omitIAT bool
}

func newFakeIdP(t *testing.T) *fakeIdP {
	t.Helper()
	f := &fakeIdP{
		kid:          "test-key",
		clientID:     "litetalk-test",
		clientSecret: "test-secret",
		redirectURL:  "https://app.test/oidc/callback",
		codes:        map[string]fakeCode{},
	}
	var err error
	if f.key, err = rsa.GenerateKey(rand.Reader, 2048); err != nil {
		t.Fatal(err)
	}
	if f.wrongKey, err = rsa.GenerateKey(rand.Reader, 2048); err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/.well-known/openid-configuration", func(w http.ResponseWriter, r *http.Request) {
		issuer := f.issuer
		if f.discoveryIssuer != "" {
			issuer = f.discoveryIssuer
		}
		_ = json.NewEncoder(w).Encode(map[string]string{
			"issuer":                 issuer,
			"authorization_endpoint": issuer + "/authorize",
			"token_endpoint":         issuer + "/token",
			"jwks_uri":               issuer + "/jwks.json",
		})
	})
	mux.HandleFunc("/token", func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			http.Error(w, "bad form", http.StatusBadRequest)
			return
		}
		f.mu.Lock()
		defer f.mu.Unlock()
		code := r.FormValue("code")
		entry, ok := f.codes[code]
		if !ok || entry.used {
			http.Error(w, `{"error":"invalid_grant"}`, http.StatusBadRequest)
			return
		}
		// client 凭据：Basic 头或表单（x/oauth2 自动探测两种风格都会发）。
		id, secret, basic := r.BasicAuth()
		if !basic {
			id, secret = r.FormValue("client_id"), r.FormValue("client_secret")
		}
		if id != f.clientID || secret != f.clientSecret || r.FormValue("redirect_uri") != f.redirectURL {
			http.Error(w, `{"error":"invalid_client"}`, http.StatusUnauthorized)
			return
		}
		// PKCE 强制校验：verifier 的 S256 摘要必须与发起授权时的 challenge 一致。
		sum := sha256.Sum256([]byte(r.FormValue("code_verifier")))
		if base64.RawURLEncoding.EncodeToString(sum[:]) != entry.challenge {
			http.Error(w, `{"error":"invalid_grant"}`, http.StatusBadRequest)
			return
		}
		entry.used = true
		f.codes[code] = entry
		// Content-Type 必须显式 application/json：不设的话 net/http 嗅探成
		// text/plain，x/oauth2 会按 query string 解析响应体而报 cannot parse。
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"access_token": "at-" + code,
			"token_type":   "Bearer",
			"expires_in":   3600,
			"id_token":     f.signIDToken(entry),
		})
	})
	mux.HandleFunc("/jwks.json", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"keys": []map[string]string{{
				"kty": "RSA", "kid": f.kid,
				"n": base64.RawURLEncoding.EncodeToString(f.key.N.Bytes()),
				"e": base64.RawURLEncoding.EncodeToString(bigE(f.key.E)),
			}},
		})
	})
	f.srv = httptest.NewServer(mux)
	t.Cleanup(f.srv.Close)
	f.issuer = f.srv.URL
	return f
}

// bigE 指数的大端最小字节（JWKS 编码口径）。
func bigE(e int) []byte {
	var out []byte
	for e > 0 {
		out = append([]byte{byte(e & 0xff)}, out...)
		e >>= 8
	}
	return out
}

// mintCode 模拟「用户在 IdP 授权页同意」：IdP 签发绑定 challenge/nonce/claims
// 的授权码。signKey 置 nil 用正常密钥；exp 非 0 覆盖过期时间。
func (f *fakeIdP) mintCode(challenge, nonce, sub string, exp int64, signKey *rsa.PrivateKey) string {
	return f.mintCodeWith(fakeCode{challenge: challenge, nonce: nonce, sub: sub, exp: exp, signKey: signKey})
}

// mintCodeWith 同上，额外载荷声明由调用方直接指定。
func (f *fakeIdP) mintCodeWith(c fakeCode) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	code := "code-" + randHex8()
	f.codes[code] = c
	return code
}

func randHex8() string {
	b := make([]byte, 4)
	_, _ = rand.Read(b)
	return strings.ToLower(base64.RawURLEncoding.EncodeToString(b))
}

// signIDToken 用绑定密钥签 RS256 ID Token（email 固定便于断言）。
func (f *fakeIdP) signIDToken(c fakeCode) string {
	key := c.signKey
	if key == nil {
		key = f.key
	}
	exp := c.exp
	if exp == 0 {
		exp = time.Now().Add(time.Hour).Unix()
	}
	header, _ := json.Marshal(map[string]string{"alg": "RS256", "kid": f.kid})
	aud := c.aud
	if aud == nil {
		aud = f.clientID
	}
	iat := c.iat
	if iat == 0 {
		iat = time.Now().Unix()
	}
	claims := map[string]any{
		"iss": f.issuer, "sub": c.sub, "aud": aud,
		"exp": exp, "iat": iat,
		"nonce": c.nonce,
		"email": "oidc-user@example.com", "email_verified": true,
		"name": "OIDC 用户", "preferred_username": "oidcuser",
	}
	if c.azp != nil {
		claims["azp"] = *c.azp
	}
	if c.omitIAT {
		delete(claims, "iat")
	}
	payload, _ := json.Marshal(claims)
	signingInput := base64.RawURLEncoding.EncodeToString(header) + "." + base64.RawURLEncoding.EncodeToString(payload)
	digest := sha256.Sum256([]byte(signingInput))
	sig, err := rsa.SignPKCS1v15(rand.Reader, key, crypto.SHA256, digest[:])
	if err != nil {
		panic(err)
	}
	return signingInput + "." + base64.RawURLEncoding.EncodeToString(sig)
}

func newTestProvider(t *testing.T, f *fakeIdP) *OIDCProvider {
	t.Helper()
	return NewOIDCProvider(OIDCConfig{
		Issuer: f.issuer, ClientID: f.clientID, ClientSecret: f.clientSecret,
		RedirectURL: f.redirectURL,
	}, nil)
}

// TestOIDCAuthorizeURLS256 授权 URL 必须带 S256 摘要与全部标准参数。
func TestOIDCAuthorizeURLS256(t *testing.T) {
	f := newFakeIdP(t)
	p := newTestProvider(t, f)
	verifier, _, err := NewOIDCPKCE()
	if err != nil {
		t.Fatal(err)
	}
	raw, err := p.AuthorizeURL(context.Background(), "state-1", verifier, "")
	if err != nil {
		t.Fatal(err)
	}
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	q := u.Query()
	if got, want := u.Scheme+"://"+u.Host+u.Path, f.issuer+"/authorize"; got != want {
		t.Fatalf("授权端点 = %s, want %s", got, want)
	}
	if q.Get("response_type") != "code" || q.Get("client_id") != f.clientID ||
		q.Get("redirect_uri") != f.redirectURL || q.Get("state") != "state-1" {
		t.Fatalf("授权 URL 参数缺失: %v", q)
	}
	if q.Get("code_challenge_method") != "S256" {
		t.Fatalf("code_challenge_method = %s, want S256", q.Get("code_challenge_method"))
	}
	sum := sha256.Sum256([]byte(verifier))
	if q.Get("code_challenge") != base64.RawURLEncoding.EncodeToString(sum[:]) {
		t.Fatal("code_challenge 不是 verifier 的 S256 摘要")
	}
}

// TestOIDCPKCEVerifierMismatch verifier 与授权时的 challenge 不匹配必须被 IdP 拒
// 绝（本实现的 fake IdP 强制校验 PKCE），并折叠成 ErrOIDCCode。
func TestOIDCPKCEVerifierMismatch(t *testing.T) {
	f := newFakeIdP(t)
	p := newTestProvider(t, f)
	ctx := context.Background()
	verifier, challenge, err := NewOIDCPKCE()
	if err != nil {
		t.Fatal(err)
	}
	state := "state-pkce"
	raw, err := p.AuthorizeURL(ctx, state, verifier, "")
	if err != nil {
		t.Fatal(err)
	}
	q := mustQuery(t, raw)
	if q.Get("code_challenge") != challenge {
		t.Fatal("URL 摘要与生成的 challenge 不一致")
	}
	code := f.mintCode(challenge, "", "sub-1", 0, nil)
	wrong, _, err := NewOIDCPKCE()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := p.Exchange(ctx, code, wrong, ""); !errors.Is(err, ErrOIDCCode) {
		t.Fatalf("verifier 不匹配应 ErrOIDCCode，实际 %v", err)
	}
	// 正确 verifier 通过（证明拒绝不是别的字段引起）。
	if _, err := p.Exchange(ctx, code, verifier, ""); err != nil {
		t.Fatalf("正确 verifier 应通过: %v", err)
	}
}

// TestOIDCIDTokenRejected 签名密钥不对 / 已过期的 ID Token 一律 ErrOIDCToken。
func TestOIDCIDTokenRejected(t *testing.T) {
	f := newFakeIdP(t)
	p := newTestProvider(t, f)
	ctx := context.Background()
	for _, tc := range []struct {
		name    string
		exp     int64
		signKey *rsa.PrivateKey
	}{
		{"签名密钥不对", 0, f.wrongKey},
		{"已过期", time.Now().Add(-time.Hour).Unix(), nil},
	} {
		verifier, challenge, err := NewOIDCPKCE()
		if err != nil {
			t.Fatal(err)
		}
		if _, err := p.AuthorizeURL(ctx, "state-x", verifier, ""); err != nil {
			t.Fatal(err)
		}
		code := f.mintCode(challenge, "", "sub-1", tc.exp, tc.signKey)
		if _, err := p.Exchange(ctx, code, verifier, ""); !errors.Is(err, ErrOIDCToken) {
			t.Fatalf("%s: 应 ErrOIDCToken，实际 %v", tc.name, err)
		}
	}
}

// TestOIDCNonceMismatch nonce 与发起时不一致必须拒绝。授权码在 IdP 侧一次性，
// 拒绝分支各用独立 code（第一次 Exchange 已把原 code 消费掉）。
func TestOIDCNonceMismatch(t *testing.T) {
	f := newFakeIdP(t)
	p := newTestProvider(t, f)
	ctx := context.Background()
	verifier, challenge, err := NewOIDCPKCE()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := p.AuthorizeURL(ctx, "state-n", verifier, "nonce-A"); err != nil {
		t.Fatal(err)
	}
	if _, err := p.Exchange(ctx, f.mintCode(challenge, "nonce-A", "sub-1", 0, nil), verifier, "nonce-B"); !errors.Is(err, ErrOIDCToken) {
		t.Fatalf("nonce 不符应 ErrOIDCToken，实际 %v", err)
	}
	if _, err := p.Exchange(ctx, f.mintCode(challenge, "nonce-A", "sub-1", 0, nil), verifier, "nonce-A"); err != nil {
		t.Fatalf("nonce 一致应通过: %v", err)
	}
}

// TestOIDCDiscoveryIssuerMismatch discovery 的 issuer 与配置不一致拒绝（防混淆端点）。
func TestOIDCDiscoveryIssuerMismatch(t *testing.T) {
	f := newFakeIdP(t)
	f.discoveryIssuer = "https://evil.example.com"
	p := newTestProvider(t, f)
	if _, err := p.AuthorizeURL(context.Background(), "s", mustVerifier(t), ""); !errors.Is(err, ErrOIDCUpstream) {
		t.Fatalf("issuer 不一致应 ErrOIDCUpstream，实际 %v", err)
	}
}

// TestOIDCStatesConsumeOnce state 一次性：首次消费成功，重放/未知/并发全部拒绝。
func TestOIDCStatesConsumeOnce(t *testing.T) {
	st := NewOIDCStates(store.NewMemKV())
	ctx := context.Background()
	state, err := st.Issue(ctx, OIDCState{Verifier: "v", Dev: "desktop"})
	if err != nil {
		t.Fatal(err)
	}
	got, err := st.Consume(ctx, state)
	if err != nil || got.Verifier != "v" {
		t.Fatalf("首次消费失败: %v %+v", err, got)
	}
	if _, err := st.Consume(ctx, state); !errors.Is(err, ErrOIDCState) {
		t.Fatalf("重放应拒绝，实际 %v", err)
	}
	if _, err := st.Consume(ctx, "unknown-state"); !errors.Is(err, ErrOIDCState) {
		t.Fatalf("未知 state 应拒绝，实际 %v", err)
	}
}

// TestOIDCStatesTTL 超过 5 分钟 TTL 的 state 过期拒绝。KV 路径的 TTL 由
// store.KV 自身到期语义承担（MemKV/Redis 各自测试覆盖），这里测进程内后备表
// 的 now 注入路径。
func TestOIDCStatesTTL(t *testing.T) {
	st := NewOIDCStates(nil)
	now := time.Now()
	st.now = func() time.Time { return now }
	ctx := context.Background()
	state, err := st.Issue(ctx, OIDCState{Verifier: "v"})
	if err != nil {
		t.Fatal(err)
	}
	now = now.Add(OIDCStateTTL + time.Second)
	if _, err := st.Consume(ctx, state); !errors.Is(err, ErrOIDCState) {
		t.Fatalf("过期 state 应拒绝，实际 %v", err)
	}
}

// TestOIDCStatesConcurrentReplay 并发消费同一 state：CAS 语义下恰一个成功。
func TestOIDCStatesConcurrentReplay(t *testing.T) {
	st := NewOIDCStates(store.NewMemKV())
	ctx := context.Background()
	state, err := st.Issue(ctx, OIDCState{Verifier: "v"})
	if err != nil {
		t.Fatal(err)
	}
	const n = 8
	results := make(chan error, n)
	for i := 0; i < n; i++ {
		go func() {
			_, err := st.Consume(ctx, state)
			results <- err
		}()
	}
	okCount := 0
	for i := 0; i < n; i++ {
		if err := <-results; err == nil {
			okCount++
		}
	}
	if okCount != 1 {
		t.Fatalf("并发消费成功数 = %d, want 1", okCount)
	}
}

// TestOIDCExchangeTimeout token 端点挂住时 Exchange 必须在超时内返回错误而不是
// 永久阻塞（PR118 P2-4）：x/oauth2 的 Exchange 缺省退回 http.DefaultClient（无超时），
// 修法是把 provider 的带超时 client 注入 ctx 并套同值 deadline。生产 client 超时
// 10s，这里注入 250ms 短超时同步缩短整条链路（ctx deadline 取 p.http.Timeout），
// 断言秒级内以 ErrOIDCUpstream 失败——无超时的旧实现会阻塞到测试超时。
func TestOIDCExchangeTimeout(t *testing.T) {
	hang := make(chan struct{}) // 关闭后挂住的 handler 才返回，让 srv.Close 不卡
	var issuer string
	mux := http.NewServeMux()
	mux.HandleFunc("/.well-known/openid-configuration", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]string{
			"issuer":                 issuer,
			"authorization_endpoint": issuer + "/authorize",
			"token_endpoint":         issuer + "/token",
			"jwks_uri":               issuer + "/jwks.json",
		})
	})
	mux.HandleFunc("/token", func(http.ResponseWriter, *http.Request) {
		<-hang // 模拟 IdP token 端点无响应
	})
	srv := httptest.NewServer(mux)
	defer func() {
		close(hang)
		srv.Close()
	}()
	issuer = srv.URL

	p := NewOIDCProvider(OIDCConfig{
		Issuer: srv.URL, ClientID: "c", ClientSecret: "s", RedirectURL: "https://app.test/cb",
	}, nil)
	p.http = &http.Client{Timeout: 250 * time.Millisecond}

	start := time.Now()
	_, err := p.Exchange(context.Background(), "code", "verifier", "nonce")
	elapsed := time.Since(start)
	if err == nil {
		t.Fatal("token 端点挂住时 Exchange 应返回错误")
	}
	if !errors.Is(err, ErrOIDCUpstream) {
		t.Fatalf("err = %v, want ErrOIDCUpstream", err)
	}
	// 超时 250ms + 调度余量；永久阻塞的旧实现到不了这里的断言。
	if elapsed > 3*time.Second {
		t.Fatalf("Exchange %v 才返回，未在超时内失败", elapsed)
	}
}

// exchangeWith 用给定载荷声明走一次完整交换（每次生成独立 verifier/授权码，
// 授权码在 IdP 侧一次性消费）。
func exchangeWith(t *testing.T, f *fakeIdP, p *OIDCProvider, c fakeCode) (*OIDCClaims, error) {
	t.Helper()
	verifier, challenge, err := NewOIDCPKCE()
	if err != nil {
		t.Fatal(err)
	}
	c.challenge = challenge
	code := f.mintCodeWith(c)
	return p.Exchange(context.Background(), code, verifier, c.nonce)
}

// TestOIDCIDTokenAZP azp 与多 aud 的对应关系：azp 出现时必须等于 client_id；
// 多个 aud 时 azp 必须出现；单 aud 无 azp 合规。
func TestOIDCIDTokenAZP(t *testing.T) {
	f := newFakeIdP(t)
	p := newTestProvider(t, f)
	ptr := func(s string) *string { return &s }
	for _, tc := range []struct {
		name    string
		aud     any
		azp     *string
		wantErr bool
	}{
		{"单 aud 无 azp 允许", nil, nil, false},
		{"单 aud 数组无 azp 允许", []any{f.clientID}, nil, false},
		{"单 aud azp 正确", nil, ptr(f.clientID), false},
		{"单 aud azp 错误", nil, ptr("other-client"), true},
		{"多 aud 无 azp 拒绝", []any{f.clientID, "other-client"}, nil, true},
		{"多 aud azp 正确", []any{f.clientID, "other-client"}, ptr(f.clientID), false},
		{"多 aud azp 错误", []any{f.clientID, "other-client"}, ptr("other-client"), true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := exchangeWith(t, f, p, fakeCode{sub: "sub-azp", aud: tc.aud, azp: tc.azp})
			if tc.wantErr {
				if !errors.Is(err, ErrOIDCToken) {
					t.Fatalf("应 ErrOIDCToken，实际 %v", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("应通过，实际 %v", err)
			}
		})
	}
}

// TestOIDCIDTokenIAT iat 合理性：缺失/未来超时钟偏差/过旧一律拒绝，
// 时钟偏差内的未来时间放行（IdP 与本机时钟可能有小幅漂移）。
func TestOIDCIDTokenIAT(t *testing.T) {
	now := time.Now()
	f := newFakeIdP(t)
	p := newTestProvider(t, f)
	p.now = func() time.Time { return now }
	for _, tc := range []struct {
		name    string
		iat     int64
		omit    bool
		wantErr bool
	}{
		{"正常", now.Unix(), false, false},
		{"时钟偏差内的未来", now.Add(oidcClockSkew / 2).Unix(), false, false},
		{"未来超出偏差", now.Add(2 * oidcClockSkew).Unix(), false, true},
		{"过旧超出上限", now.Add(-oidcIATMaxAge - time.Minute).Unix(), false, true},
		{"缺失 iat", 0, true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := exchangeWith(t, f, p, fakeCode{sub: "sub-iat", iat: tc.iat, omitIAT: tc.omit})
			if tc.wantErr {
				if !errors.Is(err, ErrOIDCToken) {
					t.Fatalf("应 ErrOIDCToken，实际 %v", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("应通过，实际 %v", err)
			}
		})
	}
}

func mustVerifier(t *testing.T) string {
	t.Helper()
	v, _, err := NewOIDCPKCE()
	if err != nil {
		t.Fatal(err)
	}
	return v
}

func mustQuery(t *testing.T, raw string) url.Values {
	t.Helper()
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	return u.Query()
}
