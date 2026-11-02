<script setup lang="ts">
// LoginView 登录页（对齐 Auth.html：分段切换登录/注册、记住密码、忘记密码）。
// 记住密码（问题 6）：启动自动填充（不自动登录）；成功登录且勾选→保存；
// 取消勾选→清除已存凭据。密码由平台保护后落盘（Windows DPAPI / Linux keyring；
// keyring 不可用时不保存，见 issue #8）。
// 游客进入（simple 模式）：一键游客登录，Go 侧复用已记住的游客身份。
// OIDC 登录（#118 P2-5）：服务端下发 oidc_enabled 才显示入口；auth_mode=oidc
// 时密码/注册入口整体关闭（服务端对密码登录直接 403，UI 不给用户撞 403）。
import { onMounted, onUnmounted, reactive, ref } from 'vue'
import { useRouter } from 'vue-router'
import { Eye, EyeOff, LockKeyhole, Mail, Check, UserRound, Info, KeyRound } from 'lucide-vue-next'
import AuthLayout from './AuthLayout.vue'
import Toast from '../../components/Toast.vue'
import { eventsOn, goApp, isNative, isWebBuild } from '../../api/bridge'
import { useAuthStore, kickedBannerText } from '../../stores/auth'
import { cleanErrorText } from '../../utils/errorText'
import './auth-forms.css'

const router = useRouter()
const auth = useAuthStore()
const toast = ref<InstanceType<typeof Toast>>()

const form = reactive({ email: '', password: '', remember: true })
const invalid = reactive({ email: false, password: false })
const showPwd = ref(false)
const loading = ref(false)
const guestLoading = ref(false)
// 无密钥环时「记住密码」不会生效（密码宁可不存），登录页直接说明，避免勾了却没记住。
const secretMode = ref('')

// kicked 被踢横幅数据源（非空即显示，文案按 reason 分支）。进入登录页时取走
// store 里的原因并清（横幅生命周期只在本次访问，不留到下次）；挂载期间的
// auth:kicked 实时刷新（事件可能在本页挂载后才到，store 层回登录页导航完成前的窗口）。
const kicked = ref('')
let offKicked: () => void = () => {}

const emailRe = /^[^\s@]+@[^\s@]+\.[^\s@]+$/

onMounted(async () => {
  kicked.value = auth.kickedReason
  auth.clearKickedReason()
  // eventsOn 返回按监听器的取消函数，卸载时逐个注销（不按事件名整体 Off）。
  offKicked = eventsOn('auth:kicked', (d) => {
    const r = (d as { reason?: unknown } | null)?.reason
    kicked.value = typeof r === 'string' && r !== '' ? r : 'login_elsewhere'
  })
  auth.loadConfig() // 拉服务端能力开关（simple 模式显示「游客进入」）
  secretMode.value = await goApp().SecretStorageMode().catch(() => '')
  // 自动填充记住的凭据（不自动登录，用户手动点登录）。
  try {
    const c = await goApp().GetSavedCredentials()
    if (c.saved) {
      form.email = c.email
      if (c.password) form.password = c.password
      form.remember = true
    }
  } catch { /* 无已存凭据 */ }
})

onUnmounted(() => {
  offKicked()
})

// 取消勾选即清除已存凭据（问题 6c）。
async function onRememberChange() {
  if (!form.remember) {
    await goApp().ClearSavedCredentials()
  }
}

function validate(): boolean {
  invalid.email = !emailRe.test(form.email.trim())
  invalid.password = form.password.length < 8
  return !invalid.email && !invalid.password
}

async function submit() {
  if (!validate()) return
  loading.value = true
  try {
    await auth.login(form.email.trim(), form.password)
    // 登录成功：勾选→保存/更新凭据；未勾选→确保清除。
    if (form.remember) {
      await goApp().SetSavedCredentials(form.email.trim(), form.password)
    } else {
      await goApp().ClearSavedCredentials()
    }
    toast.value?.show('登录成功')
    router.push('/')
  } catch (e) {
    toast.value?.show(cleanErrorText(e, '登录失败'), true)
  } finally {
    loading.value = false
  }
}

// 游客一键进入：身份由 Go 侧维持（已记住则续期同一游客）。
async function guestEnter() {
  if (guestLoading.value) return
  guestLoading.value = true
  try {
    await auth.guest()
    router.push('/')
  } catch (e) {
    toast.value?.show(cleanErrorText(e, '游客登录失败'), true)
  } finally {
    guestLoading.value = false
  }
}

// native 桌面原生环境（whenNativeReady 在 mount 前完成，判定可靠）：
// 「跳浏览器」提示只对桌面有意义，web 是当前窗口直接跳转。
const native = isNative()
const oidcLoading = ref(false)

// oidcLogin 发起 OIDC 登录（#118 P2-5）：authorize 拿授权 URL 后按端分流——
// web 当前窗口跳 IdP（回调落在服务端托管的回调页）；桌面 Go 侧已打开系统浏览器，
// 会话落在那里（本地不交换令牌）；mock 桩返回假 URL，不导航。
async function oidcLogin() {
  if (oidcLoading.value) return
  oidcLoading.value = true
  try {
    const url = await goApp().OidcAuthorize()
    if (isWebBuild() && url) location.assign(url)
  } catch (e) {
    toast.value?.show(cleanErrorText(e, '无法发起 OIDC 登录'), true)
  } finally {
    oidcLoading.value = false
  }
}
</script>

