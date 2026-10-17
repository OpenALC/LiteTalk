# LiteTalk 客户端

Go + Wails v3 桌面端：音频采集/降噪/Opus 编解码、WebRTC 通话、Vue 3 界面。

## 快捷键

| 功能 | 组合 |
|---|---|
| 静音 / 取消静音 | `Ctrl + M` |
| 耳聋 / 取消耳聋 | `Ctrl + D` |
| 全关 / 全开（静音+耳聋） | `Ctrl + Shift + M` |
| 聚焦好友搜索 | `Ctrl + K` |
| 打开设置 | `Ctrl + ,` |

输入框内不触发快捷键；设置页「快捷键」列表与上表同步。

## 系统托盘

- **显示主界面**：唤起窗口（最小化时同时还原）
- **静音**：勾选态实时同步当前静音状态
- **退出**：真正退出应用（关闭按钮只隐藏到托盘，不退出）

托盘为 Wails v3 内建 SystemTray：Windows 用 ICO 图标，Linux（StatusNotifierItem）/macOS 用 PNG 图标（见 internal/tray）。

## 最小化休眠（省内存）

设置 → 常规 →「最小化时休眠界面」，默认关闭。开启后最小化或关闭到托盘时：

- 前端切换休眠态：卸载三栏业务组件、解除事件订阅、停止定时器
- 语音连接由 Go 后端保活，不受影响
- 托盘唤起或窗口重新获得焦点时自动恢复

## 音频设备热切换

设置 → 音频设备下拉即时生效：设备流重启，编解码/混音/通话状态保留。
设备失效时自动回退系统默认设备。

## 服务器地址配置

设置 → 连接：填服务器地址（如 `https://your-domain.com`）→ 测试连接 → 保存后重启生效；WS 信令地址自动推导。
环境变量 `LITETALK_API` / `LITETALK_WS` 优先级最高（覆盖配置文件）；持久化在 `os.UserConfigDir()/LiteTalk/config.json`（Windows 即 `%APPDATA%\LiteTalk\config.json`）。

## 开发

```powershell
cd client
go test ./...            # 单元测试
cd frontend
pnpm install
pnpm test                # 前端单测（vitest）
pnpm run build           # 构建到 dist/（go:embed 依赖）
pnpm run type-check
```

### wails3 dev（热重载开发模式）

脚手架在 `Taskfile.yml` + `build/config.yml`（CI 打包不依赖 wails CLI，仍走裸 `go build`，见 `.github/workflows/ci.yml`）。

```bash
# Git Bash；cgo（libopus/malgo/WASAPI）依赖 mingw64 gcc
export PATH="/c/tools/mingw64/bin:$PATH"
export CGO_ENABLED=1
cd client
wails3 dev               # 需 wails v3 CLI（go install github.com/wailsapp/wails/v3/cmd/wails3）
```

行为：vite dev server（端口 9245，`WAILS_VITE_PORT` 可改）+ 监视 `*.go` 变更自动重建重启；前端 HMR。dev 二进制输出到 `build/bin/`（已 gitignore）。

注意：
- 系统里的 `wails`（v2 CLI）不兼容 v3 项目，必须用 `wails3`；
- vite 显式绑 `127.0.0.1`：wails v3 Windows 的 asset 代理地址是 `wails.localhost`（解析 IPv4），vite 默认只绑 IPv6 `[::1]` 会被拒连；
- Linux 构建走 `-tags gtk3`（对应 WebKitGTK 4.1，勿用默认 GTK4）。

联调 harness：`go run ./cmd/calltest --help`（入房通话）、`go run ./cmd/audioloop --help`（音频回环）。
