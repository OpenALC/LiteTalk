// Web 版 GoApp 实现：按 bridge.ts 的 GoApp 接口逐条落地（REST + WS + WebRTC）。
// 优先序与降级口径（批次任务书 §2 步骤 3）：
//   - 登录/注册/游客 → /sync → 频道与消息 → 私聊/群聊 → 未读 → 语音 → 设置；
//   - 附件浏览器侧只读展示（<img>/下载链接），上传留 P2（PickAndUploadAttachments 报错）;
//   - 桌面专有设置（休眠/托盘/热键/悬浮层/音频设备）只读展示，写入提示「桌面端可用」；
//   - 语音走浏览器原生栈（web/rtc.ts），不搬 DRED/APM。
// REST 形状与 api/openapi.yaml 对齐；本文件内的 wire 接口是手写最小集
// （生成物 types.gen.ts 的严格类型在桥接视图换算中粒度不齐，手写集在此注明）。
import type {
  AttachmentView,
  AuthResult,
  AudioSettings,
  CategoryDTO,
  ChannelDTO,
  ChatMessage,
  ConnConfigView,
  ConnTestResult,
  Conversation,
  FriendDTO,
  HotkeySettings,
  LinkPreview,
  MentionCandidate,
  NetworkDiagResult,
  PeerView,
  ServerChannels,
  ServerConfig,
  ServerDTO,
  UnreadChannel,
  Unreads,
  UserProfile,
  VoiceStateView,
} from '../types'
import type { GoApp } from '../bridge'
import { webEmit } from './events'
import { SignalSession } from './signal'
import { WEB_CLIENT_VERSION } from './version'
import {
  APIError,
  apiDel,
  apiGet,
  apiPatch,
  apiPost,
  apiPut,
  clearTokens,
  getAccessToken,
  getRefreshToken,
  hasSession,
  setTokens,
} from './http'

// ---- 手写 wire 最小集（与 api/signal、openapi.yaml 对齐；字段有出入以契约为准）----

interface RestUser {
  id: string
  nickname: string
  tag: string
  email?: string
  email_verified?: boolean
  is_guest?: boolean
  status?: number
  online?: boolean
  created_at?: string
  updated_at?: string
}

interface RestAttachment {
  id: string
  filename: string
  size: number
  content_type: string
  width?: number
  height?: number
  url: string
  thumbnail_url?: string
}

// RestMessage GET /channels/{id}/messages 等端点的消息形状（契约 Message）。
interface RestMessage {
  id: string
  channel_id: string
  user_id: string
  content: string
  created_at: string
  sender_nickname?: string
  sender_tag?: string
  edited_at?: string
  deleted_at?: string
  reply_to?: string
  reply_preview?: { id: string; user_id: string; nickname: string; content: string; deleted: boolean }
  mentions?: string[]
  attachments?: RestAttachment[]
}

interface TokenPairResponse {
  access_token: string
  refresh_token: string
  expires_in: number
  user: RestUser
}

interface RestConversation {
  id: string
  kind: 'direct' | 'group'
  name?: string
  members: { user_id: string; nickname: string; tag: string; role?: string; joined_at?: string }[]
  created_at?: string
}

interface RestFriend {
  id: string
  status: 'pending' | 'accepted' | 'blocked'
  incoming: boolean
  remark?: string
  group?: string
  user: RestUser
  channel_id?: string
  server_id?: string
}

// ---- 常量与小工具 ----

// priorityPermBit 频道覆盖里的 PrioritySpeaker 位（Go app.go priorityPermBit）。
const PRIORITY_PERM_BIT = 1n << 4n

// DESKTOP_ONLY 桌面专有设置的统一报错文案（设置页 toast 展示）。
const DESKTOP_ONLY = '该设置桌面端可用'

function toProfile(u: RestUser): UserProfile {
  return {
    id: String(u.id ?? ''),
    nickname: u.nickname ?? '',
    tag: u.tag ?? '',
    email: u.email ?? '',
    email_verified: !!u.email_verified,
    is_guest: !!u.is_guest,
    status: u.status ?? 0,
    // online 桥接补充字段：/me 等自响应恒 false；好友列表由条目自带值覆盖。
    online: !!u.online,
    created_at: u.created_at ?? '',
    updated_at: u.updated_at ?? '',
  }
}

function toFriend(f: RestFriend): FriendDTO {
  return {
    id: String(f.id ?? ''),
    status: f.status,
    incoming: !!f.incoming,
    remark: f.remark ?? '',
    group: f.group ?? '',
    user: toProfile(f.user),
    channel_id: f.channel_id ?? '',
    server_id: f.server_id ?? '',
  }
}