<template>
  <AuthLayout>
    <!-- 被踢提示：本端会话与令牌已被 Go 侧清除。文案按 reason 分支
         （server_changed 换服、login_expired 登录过期，都不是异地登录）。 -->
    <div v-if="kicked" class="kicked-bar" role="status">
      <Info :size="15" />
      <span>{{ kickedBannerText(kicked) }}</span>
    </div>

    <!-- 仅 OIDC 模式：密码/注册入口整体关闭，明示原因而不是让用户撞 403。
         主按钮不受 oidc_enabled 门控——auth_mode=oidc 而 OIDC 未配置完整属部署
         错配，这是唯一登录路径，错配在点击时以服务端错误浮出。 -->
    <div v-if="auth.oidcOnly" class="oidc-only">
      <span class="oidc-badge"><KeyRound :size="20" /></span>
      <p class="oidc-note">此服务器仅支持 OIDC 登录</p>
      <button type="button" class="btn-primary" :class="{ loading: oidcLoading }"
              :disabled="oidcLoading" @click="oidcLogin">
        <span class="spinner" />
        <span class="btn-text">使用 OIDC 登录</span>
      </button>
      <p v-if="native" class="oidc-hint">桌面端将打开浏览器完成登录</p>
    </div>

    <template v-else>
      <div class="seg">
        <div class="seg-thumb" />
        <button class="on">登录</button>
        <button @click="router.push('/register')">注册</button>
      </div>

      <form @submit.prevent="submit" novalidate>
        <div class="field" :class="{ invalid: invalid.email }">
          <label>邮箱</label>
          <div class="input-wrap">
            <Mail :size="17" />
            <input v-model="form.email" type="email" placeholder="you@example.com"
                   autocomplete="email" @input="invalid.email = false">
          </div>
          <p class="field-err">请输入有效的邮箱地址</p>
        </div>

        <div class="field" :class="{ invalid: invalid.password }">
          <label>密码</label>
          <div class="input-wrap">
            <LockKeyhole :size="17" />
            <input v-model="form.password" :type="showPwd ? 'text' : 'password'"
                   placeholder="请输入密码" autocomplete="current-password"
                   @input="invalid.password = false">
            <button type="button" class="eye" tabindex="-1" @click="showPwd = !showPwd">
              <EyeOff v-if="showPwd" :size="16" />
              <Eye v-else :size="16" />
            </button>
          </div>
          <p class="field-err">密码至少 8 位</p>
        </div>

        <div class="opt-row">
          <label class="check">
            <input v-model="form.remember" type="checkbox" @change="onRememberChange">
            <span class="box"><Check :size="11" /></span>记住密码
          </label>
          <button type="button" class="link" @click="router.push('/forgot')">忘记密码？</button>
        </div>
        <p v-if="secretMode === 'file'" class="remember-note">密钥环不可用，密码不会被记住</p>

        <button type="submit" class="btn-primary" :class="{ loading }" :disabled="loading">
          <span class="spinner" />
          <span class="btn-text">登录</span>
        </button>
      </form>

      <!-- 游客一键进入：仅 simple 模式（服务端能力开关下发） -->
      <button v-if="auth.simpleMode" type="button" class="btn-ghost"
              :class="{ loading: guestLoading }" :disabled="guestLoading"
              @click="guestEnter">
        <UserRound :size="15" />
        <span class="btn-text">游客进入</span>
      </button>

      <!-- OIDC 登录（#118 P2-5）：服务端下发 oidc_enabled 才显示。 -->
      <button v-if="auth.oidcEnabled" type="button" class="btn-ghost"
              :class="{ loading: oidcLoading }" :disabled="oidcLoading"
              @click="oidcLogin">
        <KeyRound :size="15" />
        <span class="btn-text">使用 OIDC 登录</span>
      </button>
      <p v-if="auth.oidcEnabled && native" class="oidc-hint">桌面端将打开浏览器完成登录</p>
    </template>

    <Toast ref="toast" />
  </AuthLayout>
</template>

<style scoped>
/* 被踢横幅：主题色系内低对比提示（brand-50 底 + brand-200 描边 + ink-900 字），
   无动画（出现即静态展示）。 */
.kicked-bar {
  display: flex; align-items: center; gap: 8px;
  padding: 10px 12px; margin-bottom: 18px;
  background: var(--brand-50);
  border: 1px solid var(--brand-200);
  border-radius: 10px;
  font-size: 13px; color: var(--ink-900);
}
.kicked-bar svg { color: var(--brand-500); flex: none; }
/* 无密钥环提示：跟在记住密码行下方，弱化存在感，不打断表单节奏。 */
.remember-note {
  margin: -12px 0 14px;
  font-size: 12px; color: var(--ink-400);
}
/* 仅 OIDC 模式提示块：替代密码表单位置，克制、说明性。 */
.oidc-only {
  display: flex; flex-direction: column; align-items: center; gap: 12px;
  padding: 40px 0 8px; text-align: center;
}
.oidc-badge {
  width: 44px; height: 44px; border-radius: 12px;
  background: var(--brand-50); color: var(--brand-500);
  display: flex; align-items: center; justify-content: center;
}
.oidc-note { font-size: 14px; font-weight: 600; color: var(--ink-900); }
.oidc-only .btn-primary { width: 100%; }
/* 「桌面端将打开浏览器完成登录」：按钮下的弱化说明（web 当前窗口跳转，不显示）。 */
.oidc-hint {
  margin-top: 8px; font-size: 12px; color: var(--ink-400); text-align: center;
}
</style>
