// Package hotkey 提供全局热键与按键说话（PTT）发送门。
//
// 平台实现按文件后缀隔离：
//   - Windows：user32 低级键盘/鼠标钩子（hotkey_windows.go）；
//   - 其余平台：暂态 stub（hotkey_fallback.go），Linux/macOS 落地后把该文件的
//     构建标签收窄，不要删除文件。
//
// Gate 是纯 atomic 的值对象，可被音频回调每帧调用；Manager 持有系统钩子，
// 仅在 Settings.Enabled 为真时安装——全局钩子侵入性强，默认关闭。
package hotkey

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"
)

// Action 可绑定动作。只绑当前已存在的动作；悄悄话属 issue #45，本批不加。
type Action string

const (
	ActionPTT    Action = "ptt"    // 按住说话
	ActionMute   Action = "mute"   // 切换闭麦
	ActionDeafen Action = "deafen" // 切换静音（耳聋）
	ActionDual   Action = "dual"   // 全关 / 全开
)

// allActions 的枚举顺序即设置面板里的展示顺序。
var allActions = []Action{ActionPTT, ActionMute, ActionDeafen, ActionDual}

// Mode 按键说话模式。
type Mode uint32

const (
	ModePTT   Mode = 0 // 按住说话
	ModeVoice Mode = 1 // 语音激活
	ModeOpen  Mode = 2 // 常开
)

// ParseMode 解析模式名（大小写、首尾空白不敏感）。
func ParseMode(s string) (Mode, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "ptt":
		return ModePTT, nil
	case "voice":
		return ModeVoice, nil
	case "open":
		return ModeOpen, nil
	}
	return ModePTT, fmt.Errorf("hotkey: 未知模式 %q（可选 ptt/voice/open）", s)
}

// String 返回模式名。越界值按 ModePTT 输出，与 Settings.Sanitize 的收敛一致。
func (m Mode) String() string {
	switch m {
	case ModeVoice:
		return "voice"
	case ModeOpen:
		return "open"
	default:
		return "ptt"
	}
}

// 修饰键位标记。
const (
	modCtrl uint8 = 1 << iota
	modAlt
	modShift
	modWin
)

// modOrder 是规范化输出的固定修饰键顺序（ctrl,alt,shift,win）。
var modOrder = [...]struct {
	bit  uint8
	name string
}{
	{modCtrl, "ctrl"},
	{modAlt, "alt"},
	{modShift, "shift"},
	{modWin, "win"},
}

// modBit 把修饰键 token 映射成位标记。
func modBit(name string) (uint8, bool) {
	switch name {
	case "ctrl":
		return modCtrl, true
	case "alt":
		return modAlt, true
	case "shift":
		return modShift, true
	case "win":
		return modWin, true
	}
	return 0, false
}

// Binding 是一个按键组合（修饰键 + 主键），不可变值类型。
type Binding struct {
	mods uint8
	key  string // 合法主键 token，全集见 keyTokens
}

// ParseBinding 解析绑定串：修饰键顺序不敏感，`+` 分隔，大小写与首尾空白不敏感。
// 规范化输出固定为「ctrl,alt,shift,win 序 + 主键」，全小写。
func ParseBinding(spec string) (Binding, error) {
	trimmed := strings.TrimSpace(spec)
	if trimmed == "" {
		return Binding{}, errors.New("hotkey: 绑定为空")
	}
	var b Binding
	hasMain := false
	for _, tok := range strings.Split(trimmed, "+") {
		t := strings.ToLower(strings.TrimSpace(tok))
		if t == "" {
			return Binding{}, fmt.Errorf("hotkey: 绑定 %q 含空 token", spec)
		}
		if bit, ok := modBit(t); ok {
			if b.mods&bit != 0 {
				return Binding{}, fmt.Errorf("hotkey: 绑定 %q 修饰键 %s 重复", spec, t)
			}
			b.mods |= bit
			continue
		}
		if !isKeyToken(t) {
			return Binding{}, fmt.Errorf("hotkey: 绑定 %q 含未知按键 %s", spec, t)
		}
		if hasMain {
			return Binding{}, fmt.Errorf("hotkey: 绑定 %q 出现多个主键", spec)
		}
		b.key = t
		hasMain = true
	}
	if !hasMain {
		return Binding{}, fmt.Errorf("hotkey: 绑定 %q 只有修饰键没有主键", spec)
	}
	return b, nil
}

