<script setup lang="ts">
// SearchDialog 全局消息搜索面板（#42 B2）：Ctrl+K 打开，输入 debounce 后走
// search store；↑↓ 选择、Enter / 双击打开、Esc / 点遮罩关闭。
// 结果行展示发送者、时间、频道标签与正文摘要（查询词按文本切片高亮，禁止 v-html）。
import { computed, nextTick, onBeforeUnmount, onMounted, ref, watch } from 'vue'
import { Loader, Paperclip, Search, X } from 'lucide-vue-next'
import type { ChatMessage } from '../api/types'
import { useConversationsStore } from '../stores/conversations'
import { useServersStore } from '../stores/data'
import { useSearchStore } from '../stores/search'
import { channelLabel, excerptSegments, formatSearchTime, type HighlightSegment } from '../utils/search'

const emit = defineEmits<{
  (e: 'select', message: ChatMessage): void
  (e: 'close'): void
}>()

const search = useSearchStore()
const conversations = useConversationsStore()
const servers = useServersStore()

// 输入与 store 的 query 解耦：store 只在请求真正发出后更新，高亮则固定用
// resultQuery（产生当前结果的那次查询），避免继续敲字时旧结果的命中位置漂移。
const input = ref(search.query)
const inputEl = ref<HTMLInputElement | null>(null)
const listEl = ref<HTMLElement | null>(null)
const active = ref(0)

// 频道标签解析源：会话用 displayName（私聊显示对方昵称、群聊显示群名）。
const labelSource = computed(() => ({
  conversations: conversations.list.map((c) => ({ id: c.id, name: conversations.displayName(c) })),
  servers: servers.servers.map((s) => ({
    name: s.name,
    channels: s.channels.map((c) => ({ id: c.id, name: c.name })),
  })),
}))

function labelFor(channelId: string): string {
  return channelLabel(channelId, labelSource.value)
}

// segmentsFor 结果行正文：墓碑/纯附件消息给简短说明，其余截断并高亮查询词。
function segmentsFor(m: ChatMessage): HighlightSegment[] {
  if (m.deleted_at) return [{ text: '消息已删除', hit: false }]
  if (!m.content) {
    return [{ text: m.attachments?.length ? '附件' : '（无正文）', hit: false }]
  }
  return excerptSegments(m.content, search.resultQuery)
}

// debounce 250–300ms：连续输入只发最后一次请求；清空立即清结果。
const DEBOUNCE_MS = 280
let debounceTimer: ReturnType<typeof setTimeout> | null = null

function clearDebounce() {
  if (debounceTimer) {
    clearTimeout(debounceTimer)
    debounceTimer = null
  }
}

watch(input, (v) => {
  clearDebounce()
  active.value = 0
  const q = v.trim()
  if (!q) {
    search.searchNow('')
    return
  }
  debounceTimer = setTimeout(() => {
    debounceTimer = null
    search.searchNow(q, 20).catch(() => {})
  }, DEBOUNCE_MS)
})

// 新结果到达时回到第一项，否则 active 可能指向已不存在的行。
watch(() => search.results, () => {
  active.value = 0
  nextTick(scrollActiveIntoView)
})

function move(delta: number) {
  const n = search.results.length
  if (!n) return
  active.value = (active.value + delta + n) % n
  nextTick(scrollActiveIntoView)
}

function scrollActiveIntoView() {
  const el = listEl.value?.querySelector<HTMLElement>('.sd-item.on')
  el?.scrollIntoView({ block: 'nearest' })
}

function choose(m: ChatMessage | undefined) {
  if (!m) return
  emit('select', m)
}

function close() {
  emit('close')
}

function onKeydown(e: KeyboardEvent) {
  if (e.isComposing) return
  if (e.key === 'Escape') {
    e.preventDefault()
    close()
    return
  }
  if (e.key === 'ArrowDown') {
    e.preventDefault()
    move(1)
    return
  }
  if (e.key === 'ArrowUp') {
    e.preventDefault()
    move(-1)
    return
  }
  if (e.key === 'Enter') {
    e.preventDefault()
    choose(search.results[active.value])
  }
}

onMounted(() => {
  inputEl.value?.focus()
})

onBeforeUnmount(clearDebounce)
</script>

