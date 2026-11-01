<script setup lang="ts">
// OidcCallbackView OIDC 回调页（#118 P2-5）：IdP 认证完成后重定向回部署配置的
// redirect_url（服务端托管的 Web 客户端），本页取 code/state 调 callback 端点
// 换令牌对，成功走既有登录成功路径进应用。极简：交换中的状态行 + 失败提示与
// 返回登录，无任何宣传文案。
import { onMounted, ref } from 'vue'
import { useRouter } from 'vue-router'
import { KeyRound } from 'lucide-vue-next'
import AuthLayout from './AuthLayout.vue'
import { useAuthStore } from '../../stores/auth'
import { extractOidcCallbackParams, oidcCallbackErrorText } from '../../utils/oidc'
import './auth-forms.css'

const router = useRouter()
const auth = useAuthStore()

// failed 非空即错误态（文案区分 state 失效与其余交换失败，见 oidcCallbackErrorText）。
const failed = ref('')

onMounted(async () => {
  // 参数统一走 extract：hash 段（#/oidc/callback?code=…）与 search 段（路径形
  // redirect_url 改写而来）都接住；route.query 与 hash 段同源，不再单独读。
  const { code, state } = extractOidcCallbackParams(window.location.search, window.location.hash)
  if (!code || !state) {
    failed.value = '缺少授权参数，请回到登录页重新发起'
    return
  }
  try {
    await auth.loginWithOidcCode(code, state)
    // 用 replace：回调地址是一次性的，成功后不应留在历史记录里。
    await router.replace('/')
  } catch (e) {
    failed.value = oidcCallbackErrorText(e)
  }
})
</script>

<template>
  <AuthLayout>
    <div v-if="!failed" class="oidc-state">
      <span class="badge"><KeyRound :size="18" /></span>
      <p class="oidc-title">正在完成登录</p>
    </div>

    <div v-else class="oidc-state">
      <span class="badge error"><KeyRound :size="18" /></span>
      <p class="oidc-title">登录未完成</p>
      <p class="oidc-err" role="alert">{{ failed }}</p>
      <button type="button" class="btn-primary" @click="router.replace('/login')">
        <span class="btn-text">返回登录</span>
      </button>
    </div>
  </AuthLayout>
</template>

<style scoped>
/* 回调页极简态：图标 + 一行状态。成功即走，无需更多元素。 */
.oidc-state {
  display: flex; flex-direction: column; align-items: center; gap: 12px;
  padding: 48px 0;
}
.badge {
  width: 44px; height: 44px; border-radius: 12px;
  background: var(--brand-50); color: var(--brand-500);
  display: flex; align-items: center; justify-content: center;
}
.badge.error { background: #fdecec; color: var(--red); }
.oidc-title { font-size: 15px; font-weight: 600; color: var(--ink-900); }
.oidc-err {
  font-size: 13px; color: var(--red); text-align: center;
  max-width: 320px; line-height: 1.6;
}
/* 返回登录按钮收窄到表单宽度口径，与 auth-forms 的按钮节奏一致。 */
.oidc-state .btn-primary { width: 320px; max-width: 100%; margin-top: 8px; }
</style>
