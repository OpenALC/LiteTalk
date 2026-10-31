// Web 版信令 WS 传输层：连接 + 指数退避重连 + 事件面 seq 门控（#32）。
// 对照 Go 侧 client/core/signal/ws.go 与 client/core/gateway/tracker.go。
//
// 浏览器差异（与 Go 客户端不同之处）：
//   - WebSocket API 收不到握手失败的具体 HTTP 状态码（一律 onerror/onclose 1006），
//     「401 令牌过期」与「网络不通」无法在传输层区分。策略：每次连接最多做一次
//     预刷新（EnsureAccess 语义）+ 一次失败后强制刷新重试；刷新被明确拒绝时由
//     http.refreshAccess 派 auth:kicked 并清登录态，本层停止重连。
//   - 浏览器不能主动发协议层 Ping（自动回 Pong 由内核处理）。服务端每 25s 发
//     Ping，pong 往返本身维持了双向 NAT 映射，客户端无需应用层心跳。
import { webEmit } from './events'

// ---- 信令消息类型常量（与 api/signal/messages/*.schema.json 的 x-wire-type 对齐）----
export const Sig = {
  Hello: 'hello',
  ServerHello: 'server.hello',
  ClientHello: 'client.hello',
  GatewayResume: 'gateway.resume',
  GatewayInvalidSession: 'gateway.invalid_session',
  ChannelJoin: 'channel.join',
  ChannelJoined: 'channel.joined',
  ChannelLeave: 'channel.leave',
  ChannelPeerLeft: 'channel.peer_left',
  ChannelAudio: 'channel.audio',
  ChannelUnread: 'channel.unread',
  ChatSend: 'chat.send',
  ChatMessage: 'chat.message',
  MessageEdited: 'message.edited',
  MessageDeleted: 'message.deleted',
  PresenceUpdate: 'presence.update',
  ConversationChanged: 'conversation.changed',
  RTCOffer: 'rtc.offer',
  RTCAnswer: 'rtc.answer',
  RTCIce: 'rtc.ice',
  RTCFailed: 'rtc.failed',
  VoiceState: 'voice.state',
  VoiceSpeaking: 'voice.speaking',
  VoicePriority: 'voice.priority',
  VoiceStateRequest: 'voice.state_request',
  VoiceWhisper: 'voice.whisper',
  VoiceWhisperAck: 'voice.whisper_ack',
  SessionKicked: 'session.kicked',
  SessionVoiceTaken: 'session.voice_taken',
  ServerShutdown: 'server.shutdown',
  Error: 'error',
} as const

// 协议版本区间：与服务端共同维护（#29），上限 4（voice.whisper_ack 存在的最低完整版本）。
const PROTOCOL_MIN = 1
const PROTOCOL_MAX = 4

// Envelope 信令信封（api/signal/envelope.schema.json；seq 是协议 2 起的事件序号）。
export interface Envelope {
  type: string
  data?: any
  ts: number
  seq?: number
}

// SignalHandler 下行消息处理器（分发在 WS 消息事件上同步调用，保序；不得长阻塞）。
export type SignalHandler = (env: Envelope) => void

// ---- 重连参数（对齐 Go ws.go 默认值）----
const RECONNECT_MIN_MS = 500
const RECONNECT_MAX_MS = 15_000

// nextBackoff 指数 ×2 封顶，±20% 抖动防雷群（对照 Go nextBackoff）。
function nextBackoff(cur: number): number {
  const next = Math.min(cur * 2, RECONNECT_MAX_MS)
  const jitter = Math.random() * (next / 5)
  return next - next / 10 + jitter
}

// WS close code（服务端契约 §0.5/§0.6）。
const CLOSE_NEW_LOGIN = 4001 // 异地登录踢线
const CLOSE_SESSION_REPLACED = 4000 // 同 sid 半开连接被顶替

// ---- Tracker：seq 门控（client/core/gateway/tracker.go 的逐行移植）----
// 语义保持一致：连续段投递、缺口缓冲、溢出/invalid_session 转全量补拉。

const MAX_PENDING = 512

// Tracker 导出仅供测试（ws 内部使用；语义与 Go gateway.Tracker 一致）。
export class Tracker {
  private sessionID = ''
  private lastSeq = 0
  private pending = new Map<number, Envelope>()
  private awaitBase = false
  onResync: ((reason: string) => void) | null = null

  getSessionID(): string {
    return this.sessionID
  }

