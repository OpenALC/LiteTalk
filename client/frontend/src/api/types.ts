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
  // 信令是否已连上（Go 侧登录后连接信令的首连结果）；false 时后台带退避重试，
  // UI 显示提示条。可选仅因浏览器 mock 桩不产该字段。
  session_ok?: boolean
}

// ServerConfig 服务端能力开关（GET /api/v1/config）。
export interface ServerConfig {
  auth_mode: 'full' | 'simple'
  mail_configured: boolean
}

export interface PeerView {
  user_id: string
  nickname: string
  tag: string
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
  agc: boolean // 自动增益（默认开）
  gain?: number // 旧输入增益 ×10（已由 mic_volume 取代，Go 侧兼容换算）
  mic_volume: number // 0–120（100 = 1.0 原声）
  playback_volume: number // 0–120（混音总线增益）
  vad_sens: number // 0–4 五档（旧 0/1/2 由 Go 侧加载时一次性迁移 0/2/4）
  opus_complexity: number // Opus 复杂度 0–10（默认 9；字段名与 Go json tag 对齐）
  // 输入声道下混（Go 侧总会返回；可选仅为兼容浏览器 mock 桩）。
  input_mode?: 'auto' | 'left' | 'right' | 'mix'
  // 前置增益 dB（-12~+24，RNNoise 之前；0 = 不变，omitempty 后可能缺省）。
  pre_gain_db?: number
}

// HotkeySettings 全局热键与按键说话配置（与 Go 侧 hotkey.Settings / config 的
// HotkeySettings json tag 逐字对齐，字段名不得单方面改动）。
// mode：0 按住说话 / 1 语音激活 / 2 常开；绑定为规范化串（"ctrl+shift+m"、"mouse4"），
// 空串或缺失都表示未绑定；tail_ms 尾包延迟 0–1000，omitempty 后可能缺省。
export interface HotkeySettings {
  enabled: boolean
  mode: 0 | 1 | 2
  ptt?: string
  mute?: string
  deafen?: string
  dual?: string
  tail_ms?: number
  cue_sound?: boolean
}

// AudioLevel audio:level 事件载荷（设置页麦克风测试期间 100ms 推送一次）。
// peak/rms/threshold 均为 dBFS 负值；threshold 为当前说话进入门限（电平条标线）。
export interface AudioLevel {
  peak: number
  rms: number
  threshold: number
}

// VoiceStateView 语音状态恢复视图（GetVoiceState；UI 销毁重载后回填用）。
// 字段名与 Go 侧 session.VoiceState 的 json tag 对齐（deafened 非 deafen）。
export interface VoiceStateView {
  channel_id: string
  muted: boolean
  deafened: boolean
}

// FriendDTO 好友（服务端 friendView；批次③好友真实 REST 用）。
// user.online：真实在线态（#11，服务端 presence:{uid}）；user.status 为账号状态
// 枚举（好友归组用，与 Go UserProfile.Status 对齐），在线点一律以 online 为准。
export interface FriendDTO {
  id: number
  status: string // pending / accepted / blocked
  incoming: boolean
  remark: string
  group: string
  user: { id: number; nickname: string; tag: string; status: number; online: boolean }
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
  // 分类归属（0 = 未分类，映射自服务端 NULL）与同服务器序；旧桩可能缺省。
  category_id?: number | null
  position?: number
}

// ServerChannels GET /servers/{id}/channels 响应（分类 + 频道两数组）。
export interface ServerChannels {
  categories: CategoryDTO[]
  channels: ChannelDTO[]
}
