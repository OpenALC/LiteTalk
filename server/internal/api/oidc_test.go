package api

// oidc_test.go — OIDC 登录 API 面（#54）：httptest 起最小 IdP（discovery +
// token + JWKS 固定密钥），覆盖完整 authorize → callback 换令牌流程与
// 绑定/建号规则；不依赖真实 IdP（批次 6 §7 验收前提）。

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/OpenALC/LiteTalk/server/internal/auth"
	"github.com/OpenALC/LiteTalk/server/internal/store"
)

// ---- 最小 IdP 打桩 ----

type oidcTestClaims struct {
	Sub               string
	Email             string
	EmailVerified     bool
	Name              string
	PreferredUsername string
}

type oidcTestCode struct {
	challenge string
	nonce     string
	claims    oidcTestClaims
	signKey   *rsa.PrivateKey // nil 用正常密钥；非 nil 模拟「签名验不过」
	used      bool
}

// oidcTestIdP 最小 OIDC 提供方：token 端点强制 PKCE（S256(verifier) 必须命中
// code 绑定的 challenge），JWKS 用测试期生成的固定密钥。
type oidcTestIdP struct {
	srv          *httptest.Server
	issuer       string
	key          *rsa.PrivateKey
	kid          string
	clientID     string
	clientSecret string
	redirectURL  string

	mu    sync.Mutex
	codes map[string]oidcTestCode
}

func newOIDCTestIdP(t *testing.T) *oidcTestIdP {
	t.Helper()
	p := &oidcTestIdP{
		kid:          "test-key",
		clientID:     "litetalk-test",
		clientSecret: "test-secret",
		redirectURL:  "https://app.test/oidc/callback",
		codes:        map[string]oidcTestCode{},
	}
	var err error
	if p.key, err = rsa.GenerateKey(rand.Reader, 2048); err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/.well-known/openid-configuration", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]string{
			"issuer":                 p.issuer,
			"authorization_endpoint": p.issuer + "/authorize",
			"token_endpoint":         p.issuer + "/token",
			"jwks_uri":               p.issuer + "/jwks.json",
		})
	})
	mux.HandleFunc("/token", func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			http.Error(w, `{"error":"invalid_request"}`, http.StatusBadRequest)
			return
		}
		p.mu.Lock()
		defer p.mu.Unlock()
		code := r.FormValue("code")
		entry, ok := p.codes[code]
		if !ok || entry.used {
			http.Error(w, `{"error":"invalid_grant"}`, http.StatusBadRequest)
			return
		}
		// client 凭据：Basic 头或表单（x/oauth2 自动探测两种风格都会发）。
		id, secret, basic := r.BasicAuth()
		if !basic {
			id, secret = r.FormValue("client_id"), r.FormValue("client_secret")
		}
		if id != p.clientID || secret != p.clientSecret || r.FormValue("redirect_uri") != p.redirectURL {
			http.Error(w, `{"error":"invalid_client"}`, http.StatusUnauthorized)
			return
		}
		sum := sha256.Sum256([]byte(r.FormValue("code_verifier")))
		if base64.RawURLEncoding.EncodeToString(sum[:]) != entry.challenge {
			http.Error(w, `{"error":"invalid_grant"}`, http.StatusBadRequest)
			return
		}
		entry.used = true
		p.codes[code] = entry
		// Content-Type 必须显式 application/json：不设会被 net/http 嗅探成
		// text/plain，x/oauth2 按 query string 解析响应体而报 cannot parse。
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"access_token": "at-" + code,
			"token_type":   "Bearer",
			"expires_in":   3600,
			"id_token":     p.signIDToken(entry),
		})
	})
	mux.HandleFunc("/jwks.json", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"keys": []map[string]string{{
				"kty": "RSA", "kid": p.kid,
				"n": base64.RawURLEncoding.EncodeToString(p.key.N.Bytes()),
				"e": base64.RawURLEncoding.EncodeToString(oidcBigE(p.key.E)),
			}},
		})
	})
	p.srv = httptest.NewServer(mux)
	t.Cleanup(p.srv.Close)
	p.issuer = p.srv.URL
	return p
}

