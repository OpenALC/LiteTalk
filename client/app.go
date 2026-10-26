package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	_ "net/http/pprof" // LITETALK_PPROF 探针用：仅在显式开启时监听回环地址
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/OpenALC/LiteTalk/client/core/api"
	"github.com/OpenALC/LiteTalk/client/core/audio"
	"github.com/OpenALC/LiteTalk/client/core/config"
	"github.com/OpenALC/LiteTalk/client/core/hotkey"
	"github.com/OpenALC/LiteTalk/client/core/signal"
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
	// 托盘不可用时关窗退回最小化（保留任务栏/Alt-Tab 恢复路径），
	// 绝不能隐藏或销毁窗口把用户锁死（问题 4a）。
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

	// 悄悄话（#45）：whisperMu 保护 UI 维护的目标选择与按键/生效状态。
	// whisperHeld 是物理按键态，whisperActive 是「已下发目标且旁路发送门」态
	// （目标为空时按住键也不开麦、不发信令）。
	whisperMu      sync.Mutex
	whisperTargets []string
	whisperHeld    bool
	whisperActive  bool

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
// 用 logFanout 而不是 io.MultiWriter：后者在第一个错误处短路，GUI 下 stderr
// 写失败会吞掉文件日志，轮转后重新打开失败又会吞掉 stderr。
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
	log.SetOutput(logFanout{w, os.Stderr})
	log.SetFlags(log.LstdFlags | log.Lmicroseconds)
}

// 运行时内存默认值（#96）。GOMEMLIMIT 是 Go 堆的软上限，不是进程 RSS：
// Wails/WebView2、cgo 音频缓冲与设备驱动占用都不计入其中。
// 默认走低 CPU 档：关闭基于堆增长的 GC（等价 GOGC=off），回收仅在堆接近
// 18MiB 软上限时触发。实测 core 语音全链路 live heap 约 6–12MB，18MiB 留有
// 余量；上一版 GOGC=50 + 15MiB 压得过低，GC 频繁触发导致 CPU 抖动。
// 若 live heap 逼近软上限，runtime 会连续强制回收——此时应调高上限或显式设 GOGC。
const (
	defaultMemoryLimit = 18 << 20

	// gcKeep：GOGC 已显式设置，保持 runtime 当前值，不覆盖。
	gcKeep = -1
	// gcOff：GOGC 未设置，关闭基于堆增长的 GC（SetGCPercent(-1)）。
	gcOff = -2
	// memLimitKeep：GOMEMLIMIT 已显式设置，不覆盖。
	memLimitKeep = -1
)

// runtimeMemoryPolicy 决定启动时要补的默认值，只看环境变量是否存在、不解析
// 内容：显式设置的 GOGC/GOMEMLIMIT（含空值和非法值）一律不覆盖，交由 runtime
// 按自己的规则处理，避免这里与 runtime 的校验规则漂移。
// 返回的 gcPercent：非负数为 GOGC 百分比，gcOff 表示关闭，gcKeep 表示保持；
// memLimit：>0 为字节软上限，memLimitKeep 表示不覆盖。
func runtimeMemoryPolicy(lookupEnv func(string) (string, bool)) (gcPercent int, memLimit int64) {
	gcPercent, memLimit = gcOff, defaultMemoryLimit
	if _, ok := lookupEnv("GOGC"); ok {
		gcPercent = gcKeep
	}
	if _, ok := lookupEnv("GOMEMLIMIT"); ok {
		memLimit = memLimitKeep
	}
	return gcPercent, memLimit
}

// applyRuntimeMemoryDefaults 补齐未显式设置的内存策略，返回写进 app.log 的
// 生效值描述。SetGCPercent/SetMemoryLimit 是进程级全局状态，ServiceStartup
// 早于会话与音频初始化，调用一次即覆盖整个进程生命周期。
func applyRuntimeMemoryDefaults() string {
	gcPercent, memLimit := runtimeMemoryPolicy(os.LookupEnv)
	switch gcPercent {
	case gcOff:
		debug.SetGCPercent(-1) // 关闭基于堆增长的 GC，回收交给软上限
	case gcKeep:
		// 显式 GOGC 已由 runtime 在启动时处理，不再覆盖
	default:
		debug.SetGCPercent(gcPercent)
	}
	if memLimit > 0 {
		debug.SetMemoryLimit(memLimit)
	}
	return formatRuntimeMemoryLog(os.LookupEnv)
}

// formatRuntimeMemoryLog 生成生效值描述：显式设置时打印环境变量原值，未设置
// 时打印默认值。与 apply 分离，测试格式化不会改测试进程的全局 GC 状态。
func formatRuntimeMemoryLog(lookupEnv func(string) (string, bool)) string {
	gogc := "off"
	if v, ok := lookupEnv("GOGC"); ok {
		gogc = v
	}
	limit := strconv.FormatInt(defaultMemoryLimit, 10)
	if v, ok := lookupEnv("GOMEMLIMIT"); ok {
		limit = v
	}
	return fmt.Sprintf("runtime memory: GOGC=%s GOMEMLIMIT=%s", gogc, limit)
}

// startPprofIfEnabled 按 LITETALK_PPROF 启动本地诊断端口，默认关闭。
// pprof 无鉴权且会暴露堆栈/命令行，只接受回环地址；非法或非回环值直接拒绝
// 并留日志。未设置环境变量时不监听、不起额外 goroutine。
func startPprofIfEnabled() {
	raw := os.Getenv("LITETALK_PPROF")
	if raw == "" {
		return
	}
	addr, ok := parsePprofAddr(raw)
	if !ok {
		log.Printf("pprof: 忽略 LITETALK_PPROF=%q（只接受回环 host:port）", raw)
		return
	}
	go func() {
		log.Printf("pprof: 仅本地诊断，监听 http://%s/debug/pprof/", addr)
		if err := http.ListenAndServe(addr, nil); err != nil {
			log.Printf("pprof: 监听失败: %v", err)
		}
	}()
}

