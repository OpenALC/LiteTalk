// Package state 是客户端业务状态桥：把 M1/M2 的 audio / rtc / signal 管线
// 装配成面向 UI 的会话（登录 → 连信令 → 入房 → 通话），并经 Wails Events
// 推送给前端（落地方案 §4.6 事件表）。
package state

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sync"
	"time"
)

// ErrSessionReplaced 会话已被其他设备顶替/重放（401 + X-LiteTalk-Auth:
// session_replaced/session_replayed，契约 §0.3/§0.4）。
// 致命：refresh 令牌同族已失效，刷新也救不回——不触发刷新重试，直接返回；
// 上层应断会话、清令牌、回登录页。
var ErrSessionReplaced = errors.New("会话已被其他设备替换")

// authHeaderSession 服务端 401 细分会话类失败的响应头（契约 §0.4）。
const authHeaderSession = "X-LiteTalk-Auth"

// AuthTokens 登录/刷新返回的令牌对（§5.4 auth 契约）。
// User 仅服务端响应携带时非空（/auth/refresh 会带回 user，可直接用，
// 省去一次 Me() 往返——游客续期路径依赖它避免身份丢失）。
type AuthTokens struct {
	AccessToken  string       `json:"access_token"`
	RefreshToken string       `json:"refresh_token"`
	ExpiresIn    int64        `json:"expires_in"`
	User         *UserProfile `json:"user,omitempty"`
}

// UserProfile 用户档案（无头像——硬性约束）。
// id 与服务端 store.User 一致为数字（曾误声明 string，导致登录/注册/游客
// 响应解码全部失败——服务端 wire 是 int64）。
// Status：服务端 users.status（在线状态），好友列表按它归组 online/offline 用。
type UserProfile struct {
	ID       int64  `json:"id"`
	Nickname string `json:"nickname"`
	Tag      string `json:"tag"`
	Email    string `json:"email"`
	IsGuest  bool   `json:"is_guest"`
	Status   int    `json:"status"` // 0=离线 1=在线 2=离开 3=忙碌（与服务端 users.status 对齐）
}

// AuthResult 登录/注册成功后的载荷。SessionOK 报告信令会话是否已连上：
// false 时 app 层在后台带退避重试，前端据它显示「信令连接中」提示条。
type AuthResult struct {
	AccessToken  string      `json:"access_token"`
	RefreshToken string      `json:"refresh_token"`
	User         UserProfile `json:"user"`
	SessionOK    bool        `json:"session_ok"`
}

// RefreshSource 提供当前 refresh_token（登录/注册/游客路径注入 app 层；
// 返回空串表示无可用令牌，401 不再尝试刷新）。
type RefreshSource func(ctx context.Context) (string, error)

// APIClient REST 客户端（§5.4），带 access token 注入与 401 刷新重试一次。
type APIClient struct {
	// baseMu 保护 base：SetServerConfig 热切换时替换，在飞请求自然结束。
	baseMu sync.RWMutex
	base   string
	http   *http.Client

	// tokenMu 保护 access：SetToken/Token 与每个请求的读并发（CI -race）。
	tokenMu sync.RWMutex
	access  string

	// refreshSource/onRefreshed 由 app 层注入：刷新令牌存 app（需区分
	// 持久化策略），旋转后的新令牌对回传 app 落盘。
	refreshSource RefreshSource
	onRefreshed   func(AuthTokens)

	// refreshFlight 合并并发刷新（singleflight 语义，不引 x/sync：
	// 互斥 + 共享结果通道——同一时刻只跑一次 /auth/refresh，其余请求等待）。
	refreshMu     sync.Mutex
	refreshFlight *refreshCall
}

// refreshCall 一次进行中的刷新及其结果（done 关闭前 tokens/err 已写好）。
type refreshCall struct {
	done   chan struct{}
	tokens *AuthTokens
	err    error
}

// NewAPIClient 创建 REST 客户端。base 形如 https://host（不含 /api/v1）。
func NewAPIClient(base string) *APIClient {
	return &APIClient{
		base: base,
		http: &http.Client{Timeout: 15 * time.Second},
	}
}

// SetRefreshSource 注入 refresh_token 来源（401 刷新重试的前置条件）。
func (c *APIClient) SetRefreshSource(src RefreshSource) { c.refreshSource = src }

