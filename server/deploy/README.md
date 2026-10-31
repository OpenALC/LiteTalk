# LiteTalk 服务端上线指南（RockyLinux 9.5）

默认形态是**单二进制自托管**：

- **SQLite** 存业务数据，KV（刷新令牌/验证码/限流）落在同一个库文件里，重启不丢；
- **内置 TURN**（UDP+TCP 3478）与 **内置 ACME**（443/80 自动签发续期），不装 coturn/nginx；
- **迁移 SQL 已 embed 进二进制**，启动时自动执行，不需要手工上传 `migrations/`；
- 数据目录由进程按 **0700** 创建，SQLite 库与 `-wal`/`-shm` 为 **0600**（#88）。

MySQL、Redis、外部 coturn、前置反代都是可选扩展，见本文末尾。

## TLS 三种模式（#26）

| 模式 | 启动方式 | TLS 谁终结 | 适用 |
|---|---|---|---|
| 1 内置 ACME 自动签发 | `litetalk-server --domain voice.example.com`（或 `LITETALK_DOMAIN`） | **Go 进程自己**：443 TLS-ALPN-01 + 80 HTTP-01/301，证书自动申请与续期 | 一键自托管，不装 nginx |
| 2 前置反代 | `litetalk-server --behind-proxy` | nginx / Caddy（本目录 `nginx/litetalk.conf`） | 与既有站点共存、复用已有证书体系 |
| 3 纯 HTTP | 都不设 | 无 TLS | 仅开发/内网，启动日志会提示 |

**模式 1 要点**：

- 证书缓存默认 `$LITETALK_DATA_DIR/autocert`（权限 0700，私钥不全局可读），自动续期，无需 cron。
- 443 是特权端口：root 运行，或非 root 时在 systemd 单元启用
  `AmbientCapabilities=CAP_NET_BIND_SERVICE`（`systemd/litetalk.service` 里有示例）。
- `LITETALK_ADDR` 未显式配置时 HTTPS 自动改监听 `:443`；显式配置过就不再自动改端口
  （模板里这一行是注释掉的，不要随手打开）。
- 签发域名必须是 `--domain` 单域名（HostPolicy 白名单）；其他 SNI 一律拒绝，防止被 CA 限流。
- 需要公网 80/443 可达（Let's Encrypt 验证）；安全组/防火墙放行这两个端口。
- **另占 `127.0.0.1:8077`（仅环回）**：更新器默认按该地址做 healthz 版本断言，所以 ACME 模式
  会在回环上单独起一个只提供 `GET /api/v1/healthz` 的监听，不把管理 API 带到回环面。
  该端口不对公网开放，无需放行。`LITETALK_ADDR` 显式配成 8077 时跳过这一路（主监听已覆盖）；
  8077 被别的进程占用时只记告警，服务照常启动，此时请把 `LITETALK_UPDATER_HEALTH_URL`
  改到实际可达的 healthz 地址（如 `https://域名/api/v1/healthz`），否则更新后的版本断言会超时回滚。
- 设了域名后，SFU 宣告地址默认由 DNS 解析推导；解析不出或结果不对时再显式配 `LITETALK_SFU_IP`。

**模式 2 要点**：

- 服务端**不**占 80/443，只监听 `LITETALK_ADDR`（默认 `:8077` 纯 HTTP），TLS 由反代终结。
- 必须配置 `LITETALK_TRUSTED_PROXIES`（反代的 IP/CIDR）：只有 RemoteAddr 命中才采信
  `X-Real-IP`，否则限流/审计记的是反代 IP。伪造的 X-Real-IP 永远不会被采信。
- 反代配置见 `nginx/litetalk.conf`（含 `X-Real-IP` 与 WebSocket 升级头）。

## 1. 放行端口