// parsePprofAddr 校验 LITETALK_PPROF 的值：必须是 host:port，host 只能是
// 回环地址，port 必须是 1–65535 的数字。空 host（":6060"）会监听所有网卡，
// 一律拒绝。合法时原样返回交给 ListenAndServe。
func parsePprofAddr(raw string) (string, bool) {
	host, port, err := net.SplitHostPort(raw)
	if err != nil || host == "" {
		return "", false
	}
	if n, err := strconv.Atoi(port); err != nil || n < 1 || n > 65535 {
		return "", false
	}
	if host == "localhost" {
		return raw, true
	}
	ip := net.ParseIP(host)
	if ip == nil || !ip.IsLoopback() {
		return "", false
	}
	return raw, true
}

// logFanout 向所有目标写入同一条日志，单个目标失败不短路其余目标。
// stderr 与轮转文件任一不可用，另一侧仍能留下排障线索。
type logFanout []io.Writer

func (f logFanout) Write(p []byte) (int, error) {
	var firstErr error
	written := false
	for _, w := range f {
		if _, err := w.Write(p); err != nil {
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		written = true
	}
	if !written {
		return 0, firstErr
	}
	// 至少一个目标成功即视为日志已写出；部分失败没有有意义的返回值，
	// 全部失败才向上报错。
	return len(p), nil
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
	// 内存策略在会话/音频/托盘初始化前生效；显式环境变量优先，日志落 app.log。
	log.Print(applyRuntimeMemoryDefaults())
	startPprofIfEnabled()
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
	// 不会出现，此时关窗不能隐藏/销毁（没有恢复入口），由
	// interceptClose/closeToTrayOrQuit 退回最小化，保留任务栏/Alt-Tab。
	if available := tray.Available(); available {
		a.trayReady.Store(true)
	} else {
		log.Printf("托盘不可用（无 StatusNotifierWatcher），关窗将最小化窗口")
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
// 返回 true = 已拦截（最小化、隐藏到托盘或销毁渲染进程），false = 放行真正关闭。
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

// closeAction 关窗动作。决策与执行分离，决策是纯函数以便穷举组合测试。
type closeAction int

const (
	closeActionQuit     closeAction = iota // 退出程序
	closeActionMinimise                    // 最小化窗口（托盘不可用时的唯一安全出口）
	closeActionTray                        // 隐藏到托盘 + 按休眠模式卸载界面
	closeActionDestroy                     // 销毁渲染进程（destroy 休眠模式）
)

// closeActionFor 关窗决策表（顺序即优先级）：
//  1. 托盘不可用 → 最小化。隐藏/销毁都没有恢复入口，退出又违背用户预期；
//     最小化保留任务栏与 Alt-Tab，重启托盘宿主后仍可正常使用。
//  2. 托盘可用 + destroy 休眠模式 → 销毁渲染进程（内存释放最彻底，可由托盘重建）。
//  3. 托盘可用 + 用户设置「关闭窗口时=退出程序」→ 退出。
//  4. 其余 → 隐藏到托盘，hibernate 模式同步卸载界面。
func closeActionFor(hibernateMode string, trayReady, closeToTray bool) closeAction {
	if !trayReady {
		return closeActionMinimise
	}
	if hibernateMode == "destroy" {
		return closeActionDestroy
	}
	if !closeToTray {
		return closeActionQuit
	}
	return closeActionTray
}

// closeToTrayOrQuit 关窗语义的唯一实现（interceptClose 与 WindowClose 共用，
// 两条入口行为必然一致）；决策见 closeActionFor。返回 true 表示已处理且窗口
// 仍在（Hook 侧需 Cancel 关闭），false 表示正在退出。
func (a *App) closeToTrayOrQuit() bool {
	switch closeActionFor(a.cfg.HibernateMode(), a.trayReady.Load(), a.cfg.CloseToTray()) {
	case closeActionDestroy:
		a.destroyRenderProcess()
		return true
	case closeActionMinimise:
		if win := a.getWindow(); win != nil {
			win.Minimise()
		}
		return true
	case closeActionTray:
		if win := a.getWindow(); win != nil {
			win.Hide()
		}
		a.emitHibernate()
		return true
	default: // closeActionQuit
		a.quitAfterClose()
		return false
	}
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
// 共用 closeToTrayOrQuit：托盘不可用则最小化；托盘可用时按休眠模式与
// 「关闭窗口时」设置决定销毁渲染进程、隐藏入托盘或退出程序。
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
		OnPeerJoin: func(userID, nickname, tag string, speaking, mute, deafen, priority bool) {
			a.emit("voice:peer-join", map[string]any{
				"user_id": userID, "nickname": nickname, "tag": tag,
				"speaking": speaking, "mute": mute, "deafen": deafen, "priority": priority,
			})
		},
		OnPeerLeave: func(userID string) {
			a.emit("voice:peer-leave", userID)
		},
		OnPeerPriority: func(userID string, priority bool) {
			a.emit("voice:priority", map[string]any{"user_id": userID, "priority": priority})
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
		// 媒体不可用（管线初始化失败，或入房后采集/播放启动失败，#80）：
		// 音频不可用但信令已连，前端 toast 明确提示，JoinChannel 会返回同因错误。
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
			a.clearWhisper()
			a.emit("voice:taken", nil)
		},
		// 好友在线状态变化（presence.update，#11/#92）：转给前端刷好友状态点；
		// 频道快照字段同现同缺，空串不发（键缺省，与服务端隐私口径一致）。
		OnPresenceUpdate: func(userID string, online bool, channelID, serverID string) {
			payload := map[string]any{"user_id": userID, "online": online}
			if channelID != "" {
				payload["channel_id"] = channelID
			}
			if serverID != "" {
				payload["server_id"] = serverID
			}
			a.emit("presence:update", payload)
		},
		// 私聊/群聊变更（conversation.changed，#38 B3）：只转发会话 ID 与动作，
		// 前端按 ID 重新拉取（删除/退出按 action=deleted 从列表移除）。回调跑在
		// ws 读循环 goroutine，emit 不阻塞。
		OnConversationChanged: func(conversationID, action string) {
			a.emit("conversation:changed", map[string]string{
				"conversation_id": conversationID, "action": action,
			})
		},
		// 消息编辑/软删除广播（message.edited / message.deleted，#39）：前端按
		// message_id 原地替换或显示墓碑。回调跑在 ws 读循环 goroutine，emit 不阻塞。
		// edited_at 沿用桥接口径（Unix 毫秒字符串），与 chat:message 的 ts 一致。
		OnMessageEdited: func(d signal.MessageEditedData) {
			mentions := d.Mentions
			if mentions == nil {
				mentions = []string{}
			}
			a.emit("chat:edited", map[string]any{
				"channel_id": d.ChannelID, "message_id": d.MessageID,
				"content": d.Content, "edited_at": strconv.FormatInt(d.EditedAt, 10),
				"mentions": mentions,
			})
		},
		OnMessageDeleted: func(d signal.MessageDeletedData) {
			a.emit("chat:deleted", map[string]string{
				"channel_id": d.ChannelID, "message_id": d.MessageID,
			})
		},
		// 频道未读/提及计数（channel.unread，#40）：服务端按接收者精确计算，
		// 前端收到后整体覆盖本地计数（覆盖而非累加）。回调跑在 ws 读循环
		// goroutine，emit 不阻塞。
		OnChannelUnread: func(d signal.ChannelUnreadData) {
			a.emit("chat:unread", map[string]any{
				"channel_id": d.ChannelID, "unread_count": d.UnreadCount, "mention_count": d.MentionCount,
			})
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
		// 进房被服务端拒绝（#91）：Session 已回滚房间态（InRoom=false，重连不
		// 再重发 join）。前端据 voice:join_failed 清掉 channelId/延迟并提示。
		// 同一次失败不再走 chat:error（见 ErrorHandlerEx）。
		OnJoinFailed: func(channelID, code, message string) {
			a.emit("voice:join_failed", map[string]string{
				"channel_id": channelID, "code": code, "message": message,
			})
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
	sess.ChatMessageHandler(func(d signal.ChatMessageData) {
		// 旧字段（id/channel_id/user_id/nickname/content/ts/nonce）保持不变，
		// #39 追加回复与提及；reply_to/reply_preview 缺省时不发键，前端按无回复处理。
		mentions := d.Mentions
		if mentions == nil {
			mentions = []string{}
		}
		payload := map[string]any{
			"id": d.ID, "channel_id": d.ChannelID, "user_id": d.User.ID,
			"nickname": d.User.Nickname, "content": d.Content,
			"ts": strconv.FormatInt(d.TS, 10), "nonce": d.Nonce,
			"mentions": mentions,
		}
		if d.ReplyTo != "" {
			payload["reply_to"] = d.ReplyTo
		}
		if p := d.ReplyPreview; p != nil {
			payload["reply_preview"] = map[string]any{
				"id": p.ID, "user_id": p.UserID, "nickname": p.Nickname,
				"content": p.Content, "deleted": p.Deleted,
			}
		}
		// #41：无附件时不发键，前端按无附件处理；相对 url 在这里拼成绝对地址。
		if len(d.Attachments) > 0 {
			payload["attachments"] = a.attachmentViewsFromSignal(d.Attachments)
		}
		a.emit("chat:message", payload)
	})
	// 服务端 error 帧转发前端（B1：此前未订阅，服务端拒收时消息凭空消失）。
	// 已归属进房路径的错误（#91）由 voice:join_failed 承载，这里跳过以免同一次
	// 失败弹两条提示。
	sess.ErrorHandlerEx(func(code, message string, joinHandled bool) {
		if joinHandled {
			return
		}
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
	a.clearWhisper()
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
	// 切房后旧的悄悄话目标已无意义（成员集合不同），清选择并关旁路门。
	a.clearWhisper()
	return s.JoinChannel(channelID)
}

// JoinChannelWithPassword 带密码入房（#37）。密码由 Session 缓存供重连复用；
// 不落盘、不进日志（桥接/前端 B2 再接 UI）。
func (a *App) JoinChannelWithPassword(channelID, password string) error {
	a.mu.Lock()
	s := a.sess
	a.mu.Unlock()
	if s == nil {
		return fmt.Errorf("not connected")
	}
	a.clearWhisper()
	return s.JoinChannelWithPassword(channelID, password)
}

// LeaveChannel 离开语音频道。
func (a *App) LeaveChannel() error {
	a.mu.Lock()
	s := a.sess
	a.mu.Unlock()
	a.clearWhisper()
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

// CreateSubchannel 建子频道（#37 B2）：类型继承父、分类由服务端随父。
func (a *App) CreateSubchannel(serverID, name, typ, parentID string) (*api.ChannelDTO, error) {
	return a.api.CreateSubchannel(serverID, name, typ, parentID)
}

// UpdateChannelOptions 更新频道选项（#37 B2、#46）：parentID 空串不改/"0" 顶层；
// userLimit <0 不改；audioBitrateKbps/audioFrameMS <0 不改（0 = 跟随设置/默认）；
// password 空串不改，clearPassword 清除。
func (a *App) UpdateChannelOptions(channelID, parentID string, userLimit, audioBitrateKbps, audioFrameMS int, password string, clearPassword bool) (*api.ChannelDTO, error) {
	return a.api.UpdateChannelOptions(channelID, parentID, userLimit, audioBitrateKbps, audioFrameMS, password, clearPassword)
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

// ---------- 私聊/群私聊会话（#38 B4：REST 桥接） ----------
// 以下方法直接代理 api.APIClient；ID 参数一律十进制字符串（#31）。
// 变更通知走 conversation:changed 事件，前端据此重新拉取。

// ListConversations 当前用户参与的私聊/群私聊（含成员）。
func (a *App) ListConversations() ([]api.ConversationDTO, error) {
	return a.api.ListConversations()
}

// CreateDirectConversation 创建私聊（已有则幂等返回原会话）。
func (a *App) CreateDirectConversation(userID string) (*api.ConversationDTO, error) {
	return a.api.CreateDirectConversation(userID)
}

// CreateGroupConversation 创建群私聊（name 可为空）。
func (a *App) CreateGroupConversation(name string, userIDs []string) (*api.ConversationDTO, error) {
	return a.api.CreateGroupConversation(name, userIDs)
}

// GetConversation 会话详情（仅参与者）。
func (a *App) GetConversation(id string) (*api.ConversationDTO, error) {
	return a.api.GetConversation(id)
}

// RenameConversation 群聊改名（仅 owner）。
func (a *App) RenameConversation(id, name string) (*api.ConversationDTO, error) {
	return a.api.RenameConversation(id, name)
}

// DeleteConversation 删除会话（direct 任一方；group 仅 owner）。
func (a *App) DeleteConversation(id string) error {
	return a.api.DeleteConversation(id)
}

// AddConversationMembers 群聊加人（仅 owner）。
func (a *App) AddConversationMembers(id string, userIDs []string) (*api.ConversationDTO, error) {
	return a.api.AddConversationMembers(id, userIDs)
}

// RemoveConversationMember 移除成员或退出群聊；最后一人退出时会话被删除，
// 此时返回 nil（Wails 侧为 null）。
func (a *App) RemoveConversationMember(id, userID string) (*api.ConversationDTO, error) {
	return a.api.RemoveConversationMember(id, userID)
}

// ---------- 未读与提及计数（#40） ----------
// 快照与已读上报直接代理 api.APIClient；计数变化由 chat:unread 事件推送，
// 前端必须整体覆盖（服务端按接收者精确计算）。Wails 绑定不返回指针：
// nil 归一为空结构，前端不必处理 null。

// ListUnreads 当前用户的未读快照（含 0 值项；空数组为 []）。
func (a *App) ListUnreads() (api.UnreadsDTO, error) {
	out, err := a.api.ListUnreads()
	if err != nil {
		return api.UnreadsDTO{}, err
	}
	if out == nil {
		return api.UnreadsDTO{
			Channels: []api.UnreadChannelDTO{},
			Servers:  []api.UnreadServerDTO{},
		}, nil
	}
	return *out, nil
}

// MarkChannelRead 上报频道已读到 lastMessageID，返回服务端重算后的计数。
func (a *App) MarkChannelRead(channelID, lastMessageID string) (api.UnreadChannelDTO, error) {
	out, err := a.api.MarkChannelRead(channelID, lastMessageID)
	if err != nil {
		return api.UnreadChannelDTO{}, err
	}
	if out == nil {
		return api.UnreadChannelDTO{}, nil
	}
	return *out, nil
}

// ChatMessageView 消息桥接视图：与 chat:message 事件的载荷同口径（ts 为毫秒
// 字符串、ID 均为十进制字符串），前端可把历史补拉与实时广播直接按 id 合并。
// 私聊/群聊的会话 ID 即频道 ID，历史消息仍走 /channels/{id}/messages。
// #39：EditedAt/DeletedAt 也是毫秒字符串；软删消息 Content 为空、DeletedAt 有值。
type ChatMessageView struct {
	ID           string            `json:"id"`
	ChannelID    string            `json:"channel_id"`
	UserID       string            `json:"user_id"`
	Nickname     string            `json:"nickname"`
	Content      string            `json:"content"`
	TS           string            `json:"ts"`
	EditedAt     string            `json:"edited_at,omitempty"`
	DeletedAt    string            `json:"deleted_at,omitempty"`
	ReplyTo      string            `json:"reply_to,omitempty"`
	ReplyPreview *ReplyPreviewView `json:"reply_preview,omitempty"`
	Mentions     []string          `json:"mentions"`
	// Attachments 消息附件（#41）：无附件或软删消息时缺省，前端按可选处理。
	Attachments []AttachmentView `json:"attachments,omitempty"`
}

// AttachmentView 附件桥接视图（与 api.AttachmentDTO 同口径）。url/thumbnail_url
// 已由 chatMessageView / chat.message 载荷拼成绝对地址，前端可直接作 img src。
type AttachmentView struct {
	ID           string `json:"id"`
	Filename     string `json:"filename"`
	Size         int64  `json:"size"`
	ContentType  string `json:"content_type"`
	Width        int    `json:"width"`
	Height       int    `json:"height"`
	URL          string `json:"url"`
	ThumbnailURL string `json:"thumbnail_url,omitempty"`
}

// ReplyPreviewView 回复目标预览（与 REST ReplyPreview 同口径）。
type ReplyPreviewView struct {
	ID       string `json:"id"`
	UserID   string `json:"user_id"`
	Nickname string `json:"nickname"`
	Content  string `json:"content"`
	Deleted  bool   `json:"deleted"`
}

// MentionCandidateView @提及候选用户（不含 email）。
type MentionCandidateView struct {
	UserID   string `json:"user_id"`
	Nickname string `json:"nickname"`
	Tag      string `json:"tag"`
}

// chatMessageView REST 消息 → 桥接视图（ts/edited_at/deleted_at 统一毫秒字符串；
// 附件相对 url 拼成当前基址的绝对地址，前端可直接加载）。
func (a *App) chatMessageView(m api.MessageDTO) ChatMessageView {
	v := ChatMessageView{
		ID:        m.ID.String(),
		ChannelID: m.ChannelID.String(),
		UserID:    m.UserID.String(),
		Nickname:  m.SenderNickname,
		Content:   m.Content,
		TS:        strconv.FormatInt(m.CreatedAt.UnixMilli(), 10),
		Mentions:  make([]string, 0, len(m.Mentions)),
	}
	if m.EditedAt != nil {
		v.EditedAt = strconv.FormatInt(m.EditedAt.UnixMilli(), 10)
	}
	if m.DeletedAt != nil {
		v.DeletedAt = strconv.FormatInt(m.DeletedAt.UnixMilli(), 10)
	}
	if m.ReplyTo != nil {
		v.ReplyTo = m.ReplyTo.String()
	}
	if p := m.ReplyPreview; p != nil {
		v.ReplyPreview = &ReplyPreviewView{
			ID: p.ID.String(), UserID: p.UserID.String(), Nickname: p.Nickname,
			Content: p.Content, Deleted: p.Deleted,
		}
	}
	for _, id := range m.Mentions {
		v.Mentions = append(v.Mentions, id.String())
	}
	// 软删消息不下发附件（服务端已省略，这里再兜一层，避免墓碑后仍渲染图片）。
	if m.DeletedAt == nil && m.Attachments != nil {
		v.Attachments = a.attachmentViews(*m.Attachments)
	}
	return v
}

// attachmentViews api 附件列表 → 桥接视图，相对 url 统一拼基址。
// api 未初始化（测试/启动早期）时保留相对地址，不 panic。
func (a *App) attachmentViews(list []api.AttachmentDTO) []AttachmentView {
	out := make([]AttachmentView, 0, len(list))
	for _, at := range list {
		out = append(out, AttachmentView{
			ID: at.ID.String(), Filename: at.Filename, Size: at.Size,
			ContentType: at.ContentType, Width: at.Width, Height: at.Height,
			URL:          a.resolveAttachmentURL(at.URL),
			ThumbnailURL: a.resolveAttachmentURL(at.ThumbnailURL),
		})
	}
	return out
}

// attachmentViewsFromSignal WS chat.message 的附件载荷 → 桥接视图。
func (a *App) attachmentViewsFromSignal(list []signal.AttachmentInfo) []AttachmentView {
	out := make([]AttachmentView, 0, len(list))
	for _, at := range list {
		out = append(out, AttachmentView{
			ID: at.ID, Filename: at.Filename, Size: at.Size,
			ContentType: at.ContentType, Width: int(at.Width), Height: int(at.Height),
			URL:          a.resolveAttachmentURL(at.URL),
			ThumbnailURL: a.resolveAttachmentURL(at.ThumbnailURL),
		})
	}
	return out
}

// resolveAttachmentURL 相对能力 URL → 绝对地址；空串原样返回。
func (a *App) resolveAttachmentURL(rel string) string {
	if rel == "" || a.api == nil {
		return rel
	}
	return a.api.ResolveURL(rel)
}

// ListMessages 拉频道最近消息（旧→新，limit 由 APIClient 夹到 [1,50]）。
// 打开私聊/群聊时补历史；失败由调用方静默，不阻断实时消息。
func (a *App) ListMessages(channelID string, limit int) ([]ChatMessageView, error) {
	id, err := strconv.ParseInt(channelID, 10, 64)
	if err != nil || id <= 0 {
		return nil, fmt.Errorf("invalid channel id")
	}
	if a.api == nil {
		return nil, fmt.Errorf("not connected")
	}
	msgs, err := a.api.ListMessages(id, limit)
	if err != nil {
		return nil, err
	}
	out := make([]ChatMessageView, 0, len(msgs))
	for _, m := range msgs {
		out = append(out, a.chatMessageView(m))
	}
	return out, nil
}

// SearchMessages 全局消息搜索（#42 B2）：跨服务器频道与会话，载荷与 ListMessages
// 同口径（ts 毫秒字符串、附件 URL 已绝对化），前端可直接按 channel_id 定位。
// 搜索词为空白时 APIClient 直接返回空结果；limit 由其夹到 [1,50]。
func (a *App) SearchMessages(query string, limit int) ([]ChatMessageView, error) {
	if a.api == nil {
		return nil, fmt.Errorf("not connected")
	}
	msgs, err := a.api.SearchMessages(query, limit)
	if err != nil {
		return nil, err
	}
	out := make([]ChatMessageView, 0, len(msgs))
	for _, m := range msgs {
		out = append(out, a.chatMessageView(m))
	}
	return out, nil
}

// GetLinkPreview 链接预览（#42 B4）：服务端抓取 + 缓存，客户端只渲染。
// 无预览（服务端 404 no_preview / 功能关闭）返回 (nil, nil)，前端据此静默隐藏。
func (a *App) GetLinkPreview(rawURL string) (*api.LinkPreviewDTO, error) {
	if a.api == nil {
		return nil, fmt.Errorf("not connected")
	}
	return a.api.GetLinkPreview(rawURL)
}

// GetMessage 单条消息（#39）：回复跳转与软删消息回读。不存在/不可见返回错误。
func (a *App) GetMessage(channelID, messageID string) (*ChatMessageView, error) {
	cid, err := strconv.ParseInt(channelID, 10, 64)
	if err != nil || cid <= 0 {
		return nil, fmt.Errorf("invalid channel id")
	}
	mid, err := strconv.ParseInt(messageID, 10, 64)
	if err != nil || mid <= 0 {
		return nil, fmt.Errorf("invalid message id")
	}
	if a.api == nil {
		return nil, fmt.Errorf("not connected")
	}
	m, err := a.api.GetMessage(cid, mid)
	if err != nil {
		return nil, err
	}
	v := a.chatMessageView(*m)
	return &v, nil
}

// EditMessage 编辑消息（仅作者，#39）。mentions 整体替换：空数组 = 清除全部提及。
func (a *App) EditMessage(channelID, messageID, content string, mentions []string) (*ChatMessageView, error) {
	cid, err := strconv.ParseInt(channelID, 10, 64)
	if err != nil || cid <= 0 {
		return nil, fmt.Errorf("invalid channel id")
	}
	mid, err := strconv.ParseInt(messageID, 10, 64)
	if err != nil || mid <= 0 {
		return nil, fmt.Errorf("invalid message id")
	}
	if a.api == nil {
		return nil, fmt.Errorf("not connected")
	}
	m, err := a.api.EditMessage(cid, mid, content, mentions)
	if err != nil {
		return nil, err
	}
	v := a.chatMessageView(*m)
	return &v, nil
}

// DeleteMessage 软删除消息（作者或管理员，#39）。
func (a *App) DeleteMessage(channelID, messageID string) error {
	cid, err := strconv.ParseInt(channelID, 10, 64)
	if err != nil || cid <= 0 {
		return fmt.Errorf("invalid channel id")
	}
	mid, err := strconv.ParseInt(messageID, 10, 64)
	if err != nil || mid <= 0 {
		return fmt.Errorf("invalid message id")
	}
	if a.api == nil {
		return fmt.Errorf("not connected")
	}
	return a.api.DeleteMessage(cid, mid)
}

// ListMentionCandidates @提及候选用户（#39）：服务器频道为可查看该频道的成员，
// 会话频道为会话成员；按昵称升序。空结果为 []。
func (a *App) ListMentionCandidates(channelID string) ([]MentionCandidateView, error) {
	cid, err := strconv.ParseInt(channelID, 10, 64)
	if err != nil || cid <= 0 {
		return nil, fmt.Errorf("invalid channel id")
	}
	if a.api == nil {
		return nil, fmt.Errorf("not connected")
	}
	cands, err := a.api.ListMentionCandidates(cid)
	if err != nil {
		return nil, err
	}
	out := make([]MentionCandidateView, 0, len(cands))
	for _, c := range cands {
		out = append(out, MentionCandidateView{
			UserID: c.UserID.String(), Nickname: c.Nickname, Tag: c.Tag,
		})
	}
	return out, nil
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

// priorityPermBit 频道覆盖里的 PrioritySpeaker 位（与 store.Permission 同值）。
// 只增删这一位，其余位原样保留（#45）。
const priorityPermBit uint64 = 1 << 4

// SetPrioritySpeaker 授予/撤销某成员的优先发言（#45，需 ManageChannels）。
// 读当前 member 覆盖 → 保留其他权限位、只增删 PrioritySpeaker → 空覆盖删除。
// 撤销只清 allow 位、不写 deny，角色可能已授予该权限，不能反向压制。
// 服务端权限不足时返回 API 错误（前端提示 403）。
func (a *App) SetPrioritySpeaker(channelID, userID string, enabled bool) error {
	if a.api == nil {
		return fmt.Errorf("not connected")
	}
	ows, err := a.api.ListChannelOverwrites(channelID)
	if err != nil {
		return err
	}
	var cur *api.ChannelOverwriteDTO
	for i := range ows {
		if ows[i].TargetType == "member" && ows[i].TargetID.String() == userID {
			cur = &ows[i]
			break
		}
	}
	var allow, deny uint64
	if cur != nil {
		if allow, err = parsePermBits(cur.Allow); err != nil {
			return err
		}
		if deny, err = parsePermBits(cur.Deny); err != nil {
			return err
		}
	}
	if enabled {
		allow |= priorityPermBit
	} else {
		// 撤销且无个人覆盖：角色授予保持原样，不必写一条 deny。
		if cur == nil {
			return nil
		}
		allow &^= priorityPermBit
	}
	if cur != nil && allow == 0 && deny == 0 {
		return a.api.DeleteChannelOverwrite(channelID, "member", userID)
	}
	_, err = a.api.SetChannelOverwrite(channelID, "member", userID,
		strconv.FormatUint(allow, 10), strconv.FormatUint(deny, 10))
	return err
}

// parsePermBits 十进制权限位串 → uint64；空串按 0（服务端输出可能省略 0）。
func parsePermBits(s string) (uint64, error) {
	if s == "" {
		return 0, nil
	}
	v, err := strconv.ParseUint(s, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("权限位格式非法: %q", s)
	}
	return v, nil
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
	a.hkMgr = hotkey.New(st)
	// Enabled=false 时 Start 不装钩子（返回 nil）——默认关闭，用户主动开；
	// 平台不支持时返回 ErrUnsupportedPlatform。两者都不影响可用性语义，
	// 见 GetHotkeyAvailability（只看 Available，不看 Start 的返回值）。
	startErr := a.hkMgr.Start()
	if startErr != nil {
		log.Printf("hotkey start: %v", startErr)
	}
	// 钩子没装上（平台不支持或安装失败）时 PTT 退化为常开：按键永远等不到，
	// 按 PTT 收门会把所有上行帧拦掉，服务端就收不到音频。
	a.hkGate.SetMode(hotkeyGateMode(st, startErr == nil && a.hkMgr.Available()))
	a.emitHotkeyAvailable()
	a.hkQuit = make(chan struct{})
	go a.hotkeyLoop()
}

// hotkeyGateMode 计算发送门实际应采用的门控模式。hooksAvailable 为 false 时
// （平台不支持、钩子安装失败或 Manager 未初始化）PTT 退化为常开：按键永远
// 等不到，门会恒拦。语音激活/常开不依赖钩子，保持原语义。
func hotkeyGateMode(st hotkey.Settings, hooksAvailable bool) hotkey.Mode {
	m := hotkey.EffectiveSendMode(st)
	if m == hotkey.ModePTT && !hooksAvailable {
		return hotkey.ModeOpen
	}
	return m
}

// sendGateMode 按当前平台能力计算发送门模式（设置热更新路径用）。
func (a *App) sendGateMode(st hotkey.Settings) hotkey.Mode {
	return hotkeyGateMode(st, a.hkMgr != nil && a.hkMgr.Available())
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
	case e.WhisperPress:
		a.whisperPress()
	case e.WhisperRelease:
		a.whisperRelease()
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

// ---------- 同频道悄悄话（#45） ----------

// SetWhisperTargets 更新悄悄话目标选择（UI 维护的同频道成员列表）。
// 未按住悄悄话键时只保存选择、不产生任何信令；按住期间变化立即生效：
// 目标清空会马上关旁路门并下发空数组取消，避免继续向全房广播。
func (a *App) SetWhisperTargets(targets []string) error {
	clean := make([]string, 0, len(targets))
	seen := make(map[string]bool, len(targets))
	for _, uid := range targets {
		if uid == "" || seen[uid] {
			continue
		}
		seen[uid] = true
		clean = append(clean, uid)
	}
	a.whisperMu.Lock()
	a.whisperTargets = clean
	held := a.whisperHeld
	wasActive := a.whisperActive
	active := held && len(clean) > 0
	a.whisperActive = active
	a.whisperMu.Unlock()
	if !held {
		return nil
	}
	a.setWhisperGate(active)
	if active || wasActive {
		a.sendWhisperTargets(clean)
	}
	a.emitWhisperState(true, active)
	return nil
}

// whisperPress 按住悄悄话键：有目标且在语音房内才下发并打开发送门；无目标
// 或不在房时按键无效（不开麦、不发信令），也不影响 PTT 的既有状态。
func (a *App) whisperPress() {
	a.mu.Lock()
	s := a.sess
	a.mu.Unlock()
	// 已登录但不在语音房：按住悄悄话无对象可发（会话为 nil 时无媒体链路，
	// 这里放行也只会动一个没有管线的门，便于单元测试直接验证门组合）。
	if s != nil && !s.InRoom() {
		a.emitWhisperState(true, false)
		return
	}
	a.whisperMu.Lock()
	a.whisperHeld = true
	targets := append([]string(nil), a.whisperTargets...)
	active := len(targets) > 0
	a.whisperActive = active
	a.whisperMu.Unlock()
	if active {
		a.setWhisperGate(true)
		a.sendWhisperTargets(targets)
	}
	a.emitWhisperState(true, active)
}

// whisperRelease 松开悄悄话键：关旁路门并下发空数组取消。旁路门与 PTT 的
// pressed/tail 状态分离，故「先松 PTT 再松悄悄话」或相反都不会误关麦。
func (a *App) whisperRelease() {
	a.whisperMu.Lock()
	a.whisperHeld = false
	wasActive := a.whisperActive
	a.whisperActive = false
	a.whisperMu.Unlock()
	a.setWhisperGate(false)
	if wasActive {
		a.sendWhisperTargets(nil)
	}
	a.emitWhisperState(false, false)
}

// clearWhisper 退房/断线/被抢占时清理悄悄话状态（#45）：关本地旁路门并让会话
// 丢掉缓存的目标，但不发 voice.whisper——SFU peer 会随离房销毁，且此刻发送的
// 取消消息可能被信令层误归属成进房失败（#91 的 pending join 错误归属窗口）。
// 切房后新房间的 channel.joined 会强制同步一次空目标，清掉服务端残留状态。
func (a *App) clearWhisper() {
	a.whisperMu.Lock()
	was := a.whisperHeld || a.whisperActive
	a.whisperHeld = false
	a.whisperActive = false
	a.whisperTargets = nil
	a.whisperMu.Unlock()
	a.setWhisperGate(false)
	a.mu.Lock()
	s := a.sess
	a.mu.Unlock()
	if s != nil {
		s.ClearWhisperTargets()
	}
	if was {
		a.emitWhisperState(false, false)
	}
}

// setWhisperGate 设置发送门旁路位（hkGate 在 ServiceStartup 前为 nil，测试）。
func (a *App) setWhisperGate(on bool) {
	if a.hkGate != nil {
		a.hkGate.Force(on)
	}
}

// sendWhisperTargets 非阻塞交给会话专用 goroutine 下发（协议 <3 时由会话丢弃）。
func (a *App) sendWhisperTargets(targets []string) {
	a.mu.Lock()
	s := a.sess
	a.mu.Unlock()
	if s != nil {
		s.SetWhisperTargets(targets)
	}
}

// emitWhisperState 推给前端做轻量指示（pressed=按住，active=有目标且已生效）。
func (a *App) emitWhisperState(pressed, active bool) {
	a.emit("hotkey:whisper", map[string]any{"pressed": pressed, "active": active})
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
	// 只存值，等 Start 生效）。发送门先按新设置收敛：钩子装不上时 PTT 必须落在
	// 常开，不能停在会拦掉全部上行的 PTT。
	if a.hkGate != nil {
		a.hkGate.SetMode(a.sendGateMode(clean))
	}
	if a.hkMgr != nil {
		if err := a.hkMgr.SetSettings(clean); err != nil {
			return err
		}
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
	return a.SendChatWithMeta(channelID, content, "", nil)
}

// SendChatWithMeta 发送带回复与 @提及的消息（#39）：replyTo 空串 = 普通消息，
// mentions 为空 = 无提及。断线时连同 nonce 入队，重连重放不丢字段。
func (a *App) SendChatWithMeta(channelID, content, replyTo string, mentions []string) error {
	return a.SendChatWithAttachments(channelID, content, replyTo, mentions, nil)
}

// SendChatWithAttachments 发送带附件 ID 的消息（#41）：content 可为空（纯附件
// 消息）；未连接时与 SendChat 同口径报错。附件 ID 随离线队列重放。
func (a *App) SendChatWithAttachments(channelID, content, replyTo string, mentions, attachmentIDs []string) error {
	a.mu.Lock()
	s := a.sess
	a.mu.Unlock()
	if s == nil {
		return fmt.Errorf("not connected")
	}
	return s.SendChatWithAttachments(channelID, content, replyTo, mentions, attachmentIDs)
}

// 附件选择的上限（与服务端一致）与本地读入上限。服务端真实限制是 10MiB，
// 这里放宽到 50MiB 只为在 os.ReadFile 之前拦住误选的超大文件。
const (
	maxAttachmentsPerMessage = 10
	maxAttachmentReadBytes   = 50 << 20
)

// PickAndUploadAttachments 打开系统文件选择器，逐个上传附件并返回绝对 URL 的
// 元数据。取消返回空切片；超过 10 个或单文件超过 50MiB 在读取前直接报错。
func (a *App) PickAndUploadAttachments() ([]api.AttachmentDTO, error) {
	if a.wails == nil {
		return nil, fmt.Errorf("文件选择不可用：桌面环境未就绪")
	}
	if a.api == nil {
		return nil, fmt.Errorf("not connected")
	}
	paths, err := a.wails.Dialog.OpenFile().
		SetTitle("选择附件").
		AddFilter("图片", "*.png;*.jpg;*.jpeg;*.gif;*.webp").
		AddFilter("文档", "*.pdf;*.txt;*.zip").
		PromptForMultipleSelection()
	if err != nil {
		return nil, fmt.Errorf("打开文件选择器失败: %w", err)
	}
	return a.uploadAttachmentPaths(paths)
}

// uploadAttachmentPaths 校验并上传已选中的本地文件（从对话框逻辑拆出便于测试）。
func (a *App) uploadAttachmentPaths(paths []string) ([]api.AttachmentDTO, error) {
	if a.api == nil {
		return nil, fmt.Errorf("not connected")
	}
	files := make([]string, 0, len(paths))
	for _, p := range paths {
		if strings.TrimSpace(p) != "" {
			files = append(files, p)
		}
	}
	// 取消选择：原生对话框返回空列表。
	if len(files) == 0 {
		return []api.AttachmentDTO{}, nil
	}
	if len(files) > maxAttachmentsPerMessage {
		return nil, fmt.Errorf("一次最多选择 %d 个附件", maxAttachmentsPerMessage)
	}
	// 先整体校验再逐个上传：避免部分文件已上传后才发现某个超限。
	for _, p := range files {
		info, err := os.Stat(p)
		if err != nil {
			return nil, fmt.Errorf("读取附件失败: %w", err)
		}
		if info.IsDir() {
			return nil, fmt.Errorf("不能上传文件夹")
		}
		if info.Size() > maxAttachmentReadBytes {
			return nil, fmt.Errorf("附件 %s 超过 50MiB", filepath.Base(p))
		}
	}
	out := make([]api.AttachmentDTO, 0, len(files))
	for _, p := range files {
		data, err := os.ReadFile(p)
		if err != nil {
			return nil, fmt.Errorf("读取附件失败: %w", err)
		}
		att, err := a.api.UploadAttachment(filepath.Base(p), data)
		if err != nil {
			return nil, err
		}
		// 服务端返回相对能力 URL，立即拼成绝对地址交给前端。
		att.URL = a.api.ResolveURL(att.URL)
		att.ThumbnailURL = a.api.ResolveURL(att.ThumbnailURL)
		out = append(out, *att)
	}
	return out, nil
}

// OpenAttachment 用系统默认浏览器打开附件（仅 http/https，拒绝 file/javascript
// 等本地或脚本 scheme）。url 必须是 chatMessageView 产出过的绝对地址。
func (a *App) OpenAttachment(rawURL string) error {
	u, err := url.Parse(strings.TrimSpace(rawURL))
	if err != nil || u.Host == "" {
		return fmt.Errorf("附件地址无效")
	}
	if !strings.EqualFold(u.Scheme, "http") && !strings.EqualFold(u.Scheme, "https") {
		return fmt.Errorf("仅支持打开 http/https 附件")
	}
	if a.wails == nil {
		return fmt.Errorf("浏览器不可用：桌面环境未就绪")
	}
	return a.wails.Browser.OpenURL(u.String())
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
