<script setup lang="ts">
// ForgotView 找回密码页（对齐 Auth.html：邮箱+验证码+新密码）。
import { onUnmounted, reactive, ref } from 'vue'
import { useRouter } from 'vue-router'
import { ArrowLeft, Eye, EyeOff, LockKeyhole, Mail, ShieldCheck } from 'lucide-vue-next'
import AuthLayout from './AuthLayout.vue'
import Toast from '../../components/Toast.vue'
import { useAuthStore } from '../../stores/auth'
import './auth-forms.css'

const router = useRouter()
const auth = useAuthStore()
const toast = ref<InstanceType<typeof Toast>>()
auth.loadConfig()

const form = reactive({ email: '', code: '', password: '' })
const invalid = reactive({ email: false, code: false, password: false })
const showPwd = ref(false)
const loading = ref(false)

const emailRe = /^[^\s@]+@[^\s@]+\.[^\s@]+$/
const cd = ref(0)
let cdTimer: ReturnType<typeof setInterval> | null = null

function validate(): boolean {
  invalid.email = !emailRe.test(form.email.trim())
  invalid.code = !auth.simpleMode && !/^\d{6}$/.test(form.code)
  invalid.password = form.password.length < 8
  return !(invalid.email || invalid.code || invalid.password)
}

// fallback 默认按发码场景取文案；提交动作应显式传业务兜底（发信通道与重置无关）。
function friendlyErr(e: unknown, fallback = auth.mailConfigured ? '操作失败，请稍后再试' : '发送失败：服务未配置发信通道'): string {
  const s = String(e)
  if (s.includes('too_frequent') || s.includes('发送过于频繁')) return '发送过于频繁，请稍后再试'
  if (s.includes('daily_limit') || s.includes('上限')) return '今日发送次数已达上限'
  if (s.includes('not_found') || s.includes('未注册')) return '邮箱未注册'
  return fallback
}

async function sendCode() {
  invalid.email = !emailRe.test(form.email.trim())
  if (invalid.email) return
  try {
    await auth.sendEmailCode(form.email.trim(), 'reset')
    toast.value?.show('验证码已发送')
    cd.value = 60
    cdTimer = setInterval(() => {
      cd.value--
      if (cd.value <= 0 && cdTimer) { clearInterval(cdTimer); cdTimer = null }
    }, 1000)
  } catch (e) {
    toast.value?.show(friendlyErr(e), true)
  }
}

async function submit() {
  if (!validate()) return
  loading.value = true
  try {
    await auth.resetPassword(form.email.trim(), auth.simpleMode ? '' : form.code, form.password)
    toast.value?.show('密码已重置')
    router.push('/login')
  } catch (e) {
    toast.value?.show(friendlyErr(e, '重置失败，请稍后再试'), true)
  } finally {
    loading.value = false
  }
}

onUnmounted(() => { if (cdTimer) clearInterval(cdTimer) })
</script>

<template>
  <AuthLayout>
    <button type="button" class="back-link" @click="router.push('/login')">
      <ArrowLeft :size="15" />返回登录
    </button>
    <h2 class="view-title">重置密码</h2>
    <p class="view-sub">{{ auth.simpleMode ? '填写邮箱与新密码即可重置' : '验证码将发送到你的注册邮箱' }}</p>

    <form @submit.prevent="submit" novalidate>
      <div class="field" :class="{ invalid: invalid.email }">
        <label>邮箱</label>
        <div :class="auth.simpleMode ? 'input-wrap' : 'code-row'">
          <div class="input-wrap">
            <Mail :size="17" />
            <input v-model="form.email" type="email" placeholder="you@example.com"
                   @input="invalid.email = false">
          </div>
          <button v-if="!auth.simpleMode" type="button" class="code-btn" :disabled="cd > 0" @click="sendCode">
            {{ cd > 0 ? `${cd}s` : '发送验证码' }}
          </button>
        </div>
        <p class="field-err">请输入有效的邮箱地址</p>
      </div>

      <div v-if="!auth.simpleMode" class="field" :class="{ invalid: invalid.code }">
        <label>邮箱验证码</label>
        <div class="input-wrap">
          <ShieldCheck :size="17" />
          <input v-model="form.code" type="text" placeholder="6 位数字"
                 maxlength="6" inputmode="numeric" @input="invalid.code = false">
        </div>
        <p class="field-err">请输入 6 位验证码</p>
      </div>

      <div class="field" :class="{ invalid: invalid.password }">
        <label>新密码</label>
        <div class="input-wrap">
          <LockKeyhole :size="17" />
          <input v-model="form.password" :type="showPwd ? 'text' : 'password'"
                 placeholder="至少 8 位" autocomplete="new-password"
                 @input="invalid.password = false">
          <button type="button" class="eye" tabindex="-1" @click="showPwd = !showPwd">
            <EyeOff v-if="showPwd" :size="16" />
            <Eye v-else :size="16" />
          </button>
        </div>
        <p class="field-err">密码至少 8 位</p>
      </div>

      <button type="submit" class="btn-primary" :class="{ loading }"
              :disabled="loading" style="margin-top: 6px">
        <span class="spinner" />
        <span class="btn-text">重置密码</span>
      </button>
    </form>

    <Toast ref="toast" />
  </AuthLayout>
</template>