| 端口 | 协议 | 用途 |
|---|---|---|
| 443 | TCP | 内置 ACME HTTPS（REST + WSS）；模式 2 下归反代 |
| 80 | TCP | ACME HTTP-01 与 301 跳转；模式 2 下归反代 |
| 8077 | TCP | 纯 HTTP API（无 TLS / 前置反代模式；默认只监听本机即可）。**ACME 模式下仍会在 `127.0.0.1:8077` 起一个只提供 healthz 的环回监听**（供更新器断言版本），仅本机可达、不需要放行 |
| 8088 | TCP | 自动更新器 webhook（可选安装；`/status` 仍仅环回，公网只需放行本端口） |
| 7845 | UDP | SFU 媒体（ICE-Lite UDP mux） |
| 7845 | TCP | ICE-TCP（UDP 被封网络直连；`LITETALK_SFU_TCP=0` 可关闭） |
| 3478 | TCP/UDP | 内置 TURN |
| 49152-49200 | UDP | 内置 TURN 中继端口段（`LITETALK_TURN_RELAY_PORT[_END]`） |

云服务器在安全组放行；本机防火墙：

```bash
firewall-cmd --permanent --add-port=443/tcp --add-port=80/tcp \
  --add-port=7845/udp --add-port=7845/tcp \
  --add-port=3478/tcp --add-port=3478/udp \
  --add-port=49152-49200/udp \
  --add-port=8088/tcp
firewall-cmd --reload
```

更新器（§9）只监听 8088 明文 HTTP，不需要 DNS、证书或 443，也不经过 nginx；
主服务内置 ACME 独占**公网** 443/80，并在环回另起一个 `127.0.0.1:8077` 的 healthz 面供更新器断言，
两者互不影响。不装更新器就不必放行 8088；环回 8077 任何模式都不需要放行。

## 2. 服务端二进制

Release 页下载 `litetalk-server-linux-<arch>.tar.gz`（amd64/arm64）：

```bash
sudo useradd -r -s /sbin/nologin litetalk
sudo mkdir -p /opt/litetalk/server
sudo tar -xzf litetalk-server-linux-amd64.tar.gz -C /opt/litetalk/server
sudo mkdir -p /opt/litetalk/data
sudo chown -R litetalk:litetalk /opt/litetalk/data
```

数据目录存放 SQLite 库、自动生成的 `jwt.secret` 和 ACME 证书缓存。进程首次启动会把目录收紧到
0700、库文件收紧到 0600；上面的 `chown` 是为了让非 root 服务账号可写，权限位交给进程处理。

迁移 SQL 已内嵌进二进制（embed.FS），**不需要**再解压 `migrations/`；启动时自动执行，已应用的
版本跳过（SQLite 用事务互斥）。`LITETALK_MIGRATIONS_DIR` 只用于本地调试，生产不要设置。

## 3. 环境变量

```bash
sudo cp env/litetalk.env.example /etc/litetalk.env
sudo chmod 600 /etc/litetalk.env
sudo vi /etc/litetalk.env
```

最常见的两种最小配置：

- **内置 ACME（推荐）**：只设 `LITETALK_DOMAIN=你的域名`，并把 `LITETALK_DATA_DIR` 设为
  `/opt/litetalk/data`（与第 2 步创建的目录一致；默认 `./data` 会落到只读的安装目录下）。
  JWT 密钥与 TURN 密钥留空，启动时自动生成（JWT 落盘 `$LITETALK_DATA_DIR/jwt.secret`，0600；
  TURN 密钥在内存中，重启后客户端会自动重新取凭证）。SFU 宣告 IP 由域名解析推导。
- **前置反代**：`LITETALK_BEHIND_PROXY=1`、`LITETALK_TRUSTED_PROXIES=反代IP/CIDR`、
  `LITETALK_DATA_DIR=/opt/litetalk/data`，并显式设置公网 `LITETALK_SFU_IP`。

完整变量、默认值与说明见 `env/litetalk.env.example`。生产构建会拒绝弱密钥、环回 SFU IP
和 `LITETALK_DEV_AUTH=1`。

## 4. systemd

```bash
sudo cp systemd/litetalk.service /etc/systemd/system/
sudo systemctl daemon-reload
sudo systemctl enable --now litetalk
```

