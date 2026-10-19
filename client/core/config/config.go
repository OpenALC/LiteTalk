// Package config 管理客户端本地配置（config.json 持久化）。
//
// 服务器地址优先级：环境变量 LITETALK_API/LITETALK_WS > 配置文件 > 默认本机地址。
// 环境变量优先级最高（运维/调试覆盖用）。
package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/OpenALC/LiteTalk/client/core/hotkey"
)

// Defaults 默认服务器地址（本机开发）。
const (
	DefaultBaseURL = "http://127.0.0.1:8077"
	DefaultWSURL   = "ws://127.0.0.1:8077/ws"
)

// Env 环境变量名（覆盖配置文件用）。
const (
	EnvAPI = "LITETALK_API"
	EnvWS  = "LITETALK_WS"
)

// Source 配置来源标识（env / file / default）。
type Source string

const (
	SourceEnv     Source = "env"
	SourceFile    Source = "file"
	SourceDefault Source = "default"
)

// Config config.json 的持久化结构。
type Config struct {
	ServerBase          string           `json:"server_base,omitempty"`           // 服务端基址，如 https://example.com
	HibernateOnMinimize *bool            `json:"hibernate_on_minimize,omitempty"` // 最小化时休眠界面（旧版布尔，已由 HibernateMode 取代，保留读取做迁移）
	HibernateMode       string           `json:"hibernate_mode,omitempty"`        // 休眠模式：off 不休眠 / hibernate 卸载业务组件 / destroy 整页重载（默认 off）
	CloseToTray         *bool            `json:"close_to_tray,omitempty"`         // 关闭窗口时最小化到托盘（默认 true；false=退出程序）
	SavedEmail          string           `json:"saved_email,omitempty"`           // 记住的登录邮箱
	EncPassword         string           `json:"enc_password,omitempty"`          // 记住的密码（DPAPI 密文 base64；非 Windows 恒空）
	EncGuestRefresh     string           `json:"enc_guest_refresh,omitempty"`     // 记住的游客 refresh_token（Windows DPAPI 密文 / Linux base64+0600 降级；其余平台恒空）
	Audio               *AudioSettings   `json:"audio,omitempty"`                 // 语音设置（码率/VBR/DTX/FEC/丢包率/降噪/增益/VAD 灵敏度）
	AudioCapture        string           `json:"audio_capture,omitempty"`         // 采集设备 ID（空=默认）
	AudioPlayback       string           `json:"audio_playback,omitempty"`        // 播放设备 ID（空=默认）
	Hotkeys             *hotkey.Settings `json:"hotkeys,omitempty"`               // 全局热键与按键说话（v1.2.0；默认关闭，见 hotkey 包）
	// v0.1.2：界面销毁休眠/首页服务器/介绍页隐藏（客户端级偏好，无需服务端）。
	UIDestroyOnHibernate *bool  `json:"ui_destroy_on_hibernate,omitempty"` // 休眠时 WindowReloadApp 卸载界面（默认 false）
	HomeServerID         string `json:"home_server_id,omitempty"`          // 首页服务器 id（空 = 介绍页 __intro__）
	IntroHidden          *bool  `json:"intro_hidden,omitempty"`            // 介绍页已从服务器栏删除（默认 false）
	// vad_sens_migrated：旧三档（0/1/2）→ 五档（0/2/4）的一次性迁移标记。
	// v0.1.2 之前只存在 0/1/2 三个合法值，迁移后新刻度 1/2 与旧值歧义，
	// 故用标记保证幂等（加载时迁移一次并落盘，之后读取不再做映射）。
	VadMigrated bool `json:"vad_sens_migrated,omitempty"`
}

