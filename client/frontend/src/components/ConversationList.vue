<script setup lang="ts">
// ConversationList 私信栏（#38 B4b）：私聊/群聊会话列表 + 新建群聊。
// 会话 ID 即频道 ID；点击选中后舞台渲染 ConversationChat。
// 右键菜单按会话类型与身份分流：direct 删除；group owner 改名/管理成员/解散；
// group 普通成员退出。危险操作由 ContextMenu 二次确认。
import { computed, ref } from 'vue'
import {
  LogOut, MessageCircle, Pencil, Plus, Trash2, UserCog, Users,
} from 'lucide-vue-next'
import { useConversationsStore } from '../stores/conversations'
import { useAuthStore } from '../stores/auth'
import { useFriendsStore } from '../stores/data'
import { useNotifyStore } from '../stores/notify'
import { badgeText, unreadLabel, useUnreadsStore } from '../stores/unreads'
import type { Conversation } from '../api/types'
import ContextMenu, { type CtxItem } from './ContextMenu.vue'
import GroupCreateDialog from './GroupCreateDialog.vue'
import GroupMembersDialog from './GroupMembersDialog.vue'
import PromptDialog from './PromptDialog.vue'

const conversations = useConversationsStore()
const auth = useAuthStore()
const friends = useFriendsStore()
const unreads = useUnreadsStore()
const notify = useNotifyStore()

const showCreate = ref(false)
const membersId = ref('')
const rename = ref<{ id: string; initial: string } | null>(null)

// subtitle 副标题：direct 用好友在线的粗略状态；group 显示人数与在线好友数。
function subtitle(c: Conversation): string {
  const sid = auth.user?.id ?? ''
  if (c.kind === 'direct') {
    const other = c.members.find((m) => m.user_id !== sid)
    if (!other) return '私聊'
    return friends.online.some((f) => f.userId === other.user_id) ? '在线' : '离线'
  }
  const n = c.members.length
  const on = conversations.onlineCount(c)
  return on > 0 ? `${n} 人 · ${on} 在线` : `${n} 人`
}

async function open(c: Conversation) {
  try {
    await conversations.open(c.id)
  } catch (e) {
    notify.fromErr(e, '打开会话失败')
  }
}

// ---- 右键菜单 ----
const ctx = ref<{ x: number; y: number; conv: Conversation } | null>(null)

const ctxItems = computed<CtxItem[]>(() => {
  const c = ctx.value?.conv
  if (!c) return []
  if (c.kind === 'direct') {
    return [{ key: 'delete', label: '删除会话', icon: Trash2, danger: true, confirm: true }]
  }
  if (conversations.isOwner(c)) {
    return [
      { key: 'rename', label: '重命名', icon: Pencil },
      { key: 'members', label: '管理成员', icon: UserCog },
      { key: 'delete', label: '解散群聊', icon: Trash2, danger: true, confirm: true },
    ]
  }
  return [{ key: 'leave', label: '退出群聊', icon: LogOut, danger: true, confirm: true }]
})

function openCtx(e: MouseEvent, c: Conversation) {
  ctx.value = { x: e.clientX, y: e.clientY, conv: c }
}

async function onCtxSelect(key: string) {
  const c = ctx.value?.conv
  if (!c) return
  switch (key) {
    case 'rename':
      rename.value = { id: c.id, initial: c.name }
      return
    case 'members':
      membersId.value = c.id
      return
    case 'delete':
      try {
        await conversations.delete(c.id)
        notify.show(c.kind === 'group' ? '群聊已解散' : '会话已删除')
      } catch (e) {
        notify.fromErr(e)
      }
      return
    case 'leave':
      try {
        await conversations.leave(c.id)
        notify.show('已退出群聊')
      } catch (e) {
        notify.fromErr(e)
      }
      return
  }
}

async function onRenameConfirm(value: string) {
  const r = rename.value
  rename.value = null
  if (!r) return
  try {
    await conversations.rename(r.id, value)
    notify.show('群名已更新')
  } catch (e) {
    notify.fromErr(e)
  }
}
</script>

