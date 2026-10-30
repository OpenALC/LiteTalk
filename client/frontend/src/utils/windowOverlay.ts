// 游戏内悬浮层（#47，Beta，仅 Windows）的设置状态。
// 从 SettingsView 抽成可注入桥接的 composable，便于 vitest 覆盖：
// 平台不支持时开关必须显示为关闭（不把持久化值当成可用能力），
// 保存失败回滚勾选，避免 UI 与 Go 侧实际状态不一致。
import { ref } from 'vue'
import type { WindowOverlaySettings } from '../api/types'
import { cleanErrorText } from './errorText'

// WindowOverlayBridge 只依赖设置页用到的三个绑定，便于测试注入。
export interface WindowOverlayBridge {
  WindowOverlaySupported(): Promise<boolean>
  GetWindowOverlaySettings(): Promise<WindowOverlaySettings>
  SetWindowOverlaySettings(s: WindowOverlaySettings): Promise<void>
}

export function useWindowOverlay(bridge: WindowOverlayBridge) {
  const enabled = ref(false)
  const supported = ref(false)

  // load 读取平台能力与持久化值：不支持时强制显示关闭。
  async function load() {
    supported.value = await bridge.WindowOverlaySupported().catch(() => false)
    const s = await bridge.GetWindowOverlaySettings().catch(() => null)
    enabled.value = supported.value && !!s?.enabled
  }

  // setEnabled 乐观更新后保存；失败回滚勾选。返回错误文案，成功返回 null。
  async function setEnabled(on: boolean): Promise<string | null> {
    if (on && !supported.value) return '仅 Windows 支持游戏内悬浮层'
    const prev = enabled.value
    enabled.value = on
    try {
      await bridge.SetWindowOverlaySettings({ enabled: on })
      return null
    } catch (e) {
      enabled.value = prev
      return cleanErrorText(e, '未知错误')
    }
  }

  return { enabled, supported, load, setEnabled }
}
