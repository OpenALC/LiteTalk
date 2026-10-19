// Package rtcapi 提供 TURN 短期凭证的签发原语（签发算法、有效期、URL 回填）。
// 对接 coturn use-auth-secret（TURN REST API）：
//
//	username   = <expiry_unix>:<uid>
//	credential = base64(HMAC-SHA1(key=TURN_SECRET, msg=username))
//
// 默认 12h 有效。HTTP 端点 POST /api/v1/rtc/credentials 已收编进 internal/api（#77），
// 本包只保留与传输层无关的签发逻辑。
package rtcapi

import (
	"crypto/hmac"
	"crypto/sha1"
	"encoding/base64"
	"fmt"
	"time"
)

// DefaultTTL 凭证默认有效期（§5.4：12h）。
const DefaultTTL = 12 * time.Hour

// Config 凭证签发配置。
type Config struct {
	// Secret coturn 的 static-auth-secret（env LITETALK_TURN_SECRET）；空 = 未配置，接口返回 503
	Secret string
	// TurnURLs turn/turns URL 列表（env LITETALK_TURN_URLS，逗号分隔）
	TurnURLs []string
	// StunURLs stun/stuns URL 列表（env LITETALK_STUN_URLS，逗号分隔）
	StunURLs []string
	// TTL 凭证有效期；<=0 取 DefaultTTL
	TTL time.Duration
	// Now 测试注入时钟；nil 取 time.Now
	Now func() time.Time
}

// SignCredential 计算 coturn REST 凭证：base64(HMAC-SHA1(key=secret, msg=username))。
func SignCredential(secret, username string) string {
	mac := hmac.New(sha1.New, []byte(secret))
	mac.Write([]byte(username))
	return base64.StdEncoding.EncodeToString(mac.Sum(nil))
}

// DefaultURLs 内置 TURN 启用时的 STUN/TURN URL 回填（§0.4）：显式 env 优先，
// 未设时按 host + 端口生成。TURN 监听 UDP 与 TCP 同号端口，禁 UDP 网络走
// transport=tcp 条目。host 取 --domain，否则 LITETALK_SFU_IP。
func DefaultURLs(host string, turnPort int) (turnURLs, stunURLs []string) {
	if host == "" || turnPort <= 0 {
		return nil, nil
	}
	base := fmt.Sprintf("%s:%d", host, turnPort)
	stunURLs = []string{"stun:" + base}
	turnURLs = []string{
		"turn:" + base + "?transport=udp",
		"turn:" + base + "?transport=tcp",
	}
	return turnURLs, stunURLs
}

// Issue 为 uid 生成一对短期凭证：username=<expiry_unix>:<uid>。
func (c Config) Issue(uid string) (username, credential string, expiry time.Time) {
	now := time.Now()
	if c.Now != nil {
		now = c.Now()
	}
	ttl := c.TTL
	if ttl <= 0 {
		ttl = DefaultTTL
	}
	expiry = now.Add(ttl).UTC()
	username = fmt.Sprintf("%d:%s", expiry.Unix(), uid)
	return username, SignCredential(c.Secret, username), expiry
}

// UserFromToken 从访问令牌提取用户 ID（main 注入：JWT 或 dev 令牌）。
type UserFromToken func(token string) (userID string, err error)