// oidcBigE JWKS 指数的大端最小字节。
func oidcBigE(e int) []byte {
	var out []byte
	for e > 0 {
		out = append([]byte{byte(e & 0xff)}, out...)
		e >>= 8
	}
	return out
}

// mintCode 模拟「用户在 IdP 授权页同意」：签发绑定 challenge/nonce/claims 的授权码。
func (p *oidcTestIdP) mintCode(challenge, nonce string, claims oidcTestClaims, signKey *rsa.PrivateKey) string {
	p.mu.Lock()
	defer p.mu.Unlock()
	raw := make([]byte, 8)
	if _, err := rand.Read(raw); err != nil {
		panic(err)
	}
	code := "code-" + strings.ToLower(base64.RawURLEncoding.EncodeToString(raw))
	p.codes[code] = oidcTestCode{challenge: challenge, nonce: nonce, claims: claims, signKey: signKey}
	return code
}

func (p *oidcTestIdP) signIDToken(c oidcTestCode) string {
	key := c.signKey
	if key == nil {
		key = p.key
	}
	header, _ := json.Marshal(map[string]string{"alg": "RS256", "kid": p.kid})
	payload, _ := json.Marshal(map[string]any{
		"iss": p.issuer, "sub": c.claims.Sub, "aud": p.clientID,
		"exp": time.Now().Add(time.Hour).Unix(), "iat": time.Now().Unix(),
		"nonce": c.nonce,
		"email": c.claims.Email, "email_verified": c.claims.EmailVerified,
		"name": c.claims.Name, "preferred_username": c.claims.PreferredUsername,
	})
	signingInput := base64.RawURLEncoding.EncodeToString(header) + "." + base64.RawURLEncoding.EncodeToString(payload)
	digest := sha256.Sum256([]byte(signingInput))
	sig, err := rsa.SignPKCS1v15(rand.Reader, key, crypto.SHA256, digest[:])
	if err != nil {
		panic(err)
	}
	return signingInput + "." + base64.RawURLEncoding.EncodeToString(sig)
}

// ---- 测试服务器构造 ----

// newTestServerOIDC 带（或不带）OIDC 配置的测试服务器。
func newTestServerOIDC(t *testing.T, idp *oidcTestIdP, mode string) (*Server, *store.Mem) {
	t.Helper()
	mem := store.NewMem()
	cfg := Config{
		Store:    mem,
		JWT:      auth.NewJWT("test-secret"),
		Refresh:  auth.NewRefreshStore(mem),
		Codes:    auth.NewEmailCodes(mem),
		Limits:   auth.NewRateLimit(mem),
		AuthMode: mode,
		Session:  auth.NewSessionIndex(mem, mem),
		Presence: mem,
	}
	if idp != nil {
		cfg.OIDC = &auth.OIDCConfig{
			Issuer: idp.issuer, ClientID: idp.clientID, ClientSecret: idp.clientSecret,
			RedirectURL: idp.redirectURL,
		}
	}
	return New(cfg), mem
}

// oidcFlow 走完整 authorize → 模拟 IdP 授权 → callback，返回 callback 响应。
func oidcFlow(t *testing.T, h http.Handler, idp *oidcTestIdP, claims oidcTestClaims, wantStatus int) map[string]any {
	t.Helper()
	state, code := oidcAuthorizeFor(t, h, idp, claims, nil)
	return postJSON(t, h, "/api/v1/auth/oidc/callback", "",
		map[string]string{"code": code, "state": state}, wantStatus)
}