  // onServerHello 处理 server.hello：同会话返回 resume 载荷；会话被换触发全量补拉。
  // 首次协商也发 resume{last_seq:0}（服务端环里可能已有 seq>1 的事件，#81）。
  onServerHello(sessionID: string): { sessionID: string; lastSeq: number } | null {
    if (!sessionID) return null // 旧令牌/老服务端：没有可续的会话
    if (this.sessionID && sessionID !== this.sessionID) {
      this.pending.clear()
      this.lastSeq = 0
      this.awaitBase = false
      this.sessionID = sessionID
      this.fireResync('session_changed')
      return null
    }
    this.sessionID = sessionID
    return { sessionID, lastSeq: this.lastSeq }
  }

  // gate 对每条下行信封返回应投递给订阅者的信封（按返回序投递）。
  gate(env: Envelope): Envelope[] {
    if (env.type === Sig.GatewayInvalidSession) {
      return this.handleInvalid('invalid_session', env.data?.latest_seq ?? 0)
    }
    if (!env.seq || env.seq <= 0) return [env] // 语音面/连接控制直通
    if (this.awaitBase) {
      // 溢出后等下一事件定基准：以下一条见到的 seq 为基准并投递（服务端最新 seq 不可知）。
      this.awaitBase = false
      this.lastSeq = env.seq - 1
    }
    if (env.seq <= this.lastSeq) return [] // 已投递过（补发与实时投递重叠）
    this.pending.set(env.seq, env)
    if (this.pending.size > MAX_PENDING) {
      const out = this.drainPending()
      this.awaitBase = true
      this.fireResync('pending_overflow')
      return out
    }
    return this.flush()
  }

  // handleInvalid 续传失败：投递在途事件、基准对齐服务端 latest_seq（0=清零）、触发全量补拉。
  private handleInvalid(reason: string, latestSeq: number): Envelope[] {
    const out = this.drainPending()
    this.lastSeq = latestSeq
    this.awaitBase = false
    this.fireResync(reason)
    return out
  }

  private drainPending(): Envelope[] {
    const out = [...this.pending.values()].sort((a, b) => a.seq! - b.seq!)
    this.pending.clear()
    return out
  }

  private flush(): Envelope[] {
    const out: Envelope[] = []
    for (;;) {
      const next = this.pending.get(this.lastSeq + 1)
      if (!next) return out
      this.pending.delete(this.lastSeq + 1)
      this.lastSeq++
      out.push(next)
    }
  }

  private fireResync(reason: string): void {
    const fn = this.onResync
    if (fn) fn(reason)
  }
}

export interface SignalWSOptions {
  // getURL 每次拨号时构造 WS 地址（web 构建恒为同源 /ws）。
  getURL: () => string
  // getToken 每次拨号前取 access token（REST 刷新后重连自动携带新值）。
  getToken: () => string
  // onTokenStale 握手失败后回调一次：调用方强制刷新令牌再重拨（返回 false = 放弃本次重试）。
  onTokenStale: () => Promise<boolean>
  // onKicked 被服务端踢下线（close 4001 / session.kicked 消息）。reason 为契约取值。
  onKicked: (reason: string) => void
  // onConnected / onDisconnected 连接状态回调。
  onConnected?: () => void
  onDisconnected?: () => void
}

// SignalWS 信令连接：自动重连 + 门控分发。一个登录会话一个实例。
export class SignalWS {
  private opts: SignalWSOptions
  private ws: WebSocket | null = null
  private handlers = new Map<string, Set<SignalHandler>>()
  private anyHandlers = new Set<SignalHandler>()
  private tracker = new Tracker()
  private stopped = true
  private reconnectDelay = RECONNECT_MIN_MS
  // connecting 防重入拨号；authRetried 当前连接的强制刷新重试预算（一条连接一次）。
  private connecting = false
  private authRetried = false
  // resumePending 收到 server.hello 后的 resume 载荷，OnConnect 时机发送
  //（与 Go 一致：首条下行消息处理完才回调 OnConnect，protoVer 就绪）。
  private lastError = { code: '', message: '' }

  constructor(opts: SignalWSOptions) {
    this.opts = opts
    this.tracker.onResync = (reason) => webEmit('data:resync', { reason })
  }

  // start 启动（幂等）。重连循环由 scheduleDial 驱动。
  start(): void {
    if (!this.stopped) return
    this.stopped = false
    this.dial()
  }

  // stop 停止并关闭连接（登出/被踢）。停止后不再重连。
  stop(): void {
    this.stopped = true
    const ws = this.ws
    this.ws = null
    if (ws) {
      try {
        ws.close(1000, 'bye')
      } catch {
        // 已关闭的连接 close 再抛错无害
      }
    }
  }

  isConnected(): boolean {
    return this.ws !== null && this.ws.readyState === WebSocket.OPEN
  }

  // on 注册某类型消息处理器（保序分发；同名多处理器按注册序执行）。
  on(type: string, h: SignalHandler): void {
    let set = this.handlers.get(type)
    if (!set) {
      set = new Set()
      this.handlers.set(type, set)
    }
    set.add(h)
  }

