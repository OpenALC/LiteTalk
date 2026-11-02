package push

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// mustAPNsKey 生成测试用 P-256 私钥（.p8 形态）。
func mustAPNsKey(t *testing.T) (*ecdsa.PrivateKey, string) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	der, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	return key, string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}))
}

// TestAPNsRequestShape httptest 桩测请求形状：
// POST /3/device/{token}，bearer JWT（三段式、ES256）、apns-topic、apns-push-type，
// body {"aps":{"alert":{"title","body"}}}。
func TestAPNsRequestShape(t *testing.T) {
	var gotPath, gotAuth, gotTopic, gotPushType, gotBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotAuth = r.Header.Get("Authorization")
		gotTopic = r.Header.Get("apns-topic")
		gotPushType = r.Header.Get("apns-push-type")
		buf := make([]byte, 4096)
		n, _ := r.Body.Read(buf)
		gotBody = string(buf[:n])
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	_, pemText := mustAPNsKey(t)
	a := &APNs{
		Key: pemText, KeyID: "KEYID123", TeamID: "TEAMID456", Topic: "com.litetalk.app",
		Client: srv.Client(), host: srv.URL,
	}
	if err := a.Push(context.Background(),
		[]Device{{Platform: "ios", Token: "apns-device-token"}},
		Notification{Title: "张三", Body: "请求加你为好友"}); err != nil {
		t.Fatalf("投递不应失败: %v", err)
	}
	if gotPath != "/3/device/apns-device-token" {
		t.Fatalf("路径不符: %q", gotPath)
	}
	jwt := strings.TrimPrefix(gotAuth, "bearer ")
	if !strings.HasPrefix(gotAuth, "bearer ") || strings.Count(jwt, ".") != 2 {
		t.Fatalf("Authorization 应为 bearer JWT: %q", gotAuth)
	}
	// JWT 头可解出 kid=KeyID、alg=ES256。
	head, err := base64DecodeJSON(jwt[:strings.Index(jwt, ".")])
	if err != nil {
		t.Fatalf("JWT 头解析失败: %v", err)
	}
	if head["alg"] != "ES256" || head["kid"] != "KEYID123" {
		t.Fatalf("JWT 头不符: %v", head)
	}
	if gotTopic != "com.litetalk.app" {
		t.Fatalf("apns-topic 不符: %q", gotTopic)
	}
	if gotPushType != "alert" {
		t.Fatalf("apns-push-type 不符: %q", gotPushType)
	}
	var payload map[string]any
	if err := json.Unmarshal([]byte(gotBody), &payload); err != nil {
		t.Fatalf("body 不是 JSON: %q", gotBody)
	}
	aps, ok := payload["aps"].(map[string]any)
	if !ok {
		t.Fatalf("缺 aps: %v", payload)
	}
	alert, _ := aps["alert"].(map[string]any)
	if alert["title"] != "张三" || alert["body"] != "请求加你为好友" {
		t.Fatalf("alert 不符: %v", aps)
	}
	// token 缓存：第二次投递仍成功（签名只算一次，无需专门断言次数）。
	if err := a.Push(context.Background(), []Device{{Platform: "ios", Token: "t2"}}, Notification{}); err != nil {
		t.Fatalf("缓存 token 后投递失败: %v", err)
	}
}

// TestAPNsBadKey 凭据解析失败路径：不 panic、Push 吞错记日志。
func TestAPNsBadKey(t *testing.T) {
	for name, key := range map[string]string{
		"空":    "",
		"坏PEM": "junk",
		"非ECDSA": func() string {
			der, _ := x509.MarshalPKCS8PrivateKey(mustRSAKey(t))
			return string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}))
		}(),
	} {
		a := &APNs{Key: key, KeyID: "K", TeamID: "T", Topic: "b"}
		if err := a.Push(context.Background(), []Device{{Platform: "ios", Token: "t"}}, Notification{}); err != nil {
			t.Fatalf("%s: Push 不应整体失败: %v", name, err)
		}
	}
}

// base64DecodeJSON 解 JWT 头（RawURL base64 → map）。
func base64DecodeJSON(seg string) (map[string]any, error) {
	raw, err := base64.RawURLEncoding.DecodeString(seg)
	if err != nil {
		return nil, err
	}
	var m map[string]any
	err = json.Unmarshal(raw, &m)
	return m, err
}
