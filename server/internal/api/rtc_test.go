package api

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/OpenALC/LiteTalk/server/internal/rtcapi"
)

// devRTCAuth 测试鉴权桩：dev-<uid> 通过，其余返回内部错误（不得出现在响应里）。
func devRTCAuth(token string) (string, error) {
	if strings.HasPrefix(token, "dev-") && len(token) > 4 {
		return strings.TrimPrefix(token, "dev-"), nil
	}
	return "", errors.New("jwt: token is expired by 1h")
}

// newRTCTestServer 注入 healthz 版本与凭证配置（#77）。cfg/auth 为零值时
// 等价于「TURN 未配置 / 未注入鉴权」。
func newRTCTestServer(t *testing.T, cfg rtcapi.Config, auth rtcapi.UserFromToken) http.Handler {
	t.Helper()
	srv, _ := newTestServer(t)
	srv.version = "v9.9.9-test"
	srv.rtcCfg = cfg
	srv.rtcAuth = auth
	return srv.Handler()
}

// rtcTestConfig 固定时钟的凭证配置，便于复算 username/credential。
func rtcTestConfig(secret string) rtcapi.Config {
	return rtcapi.Config{
		Secret:   secret,
		TurnURLs: []string{"turn:turn.example.com:3478"},
		StunURLs: []string{"stun:stun.example.com:3478"},
		TTL:      12 * time.Hour,
		Now:      func() time.Time { return time.Unix(1734500000, 0) },
	}
}

// TestHealthz 200 形状：{status:"ok", version}，Content-Type 带 charset。
func TestHealthz(t *testing.T) {
	h := newRTCTestServer(t, rtcapi.Config{}, nil)
	req := httptest.NewRequest(http.MethodGet, "/api/v1/healthz", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("期望 200，实际 %d body=%s", rec.Code, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/json; charset=utf-8" {
		t.Fatalf("Content-Type = %q", ct)
	}
	var body struct {
		Status  string `json:"status"`
		Version string `json:"version"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("响应不是合法 JSON: %v", err)
	}
	if body.Status != "ok" || body.Version != "v9.9.9-test" {
		t.Fatalf("healthz 形状不符: %+v", body)
	}
}

// TestRtcCredentialsIssuesTurnCredentials 200 + iceServers 结构，凭证可复算。
func TestRtcCredentialsIssuesTurnCredentials(t *testing.T) {
	h := newRTCTestServer(t, rtcTestConfig("turn-secret"), devRTCAuth)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/rtc/credentials", nil)
	req.Header.Set("Authorization", "Bearer dev-alice")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("期望 200，实际 %d body=%s", rec.Code, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/json; charset=utf-8" {
		t.Fatalf("Content-Type = %q", ct)
	}
	var body struct {
		IceServers []struct {
			Urls       []string `json:"urls"`
			Username   string   `json:"username"`
			Credential string   `json:"credential"`
		} `json:"iceServers"`
		ExpiresAt int64 `json:"expiresAt"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("响应不是合法 JSON: %v", err)
	}
	if len(body.IceServers) != 2 {
		t.Fatalf("期望 2 条 iceServers（stun+turn），实际 %d", len(body.IceServers))
	}
	if body.IceServers[0].Username != "" || body.IceServers[0].Credential != "" {
		t.Fatal("STUN 条目不应携带凭证")
	}
	if len(body.IceServers[0].Urls) != 1 || body.IceServers[0].Urls[0] != "stun:stun.example.com:3478" {
		t.Fatalf("STUN urls = %v", body.IceServers[0].Urls)
	}
	turn := body.IceServers[1]
	const wantUser = "1734543200:alice" // 1734500000 + 12h
	if turn.Username != wantUser {
		t.Fatalf("TURN username = %q，期望 %q", turn.Username, wantUser)
	}
	if turn.Credential != rtcapi.SignCredential("turn-secret", wantUser) {
		t.Fatal("TURN credential 与 HMAC 复算不一致")
	}
	if body.ExpiresAt != 1734543200 {
		t.Fatalf("expiresAt = %d", body.ExpiresAt)
	}
}

// TestRtcCredentialsQueryToken ?token= 与 Authorization: Bearer 等价（与信令一致）。
func TestRtcCredentialsQueryToken(t *testing.T) {
	h := newRTCTestServer(t, rtcTestConfig("turn-secret"), devRTCAuth)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/rtc/credentials?token=dev-bob", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("?token= 应通过，实际 %d body=%s", rec.Code, rec.Body.String())
	}
}

