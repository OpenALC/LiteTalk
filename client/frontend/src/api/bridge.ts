// bridge 封装 Wails v3 Go 绑定（@wailsio/runtime 的 Call.ByName("main.App.*")）。
// 浏览器独立开发（vite dev 无 Go 后端）时走 mock 桩，行为可联调 UI。
// 原生环境判定：Go 在 webview 启动时注入 window._wails.flags（浏览器环境恒无）。
import { Call, Events } from '@wailsio/runtime'
import type { AttachmentView, AudioSettings, AuthResult, CategoryDTO, ChannelDTO, ChatMessage, ConnConfigView, ConnTestResult, Conversation, FriendDTO, HotkeySettings, MentionCandidate, PeerView, ServerChannels, ServerConfig, ServerDTO, UnreadChannel, Unreads, UserProfile, VoiceStateView } from './types'

type GoApp = {
  GetVersion(): Promise<string>
  GetAuthConfig(): Promise<ServerConfig>
  GetHibernateMode(): Promise<string> // off / hibernate / destroy
  SetHibernateMode(mode: string): Promise<void>
  GetServerConfig(): Promise<ConnConfigView>
  SetServerConfig(baseURL: string): Promise<void>
  TestServerConnection(baseURL: string): Promise<ConnTestResult>
  WindowMinimize(): Promise<void>
  WindowMaximize(): Promise<void>
  WindowClose(): Promise<void>
  QuitApp(): Promise<void>
  TrayReady(): Promise<boolean>
  GetCloseToTray(): Promise<boolean>
  SetCloseToTray(v: boolean): Promise<void>
  GetSavedCredentials(): Promise<SavedCredentials>
  SetSavedCredentials(email: string, password: string): Promise<void>
  ClearSavedCredentials(): Promise<void>
  // 机密存储模式：keyring / file / dpapi。Linux 回落 file 时 UI 需明示提示。
  SecretStorageMode(): Promise<string>
  SendEmailCode(email: string, purpose: string): Promise<void>
  Login(email: string, password: string): Promise<AuthResult>
  LoginAsGuest(): Promise<AuthResult>
  Register(nickname: string, email: string, password: string, code: string, bootstrap: string): Promise<AuthResult>
  ResetPassword(email: string, code: string, newPassword: string): Promise<void>
  Logout(): Promise<void>
  GetMe(): Promise<UserProfile>
  ListAudioDevices(): Promise<{ capture: AudioDeviceDTO[]; playback: AudioDeviceDTO[] }>
  GetAudioDeviceSelection(): Promise<{ capture_id: string; playback_id: string }>
  SetAudioDevices(captureID: string, playbackID: string): Promise<void>
  TestMicrophone(): Promise<{ rms: number; peak: number }>
  TestSpeaker(): Promise<void>
  GetAudioSettings(): Promise<AudioSettings>
  SetAudioSettings(s: AudioSettings): Promise<void>
  // 全局热键与按键说话（GetHotkeyAvailability 为 false 时平台不支持，UI 需明示降级）。
  GetHotkeySettings(): Promise<HotkeySettings>
  SetHotkeySettings(s: HotkeySettings): Promise<void>
  GetHotkeyAvailability(): Promise<boolean>
  JoinChannel(channelID: string): Promise<void>
  // JoinChannelWithPassword 带密码入房（#37）；密码由 Go 侧缓存供重连复用。
  JoinChannelWithPassword(channelID: string, password: string): Promise<void>
  LeaveChannel(): Promise<void>
  SetMute(muted: boolean): Promise<void>
  SetDeafen(deafened: boolean): Promise<void>
  SetPeerVolume(userID: string, gain: number): Promise<void>
  // GetVoiceState UI 销毁重载后的语音态恢复（Go 会话未断，前端重建后回填）。
  GetVoiceState(): Promise<VoiceStateView>
  // RequestVoiceState 「请求静音」：请目标闭麦/静音（对方接受后生效，无强制）。
  RequestVoiceState(channelID: string, targetID: string, mute: boolean, deafen: boolean): Promise<void>
  SendChat(channelID: string, content: string): Promise<void>
  // 消息编辑/删除/回复/@提及（#39）：ID 一律十进制字符串，Go 侧校验非法 ID。
  // SendChatWithMeta：replyTo 空串 = 普通消息；mentions 空数组 = 无提及。
  // EditMessage：mentions 整体替换（空数组清除全部提及），返回更新后的消息。
  // DeleteMessage 软删；GetMessage 用于回复跳转（软删消息仍返回）。
  SendChatWithMeta(channelID: string, content: string, replyTo: string, mentions: string[]): Promise<void>
  // 附件与图片（#41）：PickAndUploadAttachments 打开系统文件选择器并上传，
  // 返回绝对 url 的附件元数据（取消返回空数组）；SendChatWithAttachments 发送
  // 纯附件消息（content 可为空串）；OpenAttachment 用系统浏览器打开 http(s) 附件。
  PickAndUploadAttachments(): Promise<AttachmentView[]>
  SendChatWithAttachments(channelID: string, content: string, replyTo: string, mentions: string[], attachmentIDs: string[]): Promise<void>
  OpenAttachment(url: string): Promise<void>
  EditMessage(channelID: string, messageID: string, content: string, mentions: string[]): Promise<ChatMessage>
  DeleteMessage(channelID: string, messageID: string): Promise<void>
  GetMessage(channelID: string, messageID: string): Promise<ChatMessage>
  ListMentionCandidates(channelID: string): Promise<MentionCandidate[]>
  // ListMessages 频道最近消息（旧→新，limit 上限 50）。私聊/群聊的会话 ID
  // 即频道 ID；载荷与 chat:message 事件同口径，按 id 去重合并。
  ListMessages(channelID: string, limit: number): Promise<ChatMessage[]>
  // SearchMessages 全局消息搜索（#42）：跨服务器频道与会话，只返回可见频道；
  // 载荷与 ListMessages 同口径（含附件）；q 1–100、limit 1–50 默认 20。
  SearchMessages(query: string, limit: number): Promise<ChatMessage[]>
  GetVoiceMembers(): Promise<PeerView[]>
  LogFromWeb(level: string, msg: string): Promise<void>
  // 服务器/频道/好友 REST CRUD（服务端已有端点；批次①接线）。
  // ID 一律十进制字符串（#31：Snowflake 超出 JS Number 精度）。
  ListServers(): Promise<ServerDTO[]>
  ListChannels(serverID: string): Promise<ServerChannels>
  CreateServer(name: string): Promise<ServerDTO>
  JoinServerByCode(code: string): Promise<ServerDTO>
  RenameServer(id: string, name: string): Promise<void>
  DeleteServer(id: string): Promise<void>
  CreateInviteCode(serverID: string): Promise<string>
  // categoryID 空串/「0」= 不归类（#72 新建即归组，不用等重启对账）。
  CreateChannel(serverID: string, name: string, typ: string, categoryID: string): Promise<ChannelDTO>
  // 频道选项（#37 B2）：子频道（类型随父）与选项更新（parentID ""=不改/"0"=顶层；
  // userLimit <0 不改；password 空串不改，clearPassword 清除）。
  CreateSubchannel(serverID: string, name: string, typ: string, parentID: string): Promise<ChannelDTO>
  UpdateChannelOptions(channelID: string, parentID: string, userLimit: number, password: string, clearPassword: boolean): Promise<ChannelDTO>
  RenameChannel(id: string, name: string): Promise<void>
  DeleteChannel(id: string): Promise<void>
  CreateCategory(serverID: string, name: string): Promise<CategoryDTO>
  DeleteCategory(id: string, withChannels: boolean): Promise<void>
  // 分类改名/排序（PATCH /categories/{id}）：name 空串 = 不改名，position < 0 = 不改位置。
  RenameCategory(id: string, name: string): Promise<void>
  UpdateCategory(id: string, name: string, position: number): Promise<CategoryDTO>
  SendFriendRequest(target: string): Promise<void>
  // 好友真实 REST（v0.1.2 批次③消费）
  ListFriends(): Promise<FriendDTO[]>
  AcceptFriend(id: string): Promise<void>
  RemoveFriend(id: string): Promise<void>
  UpdateFriend(id: string, remark: string, group: string): Promise<void>
  // 私聊/群私聊（#38 B4）：Go 桥直接代理 REST，ID 一律十进制字符串。
  // 会话变更由 conversation:changed 事件通知，前端收到后重新拉取。
  ListConversations(): Promise<Conversation[]>
  CreateDirectConversation(userID: string): Promise<Conversation>
  CreateGroupConversation(name: string, userIDs: string[]): Promise<Conversation>
  GetConversation(id: string): Promise<Conversation>
  RenameConversation(id: string, name: string): Promise<Conversation>
  DeleteConversation(id: string): Promise<void>
  AddConversationMembers(id: string, userIDs: string[]): Promise<Conversation>
  // 最后一人退出时服务端删除会话（204），Go 侧返回 null。
  RemoveConversationMember(id: string, userID: string): Promise<Conversation | null>
  // 未读与提及计数（#40）：ListUnreads 返回含 0 值项的快照（空数组非 null）；
  // MarkChannelRead 上报已读水位并返回服务端重算后的计数。增量由 chat:unread
  // 事件推送（覆盖语义，不能累加）。
  ListUnreads(): Promise<Unreads>
  MarkChannelRead(channelID: string, lastMessageID: string): Promise<UnreadChannel>
  // 介绍页/首页（v0.1.2：客户端级偏好，存 config.json）
  GetHomeServer(): Promise<string>
  SetHomeServer(id: string): Promise<void>
  GetIntroHidden(): Promise<boolean>
  SetIntroHidden(v: boolean): Promise<void>
}