// AudioSettings 语音设置。指针/零值字段在读取时回落 DefaultAudioSettings。
// v0.1.2 扩展：降噪强度/AGC/Opus 复杂度/麦克风与扬声器音量（0–120）。
// v1.1 扩展：输入声道下混 InputMode；AGC 默认开。
// 旧字段 Gain 保留读取兼容（mic_volume 为空时按 gain×10 换算）。
type AudioSettings struct {
	BitrateKbps     int     `json:"bitrate_kbps,omitempty"`     // Opus 目标码率 kbps：16/24/32/48/64（0 → 32）
	VBR             *bool   `json:"vbr,omitempty"`              // 可变码率（nil → true）
	DTX             *bool   `json:"dtx,omitempty"`              // 静音期非连续传输（nil → true）
	FEC             *bool   `json:"fec,omitempty"`              // 带内前向纠错（nil → true）
	PacketLoss      int     `json:"packet_loss,omitempty"`      // 告知编码器的预期丢包率 %（0 → 10；FEC 关闭时无意义）
	Denoise         *bool   `json:"denoise,omitempty"`          // RNNoise 降噪总开关（nil → true）
	DenoiseStrength int     `json:"denoise_strength,omitempty"` // 降噪强度 1弱/2中/3强（0 → 2；0 档配合 Denoise=false 即"关"）
	AGC             *bool   `json:"agc,omitempty"`              // 自动增益（nil → true；v1.1 起默认开）
	VADSens         *int    `json:"vad_sens,omitempty"`         // VAD 灵敏度 0–4 五档（nil → 2=中）
	Complexity      int     `json:"opus_complexity,omitempty"`  // Opus 复杂度 0–10（0 → 9）
	Gain            int     `json:"gain,omitempty"`             // 旧版输入增益 ×10（0 → 10 = 1.0；已被 MicVolume 取代，读取时换算）
	MicVolume       int     `json:"mic_volume,omitempty"`       // 麦克风音量 0–120（默认 100）
	PlaybackVolume  int     `json:"playback_volume,omitempty"`  // 扬声器音量 0–120（默认 100）
	InputMode       string  `json:"input_mode,omitempty"`       // 输入声道下混：auto/left/right/mix（"" → auto）
	PreGainDB       float64 `json:"pre_gain_db,omitempty"`      // 前置增益 dB（-12~+24，RNNoise 之前；0 → 0）
}

// InputModes 合法输入声道模式（SetAudioSettings 白名单 / GetAudioSettings 收敛）。
var InputModes = map[string]bool{"auto": true, "left": true, "right": true, "mix": true}

// 前置增益 dB 合法值域（0.25×~16×，与 audio 包 preGainMin/MaxDB 一致）。
const (
	PreGainMinDB = -12.0
	PreGainMaxDB = 24.0
)

// DefaultAudioSettings 语音设置出厂值（与管线既有硬编码一致）。
func DefaultAudioSettings() AudioSettings {
	t := true
	two := 2
	return AudioSettings{
		BitrateKbps: 32, VBR: &t, DTX: &t, FEC: &t,
		PacketLoss: 10, Denoise: &t, DenoiseStrength: 2,
		AGC: &t, VADSens: &two, Complexity: 9,
		Gain: 10, MicVolume: 100, PlaybackVolume: 100,
		InputMode: "auto",
	}
}

