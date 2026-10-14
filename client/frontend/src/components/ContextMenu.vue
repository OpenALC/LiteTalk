<script setup lang="ts">
// ContextMenu 通用右键菜单：光标处弹出，点击外部 / Esc / 再次右键关闭。
// 危险项（danger）首次点击变为「再次点击确认」二次确认态，避免误删。
import { computed, onBeforeUnmount, onMounted, ref } from 'vue'
import type { Component } from 'vue'

export interface CtxItem {
  key: string
  label: string
  icon?: Component
  danger?: boolean   // 红色项；配合 confirm 启用二次确认
  confirm?: boolean  // 首次点击不派发，转为确认态
  disabled?: boolean
}

const props = defineProps<{ x: number; y: number; items: CtxItem[] }>()
const emit = defineEmits<{ (e: 'select', key: string): void; (e: 'close'): void }>()

// 二次确认态：记录待确认的 danger 项 key（确认态下标签变为「再次点击确认」）。
const confirming = ref('')

// 视口内收拢：菜单贴右/下边缘时向左/上偏移，防止溢出屏幕。
const pos = computed(() => {
  const w = 180
  const h = props.items.length * 36 + 12
  return {
    left: Math.min(props.x, window.innerWidth - w - 8) + 'px',
    top: Math.min(props.y, window.innerHeight - h - 8) + 'px',
  }
})

function onItem(it: CtxItem) {
  if (it.disabled) return
  if (it.danger && it.confirm && confirming.value !== it.key) {
    confirming.value = it.key // 第一次点击：进入确认态，不派发
    return
  }
  emit('select', it.key)
  emit('close')
}

function onKey(e: KeyboardEvent) {
  if (e.key === 'Escape') emit('close')
}
onMounted(() => window.addEventListener('keydown', onKey))
onBeforeUnmount(() => window.removeEventListener('keydown', onKey))
</script>

<template>
  <!-- 透明遮罩：点击任意处关闭；拦截 contextmenu 防止菜单内再右键弹系统菜单 -->
  <div class="ctx-mask" @click="emit('close')" @contextmenu.prevent="emit('close')">
    <div class="ctx-menu" :style="pos" @click.stop>
      <button v-for="it in items" :key="it.key" class="ctx-item"
              :class="{ danger: it.danger, armed: confirming === it.key }"
              :disabled="it.disabled" @click="onItem(it)">
        <component :is="it.icon" v-if="it.icon" :size="14" />
        {{ confirming === it.key ? '再次点击确认' : it.label }}
      </button>
    </div>
  </div>
</template>

<style scoped>
.ctx-mask { position: fixed; inset: 0; z-index: 80; }
.ctx-menu {
  position: fixed; min-width: 172px; padding: 6px;
  background: var(--surface); border: 1px solid var(--line);
  border-radius: 12px; box-shadow: var(--shadow-2);
  display: flex; flex-direction: column; gap: 1px;
}
.ctx-item {
  display: flex; align-items: center; gap: 8px;
  height: 34px; padding: 0 10px; border-radius: 8px;
  font-size: 13px; font-weight: 500; color: var(--ink-600);
  text-align: left; white-space: nowrap;
  transition: background 0.15s, color 0.15s;
}
.ctx-item:hover:not(:disabled) { background: var(--brand-50); color: var(--ink-900); }
.ctx-item:disabled { color: var(--ink-300); cursor: default; }
.ctx-item.danger { color: var(--red); }
.ctx-item.danger:hover:not(:disabled) { background: rgba(232, 91, 91, 0.1); }
/* 二次确认态：红底实心，视觉明确 */
.ctx-item.armed { background: var(--red); color: #fff; }
</style>
