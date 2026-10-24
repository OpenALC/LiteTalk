<script setup lang="ts">
// GroupMembersDialog 群聊成员管理：成员列表（昵称 + 群主/成员），owner 可勾选
// 好友加人、移除成员，任意成员可退出群聊。权限以服务端为准（只有 owner 的
// 已接受好友可被加入）；owner 退出时服务端自动转让或删除会话。
import { computed, onMounted, ref, watch } from 'vue'
import { UserMinus, X } from 'lucide-vue-next'
import { useAuthStore } from '../stores/auth'
import { useConversationsStore } from '../stores/conversations'
import { useFriendsStore, type Friend } from '../stores/data'
import { useNotifyStore } from '../stores/notify'

const props = defineProps<{ conversationId: string }>()
const emit = defineEmits<{ (e: 'close'): void }>()

const auth = useAuthStore()
const conversations = useConversationsStore()
const friends = useFriendsStore()
const notify = useNotifyStore()

const conv = computed(() => conversations.list.find((c) => c.id === props.conversationId) ?? null)
const owner = computed(() => conversations.isOwner(conv.value))
const full = computed(() => conv.value ? conv.value.members.length >= 50 : true)
const selfId = computed(() => auth.user?.id ?? '')

// 会话被删除或自己已被移出：列表里没有了就关闭弹层，不停留在幽灵会话上。
watch(conv, (v) => {
  if (!v) emit('close')
})

onMounted(() => {
  friends.loadFriends().catch(() => {})
})

// 可加入的好友：排除已在群里的人与自己；在线在前。
const candidates = computed<Friend[]>(() => {
  const c = conv.value
  if (!c) return []
  const have = new Set(c.members.map((m) => m.user_id))
  const seen = new Set<string>()
  const out: Friend[] = []
  for (const f of [...friends.online, ...friends.offline]) {
    if (have.has(f.userId) || f.userId === selfId.value || seen.has(f.userId)) continue
    seen.add(f.userId)
    out.push(f)
  }
  return out
})

const selected = ref<string[]>([])
function toggle(id: string) {
  const i = selected.value.indexOf(id)
  if (i >= 0) selected.value.splice(i, 1)
  else selected.value.push(id)
}

const busy = ref(false)

async function addSelected() {
  if (busy.value || !selected.value.length) return
  busy.value = true
  try {
    await conversations.addMembers(props.conversationId, [...selected.value])
    selected.value = []
    notify.show('已添加成员')
  } catch (e) {
    notify.fromErr(e, '添加成员失败')
  } finally {
    busy.value = false
  }
}

// 二次确认：移除成员与退出群聊共用一条确认栏，避免误点。
const arming = ref<{ kind: 'remove'; userId: string; name: string } | { kind: 'leave' } | null>(null)

function armRemove(userId: string, name: string) {
  arming.value = { kind: 'remove', userId, name }
}
function armLeave() {
  arming.value = { kind: 'leave' }
}

async function confirmArmed() {
  const a = arming.value
  if (!a || busy.value) return
  busy.value = true
  try {
    if (a.kind === 'remove') {
      await conversations.removeMember(props.conversationId, a.userId)
      notify.show('已移出成员')
    } else {
      await conversations.leave(props.conversationId)
      notify.show('已退出群聊')
      emit('close')
    }
    arming.value = null
  } catch (e) {
    notify.fromErr(e, a.kind === 'remove' ? '移出成员失败' : '退出群聊失败')
  } finally {
    busy.value = false
  }
}
</script>

<template>
  <div class="gm-overlay" @click.self="emit('close')" @keydown.esc="emit('close')">
    <div v-if="conv" class="gm-card" role="dialog" aria-label="群聊成员">
      <div class="gm-head">
        <b>群聊成员</b>
        <button class="gm-close" title="关闭" @click="emit('close')"><X :size="16" /></button>
      </div>

      <div class="gm-list">
        <div v-for="m in conv.members" :key="m.user_id" class="gm-row">
          <span class="gm-name">
            {{ m.nickname }}<span v-if="m.user_id === selfId" class="gm-me">（我）</span>
          </span>
          <span class="gm-role" :class="{ owner: m.role === 'owner' }">
            {{ m.role === 'owner' ? '群主' : '成员' }}
          </span>
          <button v-if="owner && m.user_id !== selfId" class="gm-remove"
                  title="移出成员" @click="armRemove(m.user_id, m.nickname)">
            <UserMinus :size="15" />
          </button>
        </div>
      </div>

      <template v-if="owner">
        <p class="gm-label">添加成员 <span v-if="full">（人数已达上限）</span></p>
        <div v-if="!full && candidates.length" class="gm-add-list">
          <button v-for="f in candidates" :key="f.userId" class="gm-add-row"
                  :class="{ on: selected.includes(f.userId) }" @click="toggle(f.userId)">
            <span class="gm-dot" :class="f.status" />
            <span class="gm-add-name">{{ f.name }}</span>
          </button>
        </div>
        <p v-else-if="!full" class="gm-empty">没有可添加的好友</p>
        <div v-if="!full && candidates.length" class="gm-add-act">
          <button class="gm-add-btn" :disabled="busy || !selected.length" @click="addSelected">
            添加{{ selected.length ? `（${selected.length}）` : '' }}
          </button>
        </div>
      </template>

      <div v-if="arming" class="gm-confirm">
        <span>{{ arming.kind === 'leave' ? '确认退出群聊？' : `确认移出 ${arming.name}？` }}</span>
        <button class="gm-cancel" @click="arming = null">取消</button>
        <button class="gm-danger" :disabled="busy" @click="confirmArmed">确认</button>
      </div>
      <div v-else class="gm-acts">
        <button class="gm-leave" @click="armLeave">退出群聊</button>
      </div>
    </div>
  </div>