// oidcAuthorizeFor 发起授权并模拟 IdP 授权，返回 (state, code)。
// signKey 非 nil 时 IdP 用错误密钥签 ID Token（签名拒绝用例）。
func oidcAuthorizeFor(t *testing.T, h http.Handler, idp *oidcTestIdP, claims oidcTestClaims, signKey *rsa.PrivateKey) (state, code string) {
	t.Helper()
	resp := postJSON(t, h, "/api/v1/auth/oidc/authorize", "", map[string]string{"device": "desktop"}, http.StatusOK)
	rawURL, ok := resp["url"].(string)
	if !ok || rawURL == "" {
		t.Fatalf("authorize 未返回 url: %v", resp)
	}
	u, err := url.Parse(rawURL)
	if err != nil {
		t.Fatal(err)
	}
	q := u.Query()
	if q.Get("response_type") != "code" || q.Get("client_id") != idp.clientID ||
		q.Get("redirect_uri") != idp.redirectURL {
		t.Fatalf("授权 URL 参数缺失: %v", q)
	}
	if q.Get("code_challenge_method") != "S256" || q.Get("code_challenge") == "" {
		t.Fatal("授权 URL 必须带 S256 code_challenge")
	}
	if q.Get("state") == "" || q.Get("nonce") == "" {
		t.Fatal("授权 URL 必须带 state 与 nonce")
	}
	if u.Scheme+"://"+u.Host+u.Path != idp.issuer+"/authorize" {
		t.Fatalf("授权端点不是 IdP 的 authorize: %s", rawURL)
	}
	return q.Get("state"), idp.mintCode(q.Get("code_challenge"), q.Get("nonce"), claims, signKey)
}

// mustCreateUser 测试预置本地用户。
func mustCreateUser(t *testing.T, mem *store.Mem, email, nickname, tag string) *store.User {
	t.Helper()
	u := &store.User{Email: email, Nickname: nickname, Tag: tag, PasswordHash: "", EmailVerified: true}
	if err := mem.CreateUser(context.Background(), u); err != nil {
		t.Fatal(err)
	}
	return u
}

// ---- 用例 ----

// TestOidcLoginHappyPath 完整流程：授权 → 回调换令牌 → 建号（已验证邮箱）→
// 同一外部身份二次登录进同一账号（(issuer, sub) 绑定）。
func TestOidcLoginHappyPath(t *testing.T) {
	idp := newOIDCTestIdP(t)
	srv, mem := newTestServerOIDC(t, idp, AuthModeFull)
	h := srv.Handler()
	ctx := context.Background()

	claims := oidcTestClaims{Sub: "sub-1", Email: "alice@ex.com", EmailVerified: true, PreferredUsername: "alice"}
	resp := oidcFlow(t, h, idp, claims, http.StatusOK)
	if resp["access_token"] == "" || resp["refresh_token"] == "" {
		t.Fatalf("回调未返回令牌对: %v", resp)
	}
	self, _ := resp["user"].(map[string]any)
	if self == nil || self["email"] != "alice@ex.com" {
		t.Fatalf("回调响应 user 与本地登录不同构: %v", resp)
	}
	// 建号断言：已验证邮箱上号、昵称取 IdP 声明、无本地密码。
	u, err := mem.UserByEmail(ctx, "alice@ex.com")
	if err != nil || u.Nickname != "alice" || u.EmailVerified != true || u.PasswordHash != "" {
		t.Fatalf("OIDC 建号字段不符: %+v err=%v", u, err)
	}
	uid := u.ID
	// 绑定行存在且指向该用户。
	idn, err := mem.OIDCIdentityByIssuerSub(ctx, idp.issuer, "sub-1")
	if err != nil || idn.UserID != uid {
		t.Fatalf("绑定行缺失或指向错误: %+v err=%v", idn, err)
	}
	// 二次登录：同一 sub → 同一账号（无第二行）。
	resp2 := oidcFlow(t, h, idp, claims, http.StatusOK)
	if resp2["access_token"] == "" {
		t.Fatalf("二次登录失败: %v", resp2)
	}
	self2, _ := resp2["user"].(map[string]any)
	if self2["id"] != self["id"] {
		t.Fatalf("同一外部身份应进同一账号: %v vs %v", self["id"], self2["id"])
	}
}

