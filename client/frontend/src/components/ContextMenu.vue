<script setup lang="ts">
// ContextMenu 通用右键菜单：光标处弹出，点击外部 / Esc / 再次右键关闭。
// 危险项（danger）首次点击变为「再次点击确认」二次确认态，避免误删。
// 视口收拢：挂载后实测菜单宽高再定位（v0.1.2 修走查建议——此前 w=180 写死，
// 长标签会溢出右缘）。
import { onBeforeUnmount, onMounted, ref } from 'vue'
import type { Component } from 'vue'

export interface CtxItem {
  key: string
  label: string
  icon?: Component
  danger?: boolean   // 红色项；配合 confirm 启用二次确认
  confirm?: boolean  // 首次点击不派发，转为确认态
  disabled?: boolean
  // title 禁用原因等提示（#92）：绑定到按钮 title 属性，禁用项仍可悬停看原因。
  title?: string
}

const props = defineProps<{ x: number; y: number; items: CtxItem[] }>()
const emit = defineEmits<{ (e: 'select', key: string): void; (e: 'close'): void }>()

// 二次确认态：记录待确认的 danger 项 key（确认态下标签变为「再次点击确认」）。
const confirming = ref('')

const menuEl = ref<HTMLElement>()
const pos = ref({ left: '0px', top: '0px' })

// onMounted 实测菜单尺寸后贴边收拢：右/下溢出时向左/上偏移，最小留 8px 边距。
onMounted(() => {
  const el = menuEl.value
  if (!el) return
  const r = el.getBoundingClientRect()
  const left = Math.max(8, Math.min(props.x, window.innerWidth - r.width - 8))
  const top = Math.max(8, Math.min(props.y, window.innerHeight - r.height - 8))
  pos.value = { left: left + 'px', top: top + 'px' }
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
    <div ref="menuEl" class="ctx-menu" :style="pos" @click.stop>
      <button v-for="it in items" :key="it.key" class="ctx-item"
              :class="{ danger: it.danger, armed: confirming === it.key }"
              :disabled="it.disabled" :title="it.title" @click="onItem(it)">
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