// SetOnRefreshed 注入刷新成功回调（旋转后的新令牌对；app 层持久化用）。
func (c *APIClient) SetOnRefreshed(fn func(AuthTokens)) { c.onRefreshed = fn }

// SetToken 注入/更新 access token（登录、刷新后调用）。
func (c *APIClient) SetToken(token string) {
	c.tokenMu.Lock()
	defer c.tokenMu.Unlock()
	c.access = token
}

// Token 返回当前 access token。
func (c *APIClient) Token() string {
	c.tokenMu.RLock()
	defer c.tokenMu.RUnlock()
	return c.access
}

// Base 返回 REST 基址（会话侧据此判环回候选，见 IsLoopbackURL）。
func (c *APIClient) Base() string {
	c.baseMu.RLock()
	defer c.baseMu.RUnlock()
	return c.base
}

// SetBase 热切换 REST 基址。在飞请求可能仍走旧地址（自然结束），
// 后续请求走新地址。配合 SetServerConfig 的地址热切换使用。
func (c *APIClient) SetBase(base string) {
	c.baseMu.Lock()
	defer c.baseMu.Unlock()
	c.base = base
}

// do 执行一次请求；401（token_expired/unauthorized）且带鉴权时先合并刷新
// 再用新令牌原样重放一次（retried 防递归）；仍 401 才返回错误。
// 401 且会话被顶替/重放（X-LiteTalk-Auth: session_replaced/session_replayed）
// 则返回 ErrSessionReplaced，不走刷新。
func (c *APIClient) do(req *http.Request, auth bool) (int, []byte, error) {
	return c.doOnce(req, auth, false)
}

