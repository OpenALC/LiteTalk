import { defineStore } from 'pinia'
import { ref } from 'vue'

// 全局轻提示：跨组件动作（右键菜单、弹层提交等）的统一反馈通道。
// 渲染端为 GlobalToast（挂在 App.vue 根部，覆盖全部路由页）；
// AuthLayout/SettingsView/TitleBar 内的局部 Toast 保留不动。
export const useNotifyStore = defineStore('notify', () => {
  const visible = ref(false)
  const text = ref('')
  const isError = ref(false)
  let timer: ReturnType<typeof setTimeout> | null = null

  function show(msg: string, err = false) {
    text.value = msg
    isError.value = err
    visible.value = true
    if (timer) clearTimeout(timer)
    timer = setTimeout(() => (visible.value = false), 2200)
  }

  // fromErr 从异常提取可读文案：剥掉 JS Error 前缀，服务端 message 通常已是中文。
  function fromErr(e: unknown, fallback = '操作失败，请稍后再试') {
    const m = String(e ?? '').replace(/^Error:\s*/i, '').trim()
    show(m || fallback, true)
  }

  return { visible, text, isError, show, fromErr }
})