// TestOidcStateReplayRejected state 一次性消费：重放与伪造 state 均拒绝。
func TestOidcStateReplayRejected(t *testing.T) {
	idp := newOIDCTestIdP(t)
	srv, _ := newTestServerOIDC(t, idp, AuthModeFull)
	h := srv.Handler()
	claims := oidcTestClaims{Sub: "sub-replay", Email: "replay@ex.com", EmailVerified: true}

	state, code := oidcAuthorizeFor(t, h, idp, claims, nil)
	postJSON(t, h, "/api/v1/auth/oidc/callback", "",
		map[string]string{"code": code, "state": state}, http.StatusOK)
	// 同一 state 重放（无论 code 是否再用）一律 400 bad_state。
	errResp := postJSON(t, h, "/api/v1/auth/oidc/callback", "",
		map[string]string{"code": code, "state": state}, http.StatusBadRequest)
	if errResp["code"] != "bad_state" {
		t.Fatalf("重放 code = %v, want bad_state", errResp["code"])
	}
	// 未知 state 同口径。
	errResp2 := postJSON(t, h, "/api/v1/auth/oidc/callback", "",
		map[string]string{"code": code, "state": "forged"}, http.StatusBadRequest)
	if errResp2["code"] != "bad_state" {
		t.Fatalf("伪造 state code = %v, want bad_state", errResp2["code"])
	}
}

// TestOidcPKCEEnforcedByIdP fake IdP 强制 PKCE：回调触发的 code 换令牌必须带
// 与授权时 challenge 匹配的 verifier（服务端存取链路正确性的端到端判据）。
// verifier 不匹配的独立单元在 internal/auth（TestOIDCPKCEVerifierMismatch）。
func TestOidcPKCEEnforcedByIdP(t *testing.T) {
	idp := newOIDCTestIdP(t)
	srv, _ := newTestServerOIDC(t, idp, AuthModeFull)
	h := srv.Handler()
	claims := oidcTestClaims{Sub: "sub-pkce", Email: "pkce@ex.com", EmailVerified: true}
	oidcFlow(t, h, idp, claims, http.StatusOK)
}

// TestOidcEmailUnverifiedCreatesAccount email 未验证绝不自动绑定：邮箱与既有
// 用户相同也新建账号（占位邮箱），既有用户不受影响。
func TestOidcEmailUnverifiedCreatesAccount(t *testing.T) {
	idp := newOIDCTestIdP(t)
	srv, mem := newTestServerOIDC(t, idp, AuthModeFull)
	h := srv.Handler()
	ctx := context.Background()
	bob := mustCreateUser(t, mem, "bob@ex.com", "bob", "0001")

	claims := oidcTestClaims{Sub: "sub-2", Email: "bob@ex.com", EmailVerified: false, PreferredUsername: "bob-oidc"}
	resp := oidcFlow(t, h, idp, claims, http.StatusOK)
	if resp["access_token"] == "" {
		t.Fatalf("回调失败: %v", resp)
	}
	self, _ := resp["user"].(map[string]any)
	if self["id"] == idOf(bob.ID) {
		t.Fatal("email 未验证却自动绑定了既有用户")
	}
	if self["email"] == "bob@ex.com" {
		t.Fatalf("未验证邮箱不应写上新账号: %v", self["email"])
	}
	idn, err := mem.OIDCIdentityByIssuerSub(ctx, idp.issuer, "sub-2")
	if err != nil || idn.UserID == bob.ID {
		t.Fatalf("新账号未绑定外部身份: %+v err=%v", idn, err)
	}
}

// TestOidcEmailVerifiedAutoBinds email_verified 且邮箱命中既有用户 → 自动绑定，
// 不新建账号。
func TestOidcEmailVerifiedAutoBinds(t *testing.T) {
	idp := newOIDCTestIdP(t)
	srv, mem := newTestServerOIDC(t, idp, AuthModeFull)
	h := srv.Handler()
	ctx := context.Background()
	carol := mustCreateUser(t, mem, "carol@ex.com", "carol", "0002")

	claims := oidcTestClaims{Sub: "sub-3", Email: "carol@ex.com", EmailVerified: true, PreferredUsername: "carol"}
	resp := oidcFlow(t, h, idp, claims, http.StatusOK)
	self, _ := resp["user"].(map[string]any)
	// ID 在 wire 上是十进制字符串（#31），与 carol.ID 的十进制串比对。
	if got, _ := self["id"].(string); got != strconv.FormatInt(carol.ID, 10) {
		t.Fatalf("应绑定既有账号 %d，实际 %v", carol.ID, self["id"])
	}
	idn, err := mem.OIDCIdentityByIssuerSub(ctx, idp.issuer, "sub-3")
	if err != nil || idn.UserID != carol.ID {
		t.Fatalf("绑定行缺失或指向错误: %+v err=%v", idn, err)
	}
}

