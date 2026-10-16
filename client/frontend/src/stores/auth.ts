import { defineStore } from 'pinia'
import { computed, ref } from 'vue'
import { eventsOn, goApp } from '../api/bridge'
import type { ServerConfig, UserProfile } from '../api/types'

export const useAuthStore = defineStore('auth', () => {
  const user = ref<UserProfile | null>(null)
  const loggedIn = ref(false)
  // 信令连接态：false 时主界面顶部显示「信令连接中」提示条（Go 侧重试中）。
  const signalOk = ref(true)

  // kickedReason 被踢下线原因（auth:kicked 载荷 reason）：非空表示本端已在
  // 其他设备登录被登出。跨组件的传输通道——LoginView 进入时取走并清，
  // 横幅文案固定，不按 reason 分支。
  const kickedReason = ref('')

  // 服务端能力开关（启动时拉取一次，注册/找回页据此适配 UI）。
  const serverCfg = ref<ServerConfig>({ auth_mode: 'full', mail_configured: false })
  const simpleMode = computed(() => serverCfg.value.auth_mode === 'simple')
  const mailConfigured = computed(() => serverCfg.value.mail_configured)

  async function loadConfig() {
    try {
      serverCfg.value = await goApp().GetAuthConfig()
    } catch {
      // 拉取失败保持 full 默认（验证码流程，向后兼容）。
    }
  }

  async function login(email: string, password: string) {
    const r = await goApp().Login(email, password)
    user.value = r.user
    loggedIn.value = true
    signalOk.value = r.session_ok !== false
    clearKickedReason() // 新登录覆盖被踢态，下次进登录页不残留横幅
  }

  async function register(nickname: string, email: string, password: string, code: string) {
    const r = await goApp().Register(nickname, email, password, code)
    user.value = r.user
    loggedIn.value = true
    signalOk.value = r.session_ok !== false
    clearKickedReason()
  }

  // 游客一键进入（simple 模式）。Go 侧负责复用已记住的游客身份（refresh 轮换）。
  async function guest() {
    const r = await goApp().LoginAsGuest()
    user.value = r.user
    loggedIn.value = true
    signalOk.value = r.session_ok !== false
    clearKickedReason()
  }

  // setSignalOk 由 MainView 订阅 auth:session 事件调用（Go 侧重试/断线状态变化）。
  function setSignalOk(ok: boolean) {
    signalOk.value = ok
  }

  function setKickedReason(reason: string) {
    kickedReason.value = reason || 'login_elsewhere'
  }

  function clearKickedReason() {
    kickedReason.value = ''
  }

  async function sendEmailCode(email: string, purpose: 'register' | 'reset') {
    await goApp().SendEmailCode(email, purpose)
  }

  async function resetPassword(email: string, code: string, newPassword: string) {
    await goApp().ResetPassword(email, code, newPassword)
  }

  async function logout() {
    await goApp().Logout()
    user.value = null
    loggedIn.value = false
    signalOk.value = true
    clearKickedReason()
    // 清理各业务 store 的本地缓存（B2：chat.clear 此前是死代码，
    // 登出后换账号登录会继承上一账号的聊天记录）。
    const { useChatStore } = await import('./chat')
    const { useVoiceStore } = await import('./voice')
    useChatStore().clear()
    useVoiceStore().$reset()
  }

  // handleKicked 被踢全链路（auth:kicked）：复用 logout 清本地会话缓存与登出态，
  // 再置原因回登录页——顺序保证 LoginView 挂载时能取到原因。Go 侧 Logout 此时
  // 是空转（refreshTok 已被 Go onKicked 清空，不发 REST）。
  async function handleKicked(reason: string) {
    await logout()
    setKickedReason(reason)
    const { default: router } = await import('../router')
    if (router.currentRoute.value.name !== 'login') {
      await router.push({ name: 'login' }).catch(() => {})
    }
  }

  // 被踢监听挂 store 层而非组件：事件到达时用户多在主界面（LoginView 未挂载），
  // 事件不能只等它。node 测试环境无 window（eventsOn→isNative 会抛），跳过订阅；
  // LoginView 挂载后另有兜底监听实时刷横幅（按 eventsOn 取消函数注销）。
  if (typeof window !== 'undefined') {
    eventsOn('auth:kicked', (d) => {
      const r = (d as { reason?: unknown } | null)?.reason
      handleKicked(typeof r === 'string' && r !== '' ? r : 'login_elsewhere').catch(() => {})
    })
  }

  async function fetchMe() {
    user.value = await goApp().GetMe()
    loggedIn.value = true
  }

  return {
    user, loggedIn, signalOk, serverCfg, simpleMode, mailConfigured,
    kickedReason,
    loadConfig, login, register, guest, sendEmailCode, resetPassword, logout, fetchMe,
    setSignalOk, setKickedReason, clearKickedReason,
  }
})