  onAny(h: SignalHandler): void {
    this.anyHandlers.add(h)
  }

  // lastErrorMessage 最近一次服务端 error 帧内容（chat:error 事件用）。
  consumeLastError(): { code: string; message: string } {
    const v = { ...this.lastError }
    this.lastError = { code: '', message: '' }
    return v
  }

  // send 发送一条信令。返回 false = 无可用连接（重连窗口期），调用方自行决定
  // 是否入 outbox（聊天消息的断线重发语义在 signal.ts 里实现）。
  send(type: string, data?: unknown): boolean {
    const ws = this.ws
    if (!ws || ws.readyState !== WebSocket.OPEN) return false
    const env: Record<string, unknown> = { type, ts: Date.now() }
    if (data !== undefined) env.data = data
    try {
      ws.send(JSON.stringify(env))
      return true
    } catch {
      return false
    }
  }

  private dial(): void {
    if (this.stopped || this.connecting) return
    this.connecting = true
    const token = this.opts.getToken()
    const url = `${this.opts.getURL()}?token=${encodeURIComponent(token)}`
    let ws: WebSocket
    try {
      ws = new WebSocket(url)
    } catch {
      this.connecting = false
      this.scheduleReconnect()
      return
    }
    this.ws = ws
    this.authRetried = false

    ws.onopen = () => {
      // 协议协商：老服务端不认识 client.hello 会回 error{bad_message}，
      // 容忍并按 v1 继续（不重连风暴）。
      this.send(Sig.ClientHello, { min: PROTOCOL_MIN, max: PROTOCOL_MAX })
    }

    ws.onmessage = (ev) => {
      let env: Envelope
      try {
        env = JSON.parse(ev.data as string) as Envelope
      } catch {
        return // 坏包丢弃（对齐 Go：drop malformed message）
      }
      if (!env.type) return
      if (env.type === Sig.ServerHello) {
        // server.hello 确认会话后决定是否发 gateway.resume（tracker 语义）。
        const resume = this.tracker.onServerHello(env.data?.session_id ?? '')
        if (resume) this.send(Sig.GatewayResume, { session_id: resume.sessionID, last_seq: resume.lastSeq })
      }
      this.tracker.gate(env).forEach((e) => this.dispatch(e))
      // 首条消息（server.hello 或 hello）之后连接才算就绪：OnConnect 语义
      // 与 Go 一致，业务在此重发 channel.join / outbox。
      if (!this.connecting) return
      this.connecting = false
      this.reconnectDelay = RECONNECT_MIN_MS
      this.opts.onConnected?.()
    }

    ws.onclose = (ev) => {
      const wasOpen = this.ws === ws
      if (this.ws === ws) this.ws = null
      // 连接已结束：复位在连标记，让预算内重拨能通过 dial 的防重入检查
      //（握手即失败时 onmessage 从未跑过，connecting 仍为 true）。
      this.connecting = false
      if (this.stopped) return
      if (ev.code === CLOSE_NEW_LOGIN) {
        this.opts.onKicked('login_elsewhere')
        return
      }
      if (ev.code === CLOSE_SESSION_REPLACED) {
        // 同 sid 半开连接被新连接顶替：新连接已接管，本端停止重连（Go ErrSilentReplace 语义）。
        return
      }
      if (wasOpen) this.opts.onDisconnected?.()
      // 快速失败（握手 401 / 网络不通在传输层不可区分）：预算内强制刷新一次再拨。
      if (!this.authRetried) {
        this.authRetried = true
        this.opts
          .onTokenStale()
          .then((ok) => {
            if (ok && !this.stopped) this.dial()
            else if (!this.stopped) this.scheduleReconnect()
          })
          .catch(() => {
            if (!this.stopped) this.scheduleReconnect()
          })
        return
      }
      this.scheduleReconnect()
    }

    ws.onerror = () => {
      // 错误细节不可得（浏览器限制），交给 onclose 走重连。
    }
  }

  private scheduleReconnect(): void {
    if (this.stopped) return
    this.connecting = false
    const delay = this.reconnectDelay
    this.reconnectDelay = nextBackoff(this.reconnectDelay)
    setTimeout(() => this.dial(), delay)
  }

  private dispatch(env: Envelope): void {
    if (env.type === Sig.Error) {
      // 服务端 error 帧记录最近一条（join 归属与 chat:error 事件由 signal.ts 消费）。
      this.lastError = { code: env.data?.code ?? '', message: env.data?.message ?? '' }
    }
    const set = this.handlers.get(env.type)
    if (set) for (const h of [...set]) h(env)
    for (const h of this.anyHandlers) h(env)
  }
}
