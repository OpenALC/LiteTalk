import { defineStore } from 'pinia'
import { ref } from 'vue'
import { goApp } from '../api/bridge'
import type { ChatMessage } from '../api/types'
import { cleanErrorText } from '../utils/errorText'

// 全局消息搜索面板状态（#42 B2）。面板本身只负责展示；搜索请求按「代次 +
// 请求序号」双重保护：连续输入时旧响应可能晚于新响应返回，晚到的旧结果绝不
// 覆盖新查询；close/resetAll 递增代次，在飞请求返回后直接丢弃。
export const useSearchStore = defineStore('search', () => {
  // open 控制 MainView 里 SearchDialog 的挂载（v-if）。
  const open = ref(false)
  // query 当前输入（trim 后）；resultQuery 是产生 results 的那次查询词，
  // 高亮必须按它切片——输入框继续敲字时旧结果的命中位置不会漂移。
  const query = ref('')
  const resultQuery = ref('')
  const results = ref<ChatMessage[]>([])
  const loading = ref(false)
  const error = ref('')

  // generation 会话代次（登出/被踢/关闭面板递增）；requestSeq 查询序号。
  let generation = 0
  let requestSeq = 0

  // clear 清空面板数据并作废在飞请求。close 与 resetAll 共用。
  function clear() {
    generation++
    requestSeq++
    query.value = ''
    resultQuery.value = ''
    results.value = []
    loading.value = false
    error.value = ''
  }

  function openDialog() {
    open.value = true
  }

  function close() {
    open.value = false
    clear()
  }

  // searchNow 执行一次搜索（空查询只清空结果，不发请求）。错误停在面板内
  // （error），不弹全局 toast——连续输入时逐个报错会刷屏。
  async function searchNow(rawQuery: string, limit = 20): Promise<void> {
    const q = rawQuery.trim()
    const gen = generation
    const seq = ++requestSeq
    query.value = q
    if (!q) {
      resultQuery.value = ''
      results.value = []
      loading.value = false
      error.value = ''
      return
    }
    loading.value = true
    error.value = ''
    try {
      const out = await goApp().SearchMessages(q, limit)
      if (gen !== generation || seq !== requestSeq) return
      results.value = out ?? []
      resultQuery.value = q
    } catch (e) {
      if (gen !== generation || seq !== requestSeq) return
      results.value = []
      resultQuery.value = ''
      error.value = cleanErrorText(e, '搜索失败')
    } finally {
      if (gen === generation && seq === requestSeq) loading.value = false
    }
  }

  // resetAll 登出/被踢/换服时复位（由 auth.resetSessionStores 调用）：
  // 关闭面板，旧账号的查询与结果不能出现在下一个会话里。
  function resetAll() {
    open.value = false
    clear()
  }

  return {
    open, query, resultQuery, results, loading, error,
    openDialog, close, searchNow, resetAll,
  }
})
