<script setup lang="ts">
// FriendList 好友栏（对齐 Main.html：搜索过滤、在线/离线分组、状态点、无头像）。
// 批次①：添加好友弹层（REST 真实请求）；私聊为占位提示（服务端无私聊实体）；
// 「加入TA的频道」按好友所在频道名查找并加入语音频道。
import { computed, ref } from 'vue'
import {
  ChevronDown, MessageCircle, Search, UserPlus, AudioLines,
} from 'lucide-vue-next'
import { useFriendsStore, useServersStore, type Friend } from '../stores/data'
import { useVoiceStore } from '../stores/voice'
import { useNotifyStore } from '../stores/notify'
import PromptDialog from './PromptDialog.vue'

const friends = useFriendsStore()
const servers = useServersStore()
const voice = useVoiceStore()
const notify = useNotifyStore()
const query = ref('')
const onlineClosed = ref(false)
const offlineClosed = ref(true)

const filteredOnline = computed(() =>
  friends.online.filter((f) => f.name.toLowerCase().includes(query.value.toLowerCase())))
const filteredOffline = computed(() =>
  friends.offline.filter((f) => f.name.toLowerCase().includes(query.value.toLowerCase())))

// ---- 添加好友（REST）----
const adding = ref(false)

async function onAddConfirm(target: string) {
  adding.value = false
  try {
    await friends.sendRequest(target)
    notify.show('好友请求已发送')
  } catch (e) {
    notify.fromErr(e)
  }
}

// ---- 私聊：服务端无私聊频道实体，占位提示（不假装已实现）----
function messageFriend() {
  notify.show('私聊即将上线')
}

// joinFriendChannel 按好友所在频道名（f.sub）跨服务器查找语音频道并加入。
async function joinFriendChannel(f: Friend) {
  for (const s of servers.servers) {
    const ch = s.channels.find((c) => c.type === 'voice' && c.name === f.sub)
    if (ch) {
      servers.selectServer(s.id)
      servers.selectChannel(ch.id)
      if (voice.channelId !== ch.id) {
        try {
          await voice.join(ch.id)
        } catch (e) {
          notify.fromErr(e, '加入频道失败')
          return
        }
      }
      notify.show(`已加入「${ch.name}」`)
      return
    }
  }
  notify.show('找不到TA所在的频道', true)
}
</script>

<template>
  <aside class="friends">
    <div class="friends-inner">
      <div class="f-head">
        <b>好友列表</b>
        <button class="icon-btn" title="添加好友" @click="adding = true"><UserPlus :size="17" /></button>
      </div>

      <div class="f-search">
        <Search :size="15" />
        <input v-model="query" placeholder="搜索好友...">
      </div>

      <div class="f-scroll">
        <div class="f-group" :class="{ closed: onlineClosed }" @click="onlineClosed = !onlineClosed">
          <ChevronDown :size="12" />在线 — {{ friends.online.length }}
        </div>
        <template v-if="!onlineClosed">
          <div v-for="f in filteredOnline" :key="f.name" class="f-row">
            <span class="f-dot" :class="f.status" />
            <div class="f-info">
              <div class="fn">{{ f.name }}</div>
              <div class="fs" :class="{ 'in-chan': f.inChannel }">{{ f.sub }}</div>
            </div>
            <div class="f-act">
              <button class="icon-btn" title="私聊" @click="messageFriend"><MessageCircle :size="14" /></button>
              <button v-if="f.inChannel" class="icon-btn" title="加入TA的频道"
                      @click="joinFriendChannel(f)">
                <AudioLines :size="14" />
              </button>
            </div>
          </div>
        </template>

        <div class="f-group" :class="{ closed: offlineClosed }" @click="offlineClosed = !offlineClosed">
          <ChevronDown :size="12" />离线 — {{ friends.offline.length }}
        </div>
        <template v-if="!offlineClosed">
          <div v-for="f in filteredOffline" :key="f.name" class="f-row offline">
            <span class="f-dot offline" />
            <div class="f-info">
              <div class="fn">{{ f.name }}</div>
              <div class="fs">离线</div>
            </div>
          </div>
        </template>
      </div>
    </div>

    <!-- 添加好友弹层（昵称#tag，服务端拆分） -->
    <PromptDialog v-if="adding" title="添加好友" placeholder="昵称#1234" cta="发送请求"
                  @confirm="onAddConfirm" @close="adding = false" />
  </aside>
