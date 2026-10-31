// Web 版信令业务层：房间/成员状态、断线重入、聊天 outbox、信令 → store 事件映射。
// 职责对照 Go 侧 client/core/voice/session.go（会话编排）+ app.go（事件转发）；
// 事件名与载荷形状与 app.go 的 emit 口径逐字对齐（stores 是共同消费方）。
import type { AttachmentView, PeerView } from '../types'
import { webEmit } from './events'
import { apiPost, getAccessToken, refreshAccess } from './http'
import { VoicePeer, type ICEServerLike } from './rtc'
import { SignalWS, Sig, type Envelope } from './ws'

// WS 地址：web 构建恒同源（服务端托管静态资源），HTTP 升 WS。
function wsURL(): string {
  const proto = location.protocol === 'https:' ? 'wss:' : 'ws:'
  return `${proto}//${location.host}/ws`
}

// pendingChat 聊天 outbox 条目（断线/发送失败时保留，重连后同 nonce 重发；
// 服务端按 (channel_id, user_id, nonce) 幂等去重）。字段与 Go pendingChat 对齐。
interface PendingChat {
  channelID: string
  content: string
  nonce: string
  replyTo: string
  mentions: string[]
  attachmentIDs: string[]
}

// pendingJoin 已发出、未被 channel.joined / error 结算的进房请求（#91 错误归属）。
interface PendingJoin {
  channelID: string
  seq: number
}

const MAX_PENDING_JOINS = 16

// 进房专用错误码 / 共享错误码（Go session.go isJoinOnlyErrorCode / isJoinSharedErrorCode）。
const JOIN_ONLY_CODES = new Set(['not_found', 'already_in_channel'])
const JOIN_SHARED_CODES = new Set(['forbidden', 'bad_message', 'rate_limited', 'quota_exceeded', 'bad_password'])

// newNonce 16 字节 crypto/rand 的 hex（对照 Go newChatNonce，32 字符 ≤64 上限）。
function newNonce(): string {
  const b = new Uint8Array(16)
  crypto.getRandomValues(b)
  return Array.from(b, (x) => x.toString(16).padStart(2, '0')).join('')
}

// resolveURL 相对能力 URL → 绝对地址（同源部署下 location.origin 即服务端基址）。
function resolveURL(rel: string): string {
  if (!rel) return rel
  try {
    return new URL(rel, location.origin).toString()
  } catch {
    return rel
  }
}

// attachmentViewsFromSignal WS chat.message 附件载荷 → 桥接视图（url 绝对化）。
function attachmentViews(list: any[] | undefined): AttachmentView[] | undefined {
  if (!list?.length) return undefined
  return list.map((at) => ({
    id: String(at.id ?? ''),
    filename: at.filename ?? '',
    size: Number(at.size ?? 0),
    content_type: at.content_type ?? '',
    width: Number(at.width ?? 0),
    height: Number(at.height ?? 0),
    url: resolveURL(at.url ?? ''),
    ...(at.thumbnail_url ? { thumbnail_url: resolveURL(at.thumbnail_url) } : {}),
  }))
}

export interface SignalCallbacks {
  // onKicked 被踢下线（session.kicked / close 4001）。reason 为契约取值。
  onKicked: (reason: string) => void
  // onConnectionStateChange 信令连接状态（auth:session 事件的来源）。
  onConnectionStateChange: (connected: boolean) => void
}

export class SignalSession {
  private ws: SignalWS
  private peer: VoicePeer
  private callbacks: SignalCallbacks

  // ---- 房间/成员状态（mu 语义由单线程事件循环保证，无需锁）----
  private members = new Map<string, PeerView>()
  private _selfID = ''
  private _roomID = ''
  private pendingJoins: PendingJoin[] = []
  private joinSeq = 0
  private roomPassword = ''
  private protoVer = 0
  private _connected = false