func (c *APIClient) doOnce(req *http.Request, auth, retried bool) (int, []byte, error) {
	if auth {
		if tok := c.Token(); tok != "" {
			req.Header.Set("Authorization", "Bearer "+tok)
		}
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	if resp.StatusCode == http.StatusUnauthorized && auth {
		// 会话被其他设备顶替/重放：致命，直接上抛，不进刷新重试。
		if isSessionReplaced(resp.Header.Get(authHeaderSession), data) {
			return resp.StatusCode, data, ErrSessionReplaced
		}
		if !retried && c.refreshSource != nil {
			var e APIError
			// 服务端对过期/无效令牌统一返回 unauthorized（router.go），
			// token_expired 是文档约定码——两者都尝试刷新，刷新失败即原样报错。
			if json.Unmarshal(data, &e) == nil && (e.Code == "token_expired" || e.Code == "unauthorized") {
				if _, rerr := c.refreshOnce(req.Context()); rerr == nil {
					retry := req.Clone(req.Context())
					// GET/DELETE 无 body 时 GetBody 为 nil（http.NewRequest 只为
					// bytes.Reader/Buffer 填充），Clone 复用原 Body 即可重放。
					if req.GetBody != nil {
						body, berr := req.GetBody()
						if berr != nil {
							return resp.StatusCode, data, nil
						}
						retry.Body = body
					}
					return c.doOnce(retry, auth, true)
				} else if errors.Is(rerr, ErrSessionReplaced) {
					// 并发窗口里会话已被顶替（refresh 撞上 session_replaced），
					// 同样按致命上抛。
					return resp.StatusCode, data, ErrSessionReplaced
				}
			}
		}
	}
	return resp.StatusCode, data, nil
}

// isSessionReplaced 判定 401 属会话顶替/重放：头值优先，body code 兜底
// （契约 §0.4 头与 body code 同值，任一命中即可）。
func isSessionReplaced(header string, data []byte) bool {
	if header == "session_replaced" || header == "session_replayed" {
		return true
	}
	var e APIError
	return json.Unmarshal(data, &e) == nil &&
		(e.Code == "session_replaced" || e.Code == "session_replayed")
}

// refreshOnce 并发安全的单次刷新：已有刷新在进行则等待其结果，
// 否则自己发起。http.Client 自带 15s 超时，等待方以请求 ctx 兜底。
func (c *APIClient) refreshOnce(ctx context.Context) (*AuthTokens, error) {
	c.refreshMu.Lock()
	if f := c.refreshFlight; f != nil {
		c.refreshMu.Unlock()
		select {
		case <-f.done:
			return f.tokens, f.err
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	f := &refreshCall{done: make(chan struct{})}
	c.refreshFlight = f
	c.refreshMu.Unlock()

	f.tokens, f.err = c.doRefresh(ctx)
	close(f.done) // 先写结果再关通道：close 提供 happens-before，等待方读到完整结果
	c.refreshMu.Lock()
	c.refreshFlight = nil
	c.refreshMu.Unlock()
	return f.tokens, f.err
}

// doRefresh 真正发 /auth/refresh（auth=false 走 post，不会触发重试递归）。
// 成功后先换内存令牌并回调 app 落盘，再返回给等待方重放原请求。
func (c *APIClient) doRefresh(ctx context.Context) (*AuthTokens, error) {
	rt, err := c.refreshSource(ctx)
	if err != nil {
		return nil, err
	}
	if rt == "" {
		return nil, fmt.Errorf("no refresh token")
	}
	status, data, err := c.post("/auth/refresh", map[string]string{
		"refresh_token": rt,
	}, false)
	if err != nil {
		return nil, err
	}
	if status != http.StatusOK {
		// refresh 自身 401 不重试（防递归）；会话被顶替/重放按致命上抛，
		// 让在飞请求也能拿到 ErrSessionReplaced 而非普通 401。
		if isSessionReplaced("", data) {
			return nil, ErrSessionReplaced
		}
		return nil, decodeError(status, data) // refresh 自身 401 不重试（防递归）
	}
	var t AuthTokens
	if err := json.Unmarshal(data, &t); err != nil {
		return nil, fmt.Errorf("decode refresh: %w", err)
	}
	c.SetToken(t.AccessToken)
	if c.onRefreshed != nil {
		c.onRefreshed(t)
	}
	return &t, nil
}

// post JSON POST，auth=true 时带 Bearer。返回状态码与响应体。
func (c *APIClient) post(path string, body any, auth bool) (int, []byte, error) {
	raw, err := json.Marshal(body)
	if err != nil {
		return 0, nil, fmt.Errorf("marshal %s: %w", path, err)
	}
	req, err := http.NewRequest(http.MethodPost, c.Base()+"/api/v1"+path, bytes.NewReader(raw))
	if err != nil {
		return 0, nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	status, data, err := c.do(req, auth)
	if err != nil {
		return 0, nil, fmt.Errorf("post %s: %w", path, err)
	}
	return status, data, nil
}

// get JSON GET（带 Bearer）。
func (c *APIClient) get(path string) (int, []byte, error) {
	req, err := http.NewRequest(http.MethodGet, c.Base()+"/api/v1"+path, nil)
	if err != nil {
		return 0, nil, err
	}
	status, data, err := c.do(req, true)
	if err != nil {
		return 0, nil, fmt.Errorf("get %s: %w", path, err)
	}
	return status, data, nil
}

// patch JSON PATCH（带 Bearer，管理端点：服务器/频道重命名等）。
func (c *APIClient) patch(path string, body any) (int, []byte, error) {
	raw, err := json.Marshal(body)
	if err != nil {
		return 0, nil, fmt.Errorf("marshal %s: %w", path, err)
	}
	req, err := http.NewRequest(http.MethodPatch, c.Base()+"/api/v1"+path, bytes.NewReader(raw))
	if err != nil {
		return 0, nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	status, data, err := c.do(req, true)
	if err != nil {
		return 0, nil, fmt.Errorf("patch %s: %w", path, err)
	}
	return status, data, nil
}

// del JSON DELETE（带 Bearer，管理端点：删除服务器/频道等）。
func (c *APIClient) del(path string) (int, []byte, error) {
	req, err := http.NewRequest(http.MethodDelete, c.Base()+"/api/v1"+path, nil)
	if err != nil {
		return 0, nil, err
	}
	status, data, err := c.do(req, true)
	if err != nil {
		return 0, nil, fmt.Errorf("delete %s: %w", path, err)
	}
	return status, data, nil
}

// APIError 服务端错误响应 {code,message}。Status 为对应 HTTP 状态码，
// 不入 JSON——调用方据它区分「明确 401（清令牌）」与「网络错误（保留重试）」。
type APIError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
	Status  int    `json:"-"`
}

func (e *APIError) Error() string { return fmt.Sprintf("%s: %s", e.Code, e.Message) }

// decodeError 从响应体解析错误。
func decodeError(status int, data []byte) error {
	var e APIError
	if json.Unmarshal(data, &e) == nil && e.Message != "" {
		e.Status = status
		return &e
	}
	return fmt.Errorf("http %d: %s", status, string(data))
}

// ServerConfig 服务端能力开关（GET /api/v1/config，免鉴权）。
type ServerConfig struct {
	AuthMode       string `json:"auth_mode"`       // full | simple
	MailConfigured bool   `json:"mail_configured"` // 发信是否为真实投递
}

// GetServerConfig 获取服务端 Auth 模式与发信配置（客户端适配 UI 用）。
func (c *APIClient) GetServerConfig() (*ServerConfig, error) {
	status, data, err := c.get("/config")
	if err != nil {
		return nil, err
	}
	if status != http.StatusOK {
		return nil, decodeError(status, data)
	}
	var cfg ServerConfig
	if err := json.Unmarshal(data, &cfg); err != nil {
		return nil, err
	}
	return &cfg, nil
}

// SendEmailCode 发验证码（purpose: register / reset，60s 间隔由服务端限流）。
func (c *APIClient) SendEmailCode(email, purpose string) error {
	status, data, err := c.post("/auth/email-code", map[string]string{
		"email":   email,
		"purpose": purpose,
	}, false)
	if err != nil {
		return err
	}
	if status != http.StatusOK {
		return decodeError(status, data)
	}
	return nil
}

// Register 注册（昵称+邮箱+密码+验证码）。
func (c *APIClient) Register(nickname, email, password, code string) (*AuthResult, error) {
	status, data, err := c.post("/auth/register", map[string]string{
		"nickname": nickname,
		"email":    email,
		"password": password,
		"code":     code,
	}, false)
	if err != nil {
		return nil, err
	}
	// 服务端注册成功返回 201 Created（历史实现客户端只认 200，注册必失败——一并修复）。
	if status != http.StatusOK && status != http.StatusCreated {
		return nil, decodeError(status, data)
	}
	var r AuthResult
	if err := json.Unmarshal(data, &r); err != nil {
		return nil, fmt.Errorf("decode register: %w", err)
	}
	c.SetToken(r.AccessToken)
	return &r, nil
}

// Guest 游客一键进入（仅 simple 模式）。成功返回 201。
func (c *APIClient) Guest() (*AuthResult, error) {
	status, data, err := c.post("/auth/guest", nil, false)
	if err != nil {
		return nil, err
	}
	if status != http.StatusOK && status != http.StatusCreated {
		return nil, decodeError(status, data)
	}
	var r AuthResult
	if err := json.Unmarshal(data, &r); err != nil {
		return nil, fmt.Errorf("decode guest: %w", err)
	}
	c.SetToken(r.AccessToken)
	return &r, nil
}

// Login 邮箱+密码登录。
func (c *APIClient) Login(email, password string) (*AuthResult, error) {
	status, data, err := c.post("/auth/login", map[string]string{
		"email":    email,
		"password": password,
	}, false)
	if err != nil {
		return nil, err
	}
	if status != http.StatusOK {
		return nil, decodeError(status, data)
	}
	var r AuthResult
	if err := json.Unmarshal(data, &r); err != nil {
		return nil, fmt.Errorf("decode login: %w", err)
	}
	c.SetToken(r.AccessToken)
	return &r, nil
}

// ResetPassword 邮箱+验证码+新密码。
func (c *APIClient) ResetPassword(email, code, newPassword string) error {
	status, data, err := c.post("/auth/password/reset", map[string]string{
		"email":    email,
		"code":     code,
		"password": newPassword,
	}, false)
	if err != nil {
		return err
	}
	if status != http.StatusOK {
		return decodeError(status, data)
	}
	return nil
}

// Refresh 旋转刷新令牌。
func (c *APIClient) Refresh(refreshToken string) (*AuthTokens, error) {
	status, data, err := c.post("/auth/refresh", map[string]string{
		"refresh_token": refreshToken,
	}, false)
	if err != nil {
		return nil, err
	}
	if status != http.StatusOK {
		return nil, decodeError(status, data)
	}
	var t AuthTokens
	if err := json.Unmarshal(data, &t); err != nil {
		return nil, fmt.Errorf("decode refresh: %w", err)
	}
	c.SetToken(t.AccessToken)
	return &t, nil
}

// Logout 吊销 refresh。
func (c *APIClient) Logout(refreshToken string) error {
	status, data, err := c.post("/auth/logout", map[string]string{
		"refresh_token": refreshToken,
	}, true)
	if err != nil {
		return err
	}
	if status != http.StatusOK {
		return decodeError(status, data)
	}
	c.SetToken("")
	return nil
}

// Me 拉取当前用户档案。
func (c *APIClient) Me() (*UserProfile, error) {
	status, data, err := c.get("/users/me")
	if err != nil {
		return nil, err
	}
	if status != http.StatusOK {
		return nil, decodeError(status, data)
	}
	var u UserProfile
	if err := json.Unmarshal(data, &u); err != nil {
		return nil, fmt.Errorf("decode me: %w", err)
	}
	return &u, nil
}

// ICEServer WebRTC ICE 服务器描述（与 pion webrtc.ICEServer 字段兼容）。
type ICEServer struct {
	URLs       []string `json:"urls"`
	Username   string   `json:"username,omitempty"`
	Credential string   `json:"credential,omitempty"`
}

// ICEServersResponse POST /rtc/credentials 响应。
type ICEServersResponse struct {
	ICEServers []ICEServer `json:"iceServers"`
	ExpiresAt  int64       `json:"expiresAt"`
}

// RTCCredentials 获取 STUN/TURN 凭证。
func (c *APIClient) RTCCredentials() (*ICEServersResponse, error) {
	status, data, err := c.post("/rtc/credentials", map[string]string{}, true)
	if err != nil {
		return nil, err
	}
	if status != http.StatusOK {
		return nil, decodeError(status, data)
	}
	var r ICEServersResponse
	if err := json.Unmarshal(data, &r); err != nil {
		return nil, fmt.Errorf("decode credentials: %w", err)
	}
	return &r, nil
}

// ---------- 服务器 / 频道 / 好友（REST CRUD，服务端已有端点） ----------

// ServerDTO 服务器（与 store.Server wire 对齐；图标为 Lucide 预设名）。
type ServerDTO struct {
	ID        int64  `json:"id"`
	Name      string `json:"name"`
	OwnerID   int64  `json:"owner_id"`
	IconName  string `json:"icon_name"`
	IconColor string `json:"icon_color"`
}

// ChannelDTO 频道（与 store.Channel wire 对齐）。
type ChannelDTO struct {
	ID       int64  `json:"id"`
	ServerID int64  `json:"server_id"`
	Name     string `json:"name"`
	Type     string `json:"type"` // text | voice
	// CategoryID 分类归属（0 = 无分类/未分类，映射自服务端 NULL）；Position 同服务器序。
	CategoryID *int64 `json:"category_id"`
	Position   int    `json:"position"`
}

// ServerChannels GET /servers/{id}/channels 响应（categories + channels 两数组）。
type ServerChannels struct {
	Categories []CategoryDTO `json:"categories"`
	Channels   []ChannelDTO  `json:"channels"`
}

// ListChannels 列出服务器的分类与频道（成员可读）。重启后频道列表恢复用。
func (c *APIClient) ListChannels(serverID int64) (*ServerChannels, error) {
	status, data, err := c.get(fmt.Sprintf("/servers/%d/channels", serverID))
	if err != nil {
		return nil, err
	}
	if status != http.StatusOK {
		return nil, decodeError(status, data)
	}
	var out ServerChannels
	if err := json.Unmarshal(data, &out); err != nil {
		return nil, fmt.Errorf("decode server channels: %w", err)
	}
	return &out, nil
}

// ListServers 列出当前用户加入的服务器。
func (c *APIClient) ListServers() ([]ServerDTO, error) {
	status, data, err := c.get("/servers")
	if err != nil {
		return nil, err
	}
	if status != http.StatusOK {
		return nil, decodeError(status, data)
	}
	var list []ServerDTO
	if err := json.Unmarshal(data, &list); err != nil {
		return nil, fmt.Errorf("decode servers: %w", err)
	}
	return list, nil
}

// CreateServer 创建服务器（图标走服务端默认；创建者为 owner）。成功返回 201。
func (c *APIClient) CreateServer(name string) (*ServerDTO, error) {
	status, data, err := c.post("/servers", map[string]string{"name": name}, true)
	if err != nil {
		return nil, err
	}
	if status != http.StatusOK && status != http.StatusCreated {
		return nil, decodeError(status, data)
	}
	var s ServerDTO
	if err := json.Unmarshal(data, &s); err != nil {
		return nil, fmt.Errorf("decode server: %w", err)
	}
	return &s, nil
}

// JoinServer 凭邀请码加入服务器（已是成员时服务端原样返回服务器）。
func (c *APIClient) JoinServer(code string) (*ServerDTO, error) {
	status, data, err := c.post("/servers/join", map[string]string{"code": code}, true)
	if err != nil {
		return nil, err
	}
	if status != http.StatusOK {
		return nil, decodeError(status, data)
	}
	var s ServerDTO
	if err := json.Unmarshal(data, &s); err != nil {
		return nil, fmt.Errorf("decode joined server: %w", err)
	}
	return &s, nil
}

// RenameServer 重命名服务器（owner/admin）。
func (c *APIClient) RenameServer(id int64, name string) error {
	status, data, err := c.patch(fmt.Sprintf("/servers/%d", id), map[string]string{"name": name})
	if err != nil {
		return err
	}
	if status != http.StatusOK {
		return decodeError(status, data)
	}
	return nil
}

// DeleteServer 删除服务器（仅 owner）。
func (c *APIClient) DeleteServer(id int64) error {
	status, data, err := c.del(fmt.Sprintf("/servers/%d", id))
	if err != nil {
		return err
	}
	if status != http.StatusOK {
		return decodeError(status, data)
	}
	return nil
}

// CreateInvite 为服务器生成邀请码（admin 及以上）。返回 code 字符串。
func (c *APIClient) CreateInvite(serverID int64) (string, error) {
	status, data, err := c.post(fmt.Sprintf("/servers/%d/invites", serverID),
		map[string]int{}, true)
	if err != nil {
		return "", err
	}
	if status != http.StatusOK && status != http.StatusCreated {
		return "", decodeError(status, data)
	}
	var inv struct {
		Code string `json:"code"`
	}
	if err := json.Unmarshal(data, &inv); err != nil {
		return "", fmt.Errorf("decode invite: %w", err)
	}
	return inv.Code, nil
}

// CreateChannel 在服务器下新建频道（owner/admin）。typ 为 text / voice。成功返回 201。
func (c *APIClient) CreateChannel(serverID int64, name, typ string) (*ChannelDTO, error) {
	status, data, err := c.post(fmt.Sprintf("/servers/%d/channels", serverID),
		map[string]string{"name": name, "type": typ}, true)
	if err != nil {
		return nil, err
	}
	if status != http.StatusOK && status != http.StatusCreated {
		return nil, decodeError(status, data)
	}
	var ch ChannelDTO
	if err := json.Unmarshal(data, &ch); err != nil {
		return nil, fmt.Errorf("decode channel: %w", err)
	}
	return &ch, nil
}

// RenameChannel 重命名频道（owner/admin）。
func (c *APIClient) RenameChannel(id int64, name string) error {
	status, data, err := c.patch(fmt.Sprintf("/channels/%d", id), map[string]string{"name": name})
	if err != nil {
		return err
	}
	if status != http.StatusOK {
		return decodeError(status, data)
	}
	return nil
}

// DeleteChannel 删除频道（owner/admin）。
func (c *APIClient) DeleteChannel(id int64) error {
	status, data, err := c.del(fmt.Sprintf("/channels/%d", id))
	if err != nil {
		return err
	}
	if status != http.StatusOK {
		return decodeError(status, data)
	}
	return nil
}

// CategoryDTO 频道分类（与服务端 store.Category 对齐）。
type CategoryDTO struct {
	ID       int64  `json:"id"`
	ServerID int64  `json:"server_id"`
	Name     string `json:"name"`
	Position int    `json:"position"`
}

// CreateCategory 创建频道分类（owner/admin；1–32 字）。
func (c *APIClient) CreateCategory(serverID int64, name string) (*CategoryDTO, error) {
	status, data, err := c.post(fmt.Sprintf("/servers/%d/categories", serverID),
		map[string]string{"name": name}, true)
	if err != nil {
		return nil, err
	}
	if status != http.StatusOK && status != http.StatusCreated {
		return nil, decodeError(status, data)
	}
	var cat CategoryDTO
	if err := json.Unmarshal(data, &cat); err != nil {
		return nil, fmt.Errorf("decode category: %w", err)
	}
	return &cat, nil
}

// DeleteCategory 删除分类；withChannels=true 时级联删除其下全部频道
// （服务端单事务；分类非空且未带参会 409 category_not_empty，由 UI 二次确认后传 true）。
func (c *APIClient) DeleteCategory(id int64, withChannels bool) error {
	path := fmt.Sprintf("/categories/%d", id)
	if withChannels {
		path += "?with_channels=1"
	}
	status, data, err := c.del(path)
	if err != nil {
		return err
	}
	if status != http.StatusOK {
		return decodeError(status, data)
	}
	return nil
}

// RenameCategory 重命名分类（owner/admin；1–32 字）。
func (c *APIClient) RenameCategory(id int64, name string) error {
	_, err := c.UpdateCategory(id, &name, nil)
	return err
}

// UpdateCategory 改分类名/位置（owner/admin；至少给一个字段）。position 为目标序
// （0-based），服务端把该分类移到此位置并重排同服务器其它分类为 0..n-1。
func (c *APIClient) UpdateCategory(id int64, name *string, position *int) (*CategoryDTO, error) {
	body := map[string]any{}
	if name != nil {
		body["name"] = *name
	}
	if position != nil {
		body["position"] = *position
	}
	status, data, err := c.patch(fmt.Sprintf("/categories/%d", id), body)
	if err != nil {
		return nil, err
	}
	if status != http.StatusOK {
		return nil, decodeError(status, data)
	}
	var cat CategoryDTO
	if err := json.Unmarshal(data, &cat); err != nil {
		return nil, fmt.Errorf("decode category: %w", err)
	}
	return &cat, nil
}

// SendFriendRequest 发送好友请求（target 形如 "昵称#1234"，服务端负责拆分）。
func (c *APIClient) SendFriendRequest(target string) error {
	status, data, err := c.post("/friends/requests", map[string]string{"nickname": target}, true)
	if err != nil {
		return err
	}
	if status != http.StatusOK && status != http.StatusCreated {
		return decodeError(status, data)
	}
	return nil
}

// FriendDTO 好友条目（与服务端 friendView 对齐；Remark/Group 仅发起方视角非空）。
type FriendDTO struct {
	ID       int64       `json:"id"`
	Status   string      `json:"status"` // pending | accepted | blocked
	Incoming bool        `json:"incoming"`
	Remark   string      `json:"remark"`
	Group    string      `json:"group"`
	User     UserProfile `json:"user"`
}

// ListFriends 好友关系全量（含 pending；v0.1.2 替换前端 mock）。
func (c *APIClient) ListFriends() ([]FriendDTO, error) {
	status, data, err := c.get("/friends")
	if err != nil {
		return nil, err
	}
	if status != http.StatusOK {
		return nil, decodeError(status, data)
	}
	var list []FriendDTO
	if err := json.Unmarshal(data, &list); err != nil {
		return nil, fmt.Errorf("decode friends: %w", err)
	}
	return list, nil
}

// AcceptFriend 接受好友请求（仅被请求方可调）。
func (c *APIClient) AcceptFriend(id int64) error {
	status, data, err := c.post(fmt.Sprintf("/friends/requests/%d/accept", id), nil, true)
	if err != nil {
		return err
	}
	if status != http.StatusOK {
		return decodeError(status, data)
	}
	return nil
}

// RemoveFriend 删除好友关系（双向皆可）。
func (c *APIClient) RemoveFriend(id int64) error {
	status, data, err := c.del(fmt.Sprintf("/friends/%d", id))
	if err != nil {
		return err
	}
	if status != http.StatusOK {
		return decodeError(status, data)
	}
	return nil
}

// UpdateFriend 设置备注/分组（仅发起方；空串=清除备注/移出分组）。
// 调用方持有全量状态，两个字段总是整体提交。
func (c *APIClient) UpdateFriend(id int64, remark, group string) error {
	body := map[string]string{"remark": remark, "group": group}
	status, data, err := c.patch(fmt.Sprintf("/friends/%d", id), body)
	if err != nil {
		return err
	}
	if status != http.StatusOK {
		return decodeError(status, data)
	}
	return nil
}
