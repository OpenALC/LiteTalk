<script setup lang="ts">
// SoundOrb 声波球：无头像身份视觉（硬性约束：禁头像）。
// 身份色 = hash(user_id) % 4 取品牌邻近色；状态四态：
// 正常（波形条）/ 说话（光环+律动）/ 单关（mic-off 或 headphone-off）/
// 双关（灰球+双图标+红色双环——用户明确要求的合并样式）。
import { computed } from 'vue'
import { MicOff, HeadphoneOff } from 'lucide-vue-next'

const props = withDefaults(defineProps<{
  userId: string
  speaking?: boolean
  muted?: boolean
  deafen?: boolean
  size?: number
}>(), { speaking: false, muted: false, deafen: false, size: 76 })

const ID_COLORS = ['#4FA4F3', '#3DBBC4', '#7C8CF8', '#3FB57F']

function hashId(id: string): number {
  let h = 0
  for (let i = 0; i < id.length; i++) h = (h * 31 + id.charCodeAt(i)) | 0
  return Math.abs(h) % 4
}

const idColor = computed(() => ID_COLORS[hashId(props.userId)])
const dualOff = computed(() => props.muted && props.deafen)
const singleOff = computed(() => !dualOff.value && (props.muted || props.deafen))
const active = computed(() => props.speaking && !props.muted && !props.deafen)
</script>

<template>
  <div
    class="orb" :class="{ 'dual-off': dualOff, 'state-icon': singleOff, speaking: active }"
    :style="{ '--idc': idColor, width: size + 'px', height: size + 'px' }"
  >
    <template v-if="dualOff">
      <div class="dual-icons">
        <MicOff :size="21" />
        <HeadphoneOff :size="21" />
      </div>
    </template>
    <template v-else-if="muted">
      <MicOff :size="26" />
    </template>
    <template v-else-if="deafen">
      <HeadphoneOff :size="26" />
    </template>
    <template v-else>
      <div class="orb-bars">
        <i v-for="i in 5" :key="i" :style="{ '--i': i - 1 }" />
      </div>
    </template>
  </div>
</template>

<style scoped>
.orb {
  border-radius: 50%;
  display: flex;
  align-items: center;
  justify-content: center;
  background: color-mix(in srgb, var(--idc) 10%, #fff);
  transition: box-shadow 0.3s var(--ease);
}
.orb-bars { display: flex; align-items: center; gap: 3.5px; height: 26px; }
.orb-bars i {
  width: 4px; height: 24px; border-radius: 2px;
  background: var(--idc);
  transform: scaleY(0.333333);
}
.orb.speaking {
  box-shadow: 0 0 0 2px var(--idc), 0 0 26px color-mix(in srgb, var(--idc) 25%, transparent);
}
.orb.speaking .orb-bars i {
  animation: wave 1.05s ease-in-out infinite;
  animation-delay: calc(var(--i) * 0.13s);
}
@keyframes wave {
  0%, 100% { transform: scaleY(0.333333); }
  50% { transform: scaleY(1); }
}
.orb.state-icon :deep(svg), .orb.dual-off :deep(svg) { color: var(--ink-400); }
/* 双关态：灰球 + 双图标 + 红色双环（区别于单关） */
.orb.dual-off {
  background: rgba(126, 151, 174, 0.12);
  box-shadow: 0 0 0 2px rgba(232, 91, 91, 0.45), 0 0 0 5px rgba(232, 91, 91, 0.12);
}
.dual-icons { display: flex; align-items: center; gap: 5px; }
.dual-icons :deep(svg) { color: var(--red) !important; }
</style>
