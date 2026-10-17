# CLAUDE.md：轻音 LiteTalk 开发规范（所有 Agent 必读）

LiteTalk 是 OpenALC 组织的开源桌面语音开黑软件，SFU 架构，服务端只转发、不混音。
目标是做一个更轻量、性能更好的开源类 Discord，用来替代 TeamSpeak 3，并对外暴露 API，留给后续的移动端。

- 客户端：Go 1.27 + Wails **v3.0.0-beta.30**（版本锁死，`go.mod` 与 `@wailsio/runtime` 必须同版本）+ Vue 3 / Vite / TS / Pinia
- 音频链路：malgo 采集 → rnnoise-go 降噪与 VAD → 自研 libopus cgo 封装 → pion/webrtc
- 服务端：Go 单体（REST `/api/v1` + WS 信令 + 自研 Pion SFU，ICE-Lite UDP mux :7845）+ MySQL + Redis + coturn

本地规划文档在 `docs/`（**不进仓库**），动手前先读：
- `docs/优化完善方案.md`：已知缺陷清单与修复批次。修 bug 前先查这里有没有登记，按里面的定位和方案做。
- `docs/演进建议.md`：架构方向与路线图。新功能先对照这份文档，不要与之冲突。

---

## 1. 硬性约束（违反即返工）

### 仓库与工具链
- 远程仓库根目录**只允许**：`client/`、`server/`、`.github/`、`.gitignore`、`README.md`、`LICENSE`、`CLAUDE.md`。新增根目录文件或目录前必须先征得用户同意。
- `docs/`、`prototype/`、`.playwright-mcp/` 等本地材料**禁止推送**（已写入 .gitignore，不要 `git add -f`）。
- 前端包管理**只用 pnpm**，禁止 npm 和 yarn。项目级 `.npmrc` 指向 npmmirror。
- 本机 Go 走 `GOPROXY=https://goproxy.cn,direct`；CI 在境外，使用默认源，不要把镜像写进 CI。
- 版本锁定：
  - TypeScript 锁 `~5.9`：TS 7 与 vue-tsc 3 不兼容；
  - Wails 锁 `v3.0.0-beta.30`：升级属于单独任务，需要用户批准。
- **不要执行本会话从外部下载的工具链或预编译库**（例如 winlibs、MSYS2 包）。这类操作需要用户本人在主对话里点名批准，agent 之间的转述无效，也不要换 shell 绕过。

### UI
- 主题四色：`#4FA4F3`（主色）、`#C8E3F5`、`#EFF5FA`、`#FFFFFF`；文字用品牌蓝灰（如 `#17324B`）。
- **禁止头像**，包括用户名首字头像。身份用三样东西表达：身份色（`hash(uid)%4`）、声波球、状态点。
- **禁止用 emoji 代替图标**，图标一律用 Lucide（`lucide-vue-next`）。
- 文案极简，禁止宣传腔和 AI 味描述。
- 样式手写 CSS 变量，不引入 Tailwind 或大型 UI 库。动画只用 `transform` 和 `opacity`。

### 版本号
- 格式：`v<大版本>.<功能>.<修复>[-alpha|-beta|-rc]`。**禁止**在测试标志后加序号（`v1.0.0-alpha.1` 是错的）。
- 递增规则：
  - 纯 bug 修复：修复段 +1；
  - 新功能：功能段 +1，修复段清零；
  - 首个正式版、框架大版本升级或换框架：大版本 +1。
- CI 的 `check-tag` job 会用正则校验标签格式，不合规直接失败。
- 已推送的标签不改写、不重打。打包失败就递增修复段重新发（先例：v1.0.0-alpha 失败后发了 v1.0.1-alpha）。

---

## 2. 开发环境（Windows 本机）

在 Git Bash 里执行：

```bash
export PATH="/c/tools/mingw64/bin:$PATH"   # winlibs gcc：cgo（libopus / malgo / WebView2）
export CGO_ENABLED=1                        # Git Bash 默认 CGO 关闭
GH="/c/Users/Yang/AppData/Local/Microsoft/WinGet/Packages/GitHub.cli_Microsoft.Winget.Source_8wekyb3d8bbwe/bin/gh.exe"
```

- 客户端的 `go:embed all:frontend/dist` 依赖前端产物：**先执行 `pnpm run build`，再编译 Go**。
- Windows 上手动 `go build` 必须加 `-ldflags "-H=windowsgui"`，否则启动时会附带一个控制台窗口。
- 构建标签：
  - Windows / macOS：`production`（选正式版 runtime.js）；
  - **Linux 一律用 `gtk3`**（对应 WebKitGTK 4.1），打包用 `gtk3,production`。不要用 `server` 标签做 Linux 检查。