// TestOidcNicknameConflictGeneratesTag 昵称冲突：IdP 昵称与既有用户相同时，
// 新账号同昵称但 tag 重摇（复用 nickname#tag 唯一逻辑），登录不受影响。
func TestOidcNicknameConflictGeneratesTag(t *testing.T) {
	idp := newOIDCTestIdP(t)
	srv, mem := newTestServerOIDC(t, idp, AuthModeFull)
	h := srv.Handler()
	mustCreateUser(t, mem, "dan@ex.com", "丹尼尔", "1234")

	claims := oidcTestClaims{Sub: "sub-4", Email: "daniel@ex.com", EmailVerified: true, PreferredUsername: "丹尼尔"}
	resp := oidcFlow(t, h, idp, claims, http.StatusOK)
	if resp["access_token"] == "" {
		t.Fatalf("回调失败: %v", resp)
	}
	u, err := mem.UserByEmail(context.Background(), "daniel@ex.com")
	if err != nil {
		t.Fatal(err)
	}
	if u.Nickname != "丹尼尔" {
		t.Fatalf("昵称 = %q, want 丹尼尔", u.Nickname)
	}
	if u.Tag == "1234" {
		t.Fatal("昵称冲突时 tag 未重摇")
	}
}

// TestOidcCallbackBadSignature ID Token 签名验不过 → 400 bad_code（不建号）。
func TestOidcCallbackBadSignature(t *testing.T) {
	idp := newOIDCTestIdP(t)
	srv, mem := newTestServerOIDC(t, idp, AuthModeFull)
	h := srv.Handler()
	wrongKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	claims := oidcTestClaims{Sub: "sub-5", Email: "forge@ex.com", EmailVerified: true}
	state, code := oidcAuthorizeFor(t, h, idp, claims, wrongKey)
	errResp := postJSON(t, h, "/api/v1/auth/oidc/callback", "",
		map[string]string{"code": code, "state": state}, http.StatusBadRequest)
	if errResp["code"] != "bad_code" {
		t.Fatalf("签名拒绝 code = %v, want bad_code", errResp["code"])
	}
	if _, e := mem.UserByEmail(context.Background(), "forge@ex.com"); e == nil {
		t.Fatal("签名拒绝却建了号")
	}
}

