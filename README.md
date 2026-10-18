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

### 最低支持发行版

Linux 产物以 **Ubuntu 24.04**（glibc 2.39）为构建基线，CI 校验产物不引用更高版本的 glibc 符号；
运行环境需提供 WebKitGTK 4.1 与 libopus（deb/rpm 已声明依赖）。中文界面依赖系统 CJK 字体，
最小化安装建议补装 `fonts-noto-cjk`，否则界面文字可能显示为方框。

### 服务端

**三步启动**（干净机器）：

1. **取得程序**：Releases 下载 `litetalk-server-linux-amd64.tar.gz` 解压；或克隆仓库后 `docker compose up -d`（compose 自行构建，见 [docker-compose.yml](docker-compose.yml)）。
2. **一条命令起服**：`./litetalk-server --domain voice.example.com`；compose 场景等价于
   `LITETALK_JWT_SECRET=$(openssl rand -hex 32) LITETALK_DOMAIN=voice.example.com docker compose up -d`。
3. **认领实例**：把终端打印的管理员邀请码（compose 看 `docker logs litetalk`）填进客户端「注册」页，即可创建你的服务器并成为其所有者。

`--domain` 一条命令完成证书签发、建库、生成邀请码，并解析域名推导 SFU 宣告地址——**不用再设 `LITETALK_SFU_IP`**。
只有不用 `--domain` 或域名解析失败时才要手填（解析失败会直接报错并提示设置），生产构建对环回地址始终拒绝。
邀请码可随时 `./litetalk-server --print-admin-invite` 重打。

前提：域名 A 记录指向服务器、80/443 可达；绑定 80/443 需 root 或 `CAP_NET_BIND_SERVICE`
（compose 已配 `net.ipv4.ip_unprivileged_port_start=0`）。数据落在 `$LITETALK_DATA_DIR`（默认 `./data`，compose 为 `/data`）。

自建数据库/反代/Nginx/systemd 全套配置见 **[server/deploy/README.md](server/deploy/README.md)**：
端口、MySQL/Redis/coturn 可选扩展与验证清单，RockyLinux 9.5 按步骤照抄即可。

TLS 三种模式（详见部署手册）：

| 模式 | 做法 | 说明 |
|---|---|---|
| 内置 ACME 自动签发 | `./litetalk-server --domain voice.example.com` | 443 + 80 双监听自动申请/续期证书，免 nginx；443 需 root 或 `CAP_NET_BIND_SERVICE` |
| 前置反代 | `./litetalk-server --behind-proxy` + nginx/Caddy | 反代终结 TLS，服务端纯 HTTP；配 `LITETALK_TRUSTED_PROXIES` 后才采信 `X-Real-IP` |
| 纯 HTTP | 都不设 | 仅开发/内网，启动日志会提示 |

证书缓存默认在 `$LITETALK_DATA_DIR/autocert`（0700），自动续期无需 cron。

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