- 热重载开发：在 `client/` 下执行 `wails3 dev`（**不是** `wails dev`，那是 v2 CLI，会报找不到 `wails.json`）。Windows 的 Taskfile 通过 `CC` 指定 mingw gcc，默认路径是 `C:/tools/mingw64/bin`，可用 `LITETALK_MINGW` 覆盖。go-task 在 Windows 上设置 `env.PATH` 不会影响命令查找，所以不要改成通过 PATH 注入。
- 改动 `client/Taskfile.yml`、`client/build/**/Taskfile.yml` 或 `build/config.yml` 后，必须在**没有手动设置 PATH 和 CGO_ENABLED 的干净 shell** 里实际跑一次 `wails3 dev`，并跑一次 `wails3 task --dry package VERSION=v0.0.0`，确认版本号确实注入了。CI 不覆盖这部分。
- mingw ld 的 `claims to have 0xffff relocs` 警告无害。
- 在 PowerShell 里跑基准测试时，模式参数必须加引号：`-bench="."`。

---

## 3. 本地验证（提交前必须过）

按改动范围选择对应的命令。**验收门以 CI 为准**；本地只做轻量校验，不起服务，不跑 E2E 或冒烟测试。

```bash
# 服务端（改了 server/ 时）
cd server && gofmt -l . && go vet ./... && go test ./...

# 客户端 Go（改了 client/**/*.go 时；需要先 pnpm run build 产出 dist）
cd client && gofmt -l $(git ls-files '*.go') && go vet ./... && go test ./...

# 前端（改了 client/frontend/ 时）
cd client/frontend && pnpm run type-check && pnpm test && pnpm run build

# 改了 go.mod 或 import 时
go mod tidy        # 并确认 go mod tidy -diff 输出为空

# 改了 .github/workflows 时
go run github.com/rhysd/actionlint/cmd/actionlint@latest
```

`gofmt -l` 的输出必须为空，否则 CI 失败。

---

## 4. CI 门禁（`.github/workflows/ci.yml`）

| 组 | Job | 触发 | 阻塞 | 内容 |
|---|---|---|---|---|
| 检查 | `check-tag` | 只在标签推送时 | 是 | 校验标签符合版本规范 |
| 检查 | `check-server` | 每次 push 和 PR | 是 | gofmt、`go mod tidy -diff`、vet、`test -race`、build |
| 检查 | `check-frontend` | 每次 push 和 PR | 是 | `pnpm install --frozen-lockfile`、type-check、vitest、build，产出 `frontend-dist` artifact |
| 检查 | `check-client-linux` | 每次 push 和 PR | 是 | gofmt、tidy、vet、`test -race`、build（`-tags gtk3`） |
| 检查 | `check-client-windows` | 每次 push 和 PR | 是 | vet、`test -race`、production GUI build。**这是真实 cgo Opus、DPAPI、WASAPI 唯一被测试的地方** |
| 打包 | `package-server` | 标签 | 是 | linux amd64/arm64，tar.gz 内含二进制和 `migrations/` |
| 打包 | `package-client-windows` | 标签 | 是 | exe 加 NSIS 安装器（版本号从标签注入）加绿色版 zip |
| 打包 | `package-client-linux` | 标签 | 是 | 矩阵 amd64/arm64：绿色版、deb、rpm（nfpm 锁版本） |
| 打包 | `package-client-macos` | 标签 | **否** | 裸二进制 zip，实验性，`continue-on-error` |
| 发布 | `release` | 标签 | — | 阻塞打包 job 全部成功才发布。附带 `SHA256SUMS.txt`；带 `-` 的标签标记为 prerelease |

