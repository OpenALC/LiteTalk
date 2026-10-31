# #50 Web 客户端 Spike 实测记录

> 目的：在写任何传输层代码之前，用真实浏览器栈验证三条通路，避免整层实现
> 建立在未经证实的假设上。方法与环境见文末。

## 结论总览

| # | 假设 | 结论 |
|---|---|---|
| 1 | 浏览器经 `/api/v1/rtc/credentials` 拿 ICE 配置后能与现有 SFU 建连并收到服务端转发的音频 | **成立**（附实测数据） |
| 2 | 服务端对浏览器 `rtc.offer` 的 SDP 没有特殊要求 | **成立**（pion 原样 SetRemoteDescription） |
| 3 | WS 网关 `client.hello` 接受同一个 access token（无需特别鉴权头） | **成立**（`?token=` 即可） |

## 1. ICE / SFU 连接与音频转发 — 成立

流程：`POST /api/v1/rtc/credentials`（Bearer）→ `{"iceServers":[{"urls":["stun:…"]},{"urls":["turn:…"],"username":…,"credential":…}],"expiresAt":…}` →
`channel.join` → `channel.joined` → `RTCPeerConnection({iceServers})` +
`addTrack(振荡器轨)` + `onnegotiationneeded` 里 `createOffer` → `rtc.offer` →
`rtc.answer` → 双向 `rtc.ice` → `pc.connectionState == "connected"`。

两个浏览器页签（用户 A=host、B=listener）加入同一语音频道后的实测数据：

- 服务端日志：`路由 A → B 建立`、`路由 B → A 建立`、双向 `上行轨道就绪 kind=audio`。
- B 端 `pc.getStats()`：inbound audio `bytesReceived=172368`、`packetsReceived=2144`（A 的下行流）。
- B 端经 `AudioContext + AnalyserNode` 实测远端波形峰值 **0.3055**——与 A 端
  振荡器增益 0.3 一致，证明是 A 的音频经 SFU 转发而来，不是空包。
- `voice.speaking` 事件按服务端 RFC 6464 电平判定到达（`speaking:true`）：
  **说话指示不需要客户端上报**，浏览器原生 Opus + Chrome 默认带的
  `ssrc-audio-level` 扩展即可驱动（协议 v2 路径）。

## 2. 浏览器 SDP 兼容性 — 成立

服务端 SFU（`server/internal/sfu/peer.go`）对初始 offer 就是 pion 的
`SetRemoteDescription`，对 SDP 内容无自有解析。Chrome 生成的 offer（与 pion
生成的结构差异很大）原样接受：

```sdp
m=audio 49175 UDP/TLS/RTP/SAVPF 111
a=extmap-allow-mixed
a=extmap:1 urn:ietf:params:rtp-hdrext:ssrc-audio-level
a=rtpmap:111 opus/48000/2
a=fmtp:111 minptime=10;useinbandfec=1
c=IN IP4 127.0.0.1          （host candidate 之外还有 mDNS .local 与 relay 候选）
```

mDNS 主机候选、`extmap-allow-mixed`、transport-cc 扩展都不需要处理，
ICE 照常走通（本机还同时用上了 srflx 与 relay 候选）。

**唯一要注意的方向语义**（照抄 Go 客户端 `rtc/peer.go` 的既有约定）：
初始 offer 由客户端发**一次**；初始 answer 之后 SFU 主导重协商
（`OnNegotiationNeeded → rtc.offer S2C`），浏览器侧收到 `rtc.offer` 的处理是
`setRemoteDescription(offer) → createAnswer → 发 rtc.answer`，绝不再发自己的
`rtc.offer`（SFU 会拒绝：`重复的初始 rtc.offer`）。实测 SFU 重协商 offer
（给 B 补 A 的下行轨，1685 字节）→ 浏览器 answer → `ontrack` 触发，路径完整。

## 3. WS 鉴权与协商 — 成立

`GET /ws?token=<access_jwt>` 握手（与 Go 客户端同一 query 语义，无别的鉴权头）→
连上后发 `client.hello {min:1, max:4}` → 回 `server.hello {version:4,
session_id:…}` → 发 `gateway.resume {session_id, last_seq:0}` 服务端接受。
`hello {user:{…}}` 初始数据同连接到达。access token 过期的刷新语义沿用 REST
的 `/auth/refresh`（WS 握手 401 时刷新一次再重拨，同 Go 侧 ws.go 的预算逻辑）。

seq 语义核对（对 `ws.ts` 续传重要）：事件面消息带 `seq` 且进补发环；
**`voice.speaking` 明确 seq=0 直通**（`server/internal/signal/gateway.go` 头部
注释：高频瞬时，不进环），语音面/连接控制（rtc.*、voice.state 等）seq<=0。
`ws.ts` 的门控照抄 `client/core/gateway/tracker.go` 即可：`seq<=0` 直通，
`seq>0` 按 lastSeq 连续段投递、缺口缓冲、溢出/invalid_session 转 `/sync` 全量补拉。

## 附带发现（影响 web 实现的坑）

1. **AudioContext 自动播放策略**：非手势路径创建的 AudioContext 初始为
   suspended，必须 `resume()`；否则振荡器（测试音源）与远端流分析全为静音。
2. **Chromium 远端流 WebAudio 静音怪癖**：`createMediaStreamSource(远端流)`
   直接接 AnalyserNode 读到全零——需先把流挂到一个 `<audio>` 元素（muted 即可）
   拉动媒体管线后再做 WebAudio 分析。Web 客户端的远端播放本来就走
   `<audio>` 元素，说话指示分析要沿用同一条链。
3. **credentials 响应字段是 camelCase**：`iceServers` / `expiresAt`（与
   `RTCConfiguration` 字段对齐，生成物口径），不是 snake_case。
4. 邀请链接进房（验收目标）依赖的能力全部在 REST + WS 层，spike 未发现障碍。

## 未完整验证的边界

- **`gateway.resume` 补发环**：只验证了 resume 被接受、同会话重连协商正常；
  缺口补发路径没有实测（需要制造断线窗口 + 事件）。风险低：逻辑照抄
  tracker.go，且服务端 `Replay` 有完整测试。
- **Firefox**：只实测了 Chromium 内核。两端均为标准 WebRTC API，无
  webkit 专有路径，风险低。
- **TURN 中继连通性**：本机环境 host/srflx/relay 候选都能收集并连上；
  公网 relay 互通属部署面，不在本 spike 范围。

## Spike 环境与方法

- 服务端：worktree 内 `cd server && go run ./cmd/server`，SQLite +
  `LITETALK_AUTH_MODE=simple`，`LITETALK_SFU_UDP=27845`、`LITETALK_TURN_PORT=33478`
  （默认端口被本机其他进程占用），REST/WS 监听 `127.0.0.1:8077`。
- 页面：8078 端口起了一个**仅 spike 用**的反代静态服务器（Go `httputil.ReverseProxy`
  把 `/api/*` 与 `/ws` 转发到 8077），让测试页与 API 同源——与最终「服务端托管
  Web 客户端」的部署形态一致，CORS 不构成变量。反代与测试页都在仓库外
  （`C:\devlopment\code\lt-spike\`），不进提交。
- 浏览器：真实 Chromium 内核（ZCode 内置浏览器）两个页签分别登录 host /
  listener；上行音源用 AudioContext 振荡器生成的 MediaStream 代替麦克风。
- 未连任何远程/生产服务；测试账号 `a2@spike.test` / `b2@spike.test` 只存在于
  本机 SQLite（`server/data/`，已 gitignore）。