// GetAudioSettings 读取语音设置，未设置字段回落默认值。
func (m *Manager) GetAudioSettings() AudioSettings {
	c := m.Get()
	if c.Audio == nil {
		return DefaultAudioSettings()
	}
	s := *c.Audio
	d := DefaultAudioSettings()
	// 读路径收敛：手改 config.json 的越界值回落默认（写路径白名单只在 SetAudioSettings）
	switch s.BitrateKbps {
	case 16, 24, 32, 48, 64:
	default:
		s.BitrateKbps = d.BitrateKbps
	}
	if s.VBR == nil {
		s.VBR = d.VBR
	}
	if s.DTX == nil {
		s.DTX = d.DTX
	}
	if s.FEC == nil {
		s.FEC = d.FEC
	}
	if s.PacketLoss <= 0 || s.PacketLoss > 100 {
		s.PacketLoss = d.PacketLoss
	}
	if s.Denoise == nil {
		s.Denoise = d.Denoise
	}
	// 降噪强度：0（未设置）→ 默认中档；超出 1–3 收敛。
	if s.DenoiseStrength < 1 || s.DenoiseStrength > 3 {
		s.DenoiseStrength = d.DenoiseStrength
	}
	if s.AGC == nil {
		s.AGC = d.AGC
	}
	if s.VADSens == nil {
		s.VADSens = d.VADSens
	} else if *s.VADSens > 4 {
		one := 2
		s.VADSens = &one
	} else {
		// 拷贝 int 再使用：s 是浅拷贝，*s.VADSens 直接改写活配置（数据竞争+持久化污染）。
		v := *s.VADSens
		s.VADSens = &v
	}
	// Opus 复杂度：0 视为未设置 → 默认 9（0 档不可达，UI 滑杆 min 0 但存储层 0=未设置；
	// 与注释语义统一，避免"注释默认9/读取透传/运行时≤0不动"三处不一致）。
	if s.Complexity <= 0 || s.Complexity > 10 {
		s.Complexity = d.Complexity
	}
	// 麦克风音量：旧版 Gain（×10）→ MicVolume 换算；0（未设置）→ 100。
	// Gain 自身也收敛（仅存储兼容字段，UI 已不用）。旧 gain 13–20 换算会越界，clamp 到 120——
	// 否则读出来存不回去（SetAudioSettings 白名单拒绝 >120，设置页一保存就报错）。
	if s.Gain < 0 || s.Gain > 200 {
		s.Gain = d.Gain
	}
	if s.MicVolume <= 0 {
		if s.Gain > 0 && s.Gain <= 20 {
			s.MicVolume = s.Gain * 10
		} else {
			s.MicVolume = d.MicVolume
		}
	}
	if s.MicVolume > 120 {
		s.MicVolume = 120
	}
	if s.PlaybackVolume <= 0 {
		s.PlaybackVolume = d.PlaybackVolume
	} else if s.PlaybackVolume > 120 {
		s.PlaybackVolume = 120
	}
	// 输入声道模式：空串/非法值回落 auto。
	if !InputModes[s.InputMode] {
		s.InputMode = d.InputMode
	}
	// 前置增益：越界收敛到合法值域（0 是合法值，不做"零值即缺省"处理）。
	if s.PreGainDB < PreGainMinDB {
		s.PreGainDB = PreGainMinDB
	} else if s.PreGainDB > PreGainMaxDB {
		s.PreGainDB = PreGainMaxDB
	}
	return s
}

// SetAudioSettings 持久化语音设置（调用方传 GetAudioSettings 回落后的完整值）。
func (m *Manager) SetAudioSettings(s AudioSettings) error {
	return m.Update(func(c *Config) error {
		c.Audio = &s
		return nil
	})
}

// ---------- 全局热键与按键说话（v1.2.0） ----------

// HotkeySettings 热键设置别名：app.go 绑定方法与文档统一用它，字段/json tag
// 只在 hotkey 包定义一份（再抄一份就是两处真相，日后增删键位必然只改一边）。
type HotkeySettings = hotkey.Settings

// DefaultHotkeySettings 出厂值：默认关闭（全局钩子侵入性强）、PTT 模式、
// 无绑定、尾包 250ms。
func DefaultHotkeySettings() hotkey.Settings { return hotkey.DefaultSettings() }

// GetHotkeySettings 读路径：未配置回落默认；已配置经 Sanitize 收敛
// （模式越界→PTT、尾包夹到 [0,1000]、非法绑定按未绑定清空）。
// tail_ms 无 omitempty，不需要"0 补默认"的特例——0 是用户主动关尾包。
func (m *Manager) GetHotkeySettings() hotkey.Settings {
	c := m.Get()
	if c.Hotkeys == nil {
		return DefaultHotkeySettings()
	}
	return c.Hotkeys.Sanitize()
}

// SetHotkeySettings 持久化热键设置（调用方传完整值；非法值校验在 app 层，
// 读路径的 Sanitize 只兜手改 config.json 的情况）。
func (m *Manager) SetHotkeySettings(s hotkey.Settings) error {
	return m.Update(func(c *Config) error {
		c.Hotkeys = &s
		return nil
	})
}

