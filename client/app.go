package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/OpenALC/LiteTalk/client/core/api"
	"github.com/OpenALC/LiteTalk/client/core/audio"
	"github.com/OpenALC/LiteTalk/client/core/config"
	"github.com/OpenALC/LiteTalk/client/core/hotkey"
	"github.com/OpenALC/LiteTalk/client/core/voice"
	"github.com/OpenALC/LiteTalk/client/desktop"
	"github.com/OpenALC/LiteTalk/client/desktop/tray"
	"github.com/gen2brain/malgo"
	"github.com/wailsapp/wails/v3/pkg/application"
	"github.com/wailsapp/wails/v3/pkg/events"
)

// version 客户端版本号，可用 -ldflags "-X main.version=" 注入，默认 dev
var version = "dev"

// App 应用结构体：Wails v3 应用/窗口引用、设置项、业务会话与 API 客户端。
// 导出方法经 Service 绑定暴露给前端调用（Call.ByName("main.App.<方法>")）。
type App struct {
	// wails/window 由 main() 在 RegisterService 之后、Run 之前经 Bind 注入。
	// 测试环境为 nil：事件发送与窗口操作全部空转（见 emit/interceptClose 等）。
	wails *application.App

	// winMu 保护 window 引用（托盘回调/服务绑定/窗口事件监听分属不同 goroutine）。
	winMu  sync.Mutex
	window *application.WebviewWindow

	// recreating 程序化销毁渲染进程（里程碑② destroy 模式）的标志：
	// WindowClosing Hook 见它放行关闭。重建窗口后复位（见 showFromTray）——
	// 新窗口的 Hook 同样检查此标志，不复位会把用户正常关窗误判为销毁。
	recreating atomic.Bool

	// 销毁前保存的窗口位置/大小/最大化态，托盘唤醒重建时恢复。
	savedBounds    *application.Rect
	savedMaximised bool

	// cfg 本地配置（config.json：服务器地址 / 休眠模式持久化）。
	cfg *config.Manager

	// forceQuit 托盘「退出」或「退出程序」置位后 OnBeforeClose 放行真正关闭。
	forceQuit atomic.Bool

	// trayReady 托盘图标+菜单就绪标志（tray onReady 置位）。
	// 托盘不可用时关窗必须放行真正关闭，绝不能隐藏窗口把用户锁死（问题 4a）。
	trayReady atomic.Bool

	api  *api.APIClient
	sess *voice.Session // 登录并连接信令后创建

	// connMu 串行化 connectSession/disconnectSession：双击登录或登录与游客并发
	// 时，同一时刻只允许一个活跃会话（旧会话在切换时关闭，不再泄漏双管线）。
	connMu sync.Mutex

	// retargetMu 串行化服务器切换（SetServerConfig 的落盘+retarget 整段）：
	// Wails 绑定并发调用，两次保存交错执行两个 retarget 会留下「REST 基址指向 B、
	// 信令会话连着 A」的撕裂态。独立于 a.mu——retarget 内部要调 startSession 等
	// 取 a.mu 的路径。锁内含 retarget 的 Me() 预检等网络 I/O：切换原子性优先，
	// 且保存本就是低频用户操作，串行等待可接受；healthz 探活在锁外（不改共享状态）。
	retargetMu sync.Mutex

	// loginGen 登录代际：登出/新登录 +1，进行中的信令重试循环发现代际变更即退出。
	loginGen atomic.Uint64

	// statsGen 语音统计 ticker 的代际计数（review S9）：connectSession 覆盖会话时
	// 旧 ticker 的「sess==nil 退出」窗口极小、极易漏退导致协程叠跑/统计双发——
	// 每次建立/断开会话 +1，ticker 循环发现代际变更即退出。
	statsGen atomic.Uint64

	mu         sync.Mutex
	refreshTok string
	user       api.UserProfile

	audioCtx *audio.Context // 设备枚举用，惰性创建

	// levelMon 设置页实时电平推送（audio:level，100ms 节流）；仅麦克风测试
	// 期间活跃。a.mu 保护指针本身，Stop 在锁外等待监视 goroutine 退出。
	levelMon *audio.LevelMonitor

	// 全局热键（v1.2.0）：hkMgr 持系统钩子，hkGate 是长期存活的值对象——
	// 都不随会话重建，重连时经 Session.SetSendGate 把同一个 Gate 挂到新管线上，
	// 用户按住的 PTT 状态与尾包窗不因重连丢失。hkQuit 关闭即退出事件消费
	// goroutine（hk.Events() 的通道永不关闭，不能 for range）。
	hkMgr     *hotkey.Manager
	hkGate    *hotkey.Gate
	hkQuit    chan struct{}
	hkQuitOne sync.Once

	// trayMute 托盘「静音」勾选项：前端/热键改静音时同步勾选态（此前只有
	// 托盘自己的点击会更新，另两条路径改了静音托盘不跟着变）。
	// setupTray 在 ServiceStartup 里赋值，早于热键 goroutine 启动。
	trayMute *application.MenuItem

	// emitHook 测试专用的事件观察口（emit 广播前同步回调，生产恒为 nil）。
	// 事件是跨层唯一可观测输出；没有它，测试只能断言状态副作用、断不到 reason。
	emitHook func(name string, data any)
}

// NewApp 创建应用实例
func NewApp() *App {
	return &App{cfg: config.New()}
}

// newAppWithConfig 测试用：注入指定路径的配置管理器。
func newAppWithConfig(m *config.Manager) *App {
	return &App{cfg: m}
}

// newAppAPI 构造 REST 客户端并注入构建版本（#31 升级门，未注入时是 dev）。
// 抽成函数让测试能验证版本头，不必拉起完整 ServiceStartup。
func newAppAPI(base string) *api.APIClient {
	c := api.NewAPIClient(base)
	c.SetClientVersion(version)
	return c
}

// Bind 注入 v3 应用与主窗口引用（main() 在创建窗口后调用，晚于 RegisterService——
// 绑定持有的是 App 指针，后续字段变更对绑定透明）。
func (a *App) Bind(wails *application.App, window *application.WebviewWindow) {
	a.wails = wails
	a.setWindow(window)
}

// getWindow/setWindow window 引用的并发安全访问器。
// destroyRenderProcess 销毁后置 nil，唤醒时由 showFromTray 重建。
func (a *App) getWindow() *application.WebviewWindow {
	a.winMu.Lock()
	defer a.winMu.Unlock()
	return a.window
}

func (a *App) setWindow(w *application.WebviewWindow) {
	a.winMu.Lock()
	defer a.winMu.Unlock()
	a.window = w
}

// emit 向前端广播事件的统一出口：测试环境（wails==nil）空转。
// emitHook 仅在测试里赋值，同步回调——测试据此断言事件与载荷。
func (a *App) emit(name string, data any) {
	if a.emitHook != nil {
		a.emitHook(name, data)
	}
	if a.wails != nil {
		a.wails.Event.Emit(name, data)
	}
}