- 非 root 运行且用内置 ACME 时，启用单元里的
  `AmbientCapabilities=CAP_NET_BIND_SERVICE` 与 `CapabilityBoundingSet=CAP_NET_BIND_SERVICE`。
- 日志：`journalctl -u litetalk -f`。
- 首启会打印管理员邀请码（实例认领后失效）；之后可重新打印（需要先停服务，避免并发打开
  SQLite；以 root 读取 600 的 env 文件后再执行）：

  ```bash
  sudo systemctl stop litetalk
  sudo bash -c 'set -a; . /etc/litetalk.env; set +a; cd /opt/litetalk/server && ./litetalk-server -print-admin-invite'
  sudo systemctl start litetalk
  ```

## 5. 验证清单

```bash
# REST 健康检查（模式 1 用 https://域名，模式 2/3 用 http://127.0.0.1:8077）
curl https://你的域名/api/v1/healthz          # {"status":"ok","version":"..."}

# WS 信令（未带令牌应 401：路由通、鉴权在工作）
curl -i "https://你的域名/ws?token=x"

# TURN 凭证签发（登录后带 Bearer 令牌；/auth/* 还需 X-LiteTalk-Client: dev 或真实客户端版本）
curl -X POST https://你的域名/api/v1/rtc/credentials \
  -H "Authorization: Bearer <access_token>"   # 返回 iceServers JSON
```

全链路端到端拨测（simple 模式；自动注册两个探测账号并双向打流，覆盖 REST/WS/ICE/UDP/SFU 转发）：

```bash
go run ./cmd/e2eprobe -addr <地址>:8077
```

注意 `e2eprobe` 目前固定用 `http://`，不能直接连内置 ACME 的 443。请在前置反代/纯 HTTP
形态（8077）上跑，或临时以 `--behind-proxy` 起一个实例验证；结论 `PASS` = 全链路通。

上线自查：`LITETALK_DEV_AUTH` 未开启；显式配置的 `LITETALK_TURN_SECRET`/`LITETALK_JWT_SECRET`
均为 ≥32 字节随机值；无域名部署的 `LITETALK_SFU_IP` 是公网 IP。

## 6. 可选扩展

### 6.1 前置反代（模式 2）

```bash
sudo dnf install nginx
sudo cp nginx/litetalk.conf /etc/nginx/conf.d/
# 改 server_name 与证书路径（acme.sh --issue -d 你的域名 --nginx）
sudo nginx -t && sudo systemctl enable --now nginx
```

服务端以 `--behind-proxy` 启动，只监听 8077；`/etc/litetalk.env` 配好 `LITETALK_TRUSTED_PROXIES`。

### 6.2 MySQL / Redis

单机默认不需要。需要 MySQL 时：

```bash
sudo dnf install mysql-server
sudo systemctl enable --now mysqld
sudo mysql -e "CREATE DATABASE litetalk CHARACTER SET utf8mb4;
  CREATE USER 'litetalk'@'localhost' IDENTIFIED BY 'CHANGE_ME';
  GRANT ALL ON litetalk.* TO 'litetalk'@'localhost';"
```

然后在 `/etc/litetalk.env` 配 `LITETALK_STORE=mysql` 与 `LITETALK_MYSQL_DSN`。迁移仍然
自动执行（embed 的 MySQL 方言按文件名顺序应用）。

多实例部署必须配 `LITETALK_REDIS_ADDR` 共享 KV，并给每个实例分配不同的
`LITETALK_SNOWFLAKE_WORKER`（0-1023）。只配 Redis 不配 MySQL 时，业务数据仍走 SQLite，
KV 走 Redis；显式配置的 Redis 必须连通，否则拒绝启动。MySQL 后端未配 Redis 时会退回
进程内 KV 并打印告警（重启后刷新令牌等丢失）。

### 6.3 外部 coturn

只有多机中继、TURN-over-TLS(5349) 或复用已有 coturn 时才需要：

```bash
sudo dnf install coturn
sudo cp coturn/turnserver.conf /etc/coturn/turnserver.conf
# 改两处：external-ip=公网IP、static-auth-secret=与 LITETALK_TURN_SECRET 一致
sudo systemctl enable --now coturn
```

