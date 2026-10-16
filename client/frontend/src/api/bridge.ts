// bridge 封装 Wails v3 Go 绑定（@wailsio/runtime 的 Call.ByName("main.App.*")）。
// 浏览器独立开发（vite dev 无 Go 后端）时走 mock 桩，行为可联调 UI。
// 原生环境判定：Go 在 webview 启动时注入 window._wails.flags（浏览器环境恒无）。
import { Call, Events } from '@wailsio/runtime'
import type { AudioSettings, AuthResult, CategoryDTO, ChannelDTO, ConnConfigView, ConnTestResult, FriendDTO, PeerView, ServerConfig, ServerDTO, UserProfile, VoiceStateView } from './types'

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
  SendEmailCode(email: string, purpose: string): Promise<void>
  Login(email: string, password: string): Promise<AuthResult>
  LoginAsGuest(): Promise<AuthResult>
  Register(nickname: string, email: string, password: string, code: string): Promise<AuthResult>
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
  JoinChannel(channelID: string): Promise<void>
  LeaveChannel(): Promise<void>
  SetMute(muted: boolean): Promise<void>
  SetDeafen(deafened: boolean): Promise<void>
  SetPeerVolume(userID: string, gain: number): Promise<void>
  // GetVoiceState UI 销毁重载后的语音态恢复（Go 会话未断，前端重建后回填）。
  GetVoiceState(): Promise<VoiceStateView>
  // RequestVoiceState 「请求静音」：请目标闭麦/静音（对方接受后生效，无强制）。
  RequestVoiceState(channelID: string, targetID: string, mute: boolean, deafen: boolean): Promise<void>
  SendChat(channelID: string, content: string): Promise<void>
  GetVoiceMembers(): Promise<PeerView[]>
  LogFromWeb(level: string, msg: string): Promise<void>
  // 服务器/频道/好友 REST CRUD（服务端已有端点；批次①接线）
  ListServers(): Promise<ServerDTO[]>
  CreateServer(name: string): Promise<ServerDTO>
  JoinServerByCode(code: string): Promise<ServerDTO>
  RenameServer(id: number, name: string): Promise<void>
  DeleteServer(id: number): Promise<void>
  CreateInviteCode(serverID: number): Promise<string>
  CreateChannel(serverID: number, name: string, typ: string): Promise<ChannelDTO>
  RenameChannel(id: number, name: string): Promise<void>
  DeleteChannel(id: number): Promise<void>
  CreateCategory(serverID: number, name: string): Promise<CategoryDTO>
  DeleteCategory(id: number, withChannels: boolean): Promise<void>
  SendFriendRequest(target: string): Promise<void>
  // 好友真实 REST（v0.1.2 批次③消费）
  ListFriends(): Promise<FriendDTO[]>
  AcceptFriend(id: number): Promise<void>
  RemoveFriend(id: number): Promise<void>
  UpdateFriend(id: number, remark: string, group: string): Promise<void>
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

declare global {
  interface Window {
    _wails?: { flags?: Record<string, unknown> }
  }
}

// mock：浏览器开发环境的桩（登录一律成功、设备返回虚拟项）。
const mockApp: GoApp = {
  async GetVersion() { return 'dev' },
  async GetAuthConfig() { return { auth_mode: 'full', mail_configured: false } },
  async GetHibernateMode() { return 'off' },
  async SetHibernateMode() {},
  async QuitApp() {},
  async TrayReady() { return true },
  async GetCloseToTray() { return true },
  async SetCloseToTray() {},
  async GetSavedCredentials() { return { email: '', password: '', saved: false } },
  async SetSavedCredentials() {},
  async ClearSavedCredentials() {},
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
      user: { id: 1, nickname: email.split('@')[0] || 'dev', tag: '0001', email },
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
      user: { id: 2, nickname: '游客-0000', tag: '0000', email: '', is_guest: true },
    }
  },
  async GetMe() {
    return { id: 1, nickname: 'dev', tag: '0001', email: 'dev@example.com' }
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
  async ListAudioDevices() {
    return {
      capture: [{ id: 'default', name: '默认麦克风', is_default: true }],
      playback: [{ id: 'default', name: '默认扬声器', is_default: true }],
    }
  },
  async JoinChannel() {},
  async LeaveChannel() {},
  async SetMute() {},
  async SetDeafen() {},
  async SetPeerVolume() {},
  async SendChat() {},
  async GetVoiceMembers() { return [] },
  async GetVoiceState() { return { channel_id: '', muted: false, deafened: false } },
  async RequestVoiceState() {},
  // 以下为批次①新增的服务器/频道/好友桩：返回可联调 UI 的假数据。
  async ListServers() { return [] },
  async CreateServer(name: string) {
    return { id: Date.now(), name, owner_id: 1, icon_name: 'messages-square', icon_color: '#4FA4F3' }
  },
  async JoinServerByCode() {
    return { id: Date.now(), name: '加入的服务器', owner_id: 2, icon_name: 'messages-square', icon_color: '#4FA4F3' }
  },
  async RenameServer() {},
  async DeleteServer() {},
  async CreateInviteCode() { return 'mock-invite-code' },
  async CreateChannel(serverID: number, name: string, typ: string) {
    return { id: Date.now(), server_id: serverID, name, type: typ as 'text' | 'voice' }
  },
  async RenameChannel() {},
  async DeleteChannel() {},
  async CreateCategory(serverID: number, name: string) {
    return { id: Date.now(), server_id: serverID, name, position: 0 }
  },
  async DeleteCategory() {},
  async SendFriendRequest() {},
  // 好友真实 REST 桩：空列表（浏览器开发环境无可联调数据）。
  async ListFriends() { return [] },
  async AcceptFriend() {},
  async RemoveFriend() {},
  async UpdateFriend() {},
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
  return isNative() ? nativeApp : mockApp
}

export function isNative(): boolean {
  return !!window._wails?.flags
}

// eventsOn Wails 事件订阅。v3 回调收 WailsEvent 对象，取 .data 对齐 v2 的直载荷语义。
// 浏览器环境静默（mock 不产事件）。
export function eventsOn(name: string, cb: (...args: unknown[]) => void) {
  if (!isNative()) return
  Events.On(name, (ev) => cb((ev as { data: unknown }).data))
}

// eventsOff 取消事件订阅（休眠态清理用；v3 按事件名清掉全部监听）。
export function eventsOff(name: string) {
  if (!isNative()) return
  Events.Off(name)
}
