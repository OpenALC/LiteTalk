<script setup lang="ts">
// ChannelList 频道栏（v0.1.2）：双击加入语音频道（单击仅选中，对齐 Discord 语义）；
// 空白处右键创建频道/分类；分类头右键重命名/删除分类（含频道，二次确认），
// 按住分类头拖拽排序（#16，原生 DnD）；空服务器显示空态（#12）；
// 分类数据源为 servers.groupsOf（服务端 categories 实体）；
// 底部齿轮跳转 /settings 独立路由页。
import { computed, onMounted, ref, watch } from 'vue'
import {
  ChevronDown, Hash, Lock, Mic, Volume2, Headphones, HeadphoneOff,
  MicOff, Settings, Pencil, Trash2, FolderPlus, Folder, CircleAlert,
  Wrench,
} from 'lucide-vue-next'
import { useRouter } from 'vue-router'
import { useServersStore, catDropIndex, channelTree, type Channel } from '../stores/data'
import { badgeText, unreadLabel, useUnreadsStore, type UnreadCount } from '../stores/unreads'
import { useVoiceStore } from '../stores/voice'
import { useAuthStore } from '../stores/auth'
import { useNotifyStore } from '../stores/notify'
import { goApp } from '../api/bridge'
import type { AudioSettings } from '../api/types'
import ContextMenu, { type CtxItem } from './ContextMenu.vue'
import PromptDialog from './PromptDialog.vue'
import ChannelSettingsDialog from './ChannelSettingsDialog.vue'
import VolumeSlider from './VolumeSlider.vue'

const router = useRouter()
const servers = useServersStore()
const voice = useVoiceStore()
const unreads = useUnreadsStore()
const auth = useAuthStore()
const notify = useNotifyStore()

const collapsed = ref<Record<string, boolean>>({})

// unreadOf 频道未读计数（#40）：只对文字频道有意义，语音频道恒零（不显示徽标）。
const EMPTY_UNREAD: UnreadCount = { unread: 0, mentions: 0 }
function unreadOf(ch: Channel): UnreadCount {
  return ch.type === 'text' ? unreads.channel(ch.id) : EMPTY_UNREAD
}

// ---- 个人卡片音量悬停滑块（走查 5；批次③） ----
// 初值从持久化读；@apply（松手）才 SetAudioSettings 热应用+落盘。
const micVol = ref(100)
const playVol = ref(100)
const micOpen = ref(false)
const playOpen = ref(false)

onMounted(() => {
  goApp().GetAudioSettings()
    .then((s: AudioSettings) => {
      micVol.value = s.mic_volume
      playVol.value = s.playback_volume
    })
    .catch(() => {})
})

async function applyVolume(kind: 'mic' | 'playback', v: number) {
  try {
    const s = await goApp().GetAudioSettings()
    if (kind === 'mic') {
      s.mic_volume = v
      micVol.value = v
    } else {
      s.playback_volume = v
      playVol.value = v
    }
    await goApp().SetAudioSettings(s)
  } catch (e) {
    notify.fromErr(e, '音量保存失败')
  }
}

// 当前所在语音频道名（个人卡片状态行显示）。
// Q2：跨服务器查找——人在 A 服频道切到 B 服时不在 currentServer 里找裸 id。
const currentVoiceChannelName = computed(() => {
  if (!voice.channelId) return ''
  for (const s of servers.servers) {
    const ch = s.channels.find((c) => c.id === voice.channelId)
    if (ch) return ch.name
  }
  return ''
})

// groups：当前服务器的分类（channels group ∪ 空分类），数据源在 store。
const groups = computed(() => servers.groupsOf(servers.currentServerId, servers.currentServer.channels))

// 全折叠态：所有分组都收起时视为已折叠（顶部服务器名点击的依据）。
const allCollapsed = computed(() =>
  groups.value.length > 0 && groups.value.every((g) => collapsed.value[g.name]))

function toggleGroup(name: string) {
  collapsed.value[name] = !collapsed.value[name]
}

// toggleAll 顶部服务器名点击：折叠/展开全部频道分组。
function toggleAll() {
  const target = !allCollapsed.value
  for (const g of groups.value) {
    collapsed.value[g.name] = target
  }
}

// pick 单击：仅选中频道（文字/语音一致）；加入语音频道改为双击（joinOnDblClick）。
function pick(id: string) {
  servers.selectChannel(id)
}

