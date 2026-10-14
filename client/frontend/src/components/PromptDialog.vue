<script setup lang="ts">
// PromptDialog 单输入对话框：重命名 / 新建频道 / 添加好友等轻量输入场景。
// 父组件用 v-if 挂载（每次打开重新挂载，initial 即初值）；Esc / 点遮罩关闭。
// 可选 options 单选行（如频道类型 文字/语音）。
import { ref } from 'vue'
import { X } from 'lucide-vue-next'

const props = withDefaults(defineProps<{
  title: string
  placeholder?: string
  cta?: string
  initial?: string
  options?: { value: string; label: string }[]
  optionDefault?: string
}>(), { placeholder: '', cta: '确定', initial: '' })

const emit = defineEmits<{
  (e: 'confirm', value: string, option: string): void
  (e: 'close'): void
}>()

const value = ref(props.initial)
const option = ref(props.optionDefault ?? props.options?.[0]?.value ?? '')

function submit() {
  const v = value.value.trim()
  if (!v) return
  emit('confirm', v, option.value)
}
</script>

<template>
  <div class="pd-overlay" @click.self="emit('close')"
       @keydown.esc="emit('close')">
    <div class="pd-card" role="dialog" :aria-label="title">
      <div class="pd-head">
        <b>{{ title }}</b>
        <button class="pd-close" title="关闭" @click="emit('close')"><X :size="16" /></button>
      </div>
      <input ref="inputEl" v-model="value" class="pd-input" :placeholder="placeholder"
             maxlength="64" autofocus @keydown.enter.prevent="submit">
      <div v-if="options?.length" class="pd-opts">
        <button v-for="o in options" :key="o.value" type="button"
                :class="{ on: option === o.value }" @click="option = o.value">
          {{ o.label }}
        </button>
      </div>
      <div class="pd-acts">
        <button class="pd-cancel" @click="emit('close')">取消</button>
        <button class="pd-ok" :disabled="!value.trim()" @click="submit">{{ cta }}</button>
      </div>
    </div>
  </div>
</template>

<style scoped>
.pd-overlay {
  position: fixed; inset: 0; z-index: 70;
  background: rgba(23, 50, 75, 0.28);
  display: flex; align-items: center; justify-content: center;
}
.pd-card {
  width: 380px; max-width: calc(100vw - 48px);
  background: var(--surface); border-radius: 16px;
  box-shadow: var(--shadow-3); padding: 20px 22px 22px;
}
.pd-head { display: flex; align-items: center; justify-content: space-between; margin-bottom: 14px; }
.pd-head b { font-size: 15px; }
.pd-close {
  width: 28px; height: 28px; border-radius: 8px;
  display: flex; align-items: center; justify-content: center;
  color: var(--ink-400); transition: background 0.15s, color 0.15s;
}
.pd-close:hover { background: var(--brand-50); color: var(--ink-900); }
.pd-input {
  width: 100%; height: 40px; padding: 0 12px;
  background: var(--brand-50); border: 1.5px solid var(--brand-200);
  border-radius: 10px; font-size: 14px; color: var(--ink-900);
  outline: none; transition: border-color 0.2s, box-shadow 0.2s;
}
.pd-input:focus { border-color: var(--brand-500); box-shadow: 0 0 0 3px rgba(79, 164, 243, 0.15); }
.pd-opts { display: flex; gap: 8px; margin-top: 12px; }
.pd-opts button {
  flex: 1; height: 34px; border-radius: 9px;
  font-size: 13px; font-weight: 500; color: var(--ink-400);
  background: var(--brand-50); transition: all 0.15s;
}
.pd-opts button.on {
  background: rgba(79, 164, 243, 0.12); color: var(--brand-700); font-weight: 600;
}
.pd-acts { display: flex; justify-content: flex-end; gap: 10px; margin-top: 18px; }
.pd-cancel {
  height: 36px; padding: 0 16px; border-radius: 10px;
  font-size: 13px; font-weight: 500; color: var(--ink-600);
  transition: background 0.15s;
}
.pd-cancel:hover { background: var(--brand-50); }
.pd-ok {
  height: 36px; padding: 0 18px; border-radius: 10px;
  background: var(--brand-500); color: #fff;
  font-size: 13px; font-weight: 600;
  transition: background 0.2s;
}
.pd-ok:hover:not(:disabled) { background: var(--brand-600); }
.pd-ok:disabled { opacity: 0.5; cursor: default; }
</style>
