package rtcapi

import (
	"encoding/base64"
	"encoding/hex"
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
