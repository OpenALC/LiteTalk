// hotkeySpec 全局热键绑定语法。纯函数集中在本文件，与 Go 侧
// client/core/hotkey.ParseBinding 的语义逐条对齐（任务书 §0.2）：
//   · 修饰键 ctrl/alt/shift/win，顺序不敏感，重复即非法
//   · 主键：a-z、0-9、f1-f24、space/tab/esc/enter/up/down/left/right/home/end/
//     pgup/pgdn/ins/del，标点 ` - = [ ] \ ; ' , . /，以及 mouse1-mouse5
//   · 规范化输出：修饰键按 ctrl,alt,shift,win 固定序 + 主键，全小写，+ 连接
//   · 鼠标键可带修饰键（ctrl+alt+mouse5 合法）
//   · 拒绝：空串、未知 token、出现两个以上主键、修饰键重复、只有修饰键没有主键
// 录制态（SettingsView）只负责把 DOM 事件翻成 token 串，合法性判定全在本文件，
// 保证 UI 预览与 Go 侧落库结果一致。
import { HOTKEYS } from '../composables/useHotkeys'

// HOTKEY_ACTIONS 可绑定的五个动作（与 Go 侧 hotkey.Action 对齐；顺序即设置页
// 展示顺序）。whisper 为按住悄悄话（#45），默认不绑定。
export const HOTKEY_ACTIONS = [
  { id: 'ptt', label: '按住说话' },
  { id: 'whisper', label: '悄悄话（按住）' },
  { id: 'mute', label: '闭麦 / 取消闭麦' },
  { id: 'deafen', label: '静音 / 取消静音' },
  { id: 'dual', label: '全关 / 全开' },
] as const

export type HotkeyActionID = (typeof HOTKEY_ACTIONS)[number]['id']

// 修饰键固定序：既是输入允许集，也是规范化输出的排序依据。
const MODIFIERS = ['ctrl', 'alt', 'shift', 'win'] as const

// 命名主键（词表与 Go 侧一致）。
const NAMED_KEYS = new Set([
  'space', 'tab', 'esc', 'enter', 'up', 'down', 'left', 'right',
  'home', 'end', 'pgup', 'pgdn', 'ins', 'del',
])

// 标点主键：US 布局下 KeyboardEvent.key 的单字符。
const PUNCT_KEYS = new Set(['`', '-', '=', '[', ']', '\\', ';', "'", ',', '.', '/'])

// mouse1 左 / mouse2 右 / mouse3 中 / mouse4 XBUTTON1 / mouse5 XBUTTON2。
const MOUSE_RE = /^mouse[1-5]$/

// f1-f24：f0 与 f25 及以上均非法。
const FKEY_RE = /^f([1-9]|1[0-9]|2[0-4])$/

// KeyboardEvent.key 到主键 token 的映射（Go 侧 VK 表的反向）。
const KEY_ALIAS: Record<string, string> = {
  ' ': 'space',
  escape: 'esc',
  enter: 'enter',
  arrowup: 'up',
  arrowdown: 'down',
  arrowleft: 'left',
  arrowright: 'right',
  pageup: 'pgup',
  pagedown: 'pgdn',
  insert: 'ins',
  delete: 'del',
}

// 修饰键事件字段（只取判定所需，便于 node 环境单测）。
export interface ModifierLike {
  ctrlKey: boolean
  altKey: boolean
  shiftKey: boolean
  metaKey: boolean
}

export interface KeyLike extends ModifierLike {
  key: string
}

export interface MouseLike extends ModifierLike {
  button: number
}

// isMainKey token 是否为合法主键（不含修饰键）。
function isMainKey(t: string): boolean {
  if (NAMED_KEYS.has(t) || PUNCT_KEYS.has(t)) return true
  if (MOUSE_RE.test(t) || FKEY_RE.test(t)) return true
  return (t.length === 1 && t >= 'a' && t <= 'z') || (t.length === 1 && t >= '0' && t <= '9')
}

// keyToken KeyboardEvent.key → 主键 token（不可绑定返回空串）。
// 纯修饰键按下会自然落空（'control'/'meta' 不在词表），录制态继续等待。
function keyToken(key: string): string {
  const k = key.toLowerCase()
  const alias = KEY_ALIAS[k]
  if (alias) return alias
  return isMainKey(k) ? k : ''
}