export interface AudioDeviceDTO {
  id: string
  name: string
  is_default: boolean
}

export interface SavedCredentials {
  email: string
  password: string
  saved: boolean
}

// mockConversation 浏览器开发用的会话桩：ID 为字符串，成员不含 email。
function mockConversation(id: string, kind: Conversation['kind'], name: string): Conversation {
  return {
    id,
    kind,
    name,
    members: [
      { user_id: '1', nickname: 'dev', tag: '0001', role: 'owner', joined_at: '2026-01-01T00:00:00Z' },
    ],
    created_at: '2026-01-01T00:00:00Z',
  }
}

declare global {
  interface Window {
    _wails?: { flags?: Record<string, unknown> }
  }
}

// mock：浏览器开发环境的桩（登录一律成功、设备返回虚拟项）。
const mockApp: GoApp = {
  async GetVersion() { return 'dev' },
  async GetAuthConfig() {
    // mock 打开自托管引导，便于浏览器开发时看到注册页邀请码输入。
    return { auth_mode: 'full' as const, mail_configured: false, self_host: true, bootstrap_required: true }
  },
  async GetHibernateMode() { return 'off' },
  async SetHibernateMode() {},
  async QuitApp() {},
  async TrayReady() { return true },
  async GetCloseToTray() { return true },
  async SetCloseToTray() {},
  async GetSavedCredentials() { return { email: '', password: '', saved: false } },
  async SetSavedCredentials() {},
  async ClearSavedCredentials() {},
  async SecretStorageMode() { return 'keyring' },
  async GetServerConfig() {
    return { base_url: 'http://127.0.0.1:8077', ws_url: 'ws://127.0.0.1:8077/ws', source: 'default' as const }
  },
  async SetServerConfig() {},
  async TestServerConnection() {
    return { ok: true, latency_ms: 12, version: 'dev', error: '' }
  },
  async WindowMinimize() {},
  async WindowMaximize() {},
  async WindowClose() {},
  async SendEmailCode() {},
  async Login(email: string) {
    return {
      access_token: 'dev-token', refresh_token: 'dev-refresh',
      user: {
        id: '1', nickname: email.split('@')[0] || 'dev', tag: '0001', email,
        email_verified: true, is_guest: false, status: 0, online: true,
        created_at: '2026-01-01T00:00:00Z', updated_at: '2026-01-01T00:00:00Z',
      },
    }
  },
  async Register(_n: string, email: string) {
    return mockApp.Login(email, "mock")
  },
  async ResetPassword() {},
  async Logout() {},
  async LoginAsGuest() {
    return {
      access_token: 'dev-token', refresh_token: 'dev-refresh',
      user: {
        id: '2', nickname: '游客-0000', tag: '0000', email: '',
        email_verified: false, is_guest: true, status: 0, online: true,
        created_at: '2026-01-01T00:00:00Z', updated_at: '2026-01-01T00:00:00Z',
      },
    }
  },
  async GetMe() {
    return {
      id: '1', nickname: 'dev', tag: '0001', email: 'dev@example.com',
      email_verified: true, is_guest: false, status: 0, online: true,
      created_at: '2026-01-01T00:00:00Z', updated_at: '2026-01-01T00:00:00Z',
    }
  },
  async LogFromWeb() {},
  async SetAudioDevices() {},
  async GetAudioDeviceSelection() { return { capture_id: '', playback_id: '' } },
  async TestMicrophone() { return { rms: 800, peak: 3000 } },
  async TestSpeaker() {},
  async GetAudioSettings() {
    return {
      bitrate_kbps: 32, vbr: true, dtx: true, fec: true, packet_loss: 10,
      denoise: true, denoise_strength: 2, agc: false, gain: 10,
      mic_volume: 100, playback_volume: 100, vad_sens: 2, opus_complexity: 9,
    }
  },
  async SetAudioSettings() {},
  // 全局热键桩：默认值走 Go 侧 hotkey.DefaultSettings 的 TS 等价（全关、绑定全空）。
  // 浏览器环境没有原生钩子，可用性恒 false——设置页据此展示降级说明。
  async GetHotkeySettings() {
    return { enabled: false, mode: 0, tail_ms: 250, cue_sound: false }
  },
  async SetHotkeySettings() {},
  async GetHotkeyAvailability() { return false },
  async ListAudioDevices() {
    return {
      capture: [{ id: 'default', name: '默认麦克风', is_default: true }],
      playback: [{ id: 'default', name: '默认扬声器', is_default: true }],
    }
  },
  async JoinChannel() {},
  async JoinChannelWithPassword() {},
  async LeaveChannel() {},
  async SetMute() {},
  async SetDeafen() {},
  async SetPeerVolume() {},
  async SendChat() {},
  // #39 消息编辑/回复/提及桩：回显传入内容，供浏览器开发联调 UI。
  async SendChatWithMeta() {},
  // #41 附件桩：浏览器开发没有原生文件选择器，返回空数组；发送/打开为空操作。
  async PickAndUploadAttachments() { return [] },
  async SendChatWithAttachments() {},
  async OpenAttachment() {},
  async EditMessage(channelID: string, messageID: string, content: string, mentions: string[]) {
    return {
      id: messageID, channel_id: channelID, user_id: '1', nickname: 'dev',
      content, ts: String(Date.now()), mentions,
    }
  },
  async DeleteMessage() {},
  async GetMessage(channelID: string, messageID: string) {
    return {
      id: messageID, channel_id: channelID, user_id: '1', nickname: 'dev',
      content: '', ts: String(Date.now()),
    }
  },
  async ListMentionCandidates() { return [] },
  // 消息历史桩：浏览器开发环境无可联调数据。
  async ListMessages() { return [] },
  // 搜索桩：浏览器开发环境无可联调数据。
  async SearchMessages() { return [] },
  async GetVoiceMembers() { return [] },
  async GetVoiceState() { return { channel_id: '', muted: false, deafened: false } },
  async RequestVoiceState() {},
  // 以下为批次①新增的服务器/频道/好友桩：返回可联调 UI 的假数据。
  // ID 均为字符串（#31 wire 口径）。
  async ListServers() { return [] },
  async ListChannels(serverID: string) {
    return {
      categories: [{ id: '1', server_id: serverID, name: '默认分类', position: 0, created_at: '2026-01-01T00:00:00Z' }],
      channels: [{ id: '11', server_id: serverID, name: '大厅', type: 'voice' as const, category_id: '1', position: 0, created_at: '2026-01-01T00:00:00Z' }],
    }
  },
  async CreateServer(name: string) {
    return { id: String(Date.now()), name, owner_id: '1', icon_name: 'messages-square', icon_color: '#4FA4F3', created_at: '2026-01-01T00:00:00Z' }
  },
  async JoinServerByCode() {
    return { id: String(Date.now()), name: '加入的服务器', owner_id: '2', icon_name: 'messages-square', icon_color: '#4FA4F3', created_at: '2026-01-01T00:00:00Z' }
  },
  async RenameServer() {},
  async DeleteServer() {},
  async CreateInviteCode() { return 'mock-invite-code' },
  async CreateChannel(serverID: string, name: string, typ: string, categoryID: string) {
    return {
      id: String(Date.now()), server_id: serverID, name, type: typ as 'text' | 'voice',
      category_id: categoryID || null, position: 0, created_at: '2026-01-01T00:00:00Z',
    }
  },
  async RenameChannel() {},
  async DeleteChannel() {},
  async CreateSubchannel(serverID: string, name: string, typ: string, parentID: string) {
    return {
      id: String(Date.now()), server_id: serverID, name, type: typ as 'text' | 'voice',
      category_id: null, position: 0, created_at: '2026-01-01T00:00:00Z',
      parent_id: parentID, has_password: false, user_limit: 0,
    }
  },
  async UpdateChannelOptions(channelID: string) {
    return {
      id: channelID, server_id: '0', name: '频道', type: 'voice' as const,
      category_id: null, position: 0, created_at: '2026-01-01T00:00:00Z',
      parent_id: null, has_password: false, user_limit: 0,
    }
  },
  async CreateCategory(serverID: string, name: string) {
    return { id: String(Date.now()), server_id: serverID, name, position: 0, created_at: '2026-01-01T00:00:00Z' }
  },
  async DeleteCategory() {},
  async RenameCategory() {},
  async UpdateCategory(id: string, name: string, position: number) {
    return { id, server_id: '0', name: name || `分类${id}`, position: position < 0 ? 0 : position, created_at: '2026-01-01T00:00:00Z' }
  },
  async SendFriendRequest() {},
  // 好友真实 REST 桩：空列表（浏览器开发环境无可联调数据）。
  async ListFriends() { return [] },
  async AcceptFriend() {},
  async RemoveFriend() {},
  async UpdateFriend() {},
  // 会话桩：列表为空，创建/读取返回占位会话（浏览器开发环境无可联调数据）。
  async ListConversations() { return [] },
  async CreateDirectConversation() { return mockConversation('1001', 'direct', '') },
  async CreateGroupConversation(name: string) { return mockConversation('1002', 'group', name) },
  async GetConversation(id: string) { return mockConversation(id, 'group', '群聊') },
  async RenameConversation(id: string, name: string) { return mockConversation(id, 'group', name) },
  async DeleteConversation() {},
  async AddConversationMembers(id: string) { return mockConversation(id, 'group', '群聊') },
  async RemoveConversationMember(id: string) { return mockConversation(id, 'group', '群聊') },
  // 未读计数桩：空快照/零值（浏览器开发环境无可联调数据）。
  async ListUnreads() { return { channels: [], servers: [] } },
  async MarkChannelRead(channelID: string) {
    return { channel_id: channelID, unread_count: 0, mention_count: 0 }
  },
  // 介绍页/首页桩：默认介绍页为首页、未隐藏。
  async GetHomeServer() { return '' },
  async SetHomeServer() {},
  async GetIntroHidden() { return false },
  async SetIntroHidden() {},
}

