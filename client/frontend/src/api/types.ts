// 类型定义（与 Go 侧 state 包对齐）。
export interface UserProfile {
  id: number
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

// AudioSettings 语音设置（与 Go 侧 config.AudioSettings 对齐；指针/零值字段未设置即默认）。
// v0.1.2 扩展：降噪强度/AGC/Opus 复杂度/麦克风/扬声器音量；旧 gain 保留兼容读取。
export interface AudioSettings {
  bitrate_kbps: number // 16/24/32/48/64
  vbr: boolean
  dtx: boolean
  fec: boolean
  packet_loss: number // 5–20
  denoise: boolean
  denoise_strength: number // 1 弱 / 2 中 / 3 强（rnnoise Aggressiveness 档位）
  agc: boolean // 自动增益（实验性，默认关）
  gain?: number // 旧输入增益 ×10（已由 mic_volume 取代，Go 侧兼容换算）
  mic_volume: number // 0–120（100 = 1.0 原声）
  playback_volume: number // 0–120（混音总线增益）
  vad_sens: number // 0–4 五档（旧 0/1/2 由 Go 侧加载时一次性迁移 0/2/4）
  opus_complexity: number // Opus 复杂度 0–10（默认 9；字段名与 Go json tag 对齐）
}

// VoiceStateView 语音状态恢复视图（GetVoiceState；UI 销毁重载后回填用）。
// 字段名与 Go 侧 session.VoiceState 的 json tag 对齐（deafened 非 deafen）。
export interface VoiceStateView {
  channel_id: string
  muted: boolean
  deafened: boolean
}

// FriendDTO 好友（服务端 friendView；批次③好友真实 REST 用）。
export interface FriendDTO {
  id: number
  status: string // pending / accepted / blocked
  incoming: boolean
  remark: string
  group: string
  user: { id: number; nickname: string; tag: string }
}

// CategoryDTO 频道分类（服务端实体）。
export interface CategoryDTO {
  id: number
  server_id: number
  name: string
  position: number
}

// ServerDTO 服务器（与 Go 侧 state.ServerDTO 对齐）。
export interface ServerDTO {
  id: number
  name: string
  owner_id: number
  icon_name: string
  icon_color: string
}

// ChannelDTO 频道（与 Go 侧 state.ChannelDTO 对齐）。
export interface ChannelDTO {
  id: number
  server_id: number
  name: string
  type: 'text' | 'voice'
}
