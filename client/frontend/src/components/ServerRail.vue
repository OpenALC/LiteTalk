<script setup lang="ts">
// ServerRail 服务器轨道（v0.1.2 重构）：顶部介绍页入口 + 服务器列表 + 底部加号。
// 图标为预设 Lucide（禁头像/上传图——硬性约束）。
// 右键菜单：复制邀请码/新建频道/重命名/删除服务器/设为首页；
// 介绍页右键：删除介绍页（仅当首页不是介绍页，可恢复）。
// 角标死数据已移除（走查 13：无真实未读数据源）。
import { computed, ref } from 'vue'
import {
  Plus, Home, MessagesSquare, Gamepad2, Music, Palette, BookOpen, Coffee, Mic,
  GraduationCap, Heart, Rocket, Camera, Dumbbell, Undo2,
  Link, FolderPlus, Pencil, Trash2,
} from 'lucide-vue-next'
import { INTRO_ID, useServersStore } from '../stores/data'
import { useNotifyStore } from '../stores/notify'
import { goApp } from '../api/bridge'
import ServerDialog from './ServerDialog.vue'
import ContextMenu, { type CtxItem } from './ContextMenu.vue'
import PromptDialog from './PromptDialog.vue'

const store = useServersStore()
const notify = useNotifyStore()

// Lucide 图标名 → 组件（静态注册，避免动态导入复杂度）。
// 键集合与 server/internal/api/servers.go 的 allowedIcons 保持一致。
const icons: Record<string, unknown> = {
  MessagesSquare, Gamepad2, Music, Palette, BookOpen, Coffee, Mic,
  GraduationCap, Heart, Rocket, Camera, Dumbbell,
}
const currentIcons = computed(() =>
  store.servers.map((s) => ({ ...s, iconComp: icons[s.icon] ?? MessagesSquare })))

// ---- 创建/加入弹层 ----
const showServerDialog = ref(false)

// ---- 服务器右键菜单（设为首 introduction 页为首页时普通服务器均可设首页） ----
const ctx = ref<{ x: number; y: number; id: string; name: string } | null>(null)
const ctxItems: CtxItem[] = [
  { key: 'invite', label: '复制邀请码', icon: Link },
  { key: 'new-channel', label: '新建频道', icon: FolderPlus },
  { key: 'rename', label: '重命名', icon: Pencil },
  { key: 'set-home', label: '设为首页', icon: Home },
  { key: 'delete', label: '删除服务器', icon: Trash2, danger: true, confirm: true },
]

// ---- 介绍页右键（Q1：非首页时可「设为首页」改回，或删除介绍页） ----
const introCtx = ref<{ x: number; y: number } | null>(null)
const introCtxItems = computed<CtxItem[]>(() => [
  { key: 'set-home', label: '设为首页', icon: Home },
  { key: 'hide-intro', label: '删除介绍页', icon: Trash2, danger: true, confirm: true },
])

function openCtx(e: MouseEvent, id: string, name: string) {
  ctx.value = { x: e.clientX, y: e.clientY, id, name }
}

function openIntroCtx(e: MouseEvent) {
  // 介绍页是首页时不允许删除（否则无默认落点）。
  if (store.homeId === INTRO_ID) {
    notify.show('介绍页当前是首页，请先把其他服务器设为首页', true)
    return
  }
  introCtx.value = { x: e.clientX, y: e.clientY }
}

async function onIntroSelect(key: string) {
  if (key === 'set-home') {
    // 把介绍页改回首页（换过首页后想改回来的入口，Q1）。
    try {
      await store.setHome(INTRO_ID)
      notify.show('介绍页已设为首页')
    } catch (e) { notify.fromErr(e) }
    return
  }
  if (key !== 'hide-intro') return
  try {
    await store.setIntroHidden(true)
    notify.show('介绍页已删除，可在底部恢复')
  } catch (e) { notify.fromErr(e) }
}

async function showIntro() {
  try {
    await store.setIntroHidden(false)
  } catch (e) { notify.fromErr(e) }
}

