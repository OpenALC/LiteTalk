# AGENTS.md：轻音 LiteTalk 开发规范（所有 Agent 必读）

LiteTalk 是 OpenALC 组织的开源桌面语音开黑软件，SFU 架构，服务端只转发、不混音。
目标是做一个更轻量、性能更好的开源类 Discord，用来替代 TeamSpeak 3，并对外暴露 API，留给后续的移动端。

- 客户端：Go 1.27 + Wails **v3.0.0-beta.30**（版本锁死，`go.mod` 与 `@wailsio/runtime` 必须同版本）+ Vue 3 / Vite / TS / Pinia
- 音频链路：malgo 采集 → rnnoise-go 降噪与 VAD → 自研 libopus cgo 封装 → pion/webrtc
- 服务端：Go 单体（REST `/api/v1` + WS 信令 + 自研 Pion SFU，ICE-Lite UDP mux :7845），默认 SQLite（含持久化 KV）+ 内置 TURN/TLS；可选 MySQL、Redis、coturn 或前置反代。

本地规划文档在 `docs/`（**不进仓库**），动手前先读：
- `docs/2026-11-05-下一阶段规划.md`：当前执行入口。功能冻结，后续只做缺陷、效果、性能、资源与视觉优化；性能/资源改动须有基准、样本或日志等可测依据。
- `docs/优化完善方案.md`：已知缺陷清单与修复批次。修 bug 前先查这里有没有登记，按里面的定位和方案做。
- `docs/演进建议.md` 与 `docs/archive/` 中的批次 7、GTK4 等资料只作架构与验收历史参考；具体范围以当前规划和用户最近确认的任务书为准。

### 长期记忆（与 Claude Code 共用）

跨会话事实（部署状态、踩过的坑、用户偏好、外部资源位置）放在 `~/.litetalk-memory/`：

- `MEMORY.md` 是索引，一条事实一个 `.md` 文件，frontmatter 带 `name` / `description` / `metadata.type`。
- **开工前先读 `MEMORY.md`**，按索引只读相关文件；不要把记忆内容抄进本文件。
- 索引中的当前进度入口优先于历史档案；执行前用对应分支、CI 和任务书核对状态，历史部署记录只代表记录日期的观测。
- 写入口径：仓库、git 历史或本文件已能得知的不记；相对日期写成绝对日期。
- 该目录在仓库之外，含生产凭据等敏感信息，**禁止复制进仓库或提交**。
- Claude Code 的记忆目录已联接到同一目录，两边读写的是同一份文件。

---

## 1. 硬性约束（违反即返工）

### 仓库与工具链
- 仓库主体为 `client/`、`server/`、`api/`、`.github/`；项目规范只维护根 `AGENTS.md`，`CLAUDE.md` 通过 `@AGENTS.md` 导入，禁止维护两份规范正文。
  - `api/`（#28/#29）：REST 与信令契约的单一事实源——`openapi.yaml` + `signal/*.schema.json`，由 `server/api/tools/` 下的生成器消费。
  - `Dockerfile`/`docker-compose.yml`/`.dockerignore`（#27）：一键自托管。
  - `.gitattributes`：统一行尾为 LF。
  新增根目录文件或目录**不再需要逐次征得用户同意**（2026-10-18 一次性授权），但必须与某个 issue 对应，且不得违反其它硬约束——`docs/`、`prototype/` 等本地材料仍然禁止推送。
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
- 递增规则（2026-10-20 用户确认，覆盖旧的“新功能功能段+1”规则）：
  - **大版本**：只在重大技术栈升级、新增适配平台或用户明确指示时 +1，否则一律不动；
  - **功能段**：按计划的功能集 +1（一个功能段版本包含多个 issue 功能），功能集内不因单个 issue 升功能段；
  - **修复段**：功能集内每个 issue 交付时产生的小版本、以及纯 bug 修复，统一在修复段递增；
  - 切换功能段时修复段清零。
  - 例：`v1.6.0-alpha`（#35）→ `v1.6.1-alpha`（#36）→ `v1.6.2-alpha`（#37）……；下一功能集从 `v1.7.0-alpha` 开始。
