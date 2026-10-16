// 轻音 LiteTalk 客户端入口（Wails v3，锁定 v3.0.0-beta.30）
package main

import (
	"embed"
	"log"

	"github.com/wailsapp/wails/v3/pkg/application"
	"github.com/wailsapp/wails/v3/pkg/events"
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
		Services: []application.Service{
			application.NewService(app),
		},
		Assets: application.AssetOptions{
			// BundledAssetFileServer 同时服务前端产物与 /wails/runtime.js
			//（v3 前端 IPC 桥，production 构建标签选择正式版 runtime.js）
			Handler: application.BundledAssetFileServer(assets),
		},
	})

	window := wailsApp.Window.NewWithOptions(application.WebviewWindowOptions{
		Title:            "轻音 LiteTalk",
		Width:            1080,
		Height:           680,
		MinWidth:         1080,
		MinHeight:        680,
		Frameless:        true, // 无边框窗口 + 前端自定义标题栏（--wails-draggable 拖拽）
		BackgroundColour: application.NewRGB(255, 255, 255),
	})
	app.Bind(wailsApp, window)

	// 关窗拦截（替代 v2 OnBeforeClose）：默认隐藏到托盘而非退出。
	// Hook 先于 v3 内建关闭处理器执行，Cancel 即中止关闭（见 app.interceptClose）。
	window.RegisterHook(events.Common.WindowClosing, func(e *application.WindowEvent) {
		if app.interceptClose() {
			e.Cancel()
		}
	})

	if err := wailsApp.Run(); err != nil {
		log.Fatal("启动失败:", err)
	}
}
