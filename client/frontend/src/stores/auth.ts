import { defineStore } from 'pinia'
import { computed, ref } from 'vue'
import { goApp } from '../api/bridge'
import type { ServerConfig, UserProfile } from '../api/types'

export const useAuthStore = defineStore('auth', () => {
  const user = ref<UserProfile | null>(null)
  const loggedIn = ref(false)
  // 信令连接态：false 时主界面顶部显示「信令连接中」提示条（Go 侧重试中）。
  const signalOk = ref(true)

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
  }

  async function register(nickname: string, email: string, password: string, code: string) {
    const r = await goApp().Register(nickname, email, password, code)
    user.value = r.user
    loggedIn.value = true
    signalOk.value = r.session_ok !== false
  }

  // 游客一键进入（simple 模式）。Go 侧负责复用已记住的游客身份（refresh 轮换）。
  async function guest() {
    const r = await goApp().LoginAsGuest()
    user.value = r.user
    loggedIn.value = true
    signalOk.value = r.session_ok !== false
  }

  // setSignalOk 由 MainView 订阅 auth:session 事件调用（Go 侧重试/断线状态变化）。
  function setSignalOk(ok: boolean) {
    signalOk.value = ok
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
    // 清理各业务 store 的本地缓存（B2：chat.clear 此前是死代码，
    // 登出后换账号登录会继承上一账号的聊天记录）。
    const { useChatStore } = await import('./chat')
    const { useVoiceStore } = await import('./voice')
    useChatStore().clear()
    useVoiceStore().$reset()
  }

  async function fetchMe() {
    user.value = await goApp().GetMe()
    loggedIn.value = true
  }

  return {
    user, loggedIn, signalOk, serverCfg, simpleMode, mailConfigured,
    loadConfig, login, register, guest, sendEmailCode, resetPassword, logout, fetchMe,
    setSignalOk,
  }
})
