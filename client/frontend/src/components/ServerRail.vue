<script setup lang="ts">
// ServerRail 服务器轨道（对齐 Main.html 左栏：图标按钮+未读角标+悬浮提示）。
// 图标为预设 Lucide（禁头像/上传图——硬性约束）。
// 批次①：底部 + 打开创建/加入弹层；右键弹出管理菜单（邀请码/新建频道/重命名/删除）。
import { computed, ref } from 'vue'
import {
  Plus, MessagesSquare, Gamepad2, Music, Palette, BookOpen, Coffee, Mic,
  GraduationCap, Heart, Rocket, Camera, Dumbbell,
  Link, FolderPlus, Pencil, Trash2,
} from 'lucide-vue-next'
import { useServersStore } from '../stores/data'
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
const badges: Record<string, number> = { game: 3 }

// ---- 创建/加入弹层 ----
const showServerDialog = ref(false)

// ---- 右键菜单 ----
const ctx = ref<{ x: number; y: number; id: string; name: string } | null>(null)
const ctxItems: CtxItem[] = [
  { key: 'invite', label: '复制邀请码', icon: Link },
  { key: 'new-channel', label: '新建频道', icon: FolderPlus },
  { key: 'rename', label: '重命名', icon: Pencil },
  { key: 'delete', label: '删除服务器', icon: Trash2, danger: true, confirm: true },
]

function openCtx(e: MouseEvent, id: string, name: string) {
  ctx.value = { x: e.clientX, y: e.clientY, id, name }
}

// ---- 轻量输入弹层（重命名 / 新建频道复用同一组件） ----
const prompt = ref<
  | { mode: 'rename'; id: string; initial: string }
  | { mode: 'new-channel'; id: string }
  | null
>(null)

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
    case 'delete':
      removeServer(c.id)
      return
  }
}

async function onPromptConfirm(value: string, option: string) {
  const p = prompt.value
  prompt.value = null
  if (!p) return
  try {
    if (p.mode === 'rename') {
      await store.renameServer(p.id, value)
      notify.show('已重命名')
    } else {
      await store.createChannel(p.id, value, option === 'voice' ? 'voice' : 'text')
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
  <nav class="rail">
    <div v-for="s in currentIcons" :key="s.id" class="rail-item"
         :class="{ on: s.id === store.currentServerId }" :data-tip="s.name">
      <button class="ri-btn" @click="store.selectServer(s.id)"
              @contextmenu.prevent="openCtx($event, s.id, s.name)">
        <component :is="s.iconComp" :size="21" />
      </button>
      <span v-if="badges[s.id]" class="rail-badge">{{ badges[s.id] }}</span>
    </div>
    <div class="spacer" />
    <button class="rail-add" title="创建或加入服务器" @click="showServerDialog = true">
      <Plus :size="20" />
    </button>

    <ServerDialog v-if="showServerDialog" @close="showServerDialog = false" />

    <ContextMenu v-if="ctx" :x="ctx.x" :y="ctx.y" :items="ctxItems"
                 @select="onCtxSelect" @close="ctx = null" />

    <PromptDialog v-if="prompt?.mode === 'rename'" title="重命名服务器"
                  :initial="(prompt as any).initial" cta="保存"
                  @confirm="onPromptConfirm" @close="prompt = null" />
    <PromptDialog v-else-if="prompt?.mode === 'new-channel'" title="新建频道"
                  placeholder="频道名称" cta="创建"
                  :options="[{ value: 'voice', label: '语音频道' }, { value: 'text', label: '文字频道' }]"
                  option-default="voice"
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
.rail-badge {
  position: absolute; right: -2px; bottom: -2px;
  min-width: 17px; height: 17px; padding: 0 4px;
  border-radius: 9px; background: var(--red); color: #fff;
  font-size: 10px; font-weight: 700;
  display: flex; align-items: center; justify-content: center;
  border: 2px solid var(--brand-700);
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
</style>
