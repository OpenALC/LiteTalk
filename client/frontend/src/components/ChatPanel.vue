<script setup lang="ts">
// ChatPanel 聊天视图公共壳：头部（图标/标题/副标题/操作）+ 消息列表 + 输入发送。
// 文字频道（TextChat）与私聊/群聊（ConversationChat）共用，避免复制样式。
// 不做本地乐观回显——服务端会把消息广播回发送者，本地再插一条即双显。
import { computed, nextTick, ref, watch } from 'vue'
import { Send } from 'lucide-vue-next'
import { useChatStore } from '../stores/chat'

const props = withDefaults(defineProps<{
  channelId: string
  title: string
  subtitle?: string
  placeholder?: string
}>(), { subtitle: '', placeholder: '发送消息' })

const chat = useChatStore()
const text = ref('')

// 当前频道的消息列表（随频道切换与消息到达自动更新）。
const messages = computed(() => chat.messagesFor(props.channelId))

// ---- 滚动跟随：新消息到达时贴底；用户上翻查看历史时不抢滚动位置 ----
const listEl = ref<HTMLElement | null>(null)
let stick = true

function onScroll() {
  const el = listEl.value
  if (!el) return
  stick = el.scrollHeight - el.scrollTop - el.clientHeight < 40
}

async function scrollToBottom() {
  await nextTick()
  const el = listEl.value
  if (el) el.scrollTop = el.scrollHeight
}

watch(() => messages.value.length, () => {
  if (stick) scrollToBottom()
})

// 切换频道/会话时回到最新一条（历史补拉到达后由 length 变化再跟一次）。
watch(() => props.channelId, () => {
  stick = true
  scrollToBottom()
})

// fmtTime 把服务端毫秒时间戳格式化为 HH:MM（此前直接显示原始 epoch 数字）。
function fmtTime(ts: string): string {
  const n = Number(ts)
  if (!Number.isFinite(n) || n <= 0) return ''
  const d = new Date(n)
  const hh = String(d.getHours()).padStart(2, '0')
  const mm = String(d.getMinutes()).padStart(2, '0')
  return `${hh}:${mm}`
}

async function send() {
  const v = text.value.trim()
  if (!v || !props.channelId) return
  try {
    await chat.send(props.channelId, v)
    text.value = ''
  } catch { /* 发送失败静默，服务端 error 消息另行提示 */ }
}
</script>

<template>
  <div class="chat">
    <div class="chat-head">
      <span v-if="$slots.icon" class="chat-icon"><slot name="icon" /></span>
      <b>{{ title }}</b>
      <span v-if="subtitle" class="chat-sub">{{ subtitle }}</span>
      <div v-if="$slots.actions" class="head-actions">
        <slot name="actions" />
      </div>
    </div>

    <div ref="listEl" class="chat-list" @scroll="onScroll">
      <div v-for="m in messages" :key="m.id" class="msg">
        <div class="m-head">
          <span class="m-name">{{ m.nickname }}</span>
          <span class="m-time">{{ fmtTime(m.ts) }}</span>
        </div>
        <p class="m-text">{{ m.content }}</p>
      </div>
      <div v-if="channelId && !messages.length" class="chat-empty">还没有消息</div>
    </div>

    <div class="composer">
      <div class="c-input">
        <input v-model="text"
               :placeholder="placeholder"
               maxlength="2000"
               @keydown.enter.prevent="send">
      </div>
      <button class="send-btn" title="发送" @click="send"><Send :size="17" /></button>
    </div>
  </div>
</template>

<style scoped>
.chat {
  flex: 1; min-height: 0;
  display: flex; flex-direction: column;
  background: var(--brand-50);
}
.chat-head {
  flex: none; padding: 14px 22px 0;
  display: flex; align-items: center; gap: 8px;
  color: var(--ink-600);
}
.chat-icon { display: flex; align-items: center; }
.chat-icon :deep(svg) { color: var(--ink-400); }
.chat-head b { font-size: 15px; color: var(--ink-900); }
.chat-sub { font-size: 12px; color: var(--ink-400); }
.head-actions { margin-left: auto; display: flex; gap: 2px; }
.chat-list {
  flex: 1; overflow-y: auto;
  padding: 14px 22px;
  display: flex; flex-direction: column; gap: 14px;
}
.msg .m-head {
  display: flex; align-items: baseline; gap: 8px;
  margin-bottom: 3px;
}
.msg .m-name { font-size: 13.5px; font-weight: 600; }
.msg .m-time { font-size: 11px; color: var(--ink-300); }
.msg .m-text {
  font-size: 14px; color: var(--ink-600);
  line-height: 1.65; text-wrap: pretty;
}
.chat-empty {
  margin: auto; font-size: 12.5px; color: var(--ink-300);
}
.composer { flex: none; padding: 12px 22px 18px; display: flex; gap: 10px; }
.composer .c-input {
  flex: 1; display: flex; align-items: center;
  background: var(--surface);
  border: 1.5px solid var(--brand-200); border-radius: 12px;
  transition: border-color 0.2s, box-shadow 0.2s;
}
.composer .c-input:focus-within {
  border-color: var(--brand-500);
  box-shadow: 0 0 0 3px rgba(79, 164, 243, 0.15);
}
.composer input {
  flex: 1; height: 42px; padding: 0 14px;
  border: none; outline: none; font-size: 14px;
  background: transparent; color: var(--ink-900);
}
.composer input::placeholder { color: var(--ink-300); }
.send-btn {
  width: 42px; height: 42px; flex: none;
  border-radius: 12px; background: var(--brand-500); color: #fff;
  display: flex; align-items: center; justify-content: center;
  transition: background 0.2s, transform 0.1s var(--ease);
  box-shadow: 0 4px 12px rgba(79, 164, 243, 0.3);
}
.send-btn:hover { background: var(--brand-600); }
.send-btn:active { transform: scale(0.93); }
</style>
