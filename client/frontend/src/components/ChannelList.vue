<script setup lang="ts">
// ChannelList 频道栏（对齐 Main.html：分组折叠、文字/语音频道、人数、锁标识）。
// 批次①：顶部服务器名点击折叠/展开整个频道列表；频道右键管理（重命名/删除）；
// 底部齿轮打开设置浮层；所在语音频道行内展示成员（谁在房里一目了然）。
import { computed, ref } from 'vue'
import {
  ChevronDown, Eye, Hash, Lock, Mic, Volume2, Headphones, HeadphoneOff,
  MicOff, Settings, Pencil, Trash2,
} from 'lucide-vue-next'
import { useServersStore, useSettingsStore } from '../stores/data'
import { useVoiceStore } from '../stores/voice'
import { useAuthStore } from '../stores/auth'
import { useNotifyStore } from '../stores/notify'
import ContextMenu, { type CtxItem } from './ContextMenu.vue'
import PromptDialog from './PromptDialog.vue'

const servers = useServersStore()
const settings = useSettingsStore()
const voice = useVoiceStore()
const auth = useAuthStore()
const notify = useNotifyStore()

const collapsed = ref<Record<string, boolean>>({})

// 当前所在语音频道名（个人卡片状态行显示）
const currentVoiceChannelName = computed(() => {
  if (!voice.channelId) return ''
  const ch = servers.currentServer.channels.find((c) => c.id === voice.channelId)
  return ch?.name ?? voice.channelId
})

const groups = computed(() => {
  const map = new Map<string, typeof servers.currentServer.channels>()
  for (const ch of servers.currentServer.channels) {
    const list = map.get(ch.group) ?? []
    list.push(ch)
    map.set(ch.group, list)
  }
  return [...map.entries()]
})

// 全折叠态：所有分组都收起时视为已折叠（顶部服务器名点击的依据）。
const allCollapsed = computed(() =>
  groups.value.length > 0 && groups.value.every(([name]) => collapsed.value[name]))

function toggleGroup(name: string) {
  collapsed.value[name] = !collapsed.value[name]
}

// toggleAll 顶部服务器名点击：折叠/展开全部频道分组。
function toggleAll() {
  const target = !allCollapsed.value
  for (const [name] of groups.value) {
    collapsed.value[name] = target
  }
}

async function pick(id: string, type: string) {
  servers.selectChannel(id)
  // 重复点击已在的语音频道不再重新 join（服务端会报 already_in_channel）。
  if (type === 'voice' && id !== voice.channelId) {
    try {
      await voice.join(id)
    } catch (e) {
      notify.fromErr(e, '加入频道失败')
    }
  }
}

// ---- 频道右键菜单 ----
const ctx = ref<{ x: number; y: number; id: string; name: string } | null>(null)
const ctxItems: CtxItem[] = [
  { key: 'rename', label: '重命名频道', icon: Pencil },
  { key: 'delete', label: '删除频道', icon: Trash2, danger: true, confirm: true },
]
const renaming = ref<{ id: string; initial: string } | null>(null)

function openCtx(e: MouseEvent, id: string, name: string) {
  ctx.value = { x: e.clientX, y: e.clientY, id, name }
}

function onCtxSelect(key: string) {
  const c = ctx.value
  if (!c) return
  if (key === 'rename') {
    renaming.value = { id: c.id, initial: c.name }
    return
  }
  if (key === 'delete') {
    removeChannel(c.id)
  }
}

async function onRenameConfirm(value: string) {
  const r = renaming.value
  renaming.value = null
  if (!r) return
  try {
    await servers.renameChannel(servers.currentServerId, r.id, value)
    notify.show('频道已重命名')
  } catch (e) { notify.fromErr(e) }
}

// removeChannel 删除频道；若正身处该语音频道则先离开再删。
async function removeChannel(id: string) {
  try {
    if (voice.channelId === id) {
      await voice.leave()
    }
    await servers.deleteChannel(servers.currentServerId, id)
    notify.show('频道已删除')
  } catch (e) { notify.fromErr(e) }
}
</script>

