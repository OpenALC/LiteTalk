// bridge 封装 Wails Go 绑定（window.go.main.App.*）。
// 浏览器独立开发（vite dev 无 Go 后端）时走 mock 桩，行为可联调 UI。
import type { AudioSettings, AuthResult, ChannelDTO, ConnConfigView, ConnTestResult, PeerView, ServerConfig, ServerDTO, UserProfile } from './types'

type GoApp = {
  GetVersion(): Promise<string>
  GetAuthConfig(): Promise<ServerConfig>
  GetHibernateOnMinimize(): Promise<boolean>
  SetHibernateOnMinimize(enabled: boolean): Promise<void>
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
  SetAudioDevices(captureID: string, playbackID: string): Promise<void>
  GetAudioSettings(): Promise<AudioSettings>
  SetAudioSettings(s: AudioSettings): Promise<void>
  JoinChannel(channelID: string): Promise<void>
  LeaveChannel(): Promise<void>
  SetMute(muted: boolean): Promise<void>
  SetDeafen(deafened: boolean): Promise<void>
  SetPeerVolume(userID: string, gain: number): Promise<void>
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
  SendFriendRequest(target: string): Promise<void>
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
    go?: { main?: { App?: GoApp } }
    runtime?: {
      EventsOn(name: string, cb: (...args: unknown[]) => void): void
      EventsOff(name: string): void
    }
  }
}

// mock：浏览器开发环境的桩（登录一律成功、设备返回虚拟项）。
const mockApp: GoApp = {
  async GetVersion() { return 'dev' },
  async GetAuthConfig() { return { auth_mode: 'full', mail_configured: false } },
  async GetHibernateOnMinimize() { return false },
  async SetHibernateOnMinimize() {},
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
  async GetAudioSettings() {
    return { bitrate_kbps: 32, vbr: true, dtx: true, fec: true, packet_loss: 10, denoise: true, gain: 10, vad_sens: 1 }
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
  async SendFriendRequest() {},
}

export function goApp(): GoApp {
  return window.go?.main?.App ?? mockApp
}

export function isNative(): boolean {
  return !!window.go?.main?.App
}

// eventsOn Wails 事件订阅（浏览器环境静默）。
export function eventsOn(name: string, cb: (...args: unknown[]) => void) {
  window.runtime?.EventsOn(name, cb)
}

// eventsOff 取消事件订阅（休眠态清理用）。
export function eventsOff(name: string) {
  window.runtime?.EventsOff(name)
}