</template>

<style scoped>
.gm-overlay {
  position: fixed; inset: 0; z-index: 70;
  background: rgba(23, 50, 75, 0.28);
  display: flex; align-items: center; justify-content: center;
}
.gm-card {
  width: 400px; max-width: calc(100vw - 48px);
  background: var(--surface); border-radius: 16px;
  box-shadow: var(--shadow-3); padding: 20px 22px 22px;
}
.gm-head { display: flex; align-items: center; justify-content: space-between; margin-bottom: 12px; }
.gm-head b { font-size: 15px; }
.gm-close {
  width: 28px; height: 28px; border-radius: 8px;
  display: flex; align-items: center; justify-content: center;
  color: var(--ink-400); transition: background 0.15s, color 0.15s;
}
.gm-close:hover { background: var(--brand-50); color: var(--ink-900); }
.gm-list {
  max-height: 220px; overflow-y: auto;
  display: flex; flex-direction: column; gap: 2px;
}
.gm-row {
  display: flex; align-items: center; gap: 10px;
  height: 36px; padding: 0 10px; border-radius: 10px;
}
.gm-row:hover { background: var(--brand-50); }
.gm-name { flex: 1; min-width: 0; font-size: 13.5px; color: var(--ink-900); }
.gm-me { color: var(--ink-400); }
.gm-role {
  font-size: 11.5px; color: var(--ink-400);
  padding: 2px 8px; border-radius: 6px; background: var(--brand-50);
}
.gm-role.owner { color: var(--brand-700); background: rgba(79, 164, 243, 0.12); }
.gm-remove {
  width: 26px; height: 26px; flex: none;
  display: flex; align-items: center; justify-content: center;
  border-radius: 7px; color: var(--ink-400);
  transition: background 0.15s, color 0.15s;
}
.gm-remove:hover { background: rgba(232, 91, 91, 0.1); color: var(--red); }
.gm-label {
  margin: 16px 0 6px;
  font-size: 11.5px; letter-spacing: 0.4px; color: var(--ink-400);
}
.gm-add-list {
  max-height: 150px; overflow-y: auto;
  display: flex; flex-wrap: wrap; gap: 6px;
}
.gm-add-row {
  display: flex; align-items: center; gap: 7px;
  height: 32px; padding: 0 10px; border-radius: 8px;
  background: var(--brand-50); font-size: 12.5px; color: var(--ink-600);
  transition: background 0.15s, color 0.15s;
}
.gm-add-row.on { background: rgba(79, 164, 243, 0.12); color: var(--brand-700); }
.gm-dot { width: 7px; height: 7px; flex: none; border-radius: 50%; }
.gm-dot.online { background: var(--green); }
.gm-dot.offline { background: var(--ink-300); }
.gm-add-name { white-space: nowrap; }
.gm-add-act { display: flex; justify-content: flex-end; margin-top: 10px; }
.gm-add-btn {
  height: 32px; padding: 0 14px; border-radius: 9px;
  background: var(--brand-500); color: #fff;
  font-size: 12.5px; font-weight: 600;
  transition: background 0.2s;
}
.gm-add-btn:hover:not(:disabled) { background: var(--brand-600); }
.gm-add-btn:disabled { opacity: 0.5; cursor: default; }
.gm-empty { margin: 0; font-size: 12.5px; color: var(--ink-300); }
.gm-acts { display: flex; justify-content: flex-end; margin-top: 18px; }
.gm-leave {
  height: 34px; padding: 0 14px; border-radius: 9px;
  font-size: 12.5px; font-weight: 500; color: var(--red);
  transition: background 0.15s;
}
.gm-leave:hover { background: rgba(232, 91, 91, 0.1); }
.gm-confirm {
  display: flex; align-items: center; gap: 10px;
  margin-top: 18px;
  font-size: 12.5px; color: var(--ink-600);
}
.gm-confirm span { flex: 1; min-width: 0; }
.gm-cancel {
  height: 34px; padding: 0 14px; border-radius: 9px;
  font-size: 12.5px; color: var(--ink-600);
  transition: background 0.15s;
}
.gm-cancel:hover { background: var(--brand-50); }
.gm-danger {
  height: 34px; padding: 0 14px; border-radius: 9px;
  background: var(--red); color: #fff;
  font-size: 12.5px; font-weight: 600;
  transition: opacity 0.2s;
}
.gm-danger:disabled { opacity: 0.5; cursor: default; }
</style>
