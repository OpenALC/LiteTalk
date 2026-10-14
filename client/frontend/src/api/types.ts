// 类型定义（与 Go 侧 state 包对齐）。
export interface UserProfile {
  id: string
  nickname: string
  tag: string
  email: string
  is_guest?: boolean
}

export interface AuthResult {
  access_token: string
  refresh_token: string
  user: UserProfile
}

// ServerConfig 服务端能力开关（GET /api/v1/config）。
export interface ServerConfig {
  auth_mode: 'full' | 'simple'
  mail_configured: boolean
}

export interface PeerView {
  user_id: string
  nickname: string
  speaking: boolean
  mute: boolean
  deafen: boolean
}

export interface ChatMessage {
  id: string
  channel_id: string
  user_id: string
  nickname: string
  content: string
  ts: string
}

// ConnConfigView 当前生效的服务器地址与来源（设置页「连接」）。
export interface ConnConfigView {
  base_url: string
  ws_url: string
  source: 'env' | 'file' | 'default'
}

// ConnTestResult 连通性测试结果。
export interface ConnTestResult {
  ok: boolean
  latency_ms: number
  version: string
  error: string
}
