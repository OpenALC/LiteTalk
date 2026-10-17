<script setup lang="ts">
// LoginView 登录页（对齐 Auth.html：分段切换登录/注册、记住密码、忘记密码）。
// 记住密码（问题 6）：启动自动填充（不自动登录）；成功登录且勾选→保存；
// 取消勾选→清除已存凭据。密码经 DPAPI 加密落盘（Go 侧）。
// 游客进入（simple 模式）：一键游客登录，Go 侧复用已记住的游客身份。
import { onMounted, onUnmounted, reactive, ref } from 'vue'
import { useRouter } from 'vue-router'
import { Eye, EyeOff, LockKeyhole, Mail, Check, UserRound, Info } from 'lucide-vue-next'
import AuthLayout from './AuthLayout.vue'
import Toast from '../../components/Toast.vue'
import { eventsOn, goApp } from '../../api/bridge'
import { useAuthStore } from '../../stores/auth'
import './auth-forms.css'

const router = useRouter()
const auth = useAuthStore()
const toast = ref<InstanceType<typeof Toast>>()

const form = reactive({ email: '', password: '', remember: true })
const invalid = reactive({ email: false, password: false })
const showPwd = ref(false)
const loading = ref(false)
const guestLoading = ref(false)

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
    toast.value?.show(String(e), true)
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
    toast.value?.show(String(e), true)
  } finally {
    guestLoading.value = false
  }
}
</script>

<template>
  <AuthLayout>
    <!-- 被踢提示：本端会话与令牌已被 Go 侧清除。server_changed 是服务器地址
         热切换后令牌不被新服务端接受，不是异地登录，文案必须分开。 -->
    <div v-if="kicked" class="kicked-bar" role="status">
      <Info :size="15" />
      <span>{{ kicked === 'server_changed' ? '当前令牌不被新服务器接受，请重新登录' : '账号已在其他设备登录' }}</span>
    </div>

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
</style>