// nativeApp v3 原生实现：方法名直映射 main.App.<方法>（与 Go 侧 Service 绑定同名）。
// Proxy 只拦属性读取，调用语义与 v2 的 window.go.main.App.* 完全一致（JSON 序列化参数）。
const nativeApp = new Proxy({} as GoApp, {
  get(_target, method: string) {
    return (...args: unknown[]) => Call.ByName(`main.App.${method}`, ...args)
  },
})

export function goApp(): GoApp {
  if (isNative()) return nativeApp
  if (import.meta.env.DEV) return mockApp
  // 生产环境原生初始化失败：mock 会让「登录一律成功」，是安全事故——
  // 宁可调用原生绑定报错（响亮失败），也不静默走桩。
  return nativeApp
}

export function isNative(): boolean {
  return !!window._wails?.flags
}

// nativeInitFailed 生产环境等待原生标志超时仍未就绪时置位（模块级单例）。
// App.vue 据此渲染错误页；一旦置位绝不回落 mock。
let nativeInitFailed = false

export function isNativeInitFailed(): boolean {
  return nativeInitFailed
}

// __resetBridgeForTest 仅 vitest 用：同进程多用例共享模块级状态，需手动复位。
export function __resetBridgeForTest(): void {
  nativeInitFailed = false
}