- CI 的 `check-tag` job 会用正则校验标签格式，不合规直接失败。
- 已推送的标签不改写、不重打。打包失败就递增修复段重新发（先例：v1.0.0-alpha 失败后发了 v1.0.1-alpha）。
- 当前 v1.10.x 功能段冻结在 1.10，修复交付递增修复段；新增功能须用户单独解冻，不按旧路线图自动排期。

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
  - Linux 默认使用 GTK4 + WebKitGTK 6.0：检查不加 `gtk3`，生产构建用 `production`。`-tags gtk3` 仅保留配置兼容测试，不作为完整旧版客户端的构建或发布门禁。不要用 `server` 标签做 Linux 检查。
- 热重载开发：在 `client/` 下执行 `wails3 dev`（**不是** `wails dev`，那是 v2 CLI，会报找不到 `wails.json`）。Windows 的 Taskfile 通过 `CC`/`CXX` 指定 mingw gcc/g++，默认路径是 `C:/tools/mingw64/bin`，可用 `LITETALK_MINGW` 覆盖。go-task 在 Windows 上设置 `env.PATH` 不会影响命令查找，所以不要改成通过 PATH 注入。
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
go run github.com/rhysd/actionlint/cmd/actionlint@v1.7.12
```

`gofmt -l` 的输出必须为空，否则 CI 失败。

行尾由根目录 `.gitattributes` 统一固定为 LF（`*.go`/`*.mod`/`*.sum`/`*.sh`/`*.yml`/`*.sql`），`eol=lf` 优先于本机的 `core.autocrlf=true`。在这个文件之前就存在的克隆，工作区仍是 CRLF，`gofmt -l` 会列出全部文件——需要把这些文件删掉再检出一次才会转成 LF（`git checkout-index -f -a` 对未过期文件无效，`git add --renormalize .` 只改索引不改工作区）：

```bash
git ls-files --eol | awk '$2=="w/crlf"{print $NF}' | tr '\n' '\0' | xargs -0 rm -f
git checkout -- .
```

---

## 4. CI 门禁（`.github/workflows/ci.yml`）

| 组 | Job | 触发 | 阻塞 | 内容 |
|---|---|---|---|---|
| 检查 | `check-tag` | 只在标签推送时 | 是 | 校验标签符合版本规范 |
| 检查 | `check-server` | 每次 push 和 PR | 是 | gofmt、`go mod tidy -diff`、vet、`test -race`、build |
| 检查 | `check-mysql` | 每次 push 和 PR、标签 | 是 | MySQL 8.4.13 service、每测试独立 schema、Store 真库 `test -race`；缺 DSN 或 MySQL 用例跳过即失败，`release` 显式依赖 |
| 检查 | `check-frontend` | 每次 push 和 PR | 是 | `pnpm install --frozen-lockfile`、type-check、vitest、build，产出 `frontend-dist` artifact |
| 检查 | `check-client-linux` | 每次 push 和 PR | 是 | gofmt、tidy、vet、默认 GTK4 的 `test -race` 与 build；额外以 `-tags gtk3` 仅测 `core/config` |
| 检查 | `smoke-client-linux-install` | 各分支 push、PR 与标签 | 是 | Ubuntu 24.04、Debian 13、Fedora 40、AlmaLinux 10 安装及启动检查；必须有前端挂载后的原生绑定就绪信号，只有进程存活或早期启动日志不算通过；release 依赖此门禁 |
| 检查 | `check-client-windows` | 每次 push 和 PR | 是 | vet、`test -race`、production GUI build。**这是真实 cgo Opus、DPAPI、WASAPI 唯一被测试的地方** |
| 检查 | `check-contract` | 每次 push 和 PR | 是 | 契约 ↔ 实现双向比对（`server/api/tests`，AST + 运行时注册表 + kin-openapi）、Go 生成物新鲜度、schemathesis 模糊测试（锁版本，固定 seed，断言无 5xx 与形状违约）。**阻塞发版** |
| 打包 | `package-server` | 标签 | 是 | linux amd64/arm64，tar.gz 内含二进制；`migrations/` 已 embed 进二进制（#18），不再随包分发 |
| 打包 | `package-client-windows` | 标签 | 是 | exe 加 NSIS 安装器（版本号从标签注入）加绿色版 zip |
| 打包 | `package-client-linux` | 标签 | 是 | 矩阵 amd64/arm64：绿色版、deb、rpm（nfpm 锁版本） |
| 打包 | `package-client-macos` | 标签 | **否** | 裸二进制 zip，实验性，`continue-on-error` |
| 发布 | `release` | 标签 | — | 阻塞打包 job 全部成功才发布。附带 `SHA256SUMS.txt`；带 `-` 的标签标记为 prerelease |

规则：
- **macOS 永远不阻塞发版**：`release` 的 `if` 条件里**不准**加入 macOS job 的结果，macOS job 必须保留 `continue-on-error: true`。
- MySQL 真库检查是阻塞门禁：`release` 的 `needs` 与 `if` 都必须要求 `check-mysql` 成功，不能以缺 DSN 或 `t.Skip` 放行。
- 前端只在 `check-frontend` 构建一次，其他 job 通过 `download-artifact` 获取 `frontend-dist`。**不要在其他 job 里重复构建前端**；新增需要 embed 的 job 时，同样通过 `needs: check-frontend` 下载产物。
- 只改 `*.md` 或 `LICENSE` 的 **push** 不触发 CI（paths-ignore）；PR 仍触发检查，标签推送不受这个过滤影响。文档 push 被过滤时记录“未触发”，不能把旧 run 当成本次全绿。
- 同一 ref 的新提交会取消旧的检查运行；标签运行不会被取消。
- 第三方工具要锁版本（例如 `NFPM_VERSION`），禁止使用 `@latest`。runner 也要锁版本（`ubuntu-24.04`），不用 `ubuntu-latest`：它即将迁移到 Ubuntu 26，会改变 glibc 和 WebKitGTK 版本。
- 待办：各个 actions 仍是 v4/v5，运行在 Node 20 上，有弃用警告。升级到当前大版本（checkout/setup-go/setup-node/upload-artifact v7、download-artifact v8、pnpm/action-setup v6、action-gh-release v3）属于单独任务。打包 job 只在标签时运行，升级后要先用 `workflow_dispatch` 跑一次检查组，再借下一次发版验证打包组。
- 新增的平台专属代码（`*_windows.go`、`*_linux.go`）必须在对应平台的检查 job 中被编译到。**不准**用 `t.Skip` 掩盖"某平台功能未实现"，平台功能缺失应该让测试失败或断言失败，而不是跳过。

### 监控 CI
```bash
"$GH" run list --repo OpenALC/LiteTalk --branch dev --limit 3
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
- 当前开发以 `dev` 为集成分支；实现从任务指定基点切独立 `feat/*` 或 `fix/*` 分支，不默认直推 `main`。
- 协议或信令变更、数据库迁移、跨批次大改动，以及用户要求评审的改动，按任务书提 PR；用户审查通过后再合入 `main`，合并方式用 squash。

