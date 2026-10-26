// 类型定义。两半：
// 1) REST 契约类型——来自 api/openapi.yaml 的生成物 types.gen.ts（禁止手改生成物，
//    改契约先改 openapi.yaml 再跑 go run -C server ./api/tools/openapigen）；
// 2) 纯前端/桥接类型——UI 视图、Wails 绑定载荷，与 REST wire 不同源。
import type { components } from './types.gen'

// ---- REST 契约类型（openapi.yaml → types.gen.ts）----
// Problem：RFC 9457 问题详情（#31）；ApiError 为旧名兼容别名。
export type Problem = components['schemas']['Problem']
export type ApiError = Problem
export type ServerConfig = components['schemas']['ServerConfig']
export type User = components['schemas']['User']
export type PublicUser = components['schemas']['PublicUser']
export type TokenPair = components['schemas']['TokenPair']
export type ServerInfo = components['schemas']['ServerInfo']
export type ChannelInfo = components['schemas']['ChannelInfo']
export type CategoryInfo = components['schemas']['CategoryInfo']
export type MemberInfo = components['schemas']['MemberInfo']
export type Message = components['schemas']['Message']
// #39：消息编辑/删除/回复/@提及的契约类型。
export type ReplyPreview = components['schemas']['ReplyPreview']
export type EditMessageRequest = components['schemas']['EditMessageRequest']
export type MentionCandidate = components['schemas']['MentionCandidate']
export type Invite = components['schemas']['Invite']
export type FriendEntry = components['schemas']['FriendEntry']
export type ServerChannels = components['schemas']['ServerChannels']
export type SyncServer = components['schemas']['SyncServer']
export type SyncSnapshot = components['schemas']['SyncSnapshot']
// #35 B4：角色 / 频道覆盖 / 审计（permissions 为十进制字符串）。
export type Role = components['schemas']['Role']
export type CreateRoleRequest = components['schemas']['CreateRoleRequest']
export type UpdateRoleRequest = components['schemas']['UpdateRoleRequest']
export type SetMemberRolesRequest = components['schemas']['SetMemberRolesRequest']
export type ChannelOverwrite = components['schemas']['ChannelOverwrite']
export type SetOverwriteRequest = components['schemas']['SetOverwriteRequest']
export type AuditLogEntry = components['schemas']['AuditLogEntry']
// #38 B2：私聊/群私聊会话（无服务器频道；消息仍走 channel_id）。
export type Conversation = components['schemas']['Conversation']
export type ConversationMember = components['schemas']['ConversationMember']
export type CreateConversationRequest = components['schemas']['CreateConversationRequest']
export type PatchConversationRequest = components['schemas']['PatchConversationRequest']
export type AddConversationMembersRequest = components['schemas']['AddConversationMembersRequest']
// #40：未读与提及计数（快照含 0 值项；channel.unread 事件为覆盖语义）。
export type Unreads = components['schemas']['Unreads']
export type UnreadChannel = components['schemas']['UnreadChannel']
export type UnreadServer = components['schemas']['UnreadServer']
// #42 B4：链接预览（服务端抓取并清洗；image_url 为绝对地址，客户端直接加载）。
export type LinkPreview = components['schemas']['LinkPreview']

// ---- 桥接层兼容名（组件/stores 既有命名）----
// Wails 绑定经 Go state 包中转：字段与契约同源，个别桥接补充字段见注释。
export type ServerDTO = ServerInfo
export type ChannelDTO = ChannelInfo
export type CategoryDTO = CategoryInfo
// 桥接层把 user 解成值类型，恒非空（REST 侧 accept/patch 极端情况可为 null）。
export type FriendDTO = Omit<FriendEntry, 'user'> & { user: UserProfile }
// UserProfile：契约 User + 桥接补充的在线态（好友列表 user.online 用）。
export type UserProfile = User & { online: boolean }

// AuthResult 登录/注册的桥接载荷（与 REST TokenPair 不同源：无 expires_in，
// 多 session_ok 报告信令是否已连上——Go 侧登录后连信令的首连结果）。
// session_ok 可选仅因浏览器 mock 桩不产该字段。
export interface AuthResult {
  access_token: string
  refresh_token: string
  user: UserProfile
  session_ok?: boolean
}

// ---- 纯前端/桥接类型（不进 REST 契约）----

export interface PeerView {
  user_id: string
  nickname: string
  tag: string
  speaking: boolean
  mute: boolean
  deafen: boolean
  // priority 优先发言（#45）：接收端 ducking 与成员徽标用。
  priority: boolean
}

// AttachmentView 消息附件（#41，与 Go 侧 ChatMessageView.Attachments 同口径）。
// url/thumbnail_url 已由 Go 桥拼成绝对地址，可直接用作 img src 或交给 OpenAttachment。
export interface AttachmentView {
  id: string
  filename: string
  size: number
  content_type: string
  width: number
  height: number
  url: string
  thumbnail_url?: string
}

// ChatMessage 语音房聊天（WS chat.message 桥接视图；与 REST Message 不同源，
// 三方漂移在 #31 统一）。
export interface ChatMessage {
  id: string
  channel_id: string
  user_id: string
  nickname: string
  content: string
  ts: string
  // nonce 发送方幂等键（#31，协议 v2；老服务端/REST 补拉不带）
  nonce?: string
  // #39：编辑/软删时间为毫秒字符串；reply_to 为回复目标消息 ID；
  // reply_preview 为跳转预览（目标已软删时 content 空、deleted=true）；
  // mentions 为被 @ 的用户 ID（升序去重）。
  edited_at?: string
  deleted_at?: string
  reply_to?: string
  reply_preview?: ReplyPreview
  mentions?: string[]
  // #41 附件（无附件或软删消息缺省）；纯附件消息 content 为空串。
  attachments?: AttachmentView[]
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
  // 关闭 WebRTC 回声消除（false/缺省 = 开启；omitempty 后 false 不出现）。
  disable_aec?: boolean
}

// HotkeySettings 全局热键与按键说话配置（与 Go 侧 hotkey.Settings / config 的
// HotkeySettings json tag 逐字对齐，字段名不得单方面改动）。
// mode：0 按住说话 / 1 语音激活 / 2 常开；绑定为规范化串（"ctrl+shift+m"、"mouse4"），
// 空串或缺失都表示未绑定；tail_ms 尾包延迟 0–1000，omitempty 后可能缺省。
export interface HotkeySettings {
  enabled: boolean
  mode: 0 | 1 | 2
  ptt?: string
  // whisper 按住悄悄话（#45）：默认不绑定。
  whisper?: string
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