<template>
  <aside class="convs">
    <div class="cv-head">
      <b>私信</b>
      <button class="icon-btn" title="新建群聊" @click="showCreate = true">
        <Plus :size="17" />
      </button>
    </div>

    <div class="cv-scroll">
      <button v-for="c in conversations.list" :key="c.id" class="cv-row"
              :class="{ on: c.id === conversations.currentId }"
              @click="open(c)" @contextmenu.prevent="openCtx($event, c)">
        <span class="cv-icon">
          <MessageCircle v-if="c.kind === 'direct'" :size="16" />
          <Users v-else :size="16" />
        </span>
        <span class="cv-info">
          <span class="cv-name" :class="{ unread: unreads.channel(c.id).unread > 0 }">
            {{ conversations.displayName(c) }}
          </span>
          <span class="cv-sub">{{ subtitle(c) }}</span>
        </span>
        <!-- 未读徽标（#40）：提及数优先，否则未读圆点 -->
        <span v-if="unreads.channel(c.id).mentions > 0" class="cv-badge"
              :title="unreadLabel(unreads.channel(c.id))">{{ badgeText(unreads.channel(c.id).mentions) }}</span>
        <span v-else-if="unreads.channel(c.id).unread > 0" class="cv-dot"
              :title="unreadLabel(unreads.channel(c.id))" />
      </button>

      <div v-if="!conversations.list.length" class="cv-empty">
        还没有私信，从好友列表发起
      </div>
    </div>

    <GroupCreateDialog v-if="showCreate" @close="showCreate = false" />
    <GroupMembersDialog v-if="membersId" :conversation-id="membersId"
                        @close="membersId = ''" />
    <PromptDialog v-if="rename" title="重命名群聊" :initial="rename.initial"
                  placeholder="群名" cta="保存" :maxlen="64"
                  @confirm="onRenameConfirm" @close="rename = null" />

    <ContextMenu v-if="ctx" :x="ctx.x" :y="ctx.y" :items="ctxItems"
                 @select="onCtxSelect" @close="ctx = null" />
  </aside>
</template>

<style scoped>
.convs {
  width: 248px; flex: none;
  display: flex; flex-direction: column;
  background: var(--surface);
  border-right: 1px solid var(--line);
  min-height: 0;
}
.cv-head {
  height: 52px; flex: none;
  display: flex; align-items: center; justify-content: space-between;
  padding: 0 16px; border-bottom: 1px solid var(--line);
}
.cv-head b { font-size: 15px; font-weight: 700; letter-spacing: 0.2px; }
.icon-btn {
  width: 32px; height: 32px;
  display: flex; align-items: center; justify-content: center;
  border-radius: 8px; color: var(--ink-600);
  transition: background 0.15s, color 0.15s;
}
.icon-btn:hover { background: rgba(79, 164, 243, 0.12); color: var(--brand-700); }
.cv-scroll { flex: 1; overflow-y: auto; padding: 10px 10px 12px; }
.cv-row {
  width: 100%;
  display: flex; align-items: center; gap: 9px;
  padding: 8px 9px; margin: 1px 0; border-radius: 9px;
  text-align: left; color: var(--ink-600);
  transition: background 0.15s, color 0.15s;
}
.cv-row:hover { background: var(--brand-50); color: var(--ink-900); }
.cv-row.on { background: rgba(79, 164, 243, 0.12); color: var(--brand-700); }
.cv-icon { flex: none; display: flex; align-items: center; color: var(--ink-400); }
.cv-row.on .cv-icon { color: var(--brand-600); }
.cv-info { flex: 1; min-width: 0; display: flex; flex-direction: column; }
.cv-name {
  font-size: 13.5px; font-weight: 600;
  white-space: nowrap; overflow: hidden; text-overflow: ellipsis;
}
.cv-name.unread { color: var(--ink-900); }
/* 未读徽标（#40）：提及数为主色胶囊白字，未读为小圆点 */
.cv-badge {
  flex: none;
  min-width: 16px; height: 16px; padding: 0 4px;
  border-radius: 8px;
  background: var(--brand-500); color: var(--surface);
  font-size: 10px; font-weight: 700; line-height: 16px;
  text-align: center;
}
.cv-dot {
  flex: none;
  width: 8px; height: 8px; border-radius: 50%;
  background: var(--brand-500);
}
.cv-sub { font-size: 11.5px; color: var(--ink-400); }
.cv-empty {
  padding: 36px 12px; text-align: center;
  font-size: 12.5px; color: var(--ink-300);
}
</style>