// Manager 读写配置文件（路径：os.UserConfigDir()/LiteTalk/config.json）。
type Manager struct {
	path string

	mu  sync.Mutex
	cfg Config
}

// New 在默认路径创建 Manager 并加载（文件损坏/缺失时静默回默认值）。
func New() *Manager {
	dir, err := os.UserConfigDir()
	if err != nil {
		dir = "."
	}
	return NewWithPath(filepath.Join(dir, "LiteTalk", "config.json"))
}

// NewWithPath 指定路径创建 Manager（测试用）。
// 加载后执行一次性数据迁移（如 vad_sens 旧三档→五档），迁移结果立即落盘。
func NewWithPath(path string) *Manager {
	m := &Manager{path: path}
	m.cfg = m.read()
	m.migrateOnce()
	return m
}

// migrateOnce 执行有标记位保证幂等的配置迁移：
//   - vad_sens：v0.1.2 之前仅存在 0/1/2（低/中/高三档），五档刻度上映射为 0/2/4。
//     迁移靠 VadMigrated 标记只做一次——之后的新刻度 1/2 与旧值存在歧义，
//     不能每次读都做映射（那会污染新用户配置）。
func (m *Manager) migrateOnce() {
	// 休眠模式迁移：旧版两个布尔开关（最小化休眠 / 卸载界面）语义重叠、
	// 且卸载开关依赖休眠开关才生效（用户反馈「重复且不生效」）——合并为三态。
	// 空值 = 未迁移，非空即终态（无需标记位）。与 vad 迁移独立判断、各自幂等。
	dirty := false
	if m.cfg.HibernateMode == "" {
		switch {
		case m.cfg.UIDestroyOnHibernate != nil && *m.cfg.UIDestroyOnHibernate:
			m.cfg.HibernateMode = "destroy"
		case m.cfg.HibernateOnMinimize != nil && *m.cfg.HibernateOnMinimize:
			m.cfg.HibernateMode = "hibernate"
		default:
			m.cfg.HibernateMode = "off"
		}
		dirty = true
	}
	if !m.cfg.VadMigrated {
		if a := m.cfg.Audio; a != nil && a.VADSens != nil {
			if v := *a.VADSens; v == 1 || v == 2 {
				v *= 2
				a.VADSens = &v
			}
		}
		m.cfg.VadMigrated = true
		dirty = true
	}
	// 全新用户（文件不存在）也写盘：固化迁移结果，避免后续读出歧义值。
	if dirty {
		_ = m.Save(m.cfg)
	}
}

// Path 返回配置文件路径。
func (m *Manager) Path() string { return m.path }

// read 读盘；任何失败（缺失/损坏）都返回零值配置，不报错。
func (m *Manager) read() Config {
	data, err := os.ReadFile(m.path)
	if err != nil {
		return Config{}
	}
	var c Config
	if err := json.Unmarshal(data, &c); err != nil {
		return Config{} // 损坏：回默认值不报错
	}
	return c
}

// Save 原子写盘（临时文件 + rename）。
func (m *Manager) Save(c Config) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	dir := filepath.Dir(m.path)
	// 0700：配置目录含令牌文件（Linux 回落明文存储），同用户外不可进。
	// 旧版本用 0755 建的目录在此收紧（issue #10）。
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	_ = os.Chmod(dir, 0o700)
	data, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	tmp := m.path + ".tmp"
	// 0600：config.json 含加密后的游客 refresh_token（Linux 降级为 base64+0600，
	// 见 secret_linux.go），同用户外不可读——对齐 SSH 私钥的文件权限模型。
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	if err := os.Rename(tmp, m.path); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	m.cfg = c
	return nil
}

// Get 返回当前配置快照。
func (m *Manager) Get() Config {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.cfg
}

// Update 读-改-写配置（f 返回 false 则放弃保存）。
func (m *Manager) Update(f func(*Config) error) error {
	m.mu.Lock()
	c := m.cfg
	m.mu.Unlock()
	if err := f(&c); err != nil {
		return err
	}
	return m.Save(c)
}