</template>

<style scoped>
.friends {
  width: 272px; flex: none;
  background: var(--surface);
  border-left: 1px solid var(--line);
  overflow: hidden;
}
.friends-inner { width: 272px; height: 100%; display: flex; flex-direction: column; }
.f-head {
  height: 56px; flex: none;
  display: flex; align-items: center; justify-content: space-between;
  padding: 0 16px; border-bottom: 1px solid var(--line);
}
.f-head b { font-size: 15px; }
.icon-btn {
  width: 32px; height: 32px;
  display: flex; align-items: center; justify-content: center;
  border-radius: 8px; color: var(--ink-600);
  transition: background 0.15s, color 0.15s;
}
.icon-btn:hover { background: rgba(79, 164, 243, 0.12); color: var(--brand-700); }
.f-search {
  flex: none; margin: 12px 14px 6px;
  display: flex; align-items: center; gap: 8px;
  height: 36px; padding: 0 12px;
  background: var(--brand-50); border-radius: 10px;
  border: 1.5px solid transparent;
  transition: border-color 0.2s, background 0.2s;
}
.f-search:focus-within { border-color: var(--brand-500); background: var(--surface); }
.f-search :deep(svg) { color: var(--ink-300); flex: none; }
.f-search input {
  flex: 1; min-width: 0; border: none; outline: none;
  background: transparent; font-size: 13px; color: var(--ink-900);
}
.f-search input::placeholder { color: var(--ink-300); }
.f-scroll { flex: 1; overflow-y: auto; padding: 6px 10px; }
.f-group {
  padding: 8px 8px 4px;
  display: flex; align-items: center; gap: 4px;
  font-size: 11.5px; font-weight: 600;
  color: var(--ink-400); letter-spacing: 0.6px;
  cursor: pointer; user-select: none;
}
.f-group :deep(svg) { transition: transform 0.25s var(--ease); }
.f-group.closed :deep(svg) { transform: rotate(-90deg); }
.f-row {
  display: flex; align-items: center; gap: 10px;
  padding: 8px 10px; border-radius: 10px;
  cursor: pointer; transition: background 0.15s;
}
.f-row:hover { background: var(--brand-50); }
.f-dot { width: 8px; height: 8px; flex: none; border-radius: 50%; }
.f-dot.online { background: var(--green); }
.f-dot.idle { background: var(--orange); }
.f-dot.busy { background: var(--red); }
.f-dot.offline { background: var(--ink-300); }
.f-info { flex: 1; min-width: 0; }
.f-info .fn {
  font-size: 13.5px; font-weight: 600;
  white-space: nowrap; overflow: hidden; text-overflow: ellipsis;
}
.f-info .fs {
  font-size: 11.5px; color: var(--ink-400);
  white-space: nowrap; overflow: hidden; text-overflow: ellipsis;
}
.f-info .fs.in-chan { color: var(--brand-600); font-weight: 500; }
.f-row.offline .fn, .f-row.offline .fs { color: var(--ink-300); }
/* 操作按钮恒在但透明，hover 仅显色——避免显隐切换导致布局抖动（鼠标在两卡片间反复触发） */
.f-act { display: flex; gap: 2px; opacity: 0; transition: opacity 0.15s; }
/* hover 与键盘 focus-within 双通道显形：操作按钮透明时仍可 Tab 聚焦（可访问性） */
.f-row:hover .f-act, .f-row:focus-within .f-act { opacity: 1; }
.f-act .icon-btn { width: 28px; height: 28px; }
</style>
