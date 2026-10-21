<script setup lang="ts">
// ChannelSettingsDialog 频道设置（#37 B2）：密码（留空=不改 / 移除）、人数上限、
// 父频道。不回显现有密码；保存只发送变更字段（store 侧同约定）。校验以服务端为准。
import { computed, ref } from 'vue'
import { X } from 'lucide-vue-next'
import { useServersStore } from '../stores/data'
import { useNotifyStore } from '../stores/notify'

const props = defineProps<{ channelId: string }>()
const emit = defineEmits<{ (e: 'close'): void }>()
const servers = useServersStore()
const notify = useNotifyStore()

const ch = computed(() => servers.currentServer.channels.find((c) => c.id === props.channelId))

// parentChoices 父频道候选：同分类同类型的顶层频道，排除自己。
const parentChoices = computed(() => {
  const cur = ch.value
  if (!cur) return []
  return servers.currentServer.channels.filter((c) =>
    c.id !== cur.id && c.type === cur.type && c.group === cur.group && !c.parentId)
})

// hasChildren 自己已有子频道时禁改父级（服务端 409 的前置提示）。
const hasChildren = computed(() =>
  servers.currentServer.channels.some((c) => c.parentId === props.channelId))

const password = ref('')
const clearPassword = ref(false)
const limit = ref(ch.value?.userLimit ?? 0)
const parent = ref(ch.value?.parentId ?? '0')

function onRemovePassword() {
  clearPassword.value = true
  password.value = ''
}

async function submit() {
  const cur = ch.value
  if (!cur) return
  if (limit.value < 0 || limit.value > 100) {
    notify.show('人数上限须为 0–100', true)
    return
  }
  try {
    // parent：'0' = 提升顶层；其余 = 设置父（空串在 store 侧=不改，这里总是有值）。
    await servers.updateChannelOptions(servers.currentServerId, cur.id, parent.value, limit.value, password.value, clearPassword.value)
    notify.show('频道设置已保存')
    emit('close')
  } catch (e) {
    notify.fromErr(e)
  }
}
</script>

<template>
  <div class="cs-overlay" @click.self="emit('close')" @keydown.esc="emit('close')">
    <div class="cs-card" role="dialog" :aria-label="`频道设置：${ch?.name ?? ''}`">
      <div class="cs-head">
        <b>频道设置</b>
        <button class="cs-close" title="关闭" @click="emit('close')"><X :size="16" /></button>
      </div>

      <label class="cs-label">密码</label>
      <!-- 不预填现有密码：留空 = 不改；有密码时给「移除」入口 -->
      <div class="cs-row">
        <input v-model="password" class="cs-input" type="password" autocomplete="off"
               maxlength="72" :disabled="clearPassword" placeholder="留空不修改">
        <button v-if="ch?.hasPassword && !clearPassword" class="cs-remove" @click="onRemovePassword">移除密码</button>
        <button v-else-if="clearPassword" class="cs-remove" @click="clearPassword = false; password = ''">撤销移除</button>
      </div>

      <label class="cs-label">人数上限</label>
      <input v-model.number="limit" class="cs-input" type="number" min="0" max="100"
             :disabled="ch?.type !== 'voice'" title="0 = 不限">
      <p class="cs-hint">0 = 不限（仅语音频道生效）</p>

      <label class="cs-label">父频道</label>
      <select v-model="parent" class="cs-input" :disabled="hasChildren || !parentChoices.length">
        <option value="0">（顶层频道）</option>
        <option v-for="p in parentChoices" :key="p.id" :value="p.id">{{ p.name }}</option>
      </select>
      <p v-if="hasChildren" class="cs-hint">已有子频道的频道不能挂到别的频道下</p>

      <div class="cs-acts">
        <button class="cs-cancel" @click="emit('close')">取消</button>
        <button class="cs-ok" @click="submit">保存</button>
      </div>
    </div>
  </div>
</template>

<style scoped>
.cs-overlay {
  position: fixed; inset: 0; z-index: 70;
  background: rgba(23, 50, 75, 0.28);
  display: flex; align-items: center; justify-content: center;
}
.cs-card {
  width: 380px; max-width: calc(100vw - 48px);
  background: var(--surface); border-radius: 16px;
  box-shadow: var(--shadow-3); padding: 20px 22px 22px;
}
.cs-head { display: flex; align-items: center; justify-content: space-between; margin-bottom: 14px; }
.cs-head b { font-size: 15px; }
.cs-close {
  width: 28px; height: 28px; border-radius: 8px;
  display: flex; align-items: center; justify-content: center;
  color: var(--ink-400); transition: background 0.15s, color 0.15s;
}
.cs-close:hover { background: var(--brand-50); color: var(--ink-900); }
.cs-label {
  display: block; font-size: 11.5px; letter-spacing: 0.4px;
  color: var(--ink-400); margin: 12px 0 6px;
}
.cs-row { display: flex; gap: 8px; }
.cs-input {
  width: 100%; height: 38px; padding: 0 12px;
  background: var(--brand-50); border: 1.5px solid var(--brand-200);
  border-radius: 10px; font-size: 13.5px; color: var(--ink-900);
  outline: none; transition: border-color 0.2s, box-shadow 0.2s;
}
.cs-input:focus { border-color: var(--brand-500); box-shadow: 0 0 0 3px rgba(79, 164, 243, 0.15); }
.cs-input:disabled { opacity: 0.5; }
select.cs-input { appearance: auto; }
.cs-remove {
  flex: 0 0 auto; height: 38px; padding: 0 12px; border-radius: 10px;
  font-size: 12.5px; font-weight: 500; color: var(--ink-600);
  background: var(--brand-50); transition: background 0.15s;
  white-space: nowrap;
}
.cs-remove:hover { background: var(--brand-100); }
.cs-hint { margin: 6px 0 0; font-size: 11.5px; color: var(--ink-400); }
.cs-acts { display: flex; justify-content: flex-end; gap: 10px; margin-top: 18px; }
.cs-cancel {
  height: 36px; padding: 0 16px; border-radius: 10px;
  font-size: 13px; font-weight: 500; color: var(--ink-600);
  transition: background 0.15s;
}
.cs-cancel:hover { background: var(--brand-50); }
.cs-ok {
  height: 36px; padding: 0 18px; border-radius: 10px;
  background: var(--brand-500); color: #fff;
  font-size: 13px; font-weight: 600;
  transition: background 0.2s;
}
.cs-ok:hover { background: var(--brand-600); }
</style>
