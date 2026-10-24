<script setup lang="ts">
// ChatPanel 聊天视图公共壳：头部（图标/标题/副标题/操作）+ 消息列表 + 输入发送。
// 文字频道（TextChat）与私聊/群聊（ConversationChat）共用，避免复制样式。
// 不做本地乐观回显——服务端会把消息广播回发送者，本地再插一条即双显。
// #39：右键菜单提供回复/编辑/删除；输入区支持回复条与 @提及候选。
import { computed, nextTick, onBeforeUnmount, onMounted, ref, watch } from 'vue'
import { CornerUpLeft, Pencil, Reply, Send, Trash2, X } from 'lucide-vue-next'
import { goApp } from '../api/bridge'
import type { ChatMessage, MentionCandidate } from '../api/types'
import { useAuthStore } from '../stores/auth'
import { isMessageDeleted, useChatStore } from '../stores/chat'
import { useNotifyStore } from '../stores/notify'
import ContextMenu, { type CtxItem } from './ContextMenu.vue'

const props = withDefaults(defineProps<{
  channelId: string
  title: string
  subtitle?: string
  placeholder?: string
}>(), { subtitle: '', placeholder: '发送消息' })

const chat = useChatStore()
const auth = useAuthStore()
const notify = useNotifyStore()
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
// 回复目标、编辑态与提及候选都属于上一个频道，必须一起清掉。
watch(() => props.channelId, () => {
  stick = true
  scrollToBottom()
  editingId.value = ''
  editText.value = ''
  replyToId.value = ''
  pendingMentions.clear()
  closeMention()
  menu.value = null
})

