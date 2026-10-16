# 轻音 LiteTalk

轻量的桌面语音联机应用：进频道即聊，服务端只做 SFU 转发，不混音不转码，自托管一台 2 核 2G 小服务器就够。

## 特性

- **多人语音**：频道内最多 10 人同聊（可配置），WebRTC/SRTP 加密传输，Opus 编解码
- **降噪**：客户端 RNNoise 降噪 + 语音检测，说话才发包
- **跨平台**：Windows / Linux 桌面端（macOS 产物为实验性，非阻塞发布）
- **自托管**：单二进制服务端 + MySQL/Redis/coturn，一份部署手册照抄即可
- **开源可复用**：Auth 支持 full/simple 双模式；验证码邮件可对接任意自有发码平台（Webhook 契约）或 SMTP

## 界面

三栏布局：服务器/频道栏 · 语音房间（声波球卡片）· 好友列表。支持静音/耳聋/独立音量、
文字频道聊天、系统托盘后台挂机、最小化休眠省内存。

```
┌────────┬──────────────┬───────────────────────────┬────────────┐
│ 服务器 │ 频道树        │ 语音房间（成员声波球卡片）  │ 好友列表   │
│  轨道  │ 文字/语音频道 │ 文字聊天 / 语音房间 切换    │ 在线状态   │
│        │ 用户面板     │ 浮动通话卡片（时长/挂断）   │ 搜索       │
└────────┴──────────────┴───────────────────────────┴────────────┘
```

## 快速开始

### 桌面客户端

到 [Releases](https://github.com/OpenALC/LiteTalk/releases) 下载：

| 平台 | 产物 |
|---|---|
| Windows | `LiteTalk-windows-amd64-setup.exe`（安装器）/ `LiteTalk-windows-amd64-portable.zip`（免安装） |
| Linux amd64 | `litetalk_<版本>_amd64.deb` / `litetalk_<版本>_amd64.rpm` / `LiteTalk-linux-amd64-portable.tar.gz` |
| Linux arm64 | 同上（arm64） |
| macOS | `LiteTalk-macos-arm64.zip`（实验性） |

登录界面支持注册 / 找回密码；私域部署可开 simple 模式免验证码（见下）。

### 服务端

见 **[server/deploy/README.md](server/deploy/README.md)**：端口、MySQL/Redis/coturn/Nginx/systemd
全套配置文件与验证清单，RockyLinux 9.5 按步骤照抄即可。

## 配置速查

| 项 | 方式 |
|---|---|
| Auth 模式 | `LITETALK_AUTH_MODE=full`（验证码流程，公网推荐）/ `simple`（免验证码，仅私域） |
| 验证码邮件 | 三选一：**Webhook**（`LITETALK_MAILER_WEBHOOK`，对接自有发码平台，契约见 [server/README.md](server/README.md)）/ **SMTP**（`LITETALK_SMTP_*`）/ **Console**（默认，打印到日志） |
| 全部环境变量 | [server/deploy/env/litetalk.env.example](server/deploy/env/litetalk.env.example) |

## 开发

前置：Go 1.27+、Node 22+、pnpm。仓库结构：

- `client/` Wails v3 桌面客户端（`frontend/` 为 Vue 3 + TS 前端）
- `server/` Go 服务端（REST + WS 信令 + 自研 Pion SFU）

```bash
# 服务端
cd server && go run ./cmd/server

# 客户端：前端 watch 构建 + Go 侧 FRONTEND_DEVSERVER_URL 指向 vite
cd client/frontend && pnpm install && pnpm run dev
cd client && set FRONTEND_DEVSERVER_URL=http://localhost:5173 && go run .
```

国内开发建议先配镜像：`go env -w GOPROXY=https://goproxy.cn,direct`（前端镜像已内置）。

CI：push / PR 触发检查（gofmt、vet、`test -race`、前端类型检查与单测，Linux + Windows 双平台）；推送 `v*` 标签自动全平台打包并发布 GitHub Release（附 SHA256SUMS，macOS 为实验性非阻塞）。
开发流程、提交规范与门禁细节见 [CLAUDE.md](CLAUDE.md)。

## License

[MIT](LICENSE)