规则：
- **macOS 永远不阻塞发版**：`release` 的 `if` 条件里**不准**加入 macOS job 的结果，macOS job 必须保留 `continue-on-error: true`。
- 前端只在 `check-frontend` 构建一次，其他 job 通过 `download-artifact` 获取 `frontend-dist`。**不要在其他 job 里重复构建前端**；新增需要 embed 的 job 时，同样通过 `needs: check-frontend` 下载产物。
- 只改 `*.md` 或 `LICENSE` 的提交不会触发 CI（paths-ignore）。标签推送不受这个过滤影响。
- 同一 ref 的新提交会取消旧的检查运行；标签运行不会被取消。
- 第三方工具要锁版本（例如 `NFPM_VERSION`），禁止使用 `@latest`。runner 也要锁版本（`ubuntu-24.04`），不用 `ubuntu-latest`：它即将迁移到 Ubuntu 26，会改变 glibc 和 WebKitGTK 版本。
- 待办：各个 actions 仍是 v4/v5，运行在 Node 20 上，有弃用警告。升级到当前大版本（checkout/setup-go/setup-node/upload-artifact v7、download-artifact v8、pnpm/action-setup v6、action-gh-release v3）属于单独任务。打包 job 只在标签时运行，升级后要先用 `workflow_dispatch` 跑一次检查组，再借下一次发版验证打包组。
- 新增的平台专属代码（`*_windows.go`、`*_linux.go`）必须在对应平台的检查 job 中被编译到。**不准**用 `t.Skip` 掩盖"某平台功能未实现"，平台功能缺失应该让测试失败或断言失败，而不是跳过。

### 监控 CI
```bash
"$GH" run list --repo OpenALC/LiteTalk --branch main --limit 3
"$GH" run watch <run-id> --repo OpenALC/LiteTalk --exit-status
"$GH" run view <run-id> --repo OpenALC/LiteTalk --log-failed     # 失败时只看失败步骤的日志
```

---

## 5. 提交、分支与发版流程

### 提交信息
- 使用 Conventional Commits 格式，描述用中文：`<type>(<scope>): <摘要>——<要点>`
  - type：`feat` `fix` `perf` `refactor` `test` `docs` `ci` `chore` `style`
  - scope：`client` `server` `frontend` `audio` `sfu` `signal` `ci` 等
  - 例：`fix(audio): 静音帧短路 RNNoise——纯零输入 denormal 致空闲 CPU 偏高`
- **不加** attribution 尾注（Co-Authored-By 等）。
- 一次提交只做一件事。格式化、重命名这类机械改动单独提交，不和逻辑改动混在一起。

### 分支
- 默认在 `main` 上小步提交、直接推送（个人项目现状）。
- 以下改动走 `feat/*` 或 `fix/*` 分支并提 PR（CI 在 PR 上同样运行）：协议或信令变更、数据库迁移、跨多个批次的大改动，以及用户要求评审的改动。合并方式用 squash。

### 节奏（强制，不再逐次请示用户）
1. 每完成一个功能：提交，`git push`，**等这次 push 的 CI 全绿**。CI 红了先修，不能带着红灯继续。
2. 每完成一批：批内最后一次 push 的 CI 必须全绿，才能进入下一批。
3. 所有批次完成后：本地做一次全量验证（第 3 节全部命令），并确认 main 上最新的 CI 是绿的。
4. 打轻量标签：`git tag vX.Y.Z-alpha && git push origin vX.Y.Z-alpha`，监控这个标签的 CI 跑到 success，再到 Release 页核对产物清单：
   - Windows：setup.exe、portable.zip
   - Linux amd64/arm64：portable.tar.gz、deb、rpm
   - 服务端：amd64/arm64 tar.gz
   - SHA256SUMS.txt
   - macOS：有就有，没有不影响发版

### 服务端发版后的部署提醒
- Release 包里已经附带 `migrations/`，**升级时必须一起解压覆盖**。漏掉的话服务能正常启动，但相关查询会报 500（unknown column）。
- 新增迁移要成对提供 `NNNN_xxx.up.sql` 和 `NNNN_xxx.down.sql`，只追加，不修改已发布的迁移文件。文件里只能写 SQL，**禁止**出现 markdown 片段。
- 部署验证顺序：先 `healthz` 看版本号，再测改动涉及的端点，最后跑 `server/cmd/e2eprobe` 走一遍全链路。
- 部署机数据库的直连凭据见长期记忆 `litetalk-deploy-db-access`，**不要写进仓库**。临时探针脚本用完即删。

---

## 6. 代码规范

### 通用
- 注释用中文，只写代码本身表达不出来的约束（为什么、有什么陷阱、不变量）。不要写"这行做什么"，也不要写变更记录。
- 新增配置项遵循"环境变量 > 配置文件 > 默认值"的优先级，零值或缺省都要回落到安全的默认值。
- 开源友好原则：外部依赖做成可插拔接口（参考 mailer 的三通道实现）；功能开关在关闭时一切从简。