// 挂载时桶里可能已有消息（切走再切回）：直接定位到最新一条。
onMounted(() => {
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

// isOwn 只有作者能编辑/删除（管理入口暂不暴露：前端没有 ManageServer 信息）。
function isOwn(m: ChatMessage): boolean {
  return !!auth.user?.id && m.user_id === auth.user.id
}

// replyText 回复条里的被引用内容：软删/空正文统一显示「消息已删除」。
function replyText(m: ChatMessage): string {
  const p = m.reply_preview
  if (!p) return '原消息不可用'
  return p.deleted || p.content === '' ? '消息已删除' : excerpt(p.content)
}

// excerpt 截断摘要到一行（回复条/回复状态条共用）。
function excerpt(s: string): string {
  const t = s.replace(/\s+/g, ' ').trim()
  return t.length > 60 ? t.slice(0, 60) + '…' : t
}

// segments 把正文按 @昵称 切片做轻量高亮；不使用 v-html。
// 昵称限定为字母/数字/下划线/连字符，中文昵称也在 \p{L} 内。
function segments(content: string): { text: string; mention: boolean }[] {
  return content
    .split(/(@[\p{L}\p{N}_-]+)/u)
    .filter(Boolean)
    .map((t) => ({ text: t, mention: t.startsWith('@') }))
}

// ---- 右键菜单：回复（所有未删除消息）/ 编辑、删除（仅自己，删除二次确认）----
const menu = ref<{ x: number; y: number; message: ChatMessage } | null>(null)

function openMenu(e: MouseEvent, m: ChatMessage) {
  if (isMessageDeleted(m)) return
  menu.value = { x: e.clientX, y: e.clientY, message: m }
}

const menuItems = computed<CtxItem[]>(() => {
  const m = menu.value?.message
  if (!m) return []
  const items: CtxItem[] = [{ key: 'reply', label: '回复', icon: Reply }]
  if (isOwn(m)) {
    items.push({ key: 'edit', label: '编辑', icon: Pencil })
    items.push({ key: 'delete', label: '删除', icon: Trash2, danger: true, confirm: true })
  }
  return items
})

function onMenuSelect(key: string) {
  const m = menu.value?.message
  menu.value = null
  if (!m) return
  if (key === 'reply') startReply(m)
  else if (key === 'edit') startEdit(m)
  else if (key === 'delete') removeMessage(m).catch(() => {})
}

// ---- 回复：输入区上方显示引用条，发送时带 reply_to ----
const replyToId = ref('')
const replyMsg = computed(() => (replyToId.value ? chat.messageById(props.channelId, replyToId.value) : undefined))
const composerReplyText = computed(() => {
  const m = replyMsg.value
  if (!m) return '回复的消息不可用'
  return isMessageDeleted(m) ? '消息已删除' : excerpt(m.content)
})

function startReply(m: ChatMessage) {
  if (isMessageDeleted(m)) return
  replyToId.value = m.id
  closeMention()
  nextTick(() => inputEl.value?.focus())
}

function cancelReply() {
  replyToId.value = ''
  nextTick(() => inputEl.value?.focus())
}

// ---- 回复跳转：当前桶找不到时经 GetMessage 回填，再滚动并短暂高亮 ----
const flashId = ref('')
let flashTimer: ReturnType<typeof setTimeout> | null = null

async function jumpToReply(m: ChatMessage) {
  const targetId = m.reply_to || m.reply_preview?.id || ''
  if (!targetId) return
  // 回填历史消息会改变列表长度；先解除贴底跟随，否则长度 watch 会把视图
  // 又拉回最新一条，跳转等于没发生。
  stick = false
  if (!chat.messageById(props.channelId, targetId)) {
    try {
      const fetched = await goApp().GetMessage(props.channelId, targetId)
      chat.mergeMessage(props.channelId, fetched)
    } catch {
      return // 目标不可读（被清理/无权限）：静默，不打断阅读
    }
  }
  await nextTick()
  const el = listEl.value?.querySelector<HTMLElement>(`#msg-${targetId}`)
  if (!el) return
  el.scrollIntoView({ block: 'center' })
  onScroll() // 跳转后按实际位置重算是否贴底
  flashId.value = targetId
  if (flashTimer) clearTimeout(flashTimer)
  flashTimer = setTimeout(() => { flashId.value = '' }, 1600)
}

onBeforeUnmount(() => {
  if (flashTimer) clearTimeout(flashTimer)
})

// ---- 编辑：正文替换为多行编辑框；提及集合保持原样（服务端整体替换）----
const editingId = ref('')
const editText = ref('')
const savingEdit = ref(false)

function startEdit(m: ChatMessage) {
  if (!isOwn(m) || isMessageDeleted(m)) return
  editingId.value = m.id
  editText.value = m.content
  nextTick(() => {
    const el = listEl.value?.querySelector<HTMLTextAreaElement>('.msg-edit-input')
    el?.focus()
    el?.setSelectionRange(el.value.length, el.value.length)
  })
}

function cancelEdit() {
  editingId.value = ''
  editText.value = ''
}

async function saveEdit(m: ChatMessage) {
  const v = editText.value.trim()
  if (!v || savingEdit.value) return
  savingEdit.value = true
  try {
    await chat.editMessage(props.channelId, m.id, v, m.mentions ?? [])
    editingId.value = ''
    editText.value = ''
  } catch (e) {
    notify.fromErr(e, '消息编辑失败')
  } finally {
    savingEdit.value = false
  }
}

// Enter 保存、Shift+Enter 换行、Esc 取消；输入法组合中的 Enter 不提交。
function onEditKeydown(e: KeyboardEvent, m: ChatMessage) {
  if (e.isComposing) return
  if (e.key === 'Enter' && !e.shiftKey) {
    e.preventDefault()
    saveEdit(m).catch(() => {})
  } else if (e.key === 'Escape') {
    e.preventDefault()
    cancelEdit()
  }
}

// ---- 删除：ContextMenu 的 danger+confirm 已做二次确认 ----
async function removeMessage(m: ChatMessage) {
  if (!isOwn(m) || isMessageDeleted(m)) return
  try {
    await chat.deleteMessage(props.channelId, m.id)
    if (editingId.value === m.id) cancelEdit()
  } catch (e) {
    notify.fromErr(e, '消息删除失败')
  }
}

// ---- @提及：光标前的 @片段触发候选下拉；选中后记录 user_id，发送时按正文过滤 ----
const inputEl = ref<HTMLInputElement | null>(null)
const mentionOpen = ref(false)
const mentionQuery = ref('')
const mentionStart = ref(0)
const mentionIndex = ref(0)
// pendingMentions user_id → 昵称：只在选中候选时登记，发送时仅带仍出现在正文里的。
const pendingMentions = new Map<string, string>()

const filteredCandidates = computed<MentionCandidate[]>(() => {
  const me = auth.user?.id
  const q = mentionQuery.value.trim().toLowerCase()
  return chat.mentionCandidatesFor(props.channelId)
    .filter((c) => c.user_id !== me) // @自己无意义
    .filter((c) => !q || `${c.nickname}#${c.tag}`.toLowerCase().includes(q))
    .slice(0, 8)
})

function closeMention() {
  mentionOpen.value = false
  mentionQuery.value = ''
  mentionIndex.value = 0
}

function onComposerInput() {
  const el = inputEl.value
  if (!el) return
  const pos = el.selectionStart ?? el.value.length
  const before = el.value.slice(0, pos)
  const at = before.lastIndexOf('@')
  // @ 必须在行首或空白之后，且光标到 @ 之间不能有空白/换行。
  if (at < 0 || (at > 0 && !/\s/.test(before[at - 1]))) {
    closeMention()
    return
  }
  const q = before.slice(at + 1)
  if (/\s/.test(q)) {
    closeMention()
    return
  }
  mentionStart.value = at
  mentionQuery.value = q
  mentionIndex.value = 0
  mentionOpen.value = true
  // 候选为空时才加载（缓存命中/在飞请求直接复用）；失败静默，下次输入再试。
  if (!chat.mentionCandidatesFor(props.channelId).length) {
    chat.loadMentionCandidates(props.channelId).catch(() => {})
  }
}

function chooseMention(c: MentionCandidate) {
  const el = inputEl.value
  if (!el) return
  const caret = el.selectionStart ?? mentionStart.value + 1 + mentionQuery.value.length
  const start = mentionStart.value
  if (text.value[start] !== '@') {
    closeMention()
    return
  }
  const next = text.value.slice(0, start) + '@' + c.nickname + ' ' + text.value.slice(caret)
  text.value = next
  pendingMentions.set(c.user_id, c.nickname)
  closeMention()
  const pos = start + c.nickname.length + 2
  nextTick(() => {
    el.focus()
    el.setSelectionRange(pos, pos)
  })
}

// 下拉打开时方向键/Enter/Tab/Esc 归候选列表，否则 Enter 发送。
function onComposerKeydown(e: KeyboardEvent) {
  if (e.isComposing) return
  const list = filteredCandidates.value
  if (mentionOpen.value && list.length) {
    if (e.key === 'ArrowDown') {
      e.preventDefault()
      mentionIndex.value = (mentionIndex.value + 1) % list.length
      return
    }
    if (e.key === 'ArrowUp') {
      e.preventDefault()
      mentionIndex.value = (mentionIndex.value - 1 + list.length) % list.length
      return
    }
    if (e.key === 'Enter' || e.key === 'Tab') {
      e.preventDefault()
      chooseMention(list[mentionIndex.value])
      return
    }
    if (e.key === 'Escape') {
      e.preventDefault()
      closeMention()
      return
    }
  }
  if (e.key === 'Enter') {
    e.preventDefault()
    send()
  }
}

async function send() {
  const v = text.value.trim()
  if (!v || !props.channelId) return
  // 只带仍在正文中出现的提及（选中后又删掉的不算）。
  const mentions = [...pendingMentions.entries()]
    .filter(([, nickname]) => v.includes('@' + nickname))
    .map(([userId]) => userId)
  try {
    await chat.sendWithMeta(props.channelId, v, replyToId.value, mentions)
    text.value = ''
    replyToId.value = ''
    pendingMentions.clear()
    closeMention()
  } catch {
    // 发送失败保留输入/回复/提及，服务端 error 事件另行提示。
  }
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
      <div v-for="m in messages" :key="m.id" :id="`msg-${m.id}`" class="msg"
           :class="{ flash: flashId === m.id, deleted: isMessageDeleted(m) }"
           @contextmenu.prevent="openMenu($event, m)">
        <!-- 回复预览条：点击跳到原消息；软删目标显示「消息已删除」 -->
        <button v-if="m.reply_to || m.reply_preview" class="m-reply" @click="jumpToReply(m)">
          <CornerUpLeft :size="12" />
          <span class="m-reply-name">{{ m.reply_preview?.nickname || '未知用户' }}</span>
          <span class="m-reply-text">{{ replyText(m) }}</span>
        </button>

        <div class="m-head">
          <span class="m-name">{{ m.nickname }}</span>
          <span class="m-time">{{ fmtTime(m.ts) }}</span>
          <span v-if="m.edited_at && !isMessageDeleted(m)" class="m-edited">已编辑</span>
        </div>

        <div v-if="editingId === m.id" class="msg-edit">
          <textarea v-model="editText" class="msg-edit-input" maxlength="2000" rows="2"
                    @keydown="onEditKeydown($event, m)"></textarea>
          <div class="msg-edit-acts">
            <button class="me-cancel" @click="cancelEdit">取消</button>
            <button class="me-save" :disabled="savingEdit || !editText.trim()"
                    @click="saveEdit(m)">保存</button>
          </div>
        </div>
        <p v-else-if="isMessageDeleted(m)" class="m-text m-deleted">消息已删除</p>
        <p v-else class="m-text">
          <template v-for="(seg, i) in segments(m.content)" :key="i">
            <span v-if="seg.mention" class="m-mention">{{ seg.text }}</span>
            <template v-else>{{ seg.text }}</template>
          </template>
        </p>
      </div>
      <div v-if="channelId && !messages.length" class="chat-empty">还没有消息</div>
    </div>

    <div class="composer">
      <div v-if="replyToId" class="reply-strip">
        <CornerUpLeft :size="13" class="rs-icon" />
        <span class="rs-label">回复</span>
        <span class="rs-name">{{ replyMsg?.nickname || '原消息' }}</span>
        <span class="rs-text">{{ composerReplyText }}</span>
        <button class="rs-close" title="取消回复" @click="cancelReply"><X :size="14" /></button>
      </div>
      <div class="c-row">
        <div class="c-input">
          <input ref="inputEl"
                 v-model="text"
                 :placeholder="placeholder"
                 maxlength="2000"
                 @input="onComposerInput"
                 @keydown="onComposerKeydown"
                 @blur="closeMention">
          <!-- 候选下拉：没有候选/加载失败时不渲染，输入不受阻塞 -->
          <div v-if="mentionOpen && filteredCandidates.length" class="mention-pop">
            <button v-for="(c, i) in filteredCandidates" :key="c.user_id"
                    class="mention-item" :class="{ on: i === mentionIndex }"
                    @mousedown.prevent="chooseMention(c)">
              <span class="mi-name">{{ c.nickname }}</span>
              <span class="mi-tag">#{{ c.tag }}</span>
            </button>
          </div>
        </div>
        <button class="send-btn" title="发送" @click="send"><Send :size="17" /></button>
      </div>
    </div>

    <ContextMenu v-if="menu" :x="menu.x" :y="menu.y" :items="menuItems"
                 @select="onMenuSelect" @close="menu = null" />
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
.msg { position: relative; }
/* 回复跳转高亮：只动 opacity（规范约束），不触发重排 */
.msg.flash::after {
  content: '';
  position: absolute; inset: -4px -8px;
  border-radius: 10px;
  background: rgba(79, 164, 243, 0.18);
  pointer-events: none;
  animation: msg-flash 1.6s var(--ease);
}
@keyframes msg-flash {
  0% { opacity: 1; }
  100% { opacity: 0; }
}
.msg .m-head {
  display: flex; align-items: baseline; gap: 8px;
  margin-bottom: 3px;
}
.msg .m-name { font-size: 13.5px; font-weight: 600; }
.msg .m-time { font-size: 11px; color: var(--ink-300); }
.msg .m-edited { font-size: 11px; color: var(--ink-300); }
.msg .m-text {
  font-size: 14px; color: var(--ink-600);
  line-height: 1.65; text-wrap: pretty;
  white-space: pre-wrap; overflow-wrap: anywhere;
}
.msg .m-text.m-deleted { color: var(--ink-300); }
.msg.deleted .m-name { color: var(--ink-400); }
.m-mention { color: var(--brand-700); font-weight: 500; }
/* 回复预览条：细边 + 浅底，点击跳转 */
.m-reply {
  display: flex; align-items: center; gap: 5px;
  max-width: 100%; margin-bottom: 4px;
  padding: 2px 8px 2px 7px;
  border-left: 2px solid var(--brand-200);
  border-radius: 0 6px 6px 0;
  background: rgba(79, 164, 243, 0.07);
  font-size: 12px; color: var(--ink-400);
  text-align: left;
  transition: background 0.15s;
}
.m-reply:hover { background: rgba(79, 164, 243, 0.13); }
.m-reply :deep(svg) { flex: none; color: var(--ink-300); }
.m-reply-name { flex: none; font-weight: 600; color: var(--brand-700); }
.m-reply-text { overflow: hidden; text-overflow: ellipsis; white-space: nowrap; }
/* 编辑框：沿用主题边框与聚焦环 */
.msg-edit { display: flex; flex-direction: column; gap: 8px; }
.msg-edit-input {
  width: 100%; min-height: 64px; max-height: 200px; resize: vertical;
  padding: 8px 10px;
  background: var(--surface);
  border: 1.5px solid var(--brand-200); border-radius: 10px;
  font: inherit; font-size: 14px; line-height: 1.6; color: var(--ink-900);
  outline: none;
  transition: border-color 0.2s, box-shadow 0.2s;
}
.msg-edit-input:focus {
  border-color: var(--brand-500);
  box-shadow: 0 0 0 3px rgba(79, 164, 243, 0.15);
}
.msg-edit-acts { display: flex; justify-content: flex-end; gap: 8px; }
.msg-edit-acts button {
  height: 30px; padding: 0 12px; border-radius: 8px;
  font-size: 12.5px; font-weight: 500;
  transition: background 0.15s, color 0.15s;
}
.me-cancel { color: var(--ink-400); }
.me-cancel:hover { background: var(--brand-50); color: var(--ink-600); }
.me-save { background: var(--brand-500); color: #fff; font-weight: 600; }
.me-save:hover:not(:disabled) { background: var(--brand-600); }
.me-save:disabled { opacity: 0.5; cursor: default; }
.chat-empty {
  margin: auto; font-size: 12.5px; color: var(--ink-300);
}
.composer {
  flex: none; padding: 12px 22px 18px;
  display: flex; flex-direction: column; gap: 8px;
}
.reply-strip {
  display: flex; align-items: center; gap: 6px; min-width: 0;
  padding: 6px 10px; border-radius: 9px;
  background: rgba(79, 164, 243, 0.08);
  font-size: 12px; color: var(--ink-400);
}
.rs-icon { flex: none; color: var(--brand-600); }
.rs-label { flex: none; color: var(--ink-400); }
.rs-name { flex: none; font-weight: 600; color: var(--brand-700); }
.rs-text { overflow: hidden; text-overflow: ellipsis; white-space: nowrap; }
.rs-close {
  margin-left: auto; flex: none;
  width: 22px; height: 22px;
  display: flex; align-items: center; justify-content: center;
  border-radius: 6px; color: var(--ink-400);
  transition: background 0.15s, color 0.15s;
}
.rs-close:hover { background: rgba(79, 164, 243, 0.12); color: var(--ink-900); }
.c-row { display: flex; gap: 10px; }
.composer .c-input {
  position: relative;
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
/* @提及候选：输入框上方弹出，高度受限可滚动 */
.mention-pop {
  position: absolute; left: 0; bottom: calc(100% + 8px); z-index: 20;
  width: min(280px, 100%); max-height: 220px; overflow-y: auto;
  padding: 6px;
  background: var(--surface);
  border: 1px solid var(--line); border-radius: 12px;
  box-shadow: var(--shadow-2);
  display: flex; flex-direction: column; gap: 1px;
}
.mention-item {
  display: flex; align-items: baseline; gap: 6px;
  padding: 7px 9px; border-radius: 8px;
  font-size: 13px; color: var(--ink-600); text-align: left;
  transition: background 0.15s, color 0.15s;
}
.mention-item.on, .mention-item:hover { background: var(--brand-50); color: var(--ink-900); }
.mi-name { font-weight: 500; }
.mi-tag { font-size: 11.5px; color: var(--ink-300); }
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
