<script setup lang="ts">
// ServerDialog 创建/加入服务器弹层（服务器栏底部 + 按钮触发）。
// 创建：名称 2–32 字符；加入：粘贴邀请码。成功即选中并关闭，失败走全局通知。
import { ref } from 'vue'
import { Plus, Ticket, X } from 'lucide-vue-next'
import { useServersStore } from '../stores/data'
import { useConversationsStore } from '../stores/conversations'
import { useNotifyStore } from '../stores/notify'

const emit = defineEmits<{ (e: 'close'): void }>()
// initialTab/initialCode 邀请深链预填（#50，web/deeplink.ts）：/join/<code>
// 打开时宿主把加入页签与码带进来，确认仍由用户在本弹层完成。
const props = defineProps<{ initialTab?: 'create' | 'join'; initialCode?: string }>()
const servers = useServersStore()
const conversations = useConversationsStore()
const notify = useNotifyStore()

const tab = ref<'create' | 'join'>(props.initialTab ?? 'create')
const name = ref('')
const code = ref(props.initialCode ?? '')
const busy = ref(false)

async function submit() {
  if (busy.value) return
  busy.value = true
  try {
    if (tab.value === 'create') {
      const v = name.value.trim()
      if (v.length < 2) { notify.show('名称至少 2 个字符', true); return }
      await servers.createServer(v)
      notify.show('服务器已创建')
    } else {
      const v = code.value.trim()
      if (!v) { notify.show('请输入邀请码', true); return }
      await servers.joinServer(v)
      notify.show('已加入服务器')
    }
    // 创建/加入后已选中对应服务器；退出私信模式，否则新服务器在舞台不可见。
    conversations.exitDM()
    emit('close')
  } catch (e) {
    notify.fromErr(e)
  } finally {
    busy.value = false
  }
}
</script>

<template>
  <div class="sd-overlay" @click.self="emit('close')">
    <div class="sd-card" role="dialog" aria-label="创建或加入服务器">
      <div class="sd-head">
        <b>创建或加入服务器</b>
        <button class="sd-close" title="关闭" @click="emit('close')"><X :size="16" /></button>
      </div>

      <div class="sd-tabs">
        <button :class="{ on: tab === 'create' }" @click="tab = 'create'">
          <Plus :size="14" />创建服务器
        </button>
        <button :class="{ on: tab === 'join' }" @click="tab = 'join'">
          <Ticket :size="14" />加入服务器
        </button>
      </div>

      <template v-if="tab === 'create'">
        <p class="sd-sub">给你的朋友一个聚集地。名称可随时修改。</p>
        <input v-model="name" class="sd-input" placeholder="服务器名称（2–32 个字符）"
               maxlength="32" autofocus @keydown.enter.prevent="submit">
      </template>
      <template v-else>
        <p class="sd-sub">粘贴朋友分享给你的邀请码。</p>
        <input v-model="code" class="sd-input" placeholder="邀请码"
               maxlength="32" autofocus @keydown.enter.prevent="submit">
      </template>

      <div class="sd-acts">
        <button class="sd-cancel" @click="emit('close')">取消</button>
        <button class="sd-ok" :disabled="busy" @click="submit">
          {{ tab === 'create' ? '创建' : '加入' }}
        </button>
      </div>
    </div>
  </div>
</template>

<style scoped>
.sd-overlay {
  position: fixed; inset: 0; z-index: 70;
  background: rgba(23, 50, 75, 0.28);
  display: flex; align-items: center; justify-content: center;
}
.sd-card {
  width: 400px; max-width: calc(100vw - 48px);
  background: var(--surface); border-radius: 16px;
  box-shadow: var(--shadow-3); padding: 20px 22px 22px;
}
.sd-head { display: flex; align-items: center; justify-content: space-between; margin-bottom: 14px; }
.sd-head b { font-size: 15px; }
.sd-close {
  width: 28px; height: 28px; border-radius: 8px;
  display: flex; align-items: center; justify-content: center;
  color: var(--ink-400); transition: background 0.15s, color 0.15s;
}
.sd-close:hover { background: var(--brand-50); color: var(--ink-900); }
.sd-tabs {
  display: flex; gap: 2px; padding: 3px;
  background: var(--brand-50); border-radius: 10px; margin-bottom: 14px;
}
.sd-tabs button {
  flex: 1; height: 32px; border-radius: 8px;
  display: flex; align-items: center; justify-content: center; gap: 6px;
  font-size: 13px; font-weight: 600; color: var(--ink-400);
  transition: all 0.2s var(--ease);
}
.sd-tabs button.on {
  background: var(--surface); color: var(--brand-700); box-shadow: var(--shadow-1);
}
.sd-sub { font-size: 12.5px; color: var(--ink-400); margin: 0 0 10px; }
.sd-input {
  width: 100%; height: 40px; padding: 0 12px;
  background: var(--brand-50); border: 1.5px solid var(--brand-200);
  border-radius: 10px; font-size: 14px; color: var(--ink-900);
  outline: none; transition: border-color 0.2s, box-shadow 0.2s;
}
.sd-input:focus { border-color: var(--brand-500); box-shadow: 0 0 0 3px rgba(79, 164, 243, 0.15); }
.sd-acts { display: flex; justify-content: flex-end; gap: 10px; margin-top: 18px; }
.sd-cancel {
  height: 36px; padding: 0 16px; border-radius: 10px;
  font-size: 13px; font-weight: 500; color: var(--ink-600);
  transition: background 0.15s;
}
.sd-cancel:hover { background: var(--brand-50); }
.sd-ok {
  height: 36px; padding: 0 18px; border-radius: 10px;
  background: var(--brand-500); color: #fff;
  font-size: 13px; font-weight: 600; transition: background 0.2s;
}
.sd-ok:hover:not(:disabled) { background: var(--brand-600); }
.sd-ok:disabled { opacity: 0.5; cursor: default; }
</style>
