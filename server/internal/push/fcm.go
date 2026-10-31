package push

import (
	"bytes"
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// fcmScope Firebase HTTP v1 需要的 OAuth scope。
const fcmScope = "https://www.googleapis.com/auth/firebase.messaging"

// fcmAPIBase FCM HTTP v1 的发送端点前缀（项目 ID 拼在后面）。
const fcmAPIBase = "https://fcm.googleapis.com/v1/projects/"

// FCM Firebase Cloud Messaging HTTP v1（sendEach 语义：逐设备一请求）。
// 凭据是服务账号 JSON（LITETALK_PUSH_FCM_CREDENTIALS，内联 JSON 或文件路径），
// 走 JWT-bearer 换 OAuth access token：高熵凭据 + 标准流程，stdlib 即可，
// 不引入 firebase SDK（本任务零新依赖）。
type FCM struct {
	// Credentials 服务账号 JSON 原文（含 project_id/private_key/client_email/token_uri）。
	Credentials string
	Log         *log.Logger
	// Client 承担 OAuth 换 token 与消息发送两种请求；测试注入桩时
	// 凭据里的 token_uri 指向 httptest 即可复用同一个 client。
	Client *http.Client
	// apiBase 测试覆盖用（空 = fcmAPIBase）。
	apiBase string

	mu          sync.Mutex
	accessToken string
	tokenExpiry time.Time
	sa          *fcmServiceAccount
	key         *rsa.PrivateKey
	credErr     error // 凭据解析失败缓存：坏凭据不必每次投递都重新解析
}

// fcmServiceAccount 服务账号 JSON 的必要字段。
type fcmServiceAccount struct {
	ProjectID   string `json:"project_id"`
	ClientEmail string `json:"client_email"`
	PrivateKey  string `json:"private_key"`
	TokenURI    string `json:"token_uri"`
}

// fcmTokenResponse OAuth token 端点响应。
type fcmTokenResponse struct {
	AccessToken string `json:"access_token"`
	ExpiresIn   int64  `json:"expires_in"`
}

// fcmMessage HTTP v1 请求体：单 token 单通知。
type fcmMessage struct {
	Message struct {
		Token        string `json:"token"`
		Notification struct {
			Title string `json:"title"`
			Body  string `json:"body"`
		} `json:"notification"`
		// FcmOptions 提供点击路由；Android 端映射 route 到页面。
		FcmOptions struct {
			Link string `json:"link,omitempty"`
		} `json:"fcm_options,omitempty"`
	} `json:"message"`
}

// Push 逐设备投递。token 失效（401/404）在 FCM 是常态（应用卸载/轮换），
// 只记日志不整体失败。
func (f *FCM) Push(ctx context.Context, devices []Device, notif Notification) error {
	for _, d := range devices {
		if d.Token == "" {
			continue
		}
		if err := f.pushOne(ctx, d, notif); err != nil {
			safeLog(f.Log).Printf("[push/fcm] 设备 %s 投递失败: %v", MaskToken(d.Token), err)
		}
	}
	return nil
}

func (f *FCM) pushOne(ctx context.Context, d Device, notif Notification) error {
	sa, key, err := f.account()
	if err != nil {
		return err
	}
	tok, err := f.oauthToken(ctx, sa, key)
	if err != nil {
		return err
	}
	var msg fcmMessage
	msg.Message.Token = d.Token
	msg.Message.Notification.Title = notif.Title
	msg.Message.Notification.Body = notif.Body
	msg.Message.FcmOptions.Link = notif.Route
	body, err := json.Marshal(msg)
	if err != nil {
		return err
	}
	base := f.apiBase
	if base == "" {
		base = fcmAPIBase
	}
	endpoint := base + sa.ProjectID + "/messages:send"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+tok)
	resp, err := f.client().Do(req)
	if err != nil {
		return fmt.Errorf("fcm: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return fmt.Errorf("fcm: HTTP %d", resp.StatusCode)
	}
	return nil
}

// oauthToken 取缓存的 access token，临期（<60s）或缺失时走 JWT-bearer 换新。
// 换 token 失败不缓存错误，下次重试。
func (f *FCM) oauthToken(ctx context.Context, sa *fcmServiceAccount, key *rsa.PrivateKey) (string, error) {
	f.mu.Lock()
	if f.accessToken != "" && time.Until(f.tokenExpiry) > time.Minute {
		tok := f.accessToken
		f.mu.Unlock()
		return tok, nil
	}
	f.mu.Unlock()

	assertion, err := fcmAssertion(sa, key)
	if err != nil {
		return "", err
	}
	form := url.Values{
		"grant_type": {"urn:ietf:params:oauth:grant-type:jwt-bearer"},
		"assertion":  {assertion},
	}
	tokenURI := sa.TokenURI
	if tokenURI == "" {
		tokenURI = "https://oauth2.googleapis.com/token"
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, tokenURI,
		strings.NewReader(form.Encode()))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := f.client().Do(req)
	if err != nil {
		return "", fmt.Errorf("fcm oauth: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return "", fmt.Errorf("fcm oauth: HTTP %d", resp.StatusCode)
	}
	var tr fcmTokenResponse
	if err := json.NewDecoder(resp.Body).Decode(&tr); err != nil {
		return "", err
	}
	if tr.AccessToken == "" {
		return "", errors.New("fcm oauth: 空 access_token")
	}
	f.mu.Lock()
	f.accessToken = tr.AccessToken
	expiry := time.Duration(tr.ExpiresIn) * time.Second
	if expiry <= 0 {
		expiry = time.Hour
	}
	f.tokenExpiry = time.Now().Add(expiry)
	f.mu.Unlock()
	return tr.AccessToken, nil
}

// fcmAssertion 签出 RS256 JWT（stdlib：PKCS#8 解析 + PKCS1v15 签名）。
func fcmAssertion(sa *fcmServiceAccount, key *rsa.PrivateKey) (string, error) {
	now := time.Now()
	header, err := json.Marshal(map[string]string{"alg": "RS256", "typ": "JWT"})
	if err != nil {
		return "", err
	}
	claims, err := json.Marshal(map[string]any{
		"iss":   sa.ClientEmail,
		"scope": fcmScope,
		"aud":   sa.TokenURI,
		"iat":   now.Unix(),
		"exp":   now.Add(time.Hour).Unix(),
	})
	if err != nil {
		return "", err
	}
	var b strings.Builder
	b.WriteString(base64.RawURLEncoding.EncodeToString(header))
	b.WriteByte('.')
	b.WriteString(base64.RawURLEncoding.EncodeToString(claims))
	digest := sha256.Sum256([]byte(b.String()))
	sig, err := rsa.SignPKCS1v15(rand.Reader, key, crypto.SHA256, digest[:])
	if err != nil {
		return "", err
	}
	b.WriteByte('.')
	b.WriteString(base64.RawURLEncoding.EncodeToString(sig))
	return b.String(), nil
}

// account 解析服务账号并缓存：凭据构造后不变，坏凭据缓存错误避免每次重解析。
func (f *FCM) account() (*fcmServiceAccount, *rsa.PrivateKey, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.sa != nil || f.credErr != nil {
		return f.sa, f.key, f.credErr
	}
	f.sa, f.key, f.credErr = parseServiceAccount(f.Credentials)
	return f.sa, f.key, f.credErr
}

func parseServiceAccount(raw string) (*fcmServiceAccount, *rsa.PrivateKey, error) {
	if raw == "" {
		return nil, nil, errors.New("fcm: 未配置服务账号凭据")
	}
	var sa fcmServiceAccount
	if err := json.Unmarshal([]byte(raw), &sa); err != nil {
		return nil, nil, fmt.Errorf("fcm: 服务账号 JSON 解析失败: %w", err)
	}
	if sa.ProjectID == "" || sa.ClientEmail == "" || sa.PrivateKey == "" {
		return nil, nil, errors.New("fcm: 服务账号缺少 project_id/client_email/private_key")
	}
	block, _ := pem.Decode([]byte(sa.PrivateKey))
	if block == nil {
		return nil, nil, errors.New("fcm: private_key 不是合法 PEM")
	}
	parsed, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, nil, fmt.Errorf("fcm: private_key 解析失败: %w", err)
	}
	key, ok := parsed.(*rsa.PrivateKey)
	if !ok {
		return nil, nil, errors.New("fcm: private_key 不是 RSA 私钥")
	}
	return &sa, key, nil
}

func (f *FCM) client() *http.Client {
	if f.Client != nil {
		return f.Client
	}
	return &http.Client{Timeout: 10 * time.Second}
}
