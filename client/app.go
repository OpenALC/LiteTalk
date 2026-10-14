package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

	"github.com/OpenALC/LiteTalk/client/internal/audio"
	"github.com/OpenALC/LiteTalk/client/internal/config"
	"github.com/OpenALC/LiteTalk/client/internal/state"
	"github.com/OpenALC/LiteTalk/client/internal/tray"
	"github.com/gen2brain/malgo"
	"github.com/wailsapp/wails/v2/pkg/runtime"
)

// version 客户端版本号，可用 -ldflags "-X main.version=" 注入，默认 dev
var version = "dev"

// App 应用结构体：窗口上下文、设置项、业务会话与 API 客户端。
// 导出方法经 Wails Bind 暴露给前端调用（落地方案 §4.6）。
type App struct {
	ctx context.Context

	// cfg 本地配置（config.json：服务器地址 / 休眠开关持久化）。
	cfg *config.Manager

	// hibernateOnMinimize 设置项「最小化时休眠界面（省内存）」，默认关闭。
	hibernateOnMinimize bool

	// forceQuit 托盘「退出」或「退出程序」置位后 OnBeforeClose 放行真正关闭。
	forceQuit atomic.Bool

	// trayReady 托盘图标+菜单就绪标志（tray onReady 置位）。
	// 托盘不可用时关窗必须放行真正关闭，绝不能隐藏窗口把用户锁死（问题 4a）。
	trayReady atomic.Bool

	api  *state.APIClient
	sess *state.Session // 登录并连接信令后创建

	mu         sync.Mutex
	refreshTok string
	user       state.UserProfile

	audioCtx *audio.Context // 设备枚举用，惰性创建
}

// NewApp 创建应用实例
func NewApp() *App {
	return &App{cfg: config.New()}
}

// newAppWithConfig 测试用：注入指定路径的配置管理器。
func newAppWithConfig(m *config.Manager) *App {
	return &App{cfg: m}
}

// initLogging 把全局 log 输出重定向到文件：os.UserConfigDir()/LiteTalk/app.log
// （与 config.json 同目录）。GUI 子系统无控制台，stdout/stderr 不可见，排障
// 全依赖此文件；超过 5MB 启动时截断。文件排 MultiWriter 首位——无控制台时
// stderr 写入会失败并短路 MultiWriter，放前面会吞掉文件写入。
func initLogging() {
	dir, err := os.UserConfigDir()
	if err != nil {
		return
	}
	logDir := filepath.Join(dir, "LiteTalk")
	_ = os.MkdirAll(logDir, 0o755)
	f, err := os.OpenFile(filepath.Join(logDir, "app.log"),
		os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return
	}
	if fi, err := f.Stat(); err == nil && fi.Size() > 5<<20 {
		_ = f.Truncate(0)
		_, _ = f.Seek(0, 0)
	}
	log.SetOutput(io.MultiWriter(f, os.Stderr))
	log.SetFlags(log.LstdFlags | log.Lmicroseconds)
}

// LogFromWeb 前端 console/异常转发入口（前端 main.ts 拦截 console.*、
// window.onerror、unhandledrejection 后调用），与后端日志同落 app.log。
func (a *App) LogFromWeb(level, msg string) {
	const maxLen = 2000
	if len(msg) > maxLen {
		msg = msg[:maxLen] + "…(截断)"
	}
	log.Printf("[web:%s] %s", level, msg)
}

