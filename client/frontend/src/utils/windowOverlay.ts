// 游戏内悬浮层（#47，Beta，仅 Windows）的设置状态。
// 从 SettingsView 抽成可注入桥接的 composable，便于 vitest 覆盖：
// 平台不支持时开关必须显示为关闭（不把持久化值当成可用能力），
// 保存失败回滚勾选，避免 UI 与 Go 侧实际状态不一致。
//
// 启动失败回滚（#117）：ServiceStartup 里启用悬浮层失败时，Go 侧会把
// window_overlay.enabled 落盘改回 false 并推 sys:overlay 事件。本 composable
// 订阅该事件，把开关同步成关闭并给出错误文案——否则设置页会一直显示
// 「已开启」而实际没有窗口。
import { ref } from 'vue'
import type { WindowOverlaySettings } from '../api/types'
import { cleanErrorText } from './errorText'

// OverlayState 对应 Go 侧 App.OverlayStateEvent（sys:overlay 载荷）。
export interface OverlayState {
  enabled: boolean
  error?: string
}

// WindowOverlayBridge 只依赖设置页用到的绑定，便于测试注入。
export interface WindowOverlayBridge {
  WindowOverlaySupported(): Promise<boolean>
  GetWindowOverlaySettings(): Promise<WindowOverlaySettings>
  SetWindowOverlaySettings(s: WindowOverlaySettings): Promise<void>
  // Subscribe 可选：订阅 sys:overlay，返回取消函数。浏览器预览环境没有事件。
  Subscribe?(cb: (s: OverlayState) => void): () => void
}

export function useWindowOverlay(bridge: WindowOverlayBridge) {
  const enabled = ref(false)
  const supported = ref(false)
  // note 启动失败/保存失败的原因，空串 = 无提示。
  const note = ref('')

  // load 读取平台能力与持久化值：不支持时强制显示关闭。返回取消函数，
  // 调用方在组件卸载时执行（事件监听按监听器逐个注销）。
  async function load(): Promise<() => void> {
    supported.value = await bridge.WindowOverlaySupported().catch(() => false)
    const s = await bridge.GetWindowOverlaySettings().catch(() => null)
    enabled.value = supported.value && !!s?.enabled
    if (!bridge.Subscribe) return () => {}
    // Go 侧回滚后推的事件：即使首屏订阅晚于事件到达，读回的值也已是 false，
    // 两条路径最终一致。
    const cancel = bridge.Subscribe((st) => {
      enabled.value = supported.value && !!st?.enabled
      note.value = st?.error ?? ''
    })
    return typeof cancel === 'function' ? cancel : () => {}
  }

  // setEnabled 乐观更新后保存；失败回滚勾选。返回错误文案，成功返回 null。
  async function setEnabled(on: boolean): Promise<string | null> {
    if (on && !supported.value) return '仅 Windows 支持游戏内悬浮层'
    const prev = enabled.value
    enabled.value = on
    try {
      await bridge.SetWindowOverlaySettings({ enabled: on })
      note.value = ''
      return null
    } catch (e) {
      enabled.value = prev
      const msg = cleanErrorText(e, '未知错误')
      note.value = msg
      return msg
    }
  }

  return { enabled, supported, note, load, setEnabled }
}