<template>
  <div class="sd-overlay" @click.self="close" @keydown="onKeydown">
    <div class="sd-card" role="dialog" aria-label="搜索消息">
      <div class="sd-head">
        <Search :size="17" class="sd-icon" />
        <input ref="inputEl" v-model="input" class="sd-input" type="text" maxlength="100"
               placeholder="搜索消息" autocomplete="off" autofocus />
        <Loader v-if="search.loading" :size="15" class="sd-spin" />
        <button class="sd-close" title="关闭" @click="close"><X :size="16" /></button>
      </div>

      <div class="sd-body">
        <div v-if="search.error" class="sd-state">{{ search.error }}</div>
        <div v-else-if="!input.trim()" class="sd-state">输入关键词搜索消息</div>
        <div v-else-if="!search.results.length" class="sd-state">
          {{ search.loading ? '搜索中…' : '没有匹配的消息' }}
        </div>
        <div v-else ref="listEl" class="sd-list">
          <button v-for="(m, i) in search.results" :key="m.id" type="button"
                  class="sd-item" :class="{ on: i === active }"
                  @click="active = i" @dblclick="choose(m)" @mouseenter="active = i">
            <div class="si-head">
              <span class="si-name">{{ m.nickname || '未知用户' }}</span>
              <span class="si-time">{{ formatSearchTime(m.ts) }}</span>
              <span class="si-chan">{{ labelFor(m.channel_id) }}</span>
            </div>
            <div class="si-body">
              <Paperclip v-if="m.attachments?.length" :size="12" class="si-clip" />
              <span class="si-text">
                <template v-for="(seg, j) in segmentsFor(m)" :key="j">
                  <mark v-if="seg.hit" class="si-hit">{{ seg.text }}</mark>
                  <template v-else>{{ seg.text }}</template>
                </template>
              </span>
            </div>
          </button>
        </div>
      </div>

      <div class="sd-foot">↑↓ 选择 · Enter 打开 · Esc 关闭</div>
    </div>
  </div>
</template>

<style scoped>
.sd-overlay {
  position: fixed; inset: 0; z-index: 70;
  background: rgba(23, 50, 75, 0.28);
  display: flex; align-items: center; justify-content: center;
}
.sd-card {
  width: 620px; max-width: calc(100vw - 48px);
  max-height: 72vh;
  display: flex; flex-direction: column;
  background: var(--surface); border-radius: 16px;
  box-shadow: var(--shadow-3);
  overflow: hidden;
  animation: sd-in 0.16s var(--ease);
}
@keyframes sd-in {
  from { opacity: 0; transform: translateY(-6px) scale(0.99); }
  to { opacity: 1; transform: none; }
}
.sd-head {
  flex: none;
  display: flex; align-items: center; gap: 8px;
  padding: 12px 14px;
  border-bottom: 1px solid var(--line);
}
.sd-icon { flex: none; color: var(--ink-400); }
.sd-input {
  flex: 1; min-width: 0; height: 34px; padding: 0 4px;
  border: none; outline: none; background: transparent;
  font-size: 14px; color: var(--ink-900);
}
.sd-input::placeholder { color: var(--ink-300); }
.sd-spin { flex: none; color: var(--brand-500); animation: sd-rotate 0.9s linear infinite; }
@keyframes sd-rotate {
  from { transform: rotate(0deg); }
  to { transform: rotate(360deg); }
}
.sd-close {
  flex: none; width: 28px; height: 28px;
  display: flex; align-items: center; justify-content: center;
  border-radius: 8px; color: var(--ink-400);
  transition: background 0.15s, color 0.15s;
}
.sd-close:hover { background: var(--brand-50); color: var(--ink-900); }
.sd-body { flex: 1; min-height: 0; display: flex; flex-direction: column; }
.sd-state {
  flex: 1; min-height: 120px;
  display: flex; align-items: center; justify-content: center;
  font-size: 12.5px; color: var(--ink-300);
  padding: 20px;
}
.sd-list {
  flex: 1; min-height: 0; overflow-y: auto;
  padding: 6px; display: flex; flex-direction: column; gap: 2px;
}
.sd-item {
  display: flex; flex-direction: column; gap: 4px;
  padding: 9px 11px; border-radius: 10px;
  text-align: left;
  transition: background 0.15s;
}
.sd-item.on { background: var(--brand-50); }
.si-head { display: flex; align-items: baseline; gap: 8px; min-width: 0; }
.si-name {
  flex: none; font-size: 13px; font-weight: 600; color: var(--ink-900);
  max-width: 40%; overflow: hidden; text-overflow: ellipsis; white-space: nowrap;
}
.si-time { flex: none; font-size: 11px; color: var(--ink-300); }
.si-chan {
  margin-left: auto; flex: none; max-width: 45%;
  padding: 1px 7px; border-radius: 999px;
  background: rgba(79, 164, 243, 0.1);
  font-size: 11px; color: var(--brand-700);
  overflow: hidden; text-overflow: ellipsis; white-space: nowrap;
}
.si-body { display: flex; align-items: center; gap: 5px; min-width: 0; }
.si-clip { flex: none; color: var(--ink-300); }
.si-text {
  font-size: 13px; line-height: 1.55; color: var(--ink-600);
  overflow-wrap: anywhere;
  display: -webkit-box; -webkit-line-clamp: 2; -webkit-box-orient: vertical;
  overflow: hidden;
}
.si-hit {
  background: rgba(79, 164, 243, 0.2);
  color: var(--brand-700);
  border-radius: 3px;
  padding: 0 1px;
}
.sd-foot {
  flex: none; padding: 8px 16px 10px;
  border-top: 1px solid var(--line);
  font-size: 11px; color: var(--ink-300);
  text-align: right;
}
</style>