服务端侧设 `LITETALK_TURN_PORT=0` 关闭内置 TURN，并按需显式配置
`LITETALK_TURN_URLS` / `LITETALK_STUN_URLS` 与 `LITETALK_TURN_SECRET`。

## 7. 升级

```bash
sudo systemctl stop litetalk
sudo tar -xzf litetalk-server-linux-amd64.tar.gz -C /opt/litetalk/server
sudo systemctl start litetalk
```

- 新迁移在启动时自动执行；只需替换二进制，**不再需要**解压 `migrations/`。
- **0008 好友去重规则已修正（#100）**：旧规则只保留最小 id，反向 `accepted` 若 id 较大会被
  较小的 `pending` 顶掉，已接受的好友关系随之丢失。已执行过 0008 的实例不会重跑该迁移，
  被旧规则删掉的行也无法自动恢复——受影响用户重新发起并接受好友即可。尚未执行 0008 的
  升级或新装实例会按 `accepted > blocked > pending` 的优先级保留，同状态保留最早一行。
- 升级前建议备份数据目录：SQLite 在停止服务后复制 `litetalk.db` 即可（WAL 会在正常关闭时
  回放合并）；整个 `/opt/litetalk/data` 一起复制最稳妥。
- 旧部署的宽松权限会在首次启动时自动收紧，不需要手工 chmod。
- 装了自动更新器后优先用一键升级（与 webhook 同一条流水线，失败自动回滚）：

  ```bash
  sudo -u www /www/wwwroot/LiteTalk/update.sh --tag v1.6.2-alpha --allow-prerelease   # 手工升级
  sudo -u www /www/wwwroot/LiteTalk/update.sh --tag v1.6.2-alpha --dry-run --allow-prerelease  # 只校验不落地
  sudo -u www /www/wwwroot/LiteTalk/update.sh --tag latest --allow-prerelease         # 跟到通道内最新
  ```

  用 `sudo -u www`（服务用户）执行：root 落下的 `state.json`/锁文件属主会让以 www 运行的
  更新器读不到状态、拿不到锁（EACCES）。退出码：0 成功、1 更新失败（已回滚）、2 参数错误、
  **75 已有更新在运行**（webhook 或另一个 `--once` 持锁，等它结束再重试）。

  `latest` 按 **semver 取通道内最高**（不是 release 列表第一条：补发的旧版本 tag 会排在最前），
  并且与 webhook 同一条防降级闸——解析结果不新于已装版本时只打印"已是最新"并 **退出 0**，
  不下载、不替换、不写状态。显式指定 `--tag vX.Y.Z` 不受此闸限制，可用来手工回退。

## 8. 一键安装（#97）

`install.sh` 把上面 2-5 步压成一条命令（幂等，重复执行只补齐缺失项，不覆盖已有配置）：

```bash
sudo bash install.sh                        # 默认装到 /www/wwwroot/LiteTalk，用户 www
sudo bash install.sh --dry-run              # 只打印动作，不改文件
sudo bash install.sh --dir /opt/litetalk --user litetalk --no-updater
```

行为：

- 检测 root/OS/arch，从 GitHub Release 下载 `litetalk-server-linux-<arch>.tar.gz`
  （内含 `litetalk-server` + `litetalk-updater` + `update.sh`）并校验 `SHA256SUMS.txt`；
- 安全解包（成员白名单；拒绝路径穿越与符号链接/硬链接/设备/FIFO 等非普通条目，
  与更新器 `safeExtract` 同口径），二进制 root 所有 0755，安装目录属主是服务用户；
- 生成 `/etc/litetalk.env` 与更新器 env/secret 模板（**不覆盖已有文件**；secret 生成后不回显，
  权限 `0640 root:<服务用户>`——更新器以服务用户运行要自己读它，other 位为 0）；
- 安装 systemd 单元与 sudoers 并 `enable --now`。**已存在的 unit/sudoers 不会被覆盖**，
  新模板写到同目录 `.new` 供对比合并（`.new` 不会被 systemd/sudo 读取）。`--no-updater` 只装主服务；