// ---- 密码入房（#37 B2）：hasPassword 先弹密码框；确认后带密码 join ----
const pwdPrompt = ref<{ id: string } | null>(null)

function joinChannel(id: string, password?: string) {
  voice.join(id, password).catch((e) => notify.fromErr(e, '加入频道失败'))
}

// joinOnDblClick 双击语音频道行加入；已在频道时不重复 join。
// 仅语音频道可加入——双击文字频道进语音房是 review S2 抓的洞。
// hasPassword 先弹密码框（#37 B2）；取消不发请求。
async function joinOnDblClick(id: string) {
  servers.selectChannel(id)
  if (id === voice.channelId) return
  const ch = servers.currentServer.channels.find((c) => c.id === id)
  if (ch && ch.type !== 'voice') return
  if (ch?.hasPassword) {
    pwdPrompt.value = { id }
    return
  }
  joinChannel(id)
}

function onPwdConfirm(value: string) {
  const p = pwdPrompt.value
  pwdPrompt.value = null
  if (!p) return
  joinChannel(p.id, value)
}

// 入房失败 forbidden/bad_password → 重新弹密码框（#37 B2）：
// 覆盖 hasPassword 过期（本地以为无密码）或输错密码的情况。
watch(() => voice.lastJoinFailed, (f) => {
  if (!f) return
  if (f.code === 'bad_password' || f.code === 'forbidden') {
    pwdPrompt.value = { id: f.channelId }
  }
})

// ---- 频道右键菜单 ----
const ctx = ref<{ x: number; y: number; ch: Channel } | null>(null)
// 菜单项按频道动态出（#37 B2）：设置对所有频道开放；新建子频道仅顶层频道。
const ctxItems = computed<CtxItem[]>(() => {
  const c = ctx.value?.ch
  if (!c) return []
  const items: CtxItem[] = [
    { key: 'settings', label: '频道设置', icon: Wrench },
  ]
  if (!c.parentId) {
    items.push({ key: 'subchannel', label: '新建子频道', icon: FolderPlus })
  }
  items.push({ key: 'rename', label: '重命名频道', icon: Pencil })
  items.push({ key: 'delete', label: '删除频道', icon: Trash2, danger: true, confirm: true })
  return items
})
const renaming = ref<{ id: string; initial: string } | null>(null)
const settingsFor = ref<string | null>(null)
const subPrompt = ref<{ id: string; type: 'text' | 'voice' } | null>(null)

function openCtx(e: MouseEvent, ch: Channel) {
  ctx.value = { x: e.clientX, y: e.clientY, ch }
}

function onCtxSelect(key: string) {
  const c = ctx.value?.ch
  if (!c) return
  if (key === 'settings') {
    settingsFor.value = c.id
    return
  }
  if (key === 'subchannel') {
    // 类型继承父（#37 B2）：分类由服务端随父。
    subPrompt.value = { id: c.id, type: c.type }
    return
  }
  if (key === 'rename') {
    renaming.value = { id: c.id, initial: c.name }
    return
  }
  if (key === 'delete') {
    removeChannel(c.id)
  }
}