  // ---- 语音状态 ----
  private muted = false
  private deafened = false
  private whisperWant: string[] = []
  private whisperSeq = 0
  // whisperMaybe 服务端可能仍挂着非空目标（显式下发过至少一次）：入房同步
  // 即使 want 为空也要发一条空数组兜底；从未用过悄悄话时不发（Go 同款语义）。
  private whisperMaybe = false
  private iceServers: ICEServerLike[] = []
  // statsTimer 周期统计（入房启动，离房停止；voice:stats 载荷是 JSON 字符串）。
  private statsTimer: number | null = null

  // ---- 聊天 outbox ----
  private outbox: PendingChat[] = []

  constructor(callbacks: SignalCallbacks) {
    this.callbacks = callbacks
    this.peer = new VoicePeer({
      onLocalOffer: (sdp) => {
        this.send(Sig.RTCOffer, { sdp })
      },
      onLocalAnswer: (sdp) => {
        this.send(Sig.RTCAnswer, { sdp })
      },
      onLocalICE: (c) => {
        // wire 字段名沿用 WebRTC 标准（sdpMid/sdpMLineIndex/usernameFragment）。
        const wire: Record<string, unknown> = { candidate: c.candidate }
        if (c.sdpMid != null) wire.sdpMid = c.sdpMid
        if (c.sdpMLineIndex != null) wire.sdpMLineIndex = c.sdpMLineIndex
        if (c.usernameFragment != null) wire.usernameFragment = c.usernameFragment
        this.send(Sig.RTCIce, wire)
      },
      onConnectionState: (state) => {
        // 连接建立/断开即刻刷新统计；ICE 失败由服务端 rtc.failed 驱动重建（#19）。
        if (state === 'connected') this.startStats()
      },
    })
    this.ws = new SignalWS({
      getURL: wsURL,
      getToken: getAccessToken,
      onTokenStale: async () => {
        // 握手失败的浏览器不可分辨性（ws.ts 注释）：强制刷新一次令牌再拨。
        // 刷新被明确拒绝时 http.refreshAccess 已派 auth:kicked 并清令牌，
        // 这里返回 false 停止本次重试（退避循环继续跑，登录页会接管）。
        try {
          await refreshAccess()
          return true
        } catch {
          return false
        }
      },
      onKicked: (reason) => this.callbacks.onKicked(reason),
      onConnected: () => this.handleConnect(),
      onDisconnected: () => this.handleDisconnect(),
    })
    this.registerHandlers()
  }

  // ---- 生命周期 ----

  // start 建立信令连接（登录后调用；幂等）。
  start(): void {
    this.ws.start()
  }

  // stop 断开信令并拆链路（登出/被踢）。连接对象保留，重登即 start。
  stop(): void {
    this.ws.stop()
    this.stopStats()
    this.peer.teardown()
    this.resetRoomState()
    this.outbox = []
    this.protoVer = 0
    if (this._connected) {
      this._connected = false
      this.callbacks.onConnectionStateChange(false)
    }
  }

  get connected(): boolean {
    return this._connected
  }

  get protocolVersion(): number {
    return this.protoVer
  }

  get selfID(): string {
    return this._selfID
  }

  get roomID(): string {
    return this._roomID
  }

  // ---- 连接状态 ----

  private handleConnect(): void {
    this._connected = true
    this.callbacks.onConnectionStateChange(true)
    // 重连后若仍在房间：重发 channel.join（channel.joined 会重建媒体链路）。
    // 同样登记 pending——重入也可能被拒（#91），不登记会让异步 error 弹错对象。
    if (this._roomID) {
      this.beginJoin(this._roomID)
      this.send(Sig.ChannelJoin, { channel_id: this._roomID, password: this.roomPassword })
    }
    this.flushOutbox()
  }

  private handleDisconnect(): void {
    this._connected = false
    this.callbacks.onConnectionStateChange(false)
    // 拆媒体链路：连接没了，SFU peer 已被服务端清理。
    this.stopStats()
    this.peer.teardown()
  }