// startup 启动回调：保存上下文、创建 API 客户端、启动系统托盘。
// 服务器地址优先级：环境变量 LITETALK_API > config.json > 默认本机（运维/调试最高）。
func (a *App) startup(ctx context.Context) {
	initLogging()
	log.Printf("=== LiteTalk %s 启动 (pid=%d) ===", version, os.Getpid())
	a.ctx = ctx
	r := a.cfg.Resolve()
	base := r.BaseURL
	a.api = state.NewAPIClient(base)

	// 恢复持久化的休眠开关。
	if h := a.cfg.Get().HibernateOnMinimize; h != nil {
		a.hibernateOnMinimize = *h
	}

	// 托盘独占 goroutine（systray.Run 需要自己的消息循环）。
	go tray.Run(tray.Callbacks{
		OnReady: func() {
			// 图标与菜单建好才视为托盘可用（问题 4a/4c）。
			a.trayReady.Store(true)
		},
		OnShow: func() {
			runtime.WindowShow(a.ctx)
			runtime.WindowUnminimise(a.ctx)
			runtime.EventsEmit(a.ctx, "sys:wake")
		},
		OnToggleMute: func() bool {
			a.mu.Lock()
			s := a.sess
			a.mu.Unlock()
			if s == nil {
				return false
			}
			muted := !s.Muted()
			s.SetMute(muted)
			return muted
		},
		OnQuit: func() {
			a.forceQuit.Store(true)
			runtime.Quit(a.ctx)
		},
		IsMuted: func() bool {
			a.mu.Lock()
			s := a.sess
			a.mu.Unlock()
			return s != nil && s.Muted()
		},
	})
}

// emitHibernate 若开启「最小化时休眠」则通知前端进入休眠态。
func (a *App) emitHibernate() {
	if a.hibernateOnMinimize {
		runtime.EventsEmit(a.ctx, "sys:hibernate")
	}
}

// onBeforeClose 拦截窗口关闭：默认隐藏到托盘而非退出（问题 4 兜底链）。
//   - forceQuit（托盘「退出」/设置页「退出程序」）→ 放行真正关闭
//   - 托盘未就绪（图标不可用）→ 放行真正关闭，绝不隐藏窗口把用户锁死
//   - 用户选「关闭窗口时=退出程序」→ 放行真正关闭
//   - 其余 → 隐藏到托盘
func (a *App) onBeforeClose(ctx context.Context) bool {
	if a.forceQuit.Load() {
		return false // 真正退出
	}
	if !a.trayReady.Load() {
		// 托盘不可用：隐藏=无任何恢复路径，直接退出。
		return false
	}
	if !a.cfg.CloseToTray() {
		return false // 用户设置：关闭即退出
	}
	runtime.WindowHide(ctx)
	a.emitHibernate()
	return true
}

// QuitApp 显式退出程序（设置页「退出程序」按钮，不依赖托盘）。
func (a *App) QuitApp() {
	a.forceQuit.Store(true)
	runtime.Quit(a.ctx)
}

// TrayReady 返回托盘是否就绪（前端可在关闭前预判行为）。
func (a *App) TrayReady() bool { return a.trayReady.Load() }

// GetCloseToTray 读取「关闭窗口时」设置（true=最小化到托盘，false=退出程序）。
func (a *App) GetCloseToTray() bool { return a.cfg.CloseToTray() }

// SetCloseToTray 持久化「关闭窗口时」设置。
func (a *App) SetCloseToTray(v bool) {
	_ = a.cfg.SetCloseToTray(v)
}

// ---------- 记住密码（问题 6） ----------

// SavedCredentialsView 前端可见的凭据视图（Password 仅已记住时非空）。
type SavedCredentialsView struct {
	Email    string `json:"email"`
	Password string `json:"password"`
	Saved    bool   `json:"saved"`
}

// GetSavedCredentials 读取记住的登录凭据（密码经 DPAPI 解密）。
func (a *App) GetSavedCredentials() SavedCredentialsView {
	c := a.cfg.GetSavedCredentials()
	return SavedCredentialsView{Email: c.Email, Password: c.Password, Saved: c.Saved}
}

// SetSavedCredentials 保存登录凭据（邮箱明文 + 密码 DPAPI 加密）。
func (a *App) SetSavedCredentials(email, password string) {
	_ = a.cfg.SetSavedCredentials(email, password)
}