### dev 集成与审查工作流
- 当前按 `docs/2026-11-05-下一阶段规划.md` 与用户最近确认的任务书实施；历史批次的审查与发版授权不沿用到后续标签。
- 实施由 SubAgent 在各自 worktree 完成；**实现代理负责提交、推送与 CI 监控**，主会话只负责派活、验收、合并与发版，**不亲自写实现代码**（节省主上下文）。代理推送独立 `feat/#N-*` / `fix/#N-*` 分支并跑该分支 CI，报告 run id 与结果；主会话验收后再合入 `dev`，合入后的 `dev` CI 仍由同一代理监控。代理不直接改 `main`/`dev`。
- 主会话逐项验收后按当前任务书指定的顺序和合并方式集成至 `dev`；这次集成 CI 全绿后再合下一项。纯文档 push 按 CI 过滤规则单独记录。
- 版本或批次审查 PR 按交付范围列提交段并链接 issue；审查修正继续在独立分支交付后合入 `dev`。不因旧版本曾要求“中途不开 PR”而跳过当前审查。
- GTK4 代码已交付，`gtk` 迁移分支已退役；后续 v1.10.x 继续在 `dev` 集成，进入 `main` 与发版另按当次审查和授权执行。

### Issue 跟踪（路线图功能已全部建单）
- 每项工作以 GitHub issue 的范围和验收为准，标签仍按目标版本、优先级、领域三轴维护；旧路线图的功能单保持 backlog，不因已有 issue 就自动解冻。
- **动手前先认领**：从 issue 的"要点/验收"出发实现，不要在没单的情况下自行扩大范围。
- **提交信息引用单号**：在摘要后加 `(#N)`，例：`feat(client): 全局热键与按键说话——三平台钩子与 PTT 门控 (#4)`。一次提交只关一个单；跨多个单的改动拆开提交。
- **关闭时机**：该 issue 所属交付**已被用户审查通过且合并到 main** 后才 `gh issue close N --repo OpenALC/LiteTalk`。CI 全绿只说明可以进审查，不是关单条件；集中跟进单还要逐项对账，不能随其中一项完成就关整单。
- 复现不出来或决定不做的单，在 issue 里写结论再关，**不留说不清的 open 单**。
- 版本号以用户批准的执行计划为准，不按 `docs/演进建议.md` 的旧表推断；屏幕共享/摄像头（#48）、移动端（#55）等新增功能仍在 backlog。