<template>
  <aside class="channels">
    <!-- 顶部服务器名：点击折叠/展开全部频道分组 -->
    <div class="server-head" title="折叠 / 展开频道列表" @click="toggleAll">
      <span class="name">{{ servers.currentServer.name }}</span>
      <ChevronDown :size="16" :class="{ rotated: allCollapsed }" />
    </div>

    <div class="chan-scroll">
      <div v-for="[name, list] in groups" :key="name" class="cat"
           :class="{ closed: collapsed[name] }">
        <div class="cat-head" @click="toggleGroup(name)">
          <ChevronDown :size="13" />{{ name }}
        </div>
        <div class="cat-items">
          <template v-for="ch in list" :key="ch.id">
            <div class="chan"
                 :class="{ active: ch.id === servers.currentChannelId, 'in-voice': ch.id === voice.channelId }"
                 @click="pick(ch.id, ch.type)"
                 @contextmenu.prevent="openCtx($event, ch.id, ch.name)">
              <component :is="ch.type === 'text' ? Hash : (ch.locked ? Lock : (ch.id === 'ch-audience' ? Eye : Volume2))"
                         :size="16" />
              <span class="cname">{{ ch.name }}</span>
              <span v-if="ch.id === voice.channelId" class="count">
                <Mic :size="12" />{{ voice.members.length }}
              </span>
              <span v-else-if="ch.members" class="count"><Mic :size="12" />{{ ch.members }}</span>
            </div>
            <!-- 所在语音频道行内成员列表（频道内谁在说话可见） -->
            <div v-if="ch.id === voice.channelId && voice.members.length" class="chan-members">
              <div v-for="m in voice.members" :key="m.user_id" class="cm-row">
                <span class="cm-dot" :class="{ on: m.speaking, off: m.mute }" />
                <span class="cm-name">{{ m.nickname }}</span>
                <MicOff v-if="m.mute" :size="11" class="cm-mute" />
              </div>
            </div>
          </template>
        </div>
      </div>
    </div>

    <!-- 用户面板（双关态：状态点绿→红；显示当前语音频道与闭麦/静音状态） -->
    <div class="me">
      <span class="me-dot" :class="{ dual: voice.dualOff }" />
      <div class="me-info">
        <div class="n">{{ auth.user?.nickname ?? '未登录' }}</div>
        <div class="t">
          <template v-if="currentVoiceChannelName">
            <span class="chan-name">{{ currentVoiceChannelName }}</span>
            <span class="sep">·</span>
            <!-- 双关态两个都显示（S3：与 VoiceRoom 胶囊一致，不再只显示闭麦） -->
            <span v-if="voice.muted" class="state off">已闭麦</span>
            <span v-if="voice.deafened" class="state off">已静音</span>
            <span v-else class="state on">在线</span>
          </template>
          <template v-else>
            #{{ auth.user?.tag ?? '0000' }} · 在线
          </template>
        </div>
      </div>
      <div class="me-acts">
        <!-- 文案约定（实测走查 16）：麦克风=闭麦，扬声器=静音 -->
        <button class="icon-btn" :class="{ danger: voice.muted }"
                :title="voice.muted ? '取消闭麦' : '闭麦'"
                @click="voice.setMute(!voice.muted)">
          <MicOff v-if="voice.muted" :size="17" />
          <Mic v-else :size="17" />
        </button>
        <button class="icon-btn" :class="{ danger: voice.deafened }"
                :title="voice.deafened ? '取消静音' : '静音'"
                @click="voice.setDeafen(!voice.deafened)">
          <HeadphoneOff v-if="voice.deafened" :size="17" />
          <Headphones v-else :size="17" />
        </button>
        <button class="icon-btn" title="设置" @click="settings.openPanel()">
          <Settings :size="17" />
        </button>
      </div>
    </div>

    <ContextMenu v-if="ctx" :x="ctx.x" :y="ctx.y" :items="ctxItems"
                 @select="onCtxSelect" @close="ctx = null" />

    <PromptDialog v-if="renaming" title="重命名频道" :initial="renaming.initial" cta="保存"
                  @confirm="onRenameConfirm" @close="renaming = null" />
  </aside>
</template>

