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
    <div v-if="notify.visible" class="toast" :class="{ err: notify.isError }">
      <CircleAlert v-if="notify.isError" :size="16" />
      <CircleCheck v-else :size="16" />
      <span>{{ notify.text }}</span>
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