function toConversation(c: RestConversation): Conversation {
  return {
    id: String(c.id ?? ''),
    kind: c.kind,
    name: c.name ?? '',
    members: (c.members ?? []).map((m) => ({
      user_id: String(m.user_id ?? ''),
      nickname: m.nickname ?? '',
      tag: m.tag ?? '',
      role: m.role === 'owner' ? ('owner' as const) : ('member' as const),
      joined_at: m.joined_at ?? '',
    })),
    created_at: c.created_at ?? '',
  }
}

// toChatMessage REST 消息 → 桥接视图（Go app.go chatMessageView 的换算口径）：
// ts/edited_at/deleted_at 都是毫秒字符串，ID 十进制字符串，附件 url 绝对化。
function toChatMessage(m: RestMessage): ChatMessage {
  const v: ChatMessage = {
    id: String(m.id ?? ''),
    channel_id: String(m.channel_id ?? ''),
    user_id: String(m.user_id ?? ''),
    nickname: m.sender_nickname ?? '',
    content: m.content ?? '',
    ts: String(Date.parse(m.created_at ?? '') || 0),
    mentions: m.mentions ?? [],
  }
  if (m.edited_at) v.edited_at = String(Date.parse(m.edited_at) || 0)
  if (m.deleted_at) v.deleted_at = String(Date.parse(m.deleted_at) || 0)
  if (m.reply_to) v.reply_to = String(m.reply_to)
  if (m.reply_preview) {
    v.reply_preview = {
      id: String(m.reply_preview.id ?? ''),
      user_id: String(m.reply_preview.user_id ?? ''),
      nickname: m.reply_preview.nickname ?? '',
      content: m.reply_preview.content ?? '',
      deleted: !!m.reply_preview.deleted,
    }
  }
  // 软删消息不下发附件（服务端已省略，兜一层防止墓碑后仍渲染图片）。
  if (!m.deleted_at && m.attachments?.length) {
    v.attachments = m.attachments.map(absoluteAttachment)
  }
  return v
}

function absoluteAttachment(at: RestAttachment): AttachmentView {
  return {
    id: String(at.id ?? ''),
    filename: at.filename ?? '',
    size: Number(at.size ?? 0),
    content_type: at.content_type ?? '',
    width: Number(at.width ?? 0),
    height: Number(at.height ?? 0),
    url: new URL(at.url ?? '', location.origin).toString(),
    ...(at.thumbnail_url ? { thumbnail_url: new URL(at.thumbnail_url, location.origin).toString() } : {}),
  }
}

// parseID 校验十进制 ID 串（Go 侧桥接同款防御：非法 ID 拒发）。
function requireID(id: string, what: string): string {
  if (!/^\d+$/.test(id)) throw new Error(`非法的${what}`)
  return id
}

// localStorage 键（客户端偏好；与登录态键 litetalk.web.* 同域不同前缀）。
const PREF_HOME_SERVER = 'litetalk.web.home_server'
const PREF_INTRO_HIDDEN = 'litetalk.web.intro_hidden'

function prefGet(key: string): string {
  try {
    return localStorage.getItem(key) ?? ''
  } catch {
    return ''
  }
}

function prefSet(key: string, value: string): void {
  try {
    if (value) localStorage.setItem(key, value)
    else localStorage.removeItem(key)
  } catch {
    // 写失败按未设置处理
  }
}

// webApp 单例（SignalSession 一次登录会话一个实例；登出 stop 后复用同一对象重连）。
let session: SignalSession | null = null

function getSession(): SignalSession {
  if (!session) {
    session = new SignalSession({
      onKicked: (reason) => {
        // 被踢链路：auth:kicked 由 auth store 消费（清本地会话回登录页）。
        webEmit('auth:kicked', { reason })
      },
      onConnectionStateChange: (connected) => {
        webEmit('auth:session', { ok: connected })
      },
    })
  }
  return session
}

// startSessionWithTokens 登录三入口共用：装令牌 + 起信令（恢复已持久化的会话由
// ensureRestoredSession 在启动期调用）。
function startSessionWithTokens(access: string, refresh: string, user: RestUser): AuthResult {
  setTokens(access, refresh)
  const s = getSession()
  s.start()
  return {
    access_token: access,
    refresh_token: refresh,
    user: toProfile(user),
    session_ok: s.connected,
  }
}

// kickOut 本地清会话（登出/被踢的收尾）。
function kickOut(): void {
  session?.stop()
  clearTokens()
}

// ---- GoApp 实现 ----