### 节奏（强制，不再逐次请示用户）
1. 每完成一个功能：实现代理提交并推送独立分支，**等这次 push 的 CI 全绿**，主会话验收后合入 `dev`，再等集成 CI 全绿。CI 红了先修，不能带着红灯继续；纯文档 push 按过滤规则记录。
2. 每完成一个版本：该版本最后一次 push 的 CI 必须全绿，才能进入下一个版本。
3. 所有版本完成后：本地做一次全量验证（第 3 节全部命令），并确认 `dev` 上最新的 CI 是绿的。
4. 发版须满足当次任务书的验收、审查与用户对该标签的授权；已消费的旧授权不复用，未通过或缺测项如实记录。获准发版后核对下列资产，并等标签 CI 成功：
   - Windows：setup.exe、portable.zip
   - Linux amd64/arm64：portable.tar.gz、deb、rpm
   - 服务端：amd64/arm64 tar.gz
   - SHA256SUMS.txt
   - macOS：有就有，没有不影响发版

### 服务端发版后的部署提醒
- SQL 迁移已 embed 进服务端二进制，Release 包不再分发 `migrations/`，正常升级无需另传 SQL。`LITETALK_MIGRATIONS_DIR` 会覆盖内嵌来源；旧目录可能让新迁移缺失，部署前检查并移除无意遗留的覆盖配置。
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
- Wails 层（`app.go`）只做适配，业务逻辑已在 `core/`；`internal/` 只保留平台壳件，遵守下文分层边界。
- 并发：Wails 的绑定方法会被并发调用，共享字段必须加锁或用 atomic。CI 开启了 `-race`。

### Go 服务端
- 每个 REST 和 WS 处理函数都必须做**鉴权和资源归属校验**（用户是否是该服务器的成员，是否有对应权限），不能只校验"已登录"。
- 按 uid 清理资源（房间成员、SFU peer）时，必须核对是**同一个连接实例**，不能按 uid 盲删。
- 对外返回用户信息时**不得包含 email** 等隐私字段。
- 错误统一用 `writeErr(status, code, msg)`；500 必须在服务端记录底层错误。

### 协议变更
- 信令消息类型的**单一事实源是 `api/signal/*.json`**（JSON Schema），由 `server/api/tools/signalgen` 生成两侧 `protocol_gen.go`（`server/internal/signal/` 与 `client/core/signal/`，除 package 行外**字节相同**）。**不要手改生成物**——要改消息就改 Schema 再跑生成器；CI 的 `check-server` 会因生成物不新鲜而红，`check-contract` 会因生成物与 Schema 脱钩而红。
- 手写层只留 `Envelope`/`Identity`/`Bool` 等运行时辅助，且两边仍需逐字对齐。
- REST 改动需要同步 `client/core/api` 与 `client/frontend/src/api/types.ts`（后者从 `types.gen.ts` re-export，REST 类型由 `api/openapi.yaml` 生成，改契约要重跑 `pnpm run gen:types`）。
- 只做向后兼容的改动：新增字段可以；删字段或改语义需要升协议版本，并征得用户同意。

