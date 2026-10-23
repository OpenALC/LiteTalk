# LiteTalk Server

轻音 LiteTalk 服务端：WS 信令 hub + 自研 Pion SFU + TURN 短期凭证签发 + 账号/社交 REST。
自托管部署看本页即可；**生产上线步骤与配置文件见 [deploy/README.md](deploy/README.md)**（Nginx/coturn/systemd/env 可直接复制）。协议细节见仓库内 `client/README.md` 与源码注释。

## 本地启动

```bash
cd server
go run ./cmd/server                 # 默认监听 :8077
LITETALK_ADDR=:18077 go run ./cmd/server   # 自定义端口
```

| 环境变量 | 默认 | 说明 |
|---|---|---|
| `LITETALK_ADDR` | `:8077` | HTTP/WS 监听地址 |
| `LITETALK_SFU_UDP` | `7845` | SFU ICE-Lite UDP mux 端口（0 = 随机，测试用） |
| `LITETALK_SFU_IP` | `127.0.0.1` | SFU 对外宣告 IP（部署填公网 IP） |
| `LITETALK_SFU_MAX` | `10` | 语音房间人数上限 |
| `LITETALK_TURN_SECRET` | （空） | coturn `static-auth-secret`；空则凭证接口返回 503 |
| `LITETALK_TURN_URLS` | `turn:127.0.0.1:3478` | TURN URL，逗号分隔 |
| `LITETALK_STUN_URLS` | `stun:127.0.0.1:3478` | STUN URL，逗号分隔 |
| `LITETALK_AUTH_MODE` | `full` | `full`：注册/找回需邮箱验证码；`simple`：免验证码（见下） |

健康检查：`curl http://127.0.0.1:8077/api/v1/healthz`

## Auth 模式（full / simple）

| 行为 | `full`（默认） | `simple` |
|---|---|---|
| 注册 | 需邮箱验证码 | 邮箱+密码直接注册 |
| 找回密码 | 需邮箱验证码 | 邮箱+新密码直接重置 |
| 发验证码接口 | 正常发信 | 返回 `verification_not_required` |
| 游客登录 `POST /api/v1/auth/guest` | 不可用（403） | 一键进入，签发临时身份 |

**取舍提示**：`simple` 模式下任何知道你邮箱的人都能重置密码——只为私域部署
（家里 NAS、公司内网）从简而设；公网部署请保持 `full` 并配置发信通道。
客户端会自动读取 `GET /api/v1/config` 适配界面（simple 模式隐藏验证码输入、
显示「游客进入」按钮）。
游客身份：`游客-XXXX` 昵称 + 占位邮箱 + 空密码哈希（不可密码登录），令牌结构
与普通账号一致；客户端以 refresh_token 维持身份（rotation 后需写回持久化，
否则重启即换号）。绑定邮箱转正待后续版本。

## 发信配置（验证码邮件）

验证码投递通道按以下优先级自动选择，启动日志会打一行当前所用通道：

| 优先级 | 通道 | 环境变量 | 适用 |
|---|---|---|---|
| 1 | **Webhook**（自有发码平台） | `LITETALK_MAILER_WEBHOOK`<br>`LITETALK_MAILER_WEBHOOK_TOKEN`（可选） | 商业组织接自有短信/邮件平台 |
| 2 | **SMTP**（标准邮箱） | `LITETALK_SMTP_HOST`<br>`LITETALK_SMTP_PORT`（默认 587，465 走隐式 TLS）<br>`LITETALK_SMTP_USER`<br>`LITETALK_SMTP_PASS`<br>`LITETALK_SMTP_FROM`（必填） | 任意邮箱，建议自有域名并配 SPF/DKIM |
| 3 | **Console**（默认） | 无 | 开发调试：验证码打印到服务端日志 |

SMTP 配置不全（缺 HOST 或 FROM）会自动降级 Console 并在启动日志提示。

### Webhook 对接契约（自定义发码平台）

服务端向 `LITETALK_MAILER_WEBHOOK` 发起 POST：

```http
POST <webhook_url> HTTP/1.1
Content-Type: application/json
Authorization: Bearer <LITETALK_MAILER_WEBHOOK_TOKEN>   # 配置了 token 才带

{"email": "user@example.com", "code": "123456", "purpose": "register"}
```