// webApp 实现 GoApp 全量接口；类型标注让缺方法/错签名在 type-check 阶段暴露。
export const webApp: GoApp = {
  async GetVersion(): Promise<string> {
    return WEB_CLIENT_VERSION
  },

  async GetAuthConfig(): Promise<ServerConfig> {
    // 免鉴权端点；拉取失败由调用方回落默认值（auth store catch）。
    return apiGet<ServerConfig>('/config')
  },

  async GetHibernateMode(): Promise<string> {
    return 'off' // 浏览器无休眠概念，设置页只读展示
  },

  async SetHibernateMode(_mode: string): Promise<void> {
    throw new Error(DESKTOP_ONLY)
  },

  async GetServerConfig(): Promise<ConnConfigView> {
    // web 客户端与服务端同源部署：地址固定不可改。
    const proto = location.protocol === 'https:' ? 'wss:' : 'ws:'
    return { base_url: location.origin, ws_url: `${proto}//${location.host}/ws`, source: 'default' }
  },

  async SetServerConfig(_baseURL: string): Promise<void> {
    throw new Error('Web 客户端固定连接当前站点，服务器地址桌面端可用')
  },

  async TestServerConnection(_baseURL: string): Promise<ConnTestResult> {
    const t0 = performance.now()
    try {
      const h = await apiGet<{ status: string; version: string }>('/healthz')
      return { ok: h.status === 'ok', latency_ms: Math.round(performance.now() - t0), version: h.version, error: '' }
    } catch (e) {
      return { ok: false, latency_ms: 0, version: '', error: e instanceof Error ? e.message : String(e) }
    }
  },

  async NetworkDiag(): Promise<NetworkDiagResult> {
    // 四项独立探测（#49 C 口径）：REST/WS/STUN/TURN，单项失败不互相牵连。
    const rest: NetworkDiagResult['rest'] = { ok: false, latency_ms: 0 }
    const t0 = performance.now()
    try {
      const h = await apiGet<{ status: string; version: string }>('/healthz')
      rest.ok = h.status === 'ok'
      rest.version = h.version
    } catch (e) {
      rest.error = e instanceof Error ? e.message : String(e)
    }
    rest.latency_ms = Math.round(performance.now() - t0)

    // WS 探测拨号会顶掉活跃连接（同 sid 半开替换，见 ws.ts 注释）：已连接时跳过。
    const s = getSession()
    const ws: NetworkDiagResult['ws'] = s.connected
      ? { ok: true, latency_ms: 0, skipped: true, note: '信令已连接（未重复拨号）' }
      : await probeWS()
    const [stun, turn] = await Promise.all([probeICE('stun'), probeICE('turn')])
    return {
      ok: rest.ok && (ws.ok || !!ws.skipped),
      rest,
      ws,
      stun,
      turn,
    }
  },

  async WindowMinimize(): Promise<void> {}, // 浏览器窗口归浏览器管理
  async WindowMaximize(): Promise<void> {},
  async WindowClose(): Promise<void> {},
  async QuitApp(): Promise<void> {},
  async TrayReady(): Promise<boolean> {
    return false // web 无托盘
  },
  async GetCloseToTray(): Promise<boolean> {
    return false
  },
  async SetCloseToTray(_v: boolean): Promise<void> {
    throw new Error(DESKTOP_ONLY)
  },

  async GetSavedCredentials(): Promise<{ email: string; password: string; saved: boolean }> {
    // 密码记忆是桌面 keyring 能力；web 登录态走 token 持久化，不存密码。
    return { email: '', password: '', saved: false }
  },
  async SetSavedCredentials(_email: string, _password: string): Promise<void> {
    throw new Error(DESKTOP_ONLY)
  },
  async ClearSavedCredentials(): Promise<void> {},
  async SecretStorageMode(): Promise<string> {
    return 'web' // 展示口径：web 构建登录态存 localStorage（设置页显示原始串）
  },

  async SendEmailCode(email: string, purpose: string): Promise<void> {
    await apiPost('/auth/email-code', { email, purpose }, { auth: false })
  },

  async Login(email: string, password: string): Promise<AuthResult> {
    const r = await apiPost<TokenPairResponse>('/auth/login', { email, password }, { auth: false })
    return startSessionWithTokens(r.access_token, r.refresh_token, r.user)
  },

  async LoginAsGuest(): Promise<AuthResult> {
    const r = await apiPost<TokenPairResponse>('/auth/guest', {}, { auth: false })
    return startSessionWithTokens(r.access_token, r.refresh_token, r.user)
  },

  async Register(nickname: string, email: string, password: string, code: string, bootstrap: string): Promise<AuthResult> {
    const r = await apiPost<TokenPairResponse>(
      '/auth/register',
      { nickname, email, password, code, bootstrap },
      { auth: false },
    )
    return startSessionWithTokens(r.access_token, r.refresh_token, r.user)
  },

  async ResetPassword(email: string, code: string, newPassword: string): Promise<void> {
    await apiPost('/auth/password/reset', { email, code, password: newPassword }, { auth: false })
  },

  async Logout(): Promise<void> {
    // 离线登出语义（#14）：吊销请求失败不阻断本地清理。
    try {
      await apiPost('/auth/logout', { refresh_token: getRefreshToken() }, { auth: true })
    } catch {
      // 吊销失败仍清本地：下一次登录会拿到新令牌对。
    }
    kickOut()
  },

  async GetMe(): Promise<UserProfile> {
    const u = await apiGet<RestUser>('/users/me')
    // GetMe 是刷新后的会话恢复路径（路由守卫用它免重登）：令牌有效即起信令，
    // 与桌面端「App 启动自动重连」对齐。已连接时幂等。
    if (hasSession()) getSession().start()
    return toProfile(u)
  },

  async ListAudioDevices(): Promise<{ capture: { id: string; name: string; is_default: boolean }[]; playback: { id: string; name: string; is_default: boolean }[] }> {
    // 浏览器只暴露采集设备；播放设备由系统统一管理。
    const capture = await enumerateCaptureDevices()
    return {
      capture: capture.length ? capture : [{ id: 'default', name: '默认麦克风', is_default: true }],
      playback: [{ id: 'default', name: '系统默认', is_default: true }],
    }
  },
  async GetAudioDeviceSelection(): Promise<{ capture_id: string; playback_id: string }> {
    return { capture_id: '', playback_id: '' }
  },
  async SetAudioDevices(_captureID: string, _playbackID: string): Promise<void> {
    throw new Error(DESKTOP_ONLY)
  },

  async TestMicrophone(): Promise<{ rms: number; peak: number }> {
    // 采集 600ms 测电平：与桌面口径一致（设置页只消费两个数值）。
    const stream = await navigator.mediaDevices.getUserMedia({ audio: true })
    try {
      const ctx = new AudioContext()
      const an = ctx.createAnalyser()
      an.fftSize = 1024
      ctx.createMediaStreamSource(stream).connect(an)
      const buf = new Float32Array(an.fftSize)
      let peak = 0
      let sum = 0
      let n = 0
      const t0 = Date.now()
      while (Date.now() - t0 < 600) {
        an.getFloatTimeDomainData(buf)
        for (const v of buf) {
          const a = Math.abs(v)
          if (a > peak) peak = a
          sum += v * v
          n++
        }
        await new Promise((r) => setTimeout(r, 60))
      }
      void ctx.close()
      // rms 缩放到与桌面 mic_level 同量纲（0–~10000）。
      return { rms: Math.round(Math.sqrt(sum / Math.max(1, n)) * 10000), peak: Math.round(peak * 10000) }
    } finally {
      stream.getTracks().forEach((t) => t.stop())
    }
  },

  async TestSpeaker(): Promise<void> {
    // 播放 440Hz 0.4s 短音供用户确认出声（桌面播放测试音的浏览器等价物）。
    const ctx = new AudioContext()
    const osc = ctx.createOscillator()
    const gain = ctx.createGain()
    gain.gain.value = 0.15
    osc.frequency.value = 440
    osc.connect(gain)
    gain.connect(ctx.destination)
    osc.start()
    await new Promise((r) => setTimeout(r, 400))
    osc.stop()
    await ctx.close()
  },

  async GetAudioSettings(): Promise<AudioSettings> {
    // 浏览器栈的等价默认值（采集处理由 getUserMedia 约束完成）。
    return {
      bitrate_kbps: 32,
      vbr: true,
      dtx: true,
      fec: true,
      dred: false,
      packet_loss: 10,
      denoise: true,
      denoise_strength: 2,
      agc: true,
      gain: 10,
      mic_volume: 100,
      playback_volume: 100,
      vad_sens: 2,
      opus_complexity: 9,
      enable_apm: false,
    }
  },
  async SetAudioSettings(_s: AudioSettings): Promise<void> {
    throw new Error(DESKTOP_ONLY)
  },

  // APM（WebRTC APM Beta）与 DRED 是自研音频链路能力：浏览器用原生处理，不支持。
  async APMSupported(): Promise<boolean> {
    return false
  },
  async DREDSupported(): Promise<boolean> {
    return false
  },

  async WindowOverlaySupported(): Promise<boolean> {
    return false // 游戏内悬浮层仅 Windows 桌面
  },
  async GetWindowOverlaySettings(): Promise<{ enabled: boolean }> {
    return { enabled: false }
  },
  async SetWindowOverlaySettings(_s: { enabled: boolean }): Promise<void> {
    throw new Error(DESKTOP_ONLY)
  },

  async GetHotkeySettings(): Promise<HotkeySettings> {
    // web 无全局热键：默认值（全关）+ 可用性 false，设置页展示降级说明。
    return { enabled: false, mode: 0, tail_ms: 250, cue_sound: false }
  },
  async SetHotkeySettings(_s: HotkeySettings): Promise<void> {
    throw new Error(DESKTOP_ONLY)
  },
  async GetHotkeyAvailability(): Promise<boolean> {
    return false
  },

  // ---- 语音 ----

  async JoinChannel(channelID: string): Promise<void> {
    await getSession().joinChannel(requireID(channelID, '频道 ID'), '')
  },
  async JoinChannelWithPassword(channelID: string, password: string): Promise<void> {
    await getSession().joinChannel(requireID(channelID, '频道 ID'), password)
  },
  async LeaveChannel(): Promise<void> {
    await getSession().leaveChannel()
  },
  async SetMute(muted: boolean): Promise<void> {
    getSession().setMute(muted)
  },
  async SetDeafen(deafened: boolean): Promise<void> {
    getSession().setDeafen(deafened)
  },
  async SetPeerVolume(userID: string, gain: number): Promise<void> {
    getSession().setPeerVolume(userID, gain)
  },
  async SetWhisperTargets(targets: string[]): Promise<void> {
    getSession().setWhisperTargets(targets)
  },
  async WhisperSupported(): Promise<boolean> {
    return getSession().protocolVersion >= 3
  },
  // SetPrioritySpeaker 优先发言授予/撤销（#45）：频道覆盖位运算，逻辑照搬 Go
  // app.go 的 member 版本（allow 只动 PrioritySpeaker 位、同层 deny 清位）。
  async SetPrioritySpeaker(channelID: string, userID: string, enabled: boolean): Promise<void> {
    const cid = requireID(channelID, '频道 ID')
    const uid = requireID(userID, '用户 ID')
    const ows = await apiGet<{ target_type: string; target_id: string; allow?: string; deny?: string }[]>(
      `/channels/${cid}/overwrites`,
    )
    const cur = (ows ?? []).find((o) => o.target_type === 'member' && o.target_id === uid)
    let allow = BigInt(cur?.allow ?? '0')
    let deny = BigInt(cur?.deny ?? '0')
    if (enabled) {
      allow |= PRIORITY_PERM_BIT
      deny &= ~PRIORITY_PERM_BIT // 残留 deny 会让授予静默失效（同层 deny 胜于 allow）
    } else {
      const hadPersonal = !!cur && (allow & PRIORITY_PERM_BIT) !== 0n
      if (!hadPersonal && memberPriority(userID)) {
        throw new Error('该权限由角色授予，无法在此撤销')
      }
      if (!cur) return // 无个人覆盖可撤销：角色授予保持原样
      allow &= ~PRIORITY_PERM_BIT
    }
    if (cur && allow === 0n && deny === 0n) {
      await apiDel(`/channels/${cid}/overwrites/member/${uid}`)
      return
    }
    await apiPut(`/channels/${cid}/overwrites/member/${uid}`, {
      allow: allow.toString(),
      deny: deny.toString(),
    })
  },
  async GetVoiceState(): Promise<VoiceStateView> {
    return getSession().getVoiceState()
  },
  async RequestVoiceState(channelID: string, targetID: string, mute: boolean, deafen: boolean): Promise<void> {
    getSession().requestVoiceState(requireID(channelID, '频道 ID'), requireID(targetID, '用户 ID'), mute, deafen)
  },
  async GetVoiceMembers(): Promise<PeerView[]> {
    return getSession().getVoiceMembers()
  },

  // ---- 消息 ----

  async SendChat(channelID: string, content: string): Promise<void> {
    getSession().sendChat(requireID(channelID, '频道 ID'), content, '', [], [])
  },
  async SendChatWithMeta(channelID: string, content: string, replyTo: string, mentions: string[]): Promise<void> {
    getSession().sendChat(requireID(channelID, '频道 ID'), content, replyTo, mentions, [])
  },
  async SendChatWithAttachments(
    channelID: string,
    content: string,
    replyTo: string,
    mentions: string[],
    attachmentIDs: string[],
  ): Promise<void> {
    getSession().sendChat(requireID(channelID, '频道 ID'), content, replyTo, mentions, attachmentIDs)
  },
  async PickAndUploadAttachments(): Promise<AttachmentView[]> {
    // 上传留 P2（批次任务书）：浏览器侧附件只读展示。
    throw new Error('附件上传桌面端可用')
  },
  async OpenAttachment(url: string): Promise<void> {
    // 只放行 http(s)：附件 url 来自服务端契约，仍按入口白名单防御。
    if (!/^https?:/i.test(url)) throw new Error('不支持的附件地址')
    window.open(url, '_blank', 'noopener')
  },
  async EditMessage(channelID: string, messageID: string, content: string, mentions: string[]): Promise<ChatMessage> {
    const cid = requireID(channelID, '频道 ID')
    const mid = requireID(messageID, '消息 ID')
    // mentions 整体替换语义：null 会被服务端当「缺省=保留」，必须总是上送数组。
    const m = await apiPatch<RestMessage>(`/channels/${cid}/messages/${mid}`, {
      content,
      mentions: mentions ?? [],
    })
    return toChatMessage(m)
  },
  async DeleteMessage(channelID: string, messageID: string): Promise<void> {
    const cid = requireID(channelID, '频道 ID')
    const mid = requireID(messageID, '消息 ID')
    await apiDel(`/channels/${cid}/messages/${mid}`) // 204
  },
  async GetMessage(channelID: string, messageID: string): Promise<ChatMessage> {
    const cid = requireID(channelID, '频道 ID')
    const mid = requireID(messageID, '消息 ID')
    const m = await apiGet<RestMessage>(`/channels/${cid}/messages/${mid}`)
    return toChatMessage(m)
  },
  async ListMentionCandidates(channelID: string): Promise<MentionCandidate[]> {
    const cid = requireID(channelID, '频道 ID')
    const list = await apiGet<{ user_id: string; nickname: string; tag: string }[]>(
      `/channels/${cid}/mention-candidates`,
    )
    return (list ?? []).map((m) => ({ user_id: String(m.user_id), nickname: m.nickname, tag: m.tag }))
  },
  async ListMessages(channelID: string, limit: number): Promise<ChatMessage[]> {
    const cid = requireID(channelID, '频道 ID')
    // limit 夹到 [1,50]：服务端对显式越界值 fail-closed 400。
    const n = limit <= 0 || limit > 50 ? 50 : limit
    const list = await apiGet<RestMessage[]>(`/channels/${cid}/messages?limit=${n}`)
    return (list ?? []).map(toChatMessage)
  },
  async SearchMessages(query: string, limit: number): Promise<ChatMessage[]> {
    const q = query.trim()
    if (!q) return []
    const n = limit <= 0 ? 20 : limit > 50 ? 50 : limit
    const list = await apiGet<RestMessage[]>(`/search?q=${encodeURIComponent(q)}&limit=${n}`)
    return (list ?? []).map(toChatMessage)
  },
  async GetLinkPreview(url: string): Promise<LinkPreview | null> {
    try {
      return await apiGet<LinkPreview>(`/link-preview?url=${encodeURIComponent(url)}`)
    } catch (e) {
      // 404 no_preview（功能关闭/无预览）按 null 处理，前端静默隐藏卡片。
      if (e instanceof APIError && e.status === 404) return null
      throw e
    }
  },

  // ---- 服务器 / 频道 / 好友 ----

  async ListServers(): Promise<ServerDTO[]> {
    return (await apiGet<ServerDTO[]>('/servers')) ?? []
  },
  async ListChannels(serverID: string): Promise<ServerChannels> {
    const sid = requireID(serverID, '服务器 ID')
    return apiGet<ServerChannels>(`/servers/${sid}/channels`)
  },
  async CreateServer(name: string): Promise<ServerDTO> {
    return apiPost<ServerDTO>('/servers', { name })
  },
  async JoinServerByCode(code: string): Promise<ServerDTO> {
    return apiPost<ServerDTO>('/servers/join', { code })
  },
  async RenameServer(id: string, name: string): Promise<void> {
    await apiPatch(`/servers/${requireID(id, '服务器 ID')}`, { name })
  },
  async DeleteServer(id: string): Promise<void> {
    await apiDel(`/servers/${requireID(id, '服务器 ID')}`)
  },
  async CreateInviteCode(serverID: string): Promise<string> {
    const sid = requireID(serverID, '服务器 ID')
    const inv = await apiPost<{ code: string }>(`/servers/${sid}/invites`, {})
    return inv.code
  },
  async CreateChannel(serverID: string, name: string, typ: string, categoryID: string): Promise<ChannelDTO> {
    const sid = requireID(serverID, '服务器 ID')
    const body: Record<string, unknown> = { name, type: typ }
    if (categoryID && categoryID !== '0') body.category_id = categoryID
    return apiPost<ChannelDTO>(`/servers/${sid}/channels`, body)
  },
  async CreateSubchannel(serverID: string, name: string, typ: string, parentID: string): Promise<ChannelDTO> {
    const sid = requireID(serverID, '服务器 ID')
    return apiPost<ChannelDTO>(`/servers/${sid}/channels`, { name, type: typ, parent_id: parentID })
  },
  async UpdateChannelOptions(
    channelID: string,
    parentID: string,
    userLimit: number,
    audioBitrateKbps: number,
    audioFrameMs: number,
    password: string,
    clearPassword: boolean,
  ): Promise<ChannelDTO> {
    const cid = requireID(channelID, '频道 ID')
    // 只发送要改的字段（Go UpdateChannelOptions 同款语义：<0 不改；0 = 跟随默认）。
    const body: Record<string, unknown> = {}
    if (parentID) body.parent_id = parentID
    if (userLimit >= 0) body.user_limit = userLimit
    if (audioBitrateKbps >= 0) body.audio_bitrate_kbps = audioBitrateKbps
    if (audioFrameMs >= 0) body.audio_frame_ms = audioFrameMs
    if (clearPassword) body.password = null
    else if (password) body.password = password
    return apiPatch<ChannelDTO>(`/channels/${cid}`, body)
  },
  async RenameChannel(id: string, name: string): Promise<void> {
    await apiPatch(`/channels/${requireID(id, '频道 ID')}`, { name })
  },
  async DeleteChannel(id: string): Promise<void> {
    await apiDel(`/channels/${requireID(id, '频道 ID')}`)
  },
  async CreateCategory(serverID: string, name: string): Promise<CategoryDTO> {
    const sid = requireID(serverID, '服务器 ID')
    return apiPost<CategoryDTO>(`/servers/${sid}/categories`, { name })
  },
  async DeleteCategory(id: string, withChannels: boolean): Promise<void> {
    const cid = requireID(id, '分类 ID')
    await apiDel(`/categories/${cid}${withChannels ? '?with_channels=1' : ''}`)
  },
  async RenameCategory(id: string, name: string): Promise<void> {
    await apiPatch(`/categories/${requireID(id, '分类 ID')}`, { name })
  },
  async UpdateCategory(id: string, name: string, position: number): Promise<CategoryDTO> {
    const cid = requireID(id, '分类 ID')
    const body: Record<string, unknown> = {}
    if (name) body.name = name
    if (position >= 0) body.position = position
    return apiPatch<CategoryDTO>(`/categories/${cid}`, body)
  },

  async SendFriendRequest(target: string): Promise<void> {
    await apiPost('/friends/requests', { nickname: target })
  },
  async ListFriends(): Promise<FriendDTO[]> {
    const list = await apiGet<RestFriend[]>('/friends')
    return (list ?? []).map(toFriend)
  },
  async AcceptFriend(id: string): Promise<void> {
    await apiPost(`/friends/requests/${requireID(id, '好友请求 ID')}/accept`)
  },
  async RemoveFriend(id: string): Promise<void> {
    await apiDel(`/friends/${requireID(id, '好友 ID')}`)
  },
  async UpdateFriend(id: string, remark: string, group: string): Promise<void> {
    await apiPatch(`/friends/${requireID(id, '好友 ID')}`, { remark, group })
  },

  // ---- 私聊 / 群聊（#38）----

  async ListConversations(): Promise<Conversation[]> {
    const list = await apiGet<RestConversation[]>('/conversations')
    return (list ?? []).map(toConversation)
  },
  async CreateDirectConversation(userID: string): Promise<Conversation> {
    const r = await apiPost<RestConversation>('/conversations', { user_ids: [requireID(userID, '用户 ID')] })
    return toConversation(r)
  },
  async CreateGroupConversation(name: string, userIDs: string[]): Promise<Conversation> {
    const r = await apiPost<RestConversation>('/conversations', {
      kind: 'group',
      name,
      user_ids: userIDs.map((u) => requireID(u, '用户 ID')),
    })
    return toConversation(r)
  },
  async GetConversation(id: string): Promise<Conversation> {
    const r = await apiGet<RestConversation>(`/conversations/${requireID(id, '会话 ID')}`)
    return toConversation(r)
  },
  async RenameConversation(id: string, name: string): Promise<Conversation> {
    const r = await apiPatch<RestConversation>(`/conversations/${requireID(id, '会话 ID')}`, { name })
    return toConversation(r)
  },
  async DeleteConversation(id: string): Promise<void> {
    await apiDel(`/conversations/${requireID(id, '会话 ID')}`)
  },
  async AddConversationMembers(id: string, userIDs: string[]): Promise<Conversation> {
    const cid = requireID(id, '会话 ID')
    const r = await apiPost<RestConversation>(`/conversations/${cid}/members`, {
      user_ids: userIDs.map((u) => requireID(u, '用户 ID')),
    })
    return toConversation(r)
  },
  // RemoveConversationMember 最后一人退出时服务端删会话（204 → undefined → null）。
  async RemoveConversationMember(id: string, userID: string): Promise<Conversation | null> {
    const cid = requireID(id, '会话 ID')
    const uid = requireID(userID, '用户 ID')
    const r = await apiDel<RestConversation>(`/conversations/${cid}/members/${uid}`)
    return r ? toConversation(r) : null
  },

  // ---- 未读（#40）----

  async ListUnreads(): Promise<Unreads> {
    // 快照含 0 值项；null 归一为空结构（Go ListUnreads 同款）。
    const out = await apiGet<Unreads | null>('/unreads')
    return out ?? { channels: [], servers: [] }
  },
  async MarkChannelRead(channelID: string, lastMessageID: string): Promise<UnreadChannel> {
    const cid = requireID(channelID, '频道 ID')
    return apiPut<UnreadChannel>(`/channels/${cid}/read`, { last_message_id: lastMessageID })
  },

  // ---- 介绍页 / 首页（客户端级偏好，web 存 localStorage）----

  async GetHomeServer(): Promise<string> {
    return prefGet(PREF_HOME_SERVER)
  },
  async SetHomeServer(id: string): Promise<void> {
    prefSet(PREF_HOME_SERVER, id)
  },
  async GetIntroHidden(): Promise<boolean> {
    return prefGet(PREF_INTRO_HIDDEN) === '1'
  },
  async SetIntroHidden(v: boolean): Promise<void> {
    prefSet(PREF_INTRO_HIDDEN, v ? '1' : '')
  },

  async LogFromWeb(level: string, msg: string): Promise<void> {
    const line = `[web:${level}] ${msg}`
    if (level === 'error') console.error(line)
    else if (level === 'warn') console.warn(line)
    else console.log(line)
  },
}