<style scoped>
.channels {
  width: 248px; flex: none;
  display: flex; flex-direction: column;
  background: var(--surface);
  border-right: 1px solid var(--line);
  min-height: 0;
}
.server-head {
  height: 52px; flex: none;
  display: flex; align-items: center; justify-content: space-between;
  padding: 0 16px; border-bottom: 1px solid var(--line);
  cursor: pointer; transition: background 0.15s;
  user-select: none;
}
.server-head:hover { background: var(--brand-50); }
.server-head .name { font-size: 15px; font-weight: 700; letter-spacing: 0.2px; }
.server-head :deep(svg) { color: var(--ink-400); transition: transform 0.25s var(--ease); }
/* 全折叠时箭头旋起，与分组头折叠态一致 */
.server-head :deep(svg.rotated) { transform: rotate(-90deg); }
.chan-scroll { flex: 1; overflow-y: auto; padding: 10px 10px 12px; }
.cat { margin-bottom: 14px; }
.cat-head {
  display: flex; align-items: center; gap: 3px;
  padding: 4px 6px; font-size: 11.5px; font-weight: 600;
  color: var(--ink-400); letter-spacing: 0.6px;
  cursor: pointer; user-select: none; border-radius: 7px;
  transition: color 0.15s;
}
.cat-head:hover { color: var(--ink-900); }
.cat-head :deep(svg) { transition: transform 0.25s var(--ease); }
.cat.closed .cat-head :deep(svg) { transform: rotate(-90deg); }
.cat.closed .cat-items { display: none; }
.chan {
  display: flex; align-items: center; gap: 8px;
  height: 34px; padding: 0 9px; margin: 1px 0;
  border-radius: 8px; color: var(--ink-600);
  cursor: pointer; transition: background 0.15s, color 0.15s;
}
.chan :deep(svg) { color: var(--ink-400); flex: none; transition: color 0.15s; }
.chan .cname {
  flex: 1; font-size: 13.5px; font-weight: 500;
  white-space: nowrap; overflow: hidden; text-overflow: ellipsis;
}
.chan .count {
  font-size: 11.5px; color: var(--ink-400);
  display: flex; align-items: center; gap: 3px;
}
.chan:hover { background: var(--brand-50); color: var(--ink-900); }
.chan.active { background: rgba(79, 164, 243, 0.12); color: var(--brand-700); }
.chan.active :deep(svg) { color: var(--brand-600); }
/* 所在语音频道：左侧品牌色边条提示 */
.chan.in-voice { box-shadow: inset 2px 0 0 var(--brand-500); }

/* 频道行内成员列表（仅所在语音频道展开） */
.chan-members { padding: 2px 0 6px 33px; }
.cm-row {
  display: flex; align-items: center; gap: 7px;
  height: 24px; font-size: 12px; color: var(--ink-600);
}
.cm-dot {
  width: 6px; height: 6px; border-radius: 50%; flex: none;
  background: var(--ink-300);
}
.cm-dot.on { background: var(--green); }
.cm-dot.off { background: var(--red); }
.cm-name { white-space: nowrap; overflow: hidden; text-overflow: ellipsis; }
.cm-mute { color: var(--ink-300); flex: none; }

.me {
  margin: 0 10px 10px; padding: 10px; border-radius: 12px;
  background: var(--brand-50);
  display: flex; align-items: center; gap: 10px;
}
.me-dot {
  position: relative; width: 9px; height: 9px; flex: none; margin-left: 2px;
}
.me-dot::before {
  content: ""; position: absolute; inset: 0;
  border-radius: 50%; background: var(--green);
  transition: background 0.2s;
}
.me-dot::after {
  content: ""; position: absolute; inset: -3px;
  border-radius: 50%; border: 1.5px solid var(--green);
  opacity: 0.4; animation: pulse 2.4s var(--ease) infinite;
  transition: border-color 0.2s;
}
/* 双关态：状态点绿→红 */
.me-dot.dual::before { background: var(--red); }
.me-dot.dual::after { border-color: var(--red); }
@keyframes pulse {
  0%, 100% { transform: scale(0.8); opacity: 0.45; }
  50% { transform: scale(1.15); opacity: 0.1; }
}
.me-info { flex: 1; min-width: 0; }
.me-info .n {
  font-size: 13.5px; font-weight: 600;
  white-space: nowrap; overflow: hidden; text-overflow: ellipsis;
}
.me-info .t {
  font-size: 11px; color: var(--ink-400);
  white-space: nowrap; overflow: hidden; text-overflow: ellipsis;
}
.me-info .t .chan-name { color: var(--brand-600); font-weight: 500; }
.me-info .t .sep { margin: 0 3px; }
.me-info .t .state.on { color: var(--green); }
.me-info .t .state.off { color: var(--red); }
.me-acts { display: flex; gap: 2px; flex: none; }
.icon-btn {
  width: 32px; height: 32px; flex: none;
  display: flex; align-items: center; justify-content: center;
  border-radius: 8px; color: var(--ink-600);
  transition: background 0.15s, color 0.15s, transform 0.1s var(--ease);
}
.icon-btn:hover { background: rgba(79, 164, 243, 0.12); color: var(--brand-700); }
.icon-btn:active { transform: scale(0.92); }
.icon-btn.danger { background: rgba(232, 91, 91, 0.12); color: var(--red); }
.icon-btn.danger:hover { background: rgba(232, 91, 91, 0.2); }
</style>