// assertProblem 断言 problem+json 错误体与 code，且不泄露内部错误文本。
func assertProblem(t *testing.T, rec *httptest.ResponseRecorder, wantStatus int, wantCode string) map[string]any {
	t.Helper()
	if rec.Code != wantStatus {
		t.Fatalf("期望 %d，实际 %d body=%s", wantStatus, rec.Code, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/problem+json" {
		t.Fatalf("Content-Type = %q，期望 application/problem+json", ct)
	}
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("错误体不是合法 JSON: %v", err)
	}
	if body["code"] != wantCode {
		t.Fatalf("code = %v，期望 %q", body["code"], wantCode)
	}
	if body["status"] != float64(wantStatus) {
		t.Fatalf("status = %v，期望 %d", body["status"], wantStatus)
	}
	if strings.Contains(rec.Body.String(), "jwt: token is expired") {
		t.Fatal("错误体泄露了内部鉴权错误细节")
	}
	return body
}

// TestRtcCredentialsUnauthorized 无令牌/非法令牌 → 401 problem+json，不泄露细节。
func TestRtcCredentialsUnauthorized(t *testing.T) {
	h := newRTCTestServer(t, rtcTestConfig("turn-secret"), devRTCAuth)
	for _, path := range []string{"/api/v1/rtc/credentials", "/api/v1/rtc/credentials?token=bad"} {
		req := httptest.NewRequest(http.MethodPost, path, nil)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		assertProblem(t, rec, http.StatusUnauthorized, "unauthorized")
	}
}

// TestRtcCredentialsNotConfigured 未配置 TURN secret → 503 turn_unavailable（拒绝静默降级）。
func TestRtcCredentialsNotConfigured(t *testing.T) {
	h := newRTCTestServer(t, rtcTestConfig(""), devRTCAuth)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/rtc/credentials?token=dev-alice", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	assertProblem(t, rec, http.StatusServiceUnavailable, "turn_unavailable")
}

// TestRtcCredentialsNilAuth 未注入鉴权函数 → fail-closed 401。
func TestRtcCredentialsNilAuth(t *testing.T) {
	h := newRTCTestServer(t, rtcTestConfig("turn-secret"), nil)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/rtc/credentials?token=dev-alice", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	assertProblem(t, rec, http.StatusUnauthorized, "unauthorized")
}

// TestRtcCredentialsMethodNotAllowed 非 POST → 405 problem+json 且带 Allow（#77）。
func TestRtcCredentialsMethodNotAllowed(t *testing.T) {
	h := newRTCTestServer(t, rtcTestConfig("turn-secret"), devRTCAuth)
	req := httptest.NewRequest(http.MethodGet, "/api/v1/rtc/credentials?token=dev-alice", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	assertProblem(t, rec, http.StatusMethodNotAllowed, "method_not_allowed")
	if allow := rec.Header().Get("Allow"); !strings.Contains(allow, http.MethodPost) {
		t.Fatalf("Allow = %q，应包含 POST", allow)
	}
}

// TestHealthzMethodNotAllowed healthz 的 405 同样契约化。
func TestHealthzMethodNotAllowed(t *testing.T) {
	h := newRTCTestServer(t, rtcapi.Config{}, nil)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/healthz", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	assertProblem(t, rec, http.StatusMethodNotAllowed, "method_not_allowed")
}