// ClearSavedCredentials 清除记住的凭据。
func (a *App) ClearSavedCredentials() {
	_ = a.cfg.ClearSavedCredentials()
}

// ---------- 窗口控制（自定义标题栏调用） ----------

// WindowMinimize 最小化窗口（设置开启时同步休眠界面）。
func (a *App) WindowMinimize() {
	runtime.WindowMinimise(a.ctx)
	a.emitHibernate()
}

// WindowMaximize 最大化/还原。
func (a *App) WindowMaximize() { runtime.WindowToggleMaximise(a.ctx) }

// WindowClose 关闭（转隐藏到托盘；设置开启时同步休眠界面）。
func (a *App) WindowClose() {
	runtime.WindowHide(a.ctx)
	a.emitHibernate()
}

// ---------- 设置 ----------

// SetHibernateOnMinimize 设置「最小化时休眠界面」开关（持久化到 config.json）。
// 前端监听 sys:hibernate / sys:wake 事件切换休眠态（见 stores/hibernate.ts）。
func (a *App) SetHibernateOnMinimize(enabled bool) {
	a.hibernateOnMinimize = enabled
	_ = a.cfg.Update(func(c *config.Config) error {
		c.HibernateOnMinimize = &enabled
		return nil
	})
}

// GetHibernateOnMinimize 读取「最小化时休眠界面」开关
func (a *App) GetHibernateOnMinimize() bool { return a.hibernateOnMinimize }

// GetVersion 返回客户端版本号
func (a *App) GetVersion() string { return version }

// ---------- 服务器地址配置（设置页「连接」分组） ----------

// ServerConfigView 当前生效的服务器地址与来源。
type ServerConfigView struct {
	BaseURL string `json:"base_url"`
	WSURL   string `json:"ws_url"`
	Source  string `json:"source"` // env / file / default
}

// GetServerConfig 返回实际生效的服务器地址与来源（env 优先于 config.json）。
func (a *App) GetServerConfig() *ServerConfigView {
	r := a.cfg.Resolve()
	return &ServerConfigView{BaseURL: r.BaseURL, WSURL: r.WSURL, Source: string(r.Source)}
}

// SetServerConfig 保存服务器地址（WS 自动推导：https→wss、http→ws，末尾补 /ws）。
// 重启后生效——运行中热切换信令/REST 连接成本高，不做即时切换。
func (a *App) SetServerConfig(baseURL string) error {
	if err := config.ValidateBase(baseURL); err != nil {
		return err
	}
	return a.cfg.Update(func(c *config.Config) error {
		c.ServerBase = baseURL
		return nil
	})
}

// ConnTestResult 连通性测试结果。
type ConnTestResult struct {
	OK       bool   `json:"ok"`
	Latency  int64  `json:"latency_ms"`
	Version  string `json:"version"`
	ErrorMsg string `json:"error"`
}

