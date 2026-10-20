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
| 8077 | TCP | 纯 HTTP API（无 TLS / 前置反代模式；默认只监听本机即可） |
| 7845 | UDP | SFU 媒体（ICE-Lite UDP mux） |
| 7845 | TCP | ICE-TCP（UDP 被封网络直连；`LITETALK_SFU_TCP=0` 可关闭） |
| 3478 | TCP/UDP | 内置 TURN |
| 49152-49200 | UDP | 内置 TURN 中继端口段（`LITETALK_TURN_RELAY_PORT[_END]`） |

云服务器在安全组放行；本机防火墙：

```bash
firewall-cmd --permanent --add-port=443/tcp --add-port=80/tcp \
  --add-port=7845/udp --add-port=7845/tcp \
  --add-port=3478/tcp --add-port=3478/udp \
  --add-port=49152-49200/udp
firewall-cmd --reload
```

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
- 升级前建议备份数据目录：SQLite 在停止服务后复制 `litetalk.db` 即可（WAL 会在正常关闭时
  回放合并）；整个 `/opt/litetalk/data` 一起复制最稳妥。
- 旧部署的宽松权限会在首次启动时自动收紧，不需要手工 chmod。