// String 返回规范化形式。输出可以直接再喂给 ParseBinding（round-trip）。
func (b Binding) String() string {
	if b.IsZero() {
		return ""
	}
	var sb strings.Builder
	for _, m := range modOrder {
		if b.mods&m.bit != 0 {
			sb.WriteString(m.name)
			sb.WriteByte('+')
		}
	}
	sb.WriteString(b.key)
	return sb.String()
}

// IsZero 报告是否未绑定。
func (b Binding) IsZero() bool { return b.mods == 0 && b.key == "" }

// keyTokens 是合法主键 token 的全集。前端的归一化纯函数必须与它逐字对齐，
// 任何一边增删键位都要同步另一边（binding_test.go 里有计数断言把着门）。
var keyTokens = buildKeyTokens()

// buildKeyTokens 组装主键 token 全集：26 字母 + 10 数字 + f1–f24 + 14 个命名键
// + 11 个标点 + 5 个鼠标键 = 90。
func buildKeyTokens() map[string]struct{} {
	m := make(map[string]struct{}, 96)
	for c := 'a'; c <= 'z'; c++ {
		m[string(c)] = struct{}{}
	}
	for c := '0'; c <= '9'; c++ {
		m[string(c)] = struct{}{}
	}
	for i := 1; i <= 24; i++ {
		m["f"+strconv.Itoa(i)] = struct{}{}
	}
	for _, t := range []string{
		"space", "tab", "esc", "enter", "up", "down", "left", "right",
		"home", "end", "pgup", "pgdn", "ins", "del",
	} {
		m[t] = struct{}{}
	}
	for _, t := range []string{"`", "-", "=", "[", "]", "\\", ";", "'", ",", ".", "/"} {
		m[t] = struct{}{}
	}
	for _, t := range []string{"mouse1", "mouse2", "mouse3", "mouse4", "mouse5"} {
		m[t] = struct{}{}
	}
	return m
}

// isKeyToken 报告 token 是否是合法主键。
func isKeyToken(tok string) bool {
	_, ok := keyTokens[tok]
	return ok
}

// Settings 持久化配置（json tag snake_case，与前端 types.ts 逐字对齐）。
type Settings struct {
	Enabled  bool   `json:"enabled"`
	Mode     Mode   `json:"mode"`
	PTT      string `json:"ptt,omitempty"`
	Mute     string `json:"mute,omitempty"`
	Deafen   string `json:"deafen,omitempty"`
	Dual     string `json:"dual,omitempty"`
	TailMS   int    `json:"tail_ms,omitempty"`
	CueSound bool   `json:"cue_sound,omitempty"`
}

// DefaultTailMS 尾包窗默认 250ms：松键后补一小段话音，避免末字被硬切。
const DefaultTailMS = 250

// maxTailMS 尾包窗上限。超过 1s 的尾音会让「已经松手还在说话」的观感劣化。
const maxTailMS = 1000

// DefaultSettings 返回出厂配置：默认关闭（全局钩子侵入性强），模式 PTT，无绑定。
func DefaultSettings() Settings {
	return Settings{Enabled: false, Mode: ModePTT, TailMS: DefaultTailMS}
}

// specFor 取某个动作的原始绑定串（未规范化）。
func (s Settings) specFor(a Action) string {
	switch a {
	case ActionPTT:
		return s.PTT
	case ActionMute:
		return s.Mute
	case ActionDeafen:
		return s.Deafen
	case ActionDual:
		return s.Dual
	}
	return ""
}

// Sanitize 是读路径收敛：模式越界回落 PTT，尾包窗夹到 [0,1000]，
// 绑定解析失败按未绑定处理（清空）。幂等。
func (s Settings) Sanitize() Settings {
	out := s
	if out.Mode != ModePTT && out.Mode != ModeVoice && out.Mode != ModeOpen {
		out.Mode = ModePTT
	}
	if out.TailMS < 0 {
		out.TailMS = 0
	}
	if out.TailMS > maxTailMS {
		out.TailMS = maxTailMS
	}
	out.PTT = normalizeBinding(s.PTT)
	out.Mute = normalizeBinding(s.Mute)
	out.Deafen = normalizeBinding(s.Deafen)
	out.Dual = normalizeBinding(s.Dual)
	return out
}

