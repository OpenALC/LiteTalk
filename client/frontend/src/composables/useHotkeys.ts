// useHotkeys 应用内快捷键（webview keydown）。
// 纯函数 matchesHotkey / isTypingTarget 可单测；组合式负责注册/注销监听。
import { onMounted, onUnmounted, type Ref } from 'vue'

// HotkeyDef 快捷键定义（供设置页展示与匹配复用）。
export interface HotkeyDef {
  id: string
  label: string
  combo: string // 展示文本，如 "Ctrl + M"
  ctrl?: boolean
  shift?: boolean
  alt?: boolean
  key: string // KeyboardEvent.key，小写比较
}

// 文案约定（实测走查 16）：麦克风=闭麦，扬声器=静音。
export const HOTKEYS: HotkeyDef[] = [
  { id: 'mute', label: '闭麦 / 取消闭麦', combo: 'Ctrl + M', ctrl: true, key: 'm' },
  { id: 'deafen', label: '静音 / 取消静音', combo: 'Ctrl + D', ctrl: true, key: 'd' },
  { id: 'dual', label: '全关 / 全开', combo: 'Ctrl + Shift + M', ctrl: true, shift: true, key: 'm' },
  { id: 'search', label: '聚焦搜索', combo: 'Ctrl + K', ctrl: true, key: 'k' },
  { id: 'settings', label: '打开设置', combo: 'Ctrl + ,', ctrl: true, key: ',' },
]

// isTypingTarget 判断事件目标是否为输入类元素（输入框内不触发快捷键）。
// 鸭子类型判定：跨 realm（iframe/WebView）下 instanceof 不可靠。
export function isTypingTarget(target: unknown): boolean {
  if (!target || typeof target !== 'object') return false
  const el = target as { tagName?: unknown; isContentEditable?: unknown }
  if (typeof el.tagName !== 'string') return false
  return (
    el.tagName === 'INPUT' ||
    el.tagName === 'TEXTAREA' ||
    el.tagName === 'SELECT' ||
    el.isContentEditable === true
  )
}

// matchesHotkey 判断键盘事件是否命中快捷键定义（纯函数，便于单测）。
export function matchesHotkey(
  e: { ctrlKey: boolean; shiftKey: boolean; altKey: boolean; key: string },
  def: HotkeyDef,
): boolean {
  if (e.ctrlKey !== !!def.ctrl) return false
  if (e.shiftKey !== !!def.shift) return false
  if (e.altKey !== !!def.alt) return false
  return e.key.toLowerCase() === def.key
}

// resolveHotkey 返回事件命中的快捷键 id（未命中返回空串）。
export function resolveHotkey(
  e: { ctrlKey: boolean; shiftKey: boolean; altKey: boolean; key: string },
): string {
  for (const def of HOTKEYS) {
    if (matchesHotkey(e, def)) return def.id
  }
  return ''
}

// useHotkeys 注册全局快捷键。active 为 false 时全部不触发（休眠态注销语义）。
// handlers 按 HOTKEYS 的 id 索引；返回注销函数（onUnmounted 自动调用）。
export function useHotkeys(
  handlers: Record<string, () => void>,
  active: Ref<boolean>,
): () => void {
  function onKeyDown(e: KeyboardEvent) {
    if (!active.value) return
    if (isTypingTarget(e.target)) return
    const id = resolveHotkey(e)
    if (!id) return
    const fn = handlers[id]
    if (!fn) return
    e.preventDefault()
    fn()
  }

  onMounted(() => window.addEventListener('keydown', onKeyDown))
  onUnmounted(() => window.removeEventListener('keydown', onKeyDown))
  return () => window.removeEventListener('keydown', onKeyDown)
}