// memberPriority 成员当前生效的优先发言标记（会话成员表维持，#117 撤销路径用）。
function memberPriority(userID: string): boolean {
  return session?.getVoiceMembers().find((m) => m.user_id === userID)?.priority ?? false
}

// enumerateCaptureDevices 枚举采集设备。未授权时 label 为空，回退单个默认项
//（设置页只用于展示与选择，真实设备在授权后可见）。
async function enumerateCaptureDevices(): Promise<{ id: string; name: string; is_default: boolean }[]> {
  try {
    const devs = await navigator.mediaDevices.enumerateDevices()
    const mics = devs.filter((d) => d.kind === 'audioinput')
    return mics.map((d, i) => ({
      id: d.deviceId || 'default',
      name: d.label || (i === 0 ? '默认麦克风' : `麦克风 ${i + 1}`),
      is_default: i === 0,
    }))
  } catch {
    return []
  }
}

// probeWS 一次信令握手探测（NetworkDiag 用）。错误不含 URL（query 带 token）。
async function probeWS(): Promise<NetworkDiagResult['ws']> {
  const token = getAccessToken()
  const proto = location.protocol === 'https:' ? 'wss:' : 'ws:'
  const url = `${proto}//${location.host}/ws${token ? `?token=${encodeURIComponent(token)}` : ''}`
  return new Promise((resolve) => {
    const t0 = performance.now()
    let settled = false
    const done = (r: NetworkDiagResult['ws']) => {
      if (!settled) {
        settled = true
        try {
          ws?.close()
        } catch {
          // 已关闭
        }
        resolve(r)
      }
    }
    let ws: WebSocket | null
    try {
      ws = new WebSocket(url)
    } catch {
      resolve({ ok: false, latency_ms: 0, error: '信令地址无效' })
      return
    }
    const timer = setTimeout(() => done({ ok: false, latency_ms: 0, error: '信令连接超时' }), 5000)
    ws.onopen = () => {
      clearTimeout(timer)
      done({ ok: true, latency_ms: Math.round(performance.now() - t0) })
    }
    ws.onerror = () => {
      clearTimeout(timer)
      done({ ok: false, latency_ms: 0, error: '信令握手失败' })
    }
  })
}