- 生成的 `litetalk.service` 已带 `AmbientCapabilities/CapabilityBoundingSet=CAP_NET_BIND_SERVICE`：
  非 root 服务用户也能用 `LITETALK_DOMAIN` 起内置 ACME 监听 443，不必事后手改 unit；
- **通道策略**：默认只装正式版 tag；预发布（`-alpha/-beta/-rc`）需显式 `--prerelease`。
  `--version latest` 按 **semver 取通道内最高**（不是 release 列表第一条）。
- `--dir` 必须是绝对路径、不含 `..`、不含空白字符（路径会原样写进 systemd 单元，
  空白会把 `ExecStart` 拆段），违规直接以退出码 2 拒绝。

## 9. 自动更新（#97）

发布 Release 后服务器自动更新：GitHub `release` webhook → `litetalk-updater`（监听 8088 明文
HTTP，HMAC 校验）→ 下载 + SHA256 → 原子替换 → `systemctl restart litetalk` → healthz 版本断言
→ 失败回滚旧二进制。更新器与主服务是两个独立进程：主服务重启期间更新器存活，才能完成
断言与回滚。

### 9.1 前置：systemd 收敛与网络

1. **进程托管必须收敛到 systemd**（见 §10 生产切换）：更新器重启的目标是
   `litetalk.service`，孤儿进程形态下更新没有可靠生效点。
2. 网络：安全组/防火墙放行 **TCP 8088**。不涉及 DNS 与证书，也不占用 80/443——
   443 归主服务内置 ACME，两者互不影响；8088 必须空闲（被占用时更新器启动即退出，
   unit 的启动限流会阻止重启风暴）。`/status` 仅接受环回来源。
   更新器默认走 `http://127.0.0.1:8077/api/v1/healthz` 做版本断言：模式 2/3 下就是主服务
   自己的监听；模式 1（ACME）下主服务会在环回补起一个只有 healthz 的 8077 监听（见 §TLS 模式 1）。
   那个端口被占用时主服务只告警不退出，此时把 `LITETALK_UPDATER_HEALTH_URL` 指向实际可达的
   healthz（如 `https://域名/api/v1/healthz`），否则每次更新都会因断言超时而回滚。
3. 更新器单元与 sudoers 见 `systemd/litetalk-updater.service` 与 `sudoers/litetalk-updater`
   （`install.sh` 已装则跳过）。**注意**：更新器单元不能开 `NoNewPrivileges=true`——
   它会禁掉 sudo 的 setuid 提权，`sudo -n systemctl restart` 全部失败；最小权限由
   sudoers 保证（只放行 `restart`/`is-active litetalk.service`）。
4. 配置 `/etc/litetalk-updater.env`（0600 root:root）：

   | 变量 | 默认 | 说明 |
   |---|---|---|
   | `LITETALK_UPDATER_ENABLED` | `1` | `0` 时 webhook 只校验签名与格式并返回 202 ignored，不下载不替换不重启；`update.sh --once` 手工升级不受影响 |
   | `LITETALK_UPDATER_ALLOW_PRERELEASE` | `0` | 默认只接受正式版 tag；**当前 release 全是 `-alpha`，自动更新预发布需显式设 `1`** |
   | `LITETALK_UPDATER_LISTEN` | `:8088` | 监听地址，明文 HTTP。`:443` 会被拒绝启动（443 归主服务）；旧版 `DOMAIN`/`NO_TLS`/`TLS_CACHE` 已移除，设置了也被忽略 |
   | `LITETALK_UPDATER_SECRET_FILE` | `/etc/litetalk-updater.webhook.secret` | webhook secret，**`0640 root:<服务用户>`**（更新器以服务用户运行需组读；other 位为 0；0600 root:root 会让进程读不到而起不来）。缺失/为空时更新器拒绝启动 |
   | `LITETALK_UPDATER_REPO` | `OpenALC/LiteTalk` | 仓库白名单 |
   | `LITETALK_UPDATER_INSTALL_DIR` | `/www/wwwroot/LiteTalk` | 主服务安装目录 |
   | `LITETALK_UPDATER_SERVICE` | `litetalk.service` | 重启目标（与 sudoers 同名） |
   | `LITETALK_UPDATER_HEALTH_URL` | `http://127.0.0.1:8077/api/v1/healthz` | 更新后版本断言 |
   | `LITETALK_UPDATER_STATE_DIR` | `/var/lib/litetalk-updater` | 状态/去重记录与跨进程锁 |

   secret 只存在于 webhook 配置与服务器 env 侧，不进仓库、不进 CI、不出日志。

