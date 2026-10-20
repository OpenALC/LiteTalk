<script setup lang="ts">
// FriendList 好友栏（v0.1.2 批次③）：真实 REST 数据源（mock 已删）。
// - 结构：待处理区（对方发来的请求，带接受）→ 按分组渲染（默认组「好友」），
//   组内在线在前离线置灰；搜索过滤跨组。
// - 右键：设置备注 / 移动分组 / 删除好友（删除二次确认）。
// - 私聊仍为占位提示（服务端无私聊实体）；「加入TA的频道」因服务端无
//   「好友所在频道」数据暂不显示（inChannel 恒 false）。在线点接真实
//   presence（#11），两态：在线/离线。
import { computed, onMounted, ref } from 'vue'
import {
  ChevronDown, MessageCircle, Search, UserPlus, AudioLines,
  Pencil, FolderInput, Trash2,
} from 'lucide-vue-next'
import { useFriendsStore, useServersStore, type Friend } from '../stores/data'
import { useVoiceStore } from '../stores/voice'
import { useNotifyStore } from '../stores/notify'
import ContextMenu, { type CtxItem } from './ContextMenu.vue'
import PromptDialog from './PromptDialog.vue'

const friends = useFriendsStore()
const servers = useServersStore()
const voice = useVoiceStore()
const notify = useNotifyStore()
const query = ref('')
const closedGroups = ref<Record<string, boolean>>({})

onMounted(() => {
  // 拉取好友列表；失败静默（不影响主界面）。
  // presence:update / data:resync 的订阅在 MainView 统一绑定（#78）：好友栏
  // 可隐藏，订阅不能随本组件的挂载/卸载而丢。
  friends.loadFriends().catch(() => {})
})

// 搜索过滤（跨全部组）。
function match(f: Friend): boolean {
  const q = query.value.trim().toLowerCase()
  if (!q) return true
  return f.name.toLowerCase().includes(q) || f.nickname.toLowerCase().includes(q)
}

// 分组渲染：group 名 → 成员（在线在前，离线置灰在后）。
const groups = computed<{ name: string; list: Friend[] }[]>(() => {
  const map = new Map<string, Friend[]>()
  for (const f of [...friends.online, ...friends.offline]) {
    if (!match(f)) continue
    const key = f.group || '好友'
    if (!map.has(key)) map.set(key, [])
    map.get(key)!.push(f)
  }
  return [...map.entries()].map(([name, list]) => ({ name, list }))
})

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

// joinFriendChannel 按好友所在频道名跨服务器查找语音频道并加入。
// 当前服务端无「好友所在频道」下发，inChannel 恒 false——按钮不渲染，函数保留待数据接通。
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

// ---- 好友右键菜单：备注 / 移动分组 / 删除（二次确认） ----
const ctx = ref<{ x: number; y: number; friend: Friend } | null>(null)
// 备注/分组仅「发起方向」可写（服务端 PATCH 403 守卫）。对方加我的好友
// incoming=true，这两项禁用——否则点了必弹 403（review B1）。
const ctxItems = computed<CtxItem[]>(() => {
  const incoming = ctx.value?.friend.incoming ?? false
  return [
    { key: 'remark', label: '设置备注', icon: Pencil, disabled: incoming },
    { key: 'move', label: '移动分组', icon: FolderInput, disabled: incoming },
    { key: 'delete', label: '删除好友', icon: Trash2, danger: true, confirm: true },
  ]
})

function openCtx(e: MouseEvent, f: Friend) {
  ctx.value = { x: e.clientX, y: e.clientY, friend: f }
}

// prompt 弹层：备注（初值=现备注）或移动分组（初值=现组名）。
const prompt = ref<{ mode: 'remark' | 'move'; friend: Friend } | null>(null)

