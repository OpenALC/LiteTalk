// Package api 是客户端 REST 客户端：鉴权与令牌刷新、各资源端点的请求封装
// （#31 wire：ID 字符串化）。与 UI 无关，供 core/desktop 两侧复用。
package api

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

// errBaseChanged 请求在飞期间 REST 基址被热切换：刷新结果属于旧基址，
// 必须丢弃——绝不能把旧服务器的新令牌装回客户端、落盘或重放到新地址（#17）。
var errBaseChanged = errors.New("服务器地址已切换，刷新结果已丢弃")

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
// id 为 wire 字符串 ID（#31）：Snowflake 超出 JS Number 精度，JSON 一律字符串；
// 解码双接受数字（老服务端）。
// 字段与 api/openapi.yaml 的 User/PublicUser 对齐（#28）；Online 为桥接层
// 补充字段（REST User 无，PublicUser 有）。
// Status：服务端 users.status（在线状态），好友列表按它归组 online/offline 用。
type UserProfile struct {
	ID            ID     `json:"id"`
	Nickname      string `json:"nickname"`
	Tag           string `json:"tag"`
	Email         string `json:"email"`
	EmailVerified bool   `json:"email_verified"`
	IsGuest       bool   `json:"is_guest"`
	Status        int    `json:"status"` // 0=离线 1=在线 2=离开 3=忙碌（与服务端 users.status 对齐）
	// Online 好友在线态（#11，服务端 presence:{uid}）：好友列表的 user.online；
	// /me 等自响应恒 false（服务端不填）。
	Online    bool      `json:"online"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
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
// base 是发起刷新的请求所用的基址快照：不同基址的等待方不得复用其结果。
type refreshCall struct {
	base   string
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

// SetBaseAndClearToken 原子切换 REST 基址并清空 access token（跨 origin 换服专用）。
// 写锁会等待正在进行的 doRefresh「复查+安装+onRefreshed」读锁段结束：刷新若在
// 切换前完成，调用返回后 token 已被清掉；若在切换后复查，则直接丢弃。分两次
// 调用 SetToken/SetBase 会留下「新基址 + 旧 access token」窗口（#17）。
// 锁序 baseMu → tokenMu，与 doRefresh 的 baseMu.RLock → SetToken 一致。
func (c *APIClient) SetBaseAndClearToken(base string) {
	c.baseMu.Lock()
	defer c.baseMu.Unlock()
	c.base = base
	c.SetToken("")
}

// do 执行一次请求；401（token_expired/unauthorized）且带鉴权时先合并刷新
// 再用新令牌原样重放一次（retried 防递归）；仍 401 才返回错误。
// base 是请求创建时的基址快照（见 get/post 等）：刷新必须钉在它上面，
// 不能用可能已被 SetBase 换掉的 c.Base()（#17 换服泄露）。
// 401 且会话被顶替/重放（X-LiteTalk-Auth: session_replaced/session_replayed）
// 则返回 ErrSessionReplaced，不走刷新。
func (c *APIClient) do(req *http.Request, auth bool, base string) (int, []byte, error) {
	return c.doOnce(req, auth, false, base)
}

func (c *APIClient) doOnce(req *http.Request, auth, retried bool, base string) (int, []byte, error) {
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
				if _, rerr := c.refreshOnce(req.Context(), base); rerr == nil {
					// 刷新返回时基址可能已被热切换：旧基址的新令牌不能重放到
					// 新地址（#17），丢弃并上抛。
					if c.Base() != base {
						return resp.StatusCode, data, errBaseChanged
					}
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
					return c.doOnce(retry, auth, true, base)
				} else if errors.Is(rerr, ErrSessionReplaced) {
					// 并发窗口里会话已被顶替（refresh 撞上 session_replaced），
					// 同样按致命上抛。
					return resp.StatusCode, data, ErrSessionReplaced
				} else if errors.Is(rerr, errBaseChanged) {
					// 刷新期间基址已变：不重放，把明确原因交给上层。
					return resp.StatusCode, data, errBaseChanged
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
// base 为发起请求的基址快照；不同基址的刷新结果不能互相复用——
// 否则换服后新地址的请求会拿着旧服务器的令牌去重放（#17）。
func (c *APIClient) refreshOnce(ctx context.Context, base string) (*AuthTokens, error) {
	c.refreshMu.Lock()
	if f := c.refreshFlight; f != nil {
		c.refreshMu.Unlock()
		if f.base != base {
			return nil, errBaseChanged
		}
		select {
		case <-f.done:
			return f.tokens, f.err
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	f := &refreshCall{base: base, done: make(chan struct{})}
	c.refreshFlight = f
	c.refreshMu.Unlock()

	f.tokens, f.err = c.doRefresh(ctx, base)
	close(f.done) // 先写结果再关通道：close 提供 happens-before，等待方读到完整结果
	c.refreshMu.Lock()
	c.refreshFlight = nil
	c.refreshMu.Unlock()
	return f.tokens, f.err
}

// doRefresh 真正发 /auth/refresh（auth=false 走 post，不会触发重试递归）。
// base 是触发刷新的那次请求的基址快照：POST 必须发到它上面，不能跟随
// c.Base()（SetBase 可能已把客户端切到新服务器，发过去就是凭据泄露， #17）。
// 刷新成功后先复查基址；已切换则丢弃结果，绝不安装/回调/落盘。
func (c *APIClient) doRefresh(ctx context.Context, base string) (*AuthTokens, error) {
	rt, err := c.refreshSource(ctx)
	if err != nil {
		return nil, err
	}
	if rt == "" {
		return nil, fmt.Errorf("no refresh token")
	}
	status, data, err := c.postAt(ctx, base, "/auth/refresh", map[string]string{
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
	// 复查基址、安装令牌与回调必须在 baseMu 读锁内完成：SetBase 是写锁，
	// 两者不会交错。否则「检查通过 → SetBase → 再安装」会把旧服务器的新令牌
	// 留在新基址上（#17）。注意这里不能调 c.Base()（重复 RLock 会与等待中的
	// 写锁互相卡住），直接读 c.base。
	c.baseMu.RLock()
	if c.base != base {
		c.baseMu.RUnlock()
		return nil, errBaseChanged
	}
	c.SetToken(t.AccessToken)
	if c.onRefreshed != nil {
		c.onRefreshed(t)
	}
	c.baseMu.RUnlock()
	return &t, nil
}

// post JSON POST，auth=true 时带 Bearer。返回状态码与响应体。
func (c *APIClient) post(path string, body any, auth bool) (int, []byte, error) {
	return c.postCtx(context.Background(), path, body, auth)
}

// postCtx 同 post，但带请求上下文（可设短超时，见 RTCCredentials）。
func (c *APIClient) postCtx(ctx context.Context, path string, body any, auth bool) (int, []byte, error) {
	return c.postAt(ctx, c.Base(), path, body, auth)
}

// postAt 用指定基址构造 POST。基址在构造时取快照并一路传到 do/doRefresh，
// 刷新才不会被 SetBase 带到新服务器上。
func (c *APIClient) postAt(ctx context.Context, base, path string, body any, auth bool) (int, []byte, error) {
	raw, err := json.Marshal(body)
	if err != nil {
		return 0, nil, fmt.Errorf("marshal %s: %w", path, err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, base+"/api/v1"+path, bytes.NewReader(raw))
	if err != nil {
		return 0, nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	status, data, err := c.do(req, auth, base)
	if err != nil {
		return 0, nil, fmt.Errorf("post %s: %w", path, err)
	}
	return status, data, nil
}

// get JSON GET（带 Bearer）。
func (c *APIClient) get(path string) (int, []byte, error) {
	base := c.Base()
	req, err := http.NewRequest(http.MethodGet, base+"/api/v1"+path, nil)
	if err != nil {
		return 0, nil, err
	}
	status, data, err := c.do(req, true, base)
	if err != nil {
		return 0, nil, fmt.Errorf("get %s: %w", path, err)
	}
	return status, data, nil
}

// patch JSON PATCH（带 Bearer，管理端点：服务器/频道重命名等）。
func (c *APIClient) patch(path string, body any) (int, []byte, error) {
	base := c.Base()
	raw, err := json.Marshal(body)
	if err != nil {
		return 0, nil, fmt.Errorf("marshal %s: %w", path, err)
	}
	req, err := http.NewRequest(http.MethodPatch, base+"/api/v1"+path, bytes.NewReader(raw))
	if err != nil {
		return 0, nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	status, data, err := c.do(req, true, base)
	if err != nil {
		return 0, nil, fmt.Errorf("patch %s: %w", path, err)
	}
	return status, data, nil
}

// del JSON DELETE（带 Bearer，管理端点：删除服务器/频道等）。
func (c *APIClient) del(path string) (int, []byte, error) {
	base := c.Base()
	req, err := http.NewRequest(http.MethodDelete, base+"/api/v1"+path, nil)
	if err != nil {
		return 0, nil, err
	}
	status, data, err := c.do(req, true, base)
	if err != nil {
		return 0, nil, fmt.Errorf("delete %s: %w", path, err)
	}
	return status, data, nil
}

// APIError 服务端错误响应（#31 RFC 9457 problem+json）：{type,title,status,code,detail}。
// code 为机器码（扩展字段，与旧版一致）；Message 取 detail（旧 message 字段兼容读）。
// Status 为对应 HTTP 状态码，不入 JSON——调用方据它区分「明确 401（清令牌）」与
// 「网络错误（保留重试）」。
type APIError struct {
	Code    string `json:"code"`
	Message string `json:"detail"`
	Status  int    `json:"-"`
}

func (e *APIError) Error() string { return fmt.Sprintf("%s: %s", e.Code, e.Message) }

// UnmarshalJSON 兼容 problem+json 的 detail 与旧版 message 两种提示字段。
func (e *APIError) UnmarshalJSON(b []byte) error {
	var raw struct {
		Code   string `json:"code"`
		Detail string `json:"detail"`
		Legacy string `json:"message"`
	}
	if err := json.Unmarshal(b, &raw); err != nil {
		return err
	}
	e.Code = raw.Code
	e.Message = raw.Detail
	if e.Message == "" {
		e.Message = raw.Legacy
	}
	return nil
}

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
	AuthMode          string `json:"auth_mode"`          // full | simple
	MailConfigured    bool   `json:"mail_configured"`    // 发信是否为真实投递
	SelfHost          bool   `json:"self_host"`          // 一键自托管形态（#26/#27）
	BootstrapRequired bool   `json:"bootstrap_required"` // 管理员邀请码未消费（#27）
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

// Register 注册（昵称+邮箱+密码+验证码+可选管理员邀请码 bootstrap，#27）。
// bootstrap 空 = 不使用邀请码（字段仍上送，服务端视空为缺省）。
func (c *APIClient) Register(nickname, email, password, code, bootstrap string) (*AuthResult, error) {
	status, data, err := c.post("/auth/register", map[string]string{
		"nickname":  nickname,
		"email":     email,
		"password":  password,
		"code":      code,
		"bootstrap": bootstrap,
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

// RTCredentialsTimeout /rtc/credentials 短超时：该请求可能落在进房链路上，
// 用全局 15s 会把 WS 读循环停摆到撞心跳预算（#13）。失败由调用方降级空 ICE。
// 导出供会话侧测试临时调小以覆盖超时降级路径。
var RTCredentialsTimeout = 3 * time.Second

// RTCCredentials 获取 STUN/TURN 凭证。
func (c *APIClient) RTCCredentials() (*ICEServersResponse, error) {
	ctx, cancel := context.WithTimeout(context.Background(), RTCredentialsTimeout)
	defer cancel()
	status, data, err := c.postCtx(ctx, "/rtc/credentials", map[string]string{}, true)
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

// ServerDTO 服务器（与 store.Server / 契约 ServerInfo wire 对齐；图标为 Lucide 预设名）。
// ID 为字符串 wire（#31）。
type ServerDTO struct {
	ID        ID        `json:"id"`
	Name      string    `json:"name"`
	OwnerID   ID        `json:"owner_id"`
	IconName  string    `json:"icon_name"`
	IconColor string    `json:"icon_color"`
	CreatedAt time.Time `json:"created_at"`
}

// ChannelDTO 频道（与 store.Channel / 契约 ChannelInfo wire 对齐）。ID 为字符串 wire（#31）。
type ChannelDTO struct {
	ID       ID     `json:"id"`
	ServerID ID     `json:"server_id"`
	Name     string `json:"name"`
	Type     string `json:"type"` // text | voice
	// CategoryID 分类归属（0/空 = 无分类/未分类，映射自服务端 NULL）；Position 同服务器序。
	CategoryID *ID       `json:"category_id"`
	Position   int       `json:"position"`
	CreatedAt  time.Time `json:"created_at"`
}

// ServerChannels GET /servers/{id}/channels 响应（categories + channels 两数组）。
type ServerChannels struct {
	Categories []CategoryDTO `json:"categories"`
	Channels   []ChannelDTO  `json:"channels"`
}

// ListChannels 列出服务器的分类与频道（成员可读）。重启后频道列表恢复用。
func (c *APIClient) ListChannels(serverID string) (*ServerChannels, error) {
	status, data, err := c.get(fmt.Sprintf("/servers/%s/channels", serverID))
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

// RenameServer 重命名服务器（owner/admin）。id 为十进制 ID 串（#31）。
func (c *APIClient) RenameServer(id string, name string) error {
	status, data, err := c.patch(fmt.Sprintf("/servers/%s", id), map[string]string{"name": name})
	if err != nil {
		return err
	}
	if status != http.StatusOK {
		return decodeError(status, data)
	}
	return nil
}

// DeleteServer 删除服务器（仅 owner）。
func (c *APIClient) DeleteServer(id string) error {
	status, data, err := c.del(fmt.Sprintf("/servers/%s", id))
	if err != nil {
		return err
	}
	if status != http.StatusOK {
		return decodeError(status, data)
	}
	return nil
}

// CreateInvite 为服务器生成邀请码（admin 及以上）。返回 code 字符串。
func (c *APIClient) CreateInvite(serverID string) (string, error) {
	status, data, err := c.post(fmt.Sprintf("/servers/%s/invites", serverID),
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

// CreateChannel 在服务器下新建频道（owner/admin）。typ 为 text / voice。
// categoryID 为目标分类（空/「0」= 不归类，服务端落 NULL）——带上它频道才在会话内
// 就归位，不用等重启后 ListChannels 对账（#72）。成功返回 201。
func (c *APIClient) CreateChannel(serverID string, name, typ string, categoryID string) (*ChannelDTO, error) {
	body := map[string]any{"name": name, "type": typ}
	if categoryID != "" {
		body["category_id"] = categoryID
	}
	status, data, err := c.post(fmt.Sprintf("/servers/%s/channels", serverID),
		body, true)
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
func (c *APIClient) RenameChannel(id string, name string) error {
	status, data, err := c.patch(fmt.Sprintf("/channels/%s", id), map[string]string{"name": name})
	if err != nil {
		return err
	}
	if status != http.StatusOK {
		return decodeError(status, data)
	}
	return nil
}

// DeleteChannel 删除频道（owner/admin）。
func (c *APIClient) DeleteChannel(id string) error {
	status, data, err := c.del(fmt.Sprintf("/channels/%s", id))
	if err != nil {
		return err
	}
	if status != http.StatusOK {
		return decodeError(status, data)
	}
	return nil
}

// CategoryDTO 频道分类（与 store.Category / 契约 CategoryInfo 对齐）。ID 为字符串 wire（#31）。
type CategoryDTO struct {
	ID        ID        `json:"id"`
	ServerID  ID        `json:"server_id"`
	Name      string    `json:"name"`
	Position  int       `json:"position"`
	CreatedAt time.Time `json:"created_at"`
}

// CreateCategory 创建频道分类（owner/admin；1–32 字）。
func (c *APIClient) CreateCategory(serverID string, name string) (*CategoryDTO, error) {
	status, data, err := c.post(fmt.Sprintf("/servers/%s/categories", serverID),
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
func (c *APIClient) DeleteCategory(id string, withChannels bool) error {
	path := fmt.Sprintf("/categories/%s", id)
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
func (c *APIClient) RenameCategory(id string, name string) error {
	_, err := c.UpdateCategory(id, &name, nil)
	return err
}

// UpdateCategory 改分类名/位置（owner/admin；至少给一个字段）。position 为目标序
// （0-based），服务端把该分类移到此位置并重排同服务器其它分类为 0..n-1。
func (c *APIClient) UpdateCategory(id string, name *string, position *int) (*CategoryDTO, error) {
	body := map[string]any{}
	if name != nil {
		body["name"] = *name
	}
	if position != nil {
		body["position"] = *position
	}
	status, data, err := c.patch(fmt.Sprintf("/categories/%s", id), body)
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
// ID 为字符串 wire（#31）。
type FriendDTO struct {
	ID       ID          `json:"id"`
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
func (c *APIClient) AcceptFriend(id string) error {
	status, data, err := c.post(fmt.Sprintf("/friends/requests/%s/accept", id), nil, true)
	if err != nil {
		return err
	}
	if status != http.StatusOK {
		return decodeError(status, data)
	}
	return nil
}

// RemoveFriend 删除好友关系（双向皆可）。
func (c *APIClient) RemoveFriend(id string) error {
	status, data, err := c.del(fmt.Sprintf("/friends/%s", id))
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
func (c *APIClient) UpdateFriend(id string, remark, group string) error {
	body := map[string]string{"remark": remark, "group": group}
	status, data, err := c.patch(fmt.Sprintf("/friends/%s", id), body)
	if err != nil {
		return err
	}
	if status != http.StatusOK {
		return decodeError(status, data)
	}
	return nil
}

// ---------- 频道消息（#32 全量补拉） ----------

// flexID 兼容数字与字符串两种 JSON ID 形态：#31 把 ID 字符串化前后的响应都能解。
// 输出一律字符串（调用方按 string 消费，不再关心 wire 形态）。
type flexID string

func (f *flexID) UnmarshalJSON(b []byte) error {
	var s string
	if err := json.Unmarshal(b, &s); err == nil {
		*f = flexID(s)
		return nil
	}
	var n json.Number
	if err := json.Unmarshal(b, &n); err != nil {
		return fmt.Errorf("id 需为数字或字符串: %s", b)
	}
	*f = flexID(n.String())
	return nil
}

// String 归一后的字符串形态。
func (f flexID) String() string { return string(f) }

// MessageDTO 频道消息（GET /channels/{id}/messages，与 store.Message 的 wire 对齐）。
// ID 用 flexID 吸收 #31 的字符串化；CreatedAt 解析 RFC3339（含 Nano）。
type MessageDTO struct {
	ID             flexID    `json:"id"`
	ChannelID      flexID    `json:"channel_id"`
	UserID         flexID    `json:"user_id"`
	Content        string    `json:"content"`
	CreatedAt      time.Time `json:"created_at"`
	SenderNickname string    `json:"sender_nickname,omitempty"`
	SenderTag      string    `json:"sender_tag,omitempty"`
}

// ListMessages 拉频道最近消息（旧→新，limit 上限 50）。不带游标只取最新——
// 断线续传失败后的全量补拉就是这个用法；游标翻页是 REST 列表页的事。
func (c *APIClient) ListMessages(channelID int64, limit int) ([]MessageDTO, error) {
	if limit <= 0 || limit > 50 {
		limit = 50
	}
	status, data, err := c.get(fmt.Sprintf("/channels/%d/messages?limit=%d", channelID, limit))
	if err != nil {
		return nil, err
	}
	if status != http.StatusOK {
		return nil, decodeError(status, data)
	}
	var list []MessageDTO
	if err := json.Unmarshal(data, &list); err != nil {
		return nil, fmt.Errorf("decode messages: %w", err)
	}
	return list, nil
}