### 9.2 创建 webhook

```bash
# 生成 secret 并写入服务器（不回显到终端历史之外的地方）
# 权限 0640 root:www：更新器以 www 运行、自己读本文件；other 位为 0
sudo bash -c 'umask 077; openssl rand -hex 32 > /etc/litetalk-updater.webhook.secret'
sudo chown root:www /etc/litetalk-updater.webhook.secret
sudo chmod 640 /etc/litetalk-updater.webhook.secret

gh api repos/OpenALC/LiteTalk/hooks \
  -f name=web -f active=true \
  -f 'events[]=release' \
  -f 'config[url]=http://<服务器IP>:8088/hooks/github-release' \
  -f 'config[content_type]=json' \
  -f "config[secret]=$(sudo cat /etc/litetalk-updater.webhook.secret)"
```

- GitHub 允许 HTTP 目标。更新器不提供 TLS：HMAC-SHA256 是真实性边界（secret 双向一致），
  payload 是 release 元数据、不含任何秘密，因此明文传输可接受；但要保证 secret 与
  webhook 配置只在服务器与 GitHub 之间传递。
- **不要**在同机加反向代理指向 8088：反代会让来源恒为环回，`/status` 被误判为本地访问而
  公开。将来若需要 TLS，应另行设计，而不是套一层反代。
- ping 验证：`gh api repos/OpenALC/LiteTalk/hooks/<id>/ping`，更新器应返回 202，
  `journalctl -u litetalk-updater` 可见 `ping 已忽略`。
- 真实投递验证：对某次 release 用 GitHub 网页 "Redeliver" 重投一次，或等下一次发版。
- 更新器只接受 `release` + `action=published`；其他 action 返回 202 ignored。
  同一 delivery 重投返回 200 no-op；不新于当前版本的 tag 会被防降级闸忽略（200）；
  更新进行中的并发投递返回 **409**（进程内 worker busy，或外部 `update.sh --once`
  正持有跨进程锁）。

### 9.3 更新流程与安全约束

1. 校验 `X-Hub-Signature-256`（HMAC-SHA256，常量时间比较），请求体上限 1MiB；
2. 仓库白名单 + tag 白名单 `^v[0-9]+\.[0-9]+\.[0-9]+(-(alpha|beta|rc))?$` + 通道策略；
3. 校验通过立即 202，后台单飞 worker 执行（GitHub 10s 超时不会打断下载）；
   worker 先取**跨进程锁**（状态目录下），与 `update.sh --once` 互斥：同一时刻只有一条
   更新流水线。webhook 与手工升级撞车时，webhook 返回 409，`--once` 直接退出 **75**；
4. GitHub API 查资产（晚到重试 12×5s）→ 下载 `litetalk-server-linux-<arch>.tar.gz` +
   `SHA256SUMS.txt`（只认同名行）→ SHA256 校验；
5. 安全解包：只接受白名单成员（`litetalk-server`/`litetalk-updater`/`update.sh`），
   拒绝路径穿越/符号链接/额外条目/超大文件；只安装 `litetalk-server`（更新器不自我替换）；
6. 磁盘 ≥200MB → 备份旧二进制为 `litetalk-server.bak-<旧版本>-<时间戳>`（留最近 3 份）
   → 同文件系统原子 rename → `sudo -n systemctl restart litetalk.service`；
7. healthz 轮询直到 `version == tag`（默认 60s）；失败回滚备份并再重启，写失败状态。
   webhook 层面失败记录在状态文件（`/status` 可查），由 GitHub 重投或 `update.sh` 重试。