// Resolved 服务器地址解析结果。
type Resolved struct {
	BaseURL string `json:"base_url"`
	WSURL   string `json:"ws_url"`
	Source  Source `json:"source"` // env / file / default
}

// Resolve 按优先级解析服务器地址：env > file > default。
// WS 地址：env LITETALK_WS 优先；否则由 base 推导（DeriveWS）。
func (m *Manager) Resolve() Resolved {
	envBase := strings.TrimRight(strings.TrimSpace(os.Getenv(EnvAPI)), "/")
	envWS := strings.TrimSpace(os.Getenv(EnvWS))

	fileBase := strings.TrimRight(strings.TrimSpace(m.Get().ServerBase), "/")

	base, source := DefaultBaseURL, SourceDefault
	if fileBase != "" {
		base, source = fileBase, SourceFile
	}
	if envBase != "" {
		base, source = envBase, SourceEnv
	}

	ws := ""
	switch {
	case envWS != "":
		ws = envWS
		source = SourceEnv
	default:
		ws, _ = DeriveWS(base) // base 已保证合法时推导必然成功；异常时留空由调用方兜底
		if ws == "" {
			ws = DefaultWSURL
		}
	}
	return Resolved{BaseURL: base, WSURL: ws, Source: source}
}

// DeriveWS 由 http(s) 基址推导 ws(s) 信令地址。
// 规则：https→wss、http→ws；末尾追加 /ws（已带 /ws 结尾则不重复拼）。
func DeriveWS(base string) (string, error) {
	u, err := url.Parse(strings.TrimSpace(base))
	if err != nil || u.Host == "" {
		return "", errors.New("服务器地址无效")
	}
	var wsScheme string
	switch u.Scheme {
	case "https":
		wsScheme = "wss"
	case "http":
		wsScheme = "ws"
	default:
		return "", errors.New("服务器地址需以 http:// 或 https:// 开头")
	}
	path := strings.TrimRight(u.Path, "/")
	if path == "" || path == "/ws" {
		path = "/ws"
	} else {
		path = path + "/ws"
	}
	return wsScheme + "://" + u.Host + path, nil
}

// ValidateBase 校验服务器基址合法性（http/https 前缀 + 可解析）。
func ValidateBase(base string) error {
	u, err := url.Parse(strings.TrimSpace(base))
	if err != nil || u.Host == "" {
		return errors.New("服务器地址无效")
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return errors.New("服务器地址需以 http:// 或 https:// 开头")
	}
	return nil
}

// ---------- 关闭行为（问题 4b：托盘 / 退出程序，用户可选） ----------

// CloseToTray 返回关闭窗口行为：true=最小化到托盘（默认），false=退出程序。
func (m *Manager) CloseToTray() bool {
	v := m.Get().CloseToTray
	return v == nil || *v
}

// SetCloseToTray 持久化关闭窗口行为。
func (m *Manager) SetCloseToTray(v bool) error {
	return m.Update(func(c *Config) error {
		c.CloseToTray = &v
		return nil
	})
}

// ---------- v0.1.2 界面偏好：UI 销毁休眠 / 首页服务器 / 介绍页隐藏 ----------

// HibernateModeValid 合法休眠模式。
var HibernateModeValid = map[string]bool{"off": true, "hibernate": true, "destroy": true}

// HibernateMode 休眠模式（off/hibernate/destroy；非法或空值回落 off）。
func (m *Manager) HibernateMode() string {
	v := m.Get().HibernateMode
	if !HibernateModeValid[v] {
		return "off"
	}
	return v
}

// SetHibernateMode 持久化休眠模式（校验 off/hibernate/destroy）。
func (m *Manager) SetHibernateMode(mode string) error {
	if !HibernateModeValid[mode] {
		return fmt.Errorf("休眠模式非法: %s", mode)
	}
	return m.Update(func(c *Config) error {
		c.HibernateMode = mode
		return nil
	})
}

// UIDestroyOnHibernate 休眠时是否 WindowReloadApp 卸载界面（旧版布尔，保留读取供迁移）。
func (m *Manager) UIDestroyOnHibernate() bool {
	v := m.Get().UIDestroyOnHibernate
	return v != nil && *v
}