// probeICE STUN/TURN 可达性探测（NetworkDiag 用）：收集候选即视为可达，
// TURN 有 relay 候选才算通过（STUN 只要 srflx）。
async function probeICE(kind: 'stun' | 'turn'): Promise<NetworkDiagResult['stun']> {
  const out: NetworkDiagResult['stun'] = { ok: false, latency_ms: 0 }
  try {
    const cred = await apiPost<{ iceServers?: { urls: string[]; username?: string; credential?: string }[] }>(
      '/rtc/credentials',
      {},
      { auth: true },
    )
    const servers = (cred.iceServers ?? []).filter((s) =>
      kind === 'stun' ? s.urls.some((u) => u.startsWith('stun')) : s.urls.some((u) => u.startsWith('turn')),
    )
    if (!servers.length) {
      out.skipped = true
      out.note = '未配置'
      out.ok = true
      return out
    }
    const pc = new RTCPeerConnection({ iceServers: servers as RTCIceServer[] })
    pc.createDataChannel('probe')
    const t0 = performance.now()
    const candidate = await new Promise<string>((resolve) => {
      const timer = setTimeout(() => resolve(''), 6000)
      pc.onicecandidate = (e) => {
        if (!e.candidate) {
          clearTimeout(timer)
          resolve('')
          return
        }
        const c = e.candidate
        if (kind === 'stun' && c.type === 'srflx') {
          clearTimeout(timer)
          resolve(`${c.address ?? ''}:${c.port}`)
        }
        if (kind === 'turn' && c.type === 'relay') {
          clearTimeout(timer)
          resolve(`${c.address ?? ''}:${c.port}`)
        }
      }
      void pc.createOffer().then((o) => pc.setLocalDescription(o))
    })
    pc.close()
    out.latency_ms = Math.round(performance.now() - t0)
    if (candidate) {
      out.ok = true
      out.candidate = candidate
    } else {
      out.error = kind === 'stun' ? '未收集到 srflx 候选' : '未收集到 relay 候选'
    }
  } catch (e) {
    out.error = e instanceof Error ? e.message : String(e)
  }
  return out
}