日志只记 tag、资产、校验和、耗时、结果——不记 secret、请求体、密码。

### 9.4 状态与排查

```bash
curl -fsS http://<服务器IP>:8088/healthz                  # 更新器存活（明文 HTTP，无证书）
# /status 只允许环回访问：SSH 端口转发后在本机看
ssh -N -L 127.0.0.1:8088:127.0.0.1:8088 <用户>@<服务器>
#   另开终端：curl -fsS http://127.0.0.1:8088/status
# 或者直接经 SSH 在服务器上执行：
ssh <用户>@<服务器> 'curl -fsS http://127.0.0.1:8088/status'
journalctl -u litetalk-updater -n 100 --no-pager
ls -lt /www/wwwroot/LiteTalk/litetalk-server.bak-* | head  # 备份
curl -fsS http://127.0.0.1:8077/api/v1/healthz             # 主服务版本应为目标 tag
```

常见问题：

| 症状 | 原因与处置 |
|---|---|
| webhook 401 | secret 不一致；重新同步 GitHub webhook 的 secret 与 `/etc/litetalk-updater.webhook.secret` |
| webhook 429 | 同一来源 IP 一分钟内签名失败超过 20 次：超限后**在读 body 之前**直接 429（含正确签名）。等窗口过期（≤1 分钟）自动恢复；正常投递达不到阈值，反复 429 说明有来源在猜 secret |
| ping/发布后无动作、日志 `忽略：预发布` | 当前 tag 带 `-alpha` 等后缀，`LITETALK_UPDATER_ALLOW_PRERELEASE=0`（默认）；接受预发布就显式设 `1` |
| 日志 `忽略：更新器已关闭` | `LITETALK_UPDATER_ENABLED=0`；排障完成改回 `1` |
| 更新失败 `sudo ... : exit status 1` | sudoers 缺失/未 `chmod 440`，或单元开了 `NoNewPrivileges`（见 §9.1 注） |
| 更新失败 `healthz 未在…内报告版本` | 新版本起不来；已自动回滚。查 `journalctl -u litetalk` 定位启动失败原因 |
| 更新失败 `磁盘可用空间不足` | 清理备份/日志，需 ≥200MB |
| 更新器起不来，8088 被占用 | `ss -ltnp \| grep :8088` 找占用进程；8088 必须空闲（443 归主服务，更新器不碰 443） |
| 启动报 `LISTEN=:443` 或日志提示忽略 TLS 变量 | 残留旧 env：删掉 `DOMAIN`/`NO_TLS`/`TLS_CACHE`，改 `LITETALK_UPDATER_LISTEN=:8088` 后重启 |
| `update.sh` 退出 75 | 已有更新在运行（webhook 或另一个 `--once` 持锁）；等结束后重试 |
| 状态/锁文件 `EACCES` | 曾用 root 跑过更新；`sudo chown -R www:www /var/lib/litetalk-updater`，之后用 `sudo -u www` |
| webhook 投递不稳（GitHub → 国内 8088） | 兜底：release 流水线可 SSH 触发（见 §9.5），或对 release 做 Redeliver |

### 9.5 兜底触发（SSH）

webhook 不可达时，GitHub Actions 在 release 发布后可经 SSH 调服务器上的
`update.sh --tag <tag>`（secrets：`DEPLOY_SSH_KEY` / `DEPLOY_HOST` / `DEPLOY_USER`；
缺任一即跳过，不阻塞 release）。CI 对预发布 tag 自动追加 `--allow-prerelease`；
远端退出码 **75**（已有更新在运行）视为已被覆盖，只记 notice、不算失败。
与 webhook 走同一条流水线（跨进程锁互斥），重复触发不会二次替换。