// SetUIDestroyOnHibernate 持久化 UI 销毁休眠开关。
func (m *Manager) SetUIDestroyOnHibernate(v bool) error {
	return m.Update(func(c *Config) error {
		c.UIDestroyOnHibernate = &v
		return nil
	})
}

// HomeServerID 首页服务器 id；空串 = 介绍页（__intro__）。
func (m *Manager) HomeServerID() string { return m.Get().HomeServerID }

// SetHomeServerID 持久化首页服务器（空串恢复介绍页为首页）。
func (m *Manager) SetHomeServerID(id string) error {
	return m.Update(func(c *Config) error {
		c.HomeServerID = id
		return nil
	})
}

// IntroHidden 介绍页是否已被用户从服务器栏删除（默认 false）。
func (m *Manager) IntroHidden() bool {
	v := m.Get().IntroHidden
	return v != nil && *v
}

// SetIntroHidden 持久化介绍页隐藏状态。
func (m *Manager) SetIntroHidden(v bool) error {
	return m.Update(func(c *Config) error {
		c.IntroHidden = &v
		return nil
	})
}

// ---------- 记住密码（问题 6：DPAPI 加密存储） ----------

// SavedCredentials 已保存的登录凭据（Password 仅非空时为已记住）。
type SavedCredentials struct {
	Email    string
	Password string
	Saved    bool
}

// GetSavedCredentials 读取记住的凭据。密码经 DPAPI 解密；
// 解密失败/非 Windows 平台返回空密码（只记邮箱）。
func (m *Manager) GetSavedCredentials() SavedCredentials {
	c := m.Get()
	if c.SavedEmail == "" {
		return SavedCredentials{}
	}
	pw := ""
	if c.EncPassword != "" {
		if plain, err := unprotectString(c.EncPassword); err == nil {
			pw = plain
		}
	}
	return SavedCredentials{Email: c.SavedEmail, Password: pw, Saved: true}
}

// SetSavedCredentials 保存凭据：邮箱明文、密码 DPAPI 加密（user scope）。
// 加密失败（非 Windows）只记邮箱不记密码。
func (m *Manager) SetSavedCredentials(email, password string) error {
	enc := ""
	if password != "" {
		if s, err := protectString(password); err == nil {
			enc = s
		}
	}
	return m.Update(func(c *Config) error {
		c.SavedEmail = email
		c.EncPassword = enc
		return nil
	})
}

// ClearSavedCredentials 清除记住的凭据（取消勾选/登出时调用）。
func (m *Manager) ClearSavedCredentials() error {
	return m.Update(func(c *Config) error {
		c.SavedEmail = ""
		c.EncPassword = ""
		return nil
	})
}

// ---------- 记住游客身份（refresh_token 轮换维持同一游客） ----------

// 机密存储模式（sys:secret 事件的 mode 取值，见契约 §0.6）：
// keyring = Secret Service（Linux 优先）；file = 0600 文件弱保护（Linux 回落）；
// dpapi = Windows DPAPI。
const (
	SecretModeKeyring = "keyring"
	SecretModeFile    = "file"
	SecretModeDPAPI   = "dpapi"
)

// SecretMode 当前机密存储模式（平台相关，见 secret_*.go）。UI 据此提示降级。
func SecretMode() string { return secretMode() }

// GetGuestRefresh 读取记住的游客 refresh_token（平台保护后解密/解码）。
// 解密失败/不支持的平台返回空（视为无记住的游客）。
func (m *Manager) GetGuestRefresh() string {
	return guestTokenGet(m.Get().EncGuestRefresh)
}

// SetGuestRefresh 保存游客 refresh_token（平台保护后落盘）。
// 传空串清除。保护失败（不支持的 platform）不保存——游客没有密码兜底，
// 存不了就下次新建，绝不明文落盘。
func (m *Manager) SetGuestRefresh(token string) error {
	enc := guestTokenPut(token)
	return m.Update(func(c *Config) error {
		c.EncGuestRefresh = enc
		return nil
	})
}