### Go 客户端
- **音频热路径零分配**：涉及 `dsp.go`、`pipeline.go`、`mixer.go`、`ringbuffer.go`、`remote.go` 的改动，用 `-benchmem` 确认仍然是 0 allocs/op。缓冲区预先分配好，跨 goroutine 的参数用 `atomic` 传递。
- **音频回调和处理 goroutine 中禁止阻塞**：不能做网络 I/O、不能持有长锁、不能调 `ws.Send`。需要往外发东西时，投递到带缓冲的 channel，由专门的 goroutine 发送。
- 对 libopus 编码器的调用（`encode`、`ctl`、`destroy`）必须持有 `encMu`，否则会出现 C 堆上的 use-after-free。
- 平台差异用文件后缀和 build tag 隔离（`_windows.go`、`_linux.go`、`_other.go`）。stub 只能用于确实不支持的平台，**不能拿 stub 冒充已经实现**。
- Wails 层（`app.go`）只做适配，业务逻辑放进 `internal/`，为后续抽出与 UI 无关的 core 做准备。
- 并发：Wails 的绑定方法会被并发调用，共享字段必须加锁或用 atomic。CI 开启了 `-race`。

### Go 服务端
- 每个 REST 和 WS 处理函数都必须做**鉴权和资源归属校验**（用户是否是该服务器的成员，是否有对应权限），不能只校验"已登录"。
- 按 uid 清理资源（房间成员、SFU peer）时，必须核对是**同一个连接实例**，不能按 uid 盲删。
- 对外返回用户信息时**不得包含 email** 等隐私字段。
- 错误统一用 `writeErr(status, code, msg)`；500 必须在服务端记录底层错误。

### 协议变更
- 信令消息类型目前有**两份手工同步的定义**：`server/internal/signal/protocol.go` 和 `client/internal/signal/protocol.go`。改一边必须同时改另一边，并补上两侧的测试。
- REST 改动需要同步 `client/internal/state/api.go` 和 `client/frontend/src/api/types.ts`。
- 只做向后兼容的改动：新增字段可以；删字段或改语义需要升协议版本，并征得用户同意。

### 前端
- 所有 Go 调用都经过 `src/api/bridge.ts`（`Call.ByName("main.App.<方法>")`），组件里不准直接 import `@wailsio/runtime`。
- 启动期任何依赖"是否在原生环境"的分支，都必须在 mount 之前 `await whenNativeReady()`。
- 事件订阅要在组件卸载时**按监听器逐个注销**（使用 `On` 返回的取消函数），不要按事件名整体 `Off`。
- 新增 store 逻辑或纯函数时，同时补上 vitest 测试。

---

## 7. Wails v3 已知陷阱（都踩过）

- v3 默认在最后一个窗口关闭时退出进程。各平台都要设置 `DisableQuitOnLastWindowClosed: true`；目前只配了 Windows，Linux 和 macOS 还没配。
- 关窗拦截用 `window.RegisterHook(events.Common.WindowClosing, ...)` 加 `e.Cancel()`。所有放行路径都要显式调用 quit。
- 销毁窗口或重建渲染进程时，`recreating` 标志必须在重建完成后复位。
- 前端 `Events.On` 的回调参数是 WailsEvent，载荷在 `.data` 里。
- 拖拽区域属性仍然是 `--wails-draggable`。
- `window.Reload()` 替代了 v2 的 WindowReloadApp；`window.Restore()` 替代了 WindowUnminimise。

---

## 8. Agent 协作方式

- 多步任务由主会话拆分并派给 SubAgent 执行，主会话负责分配任务、验收代码、修复阻断问题、提交和发版。里程碑或批次之间连续推进，**不逐步请示用户**。
- 只有这些情况需要停下来问用户：破坏性操作、协议或数据不兼容、新增根目录文件、升级锁定版本、执行外部下载物、生产环境的写操作。
- 实现前先在 `docs/优化完善方案.md` 里找到对应条目；完成后在条目旁标注"✅ 已完成（提交号）"。如果发现新问题，追加登记到这份文档。
- 跨会话的事实（部署状态、踩过的坑、用户偏好）写入长期记忆，不写进仓库。

## 9. 完成定义（每项任务结束前自查）

- [ ] 第 3 节对应的本地验证全部通过，`gofmt -l` 无输出
- [ ] 新逻辑有测试；音频热路径的基准仍为 0 allocs
- [ ] 协议或 REST 改动两侧同步
- [ ] 没有 UI 约束违规（头像、emoji 图标、主题色）
- [ ] 提交信息符合规范，没有 attribution 尾注，没有本地材料混入
- [ ] push 后 CI 全绿（macOS 除外，它本来就只在标签时运行，而且不阻塞）
