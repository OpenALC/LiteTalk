package push

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"sync"
	"time"
)

// APNs 推送端点。sandbox 供开发证书使用，由 LITETALK_PUSH_APNS_SANDBOX 切换。
const (
	apnsHostProduction = "https://api.push.apple.com"
	apnsHostSandbox    = "https://api.sandbox.push.apple.com"
)

// APNs Apple Push Notification service（token-based 认证）。
// 凭据三件套：.p8 私钥（PEM）、Key ID、Team ID（LITETALK_PUSH_APNS_*）。
// JWT ES256 由 stdlib 签出（crypto/ecdsa + P1363 定长签名，非 ASN.1——
// JOSE 规范要求 r||s 各 32 字节左填充），token 缓存 55 分钟（Apple 上限 1 小时）。
type APNs struct {
	// Key 是 .p8 私钥 PEM 原文（内联）或文件路径；KeyID/TeamID 来自开发者后台。
	Key    string
	KeyID  string
	TeamID string
	// Topic 是目标 App 的 bundle id（apns-topic 头）。
	Topic   string
	Sandbox bool
	Log     *log.Logger
	Client  *http.Client // nil 时用默认 10s 超时；测试注入 httptest 桩
	host    string       // 测试覆盖用（空 = 按 Sandbox 选官方端点）

	mu        sync.Mutex
	authToken string
	authExp   time.Time
	key       *ecdsa.PrivateKey
	keyErr    error
}

// apnsPayload 请求体：仅 alert 类基本字段，不含消息正文（包红线）。
type apnsPayload struct {
	Aps struct {
		Alert struct {
			Title string `json:"title"`
			Body  string `json:"body"`
		} `json:"alert"`
		Sound string `json:"sound,omitempty"`
	} `json:"aps"`
}

// apnsJWTHeader / apnsJWTClaims token 认证的 JWT 结构。
type apnsJWTHeader struct {
	Alg string `json:"alg"`
	Kid string `json:"kid"`
}

type apnsJWTClaims struct {
	Iss string `json:"iss"`
	Iat int64  `json:"iat"`
}

// Push 逐设备 POST /3/device/{token}。410（token 失效）是常态，只记日志；
// 429/5xx 同样不整体失败——推送是尽力而为的旁路。
func (a *APNs) Push(ctx context.Context, devices []Device, notif Notification) error {
	for _, d := range devices {
		if d.Token == "" {
			continue
		}
		if err := a.pushOne(ctx, d, notif); err != nil {
			safeLog(a.Log).Printf("[push/apns] 设备 %s 投递失败: %v", MaskToken(d.Token), err)
		}
	}
	return nil
}

func (a *APNs) pushOne(ctx context.Context, d Device, notif Notification) error {
	tok, err := a.authJWT()
	if err != nil {
		return err
	}
	var p apnsPayload
	p.Aps.Alert.Title = notif.Title
	p.Aps.Alert.Body = notif.Body
	p.Aps.Sound = "default"
	body, err := json.Marshal(p)
	if err != nil {
		return err
	}
	host := a.host
	if host == "" {
		host = apnsHostProduction
		if a.Sandbox {
			host = apnsHostSandbox
		}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		host+"/3/device/"+d.Token, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "bearer "+tok)
	req.Header.Set("apns-topic", a.Topic)
	req.Header.Set("apns-push-type", "alert")
	resp, err := a.client().Do(req)
	if err != nil {
		return fmt.Errorf("apns: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return fmt.Errorf("apns: HTTP %d", resp.StatusCode)
	}
	return nil
}

// authJWT 取缓存的 provider token，临期（<5 分钟）重签。
func (a *APNs) authJWT() (string, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.authToken != "" && time.Until(a.authExp) > 5*time.Minute {
		return a.authToken, nil
	}
	key, err := a.privateKey()
	if err != nil {
		return "", err
	}
	header, err := json.Marshal(apnsJWTHeader{Alg: "ES256", Kid: a.KeyID})
	if err != nil {
		return "", err
	}
	claims, err := json.Marshal(apnsJWTClaims{Iss: a.TeamID, Iat: time.Now().Unix()})
	if err != nil {
		return "", err
	}
	var b bytes.Buffer
	b.WriteString(base64.RawURLEncoding.EncodeToString(header))
	b.WriteByte('.')
	b.WriteString(base64.RawURLEncoding.EncodeToString(claims))
	digest := sha256.Sum256(b.Bytes())
	r, s, err := ecdsa.Sign(rand.Reader, key, digest[:])
	if err != nil {
		return "", err
	}
	// JOSE ES256 签名 = r||s 各按曲线字节数左填充（这里恒 P-256 → 32 字节）。
	sig := make([]byte, 64)
	r.FillBytes(sig[:32])
	s.FillBytes(sig[32:])
	b.WriteByte('.')
	b.WriteString(base64.RawURLEncoding.EncodeToString(sig))
	a.authToken = b.String()
	a.authExp = time.Now().Add(55 * time.Minute)
	return a.authToken, nil
}

// privateKey 解析并缓存 .p8 私钥（PKCS#8 ECDSA）。Key 支持内联 PEM 或文件路径。
func (a *APNs) privateKey() (*ecdsa.PrivateKey, error) {
	if a.key != nil || a.keyErr != nil {
		return a.key, a.keyErr
	}
	a.key, a.keyErr = parseAPNsKey(a.Key)
	return a.key, a.keyErr
}

func parseAPNsKey(raw string) (*ecdsa.PrivateKey, error) {
	if raw == "" {
		return nil, errors.New("apns: 未配置 .p8 私钥")
	}
	pemData := []byte(raw)
	if !bytes.Contains(pemData, []byte("-----BEGIN")) {
		// 不含 PEM 头：按文件路径读取（凭据通常落盘管理）。
		data, err := os.ReadFile(raw)
		if err != nil {
			return nil, fmt.Errorf("apns: 读取私钥文件失败: %w", err)
		}
		pemData = data
	}
	block, _ := pem.Decode(pemData)
	if block == nil {
		return nil, errors.New("apns: .p8 私钥不是合法 PEM")
	}
	parsed, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("apns: .p8 私钥解析失败: %w", err)
	}
	key, ok := parsed.(*ecdsa.PrivateKey)
	if !ok {
		return nil, errors.New("apns: .p8 私钥不是 ECDSA 私钥")
	}
	return key, nil
}

func (a *APNs) client() *http.Client {
	if a.Client != nil {
		return a.Client
	}
	return &http.Client{Timeout: 10 * time.Second}
}
