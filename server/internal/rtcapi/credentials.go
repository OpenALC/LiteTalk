// Package rtcapi 提供 RTC 相关 REST：TURN 短期凭证签发（docs/落地方案.md §5.4/§5.5）。
// 对接 coturn use-auth-secret（TURN REST API）：
//
//	username   = <expiry_unix>:<uid>
//	credential = base64(HMAC-SHA1(key=TURN_SECRET, msg=username))
//
// 默认 12h 有效。M2 鉴权沿用开发令牌 dev-<username>，M3 换 JWT。
package rtcapi

import (
	"crypto/hmac"
	"crypto/sha1"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strings"
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

// ICEServer 单条 ICE server。字段名对齐 WebRTC RTCIceServer
// （urls / username / credential），客户端可直接填入 webrtc.Configuration。
type ICEServer struct {
	URLs       []string `json:"urls"`
	Username   string   `json:"username,omitempty"`
	Credential string   `json:"credential,omitempty"`
}

// CredentialsResponse POST /api/v1/rtc/credentials 响应体。
type CredentialsResponse struct {
	ICEServers []ICEServer `json:"iceServers"`
	ExpiresAt  int64       `json:"expiresAt"` // 凭证过期时刻（unix 秒）
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

// UserFromToken 从访问令牌提取用户 ID（M2: dev-<username>；M3 换 JWT 时替换实现）。
type UserFromToken func(token string) (userID string, err error)

// Handler POST /api/v1/rtc/credentials。
type Handler struct {
	cfg  Config
	auth UserFromToken
	log  *log.Logger
}

// NewHandler 构造凭证签发 handler。logger 为 nil 时丢弃日志。
func NewHandler(cfg Config, auth UserFromToken, logger *log.Logger) *Handler {
	if logger == nil {
		logger = log.Default()
	}
	return &Handler{cfg: cfg, auth: auth, log: logger}
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	uid, err := h.auth(tokenFromRequest(r))
	if err != nil || uid == "" {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	if h.cfg.Secret == "" {
		// 未配置 LITETALK_TURN_SECRET：明确报错，避免上线后静默降级
		http.Error(w, "turn not configured (LITETALK_TURN_SECRET)", http.StatusServiceUnavailable)
		return
	}

	username, credential, expiry := h.cfg.Issue(uid)
	resp := CredentialsResponse{ICEServers: []ICEServer{}, ExpiresAt: expiry.Unix()}
	if len(h.cfg.StunURLs) > 0 {
		resp.ICEServers = append(resp.ICEServers, ICEServer{URLs: h.cfg.StunURLs})
	}
	if len(h.cfg.TurnURLs) > 0 {
		resp.ICEServers = append(resp.ICEServers, ICEServer{
			URLs:       h.cfg.TurnURLs,
			Username:   username,
			Credential: credential,
		})
	}

	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	if err := json.NewEncoder(w).Encode(resp); err != nil {
		h.log.Printf("rtcapi: 编码响应失败: %v", err)
	}
}

// tokenFromRequest 提取访问令牌：优先 Authorization: Bearer，其次 ?token=（与 WS 一致）。
func tokenFromRequest(r *http.Request) string {
	if h := r.Header.Get("Authorization"); strings.HasPrefix(h, "Bearer ") {
		return strings.TrimPrefix(h, "Bearer ")
	}
	return r.URL.Query().Get("token")
}