- `purpose`：`register`（注册验证）| `reset`（密码重置）
- 返回 **2xx** 即视为发送成功；其他状态码视为失败（服务端记日志，不向用户暴露）
- 你的平台收到后自行完成短信/邮件/企业微信等任意投递，10 分钟内送达用户即可
  （验证码有效期 10 分钟）
- 实现参考：`server/internal/mailer/mailer.go` 的 `Webhook`（约 30 行），
  如需其他协议（如阿里云邮件推送 SDK），实现 `mailer.Mailer` 接口并在
  `mailer.New` 中注册即可

## wscat 拨测 join

```bash
npm i -g wscat
wscat -c "ws://127.0.0.1:8077/ws?token=dev-alice"
```

连接后服务端先下发 `hello`，随后逐条粘贴：

```json
{"type":"channel.join","data":{"channel_id":"demo"},"ts":0}
{"type":"voice.state","data":{"speaking":true},"ts":0}
{"type":"channel.leave","data":{"channel_id":"demo"},"ts":0}
```

预期回包：`channel.joined`（成员全量，含 `self`）→ 其他成员收到 `voice.state` → 离开后其余成员收到 `channel.peer_left`。
M2 开发令牌格式 `dev-<用户名>`，无签名校验（M3 换 JWT）。

也可以用仓库自带的 Go 拨测（join + 收 hello + leave 一气呵成）：

```bash
go run ./cmd/wsprobe -addr 127.0.0.1:8077 -token dev-alice -room demo
```

## TURN 短期凭证

```bash
curl -X POST "http://127.0.0.1:8077/api/v1/rtc/credentials?token=dev-alice"
```

返回（coturn `use-auth-secret` / TURN REST API 格式，12h 有效）：

```json
{
  "iceServers": [
    {"urls": ["stun:127.0.0.1:3478"]},
    {"urls": ["turn:127.0.0.1:3478"], "username": "1734543200:alice", "credential": "<base64(HMAC-SHA1(username, TURN_SECRET))>"}
  ],
  "expiresAt": 1734543200
}
```

鉴权：`Authorization: Bearer <token>` 或 `?token=`。需先配置 `LITETALK_TURN_SECRET`（与 coturn 的 `static-auth-secret` 一致）。

## 端到端拨测（部署验证）

`cmd/e2eprobe` 以真实客户端身份走完整链路：REST 注册/登录 → 取 TURN/STUN 凭证
→ WS 信令入房 → Pion PeerConnection 完整 ICE → 50pps 哑 RTP 双向收发计数。
用于部署后或防火墙/网络变更后验证全链路；与 `cmd/loadtest` 的区别是走真实
JWT 鉴权（loadtest 走 dev-token，仅限 `LITETALK_DEV_AUTH=1` 的测试实例），
可对公网生产实例直接拨测。

```bash
# 对公网 HTTPS 实例拨测（simple 模式服务端，自动注册两个探测账号）
go run ./cmd/e2eprobe -addr litetalk.example.com -tls

# 对本机服务端拨测（SFU 宣告 127.0.0.1，需放开环回候选）
LITETALK_AUTH_MODE=simple go run ./cmd/server
go run ./cmd/e2eprobe -addr 127.0.0.1:8077 -loopback
```

- 输出 `结论: PASS` = REST/WS/UDP 全链路正常；`iceServers=N 条` 顺带验证
  rtc/credentials 签发在工作
- `上行包/下行包` 接近相等且 `媒体错误=0` = SFU 双向转发正常
  （末尾少量缺失为停包排空，属正常）
- 需要 simple 模式（依赖免验证码注册）；full 模式服务端请用 wscat 或真实客户端验证

常用参数：`-users N`（同房人数，默认 2）、`-duration 8s`（发包时长）、
`-loopback`（测本机服务端时必开，对应 pion 环回候选）。

## 自验

```bash
go vet ./... && go test -count=1 ./... && go build ./...
```

- `internal/sfu`：双客户端 RTP 逐字节透传、第三人加入重协商、断连停止转发、房间满员（真实 Pion ICE/DTLS/SRTP）
- `internal/rtcapi`：HMAC-SHA1 签名（RFC 2202 向量）、凭证格式、HTTP 鉴权/错误分支
- `internal/signal`：WS 信令 hub（经真实服务端冒烟覆盖）
