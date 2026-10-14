<script setup lang="ts">
// LoginView 登录页（对齐 Auth.html：分段切换登录/注册、记住密码、忘记密码）。
// 记住密码（问题 6）：启动自动填充（不自动登录）；成功登录且勾选→保存；
// 取消勾选→清除已存凭据。密码经 DPAPI 加密落盘（Go 侧）。
// 游客进入（simple 模式）：一键游客登录，Go 侧复用已记住的游客身份。
import { onMounted, reactive, ref } from 'vue'
import { useRouter } from 'vue-router'
import { Eye, EyeOff, LockKeyhole, Mail, Check, UserRound } from 'lucide-vue-next'
import AuthLayout from './AuthLayout.vue'
import Toast from '../../components/Toast.vue'
import { goApp } from '../../api/bridge'
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

const emailRe = /^[^\s@]+@[^\s@]+\.[^\s@]+$/

onMounted(async () => {
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