**端口拓扑**：更新器监听 8088 明文 HTTP；主服务内置 ACME 独占公网 443/80，并在
`127.0.0.1:8077` 补一个只提供 healthz 的环回监听（更新器的版本断言入口，见 §TLS 模式 1），
三者互不冲突；8088 必须空闲，不需要 DNS/证书，8077 只在环回内占用、不需要放行。
不要在同机加反向代理指向 8088——反代会让来源恒为环回，`/status` 被误判为本地访问而公开。
将来若需要 TLS，应另行设计。

### 9.6 回滚

自动回滚由更新器自己完成（healthz 断言失败即恢复备份并重启）。手工回滚：

```bash
sudo systemctl stop litetalk
sudo cp /www/wwwroot/LiteTalk/litetalk-server.bak-<旧版本>-<时间戳> /www/wwwroot/LiteTalk/litetalk-server
sudo systemctl start litetalk
curl -fsS http://127.0.0.1:8077/api/v1/healthz   # version 应为旧版本
# 或直接指定旧 tag 走标准流水线（显式手工操作不受防降级闸限制）：
sudo -u www /www/wwwroot/LiteTalk/update.sh --tag v1.6.1-alpha --allow-prerelease
```

## 10. 生产切换 runbook（面板/孤儿进程 → systemd，维护窗口）

适用于「服务由面板或登录会话启动、没有可靠 supervisor」的存量机器。约 10 分钟。
**先备份，后切换**；任一步失败按 §10.5 回滚。

### 10.1 备份

```bash
PID=$(pgrep -f 'litetalk-server' | head -1)          # 确认是目标进程后再继续
sudo cp -a /www/wwwroot/LiteTalk /www/wwwroot/LiteTalk.bak-$(date +%Y%m%d-%H%M%S)
sudo systemctl stop litetalk 2>/dev/null || true      # 停空转的坏 unit（如果有）
sudo kill "$PID" && sleep 2 && pgrep -f litetalk-server || echo "孤儿进程已停止"
```

### 10.2 生成 /etc/litetalk.env（保留现网运行参数）

```bash
# 从现网进程环境提取 LITETALK_*；迁移已 embed，LITETALK_MIGRATIONS_DIR 必须删除
sudo bash -c "tr '\0' '\n' < /proc/$PID/environ | grep '^LITETALK_' | grep -v '^LITETALK_MIGRATIONS_DIR=' > /etc/litetalk.env"
sudo chmod 600 /etc/litetalk.env
sudo sed -i '/^LITETALK_MIGRATIONS_DIR=/d' /etc/litetalk.env   # 双保险
sudo cat /etc/litetalk.env    # 自查：无密码外泄、无 LITETALK_MIGRATIONS_DIR
```

### 10.3 修正 unit 并切 systemd

`/etc/systemd/system/litetalk.service`：`User=www` / `Group=www` / `WorkingDirectory=/www/wwwroot/LiteTalk`
/ `EnvironmentFile=/etc/litetalk.env` / `ExecStart=/www/wwwroot/LiteTalk/litetalk-server`
/ `Restart=on-failure` / `StartLimitIntervalSec=60` / `StartLimitBurst=10`
（防再次空转重启风暴；`install.sh` 生成的模板已满足）。

```bash
sudo systemctl daemon-reload
sudo systemctl enable --now litetalk
systemctl status litetalk --no-pager     # active (running)，且不再空转重启
```

### 10.4 验证

```bash
curl -fsS http://127.0.0.1:8077/api/v1/healthz        # {"status":"ok","version":"..."}
# MySQL/Redis（若配置）：journalctl -u litetalk | grep -i -E 'mysql|redis'
# SFU/TURN：7845/udp、3478/udp 监听（ss -lunp | grep -E '7845|3478'），客户端进房通话拨测
# 全链路：go run ./cmd/e2eprobe -addr 127.0.0.1:8077
```

### 10.5 回滚方式

```bash
sudo systemctl disable --now litetalk
# 恢复备份二进制后按面板原方式启动
sudo cp -a /www/wwwroot/LiteTalk.bak-<时间戳>/. /www/wwwroot/LiteTalk/
```

切换完成后再按 §9 装更新器、放行 TCP 8088 并配 webhook，用 ping + Redeliver 做演练；
`/status` 用 SSH 端口转发查看。
