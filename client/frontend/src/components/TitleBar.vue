<script setup lang="ts">
// TitleBar 自定义标题栏（无边框窗口拖拽区 + 设置/最小化/最大化/关闭）。
// Wails 无边框窗口（v2/v3 相同）：拖拽用 --wails-draggable（WebView2 不认 Electron 的
// -webkit-app-region）。
// 关闭走 useWindowClose：首次关窗提示「已最小化到托盘」（问题 4d）。
import { ref } from 'vue'
import { useRouter } from 'vue-router'
import { Minus, Square, X, AudioWaveform, Settings } from 'lucide-vue-next'
import { goApp } from '../api/bridge'
import { closeWithHint } from '../composables/useWindowClose'
import Toast from './Toast.vue'

const toast = ref<InstanceType<typeof Toast>>()
const router = useRouter()
</script>

<template>
  <header class="titlebar">
    <span class="tb-logo"><AudioWaveform :size="11" /></span>
    <span class="tb-name">轻音 LiteTalk</span>
    <!-- 设置按钮必须留在 no-drag 的 .tb-controls 内，否则点击会被标题栏拖拽吞掉。 -->
    <div class="tb-controls">
      <button title="设置" @click="router.push('/settings')"><Settings :size="15" /></button>
      <button title="最小化" @click="goApp().WindowMinimize()"><Minus :size="15" /></button>
      <button title="最大化" @click="goApp().WindowMaximize()"><Square :size="15" /></button>
      <button class="tb-close" title="关闭" @click="closeWithHint()"><X :size="15" /></button>
    </div>
    <Toast ref="toast" />
  </header>
</template>

<style scoped>
.titlebar {
  height: 38px; flex: none;
  display: flex; align-items: center;
  padding: 0 6px 0 14px;
  background: var(--surface);
  border-bottom: 1px solid var(--line);
  --wails-draggable: drag;
  user-select: none;
  z-index: 30;
}
.tb-logo {
  width: 18px; height: 18px; border-radius: 5px;
  background: var(--brand-500);
  display: flex; align-items: center; justify-content: center;
  margin-right: 8px;
}
.tb-logo :deep(svg) { color: #fff; }
.tb-name { font-size: 12px; font-weight: 600; color: var(--ink-600); letter-spacing: 0.4px; }
.tb-controls { margin-left: auto; display: flex; --wails-draggable: no-drag; }
.tb-controls button {
  width: 42px; height: 38px;
  display: flex; align-items: center; justify-content: center;
  color: var(--ink-400);
  transition: background 0.15s, color 0.15s;
}
.tb-controls button:hover { background: var(--brand-50); color: var(--ink-900); }
.tb-controls .tb-close:hover { background: var(--red); color: #fff; }
</style>