// onSubConfirm 新建子频道：名称经 PromptDialog，类型随父。
async function onSubConfirm(value: string) {
  const p = subPrompt.value
  subPrompt.value = null
  if (!p) return
  try {
    await servers.createSubchannel(servers.currentServerId, value, p.id)
    notify.show('子频道已创建')
  } catch (e) { notify.fromErr(e) }
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

// ---- 空白处右键：创建频道 / 创建分类 ----
const blankCtx = ref<{ x: number; y: number } | null>(null)
const blankCtxItems: CtxItem[] = [
  { key: 'new-channel', label: '创建频道', icon: FolderPlus },
  { key: 'new-category', label: '创建分类', icon: Folder },
]

function openBlankCtx(e: MouseEvent) {
  // 频道行/分类头有自己的菜单，空白处菜单不抢它们的事件（向上冒泡时目标判定）。
  const t = e.target as HTMLElement
  if (t.closest('.chan') || t.closest('.cat-head')) return
  blankCtx.value = { x: e.clientX, y: e.clientY }
}

const blankPrompt = ref<{ mode: 'channel' | 'category' } | null>(null)

// catChoices 归入分类候选（#72）：真实分类 + 末位「未分类」。
// groupsOf 的合成组（chan: 前缀，无分类频道归组用）不是服务端分类，不能当归宿。
const catChoices = computed(() => [
  ...groups.value.filter((g) => !g.id.startsWith('chan:')).map((g) => ({ value: g.id, label: g.name })),
  { value: '', label: '未分类' },
])

// defaultCatId 默认归入当前选中频道所在的分类，没有选中或该频道不在真实分类时
// 取第一个分类——用户打开对话框就能直接确认，不必每次手动选。
const defaultCatId = computed(() => {
  const s = servers.currentServer
  const ch = s.channels.find((c) => c.id === servers.currentChannelId)
  const real = groups.value.filter((g) => !g.id.startsWith('chan:'))
  if (ch) {
    const g = real.find((x) => x.name === ch.group)
    if (g) return g.id
  }
  return real[0]?.id ?? ''
})

function onBlankSelect(key: string) {
  if (key === 'new-channel' || key === 'new-category') {
    blankPrompt.value = { mode: key === 'new-channel' ? 'channel' : 'category' }
  }
}

async function onBlankConfirm(value: string, _option: string, option2: string) {
  const p = blankPrompt.value
  blankPrompt.value = null
  if (!p) return
  try {
    if (p.mode === 'channel') {
      // 第三参数是归入分类 id（'' = 未分类）：显式带 category_id 新建（#72）。
      await createChannelInGroup(servers.currentServerId, value, option2)
      notify.show('频道已创建')
    } else {
      await servers.createCategory(servers.currentServerId, value)
      notify.show('分类已创建')
    }
  } catch (e) { notify.fromErr(e) }
}

// createChannelInGroup：新建频道并显式归入分类（#72）。categoryId 是服务端分类
// id（'' = 不归类）；显式分类优先生效，缺省才由 store 走按名归组的旧路径。
async function createChannelInGroup(serverId: string, name: string, categoryId: string) {
  await servers.createChannel(serverId, name, 'voice', categoryId)
}

// ---- 分类头右键：重命名 / 删除分类（含频道，二次确认） ----
const catCtx = ref<{ x: number; y: number; id: string; name: string } | null>(null)
const catCtxItems: CtxItem[] = [
  { key: 'rename-cat', label: '重命名分类', icon: Pencil },
  { key: 'del-cat', label: '删除分类（含频道）', icon: Trash2, danger: true, confirm: true },
]
const catRenaming = ref<{ id: string; initial: string } | null>(null)

function openCatCtx(e: MouseEvent, id: string, name: string) {
  catCtx.value = { x: e.clientX, y: e.clientY, id, name }
}

async function onCatSelect(key: string) {
  const c = catCtx.value
  if (!c) return
  if (key === 'rename-cat') {
    catRenaming.value = { id: c.id, initial: c.name }
    return
  }
  if (key !== 'del-cat') return
  try {
    // 身处将被删除频道的语音频道时先离开。
    const s = servers.findServer(servers.currentServerId)
    if (s && s.channels.some((ch) => ch.group === c.name && ch.id === voice.channelId)) {
      await voice.leave()
    }
    await servers.deleteCategory(servers.currentServerId, c.id, c.name)
    notify.show('分类已删除')
  } catch (e) { notify.fromErr(e) }
}

async function onCatRenameConfirm(value: string) {
  const r = catRenaming.value
  catRenaming.value = null
  if (!r) return
  try {
    await servers.renameCategory(servers.currentServerId, r.id, value)
    notify.show('分类已重命名')
  } catch (e) { notify.fromErr(e) }
}

// ---- 分类拖拽排序（#16）：原生 DnD，不引第三方库；动效只用 opacity ----
const dragCat = ref<string | null>(null)
const dropIdx = ref<number | null>(null)

// dropPos 把 groups 序号映射到 reorderCategory 的插入序：落到非分类条目（未分类/
// 内置自带组）取分类区末尾；落到分类上 = 插到它之前（.drop-target 顶边线）。
function dropPos(gi: number, dragId: string): number {
  const list = servers.extraGroups[servers.currentServerId] ?? []
  const g = groups.value[gi]
  const to = g ? list.findIndex((x) => x.id === g.id) : -1
  return catDropIndex(list, dragId, to)
}

function onCatDragStart(e: DragEvent, id: string) {
  // 未分类/内置自带组（chan: 前缀）无排序语义，不可拖。
  if (id.startsWith('chan:')) {
    e.preventDefault()
    return
  }
  dragCat.value = id
  if (e.dataTransfer) {
    e.dataTransfer.effectAllowed = 'move'
    e.dataTransfer.setData('text/plain', id)
  }
}

function onCatDragOver(e: DragEvent, gi: number) {
  if (!dragCat.value) return
  e.preventDefault()
  dropIdx.value = gi
}

function onCatDragEnd() {
  dragCat.value = null
  dropIdx.value = null
}

async function onCatDrop(e: DragEvent, gi: number) {
  e.preventDefault()
  const id = dragCat.value
  dragCat.value = null
  dropIdx.value = null
  if (!id) return
  try {
    await servers.reorderCategory(servers.currentServerId, id, dropPos(gi, id))
  } catch (err) {
    notify.fromErr(err, '排序保存失败')
  }
}
</script>

<template>
  <aside class="channels">
    <!-- 顶部服务器名：点击折叠/展开全部频道分组 -->
    <div class="server-head" title="折叠 / 展开频道列表" @click="toggleAll">
      <span class="name">{{ servers.currentServer.name }}</span>
      <ChevronDown :size="16" :class="{ rotated: allCollapsed }" />
    </div>

    <!-- 空白处右键：只挂自定义「创建频道」菜单，原生菜单由全局拦截屏蔽 -->
    <div class="chan-scroll" @contextmenu.prevent="openBlankCtx">
      <!-- 空服务器空态（#12）：右键空白处仍可创建频道；加载失败给解释而不是恒空 -->
      <div v-if="groups.length === 0" class="empty">
        <CircleAlert v-if="servers.currentServer.unavailable" :size="26" />
        <Hash v-else :size="26" />
        <span>{{ servers.currentServer.unavailable ? '无法加载频道' : '暂无频道' }}</span>
      </div>
      <div v-for="(g, gi) in groups" :key="g.name" class="cat"
           :class="{ closed: collapsed[g.name], dragging: dragCat === g.id, 'drop-target': dropIdx === gi && dragCat !== null && dragCat !== g.id }"
           @dragover.prevent="onCatDragOver($event, gi)"
           @drop.prevent="onCatDrop($event, gi)">
        <!-- 分类头：左键折叠/展开，右键重命名/删除，按住拖拽排序（#16） -->
        <div class="cat-head" draggable="true"
             @click="toggleGroup(g.name)"
             @contextmenu.prevent="openCatCtx($event, g.id, g.name)"
             @dragstart="onCatDragStart($event, g.id)"
             @dragend="onCatDragEnd">
          <ChevronDown :size="13" />{{ g.name }}
        </div>
        <div class="cat-items">
          <!-- 频道树（#37 B2）：根 + 缩进子频道，保持服务端顺序 -->
          <template v-for="node in channelTree(servers.currentServer.channels.filter((c) => c.group === g.name))" :key="node.ch.id">
            <div class="chan"
                 :class="{ active: node.ch.id === servers.currentChannelId, 'in-voice': node.ch.id === voice.channelId }"
                 :title="node.ch.type === 'voice' ? '双击加入语音频道' : ''"
                 @click="pick(node.ch.id)"
                 @dblclick.prevent="joinOnDblClick(node.ch.id)"
                 @contextmenu.prevent="openCtx($event, node.ch)">
              <component :is="node.ch.hasPassword || node.ch.locked ? Lock : (node.ch.type === 'text' ? Hash : Volume2)"
                         :size="16" />
              <span class="cname" :class="{ unread: unreadOf(node.ch).unread > 0 }">{{ node.ch.name }}</span>
              <!-- 未读徽标（#40）：提及数优先，否则未读圆点；语音频道不显示 -->
              <span v-if="unreadOf(node.ch).mentions > 0" class="unread-badge"
                    :title="unreadLabel(unreadOf(node.ch))">{{ badgeText(unreadOf(node.ch).mentions) }}</span>
              <span v-else-if="unreadOf(node.ch).unread > 0" class="unread-dot"
                    :title="unreadLabel(unreadOf(node.ch))" />
              <span v-if="node.ch.id === voice.channelId" class="count">
                <Mic :size="12" />{{ voice.members.length }}
              </span>
              <span v-else-if="node.ch.members" class="count"><Mic :size="12" />{{ node.ch.members }}</span>
            </div>
            <!-- 所在语音频道行内成员列表（频道内谁在说话可见） -->
            <div v-if="node.ch.id === voice.channelId && voice.members.length" class="chan-members">
              <div v-for="m in voice.members" :key="m.user_id" class="cm-row">
                <span class="cm-dot" :class="{ on: m.speaking, off: m.mute }" />
                <span class="cm-name">{{ m.nickname }}</span>
                <MicOff v-if="m.mute" :size="11" class="cm-mute" />
              </div>
            </div>
            <div v-for="child in node.children" :key="child.id" class="chan sub"
                 :class="{ active: child.id === servers.currentChannelId, 'in-voice': child.id === voice.channelId }"
                 :title="child.type === 'voice' ? '双击加入语音频道' : ''"
                 @click="pick(child.id)"
                 @dblclick.prevent="joinOnDblClick(child.id)"
                 @contextmenu.prevent="openCtx($event, child)">
              <component :is="child.hasPassword || child.locked ? Lock : (child.type === 'text' ? Hash : Volume2)"
                         :size="16" />
              <span class="cname" :class="{ unread: unreadOf(child).unread > 0 }">{{ child.name }}</span>
              <span v-if="unreadOf(child).mentions > 0" class="unread-badge"
                    :title="unreadLabel(unreadOf(child))">{{ badgeText(unreadOf(child).mentions) }}</span>
              <span v-else-if="unreadOf(child).unread > 0" class="unread-dot"
                    :title="unreadLabel(unreadOf(child))" />
              <span v-if="child.id === voice.channelId" class="count">
                <Mic :size="12" />{{ voice.members.length }}
              </span>
              <span v-else-if="child.members" class="count"><Mic :size="12" />{{ child.members }}</span>
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
            <!-- 双关态两个都显示；有任何状态时不再显示「在线」（S3：v-else 原只挂静音分支，
                 闭麦时会并排「已闭麦 · 在线」）。 -->
            <template v-if="voice.muted || voice.deafened">
              <span v-if="voice.muted" class="state off">已闭麦</span>
              <span v-if="voice.deafened" class="state off">已静音</span>
            </template>
            <span v-else class="state on">在线</span>
          </template>
          <template v-else>
            #{{ auth.user?.tag ?? '0000' }} · 在线
          </template>
        </div>
      </div>
      <div class="me-acts">
        <!-- 文案约定（实测走查 16）：麦克风=闭麦，扬声器=静音 -->
        <!-- v0.1.2 批次③（走查 5）：hover 按钮上方弹竖向音量滑块（0 底 120 顶） -->
        <div class="vol-wrap" @mouseenter="micOpen = true" @mouseleave="micOpen = false">
          <VolumeSlider :open="micOpen" :value="micVol" label="麦克风音量"
                        @input="micVol = $event" @apply="applyVolume('mic', $event)" />
          <button class="icon-btn" :class="{ danger: voice.muted }"
                  :title="voice.muted ? '取消闭麦' : '闭麦'"
                  @click="voice.setMute(!voice.muted)">
            <MicOff v-if="voice.muted" :size="17" />
            <Mic v-else :size="17" />
          </button>
        </div>
        <div class="vol-wrap" @mouseenter="playOpen = true" @mouseleave="playOpen = false">
          <VolumeSlider :open="playOpen" :value="playVol" label="扬声器音量"
                        @input="playVol = $event" @apply="applyVolume('playback', $event)" />
          <button class="icon-btn" :class="{ danger: voice.deafened }"
                  :title="voice.deafened ? '取消静音' : '静音'"
                  @click="voice.setDeafen(!voice.deafened)">
            <HeadphoneOff v-if="voice.deafened" :size="17" />
            <Headphones v-else :size="17" />
          </button>
        </div>
        <!-- v0.1.2：设置独立路由页 -->
        <button class="icon-btn" title="设置" @click="router.push('/settings')">
          <Settings :size="17" />
        </button>
      </div>
    </div>

    <ContextMenu v-if="ctx" :x="ctx.x" :y="ctx.y" :items="ctxItems"
                 @select="onCtxSelect" @close="ctx = null" />
    <ContextMenu v-if="blankCtx" :x="blankCtx.x" :y="blankCtx.y" :items="blankCtxItems"
                 @select="onBlankSelect" @close="blankCtx = null" />
    <ContextMenu v-if="catCtx" :x="catCtx.x" :y="catCtx.y" :items="catCtxItems"
                 @select="onCatSelect" @close="catCtx = null" />

    <PromptDialog v-if="renaming" title="重命名频道" :initial="renaming.initial" cta="保存"
                  @confirm="onRenameConfirm" @close="renaming = null" />
    <!-- 分类名上限 32（服务端 validCategoryName）：上限放到 64 会打到 400 才报错（#16） -->
    <PromptDialog v-if="catRenaming" title="重命名分类" :initial="catRenaming.initial" cta="保存"
                  :maxlen="32" @confirm="onCatRenameConfirm" @close="catRenaming = null" />
    <!-- 归入分类是显式选择（#72）：默认当前选中频道所在分类，改选即可换宿。 -->
    <PromptDialog v-if="blankPrompt?.mode === 'channel'" title="创建频道"
                  placeholder="频道名称" cta="创建"
                  :options2="catChoices" :option2-default="defaultCatId"
                  options2-label="归入分类"
                  @confirm="onBlankConfirm" @close="blankPrompt = null" />
    <PromptDialog v-else-if="blankPrompt?.mode === 'category'" title="创建分类"
                  placeholder="分类名称" cta="创建"
                  :maxlen="32"
                  @confirm="onBlankConfirm" @close="blankPrompt = null" />
    <!-- 密码入房（#37 B2）：secret 输入、不 trim；取消不发请求 -->
    <PromptDialog v-if="pwdPrompt" title="输入频道密码"
                  placeholder="频道密码" cta="进入" :maxlen="72"
                  secret :trim="false"
                  @confirm="onPwdConfirm" @close="pwdPrompt = null" />
    <!-- 新建子频道（#37 B2）：名称输入，类型随父、分类由服务端随父 -->
    <PromptDialog v-if="subPrompt" title="新建子频道"
                  placeholder="子频道名称" cta="创建"
                  @confirm="onSubConfirm" @close="subPrompt = null" />
    <!-- 频道设置（#37 B2）：密码/人数上限/父频道 -->
    <ChannelSettingsDialog v-if="settingsFor" :channel-id="settingsFor"
                           @close="settingsFor = null" />
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
/* 空服务器空态（#12）：四色内的弱化灰，不用 emoji */
.empty {
  display: flex; flex-direction: column; align-items: center; justify-content: center;
  gap: 10px; padding: 36px 12px;
  color: var(--ink-400); font-size: 12.5px;
}
.empty :deep(svg) { color: var(--ink-300); }
.cat { margin-bottom: 14px; transition: opacity 0.15s; }
/* 拖拽排序（#16）：动效只用 opacity；落点以上边线示意 */
.cat.dragging { opacity: 0.45; }
.cat.drop-target { box-shadow: inset 0 2px 0 var(--brand-500); }
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
  user-select: none;
}
.chan :deep(svg) { color: var(--ink-400); flex: none; transition: color 0.15s; }
.chan .cname {
  flex: 1; font-size: 13.5px; font-weight: 500;
  white-space: nowrap; overflow: hidden; text-overflow: ellipsis;
}
/* 有未读的频道名加粗（#40），颜色用品牌蓝灰 */
.chan .cname.unread { font-weight: 600; color: var(--ink-900); }
/* 未读徽标（#40）：提及数为主色胶囊白字，未读为小圆点 */
.chan .unread-badge {
  flex: none;
  min-width: 16px; height: 16px; padding: 0 4px;
  border-radius: 8px;
  background: var(--brand-500); color: var(--surface);
  font-size: 10px; font-weight: 700; line-height: 16px;
  text-align: center;
}
.chan .unread-dot {
  flex: none;
  width: 8px; height: 8px; border-radius: 50%;
  background: var(--brand-500);
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
/* 子频道缩进（#37 B2 频道树两级） */
.chan.sub { padding-left: 26px; }

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
/* v0.1.2 批次③：音量滑块的定位父级（hover 按钮上方弹出） */
.vol-wrap { position: relative; }
</style>
