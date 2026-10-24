// 搜索面板的纯函数（#42 B2）：频道标签解析、正文摘要与查询词高亮切片、
// 结果时间格式化。全部只依赖入参，便于 vitest 直接覆盖。

// ChannelLabelSource 频道标签解析输入：会话侧用已解析的显示名（displayName），
// 服务器频道侧保留服务器名以便拼「服务器名 / #频道名」。
export interface ChannelLabelSource {
  conversations: Array<{ id: string; name: string }>
  servers: Array<{ name: string; channels: Array<{ id: string; name: string }> }>
}

// channelLabel 搜索结果行的频道标签：先按会话 ID 命中（私聊/群聊），
// 再在服务器频道里找；都找不到时给中性占位，不把裸 Snowflake ID 暴露给用户。
export function channelLabel(channelId: string, src: ChannelLabelSource): string {
  if (!channelId) return '#频道'
  const conv = src.conversations.find((c) => c.id === channelId)
  if (conv) return conv.name || '会话'
  for (const s of src.servers) {
    const ch = s.channels.find((c) => c.id === channelId)
    if (ch) return `${s.name} / #${ch.name}`
  }
  return '#频道'
}

// HighlightSegment 摘要切片：hit 为 true 的片段是命中查询词的部分。
export interface HighlightSegment {
  text: string
  hit: boolean
}

// SEARCH_EXCERPT_CHARS 摘要截断长度（约两行）。超出时追加省略号。
const SEARCH_EXCERPT_CHARS = 120

// excerptSegments 把正文折叠空白、截断后按查询词做大小写不敏感的切片高亮。
// 用文本切片渲染（禁止 v-html）；大小写折叠改变长度（罕见 Unicode）时放弃
// 高亮，保证切片下标不会错位。
export function excerptSegments(
  content: string,
  query: string,
  max = SEARCH_EXCERPT_CHARS,
): HighlightSegment[] {
  const flat = content.replace(/\s+/g, ' ').trim()
  if (!flat) return []
  const cut = flat.length > max ? flat.slice(0, max) + '…' : flat
  const needle = query.trim()
  if (!needle) return [{ text: cut, hit: false }]
  const hay = cut.toLowerCase()
  const lower = needle.toLowerCase()
  if (hay.length !== cut.length || lower.length !== needle.length) {
    return [{ text: cut, hit: false }]
  }
  const out: HighlightSegment[] = []
  let from = 0
  // 空 needle 会让 indexOf 原地返回 from 造成死循环；折叠后为空按无高亮处理。
  if (!lower.length) return [{ text: cut, hit: false }]
  for (;;) {
    const at = hay.indexOf(lower, from)
    if (at < 0) break
    if (at > from) out.push({ text: cut.slice(from, at), hit: false })
    out.push({ text: cut.slice(at, at + lower.length), hit: true })
    from = at + lower.length
  }
  if (from < cut.length) out.push({ text: cut.slice(from), hit: false })
  return out.length ? out : [{ text: cut, hit: false }]
}

function pad2(n: number): string {
  return String(n).padStart(2, '0')
}

// formatSearchTime 结果行时间：今天只显示 HH:MM，同年显示「M月D日 HH:MM」，
// 跨年显示「YYYY年M月D日」。now 可注入便于测试。
export function formatSearchTime(ts: string, now = Date.now()): string {
  const n = Number(ts)
  if (!Number.isFinite(n) || n <= 0) return ''
  const d = new Date(n)
  const ref = new Date(now)
  const hm = `${pad2(d.getHours())}:${pad2(d.getMinutes())}`
  const sameDay = d.getFullYear() === ref.getFullYear()
    && d.getMonth() === ref.getMonth()
    && d.getDate() === ref.getDate()
  if (sameDay) return hm
  const md = `${d.getMonth() + 1}月${d.getDate()}日`
  if (d.getFullYear() === ref.getFullYear()) return `${md} ${hm}`
  return `${d.getFullYear()}年${md}`
}