// TestOidcOnlyModeLocalEntriesDisabled 仅 OIDC 模式：密码登录/注册/找回 403，
// 游客 403（simple 专属不变），email-code 400。
func TestOidcOnlyModeLocalEntriesDisabled(t *testing.T) {
	idp := newOIDCTestIdP(t)
	srv, _ := newTestServerOIDC(t, idp, AuthModeOIDC)
	h := srv.Handler()

	errResp := postJSON(t, h, "/api/v1/auth/login", "", map[string]string{
		"email": "x@ex.com", "password": "password123",
	}, http.StatusForbidden)
	if errResp["code"] != "password_login_disabled" {
		t.Fatalf("密码登录 code = %v, want password_login_disabled", errResp["code"])
	}
	errResp = postJSON(t, h, "/api/v1/auth/register", "", map[string]string{
		"nickname": "某人", "email": "x@ex.com", "password": "password123",
	}, http.StatusForbidden)
	if errResp["code"] != "register_disabled" {
		t.Fatalf("注册 code = %v, want register_disabled", errResp["code"])
	}
	errResp = postJSON(t, h, "/api/v1/auth/password/reset", "", map[string]string{
		"email": "x@ex.com", "code": "000000", "password": "password123",
	}, http.StatusForbidden)
	if errResp["code"] != "reset_unavailable" {
		t.Fatalf("找回密码 code = %v, want reset_unavailable", errResp["code"])
	}
	errResp = postJSON(t, h, "/api/v1/auth/email-code", "", map[string]string{
		"email": "x@ex.com", "purpose": "register",
	}, http.StatusBadRequest)
	if errResp["code"] != "verification_not_required" {
		t.Fatalf("email-code code = %v, want verification_not_required", errResp["code"])
	}
	errResp = postJSON(t, h, "/api/v1/auth/guest", "", map[string]string{}, http.StatusForbidden)
	if errResp["code"] != "guest_disabled" {
		t.Fatalf("游客登录 code = %v, want guest_disabled", errResp["code"])
	}

	// /config：auth_mode=oidc 且 oidc_enabled=true。
	cfg, _ := getJSON(t, h, "/api/v1/config", "", http.StatusOK).(map[string]any)
	if cfg["auth_mode"] != "oidc" {
		t.Fatalf("auth_mode = %v, want oidc", cfg["auth_mode"])
	}
	if cfg["oidc_enabled"] != true {
		t.Fatalf("oidc_enabled = %v, want true", cfg["oidc_enabled"])
	}
}

// TestOidcEnabledReflectsConfig oidc_enabled 反映配置（开/关）。
func TestOidcEnabledReflectsConfig(t *testing.T) {
	idp := newOIDCTestIdP(t)
	srvOn, _ := newTestServerOIDC(t, idp, AuthModeFull)
	cfgOn, _ := getJSON(t, srvOn.Handler(), "/api/v1/config", "", http.StatusOK).(map[string]any)
	if cfgOn["oidc_enabled"] != true || cfgOn["auth_mode"] != "full" {
		t.Fatalf("配置完整应 oidc_enabled=true 且 auth_mode 不变: %v", cfgOn)
	}

	srvOff, _ := newTestServerOIDC(t, nil, AuthModeFull)
	cfgOff, _ := getJSON(t, srvOff.Handler(), "/api/v1/config", "", http.StatusOK).(map[string]any)
	if cfgOff["oidc_enabled"] != false || cfgOff["auth_mode"] != "full" {
		t.Fatalf("未配置应 oidc_enabled=false 且 auth_mode 不变: %v", cfgOff)
	}
}

// TestOidcDisabledZeroSideEffects 未配置 = 端点 404 且零副作用：不构造
// provider/state 表，无任何后台资源。
func TestOidcDisabledZeroSideEffects(t *testing.T) {
	srv, _ := newTestServerOIDC(t, nil, AuthModeFull)
	h := srv.Handler()
	if srv.oidc != nil || srv.oidcStates != nil {
		t.Fatal("未配置时不应构造 OIDC 依赖方与状态表")
	}
	resp := postJSON(t, h, "/api/v1/auth/oidc/authorize", "", map[string]string{}, http.StatusNotFound)
	if resp["code"] != "not_found" {
		t.Fatalf("authorize code = %v, want not_found", resp["code"])
	}
	resp = postJSON(t, h, "/api/v1/auth/oidc/callback", "",
		map[string]string{"code": "c", "state": "s"}, http.StatusNotFound)
	if resp["code"] != "not_found" {
		t.Fatalf("callback code = %v, want not_found", resp["code"])
	}
}

// TestOidcAuthorizeBadDevice 设备类型白名单照常生效。
func TestOidcAuthorizeBadDevice(t *testing.T) {
	idp := newOIDCTestIdP(t)
	srv, _ := newTestServerOIDC(t, idp, AuthModeFull)
	errResp := postJSON(t, srv.Handler(), "/api/v1/auth/oidc/authorize", "",
		map[string]string{"device": "tablet"}, http.StatusBadRequest)
	if errResp["code"] != "bad_device" {
		t.Fatalf("code = %v, want bad_device", errResp["code"])
	}
}
