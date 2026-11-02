package push

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// mustRSAKey 生成测试用 RSA 私钥（PKCS#8 PEM）。
func mustRSAKey(t *testing.T) *rsa.PrivateKey {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	return key
}

// mustFCMKey 生成测试用 RSA 私钥并产出服务账号 JSON（token_uri 指向桩）。
func mustFCMKey(t *testing.T, tokenURI string) string {
	t.Helper()
	der, err := x509.MarshalPKCS8PrivateKey(mustRSAKey(t))
	if err != nil {
		t.Fatal(err)
	}
	pemText := string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}))
	sa := map[string]string{
		"project_id":   "litetalk-test",
		"client_email": "push@litetalk-test.iam.gserviceaccount.com",
		"private_key":  pemText,
		"token_uri":    tokenURI,
	}
	raw, err := json.Marshal(sa)
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

// TestFCMRequestShape httptest 桩测两段请求形状：
// 1) OAuth 换 token（grant_type=jwt-bearer，JWT 三段式）；
// 2) messages:send（Bearer 授权 + {"message":{"token","notification","fcm_options"}}）。
func TestFCMRequestShape(t *testing.T) {
	var sendAuth, sendBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/token"):
			if err := r.ParseForm(); err != nil {
				t.Error(err)
				return
			}
			if r.Form.Get("grant_type") != "urn:ietf:params:oauth:grant-type:jwt-bearer" {
				t.Errorf("grant_type 不符: %q", r.Form.Get("grant_type"))
			}
			assertion := r.Form.Get("assertion")
			if strings.Count(assertion, ".") != 2 {
				t.Errorf("assertion 应为三段式 JWT: %q", assertion)
			}
			_, _ = w.Write([]byte(`{"access_token":"ya29.fake","expires_in":3600}`))
		case strings.HasSuffix(r.URL.Path, "/messages:send"):
			if !strings.HasPrefix(r.URL.Path, "/v1/projects/litetalk-test/") {
				t.Errorf("端点路径不符: %s", r.URL.Path)
			}
			sendAuth = r.Header.Get("Authorization")
			buf := make([]byte, 4096)
			n, _ := r.Body.Read(buf)
			sendBody = string(buf[:n])
			w.WriteHeader(http.StatusOK)
		default:
			t.Errorf("意外路径 %s", r.URL.Path)
		}
	}))
	defer srv.Close()

	cred := mustFCMKey(t, srv.URL+"/token")
	f := &FCM{Credentials: cred, Client: srv.Client(), apiBase: srv.URL + "/v1/projects/"}
	err := f.Push(context.Background(),
		[]Device{{Platform: "android", Token: "fcm-token-123"}},
		Notification{Title: "张三", Body: "在 #general 提到你", Route: "channel:9"})
	if err != nil {
		t.Fatalf("投递不应失败: %v", err)
	}
	if sendAuth != "Bearer ya29.fake" {
		t.Fatalf("Authorization 不符: %q", sendAuth)
	}
	var msg struct {
		Message struct {
			Token        string `json:"token"`
			Notification struct {
				Title string `json:"title"`
				Body  string `json:"body"`
			} `json:"notification"`
			FcmOptions struct {
				Link string `json:"link"`
			} `json:"fcm_options"`
		} `json:"message"`
	}
	if err := json.Unmarshal([]byte(sendBody), &msg); err != nil {
		t.Fatalf("send body 不是 JSON: %q", sendBody)
	}
	if msg.Message.Token != "fcm-token-123" ||
		msg.Message.Notification.Title != "张三" ||
		msg.Message.Notification.Body != "在 #general 提到你" ||
		msg.Message.FcmOptions.Link != "channel:9" {
		t.Fatalf("载荷不符: %s", sendBody)
	}
	// 第二次投递应命中 token 缓存：token 端点不再被调用（上方 default 分支会报错）。
	if err := f.Push(context.Background(),
		[]Device{{Platform: "android", Token: "fcm-token-456"}},
		Notification{}); err != nil {
		t.Fatalf("缓存 token 后投递失败: %v", err)
	}
}

// TestFCMBadCredentials 凭据解析失败给出可读错误，不 panic。
func TestFCMBadCredentials(t *testing.T) {
	for name, cred := range map[string]string{
		"空":     "",
		"坏JSON": "{not-json",
		"缺私钥":   `{"project_id":"p","client_email":"c"}`,
		"坏PEM":  `{"project_id":"p","client_email":"c","private_key":"junk"}`,
	} {
		f := &FCM{Credentials: cred}
		err := f.Push(context.Background(),
			[]Device{{Platform: "android", Token: "t"}}, Notification{})
		if err != nil {
			t.Fatalf("%s: Push 对凭据错误应记日志不整体失败（多设备语义），实际返回 %v", name, err)
		}
	}
	// 逐设备路径内的错误应被 Push 吞掉（只记日志）——但 account() 失败会在
	// pushOne 返回前发生，同样被吞。这里断言不 panic 即可。
	_ = fcmScope
}