  // ---- 发送 ----

  private send(type: string, data?: unknown): boolean {
    return this.ws.send(type, data)
  }

  // prefetchICE 预取 ICE 凭证（进房前调用；失败留空缓存降级直连，同 Go #13 语义）。
  async prefetchICE(): Promise<void> {
    try {
      const r = await apiPost<{ iceServers?: ICEServerLike[] }>('/rtc/credentials', {}, { auth: true })
      this.iceServers = (r.iceServers ?? []).map((s) => ({
        urls: s.urls ?? [],
        ...(s.username ? { username: s.username } : {}),
        ...(s.credential ? { credential: s.credential } : {}),
      }))
    } catch {
      // 503（TURN 未配置）/超时：host/srflx 候选仍可直连，不阻断进房。
      this.iceServers = []
    }
  }

  // joinChannel 进房（乐观写 roomID 供断线重连恢复；服务端拒绝是异步 error 帧）。
  // 采集**不阻塞**进房：getUserMedia 在权限弹窗期间永不 settle，await 它会让
  // 入房挂死且无反馈。这里只发 channel.join；采集并行推进，channel.joined 的
  // ensurePeer 给弹窗一个有限窗口，明确失败经 voice:unavailable 上报并降级只收。
  async joinChannel(channelID: string, password: string): Promise<void> {
    await this.prefetchICE()
    this.roomPassword = password
    this.beginJoin(channelID)
    void this.peer.startCapture()
    if (!this.send(Sig.ChannelJoin, { channel_id: channelID, password })) {
      this.abortJoin(channelID, true)
      throw new Error('信令未连接，进房请求未发出')
    }
  }

  private beginJoin(channelID: string): void {
    this.joinSeq++
    this.pendingJoins.push({ channelID, seq: this.joinSeq })
    if (this.pendingJoins.length > MAX_PENDING_JOINS) {
      this.pendingJoins = this.pendingJoins.slice(-MAX_PENDING_JOINS)
    }
    this._roomID = channelID
  }

  // abortJoin 摘掉一次进房意图。rollbackRoom=true 且 roomID 仍是该频道时回滚。
  private abortJoin(channelID: string, rollbackRoom: boolean): void {
    const idx = this.pendingJoins.findIndex((p) => p.channelID === channelID)
    if (idx >= 0) this.pendingJoins.splice(idx, 1)
    if (rollbackRoom && this._roomID === channelID) {
      this._roomID = ''
      this.roomPassword = ''
    }
  }

  // confirmJoin channel.joined 回执结算（广播帧不改状态，返回是否命中本端请求）。
  private confirmJoin(channelID: string): boolean {
    const idx = this.pendingJoins.findIndex((p) => p.channelID === channelID)
    if (idx < 0) return false
    this.pendingJoins.splice(idx, 1)
    if (!this._roomID) this._roomID = channelID // 发送失败清过 roomID、更早 join 实际成功
    return true
  }

  // settlePendingJoinHead 结算最早未完成的 join（error 帧无关联 id，按 FIFO 归属，#91）。
  private settlePendingJoinHead(rollback: boolean): { channelID: string; consumed: boolean; rolledBack: boolean } {
    const head = this.pendingJoins.shift()
    if (!head) return { channelID: '', consumed: false, rolledBack: false }
    if (!rollback) return { channelID: head.channelID, consumed: true, rolledBack: false }
    if (this._roomID !== head.channelID) {
      // 快速切房：失败的是旧 join，不能回滚新房。
      return { channelID: head.channelID, consumed: true, rolledBack: false }
    }
    this._roomID = ''
    this.roomPassword = ''
    this.teardownRoomLocal()
    return { channelID: head.channelID, consumed: true, rolledBack: true }
  }

  // leaveChannel 退房：先拆本地再发 leave（发送失败也不留幽灵成员）。
  async leaveChannel(): Promise<void> {
    const room = this._roomID
    this.pendingJoins = []
    this._roomID = ''
    this.roomPassword = ''
    this.teardownRoomLocal()
    if (!room) return
    this.send(Sig.ChannelLeave, { channel_id: room })
  }

