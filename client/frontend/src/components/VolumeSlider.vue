<script setup lang="ts">
// VolumeSlider 竖向音量滑块 popover（v0.1.2 批次③，走查 5）：
// hover 频道列表底部个人卡片的麦克风/耳机按钮时在其上方弹出，
// 0 在底部、120 在顶部；拖动过程 @input 只更新显示，松手 @change 才由父级
// 热应用 + 落盘（避免高频调 Go 侧）。
defineProps<{
  open: boolean
  value: number // 0–120
  label: string // 「麦克风音量」/「扬声器音量」，读屏与提示用
}>()
const emit = defineEmits<{
  (e: 'input', v: number): void
  (e: 'apply', v: number): void
}>()
</script>

<template>
  <div v-if="open" class="vol-pop" role="slider" :aria-label="label"
       :aria-valuenow="value" aria-valuemin="0" aria-valuemax="120">
    <span class="vol-num">{{ value }}</span>
    <!-- writing-mode 竖排：WebView2(Chromium) 支持；0 在底部由 direction: rtl 保证 -->
    <input type="range" class="vol-range" min="0" max="120" step="5"
           :value="value"
           @input="emit('input', Number(($event.target as HTMLInputElement).value))"
           @change="emit('apply', Number(($event.target as HTMLInputElement).value))">
  </div>
</template>

<style scoped>
/* bottom: 100%（与按钮命中区无缝衔接）+ pop 自带 padding-bottom 造视觉间距——
   早期 calc(100% + 8px) 的 8px 缝隙会让慢速移向滑块时中途丢 hover（review B2，
   滑块交互本质是慢速精确瞄准，缝隙不可接受）。 */
.vol-pop {
  position: absolute; bottom: 100%; left: 50%;
  transform: translateX(-50%);
  z-index: 45;
  display: flex; flex-direction: column; align-items: center; gap: 6px;
  padding: 10px 10px 18px;
  background: var(--surface); border: 1px solid var(--line);
  border-radius: 12px 12px 4px 4px; box-shadow: var(--shadow-2);
}
.vol-num {
  font-size: 11.5px; font-weight: 600; color: var(--ink-600);
  font-variant-numeric: tabular-nums;
}
/* 竖向 range：Chromium 的 writing-mode 方案；appearance 保持默认滑块样式 */
.vol-range {
  writing-mode: vertical-lr;
  direction: rtl; /* 自下而上：底=0 顶=120 */
  width: 20px; height: 104px;
  accent-color: var(--brand-500);
  cursor: pointer;
}
</style>
