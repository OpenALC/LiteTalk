<script setup lang="ts">
// GlobalToast 全局轻提示（notify store 驱动，挂 App.vue 根部）。
// 样式与局部 Toast.vue 同款（深底圆角、成功/错误态），差异：pointer-events
// 一律穿透——全局常驻节点绝不能拦截点击。
import { CircleCheck, CircleAlert } from 'lucide-vue-next'
import { useNotifyStore } from '../stores/notify'

const notify = useNotifyStore()
</script>

<template>
  <Transition name="toast">
    <!-- role=status：提示是状态播报（如「已在其他设备加入语音」），
         辅助技术应 passively 读出，不该抢焦点。 -->
    <div v-if="notify.visible" class="toast" :class="{ err: notify.isError, actionable: !!notify.actionLabel }" role="status">
      <CircleAlert v-if="notify.isError" :size="16" />
      <CircleCheck v-else :size="16" />
      <span>{{ notify.text }}</span>
      <!-- 操作按钮（如「请求静音 → 接受」）：仅在 showAction 时渲染，恢复可点击 -->
      <button v-if="notify.actionLabel" class="toast-act" @click="notify.runAction()">
        {{ notify.actionLabel }}
      </button>
    </div>
  </Transition>
</template>

<style scoped>
.toast {
  position: fixed; top: 52px; left: 50%; z-index: 90;
  display: flex; align-items: center; gap: 8px;
  padding: 10px 18px; border-radius: 11px;
  background: var(--ink-900); color: #fff;
  font-size: 13px; font-weight: 500;
  box-shadow: var(--shadow-2);
  pointer-events: none; /* 全局常驻：绝不拦截点击 */
}
/* actionable：带操作按钮时恢复可点击（按钮自身也设 pointer-events 双保险） */
.toast.actionable { pointer-events: auto; }
.toast-act {
  padding: 4px 12px; border-radius: 7px; margin-left: 4px;
  background: var(--brand-500); color: #fff;
  font-size: 12.5px; font-weight: 600;
  pointer-events: auto;
  transition: background 0.15s;
}
.toast-act:hover { background: var(--brand-600); }
.toast svg { color: var(--green); }
.toast.err svg { color: var(--red); }
.toast-enter-active, .toast-leave-active {
  transition: opacity 0.3s var(--ease), transform 0.3s var(--ease);
}
.toast-enter-from, .toast-leave-to {
  opacity: 0; transform: translate(-50%, -16px);
}
.toast { transform: translate(-50%, 0); }
</style>
