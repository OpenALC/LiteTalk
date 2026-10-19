// 轻音 LiteTalk 客户端入口（Wails v3，锁定 v3.0.0-beta.30）
package main

import (
	"embed"
	"log"

	"github.com/wailsapp/wails/v3/pkg/application"
)

// 嵌入前端构建产物（CI 中须先 pnpm run build 再执行 go 命令）
//
//go:embed all:frontend/dist
var assets embed.FS

// 应用/窗口图标（任务栏、托盘、关于框共用；client/build/windows/icon.ico）
//
//go:embed build/windows/icon.ico
var appIcon []byte

func main() {
	app := NewApp()

	wailsApp := application.New(application.Options{
		Name:        "轻音 LiteTalk",
		Description: "开源自建的桌面语音聊天",
		Icon:        appIcon,
		// 单实例：重复启动不在 New 里再建应用——wails 通知首实例（回调
		// OnSecondInstanceLaunch）后以 ExitCode 0 退出进程。UniqueID 全局
		// 唯一，写反向域名。
		SingleInstance: &application.SingleInstanceOptions{
			UniqueID: "io.github.openalc.LiteTalk",
			OnSecondInstanceLaunch: func(application.SecondInstanceData) {
				app.ShowMainWindow()
			},
		},
		Services: []application.Service{
			application.NewService(app),
		},
		Assets: application.AssetOptions{
			// BundledAssetFileServer 同时服务前端产物与 /wails/runtime.js
			//（v3 前端 IPC 桥，production 构建标签选择正式版 runtime.js）
			Handler: application.BundledAssetFileServer(assets),
		},
		Windows: application.WindowsOptions{
			// 托盘应用语义：destroy 休眠模式销毁唯一窗口后进程必须存活（托盘唤醒重建）。
			// v3 默认「最后一窗关闭即退进程」会绕过托盘把进程杀掉，需禁用并自行管理退出：
			// interceptClose 放行路径（关闭即退出/托盘不可用时托盘不可用）显式调 Quit。
			DisableQuitOnLastWindowClosed: true,
		},
		Linux: application.LinuxOptions{
			// 与 Windows 同理：destroy 休眠模式销毁唯一窗口后须由托盘唤醒重建。
			DisableQuitOnLastWindowClosed: true,
			// 不设时 Wails 会推导出 org.wails.litetalk，与 .desktop 的
			// StartupWMClass 对不上（任务栏通用图标/重复条目）。GTK 同时把
			// 它作为 Wayland 的 app_id 与 g_set_prgname，须与 .desktop 文件名
			// io.github.openalc.LiteTalk.desktop 完全一致。
			ApplicationID: "io.github.openalc.LiteTalk",
		},
		Mac: application.MacOptions{
			// macOS 语义相反：该字段为 true 时最后一窗关闭才终止进程，
			// v3 默认 false（不终止）。显式写死防上游默认值漂移——
			// destroy 模式同样依赖关窗后进程存活。
			ApplicationShouldTerminateAfterLastWindowClosed: false,
		},
	})

	// 窗口创建与生命周期钩子注册在 App.buildWindow（托盘唤醒重建时复用）。
	// buildWindow 依赖 a.wails——Bind 先注入 wails 引用（window 先给 nil），
	// 建好窗口后再 Bind 一次补上。
	app.Bind(wailsApp, nil)
	window := app.buildWindow(application.WebviewWindowOptions{
		Title:            "轻音 LiteTalk",
		Width:            1080,
		Height:           680,
		MinWidth:         960,
		MinHeight:        600,
		Frameless:        true, // 无边框窗口 + 前端自定义标题栏（--wails-draggable 拖拽）
		BackgroundColour: application.NewRGB(255, 255, 255),
	})
	app.Bind(wailsApp, window)

	if err := wailsApp.Run(); err != nil {
		log.Fatal("启动失败:", err)
	}
}
