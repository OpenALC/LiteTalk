package rtcapi

import (
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// TestSignCredentialRFC2202 用 RFC 2202 HMAC-SHA1 测试向量 2 校验签名算法本身。
func TestSignCredentialRFC2202(t *testing.T) {
	got := SignCredential("Jefe", "what do ya want for nothing?")
	raw, err := base64.StdEncoding.DecodeString(got)
	if err != nil {
		t.Fatalf("凭证不是合法 base64: %v", err)
	}
	const want = "effcdf6ae5eb2fa2d27416d5f184df9c259a7c79"
	if hex.EncodeToString(raw) != want {
		t.Fatalf("HMAC-SHA1 不匹配 RFC 2202 向量，want %s got %s", want, hex.EncodeToString(raw))
	}
}

// TestIssue 校验 coturn REST 格式：username=<expiry_unix>:<uid>。
func TestIssue(t *testing.T) {
	cfg := Config{
		Secret: "turn-secret",
		TTL:    12 * time.Hour,
		Now:    func() time.Time { return time.Unix(1734500000, 0) },
	}
	username, credential, expiry := cfg.Issue("alice")

	const wantUser = "1734543200:alice" // 1734500000 + 12h
	if username != wantUser {
		t.Fatalf("username 格式不符，want %q got %q", wantUser, username)
	}
	if expiry.Unix() != 1734543200 {
		t.Fatalf("expiry 不符，want 1734543200 got %d", expiry.Unix())
	}
	if want := SignCredential("turn-secret", wantUser); credential != want {
		t.Fatalf("credential 不符，want %q got %q", want, credential)
	}
}

func newTestHandler(secret string) *Handler {
	return NewHandler(Config{
		Secret:   secret,
		TurnURLs: []string{"turn:turn.example.com:3478"},
		StunURLs: []string{"stun:stun.example.com:3478"},
		TTL:      12 * time.Hour,
		Now:      func() time.Time { return time.Unix(1734500000, 0) },
	}, func(token string) (string, error) {
		if strings.HasPrefix(token, "dev-") && len(token) > 4 {
			return strings.TrimPrefix(token, "dev-"), nil
		}
		return "", http.ErrNoCookie
	}, nil)
}

// TestHandlerIssuesTurnCredentials 完整 HTTP 一轮：200 + iceServers 结构与凭证可复算。
func TestHandlerIssuesTurnCredentials(t *testing.T) {
	h := newTestHandler("turn-secret")
	req := httptest.NewRequest(http.MethodPost, "/api/v1/rtc/credentials?token=dev-alice", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	res := rec.Result()
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("期望 200，实际 %d", res.StatusCode)
	}
	var body CredentialsResponse
	if err := json.NewDecoder(res.Body).Decode(&body); err != nil {
		t.Fatalf("响应不是合法 JSON: %v", err)
	}
	if len(body.ICEServers) != 2 {
		t.Fatalf("期望 2 条 iceServers（stun+turn），实际 %d", len(body.ICEServers))
	}
	if body.ICEServers[0].Username != "" || body.ICEServers[0].Credential != "" {
		t.Fatal("STUN 条目不应携带凭证")
	}
	turn := body.ICEServers[1]
	if turn.Username != "1734543200:alice" {
		t.Fatalf("TURN username 不符，got %q", turn.Username)
	}
	if turn.Credential != SignCredential("turn-secret", turn.Username) {
		t.Fatal("TURN credential 与 HMAC 复算不一致")
	}
	if body.ExpiresAt != 1734543200 {
		t.Fatalf("expiresAt 不符，got %d", body.ExpiresAt)
	}
}

// TestHandlerBearerToken Authorization: Bearer 与 ?token= 等价。
func TestHandlerBearerToken(t *testing.T) {
	h := newTestHandler("turn-secret")
	req := httptest.NewRequest(http.MethodPost, "/api/v1/rtc/credentials", nil)
	req.Header.Set("Authorization", "Bearer dev-bob")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Result().StatusCode != http.StatusOK {
		t.Fatalf("Bearer 令牌应通过，实际 %d", rec.Result().StatusCode)
	}
}

// TestHandlerUnauthorized 无令牌 / 非法令牌 → 401。
func TestHandlerUnauthorized(t *testing.T) {
	h := newTestHandler("turn-secret")
	for _, path := range []string{"/api/v1/rtc/credentials", "/api/v1/rtc/credentials?token=bad"} {
		req := httptest.NewRequest(http.MethodPost, path, nil)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Result().StatusCode != http.StatusUnauthorized {
			t.Fatalf("%s 期望 401，实际 %d", path, rec.Result().StatusCode)
		}
	}
}

// TestHandlerMethodNotAllowed 非 POST → 405。
func TestHandlerMethodNotAllowed(t *testing.T) {
	h := newTestHandler("turn-secret")
	req := httptest.NewRequest(http.MethodGet, "/api/v1/rtc/credentials?token=dev-alice", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Result().StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("期望 405，实际 %d", rec.Result().StatusCode)
	}
}

// TestHandlerSecretNotConfigured 未配置 TURN secret → 503（拒绝静默降级）。
func TestHandlerSecretNotConfigured(t *testing.T) {
	h := newTestHandler("")
	req := httptest.NewRequest(http.MethodPost, "/api/v1/rtc/credentials?token=dev-alice", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Result().StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("期望 503，实际 %d", rec.Result().StatusCode)
	}
}

// TestDefaultURLs 内置 TURN 回填：host+端口生成 STUN 与 TURN(udp/tcp) 三条 URL。
func TestDefaultURLs(t *testing.T) {
	turnURLs, stunURLs := DefaultURLs("voice.example.com", 3478)
	if len(stunURLs) != 1 || stunURLs[0] != "stun:voice.example.com:3478" {
		t.Fatalf("stun URLs = %v", stunURLs)
	}
	if len(turnURLs) != 2 {
		t.Fatalf("turn URLs 应有 udp/tcp 两条，实际 %v", turnURLs)
	}
	if turnURLs[0] != "turn:voice.example.com:3478?transport=udp" || turnURLs[1] != "turn:voice.example.com:3478?transport=tcp" {
		t.Fatalf("turn URLs = %v", turnURLs)
	}
	// host 或端口非法时返回空（不回填，交由显式 env 或 503 路径）
	if u, s := DefaultURLs("", 3478); u != nil || s != nil {
		t.Fatalf("空 host 应不回填，实际 %v %v", u, s)
	}
	if u, s := DefaultURLs("h", 0); u != nil || s != nil {
		t.Fatalf("端口 0 应不回填，实际 %v %v", u, s)
	}
}