// normalizeBinding 把绑定串收敛成规范化形式；解析失败返回空串。
func normalizeBinding(spec string) string {
	b, err := ParseBinding(spec)
	if err != nil {
		return ""
	}
	return b.String()
}

// checkBindings 校验原始绑定串。写入路径（Manager.SetSettings）用它把
// 「用户填了非法绑定」报出来，而不是像读路径那样静默清空——
// 否则用户在设置里看到的绑定其实根本没生效。
func checkBindings(s Settings) error {
	for _, a := range allActions {
		spec := s.specFor(a)
		if spec == "" {
			continue
		}
		if _, err := ParseBinding(spec); err != nil {
			return fmt.Errorf("hotkey: %s: %w", a, err)
		}
	}
	return nil
}

// Event 是一次输入事件。Pressed=false 表示抬起。
type Event struct {
	Action  Action
	Pressed bool
}

// eventChanCap 事件通道容量。满则丢（见 emit）——钩子回调里绝不能阻塞，
// Windows 会把超时的低级钩子（LowLevelHooksTimeout，默认 300ms）整个摘掉。
const eventChanCap = 64

// emit 非阻塞投递：通道满立即丢弃。
func emit(ch chan<- Event, ev Event) {
	select {
	case ch <- ev:
	default:
	}
}

// ErrUnsupportedPlatform 表示当前平台没有可用的全局钩子实现。
var ErrUnsupportedPlatform = errors.New("当前平台不支持全局热键")

// source 是 Manager 依赖的事件源。收窄成可注入接口，让 Manager 的生命周期与
// 分发逻辑能用假源在测试里验证；平台实现见 hotkey_windows.go / hotkey_fallback.go。
type source interface {
	// available 报告平台能否真正装钩子（不实际安装）。
	available() bool
	// start 按 st 的绑定安装钩子，事件经 emit 非阻塞投递到 ch。
	start(st Settings, ch chan<- Event) error
	// stop 卸载钩子并停止事件循环。幂等。
	stop()
	// update 热替换绑定表；st 已由 Manager 校验收敛。
	update(st Settings)
}

// Manager 全局热键监听。零值不可用，必须用 New 构造。
type Manager struct {
	mu       sync.Mutex
	src      source
	ch       chan Event
	settings Settings
	started  bool
	running  bool
}

// New 构造 Manager。s 先经 Sanitize 收敛（非法绑定清空、模式越界回落 PTT）。
func New(s Settings) *Manager {
	return &Manager{
		src:      newPlatformSource(),
		ch:       make(chan Event, eventChanCap),
		settings: s.Sanitize(),
	}
}

// Available 报告当前平台是否支持全局热键。
func (m *Manager) Available() bool { return m.src.available() }

// Events 返回事件通道。容量 64，非阻塞投递，满则丢弃。
// 通道不关闭：Stop 之后只是不再有新事件，消费方需要用自己的退出信号结束循环。
func (m *Manager) Events() <-chan Event { return m.ch }

// Start 开始监听。平台不可用返回 ErrUnsupportedPlatform；
// Settings.Enabled 为 false 时不安装钩子（返回 nil）——默认关闭，
// 避免用户没有主动开启时全局捕获输入。
func (m *Manager) Start() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.started {
		return nil
	}
	m.started = true
	if !m.settings.Enabled {
		return nil
	}
	return m.startLocked()
}

// Stop 停止监听并卸载钩子。幂等。
func (m *Manager) Stop() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.started = false
	if !m.running {
		return
	}
	m.running = false
	m.src.stop()
}

// SetSettings 热更新设置。绑定解析失败返回错误且不改动现有绑定。
func (m *Manager) SetSettings(s Settings) error {
	if err := checkBindings(s); err != nil {
		return err
	}
	clean := s.Sanitize()

	m.mu.Lock()
	defer m.mu.Unlock()
	m.settings = clean
	if !m.started {
		return nil
	}
	switch {
	case !clean.Enabled:
		if m.running {
			m.running = false
			m.src.stop()
		}
	case m.running:
		m.src.update(clean)
	default:
		return m.startLocked()
	}
	return nil
}

// startLocked 安装钩子。调用方持有 m.mu。
func (m *Manager) startLocked() error {
	if err := m.src.start(m.settings, m.ch); err != nil {
		return err
	}
	m.running = true
	return nil
}