// ---- 轻量输入弹层（重命名 / 新建频道复用同一组件） ----
const prompt = ref<
  | { mode: 'rename'; id: string; initial: string }
  | { mode: 'new-channel'; id: string }
  | null
>(null)

// newChannelCats 归入分类候选（#72）：目标服务器的真实分类 + 末位「未分类」。
// 目标是右键的服务器，不一定当前选中，故按 prompt.id 取；groupsOf 的合成组
// （chan: 前缀）不是服务端分类，不能当归宿。
const newChannelCats = computed(() => {
  const p = prompt.value
  if (!p || p.mode !== 'new-channel') return []
  const s = store.findServer(p.id)
  return [
    ...store.groupsOf(p.id, s?.channels ?? [])
      .filter((g) => !g.id.startsWith('chan:'))
      .map((g) => ({ value: g.id, label: g.name })),
    { value: '', label: '未分类' },
  ]
})

// newChannelDefaultCat 本入口没有「当前选中分类」语境，默认取第一个真实分类。
const newChannelDefaultCat = computed(() =>
  newChannelCats.value.find((c) => c.value !== '')?.value ?? '')

async function onCtxSelect(key: string) {
  const c = ctx.value
  if (!c) return
  const server = store.findServer(c.id)
  if (!server) return
  switch (key) {
    case 'invite': {
      // 邀请码为服务端实体操作；内置示例服务器无对应实体。
      if (server.builtin) { notify.show('示例服务器无邀请码', true); return }
      try {
        const code = await goApp().CreateInviteCode(Number(c.id))
        await navigator.clipboard.writeText(code)
        notify.show('邀请码已复制')
      } catch (e) { notify.fromErr(e) }
      return
    }
    case 'new-channel':
      if (server.builtin) { notify.show('示例服务器不可新建频道', true); return }
      prompt.value = { mode: 'new-channel', id: c.id }
      return
    case 'rename':
      prompt.value = { mode: 'rename', id: c.id, initial: c.name }
      return
    case 'set-home':
      try {
        await store.setHome(c.id)
        notify.show(`已将「${c.name}」设为首页`)
      } catch (e) { notify.fromErr(e) }
      return
    case 'delete':
      removeServer(c.id)
      return
  }
}

async function onPromptConfirm(value: string, option: string, option2: string) {
  const p = prompt.value
  prompt.value = null
  if (!p) return
  try {
    if (p.mode === 'rename') {
      await store.renameServer(p.id, value)
      notify.show('已重命名')
    } else {
      // option 是类型、option2 是归入分类 id（'' = 不归类，#72）。
      await store.createChannel(p.id, value, option === 'voice' ? 'voice' : 'text', option2)
      notify.show('频道已创建')
    }
  } catch (e) { notify.fromErr(e) }
}

async function removeServer(id: string) {
  try {
    await store.removeServer(id)
    notify.show('服务器已删除')
  } catch (e) { notify.fromErr(e) }
}
</script>

