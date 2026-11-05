import { computed, ref } from 'vue'
import type { RuntimeGCSettings } from '../api/bridge'
import { cleanErrorText } from './errorText'

export const GC_LEVEL_LABELS = ['最低', '较低', '默认', '较高', '最高'] as const

interface GCSettingsBridge {
  GetGCSettings(): Promise<RuntimeGCSettings>
  SetGCLevel(level: number): Promise<RuntimeGCSettings>
}

export function useGCSettings(bridge: GCSettingsBridge) {
  const settings = ref<RuntimeGCSettings | null>(null)
  const level = ref(3)
  const loading = ref(true)
  const saving = ref(false)
  const error = ref('')
  let pending: number | null = null

  const disabled = computed(() => !settings.value?.supported ||
    (settings.value.gogc_overridden && settings.value.memory_limit_overridden))
  const label = computed(() => GC_LEVEL_LABELS[level.value - 1] ?? GC_LEVEL_LABELS[2])

  async function load() {
    loading.value = true
    error.value = ''
    try {
      const current = await bridge.GetGCSettings()
      settings.value = current
      level.value = current.level
    } catch (e) {
      settings.value = null
      error.value = `读取 GC 强度失败：${cleanErrorText(e, '未知错误')}`
    } finally {
      loading.value = false
    }
  }

  // Wails 调用按顺序发送；拖动时只保留尚未发送的最新档位。
  // 已确认值由 Go 回包决定，最后一次失败回到该值。
  async function setLevel(next: number) {
    if (!Number.isInteger(next) || next < 1 || next > 5 || disabled.value || loading.value) return
    level.value = next
    pending = next
    if (saving.value) return
    saving.value = true
    try {
      while (pending !== null) {
        const requested = pending
        pending = null
        try {
          const current = await bridge.SetGCLevel(requested)
          settings.value = current
          error.value = ''
          if (pending === null) level.value = current.level
          if (!current.supported || (current.gogc_overridden && current.memory_limit_overridden)) {
            pending = null
            level.value = current.level
          }
        } catch (e) {
          error.value = `保存 GC 强度失败：${cleanErrorText(e, '未知错误')}`
          if (pending === null) level.value = settings.value?.level ?? 3
        }
      }
    } finally {
      saving.value = false
    }
  }

  return { settings, level, label, loading, saving, disabled, error, load, setLevel }
}
