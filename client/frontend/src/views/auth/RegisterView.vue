<script setup lang="ts">
// RegisterView 注册页（对齐 Auth.html：昵称/邮箱/验证码倒计时/密码强度）。
import { computed, onUnmounted, reactive, ref } from 'vue'
import { useRouter } from 'vue-router'
import { Check, Eye, EyeOff, KeyRound, LockKeyhole, Mail, ShieldCheck, UserRound } from 'lucide-vue-next'
import AuthLayout from './AuthLayout.vue'
import Toast from '../../components/Toast.vue'
import { useAuthStore } from '../../stores/auth'
import './auth-forms.css'

const router = useRouter()
const auth = useAuthStore()
const toast = ref<InstanceType<typeof Toast>>()
auth.loadConfig()

const form = reactive({ nickname: '', email: '', code: '', password: '', invite: '' })
const invalid = reactive({ name: false, email: false, code: false, password: false })
const showPwd = ref(false)
const loading = ref(false)

const emailRe = /^[^\s@]+@[^\s@]+\.[^\s@]+$/
const cd = ref(0)
let cdTimer: ReturnType<typeof setInterval> | null = null

const strengthClass = computed(() => {
  const v = form.password
  if (!v) return ''
  let s = 0
  if (v.length >= 8) s++
  if (/\d/.test(v) && /[a-zA-Z]/.test(v)) s++
  if (v.length >= 12 && /[^a-zA-Z0-9]/.test(v)) s++
  return s ? `s${s}` : ''
})

function validate(): boolean {
  invalid.name = form.nickname.trim().length < 2 || form.nickname.trim().length > 16
  invalid.email = !emailRe.test(form.email.trim())
  invalid.code = !auth.simpleMode && !/^\d{6}$/.test(form.code)
  invalid.password = form.password.length < 8
  return !(invalid.name || invalid.email || invalid.code || invalid.password)
}

// friendlyErr 发码/注册失败的友好提示（不暴露内部错误）。
// fallback 默认按发码场景取文案；提交动作应显式传业务兜底（发信通道与注册无关）。
function friendlyErr(e: unknown, fallback = auth.mailConfigured ? '操作失败，请稍后再试' : '发送失败：服务未配置发信通道'): string {
  const s = String(e)
  if (s.includes('too_frequent') || s.includes('发送过于频繁')) return '发送过于频繁，请稍后再试'
  if (s.includes('daily_limit') || s.includes('上限')) return '今日发送次数已达上限'
  if (s.includes('email_taken') || s.includes('已注册')) return '邮箱已注册'
  if (s.includes('bad_credentials') || s.includes('密码错误')) return '邮箱或密码错误'
  if (s.includes('bad_bootstrap') || s.includes('邀请码无效')) return '邀请码无效或已被使用'
  return fallback
}

async function sendCode() {
  invalid.email = !emailRe.test(form.email.trim())
  if (invalid.email) return
  try {
    await auth.sendEmailCode(form.email.trim(), 'register')
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
    await auth.register(form.nickname.trim(), form.email.trim(), form.password,
      auth.simpleMode ? '' : form.code, form.invite.trim())
    toast.value?.show('注册成功')
    router.push('/')
  } catch (e) {
    toast.value?.show(friendlyErr(e, '注册失败，请稍后再试'), true)
  } finally {
    loading.value = false
  }
}

onUnmounted(() => { if (cdTimer) clearInterval(cdTimer) })
</script>

<template>
  <AuthLayout>
    <div class="seg reg">
      <div class="seg-thumb" />
      <button @click="router.push('/login')">登录</button>
      <button class="on">注册</button>
    </div>

    <form @submit.prevent="submit" novalidate>
      <div class="field" :class="{ invalid: invalid.name }">
        <label>昵称</label>
        <div class="input-wrap">
          <UserRound :size="17" />
          <input v-model="form.nickname" type="text" placeholder="2–16 个字符"
                 maxlength="16" @input="invalid.name = false">
        </div>
        <p class="field-err">昵称需为 2–16 个字符</p>
      </div>

      <div class="field" :class="{ invalid: invalid.email }">
        <label>邮箱</label>
        <div :class="auth.simpleMode ? 'input-wrap' : 'code-row'">
          <div class="input-wrap">
            <Mail :size="17" />
            <input v-model="form.email" type="email" placeholder="you@example.com"
                   autocomplete="email" @input="invalid.email = false">
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

      <!-- 管理员邀请码（#27）：自托管引导，可选 -->
      <div v-if="auth.showInviteField" class="field">
        <label>邀请码</label>
        <div class="input-wrap">
          <KeyRound :size="17" />
          <input v-model="form.invite" type="text" placeholder="管理员邀请码（选填）"
                 maxlength="32" autocomplete="off">
        </div>
        <p v-if="auth.bootstrapRequired" class="field-hint">填入服务端终端打印的邀请码，创建你的服务器</p>
      </div>

      <div class="field" :class="{ invalid: invalid.password }">
        <label>设置密码</label>
        <div class="input-wrap">
          <LockKeyhole :size="17" />
          <input v-model="form.password" :type="showPwd ? 'text' : 'password'"
                 placeholder="至少 8 位，建议混合字母与数字"
                 autocomplete="new-password" @input="invalid.password = false">
          <button type="button" class="eye" tabindex="-1" @click="showPwd = !showPwd">
            <EyeOff v-if="showPwd" :size="16" />
            <Eye v-else :size="16" />
          </button>
        </div>
        <div class="strength" :class="strengthClass"><i /><i /><i /></div>
        <p class="field-err">密码至少 8 位</p>
      </div>

      <button type="submit" class="btn-primary" :class="{ loading }"
              :disabled="loading" style="margin-top: 6px">
        <span class="spinner" />
        <span class="btn-text">创建账号</span>
      </button>
    </form>

    <Toast ref="toast" />
  </AuthLayout>
</template>