// modifierPrefix 按固定序拼出修饰键段（无修饰键返回空串）。
export function modifierPrefix(e: ModifierLike): string {
  const m: string[] = []
  if (e.ctrlKey) m.push('ctrl')
  if (e.altKey) m.push('alt')
  if (e.shiftKey) m.push('shift')
  if (e.metaKey) m.push('win')
  return m.join('+')
}

// normalizeBinding 解析并规范化绑定串；非法返回 null（对齐 ParseBinding 的错误路径）。
// 空白容忍：整串与各 token 的两侧空白都去掉——规范化产物恒为规范形态，
// 落库前不会把带空格的串传给 Go 侧。
export function normalizeBinding(spec: string): string | null {
  const parts = spec
    .trim()
    .toLowerCase()
    .split('+')
    .map((p) => p.trim())
  if (parts.some((p) => p === '')) return null // 空串入参，或 "ctrl++m" 这类空 token
  const mods: string[] = []
  let main = ''
  for (const p of parts) {
    if ((MODIFIERS as readonly string[]).includes(p)) {
      if (mods.includes(p)) return null // 修饰键重复
      mods.push(p)
      continue
    }
    if (!isMainKey(p)) return null // 未知 token
    if (main) return null // 出现两个以上主键
    main = p
  }
  if (!main) return null // 只有修饰键没有主键
  const ordered = MODIFIERS.filter((m) => mods.includes(m))
  return [...ordered, main].join('+')
}

// bindingFromKeyEvent 键盘事件 → 规范化绑定串（不可绑定返回空串）。
export function bindingFromKeyEvent(e: KeyLike): string {
  const main = keyToken(e.key)
  if (!main) return ''
  return normalizeBinding([modifierPrefix(e), main].filter(Boolean).join('+')) ?? ''
}

// bindingFromMouseEvent 鼠标事件 → 规范化绑定串（0-4 之外的按键不可绑定）。
export function bindingFromMouseEvent(e: MouseLike): string {
  const main = `mouse${e.button + 1}`
  if (!MOUSE_RE.test(main)) return ''
  return normalizeBinding([modifierPrefix(e), main].filter(Boolean).join('+')) ?? ''
}

// bindingLabel 绑定串的展示文本（"ctrl+shift+m" → "Ctrl + Shift + M"；未绑定返回空串）。
export function bindingLabel(spec?: string): string {
  const b = normalizeBinding(spec ?? '')
  if (!b) return ''
  return b
    .split('+')
    .map((t) => t.charAt(0).toUpperCase() + t.slice(1))
    .join(' + ')
}

// findHotkeyConflicts 返回被多个动作同时占用的规范化组合（去重，保持首次出现序）。
// 冲突时 UI 必须提示，且不得静默覆盖其中一个绑定。
export function findHotkeyConflicts(s: Partial<Record<HotkeyActionID, string | undefined>>): string[] {
  const count = new Map<string, number>()
  for (const a of HOTKEY_ACTIONS) {
    const b = normalizeBinding(s[a.id] ?? '')
    if (!b) continue
    count.set(b, (count.get(b) ?? 0) + 1)
  }
  return [...count.entries()].filter(([, n]) => n > 1).map(([b]) => b)
}

// IN_APP_COMBOS 应用内快捷键（useHotkeys 的 HOTKEYS）的规范化组合。
// 全局热键与它们撞车时，窗口聚焦下两条路径都会触发同一个动作：OS 钩子先到、
// webview keydown 后到，两者各自读一份状态再取反，净效果取决于到达顺序，
// 可能一次翻转也可能互相抵消。只能提示用户改绑，无法在运行时可靠去重。
export const IN_APP_COMBOS: readonly string[] = HOTKEYS.map((h) => normalizeBinding(h.combo)).filter(
  (b): b is string => b !== null,
)

// findInAppClashes 返回与应用内快捷键撞车的全局绑定（规范化串，保持动作序）。
export function findInAppClashes(s: Partial<Record<HotkeyActionID, string | undefined>>): string[] {
  const out: string[] = []
  for (const a of HOTKEY_ACTIONS) {
    const b = normalizeBinding(s[a.id] ?? '')
    if (b && IN_APP_COMBOS.includes(b)) out.push(b)
  }
  return out
}