// TestServerConnection 测试服务器连通性：GET <base>/api/v1/healthz（3s 超时）。
// 用于设置页「测试连接」，可对未保存的地址直接测试。
func (a *App) TestServerConnection(baseURL string) *ConnTestResult {
	if err := config.ValidateBase(baseURL); err != nil {
		return &ConnTestResult{OK: false, ErrorMsg: err.Error()}
	}
	client := &http.Client{Timeout: 3 * time.Second}
	start := time.Now()
	resp, err := client.Get(baseURL + "/api/v1/healthz")
	if err != nil {
		return &ConnTestResult{OK: false, ErrorMsg: "连接失败"}
	}
	defer resp.Body.Close()
	latency := time.Since(start).Milliseconds()
	if resp.StatusCode != http.StatusOK {
		return &ConnTestResult{OK: false, Latency: latency, ErrorMsg: fmt.Sprintf("服务端返回 %d", resp.StatusCode)}
	}
	var body struct {
		Status  string `json:"status"`
		Version string `json:"version"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return &ConnTestResult{OK: true, Latency: latency} // 可达但响应格式异常：仍算连通
	}
	return &ConnTestResult{OK: true, Latency: latency, Version: body.Version}
}

// ---------- 认证（转发 REST，§5.4） ----------

// GetAuthConfig 获取服务端 Auth 模式与发信配置（前端适配 UI 用）。
func (a *App) GetAuthConfig() (*state.ServerConfig, error) {
	return a.api.GetServerConfig()
}

// SendEmailCode 发送验证码（purpose: register / reset）。
func (a *App) SendEmailCode(email, purpose string) error {
	return a.api.SendEmailCode(email, purpose)
}

// Login 邮箱密码登录。成功后连接信令。
func (a *App) Login(email, password string) (*state.AuthResult, error) {
	r, err := a.api.Login(email, password)
	if err != nil {
		return nil, err
	}
	a.mu.Lock()
	a.refreshTok = r.RefreshToken
	a.user = r.User
	a.mu.Unlock()
	if err := a.connectSession(r.AccessToken, r.User.Nickname); err != nil {
		log.Printf("connect session: %v", err) // 登录成功但信令暂不可达：UI 可重试
	}
	return r, nil
}

// Register 注册并自动登录。
func (a *App) Register(nickname, email, password, code string) (*state.AuthResult, error) {
	r, err := a.api.Register(nickname, email, password, code)
	if err != nil {
		return nil, err
	}
	a.mu.Lock()
	a.refreshTok = r.RefreshToken
	a.user = r.User
	a.mu.Unlock()
	_ = a.connectSession(r.AccessToken, r.User.Nickname)
	return r, nil
}

// LoginAsGuest 游客一键进入（simple 模式）。优先复用记住的游客身份：
// 已存 refresh_token 先走 /auth/refresh 续期（rotation 写回持久化），
// 失败/过期再新建游客。游客身份跨启动保持同一账号，避免账号堆积。
func (a *App) LoginAsGuest() (*state.AuthResult, error) {
	if saved := a.cfg.GetGuestRefresh(); saved != "" {
		if t, err := a.api.Refresh(saved); err == nil {
			if u, merr := a.api.Me(); merr == nil {
				_ = a.cfg.SetGuestRefresh(t.RefreshToken) // 轮换写回（guest 无密码兜底，丢了就变新号）
				a.mu.Lock()
				a.refreshTok = t.RefreshToken
				a.user = *u
				a.mu.Unlock()
				_ = a.connectSession(t.AccessToken, u.Nickname)
				return &state.AuthResult{
					AccessToken: t.AccessToken, RefreshToken: t.RefreshToken, User: *u,
				}, nil
			}
		}
		// 已存令牌失效（过期/被吊销/服务端迁移）：清掉走新建。
		_ = a.cfg.SetGuestRefresh("")
	}

	r, err := a.api.Guest()
	if err != nil {
		return nil, err
	}
	_ = a.cfg.SetGuestRefresh(r.RefreshToken)
	a.mu.Lock()
	a.refreshTok = r.RefreshToken
	a.user = r.User
	a.mu.Unlock()
	_ = a.connectSession(r.AccessToken, r.User.Nickname)
	return r, nil
}

// ResetPassword 邮箱验证码重置密码。
func (a *App) ResetPassword(email, code, newPassword string) error {
	return a.api.ResetPassword(email, code, newPassword)
}

// Logout 注销：断信令、吊销 refresh。
func (a *App) Logout() error {
	a.mu.Lock()
	rt := a.refreshTok
	a.refreshTok = ""
	a.mu.Unlock()
	a.disconnectSession()
	if rt != "" {
		return a.api.Logout(rt)
	}
	return nil
}

// GetMe 当前用户档案。
func (a *App) GetMe() (*state.UserProfile, error) {
	a.mu.Lock()
	u := a.user
	a.mu.Unlock()
	if u.ID != 0 {
		return &u, nil
	}
	return a.api.Me()
}

// ---------- 音频设备（设置页） ----------

// AudioDevice 音频设备描述。
type AudioDevice struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	IsDefault bool   `json:"is_default"`
}

// ListAudioDevices 枚举采集/播放设备。
func (a *App) ListAudioDevices() (map[string][]AudioDevice, error) {
	a.mu.Lock()
	if a.audioCtx == nil {
		ctx, err := audio.NewContext(nil)
		if err != nil {
			a.mu.Unlock()
			return nil, err
		}
		a.audioCtx = ctx
	}
	ctx := a.audioCtx
	a.mu.Unlock()

	caps, err := ctx.Devices(malgo.Capture)
	if err != nil {
		return nil, err
	}
	plays, err := ctx.Devices(malgo.Playback)
	if err != nil {
		return nil, err
	}
	toDTO := func(list []audio.DeviceInfo) []AudioDevice {
		out := make([]AudioDevice, len(list))
		for i, d := range list {
			out[i] = AudioDevice{ID: d.ID, Name: d.Name, IsDefault: d.IsDefault}
		}
		return out
	}
	return map[string][]AudioDevice{
		"capture":  toDTO(caps),
		"playback": toDTO(plays),
	}, nil
}

// ---------- 语音会话 ----------

// connectSession 建立信令会话并把事件接到 Wails Events。
func (a *App) connectSession(token, nickname string) error {
	a.disconnectSession()
	ev := state.Events{
		OnPeerJoin: func(userID, nickname string, speaking, mute, deafen bool) {
			runtime.EventsEmit(a.ctx, "voice:peer-join", map[string]any{
				"user_id": userID, "nickname": nickname,
				"speaking": speaking, "mute": mute, "deafen": deafen,
			})
		},
		OnPeerLeave: func(userID string) {
			runtime.EventsEmit(a.ctx, "voice:peer-leave", userID)
		},
		OnPeerSpeaking: func(userID string, speaking, mute, deafen bool) {
			runtime.EventsEmit(a.ctx, "voice:speaking", map[string]any{
				"user_id": userID, "speaking": speaking, "mute": mute, "deafen": deafen,
			})
		},
		OnConnection: func(st string) {
			runtime.EventsEmit(a.ctx, "rtc:connection", st)
		},
		OnSelf: func(userID string) {
			runtime.EventsEmit(a.ctx, "voice:self", userID)
		},
	}
	server := a.cfg.Resolve().WSURL
	sess := state.NewSession(a.api, token, nickname, ev, log.Default())
	if err := sess.Connect(server); err != nil {
		return err
	}
	// 连接即应用持久化的语音设置（码率/VBR/DTX/FEC/丢包率/降噪/增益/VAD 灵敏度）。
	if err := sess.ApplyAudio(a.runtimeAudio()); err != nil {
		log.Printf("apply audio settings: %v", err)
	}
	// 连接即应用持久化的设备选择（设置页未连接时仅落盘，此处补应用）。
	c := a.cfg.Get()
	if c.AudioCapture != "" || c.AudioPlayback != "" {
		if err := sess.RestartDevices(c.AudioCapture, c.AudioPlayback); err != nil {
			log.Printf("apply audio devices: %v", err)
		}
	}
	sess.ChatHandler(func(channelID, msgID, userID, nick, content, ts string) {
		runtime.EventsEmit(a.ctx, "chat:message", map[string]string{
			"id": msgID, "channel_id": channelID, "user_id": userID,
			"nickname": nick, "content": content, "ts": ts,
		})
	})
	// 服务端 error 帧转发前端（B1：此前未订阅，服务端拒收时消息凭空消失）。
	sess.ErrorHandler(func(code, message string) {
		runtime.EventsEmit(a.ctx, "chat:error", map[string]string{
			"code": code, "message": message,
		})
	})
	a.mu.Lock()
	a.sess = sess
	a.mu.Unlock()
	// 周期推送语音统计（延迟/成员数）。
	go func() {
		tick := time.NewTicker(2 * time.Second)
		defer tick.Stop()
		for range tick.C {
			a.mu.Lock()
			s := a.sess
			a.mu.Unlock()
			if s == nil {
				return
			}
			runtime.EventsEmit(a.ctx, "voice:stats", s.Stats())
		}
	}()
	return nil
}

// disconnectSession 关闭会话。
func (a *App) disconnectSession() {
	a.mu.Lock()
	s := a.sess
	a.sess = nil
	a.mu.Unlock()
	if s != nil {
		s.Close()
	}
}

// JoinChannel 加入语音频道（真实通话链路）。
func (a *App) JoinChannel(channelID string) error {
	a.mu.Lock()
	s := a.sess
	a.mu.Unlock()
	if s == nil {
		return fmt.Errorf("not connected")
	}
	return s.JoinChannel(channelID)
}

// LeaveChannel 离开语音频道。
func (a *App) LeaveChannel() error {
	a.mu.Lock()
	s := a.sess
	a.mu.Unlock()
	if s == nil {
		return nil
	}
	return s.LeaveChannel()
}

// SetMute 本地静音开关。
func (a *App) SetMute(muted bool) {
	a.mu.Lock()
	s := a.sess
	a.mu.Unlock()
	if s != nil {
		s.SetMute(muted)
	}
}

// SetDeafen 耳聋开关。
func (a *App) SetDeafen(deafened bool) {
	a.mu.Lock()
	s := a.sess
	a.mu.Unlock()
	if s != nil {
		s.SetDeafen(deafened)
	}
}

// ---------- 服务器 / 频道 / 好友（REST CRUD） ----------
// 以下方法直接代理 state.APIClient；令牌已由登录/注册/游客/刷新路径注入。

// ListServers 列出当前用户加入的服务器。
func (a *App) ListServers() ([]state.ServerDTO, error) {
	return a.api.ListServers()
}

// CreateServer 创建服务器（名称为唯一必填项）。
func (a *App) CreateServer(name string) (*state.ServerDTO, error) {
	return a.api.CreateServer(name)
}

// JoinServerByCode 凭邀请码加入服务器。
func (a *App) JoinServerByCode(code string) (*state.ServerDTO, error) {
	return a.api.JoinServer(code)
}

// RenameServer 重命名服务器（owner/admin）。
func (a *App) RenameServer(id int64, name string) error {
	return a.api.RenameServer(id, name)
}

// DeleteServer 删除服务器（仅 owner）。
func (a *App) DeleteServer(id int64) error {
	return a.api.DeleteServer(id)
}

// CreateInviteCode 为服务器生成邀请码（admin 及以上），返回纯 code。
func (a *App) CreateInviteCode(serverID int64) (string, error) {
	return a.api.CreateInvite(serverID)
}

// CreateChannel 新建频道（typ: text / voice）。
func (a *App) CreateChannel(serverID int64, name, typ string) (*state.ChannelDTO, error) {
	return a.api.CreateChannel(serverID, name, typ)
}

// RenameChannel 重命名频道（owner/admin）。
func (a *App) RenameChannel(id int64, name string) error {
	return a.api.RenameChannel(id, name)
}

// DeleteChannel 删除频道（owner/admin）。
func (a *App) DeleteChannel(id int64) error {
	return a.api.DeleteChannel(id)
}

// SendFriendRequest 发送好友请求（target 形如 "昵称#1234"）。
func (a *App) SendFriendRequest(target string) error {
	return a.api.SendFriendRequest(target)
}

// SetPeerVolume 调整某远端成员音量（0.0–2.0）。
func (a *App) SetPeerVolume(userID string, gain float64) {
	a.mu.Lock()
	s := a.sess
	a.mu.Unlock()
	if s != nil {
		s.SetPeerVolume(userID, gain)
	}
}

// SetAudioDevices 切换采集/播放设备（设置页下拉即时应用）。
// selector 为设备 ID 十六进制串或名称子串；空 = 默认设备；设备消失时管线自动回退默认。
// 未连接时仅持久化到 config.json，下次连接时由 connectSession 的 RestartDevices 应用；
// 已连接则立即热重启设备流。
func (a *App) SetAudioDevices(captureID, playbackID string) error {
	// 先落盘（无论是否连接，配置不丢）
	if err := a.cfg.Update(func(c *config.Config) error {
		c.AudioCapture = captureID
		c.AudioPlayback = playbackID
		return nil
	}); err != nil {
		return err
	}

	a.mu.Lock()
	s := a.sess
	a.mu.Unlock()
	if s == nil {
		return nil // 未连接：仅落盘，连接后应用
	}
	return s.RestartDevices(captureID, playbackID)
}

// ---------- 语音设置（Opus 编码参数 + DSP；持久化 config.json，通话中热生效） ----------

// runtimeAudio 把持久化语音设置转成管线的运行时设置。
func (a *App) runtimeAudio() audio.RuntimeSettings {
	as := a.cfg.GetAudioSettings()
	return audio.RuntimeSettings{
		Opus: audio.OpusConfig{
			BitrateKbps: as.BitrateKbps,
			VBR:         as.VBR, DTX: as.DTX, FEC: as.FEC,
			PacketLoss: as.PacketLoss,
		},
		Denoise: as.Denoise != nil && *as.Denoise,
		Gain:    float64(as.Gain) / 10,
		VADSens: derefInt(as.VADSens, 1),
	}
}

func derefInt(p *int, def int) int {
	if p == nil {
		return def
	}
	return *p
}

// GetAudioSettings 读取持久化语音设置（设置页初始化用；未设置字段回落默认值）。
func (a *App) GetAudioSettings() config.AudioSettings { return a.cfg.GetAudioSettings() }

// SetAudioSettings 持久化语音设置，并对当前会话热应用（未连接时仅持久化，
// 下次 Connect 后经 connectSession 的 ApplyAudio 生效）。
func (a *App) SetAudioSettings(s config.AudioSettings) error {
	switch s.BitrateKbps {
	case 0, 16, 24, 32, 48, 64: // 0 = 回落默认
	default:
		return fmt.Errorf("码率非法: %d", s.BitrateKbps)
	}
	if s.PacketLoss < 0 || s.PacketLoss > 100 {
		return fmt.Errorf("丢包率非法: %d", s.PacketLoss)
	}
	if s.Gain < 0 || s.Gain > 20 {
		return fmt.Errorf("增益非法: %d", s.Gain)
	}
	if err := a.cfg.SetAudioSettings(s); err != nil {
		return err
	}
	a.mu.Lock()
	sess := a.sess
	a.mu.Unlock()
	if sess != nil {
		return sess.ApplyAudio(a.runtimeAudio())
	}
	return nil
}

// SendChat 发送文字消息。
func (a *App) SendChat(channelID, content string) error {
	a.mu.Lock()
	s := a.sess
	a.mu.Unlock()
	if s == nil {
		return fmt.Errorf("not connected")
	}
	return s.SendChat(channelID, content)
}

// GetVoiceMembers 房间成员快照。
func (a *App) GetVoiceMembers() []state.PeerView {
	a.mu.Lock()
	s := a.sess
	a.mu.Unlock()
	if s == nil {
		return nil
	}
	return s.Members()
}

// ---------- 生命周期 ----------

// shutdown 退出清理。
func (a *App) shutdown(ctx context.Context) {
	a.disconnectSession()
	a.mu.Lock()
	if a.audioCtx != nil {
		_ = a.audioCtx.Close()
		a.audioCtx = nil
	}
	a.mu.Unlock()
}