  // teardownRoomLocal 拆房间态（媒体链路 + 成员表 + 统计）。不发 channel.leave。
  private teardownRoomLocal(): void {
    this.stopStats()
    this.peer.teardown()
    this.members.clear()
    this.whisperWant = []
  }

  private resetRoomState(): void {
    this.members.clear()
    this._selfID = ''
    this._roomID = ''
    this.pendingJoins = []
    this.roomPassword = ''
    this.muted = false
    this.deafened = false
    this.whisperWant = []
    this.whisperSeq = 0
  }

  // ---- 聊天 outbox（#31：断线保留 nonce，重连同 nonce 重发，服务端幂等）----

  // sendChat 入队并立即尝试发送；入队先于发送判定（失败时条目已在 outbox 里）。
  sendChat(channelID: string, content: string, replyTo: string, mentions: string[], attachmentIDs: string[]): void {
    const item: PendingChat = {
      channelID,
      content,
      nonce: newNonce(),
      replyTo,
      mentions,
      attachmentIDs,
    }
    this.outbox.push(item)
    this.flushOutbox()
  }

  private flushOutbox(): void {
    if (!this.outbox.length) return
    const remaining: PendingChat[] = []
    for (const item of this.outbox) {
      const data: Record<string, unknown> = {
        channel_id: item.channelID,
        type: 'text',
        content: item.content,
      }
      if (this.protoVer >= 2) {
        data.nonce = item.nonce
        if (item.replyTo) data.reply_to = item.replyTo
        if (item.mentions.length) data.mentions = item.mentions
        if (item.attachmentIDs.length) data.attachment_ids = item.attachmentIDs
      }
      if (!this.send(Sig.ChatSend, data)) remaining.push(item)
    }
    this.outbox = remaining
  }

  // ---- 语音操作 ----

  setMute(muted: boolean): void {
    this.muted = muted
    this.peer.applyMute(muted)
    // voice.state 上行不带 user_id（服务端补来源）；协议 2 起不再报 speaking。
    this.send(Sig.VoiceState, { mute: muted, deafen: this.deafened })
  }

  setDeafen(deafened: boolean): void {
    this.deafened = deafened
    this.peer.applyDeafen(deafened)
    this.send(Sig.VoiceState, { mute: this.muted, deafen: deafened })
  }

  get isMuted(): boolean {
    return this.muted
  }

  get isDeafened(): boolean {
    return this.deafened
  }

  setPeerVolume(uid: string, gain: number): void {
    this.peer.setPeerVolume(uid, gain)
  }

  // setWhisperTargets 悄悄话目标（协议 ≥3 才真正下发；空数组 = 取消）。
  setWhisperTargets(targets: string[]): void {
    if (this.protoVer < 3) return
    this.whisperWant = targets.filter((t) => t !== '')
    this.whisperMaybe = true // 服务端可能持有非空目标，入房同步要发空数组兜底
    // seq 随每次显式下发递增，服务端 voice.whisper_ack 原样回传（#116 v4）。
    this.whisperSeq++
    this.send(Sig.VoiceWhisper, { targets: this.whisperWant, seq: this.whisperSeq })
  }

  requestVoiceState(channelID: string, targetID: string, mute: boolean, deafen: boolean): void {
    this.send(Sig.VoiceStateRequest, { channel_id: channelID, target_id: targetID, mute, deafen })
  }

  getVoiceMembers(): PeerView[] {
    return [...this.members.values()]
  }

  getVoiceState(): { channel_id: string; muted: boolean; deafened: boolean } {
    return { channel_id: this._roomID, muted: this.muted, deafened: this.deafened }
  }

  // ---- 统计（voice:stats 载荷为 JSON 字符串，与 Go OnVoiceStats 一致）----

