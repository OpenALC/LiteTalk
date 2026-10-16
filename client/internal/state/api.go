// Package state 是客户端业务状态桥：把 M1/M2 的 audio / rtc / signal 管线
// 装配成面向 UI 的会话（登录 → 连信令 → 入房 → 通话），并经 Wails Events
// 推送给前端（落地方案 §4.6 事件表）。
package state

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

// AuthTokens 登录/刷新返回的令牌对（§5.4 auth 契约）。
type AuthTokens struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	ExpiresIn    int64  `json:"expires_in"`
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

// AuthResult 登录/注册成功后的载荷。
type AuthResult struct {
	AccessToken  string      `json:"access_token"`
	RefreshToken string      `json:"refresh_token"`
	User         UserProfile `json:"user"`
}

// APIClient REST 客户端（§5.4），带 access token 注入与 401 刷新重试一次。
type APIClient struct {
	base   string
	http   *http.Client
	access string
}

// NewAPIClient 创建 REST 客户端。base 形如 https://host（不含 /api/v1）。
func NewAPIClient(base string) *APIClient {
	return &APIClient{
		base: base,
		http: &http.Client{Timeout: 15 * time.Second},
	}
}

// SetToken 注入/更新 access token（登录、刷新后调用）。
func (c *APIClient) SetToken(token string) { c.access = token }

// Token 返回当前 access token。
func (c *APIClient) Token() string { return c.access }

// post JSON POST，auth=true 时带 Bearer。返回状态码与响应体。
func (c *APIClient) post(path string, body any, auth bool) (int, []byte, error) {
	raw, err := json.Marshal(body)
	if err != nil {
		return 0, nil, fmt.Errorf("marshal %s: %w", path, err)
	}
	req, err := http.NewRequest(http.MethodPost, c.base+"/api/v1"+path, bytes.NewReader(raw))
	if err != nil {
		return 0, nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	if auth && c.access != "" {
		req.Header.Set("Authorization", "Bearer "+c.access)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return 0, nil, fmt.Errorf("post %s: %w", path, err)
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, data, nil
}

// get JSON GET（带 Bearer）。
func (c *APIClient) get(path string) (int, []byte, error) {
	req, err := http.NewRequest(http.MethodGet, c.base+"/api/v1"+path, nil)
	if err != nil {
		return 0, nil, err
	}
	if c.access != "" {
		req.Header.Set("Authorization", "Bearer "+c.access)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return 0, nil, fmt.Errorf("get %s: %w", path, err)
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, data, nil
}

// patch JSON PATCH（带 Bearer，管理端点：服务器/频道重命名等）。
func (c *APIClient) patch(path string, body any) (int, []byte, error) {
	raw, err := json.Marshal(body)
	if err != nil {
		return 0, nil, fmt.Errorf("marshal %s: %w", path, err)
	}
	req, err := http.NewRequest(http.MethodPatch, c.base+"/api/v1"+path, bytes.NewReader(raw))
	if err != nil {
		return 0, nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	if c.access != "" {
		req.Header.Set("Authorization", "Bearer "+c.access)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return 0, nil, fmt.Errorf("patch %s: %w", path, err)
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, data, nil
}

// del JSON DELETE（带 Bearer，管理端点：删除服务器/频道等）。
func (c *APIClient) del(path string) (int, []byte, error) {
	req, err := http.NewRequest(http.MethodDelete, c.base+"/api/v1"+path, nil)
	if err != nil {
		return 0, nil, err
	}
	if c.access != "" {
		req.Header.Set("Authorization", "Bearer "+c.access)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return 0, nil, fmt.Errorf("delete %s: %w", path, err)
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, data, nil
}

// APIError 服务端错误响应 {code,message}。
type APIError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

func (e *APIError) Error() string { return fmt.Sprintf("%s: %s", e.Code, e.Message) }

// decodeError 从响应体解析错误。
func decodeError(status int, data []byte) error {
	var e APIError
	if json.Unmarshal(data, &e) == nil && e.Message != "" {
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
	c.access = r.AccessToken
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
	c.access = r.AccessToken
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
	c.access = r.AccessToken
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
	c.access = t.AccessToken
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
	c.access = ""
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
