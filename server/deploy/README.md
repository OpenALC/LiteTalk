# LiteTalk 服务端上线指南（RockyLinux 9.5）

单机部署：Nginx（TLS）+ Go 服务端 + MySQL + Redis + coturn。
按顺序执行即可；配置文件都在本目录，复制上服务器后改占位符。

## TLS 三种模式（#26）

| 模式 | 启动方式 | TLS 谁终结 | 适用 |
|---|---|---|---|
| 1 内置 ACME 自动签发 | `litetalk-server --domain voice.example.com`（或 `LITETALK_DOMAIN`） | **Go 进程自己**：443 TLS-ALPN-01 + 80 HTTP-01/301，证书自动申请与续期 | 一键自托管，不装 nginx |
| 2 前置反代 | `litetalk-server --behind-proxy` | nginx / Caddy（本目录 `nginx/litetalk.conf`） | 与既有站点共存、复用已有证书体系 |
| 3 纯 HTTP | 都不设 | 无 TLS | 仅开发/内网，启动日志会提示 |

**模式 1 要点**：

- 证书缓存默认 `$LITETALK_DATA_DIR/autocert`（权限 0700，私钥不全局可读），autocert 自动续期，无需 cron。
- 443 是特权端口：root 运行，或非 root 时在 systemd 单元启用
  `AmbientCapabilities=CAP_NET_BIND_SERVICE`（`systemd/litetalk.service` 里有示例）。
- `LITETALK_ADDR` 未显式配置时 HTTPS 自动改监听 `:443`；显式配置过就不再自动改端口。
- 签发域名必须是 `--domain` 单域名（HostPolicy 白名单）；其他 SNI 一律拒绝，防止被 CA 限流。
- 需要公网 80/443 可达（Let's Encrypt 验证）；安全组/防火墙放行这两个端口。

**模式 2 要点**：

- 服务端**不**占 80/443，只监听 `LITETALK_ADDR`（默认 `:8077` 纯 HTTP）。
- 必须配置 `LITETALK_TRUSTED_PROXIES`（反代的 IP/CIDR）：只有 RemoteAddr 命中才采信
  `X-Real-IP`，否则限流/审计记的是反代 IP。伪造的 X-Real-IP 永远不会被采信。
- 反代配置见 `nginx/litetalk.conf`（含 `X-Real-IP` 与 WebSocket 升级头）。

## 1. 放行端口

| 端口 | 协议 | 用途 |
|---|---|---|
| 443 | TCP | Nginx（REST + WSS） |
| 80 | TCP | 证书签发/跳转 |
| 3478 | TCP/UDP | coturn STUN/TURN |
| 49152-49200 | UDP | coturn 中继端口段 |
| 7845 | UDP | SFU 媒体（ICE-Lite） |

云服务器在安全组放行；本机防火墙：

```bash
firewall-cmd --permanent --add-port=443/tcp --add-port=80/tcp \
  --add-port=3478/tcp --add-port=3478/udp --add-port=7845/udp \
  --add-port=49152-49200/udp
firewall-cmd --reload
```

## 2. MySQL / Redis

```bash
sudo dnf install mysql-server redis
sudo systemctl enable --now mysqld redis

# 建库建用户（密码换成自己的）
sudo mysql -e "CREATE DATABASE litetalk CHARACTER SET utf8mb4;
  CREATE USER 'litetalk'@'localhost' IDENTIFIED BY 'CHANGE_ME';
  GRANT ALL ON litetalk.* TO 'litetalk'@'localhost';"
```

## 3. 服务端二进制

Release 页下载 `litetalk-server-linux-<arch>.tar.gz`（amd64/arm64）：

```bash
sudo useradd -r -s /sbin/nologin litetalk
sudo mkdir -p /opt/litetalk/server
sudo tar -xzf litetalk-server-linux-amd64.tar.gz -C /opt/litetalk/server
```

迁移 SQL 已内嵌进二进制（embed.FS），升级时**不再需要解压 `migrations/` 目录**；多实例同时启动时自动用 MySQL `GET_LOCK` 互斥，只迁移一次。

环境变量（**逐项改掉 CHANGE_ME**）：

```bash
sudo cp env/litetalk.env.example /etc/litetalk.env
sudo chmod 600 /etc/litetalk.env
sudo vi /etc/litetalk.env
```

systemd：

```bash
sudo cp systemd/litetalk.service /etc/systemd/system/
sudo systemctl daemon-reload
sudo systemctl enable --now litetalk
```

## 4. coturn

```bash
sudo dnf install coturn
sudo cp coturn/turnserver.conf /etc/coturn/turnserver.conf
# 改两处：external-ip=公网IP、static-auth-secret=与 LITETALK_TURN_SECRET 一致
sudo systemctl enable --now coturn
```

## 5. Nginx

```bash
sudo dnf install nginx
sudo cp nginx/litetalk.conf /etc/nginx/conf.d/
# 改 server_name 与证书路径（acme.sh --issue -d 你的域名 --nginx）
sudo nginx -t && sudo systemctl enable --now nginx
```

## 6. 验证清单

```bash
# REST 健康检查
curl https://你的域名/api/v1/healthz          # {"status":"ok","version":"..."}

# WS 信令（开发令牌仅在 LITETALK_DEV_AUTH=1 时可用；生产用客户端登录）
curl -i "https://你的域名/ws?token=x"         # 401 = 路由通、鉴权在工作

# TURN 凭证签发（登录后带 Bearer 令牌）
curl -X POST https://你的域名/api/v1/rtc/credentials \
  -H "Authorization: Bearer <access_token>"   # 返回 iceServers JSON
```

全链路端到端拨测（simple 模式；在任意可达机器上执行，自动注册两个探测
账号并双向打流，覆盖 REST/WS/ICE/UDP/SFU 转发）：

```bash
go run ./cmd/e2eprobe -addr <公网IP或域名>:8077   # 结论 PASS = 全链路通
```

上线自查：`LITETALK_DEV_AUTH` 未开启；`LITETALK_TURN_SECRET`/`LITETALK_JWT_SECRET`/数据库密码均为强随机值；`LITETALK_SFU_IP` 为公网 IP。