  private startStats(): void {
    if (this.statsTimer !== null) return
    const tick = () => {
      if (!this._roomID) return
      void this.peer.statsSnapshot(this.members.size).then((s) => {
        webEmit('voice:stats', JSON.stringify(s))
      })
    }
    tick()
    this.statsTimer = window.setInterval(tick, 5000)
  }

  private stopStats(): void {
    if (this.statsTimer !== null) {
      clearInterval(this.statsTimer)
      this.statsTimer = null
    }
  }

  // ---- 信令处理（对照 Go session.registerHandlers + app.go 事件转发）----

  private registerHandlers(): void {
    const ws = this.ws

    ws.on(Sig.Hello, (env) => {
      if (env.data?.user?.id) this._selfID = String(env.data.user.id)
    })

    ws.on(Sig.ChannelJoined, (env) => {
      const d = env.data
      if (!d?.channel_id) return
      const matched = this.confirmJoin(String(d.channel_id))
      // 迟到回执（#99）：roomID 已不指向该频道就整帧丢弃——不重建成员表。
      if (this._roomID !== String(d.channel_id)) return
      // 成员表整体重建（channel.joined 是全量快照，含重复重发）。
      this.members.clear()
      const list: PeerView[] = []
      for (const m of d.members ?? []) {
        const pv: PeerView = {
          user_id: String(m.user_id ?? ''),
          nickname: m.nickname ?? '',
          tag: m.tag ?? '',
          speaking: !!m.speaking,
          mute: !!m.mute,
          deafen: !!m.deafen,
          priority: !!m.priority,
        }
        this.members.set(pv.user_id, pv)
        list.push(pv)
      }
      const selfID = String(d.self?.user_id ?? '')
      if (selfID) this._selfID = selfID
      // 入房即补发完整语音状态：重连/重入房后服务端内存可能已丢（#117 语义）。
      this.send(Sig.VoiceState, { mute: this.muted, deafen: this.deafened })
      if (this.protoVer >= 3 && (this.whisperWant.length > 0 || this.whisperMaybe)) {
        // 目标同步走 seq=0 语义（服务端不回 ack）：入房重建不是一次用户请求。
        this.send(Sig.VoiceWhisper, { targets: this.whisperWant, seq: 0 })
      }
      webEmit('voice:self', selfID)
      for (const m of list) webEmit('voice:peer-join', m)
      // 建连：无存活 PC 才建（他人入房广播不动媒体链路，与 Go 一致）。
      void this.peer.ensurePeer(this.iceServers)
      this.startStats()
    })

    // channel.audio（#46）：web 不经自研编码器，码率/帧长由浏览器栈管理，不消费。

    ws.on(Sig.RTCOffer, (env) => {
      // SFU 主导的重协商：应答 rtc.answer（下行 offer 只应答、绝不反向 offer）。
      if (!env.data?.sdp) return
      void this.peer.handleRemoteOffer(String(env.data.sdp)).then((answer) => {
        if (answer) this.send(Sig.RTCAnswer, { sdp: answer })
      })
    })

    ws.on(Sig.RTCAnswer, (env) => {
      if (env.data?.sdp) void this.peer.handleRemoteAnswer(String(env.data.sdp))
    })

    ws.on(Sig.RTCIce, (env) => {
      if (!env.data?.candidate) return
      const c: RTCIceCandidateInit = { candidate: String(env.data.candidate) }
      if (env.data.sdpMid != null) c.sdpMid = String(env.data.sdpMid)
      if (env.data.sdpMLineIndex != null) c.sdpMLineIndex = Number(env.data.sdpMLineIndex)
      if (env.data.usernameFragment != null) c.usernameFragment = String(env.data.usernameFragment)
      void this.peer.addRemoteICE(c)
    })

    ws.on(Sig.RTCFailed, (env) => {
      // ICE 失败重建（#19）：拆 PC 后重发 channel.join；channel.joined 见无 PC 会重建。
      const room = this._roomID
      const failedID = env.data?.channel_id ? String(env.data.channel_id) : ''
      if (!room || (failedID && failedID !== room)) return
      if (this.peer.isConnected()) return // 迟到的失败通知，链路已健康
      this.peer.teardown()
      this.beginJoin(room)
      this.send(Sig.ChannelJoin, { channel_id: room, password: this.roomPassword })
      webEmit('rtc:failed', { channel_id: room })
    })

    ws.on(Sig.VoiceState, (env) => {
      const d = env.data
      if (!d?.user_id) return
      const uid = String(d.user_id)
      const m = this.members.get(uid)
      if (!m) return
      if (typeof d.mute === 'boolean') m.mute = d.mute
      if (typeof d.deafen === 'boolean') m.deafen = d.deafen
      if (typeof d.speaking === 'boolean') m.speaking = d.speaking
      webEmit('voice:speaking', { user_id: uid, speaking: m.speaking, mute: m.mute, deafen: m.deafen })
    })

    ws.on(Sig.VoiceSpeaking, (env) => {
      const d = env.data
      if (!d?.user_id) return
      const uid = String(d.user_id)
      const m = this.members.get(uid)
      const self = uid === this._selfID
      if (m) m.speaking = !!d.speaking
      // 自己的 mute/deafen 以本地状态为准（StateTable 无 self 条目，Go 同款处理）。
      webEmit('voice:speaking', {
        user_id: uid,
        speaking: !!d.speaking,
        mute: self ? this.muted : (m?.mute ?? false),
        deafen: self ? this.deafened : (m?.deafen ?? false),
      })
    })

    ws.on(Sig.VoicePriority, (env) => {
      const d = env.data
      if (!d?.user_id || !d.channel_id) return
      if (String(d.channel_id) !== this._roomID) return
      const m = this.members.get(String(d.user_id))
      if (!m) return
      m.priority = !!d.priority
      webEmit('voice:priority', { user_id: m.user_id, priority: m.priority })
    })

    ws.on(Sig.ChannelPeerLeft, (env) => {
      const d = env.data
      if (!d?.user_id) return
      const uid = String(d.user_id)
      this.members.delete(uid)
      webEmit('voice:peer-leave', uid)
    })

    ws.on(Sig.VoiceStateRequest, (env) => {
      const d = env.data
      if (!d?.from?.id) return
      webEmit('voice:state_request', {
        from_id: String(d.from.id),
        nickname: d.from.nickname ?? '',
        mute: !!d.mute,
        deafen: !!d.deafen,
      })
    })

    ws.on(Sig.VoiceWhisperAck, (env) => {
      const d = env.data
      if (!d || d.seq <= 0 || !this._roomID) return
      // ack 驱动悄悄话指示条（hotkey:whisper 载荷，web 无热键，恒 pressed=true）。
      webEmit('hotkey:whisper', { pressed: true, active: !!d.active, supported: this.protoVer >= 3 })
    })

    ws.on(Sig.SessionKicked, (env) => {
      const reason = env.data?.reason || 'login_elsewhere'
      this.callbacks.onKicked(reason)
    })

    ws.on(Sig.SessionVoiceTaken, () => {
      // 语音被抢占：服务端已把本端摘出房间，本地退房但绝不发 channel.leave
      //（那条 leave 按 uid 处理会连带抢占端退房）。
      this.teardownRoomLocal()
      webEmit('voice:taken', null)
    })

    ws.on(Sig.PresenceUpdate, (env) => {
      const d = env.data
      if (!d?.user_id) return
      const payload: Record<string, unknown> = {
        user_id: String(d.user_id),
        online: !!d.online,
      }
      if (d.channel_id) payload.channel_id = String(d.channel_id)
      if (d.server_id) payload.server_id = String(d.server_id)
      webEmit('presence:update', payload)
    })

    ws.on(Sig.ConversationChanged, (env) => {
      const d = env.data
      if (!d?.conversation_id) return
      webEmit('conversation:changed', {
        conversation_id: String(d.conversation_id),
        action: d.action ?? '',
      })
    })

    ws.on(Sig.ChatMessage, (env) => {
      const d = env.data
      if (!d?.id) return
      const payload: Record<string, unknown> = {
        id: String(d.id),
        channel_id: String(d.channel_id ?? ''),
        user_id: String(d.user?.id ?? ''),
        nickname: d.user?.nickname ?? '',
        content: d.content ?? '',
        ts: String(d.ts ?? 0),
        mentions: Array.isArray(d.mentions) ? d.mentions.map(String) : [],
      }
      if (d.nonce) payload.nonce = String(d.nonce)
      if (d.reply_to) payload.reply_to = String(d.reply_to)
      if (d.reply_preview) {
        payload.reply_preview = {
          id: String(d.reply_preview.id ?? ''),
          user_id: String(d.reply_preview.user_id ?? ''),
          nickname: d.reply_preview.nickname ?? '',
          content: d.reply_preview.content ?? '',
          deleted: !!d.reply_preview.deleted,
        }
      }
      const atts = attachmentViews(d.attachments)
      if (atts) payload.attachments = atts
      webEmit('chat:message', payload)
    })

    ws.on(Sig.MessageEdited, (env) => {
      const d = env.data
      if (!d?.channel_id || !d?.message_id) return
      webEmit('chat:edited', {
        channel_id: String(d.channel_id),
        message_id: String(d.message_id),
        content: d.content ?? '',
        edited_at: String(d.edited_at ?? 0),
        mentions: Array.isArray(d.mentions) ? d.mentions.map(String) : [],
      })
    })

    ws.on(Sig.MessageDeleted, (env) => {
      const d = env.data
      if (!d?.channel_id || !d?.message_id) return
      webEmit('chat:deleted', {
        channel_id: String(d.channel_id),
        message_id: String(d.message_id),
      })
    })

    ws.on(Sig.ChannelUnread, (env) => {
      const d = env.data
      if (!d?.channel_id) return
      webEmit('chat:unread', {
        channel_id: String(d.channel_id),
        unread_count: Number(d.unread_count ?? 0),
        mention_count: Number(d.mention_count ?? 0),
      })
    })

    ws.on(Sig.ServerShutdown, (env) => {
      const reason = env.data?.reason || 'shutdown'
      webEmit('sys:shutdown', { reason })
      // 服务端优雅关闭：停自动重连（Go 侧 ws.Close 停 Run，同语义）。
      this.ws.stop()
      this.handleDisconnect()
    })

    ws.on(Sig.Error, (env) => this.handleErrorFrame(env))
  }

  // handleErrorFrame 服务端 error 帧归属（#91）：进房失败结算 pending join 并
  // 回滚房间态，其余转 chat:error。码表与 Go session.go 逐字对齐。
  private handleErrorFrame(env: Envelope): void {
    const code = String(env.data?.code ?? '')
    const message = String(env.data?.message ?? '')
    const joinOnly = JOIN_ONLY_CODES.has(code)
    const joinRelated = joinOnly || JOIN_SHARED_CODES.has(code)

    let channelID = ''
    let consumed = false
    let rolledBack = false
    if (joinRelated) {
      // already_in_channel 是「本连接已在该房」的确认：只结算、不回滚。
      const r = this.settlePendingJoinHead(code !== 'already_in_channel')
      channelID = r.channelID
      consumed = r.consumed
      rolledBack = r.rolledBack
    }
    if (!consumed && joinOnly) {
      // 进房专用码没有 pending 可归属：迟到/重复帧，静默丢弃。
      return
    }
    if (rolledBack) {
      webEmit('voice:join_failed', { channel_id: channelID, code, message })
      return
    }
    if (consumed) return // 已归属进房路径（如 already_in_channel），不重复提示
    webEmit('chat:error', { code, message })
  }
}
