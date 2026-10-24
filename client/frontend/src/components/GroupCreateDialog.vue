<script setup lang="ts">
// GroupCreateDialog 新建群聊：从好友列表（在线+离线）多选，至少 2 人；
// 群名可选（1–64 字符，服务端校验）。创建成功后由 conversations store 选中
// 新会话并进入私信模式。
import { computed, onMounted, ref } from 'vue'
import { Check, X } from 'lucide-vue-next'
import { useConversationsStore } from '../stores/conversations'
import { useFriendsStore, type Friend } from '../stores/data'
import { useNotifyStore } from '../stores/notify'

const emit = defineEmits<{ (e: 'close'): void }>()
const conversations = useConversationsStore()
const friends = useFriendsStore()
const notify = useNotifyStore()

const name = ref('')
const selected = ref<string[]>([])
const busy = ref(false)

onMounted(() => {
  // 好友列表由 FriendList 挂载时拉取；弹层独立打开时兜底再拉一次，失败静默。
  friends.loadFriends().catch(() => {})
})

// candidates 全部 accepted 好友（在线在前），成员 ID 用 userId。
const candidates = computed<Friend[]>(() => {
  const seen = new Set<string>()
  const out: Friend[] = []
  for (const f of [...friends.online, ...friends.offline]) {
    if (seen.has(f.userId)) continue
    seen.add(f.userId)
    out.push(f)
  }
  return out
})

// 群聊总人数上限 50（含创建者），因此最多邀请 49 人；超出的选择不生效，
// 避免提交后在服务端才拿到 bad_members。
const MAX_INVITES = 49

function toggle(id: string) {
  const i = selected.value.indexOf(id)
  if (i >= 0) {
    selected.value.splice(i, 1)
    return
  }
  if (selected.value.length >= MAX_INVITES) return
  selected.value.push(id)
}

async function submit() {
  if (busy.value || selected.value.length < 2) return
  busy.value = true
  try {
    await conversations.createGroup(name.value.trim(), [...selected.value])
    notify.show('群聊已创建')
    emit('close')
  } catch (e) {
    notify.fromErr(e, '创建群聊失败')
  } finally {
    busy.value = false
  }
}
</script>

<template>
  <div class="gc-overlay" @click.self="emit('close')" @keydown.esc="emit('close')">
    <div class="gc-card" role="dialog" aria-label="新建群聊">
      <div class="gc-head">
        <b>新建群聊</b>
        <button class="gc-close" title="关闭" @click="emit('close')"><X :size="16" /></button>
      </div>

      <input v-model="name" class="gc-input" placeholder="群名（可选，1–64 个字符）"
             maxlength="64" autofocus @keydown.enter.prevent="submit">

      <p class="gc-hint">选择至少 2 位好友</p>
      <div class="gc-list">
        <button v-for="f in candidates" :key="f.userId" class="gc-row"
                :class="{ on: selected.includes(f.userId) }" @click="toggle(f.userId)">
          <span class="gc-dot" :class="f.status" />
          <span class="gc-name">{{ f.name }}</span>
          <span class="gc-check"><Check v-if="selected.includes(f.userId)" :size="15" /></span>
        </button>
        <div v-if="!candidates.length" class="gc-empty">暂无好友，先添加好友</div>
      </div>

      <div class="gc-acts">
        <span class="gc-count">已选 {{ selected.length }} 人</span>
        <button class="gc-cancel" @click="emit('close')">取消</button>
        <button class="gc-ok" :disabled="busy || selected.length < 2" @click="submit">创建</button>
      </div>
    </div>
  </div>
</template>

<style scoped>
.gc-overlay {
  position: fixed; inset: 0; z-index: 70;
  background: rgba(23, 50, 75, 0.28);
  display: flex; align-items: center; justify-content: center;
}
.gc-card {
  width: 400px; max-width: calc(100vw - 48px);
  background: var(--surface); border-radius: 16px;
  box-shadow: var(--shadow-3); padding: 20px 22px 22px;
}
.gc-head { display: flex; align-items: center; justify-content: space-between; margin-bottom: 14px; }
.gc-head b { font-size: 15px; }
.gc-close {
  width: 28px; height: 28px; border-radius: 8px;
  display: flex; align-items: center; justify-content: center;
  color: var(--ink-400); transition: background 0.15s, color 0.15s;
}
.gc-close:hover { background: var(--brand-50); color: var(--ink-900); }
.gc-input {
  width: 100%; height: 40px; padding: 0 12px;
  background: var(--brand-50); border: 1.5px solid var(--brand-200);
  border-radius: 10px; font-size: 14px; color: var(--ink-900);
  outline: none; transition: border-color 0.2s, box-shadow 0.2s;
}
.gc-input:focus { border-color: var(--brand-500); box-shadow: 0 0 0 3px rgba(79, 164, 243, 0.15); }
.gc-hint {
  margin: 14px 0 6px;
  font-size: 11.5px; letter-spacing: 0.4px; color: var(--ink-400);
}
.gc-list {
  max-height: 260px; overflow-y: auto;
  display: flex; flex-direction: column; gap: 2px;
}
.gc-row {
  display: flex; align-items: center; gap: 10px;
  height: 38px; padding: 0 10px; border-radius: 10px;
  font-size: 13.5px; color: var(--ink-600);
  transition: background 0.15s, color 0.15s;
}
.gc-row:hover { background: var(--brand-50); }
.gc-row.on { background: rgba(79, 164, 243, 0.12); color: var(--brand-700); }
.gc-dot { width: 8px; height: 8px; flex: none; border-radius: 50%; }
.gc-dot.online { background: var(--green); }
.gc-dot.offline { background: var(--ink-300); }
.gc-name { flex: 1; min-width: 0; text-align: left; }
.gc-check {
  width: 20px; height: 20px; flex: none;
  display: flex; align-items: center; justify-content: center;
  border-radius: 6px; border: 1.5px solid var(--brand-200);
  color: #fff;
}
.gc-row.on .gc-check { background: var(--brand-500); border-color: var(--brand-500); }
.gc-empty { padding: 22px 12px; text-align: center; font-size: 12.5px; color: var(--ink-300); }
.gc-acts {
  display: flex; align-items: center; justify-content: flex-end;
  gap: 10px; margin-top: 18px;
}
.gc-count { margin-right: auto; font-size: 12px; color: var(--ink-400); }
.gc-cancel {
  height: 36px; padding: 0 16px; border-radius: 10px;
  font-size: 13px; font-weight: 500; color: var(--ink-600);
  transition: background 0.15s;
}
.gc-cancel:hover { background: var(--brand-50); }
.gc-ok {
  height: 36px; padding: 0 18px; border-radius: 10px;
  background: var(--brand-500); color: #fff;
  font-size: 13px; font-weight: 600; transition: background 0.2s;
}
.gc-ok:hover:not(:disabled) { background: var(--brand-600); }
.gc-ok:disabled { opacity: 0.5; cursor: default; }
</style>