// whenNativeReady 等待 Go 侧注入 window._wails.flags（v3 在 NavigationCompleted
// 时经 execJS 注入，晚于前端模块求值——路由守卫若抢先跑会把原生误判成 mock）。
// Go 注入完成会派发 wails:runtime-config-ready 事件；必须在 mount 前 await，
// 保证首个守卫的 isNative() 判定可靠。
//
// 超时语义分环境：浏览器开发（DEV）超时按 mock 处理（开发联调无 Go 后端）；
// 生产环境 WebView2 冷启动可能数秒，等待延长到 10s，超时判为原生初始化失败并
// 置 nativeInitFailed——生产静默落 mock 是安全事故，绝不发生。
export function whenNativeReady(timeoutMs?: number): Promise<void> {
  if (isNative() || typeof window === 'undefined') return Promise.resolve()
  const timeout = timeoutMs ?? (import.meta.env.DEV ? 800 : 10_000)
  return new Promise((resolve) => {
    const done = () => {
      window.removeEventListener('wails:runtime-config-ready', done)
      resolve()
    }
    window.addEventListener('wails:runtime-config-ready', done, { once: true })
    // 附加监听后复查：事件恰在监听前派发（flags 已就位）时立即返回
    if (isNative()) {
      done()
      return
    }
    setTimeout(() => {
      if (!isNative() && !import.meta.env.DEV) {
        nativeInitFailed = true
      }
      done()
    }, timeout)
  })
}

// eventsOn Wails 事件订阅。v3 回调收 WailsEvent 对象，取 .data 对齐 v2 的直载荷语义。
// 浏览器环境静默（mock 不产事件）。
export function eventsOn(name: string, cb: (...args: unknown[]) => void): () => void {
  if (!isNative()) return () => {}
  // v3 的 Events.On 返回按监听器的取消函数：卸载时逐个注销，
  // 不许用 eventsOff 按事件名清（会把别人注册的同名监听一并清掉）。
  return Events.On(name, (ev) => cb((ev as { data: unknown }).data))
}

// eventsOff 取消事件订阅（休眠态清理用；v3 按事件名清掉全部监听）。
export function eventsOff(name: string) {
  if (!isNative()) return
  Events.Off(name)
}