function onCtxSelect(key: string) {
  const c = ctx.value
  if (!c) return
  if (key === 'remark' || key === 'move') {
    prompt.value = { mode: key, friend: c.friend }
    return
  }
  if (key === 'delete') {
    // danger+confirm 已由 ContextMenu 做过二次确认，这里直接执行。
    friends.removeFriend(c.friend.id)
      .then(() => notify.show('已删除好友'))
      .catch((e) => notify.fromErr(e))
  }
}

async function onPromptConfirm(value: string) {
  const p = prompt.value
  prompt.value = null
  if (!p) return
  try {
    if (p.mode === 'remark') {
      await friends.setMeta(p.friend.id, value.trim(), p.friend.group)
      notify.show('备注已更新')
    } else {
      await friends.setMeta(p.friend.id, p.friend.remark, value.trim() || '好友')
      notify.show('已移动分组')
    }
  } catch (e) {
    notify.fromErr(e)
  }
}

// acceptPending 接受好友请求。
async function acceptPending(id: string) {
  try {
    await friends.acceptRequest(id)
    notify.show('已接受好友请求')
  } catch (e) {
    notify.fromErr(e)
  }
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
        <!-- 待处理：incoming=对方发来的（可接受）；outgoing=我发出的（S3：仅展示） -->
        <template v-if="friends.pending.length && !query">
          <div class="f-group"><ChevronDown :size="12" />待处理 — {{ friends.pending.length }}</div>
          <div v-for="p in friends.pending" :key="p.id" class="f-row">
            <span class="f-dot idle" />
            <div class="f-info">
              <div class="fn">{{ p.name }}</div>
              <div class="fs">{{ p.outgoing ? '等待对方接受' : '请求加为好友' }}</div>
            </div>
            <div v-if="!p.outgoing" class="f-act always">
              <button class="icon-btn accept" title="接受"
                      @click="acceptPending(p.id)">接受</button>
            </div>
          </div>
        </template>

        <template v-for="g in groups" :key="g.name">
          <div class="f-group" :class="{ closed: closedGroups[g.name] }"
               @click="closedGroups[g.name] = !closedGroups[g.name]">
            <ChevronDown :size="12" />{{ g.name }} — {{ g.list.length }}
          </div>
          <template v-if="!closedGroups[g.name]">
            <div v-for="f in g.list" :key="f.id"
                 class="f-row" :class="{ offline: f.status === 'offline' }"
                 @contextmenu.prevent="openCtx($event, f)">
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
        </template>

        <div v-if="!groups.length && !friends.pending.length" class="f-empty">
          暂无好友，点右上角 + 添加
        </div>
      </div>
    </div>

    <!-- 添加好友弹层（昵称#tag，服务端拆分） -->
    <PromptDialog v-if="adding" title="添加好友" placeholder="昵称#1234" cta="发送请求"
                  @confirm="onAddConfirm" @close="adding = false" />
    <!-- 备注 / 移动分组弹层 -->
    <PromptDialog v-if="prompt" :title="prompt.mode === 'remark' ? '设置备注' : '移动分组'"
                  :placeholder="prompt.mode === 'remark' ? '备注名称' : '分组名称'"
                  :initial="prompt.mode === 'remark' ? prompt.friend.remark : prompt.friend.group"
                  cta="确定" @confirm="onPromptConfirm" @close="prompt = null" />
    <!-- 右键菜单 -->
    <ContextMenu v-if="ctx" :x="ctx.x" :y="ctx.y" :items="ctxItems"
                 @select="onCtxSelect" @close="ctx = null" />
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
/* 待处理区的「接受」按钮：常显的小号主按钮 */
.icon-btn.accept {
  width: auto; height: 26px; padding: 0 12px;
  font-size: 12px; font-weight: 600;
  color: #fff; background: var(--brand-500);
}
.icon-btn.accept:hover { background: var(--brand-600); color: #fff; }
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
/* 待处理区常显 */
.f-act.always { opacity: 1; }
.f-act .icon-btn { width: 28px; height: 28px; }
.f-empty {
  padding: 28px 12px; text-align: center;
  font-size: 12.5px; color: var(--ink-300);
}
</style>