// initLogging 把全局 log 输出重定向到文件：os.UserConfigDir()/LiteTalk/app.log
// （与 config.json 同目录）。GUI 子系统无控制台，stdout/stderr 不可见，排障
// 全依赖此文件；5MB × 3 档滚动（issue #10：不再启动截断，超限先滚动）。
// 文件 0600、目录 0700——日志含令牌/连接细节，同用户外不可读。
// 文件排 MultiWriter 首位——无控制台时 stderr 写入会失败并短路 MultiWriter，
// 放前面会吞掉文件写入。
func initLogging() {
	dir, err := os.UserConfigDir()
	if err != nil {
		return
	}
	logDir := filepath.Join(dir, "LiteTalk")
	_ = os.MkdirAll(logDir, 0o700)
	_ = os.Chmod(logDir, 0o700) // 旧版本 0755 建的目录在此收紧
	w, err := config.NewRotateWriter(filepath.Join(logDir, "app.log"), 5<<20, 3, 0o600)
	if err != nil {
		return
	}
	log.SetOutput(io.MultiWriter(w, os.Stderr))
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

// ServiceStartup v3 服务启动回调：初始化日志、创建 API 客户端、配置系统托盘。
// 服务器地址优先级：环境变量 LITETALK_API > config.json > 默认本机（运维/调试最高）。
// 托盘使用 v3 内建 SystemTray（替代 v2 getlantern/systray）；创建后由 Run 统一拉起。
func (a *App) ServiceStartup(ctx context.Context, _ application.ServiceOptions) error {
	initLogging()
	log.Printf("=== LiteTalk %s 启动 (pid=%d) ===", version, os.Getpid())
	r := a.cfg.Resolve()
	a.api = newAppAPI(r.BaseURL)
	// 401 自动刷新重试的令牌来源与回写：refresh_token 存 app 层（区分持久化
	// 策略——游客落盘 config.json），旋转后的新令牌对经回调回传。
	a.api.SetRefreshSource(func(ctx context.Context) (string, error) {
		a.mu.Lock()
		defer a.mu.Unlock()
		return a.refreshTok, nil
	})
	a.api.SetOnRefreshed(func(t api.AuthTokens) {
		a.mu.Lock()
		a.refreshTok = t.RefreshToken
		isGuest := a.user.IsGuest
		a.mu.Unlock()
		if isGuest && t.RefreshToken != "" { // 游客无密码兜底，旋转后立刻落盘
			_ = a.cfg.SetGuestRefresh(t.RefreshToken)
		}
	})
	a.setupTray()
	a.startHotkeys()
	// 机密存储模式推一次给前端（契约 §0.6 sys:secret）：回落明文文件存储必须
	// 让 UI 可感知，不能静默降级。前端也可能主动调 SecretStorageMode 补拉。
	a.emit("sys:secret", SecretModeEvent{Mode: config.SecretMode()})
	return nil
}

// setupTray 配置系统托盘：图标 + 右键菜单（显示主界面 / 静音 / 退出）。
// 菜单就绪即置 trayReady（v3 托盘由 Run 统一启动，失败会走 fatal 弹窗而非静默）。
// 图标按平台选格式：Windows 用 ICO，Linux（StatusNotifierItem）/macOS 用 PNG——
// v3 Linux 托盘 SetIcon 内部 png.Decode 只认 PNG，喂 ICO 会静默失败（托盘无图标）。
func (a *App) setupTray() {
	t := a.wails.SystemTray.New()
	t.SetIcon(tray.IconForRuntime()).SetTooltip("轻音 LiteTalk")
	// 左键单击托盘 = 显示主界面（与 Windows 行为一致；v3 SystemTray 的点击回调
	// 两平台共用同一路径，Linux StatusNotifierItem 同样可达）。
	t.OnClick(func() {
		a.showFromTray()
	})

	menu := a.wails.Menu.New()
	showItem := menu.Add("显示主界面")
	muteItem := menu.AddCheckbox("静音", false)
	a.trayMute = muteItem // 热键/前端改静音时同步勾选态（见 syncTrayMute）
	menu.AddSeparator()
	quitItem := menu.Add("退出")

	showItem.OnClick(func(*application.Context) {
		a.showFromTray()
	})
	muteItem.OnClick(func(*application.Context) {
		a.mu.Lock()
		s := a.sess
		a.mu.Unlock()
		if s == nil {
			muteItem.SetChecked(false)
			return
		}
		// 收口到 App.SetMute：勾选态同步与 voice:state 推送都在那一条路上，
		// 托盘不再自己调 Session.SetMute（那条路前端收不到状态变化）。
		a.SetMute(!s.Muted())
	})
	quitItem.OnClick(func(*application.Context) {
		a.forceQuit.Store(true)
		a.wails.Quit()
	})

	t.SetMenu(menu)
	// Linux 桌面可能没有托盘宿主（GNOME 未装 AppIndicator 扩展）：图标
	// 不会出现，此时关窗若仍隐藏就等于把应用藏丢。按实际可用性置位，
	// 不可用则关窗语义由 interceptClose/closeToTrayOrQuit 回落为退出。
	if available := tray.Available(); available {
		a.trayReady.Store(true)
	} else {
		log.Printf("托盘不可用（无 StatusNotifierWatcher），关窗将退出程序")
	}
}

// buildWindow 创建 webview 窗口并注册生命周期钩子（WindowClosing 拦截 +
// WindowMinimise 监听）。main() 启动与 showFromTray() 重建共用——钩子是
// 窗口实例级的，重建必须重新注册。
func (a *App) buildWindow(opts application.WebviewWindowOptions) *application.WebviewWindow {
	win := a.wails.Window.NewWithOptions(opts)

	// 关窗拦截：默认隐藏到托盘而非退出（见 interceptClose）。
	// Hook 先于 v3 内建关闭处理器执行，Cancel 即中止关闭。
	win.RegisterHook(events.Common.WindowClosing, func(e *application.WindowEvent) {
		if a.interceptClose() {
			e.Cancel()
		}
	})

	// 最小化监听：destroy 模式最小化即销毁渲染进程（监听器在独立 goroutine 执行）。
	win.OnWindowEvent(events.Common.WindowMinimise, func(*application.WindowEvent) {
		if a.cfg.HibernateMode() == "destroy" {
			a.destroyRenderProcess()
		}
	})

	return win
}

// destroyRenderProcess 销毁渲染进程（里程碑②，destroy 休眠模式的真实实现）：
// 释放 webview 内存（JS 堆/DOM/合成器），Go 侧语音会话与信令保持运行——
// 这是与「隐藏窗口」的本质区别。关闭是异步的：Close 返回时销毁未必完成，
// 旧窗口收尾与新窗口可短暂并存，v3 WindowManager 负责移除旧实例。
// 重复触发安全（window 已 nil 直接返回）。
func (a *App) destroyRenderProcess() {
	win := a.getWindow()
	if win == nil {
		return
	}
	if b := win.Bounds(); b.Width > 0 {
		bb := b
		a.savedBounds = &bb
		a.savedMaximised = win.IsMaximised()
	}
	a.recreating.Store(true)
	win.Close()
	a.setWindow(nil)
}

// showFromTray 托盘「显示主界面」：窗口被销毁（destroy 休眠模式）则按记忆的
// 位置/大小/最大化态重建；否则解除最小化并前置。发 sys:wake 让前端清休眠态。
func (a *App) showFromTray() {
	win := a.getWindow()
	if win == nil {
		if a.wails == nil {
			return // 测试环境
		}
		opts := application.WebviewWindowOptions{
			Title:            "轻音 LiteTalk",
			Width:            1080,
			Height:           680,
			MinWidth:         960,
			MinHeight:        600,
			Frameless:        true,
			BackgroundColour: application.NewRGB(255, 255, 255),
			Hidden:           true, // 先隐藏创建，恢复最大化态后再 Show，避免闪烁
		}
		if a.savedBounds != nil && a.savedBounds.Width > 0 {
			// Wayland 协议不允许应用定位窗口（坐标写入被忽略或告警）：只记忆尺寸，
			// 不写 X/Y，改由 Focus 前置（见下方 show 分支）。X11/Windows 保留完整位置记忆。
			if !desktop.IsWaylandSession() {
				opts.X, opts.Y = a.savedBounds.X, a.savedBounds.Y
			}
			opts.Width, opts.Height = a.savedBounds.Width, a.savedBounds.Height
		}
		win = a.buildWindow(opts)
		a.setWindow(win)
		if a.savedMaximised {
			win.Maximise()
		}
		// 新窗口生命周期开始：复位程序化销毁标志（新窗口的 Hook 同样检查
		// recreating，不复位会把用户正常关窗误判为销毁而直接退出）。
		a.recreating.Store(false)
	}
	// GTK（Linux）窗口 hide→show 重映射后会丢失 undecorated 状态（重新与合成器
	// 协商装饰时原生标题栏复活，自定义标题栏变成双标题栏）——托盘唤醒统一重设无边框。
	if runtime.GOOS == "linux" {
		win.SetFrameless(true)
	}
	win.Show()
	if win.IsMinimised() {
		win.Restore()
	}
	// Wayland 无坐标可用，靠 Focus 抢输入焦点完成前置（X11/Windows 由坐标记忆保证位置）。
	if desktop.IsWaylandSession() {
		win.Focus()
	}
	a.emit("sys:wake", nil)
}

// emitHibernate 休眠处理（设置页「休眠模式」三态之一 hibernate）：
// 发 sys:hibernate，前端卸载业务组件（省内存，前端状态保留）。
// destroy 模式不走这里——由 destroyRenderProcess 直接销毁渲染进程（里程碑②）。
// off / 未启用：什么都不做（独立生效，不再像旧版双开关互相依赖）。
func (a *App) emitHibernate() {
	if a.cfg.HibernateMode() == "hibernate" {
		a.emit("sys:hibernate", nil)
	}
}

// interceptClose 拦截窗口关闭（v3 WindowClosing Hook，替代 v2 OnBeforeClose）：
// 返回 true = 已拦截（隐藏到托盘或销毁渲染进程），false = 放行真正关闭。
// 实际语义收敛在 closeToTrayOrQuit，与 WindowClose（前端标题栏按钮）共用；
// 本函数只叠加 OS 关窗钩子特有的放行条件：
//   - recreating（程序化销毁渲染进程，里程碑②）→ 放行
//   - forceQuit（托盘「退出」/设置页「退出程序」，调用方已 Quit）→ 放行
//
// 注意：main.go 各平台均禁用/未启用「最后一窗关闭即退进程」（destroy 模式
// 销毁唯一窗口后进程须存活），所有退出路径必须显式 Quit，否则残留无窗进程。
func (a *App) interceptClose() bool {
	if a.recreating.Load() {
		return false // 程序化销毁（里程碑②），放行
	}
	if a.forceQuit.Load() {
		return false // 真正退出（Quit 已由调用方发起）
	}
	return a.closeToTrayOrQuit()
}

// closeToTrayOrQuit 关窗语义的唯一实现（interceptClose 与 WindowClose 共用，
// 两条入口行为必然一致）：
//   - destroy 休眠模式 → 销毁渲染进程替代隐藏（内存释放更彻底）
//   - 托盘不可用或用户设置「关闭窗口时=退出程序」→ 显式 Quit
//   - 其余 → 隐藏到托盘 + hibernate 模式同步卸载界面
//
// 返回 true 表示已处理且窗口仍在（Hook 侧需 Cancel 关闭），false 表示正在退出。
func (a *App) closeToTrayOrQuit() bool {
	if a.cfg.HibernateMode() == "destroy" {
		a.destroyRenderProcess()
		return true
	}
	if !a.trayReady.Load() || !a.cfg.CloseToTray() {
		// 托盘不可用：隐藏后无任何恢复路径；或用户明确设置关闭即退出。
		a.quitAfterClose()
		return false
	}
	if win := a.getWindow(); win != nil {
		win.Hide()
	}
	a.emitHibernate()
	return true
}

// quitAfterClose 在窗口关闭完成后退出进程。窗口关闭是异步的（Hook 经 channel
// 派发），同步调 Quit 会与 WM_CLOSE 收尾重入；起 goroutine 延后 50ms 再 Quit，
// 此时窗口已销毁。Quit 幂等（多路径重复调用安全）。
func (a *App) quitAfterClose() {
	if a.wails == nil {
		return
	}
	go func() {
		time.Sleep(50 * time.Millisecond)
		a.wails.Quit()
	}()
}

// QuitApp 显式退出程序（设置页「退出程序」按钮，不依赖托盘）。
func (a *App) QuitApp() {
	a.forceQuit.Store(true)
	if a.wails != nil {
		a.wails.Quit()
	}
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

// GetSavedCredentials 读取记住的登录凭据（密码由平台实现解密：Windows DPAPI /
// Linux keyring）。
func (a *App) GetSavedCredentials() SavedCredentialsView {
	c := a.cfg.GetSavedCredentials()
	return SavedCredentialsView{Email: c.Email, Password: c.Password, Saved: c.Saved}
}

// SetSavedCredentials 保存登录凭据（邮箱明文；密码 Windows DPAPI / Linux keyring，
// keyring 不可用时不保存密码）。
func (a *App) SetSavedCredentials(email, password string) {
	_ = a.cfg.SetSavedCredentials(email, password)
}

// ClearSavedCredentials 清除记住的凭据。
func (a *App) ClearSavedCredentials() {
	_ = a.cfg.ClearSavedCredentials()
}

// SecretModeEvent sys:secret 事件载荷（契约 §0.6）。
type SecretModeEvent struct {
	Mode string `json:"mode"` // keyring / file / dpapi
}

// SecretStorageMode 当前机密存储模式（keyring / file / dpapi）。
// Linux 回落明文文件存储时前端需明示提示，不静默降级（issue #8）。
func (a *App) SecretStorageMode() string {
	return config.SecretMode()
}

// ---------- 窗口控制（自定义标题栏调用） ----------

// WindowMinimize 最小化窗口。destroy 模式直接销毁渲染进程（跳过最小化动画，
// 避免先进任务栏再消失的闪烁）；hibernate 模式同步发 sys:hibernate 卸载界面。
func (a *App) WindowMinimize() {
	if a.cfg.HibernateMode() == "destroy" {
		a.destroyRenderProcess()
		return
	}
	if win := a.getWindow(); win != nil {
		win.Minimise()
	}
	a.emitHibernate()
}

// WindowMaximize 最大化/还原。
func (a *App) WindowMaximize() {
	if win := a.getWindow(); win != nil {
		win.ToggleMaximise()
	}
}

// WindowClose 关闭。前端自定义标题栏按钮调用，与 OS 关窗（interceptClose）
// 共用 closeToTrayOrQuit：destroy 模式销毁渲染进程；否则按「关闭窗口时」设置
// 与托盘可用性决定隐藏入托盘或退出程序（v2 语义曾无条件隐藏，设置不生效）。
func (a *App) WindowClose() {
	a.closeToTrayOrQuit()
}

// ShowMainWindow 唤起主窗（单实例二次启动 OnSecondInstanceLaunch 调用）：
// 复用托盘唤醒逻辑（destroy 模式按记忆位置重建 / Show / 已最小化则 Restore），
// 再前置聚焦。回调来自 wails 单实例监听 goroutine，与托盘回调同级，无新约束。
func (a *App) ShowMainWindow() {
	a.showFromTray()
	if win := a.getWindow(); win != nil {
		win.Focus()
	}
}

// ---------- 设置 ----------

// GetHibernateMode 休眠模式：off（不休眠）/ hibernate（卸载业务组件）/ destroy（整页重载）。
// 前端监听 sys:hibernate / sys:wake 事件切换休眠态（见 stores/hibernate.ts）。
func (a *App) GetHibernateMode() string { return a.cfg.HibernateMode() }

// SetHibernateMode 持久化休眠模式（校验 off/hibernate/destroy）。
func (a *App) SetHibernateMode(mode string) error { return a.cfg.SetHibernateMode(mode) }

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

// sameOrigin 判断两个基址是否同源（scheme + host + port 全同）。
// scheme/host 大小写不敏感；缺省端口按 scheme 归一化（http/ws→80、https/wss→443），
// 因此 http://a 与 http://a:80/ 同源。基址里的 path 不参与比较。
func sameOrigin(a, b string) bool {
	ua, err := url.Parse(strings.TrimSpace(a))
	if err != nil || ua.Host == "" {
		return false
	}
	ub, err := url.Parse(strings.TrimSpace(b))
	if err != nil || ub.Host == "" {
		return false
	}
	if !strings.EqualFold(ua.Scheme, ub.Scheme) || !strings.EqualFold(ua.Hostname(), ub.Hostname()) {
		return false
	}
	return effectivePort(ua) == effectivePort(ub)
}

// effectivePort 返回 URL 的有效端口：显式端口优先，否则按 scheme 的默认端口。
func effectivePort(u *url.URL) string {
	if p := u.Port(); p != "" {
		return p
	}
	switch strings.ToLower(u.Scheme) {
	case "http", "ws":
		return "80"
	case "https", "wss":
		return "443"
	}
	return ""
}

// SetServerConfig 保存服务器地址并热切换（WS 自动推导：https→wss、http→ws，末尾补 /ws）。
// 流程：校验 → healthz 探活 → 落盘 → 重定向 REST/信令到新地址 →
// 同 origin 时用当前令牌重建信令会话；跨 origin 时清空会话回登录页（见 retarget）。
// 探活失败保持原地址不动，返回错误给前端提示。
func (a *App) SetServerConfig(baseURL string) error {
	if err := config.ValidateBase(baseURL); err != nil {
		return err
	}
	// 先探活再落盘：不通的新地址不写盘，运行中状态不受影响。探活在锁外：
	// 只读探测、不改共享状态，并发保存各自探活即可。
	r := a.TestServerConnection(baseURL)
	if !r.OK {
		return fmt.Errorf("切换失败，已恢复原地址: %s", r.ErrorMsg)
	}
	// 落盘 + retarget 整段原子：交错的 retarget 会留下 REST 基址与信令会话
	// 分属两台服务器的撕裂态（双击保存即可触发）。
	a.retargetMu.Lock()
	defer a.retargetMu.Unlock()
	old := a.cfg.Resolve()
	if err := a.cfg.Update(func(c *config.Config) error {
		c.ServerBase = baseURL
		return nil
	}); err != nil {
		return err
	}
	cur := a.cfg.Resolve()
	// env 覆盖时 Resolve 结果不变（LITETALK_API 优先于 config.json）：无变化不折腾会话。
	if cur.BaseURL == old.BaseURL && cur.WSURL == old.WSURL {
		return nil
	}
	a.retarget(cur.BaseURL)
	return nil
}

// retarget 把 REST 基址切到新地址并重建信令会话（热切换核心）。
// 同 origin（仅 path 变化）令牌仍属同一服务端，可做 Me() 预检后重连；
// 跨 origin 时旧服务器的 access/refresh 一律不发给新地址——预检本身就是泄露面
// （#17），改为清会话直接回登录页。调用方须持 retargetMu——与落盘合成一个
// 原子切换，防并发保存交错。
func (a *App) retarget(baseURL string) {
	if a.api == nil {
		return // 测试环境未初始化 API 客户端，跳过
	}
	if !sameOrigin(a.api.Base(), baseURL) {
		// 跨 origin：先停重试与旧会话，原子切换基址并清 access token，再清 app 层
		// 登录态。
		//   - loginGen+1 放最前：配置已落盘，旧 startSession 的重试循环下一跳会按
		//     新地址解析 WS，必须尽快停掉。
		//   - refresh_token 切换前后各清一次：切换前那次保证新基址上的 401 拿不到
		//     旧 refresh_token；SetBaseAndClearToken 的写锁会等在飞刷新「复查+安装+
		//     onRefreshed」读锁段结束，切换后那次抹掉它可能刚回写的令牌。
		//   - 落盘游客令牌属于旧服务器，无条件作废——否则下次 LoginAsGuest 会把
		//     它发给新服务器，绕开这里的所有清理。
		// 在飞请求无法撤销（api.SetBase 的语义是自然结束于旧地址）；刷新请求本身
		// 钉在请求创建时的 base 快照上，基址已变时结果被丢弃（见 doRefresh）。
		a.loginGen.Add(1)
		a.disconnectSession()
		a.mu.Lock()
		a.refreshTok = ""
		a.mu.Unlock()
		a.api.SetBaseAndClearToken(baseURL)
		a.mu.Lock()
		a.refreshTok = ""
		a.user = api.UserProfile{}
		a.mu.Unlock()
		_ = a.cfg.SetGuestRefresh("")
		// 复用 auth:kicked 让前端走 logout → 登录页（现有唯一回登录页机制）。
		// 前端随后的 Logout() 因令牌已空，不会向新服务器发 /auth/logout。
		a.emit("auth:kicked", map[string]any{"reason": "server_changed"})
		return
	}
	a.api.SetBase(baseURL)
	a.mu.Lock()
	token := a.api.Token()
	nickname := a.user.Nickname
	loggedIn := a.user.ID != 0
	a.mu.Unlock()
	// 停旧会话与退避重试（loginGen+1），避免旧地址的重试循环赖着不走。
	a.loginGen.Add(1)
	a.disconnectSession()
	if !loggedIn || token == "" {
		a.emit("auth:session", map[string]any{"ok": false})
		return
	}
	// 令牌兼容性预检：同 origin 下令牌本就属于该服务端，401 自动刷新不会跨服泄露；
	// Me() 能通就重连信令，401（含刷新失败）才清会话回登录态。
	if _, err := a.api.Me(); err != nil {
		var ae *api.APIError
		if errors.As(err, &ae) && ae.Status == http.StatusUnauthorized {
			log.Printf("server switch: token rejected by new server: %v", err)
			a.mu.Lock()
			a.refreshTok = ""
			isGuest := a.user.IsGuest
			a.user = api.UserProfile{}
			a.mu.Unlock()
			// 清 access token：换服后旧令牌不被接受，残留会让 GetMe 把用户
			// 「登回」到一个无信令连接的僵尸会话。
			a.api.SetToken("")
			if isGuest {
				_ = a.cfg.SetGuestRefresh("")
			}
			// 复用 auth:kicked 让前端走 logout → 登录页（现有唯一回登录页机制）。
			a.emit("auth:kicked", map[string]any{"reason": "server_changed"})
			return
		}
		// 非 401（网络抖动等）：继续尝试连接信令，交给 startSession 的重试兜底。
		log.Printf("server switch: me check: %v（继续尝试信令连接）", err)
	}
	a.startSession(token, nickname)
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
func (a *App) GetAuthConfig() (*api.ServerConfig, error) {
	return a.api.GetServerConfig()
}

// SendEmailCode 发送验证码（purpose: register / reset）。
func (a *App) SendEmailCode(email, purpose string) error {
	return a.api.SendEmailCode(email, purpose)
}

// Login 邮箱密码登录。成功后连接信令（SessionOK 报告信令是否连上，
// false 时后台带退避重试，期间前端显示「信令连接中」提示条）。
func (a *App) Login(email, password string) (*api.AuthResult, error) {
	r, err := a.api.Login(email, password)
	if err != nil {
		return nil, err
	}
	a.mu.Lock()
	a.refreshTok = r.RefreshToken
	a.user = r.User
	a.mu.Unlock()
	r.SessionOK = a.startSession(r.AccessToken, r.User.Nickname)
	return r, nil
}

// Register 注册并自动登录。bootstrap 为可选管理员邀请码（#27，空=不使用）。
func (a *App) Register(nickname, email, password, code, bootstrap string) (*api.AuthResult, error) {
	r, err := a.api.Register(nickname, email, password, code, bootstrap)
	if err != nil {
		return nil, err
	}
	a.mu.Lock()
	a.refreshTok = r.RefreshToken
	a.user = r.User
	a.mu.Unlock()
	r.SessionOK = a.startSession(r.AccessToken, r.User.Nickname)
	return r, nil
}

// LoginAsGuest 游客一键进入（simple 模式）。优先复用记住的游客身份：
// 已存 refresh_token 先走 /auth/refresh 续期（rotation 立即落盘）。
// 旧实现 refresh 成功但 Me() 失败会清令牌新建游客——旧身份/好友全丢（S6）；
// 现在：refresh 响应自带 user 直接用（不带才 Me），令牌先落盘，
// 只有明确 401 才清令牌，网络错误保留已存令牌下次启动续试。
func (a *App) LoginAsGuest() (*api.AuthResult, error) {
	if saved := a.cfg.GetGuestRefresh(); saved != "" {
		t, err := a.api.Refresh(saved)
		if err == nil {
			_ = a.cfg.SetGuestRefresh(t.RefreshToken) // 轮换立即落盘
			a.mu.Lock()
			a.refreshTok = t.RefreshToken
			a.mu.Unlock()
			u := t.User
			if u == nil {
				if mu, merr := a.api.Me(); merr == nil {
					u = mu
				}
			}
			if u == nil {
				// 拿不到 user 但令牌已保：本次登录失败，下次启动续用同一游客。
				return nil, fmt.Errorf("获取用户信息失败，请重试")
			}
			a.mu.Lock()
			a.user = *u
			a.mu.Unlock()
			ok := a.startSession(t.AccessToken, u.Nickname)
			return &api.AuthResult{
				AccessToken: t.AccessToken, RefreshToken: t.RefreshToken,
				User: *u, SessionOK: ok,
			}, nil
		}
		var ae *api.APIError
		if errors.As(err, &ae) && ae.Status == http.StatusUnauthorized {
			_ = a.cfg.SetGuestRefresh("") // 明确失效（过期/被吊销/服务端迁移）才清
		} else {
			return nil, err // 网络错误：保留已存令牌，不覆盖不新建
		}
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
	r.SessionOK = a.startSession(r.AccessToken, r.User.Nickname)
	return r, nil
}

// ResetPassword 邮箱验证码重置密码。
func (a *App) ResetPassword(email, code, newPassword string) error {
	return a.api.ResetPassword(email, code, newPassword)
}

// Logout 注销：断信令、吊销 refresh，清空本地用户档案。
func (a *App) Logout() error {
	a.loginGen.Add(1) // 使进行中的信令重试循环退出
	a.mu.Lock()
	rt := a.refreshTok
	a.refreshTok = ""
	a.user = api.UserProfile{} // 清档案，否则 GetMe 仍返回旧用户
	a.mu.Unlock()
	a.disconnectSession()
	var err error
	if rt != "" {
		// 吊销请求要带 Bearer，故不能先清 access token。
		err = a.api.Logout(rt)
	}
	// 无条件清 access token：GetMe 在 user 为空时回落 a.api.Me()，残留有效
	// 令牌会把已登出的用户「登回来」（被踢后 rt 已空、api.Logout 根本不走）。
	a.api.SetToken("")
	return err
}

// onKicked 被服务端踢下线（同类设备在其他设备登录，契约 §0.5/§0.6）：
// 停信令退避重试（loginGen+1）→ 断会话 → 清内存 refresh（游客连同落盘
// 令牌一并作废，否则下次启动会拿它续出一个无人在线的会话）→ 通知前端。
// 只在被踢路径调用：主动 Logout 不发 auth:kicked，登出不弹「已在其他设备登录」。
func (a *App) onKicked(reason string) {
	a.loginGen.Add(1) // 停 startSession 的后台重试循环
	a.disconnectSession()
	a.mu.Lock()
	a.refreshTok = ""
	isGuest := a.user.IsGuest
	a.mu.Unlock()
	if isGuest {
		_ = a.cfg.SetGuestRefresh("")
	}
	if reason == "" {
		reason = "login_elsewhere"
	}
	log.Printf("session kicked: %s", reason)
	a.emit("auth:kicked", map[string]any{"reason": reason})
}

// GetMe 当前用户档案。
func (a *App) GetMe() (*api.UserProfile, error) {
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

// AudioDeviceSelection 持久化的输入/输出设备选择（空串 = 系统默认）。
type AudioDeviceSelection struct {
	CaptureID  string `json:"capture_id"`
	PlaybackID string `json:"playback_id"`
}

// GetAudioDeviceSelection 读取持久化的设备选择（设置页下拉初值；空 = 系统默认）。
func (a *App) GetAudioDeviceSelection() *AudioDeviceSelection {
	c := a.cfg.Get()
	return &AudioDeviceSelection{CaptureID: c.AudioCapture, PlaybackID: c.AudioPlayback}
}

// TestMicrophone 麦克风自检：用持久化选择的采集设备采样 2 秒，返回 RMS 与峰值
// （设置页「测试」按钮；独立于会话管线，不动通话状态；下混模式与通话链路一致）。
func (a *App) TestMicrophone() (map[string]float64, error) {
	as := a.cfg.GetAudioSettings()
	rms, peak, err := audio.TestCapture(a.cfg.Get().AudioCapture, audio.ParseInputMode(as.InputMode), 2*time.Second)
	if err != nil {
		return nil, err
	}
	return map[string]float64{"rms": rms, "peak": peak}, nil
}

// TestSpeaker 扬声器自检：播放 880Hz 正弦测试音 1.5 秒（阻塞至播完）。
func (a *App) TestSpeaker() error {
	return audio.TestPlayback(a.cfg.Get().AudioPlayback, 880, 1500*time.Millisecond)
}

// StartLevelMonitor 启动设置页实时电平推送（audio:level 事件，100ms 节流）。
// 仅麦克风测试期间调用（TestMicrophone 启动、结束时 StopLevelMonitor）；
// 重复调用为无害 no-op。设备/下混模式取持久化配置，与通话链路一致。
func (a *App) StartLevelMonitor() error {
	as := a.cfg.GetAudioSettings()
	selector := a.cfg.Get().AudioCapture
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.levelMon != nil {
		return nil
	}
	mon, err := audio.StartLevelMonitor(selector, audio.ParseInputMode(as.InputMode), func(s audio.LevelSample) {
		// 监视 goroutine 上回调：只投递事件，不做任何阻塞操作。
		a.emit("audio:level", map[string]any{
			"peak": s.PeakDB, "rms": s.RmsDB, "threshold": s.ThresholdDB,
		})
	})
	if err != nil {
		return err
	}
	a.levelMon = mon
	return nil
}

// StopLevelMonitor 停止电平推送并释放设备（幂等；锁外等待监视退出，避免死锁）。
func (a *App) StopLevelMonitor() {
	a.mu.Lock()
	mon := a.levelMon
	a.levelMon = nil
	a.mu.Unlock()
	if mon != nil {
		mon.Stop()
	}
}

// ---------- 语音会话 ----------

// signalFirstWait 首连等待上限：WS 拨号跑在后台 goroutine，connectSession
// 轮询 SignalConnected 定 session_ok；超时视为信令暂不可达（转后台重试）。
const signalFirstWait = 5 * time.Second

// startSession 建立信令会话并报告是否连上（session_ok）。未连上时起一个
// 带退避的后台重试（1s/2s/4s/8s/16s…封顶 5 分钟），直到成功或登出/切换会话
// （loginGen 代际校验）；每次状态变化发 auth:session{ok} 事件给前端提示条。
func (a *App) startSession(token, nickname string) bool {
	a.loginGen.Add(1)
	gen := a.loginGen.Load()
	attempt := func() bool {
		if err := a.connectSession(token, nickname, gen); err != nil {
			log.Printf("connect session: %v", err)
			return false
		}
		a.emit("auth:session", map[string]any{"ok": true})
		return true
	}
	if attempt() {
		return true
	}
	a.emit("auth:session", map[string]any{"ok": false})
	go func() {
		backoff := time.Second
		for {
			select {
			case <-time.After(backoff):
			}
			if a.loginGen.Load() != gen {
				return // 已登出或切换会话
			}
			if attempt() {
				return
			}
			backoff *= 2
			if backoff > 5*time.Minute {
				backoff = 5 * time.Minute
			}
		}
	}()
	return false
}

// connectSession 建立信令会话并把事件接到 Wails Events。
// connMu 全程串行化：进入时先关闭旧会话（disconnectSessionLocked），
// 保证同一时刻只有一个活跃会话——并发登录不再泄漏旧会话的双管线。
// gen 为发起方的登录代际：持锁后复查，新登录/登出已发生时放弃本次建立
// （否则旧令牌的重试可能覆盖新会话）。
func (a *App) connectSession(token, nickname string, gen uint64) error {
	a.connMu.Lock()
	defer a.connMu.Unlock()
	if a.loginGen.Load() != gen {
		return fmt.Errorf("session superseded")
	}
	return a.connectSessionLocked(token, nickname)
}

// connectSessionLocked 前提：已持 connMu。
func (a *App) connectSessionLocked(token, nickname string) error {
	a.disconnectSessionLocked()
	ev := voice.Events{
		OnPeerJoin: func(userID, nickname, tag string, speaking, mute, deafen bool) {
			a.emit("voice:peer-join", map[string]any{
				"user_id": userID, "nickname": nickname, "tag": tag,
				"speaking": speaking, "mute": mute, "deafen": deafen,
			})
		},
		OnPeerLeave: func(userID string) {
			a.emit("voice:peer-leave", userID)
		},
		OnPeerSpeaking: func(userID string, speaking, mute, deafen bool) {
			a.emit("voice:speaking", map[string]any{
				"user_id": userID, "speaking": speaking, "mute": mute, "deafen": deafen,
			})
		},
		OnConnection: func(st string) {
			a.emit("rtc:connection", st)
			// 信令连断映射 auth:session：中途断线同样要翻前端提示条。
			switch st {
			case "connected":
				a.emit("auth:session", map[string]any{"ok": true})
			case "disconnected":
				a.emit("auth:session", map[string]any{"ok": false})
			}
		},
		OnSelf: func(userID string) {
			a.emit("voice:self", userID)
		},
		// v0.1.2 请求静音：定向事件给前端（载荷带昵称，前端弹「接受」）。
		OnStateRequest: func(fromID, fromNickname string, mute, deafen *bool) {
			a.emit("voice:state_request", map[string]any{
				"from_id": fromID, "nickname": fromNickname,
				"mute": mute != nil && *mute, "deafen": deafen != nil && *deafen,
			})
		},
		// 媒体管线初始化失败（如 Linux 缺 Opus）：音频不可用但信令已连，
		// 前端 toast 明确提示，JoinChannel 会返回同因错误。
		OnVoiceUnavailable: func(errMsg string) {
			a.emit("voice:unavailable", map[string]string{"error": errMsg})
		},
		// 被踢（session.kicked / WS 4001）：回调跑在 ws 读循环 goroutine（会话
		// wg 内），而断开会话要 wg.Wait 这条读循环——同步执行必死锁，故异步。
		OnKicked: func(reason string) {
			go a.onKicked(reason)
		},
		// 语音被抢占（session.voice_taken，契约 §0.10）：Go 侧已本地退房，
		// 此处只把事实转给前端（toast + 清房间态）。同样跑在 ws 读循环
		// goroutine 上，不得同步断开会话。
		OnVoiceTaken: func() {
			a.emit("voice:taken", nil)
		},
		// 好友在线状态变化（presence.update，#11）：转给前端刷好友状态点。
		OnPresenceUpdate: func(userID string, online bool) {
			a.emit("presence:update", map[string]any{"user_id": userID, "online": online})
		},
		// 服务端优雅关闭（server.shutdown，#21）：前端提示「服务端已关闭」；
		// 停止自动重连在 Session 侧已异步处理。
		OnServerShutdown: func(reason string) {
			a.emit("sys:shutdown", map[string]any{"reason": reason})
		},
		// ICE 失败重建（rtc.failed，#19）：Go 侧已销毁 PC 并重发 join，前端仅
		// 提示语音链路重建中。回调跑在 ws 读循环 goroutine，不得同步断开会话。
		OnRTCFailed: func(channelID string) {
			a.emit("rtc:failed", map[string]any{"channel_id": channelID})
		},
		// 续传补拉完成（#32/#78）：消息与好友在线态已由 Session 内部回灌，
		// 服务器/频道等 REST 视图可能已过期。事件名固定 data:resync，只做提示，
		// 前端收到后自行经绑定刷新（data/friends store 各自订阅）。
		// Session 跑在独立 goroutine，emit 与其它语音事件同用法。
		OnResync: func() {
			a.emit("data:resync", nil)
		},
	}
	server := a.cfg.Resolve().WSURL
	sess := voice.NewSession(a.api, token, nickname, ev, log.Default())
	// 按键说话发送门：App 级长期存活的同一个 Gate，重连不换（PTT 按住状态不丢）。
	sess.SetSendGate(a.hkGate)
	sess.Connect(server) // 音频降级不阻塞：Connect 不再整体失败
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
		a.emit("chat:message", map[string]string{
			"id": msgID, "channel_id": channelID, "user_id": userID,
			"nickname": nick, "content": content, "ts": ts,
		})
	})
	// 服务端 error 帧转发前端（B1：此前未订阅，服务端拒收时消息凭空消失）。
	sess.ErrorHandler(func(code, message string) {
		a.emit("chat:error", map[string]string{
			"code": code, "message": message,
		})
	})
	a.mu.Lock()
	a.sess = sess
	a.mu.Unlock()
	// 首连结果决定本次 session_ok；超时则返回错误交给 startSession 退避重试。
	deadline := time.Now().Add(signalFirstWait)
	for !sess.SignalConnected() {
		if time.Now().After(deadline) {
			return fmt.Errorf("signal connect timeout (%s)", server)
		}
		time.Sleep(50 * time.Millisecond)
	}
	// 周期推送语音统计（延迟/成员数），仅入房期间发送——未入房无通话数据，
	// 恒定 0 无意义还白唤醒前端（采集/播放也是入房才启动，见 startMedia）。
	// 代际校验：connectSession 覆盖旧会话时旧 ticker 立即退出（S9 防叠跑）。
	gen := a.statsGen.Add(1)
	go func() {
		tick := time.NewTicker(2 * time.Second)
		defer tick.Stop()
		for range tick.C {
			if a.statsGen.Load() != gen {
				return
			}
			a.mu.Lock()
			s := a.sess
			a.mu.Unlock()
			if s == nil {
				return
			}
			if !s.InRoom() {
				continue
			}
			a.emit("voice:stats", s.Stats())
		}
	}()
	return nil
}

// disconnectSession 关闭会话。
func (a *App) disconnectSession() {
	a.connMu.Lock()
	defer a.connMu.Unlock()
	a.disconnectSessionLocked()
}

// disconnectSessionLocked 前提：已持 connMu。
func (a *App) disconnectSessionLocked() {
	a.statsGen.Add(1) // 旧 ticker 代际失效（S9）
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

// SetMute 本地静音开关。前端、托盘、全局热键三条路径都收口到这里，
// 各自只负责算出目标状态，不同时维护自己的副本。
func (a *App) SetMute(muted bool) {
	a.mu.Lock()
	s := a.sess
	a.mu.Unlock()
	if s != nil {
		s.SetMute(muted)
	}
	a.syncTrayMute(muted)
	a.emitVoiceState()
}

// SetDeafen 耳聋开关。与 SetMute 同理，是耳聋状态的唯一入口。
func (a *App) SetDeafen(deafened bool) {
	a.mu.Lock()
	s := a.sess
	a.mu.Unlock()
	if s != nil {
		s.SetDeafen(deafened)
	}
	a.emitVoiceState()
}

// ---------- 服务器 / 频道 / 好友（REST CRUD） ----------
// 以下方法直接代理 api.APIClient；令牌已由登录/注册/游客/刷新路径注入。

// ListServers 列出当前用户加入的服务器。
func (a *App) ListServers() ([]api.ServerDTO, error) {
	return a.api.ListServers()
}

// ListChannels 列出服务器的分类与频道（成员可读）；重启后频道列表恢复用。
// ID 参数一律十进制字符串（#31：Snowflake 超出 JS Number 精度，桥上不用数字）。
func (a *App) ListChannels(serverID string) (*api.ServerChannels, error) {
	return a.api.ListChannels(serverID)
}

// CreateServer 创建服务器（名称为唯一必填项）。
func (a *App) CreateServer(name string) (*api.ServerDTO, error) {
	return a.api.CreateServer(name)
}

// JoinServerByCode 凭邀请码加入服务器。
func (a *App) JoinServerByCode(code string) (*api.ServerDTO, error) {
	return a.api.JoinServer(code)
}

// RenameServer 重命名服务器（owner/admin）。
func (a *App) RenameServer(id string, name string) error {
	return a.api.RenameServer(id, name)
}

// DeleteServer 删除服务器（仅 owner）。
func (a *App) DeleteServer(id string) error {
	return a.api.DeleteServer(id)
}

// CreateInviteCode 为服务器生成邀请码（admin 及以上），返回纯 code。
func (a *App) CreateInviteCode(serverID string) (string, error) {
	return a.api.CreateInvite(serverID)
}

// CreateChannel 新建频道（typ: text / voice；categoryID 空串/「0」= 不归类）。
func (a *App) CreateChannel(serverID string, name, typ string, categoryID string) (*api.ChannelDTO, error) {
	return a.api.CreateChannel(serverID, name, typ, categoryID)
}

// RenameChannel 重命名频道（owner/admin）。
func (a *App) RenameChannel(id string, name string) error {
	return a.api.RenameChannel(id, name)
}

// DeleteChannel 删除频道（owner/admin）。
func (a *App) DeleteChannel(id string) error {
	return a.api.DeleteChannel(id)
}

// CreateCategory 创建频道分类（owner/admin；1–32 字）。
func (a *App) CreateCategory(serverID string, name string) (*api.CategoryDTO, error) {
	return a.api.CreateCategory(serverID, name)
}

// DeleteCategory 删除分类；withChannels=true 级联删除其下频道（服务端单事务）。
func (a *App) DeleteCategory(id string, withChannels bool) error {
	return a.api.DeleteCategory(id, withChannels)
}

// RenameCategory 重命名分类（owner/admin；1–32 字）。
func (a *App) RenameCategory(id string, name string) error {
	return a.api.RenameCategory(id, name)
}

// UpdateCategory 改分类名/位置（owner/admin）。Wails 绑定不用指针参数：
// name 为空串表示不改名，position < 0 表示不改位置；position 为目标序（0-based），
// 服务端移到此位置并重排同服务器其它分类为 0..n-1。
func (a *App) UpdateCategory(id string, name string, position int) (*api.CategoryDTO, error) {
	var n *string
	if name != "" {
		n = &name
	}
	var p *int
	if position >= 0 {
		p = &position
	}
	return a.api.UpdateCategory(id, n, p)
}

// SendFriendRequest 发送好友请求（target 形如 "昵称#1234"）。
func (a *App) SendFriendRequest(target string) error {
	return a.api.SendFriendRequest(target)
}

// ---------- 好友管理（v0.1.2：接真实 REST，替换前端 mock） ----------

// ListFriends 好友关系全量（accepted 分在线/离线由前端按 user.status 归组）。
func (a *App) ListFriends() ([]api.FriendDTO, error) { return a.api.ListFriends() }

// AcceptFriend 接受好友请求（仅被请求方）。
func (a *App) AcceptFriend(id string) error { return a.api.AcceptFriend(id) }

// RemoveFriend 删除好友关系。
func (a *App) RemoveFriend(id string) error { return a.api.RemoveFriend(id) }

// UpdateFriend 设置好友备注/分组（仅发起方向；空串清除）。
func (a *App) UpdateFriend(id string, remark, group string) error {
	return a.api.UpdateFriend(id, remark, group)
}

// ---------- 请求静音（v0.1.2） ----------

// RequestVoiceState 请求目标成员闭麦/静音（voice.request_state，定向转发）。
func (a *App) RequestVoiceState(channelID, targetID string, mute, deafen bool) error {
	a.mu.Lock()
	s := a.sess
	a.mu.Unlock()
	if s == nil {
		return fmt.Errorf("not connected")
	}
	return s.RequestVoiceState(channelID, targetID, mute, deafen)
}

// GetVoiceState 当前语音态快照（UI 销毁重载后回填用；channel_id 空=未在频道）。
func (a *App) GetVoiceState() voice.VoiceStateView {
	a.mu.Lock()
	s := a.sess
	a.mu.Unlock()
	if s == nil {
		return voice.VoiceStateView{}
	}
	return s.VoiceState()
}

// ---------- 界面偏好（v0.1.2：config.json 持久化） ----------

// GetHomeServer 首页服务器 id（空 = 介绍页）。
func (a *App) GetHomeServer() string { return a.cfg.HomeServerID() }

// SetHomeServer 设置首页服务器（空串恢复介绍页为首页）。
func (a *App) SetHomeServer(id string) error { return a.cfg.SetHomeServerID(id) }

// GetIntroHidden 介绍页是否已从服务器栏删除。
func (a *App) GetIntroHidden() bool { return a.cfg.IntroHidden() }

// SetIntroHidden 持久化介绍页隐藏状态。
func (a *App) SetIntroHidden(v bool) error { return a.cfg.SetIntroHidden(v) }

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
// v0.1.2：麦克风音量(0–120)→Gain、扬声器音量→PlaybackGain，新增复杂度/强度/AGC。
func (a *App) runtimeAudio() audio.RuntimeSettings {
	as := a.cfg.GetAudioSettings()
	complexity := as.Complexity
	if complexity <= 0 {
		complexity = 0 // 0 = 不主动改（管线构造已带默认 9）
	}
	return audio.RuntimeSettings{
		Opus: audio.OpusConfig{
			BitrateKbps: as.BitrateKbps,
			VBR:         as.VBR, DTX: as.DTX, FEC: as.FEC,
			PacketLoss: as.PacketLoss,
		},
		Denoise:         as.Denoise != nil && *as.Denoise,
		DenoiseStrength: as.DenoiseStrength,
		AGC:             as.AGC != nil && *as.AGC,
		Gain:            float64(as.MicVolume) / 100,
		PlaybackGain:    float64(as.PlaybackVolume) / 100,
		VADSens:         desktop.DerefInt(as.VADSens, 2),
		Complexity:      complexity,
		InputMode:       audio.ParseInputMode(as.InputMode),
		PreGainDB:       as.PreGainDB,
	}
}

// GetAudioSettings 读取持久化语音设置（设置页初始化用；未设置字段回落默认值）。
func (a *App) GetAudioSettings() config.AudioSettings { return a.cfg.GetAudioSettings() }

// SetAudioSettings 持久化语音设置，并对当前会话热应用（未连接时仅持久化，
// 下次 Connect 后经 connectSession 的 ApplyAudio 生效）。
// v0.1.2 校验扩展：强度 0–3（0=回落）、复杂度 0–10、麦克风/扬声器 0–120。
func (a *App) SetAudioSettings(s config.AudioSettings) error {
	switch s.BitrateKbps {
	case 0, 16, 24, 32, 48, 64: // 0 = 回落默认
	default:
		return fmt.Errorf("码率非法: %d", s.BitrateKbps)
	}
	if s.PacketLoss < 0 || s.PacketLoss > 100 {
		return fmt.Errorf("丢包率非法: %d", s.PacketLoss)
	}
	if s.DenoiseStrength < 0 || s.DenoiseStrength > 3 {
		return fmt.Errorf("降噪强度非法: %d", s.DenoiseStrength)
	}
	if s.Complexity < 0 || s.Complexity > 10 {
		return fmt.Errorf("复杂度非法: %d", s.Complexity)
	}
	if s.MicVolume < 0 || s.MicVolume > 120 {
		return fmt.Errorf("麦克风音量非法: %d", s.MicVolume)
	}
	if s.PlaybackVolume < 0 || s.PlaybackVolume > 120 {
		return fmt.Errorf("扬声器音量非法: %d", s.PlaybackVolume)
	}
	if s.InputMode != "" && !config.InputModes[s.InputMode] {
		return fmt.Errorf("输入声道模式非法: %s", s.InputMode)
	}
	if s.PreGainDB < config.PreGainMinDB || s.PreGainDB > config.PreGainMaxDB {
		return fmt.Errorf("前置增益越界: %.1fdB", s.PreGainDB)
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

// ---------- 全局热键（v1.2.0：PTT / 闭麦 / 静音 / 全关全开） ----------

// startHotkeys 创建全局热键 Manager 与按键说话发送门，按持久化配置启动监听，
// 并起 goroutine 把输入事件转成动作。ServiceStartup 调用一次。
//
// Manager 与 Gate 都是应用级、不随会话重建：重连时 connectSession 经
// Session.SetSendGate 把同一个 Gate 挂到新管线上。
func (a *App) startHotkeys() {
	st := a.cfg.GetHotkeySettings()
	a.hkGate = &hotkey.Gate{}
	a.hkGate.SetMode(st.Mode)
	a.hkMgr = hotkey.New(st)
	// Enabled=false 时 Start 不装钩子（返回 nil）——默认关闭，用户主动开；
	// 平台不支持时返回 ErrUnsupportedPlatform。两者都不影响可用性语义，
	// 见 GetHotkeyAvailability（只看 Available，不看 Start 的返回值）。
	if err := a.hkMgr.Start(); err != nil {
		log.Printf("hotkey start: %v", err)
	}
	a.emitHotkeyAvailable()
	a.hkQuit = make(chan struct{})
	go a.hotkeyLoop()
}

// stopHotkeys 退出事件消费 goroutine 并卸载钩子。ServiceShutdown 调用，幂等。
func (a *App) stopHotkeys() {
	// hkQuit 只关一次：关闭后不能置 nil，否则 hotkeyLoop 的 select 会拿到
	// nil 通道（永久阻塞）而不是退出信号。
	a.hkQuitOne.Do(func() {
		if a.hkQuit != nil {
			close(a.hkQuit)
		}
	})
	if a.hkMgr != nil {
		a.hkMgr.Stop()
	}
}

// emitHotkeyAvailable 推送一次平台可用性（设置页据此展示降级说明，而非灰掉了事）。
// reason 仅在不可用时给出原因。
func (a *App) emitHotkeyAvailable() {
	if a.hkMgr == nil {
		return
	}
	available := a.hkMgr.Available()
	reason := ""
	if !available {
		reason = hotkey.ErrUnsupportedPlatform.Error()
	}
	a.emit("hotkey:available", map[string]any{"available": available, "reason": reason})
}

// hotkeyLoop 消费热键事件。
// 不能 for range a.hkMgr.Events()：该通道永不关闭（Stop 之后只是不再有新事件），
// for range 会把这个 goroutine 永久挂住——必须用自己的退出信号结束循环。
func (a *App) hotkeyLoop() {
	for {
		select {
		case <-a.hkQuit:
			return
		case ev := <-a.hkMgr.Events():
			a.handleHotkeyEvent(ev)
		}
	}
}

// handleHotkeyEvent 热键动作分发，跑在专用 goroutine 上。跨线程约定与 OnKicked
// 一致：不碰会话内部状态，只走 Session 的公开方法（内部已原子/加锁），
// emit 非阻塞可直调。
func (a *App) handleHotkeyEvent(ev hotkey.Event) {
	e := desktop.EffectOf(ev)
	switch {
	case e.PTTPress:
		a.hkGate.Press()
	case e.PTTRelease:
		// 尾包窗：松手后补一小段话音，避免末字被硬切。
		tail := time.Duration(a.cfg.GetHotkeySettings().TailMS) * time.Millisecond
		a.hkGate.Release(tail)
	case e.Mute:
		mute, _ := a.voiceMuteState()
		a.SetMute(!mute)
	case e.Deafen:
		_, deafen := a.voiceMuteState()
		a.SetDeafen(!deafen)
	case e.Dual:
		// 全关 / 全开：已全关则全开，否则全关（与前端 dual 语义一致）。
		mute, deafen := a.voiceMuteState()
		allOff := !(mute && deafen)
		a.SetMute(allOff)
		a.SetDeafen(allOff)
	}
	// 仅 UI 反馈用（按住高亮），不影响发送门本身。
	if ev.Action == hotkey.ActionPTT {
		a.emit("hotkey:ptt", map[string]any{"pressed": ev.Pressed})
	}
}

// voiceMuteState 当前闭麦/静音态（无会话时两者皆 false）。
func (a *App) voiceMuteState() (mute, deafen bool) {
	a.mu.Lock()
	s := a.sess
	a.mu.Unlock()
	if s == nil {
		return false, false
	}
	return s.Muted(), s.Deafened()
}

// syncTrayMute 同步托盘「静音」勾选态。托盘点击、前端设置、全局热键三条路径
// 改静音都必须收口到这里，否则托盘勾选与真实状态会各说各话。
func (a *App) syncTrayMute(muted bool) {
	if a.trayMute != nil {
		a.trayMute.SetChecked(muted)
	}
}

// emitVoiceState 把本地闭麦/静音态推给前端。全局热键和托盘都在 webview 之外
// 改状态，前端只靠自己那一路的乐观更新会停在旧值——用户按了全局热键却看到
// 界面没反应。三条路径都走 App.SetMute/SetDeafen，在这里统一出口。
func (a *App) emitVoiceState() {
	mute, deafen := a.voiceMuteState()
	a.emit("voice:state", map[string]any{"muted": mute, "deafened": deafen})
}

// GetHotkeySettings 读取持久化热键设置（设置页初始化用；未配置回落默认值）。
func (a *App) GetHotkeySettings() hotkey.Settings { return a.cfg.GetHotkeySettings() }

// SetHotkeySettings 持久化并热应用（钩子绑定 + 发送门模式即时生效，不重连会话）。
// 校验：Sanitize 之后与入参不同即报错，不静默改写用户的输入——否则设置里看到
// 的绑定其实没生效（读路径对非法绑定是直接清空的）。
func (a *App) SetHotkeySettings(s hotkey.Settings) error {
	clean := s.Sanitize()
	if clean != s {
		return fmt.Errorf("热键设置含非法值：绑定需为修饰键加主键（如 ctrl+shift+m、mouse4），尾包延迟 0–1000ms")
	}
	if err := a.cfg.SetHotkeySettings(clean); err != nil {
		return err
	}
	// 热应用：绑定换了立刻重装钩子，模式换了立刻改发送门（未启动时 SetSettings
	// 只存值，等 Start 生效）。
	if a.hkMgr != nil {
		if err := a.hkMgr.SetSettings(clean); err != nil {
			return err
		}
	}
	if a.hkGate != nil {
		a.hkGate.SetMode(clean.Mode)
	}
	return nil
}

// GetHotkeyAvailability 当前平台是否支持全局热键（设置页据此展示降级说明）。
// 直接问 Manager 的平台能力：Enabled=false 时 Start() 也返回 nil，只有
// Available() 反映平台支持，不能用 Start 的返回值判断。
func (a *App) GetHotkeyAvailability() bool {
	return a.hkMgr != nil && a.hkMgr.Available()
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
func (a *App) GetVoiceMembers() []voice.PeerView {
	a.mu.Lock()
	s := a.sess
	a.mu.Unlock()
	if s == nil {
		return nil
	}
	return s.Members()
}

// ---------- 生命周期 ----------

// ServiceShutdown v3 服务关闭回调：退出清理。
func (a *App) ServiceShutdown() error {
	a.disconnectSession()
	a.stopHotkeys()
	a.mu.Lock()
	if a.audioCtx != nil {
		_ = a.audioCtx.Close()
		a.audioCtx = nil
	}
	a.mu.Unlock()
	return nil
}