### 客户端分层（#34）
- `client/core/` 是与 UI 无关的业务层：`{api,audio,codec,config,gateway,hotkey,rtc,signal,voice}`。**`core/` 禁止 import `desktop/`、`wails`、`application`、`@wailsio`**——由 `client/core_boundary_test.go` 用 `go/ast` 机制性强制，改 `core/` 后必须 `go test -count=1 -run TestCoreForbidsDesktopImports .`（该测试运行时扫目录，**不加 `-count=1` 会命中测试缓存拿到旧结果**）。
- `client/internal/` 现在只剩平台壳件；桌面适配层在 `app.go`（Wails 绑定、窗口、托盘、事件转发），只做适配，业务逻辑在 `core/`。
- 音频热路径文件（`core/audio/{dsp,pipeline,mixer,ringbuffer,remote}.go`）的 #34 机械搬迁已完成；后续逻辑改动按对应 issue 实施并复跑基准（参考值：`BenchmarkSendOpus` 0 allocs、`SendOpusWrite/noext` 0、`SendOpusWrite/ext` 1——那 1 个是 pion stats 拦截器的 `header.Clone()`，非本项目代码）。
- 报音频基准一律 `-benchtime=200x -count=3` 或默认 benchtime，**禁用 `-benchtime=1x`**（单次迭代会把懒初始化算进 alloc 计数，数字不可信）。

### 前端
- 所有 Go 调用都经过 `src/api/bridge.ts`（`Call.ByName("main.App.<方法>")`），组件里不准直接 import `@wailsio/runtime`。
- 启动期任何依赖"是否在原生环境"的分支，都必须在 mount 之前 `await whenNativeReady()`。
- 事件订阅要在组件卸载时**按监听器逐个注销**（使用 `On` 返回的取消函数），不要按事件名整体 `Off`。
- 新增 store 逻辑或纯函数时，同时补上 vitest 测试。

---

## 7. Wails v3 已知陷阱（都踩过）

- destroy 休眠依赖最后一窗关闭后进程仍存活。Windows、Linux 已设置 `DisableQuitOnLastWindowClosed: true`；macOS 使用相反语义的 `ApplicationShouldTerminateAfterLastWindowClosed: false`。维护窗口生命周期时保留这些设置，不要跨平台照抄字段。
- 关窗拦截用 `window.RegisterHook(events.Common.WindowClosing, ...)` 加 `e.Cancel()`。所有放行路径都要显式调用 quit。
- Windows 的程序化关窗标记按窗口实例保存；旧窗口的迟到关闭/最小化事件不得影响新窗口。托盘与单实例唤醒共享一次重建认领，Wails 的 `Show`/`Close` 调用在 `winMu` 锁外执行；其它平台仍需在重建后复位 `recreating`。
- 前端 `Events.On` 的回调参数是 WailsEvent，载荷在 `.data` 里。
- 拖拽区域属性仍然是 `--wails-draggable`。
- `window.Reload()` 替代了 v2 的 WindowReloadApp；`window.Restore()` 替代了 WindowUnminimise。

---

## 8. Agent 协作方式

- 多步任务由主会话拆分并派给 SubAgent 执行，主会话负责分配任务、验收代码、修复阻断问题、提交和发版。里程碑或批次之间连续推进，**不逐步请示用户**。
- 只有这些情况需要停下来问用户：破坏性操作、协议或数据不兼容、升级锁定版本、执行外部下载物、生产环境的写操作。（新增根目录文件已获一次性授权，不再逐次问。）
- 实现前先在 `docs/优化完善方案.md` 里找到对应条目；完成后在条目旁标注"✅ 已完成（提交号）"。如果发现新问题，追加登记到这份文档。
- 跨会话的事实（部署状态、踩过的坑、用户偏好）写入长期记忆，不写进仓库。

## 9. 完成定义（每项任务结束前自查）

- [ ] 第 3 节对应的本地验证全部通过，`gofmt -l` 无输出
- [ ] 新逻辑有测试；音频热路径的基准仍为 0 allocs
- [ ] 协议或 REST 改动两侧同步
- [ ] 没有 UI 约束违规（头像、emoji 图标、主题色）
- [ ] 提交信息符合规范，没有 attribution 尾注，没有本地材料混入
- [ ] 代码 push 后 CI 全绿；纯文档 push 若被过滤，记录“未触发”（macOS 只在标签时运行且不阻塞）
