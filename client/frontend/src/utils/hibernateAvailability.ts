import { computed, ref } from 'vue'

export type HibernateDestroyState = 'loading' | 'available' | 'unavailable' | 'error'

// 能力未确认时先隐藏 destroy；查询失败与明确不支持须分别提示。
export function useHibernateDestroyAvailability(query: () => Promise<boolean>) {
  const state = ref<HibernateDestroyState>('loading')
  const destroyAvailable = computed(() => state.value === 'available')

  async function load(): Promise<void> {
    try {
      state.value = await query() ? 'available' : 'unavailable'
    } catch {
      state.value = 'error'
    }
  }

  return { state, destroyAvailable, load }
}
