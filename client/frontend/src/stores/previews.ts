import { defineStore } from 'pinia'
import { reactive, ref } from 'vue'
import { goApp } from '../api/bridge'
import type { LinkPreview } from '../api/types'

// 链接预览缓存（#42 B4）：按 URL 去重请求，同一 URL 只抓一次。
// 值 'none' 是负缓存（服务端返回无预览/请求失败）——错误静默处理，不打扰聊天。
// generation 在 resetAll（登出/换服）时递增，晚到的旧响应直接丢弃。
export const usePreviewStore = defineStore('previews', () => {
  const map = ref<Record<string, LinkPreview | 'none'>>({})
  const loading = reactive(new Set<string>())
  let generation = 0

  function get(url: string): LinkPreview | 'none' | undefined {
    return map.value[url]
  }

  function isLoading(url: string): boolean {
    return loading.has(url)
  }

  // ensure 确保 url 的预览已请求：已在飞或已有结果（含负缓存）时直接返回。
  async function ensure(rawURL: string): Promise<void> {
    const url = rawURL.trim()
    if (!url) return
    if (map.value[url] !== undefined || loading.has(url)) return
    const gen = generation
    loading.add(url)
    try {
      const p = await goApp().GetLinkPreview(url)
      if (gen !== generation) return
      map.value[url] = p ?? 'none'
    } catch {
      // 无预览是常态（非 HTML、抓取失败、功能关闭），不弹 toast、不重试。
      if (gen !== generation) return
      map.value[url] = 'none'
    } finally {
      if (gen === generation) loading.delete(url)
    }
  }

  // resetAll 登出/被踢/换服时清空，旧账号的预览不能进入下一个会话。
  function resetAll(): void {
    generation++
    map.value = {}
    loading.clear()
  }

  return { map, get, isLoading, ensure, resetAll }
})
