<script setup lang="ts">
// ConversationChat 私聊/群聊舞台：头部身份（昵称/群名 + 私聊/N 人）与成员管理入口，
// 消息列表与输入框复用 ChatPanel 的视觉。不做本地乐观回显，发送走 chat.send
//（会话 ID 即频道 ID，服务端把消息广播回全部成员）。
// 打开会话时补拉最近历史；失败静默，不阻断实时消息。
import { computed, ref, watch } from 'vue'
import { MessageCircle, UserCog, Users } from 'lucide-vue-next'
import ChatPanel from './ChatPanel.vue'
import GroupMembersDialog from './GroupMembersDialog.vue'
import { useChatStore } from '../stores/chat'
import { useConversationsStore } from '../stores/conversations'

const conversations = useConversationsStore()
const chat = useChatStore()
const conv = computed(() => conversations.current)
const title = computed(() => conversations.displayName(conv.value))
const subtitle = computed(() => {
  const c = conv.value
  if (!c) return ''
  return c.kind === 'direct' ? '私聊' : `${c.members.length} 人`
})
const showMembers = ref(false)

watch(() => conv.value?.id, (id, prev) => {
  // 切换会话时关掉上一个会话的成员弹层，避免弹层内容跟着变成新会话。
  if (prev && id !== prev) showMembers.value = false
  if (id) chat.loadHistory(id).catch(() => {})
}, { immediate: true })
</script>

<template>
  <ChatPanel v-if="conv" :channel-id="conv.id" :title="title" :subtitle="subtitle"
             :placeholder="conv.kind === 'direct' ? '发送私信' : '发送消息'">
    <template #icon>
      <MessageCircle v-if="conv.kind === 'direct'" :size="17" />
      <Users v-else :size="17" />
    </template>
    <template v-if="conv.kind === 'group'" #actions>
      <button class="icon-btn" title="成员管理" @click="showMembers = true">
        <UserCog :size="17" />
      </button>
    </template>
  </ChatPanel>

  <div v-else class="dm-empty">
    <MessageCircle :size="28" />
    <p>从左侧选择会话，或从好友列表发起私信</p>
  </div>

  <GroupMembersDialog v-if="showMembers && conv" :conversation-id="conv.id"
                      @close="showMembers = false" />
</template>

<style scoped>
.icon-btn {
  width: 32px; height: 32px;
  display: flex; align-items: center; justify-content: center;
  border-radius: 8px; color: var(--ink-600);
  transition: background 0.15s, color 0.15s;
}
.icon-btn:hover, .icon-btn.on {
  background: rgba(79, 164, 243, 0.12); color: var(--brand-700);
}
.dm-empty {
  flex: 1; min-height: 0;
  display: flex; flex-direction: column;
  align-items: center; justify-content: center; gap: 10px;
  color: var(--ink-300);
}
.dm-empty p { font-size: 12.5px; }
</style>