<template>
  <nav class="rail" data-ctx>
    <!-- 介绍页入口（v0.1.2 首页体系；被删除时隐藏，底部可恢复） -->
    <div v-if="!store.introHidden" class="rail-item intro-item"
         :class="{ on: store.currentServerId === INTRO_ID }" data-tip="介绍页">
      <button class="ri-btn intro-btn" @click="store.selectServer(INTRO_ID)"
              @contextmenu.prevent="openIntroCtx">
        <Home :size="21" />
      </button>
    </div>
    <div class="rail-sep" />

    <div v-for="s in currentIcons" :key="s.id" class="rail-item"
         :class="{ on: s.id === store.currentServerId }" :data-tip="s.name">
      <button class="ri-btn" @click="store.selectServer(s.id)"
              @contextmenu.prevent="openCtx($event, s.id, s.name)">
        <component :is="s.iconComp" :size="21" />
      </button>
    </div>
    <div class="spacer" />
    <!-- 介绍页被删除后的恢复入口 -->
    <button v-if="store.introHidden" class="rail-add rail-restore" title="显示介绍页"
            @click="showIntro">
      <Undo2 :size="18" />
    </button>
    <button class="rail-add" title="创建或加入服务器" @click="showServerDialog = true">
      <Plus :size="20" />
    </button>

    <ServerDialog v-if="showServerDialog" @close="showServerDialog = false" />

    <ContextMenu v-if="ctx" :x="ctx.x" :y="ctx.y" :items="ctxItems"
                 @select="onCtxSelect" @close="ctx = null" />
    <ContextMenu v-if="introCtx" :x="introCtx.x" :y="introCtx.y" :items="introCtxItems"
                 @select="onIntroSelect" @close="introCtx = null" />

    <PromptDialog v-if="prompt?.mode === 'rename'" title="重命名服务器"
                  :initial="(prompt as any).initial" cta="保存"
                  @confirm="onPromptConfirm" @close="prompt = null" />
    <PromptDialog v-else-if="prompt?.mode === 'new-channel'" title="新建频道"
                  placeholder="频道名称" cta="创建"
                  :options="[{ value: 'voice', label: '语音频道' }, { value: 'text', label: '文字频道' }]"
                  option-default="voice"
                  :options2="newChannelCats" :option2-default="newChannelDefaultCat"
                  options2-label="归入分类"
                  @confirm="onPromptConfirm" @close="prompt = null" />
  </nav>
</template>

<style scoped>
.rail {
  width: 72px; flex: none;
  display: flex; flex-direction: column; align-items: center;
  padding: 12px 0; gap: 10px;
  background: linear-gradient(180deg, var(--brand-600), var(--brand-700));
}
.rail-item { position: relative; width: 46px; height: 46px; flex: none; }
.ri-btn {
  width: 46px; height: 46px; border-radius: 16px;
  display: flex; align-items: center; justify-content: center;
  background: rgba(255, 255, 255, 0.16); color: #fff;
  transition: border-radius 0.25s var(--ease), background 0.25s var(--ease);
}
.rail-item:hover .ri-btn {
  border-radius: 13px; background: rgba(255, 255, 255, 0.28);
}
.rail-item.on .ri-btn {
  border-radius: 13px; background: #fff; color: var(--brand-600);
}
.rail-item::before {
  content: ""; position: absolute; left: -13px; top: 50%;
  transform: translateY(-50%) scaleY(0);
  width: 4px; height: 22px; border-radius: 2px; background: #fff;
  transition: transform 0.25s var(--ease);
}
.rail-item.on::before { transform: translateY(-50%) scaleY(1); }
/* 介绍页入口与服务器的分隔线 */
.rail-sep {
  width: 30px; height: 2px; flex: none;
  border-radius: 1px; background: rgba(255, 255, 255, 0.25);
}
.rail-item[data-tip]::after {
  content: attr(data-tip);
  position: absolute; left: 60px; top: 50%;
  transform: translateY(-50%) translateX(-4px);
  padding: 6px 11px; border-radius: 8px; white-space: nowrap;
  background: var(--ink-900); color: #fff;
  font-size: 12px; font-weight: 500;
  opacity: 0; pointer-events: none;
  transition: opacity 0.2s var(--ease), transform 0.2s var(--ease);
  z-index: 40; box-shadow: var(--shadow-2);
}
.rail-item[data-tip]:hover::after {
  opacity: 1; transform: translateY(-50%) translateX(0);
}
.spacer { flex: 1; }
.rail-add {
  width: 46px; height: 46px; border-radius: 16px;
  border: 1.5px dashed rgba(255, 255, 255, 0.45);
  display: flex; align-items: center; justify-content: center;
  color: rgba(255, 255, 255, 0.8);
  transition: border-radius 0.25s var(--ease), background 0.2s, color 0.2s;
}
.rail-add:hover {
  border-radius: 13px; background: rgba(255, 255, 255, 0.14); color: #fff;
}
/* 介绍页恢复按钮与加号同风格，实底区分 */
.rail-restore { border-style: solid; }
</style>
