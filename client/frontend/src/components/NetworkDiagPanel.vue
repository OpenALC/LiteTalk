<script setup lang="ts">
// NetworkDiagPanel 一键网络诊断（#49 C）：REST / 信令 WS / STUN / TURN 四项
// 独立探测，结果用紧凑列表展示。运行中禁用按钮，失败只做提示不阻塞页面。
import { computed, ref } from 'vue'
import { Activity, CircleCheck, CircleX, Minus } from 'lucide-vue-next'
import { goApp } from '../api/bridge'
import type { NetworkDiagProbe, NetworkDiagResult } from '../api/types'
import { useNotifyStore } from '../stores/notify'

const notify = useNotifyStore()
const running = ref(false)
const result = ref<NetworkDiagResult | null>(null)

const ROWS = [
  { key: 'rest', label: 'REST' },
  { key: 'ws', label: '信令' },
  { key: 'stun', label: 'STUN' },
  { key: 'turn', label: 'TURN' },
] as const

// rows 预先把结果映射到行，模板里不用再写非空断言。
const rows = computed(() => {
  const r = result.value
  if (!r) return []
  return ROWS.map((row) => ({ ...row, probe: r[row.key] as NetworkDiagProbe }))
})

// detail 一行摘要：跳过显示原因；失败显示脱敏错误；成功依次给耗时、
// 版本/映射地址/relay 地址与补充说明。多候选回退成功时note 已说明第几个
// 候选可用，这里再补上地址本身（#117）。
function detail(p: NetworkDiagProbe): string {
  if (p.skipped) return p.note || '未配置'
  if (!p.ok) return p.error || '失败'
  // 未实际拨号（如信令已连接）时没有耗时，只显示说明。
  if (p.note && p.latency_ms === 0) return p.note
  const parts = [`${p.latency_ms}ms`]
  if (p.version) parts.push(p.version)
  if (p.address) parts.push(p.address)
  if (p.candidate) parts.push(p.candidate)
  if (p.note) parts.push(p.note)
  return parts.join(' · ')
}

function iconFor(p: NetworkDiagProbe) {
  if (p.skipped) return Minus
  return p.ok ? CircleCheck : CircleX
}

function stateOf(p: NetworkDiagProbe): string {
  if (p.skipped) return 'skip'
  return p.ok ? 'ok' : 'bad'
}

async function run() {
  if (running.value) return
  running.value = true
  try {
    const r = await goApp().NetworkDiag()
    result.value = r
    if (!r.ok) {
      const failed = ROWS.map((row) => r[row.key]).filter((p) => !p.ok && !p.skipped)
      notify.show(failed.length ? `网络诊断：${failed.length} 项异常` : '网络诊断发现问题', true)
    }
  } catch (e) {
    notify.fromErr(e, '网络诊断失败')
  } finally {
    running.value = false
  }
}
</script>

<template>
  <div class="ndp">
    <div class="ndp-head">
      <button class="ndp-btn" :disabled="running" @click="run">
        <Activity :size="14" :class="{ spin: running }" />
        {{ running ? '诊断中…' : '网络诊断' }}
      </button>
      <span v-if="result" class="ndp-sum" :class="{ bad: !result.ok }">
        <component :is="result.ok ? CircleCheck : CircleX" :size="13" />
        {{ result.ok ? '连接正常' : '存在问题' }}
      </span>
    </div>
    <div v-if="result" class="ndp-list">
      <div v-for="row in rows" :key="row.key" class="ndp-row">
        <component :is="iconFor(row.probe)" :size="13"
                   class="ndp-ic" :class="stateOf(row.probe)" />
        <span class="ndp-label">{{ row.label }}</span>
        <span class="ndp-val" :class="stateOf(row.probe)">{{ detail(row.probe) }}</span>
      </div>
    </div>
    <p v-if="running" class="ndp-note">诊断会向服务端与 ICE 服务器各发一次探测请求</p>
  </div>
</template>

<style scoped>
.ndp { display: flex; flex-direction: column; gap: 8px; margin-top: 10px; }
.ndp-head { display: flex; align-items: center; gap: 10px; }
.ndp-btn {
  display: inline-flex; align-items: center; gap: 6px;
  height: 32px; padding: 0 14px; border-radius: 8px;
  font-size: 13px; font-weight: 600;
  color: var(--brand-700); background: var(--brand-50);
  transition: background 0.2s var(--ease), opacity 0.2s;
}
.ndp-btn:hover:not(:disabled) { background: var(--brand-200); }
.ndp-btn:disabled { opacity: 0.6; cursor: default; }
.spin { animation: ndp-spin 0.8s linear infinite; }
@keyframes ndp-spin { to { transform: rotate(360deg); } }
.ndp-sum {
  display: inline-flex; align-items: center; gap: 5px;
  font-size: 12px; color: var(--green);
}
.ndp-sum.bad { color: var(--red); }
.ndp-list {
  display: flex; flex-direction: column;
  border: 1px solid var(--line); border-radius: 10px;
  overflow: hidden;
}
.ndp-row {
  display: flex; align-items: center; gap: 8px;
  min-height: 32px; padding: 6px 12px;
  font-size: 12.5px;
}
.ndp-row + .ndp-row { border-top: 1px solid var(--line); }
.ndp-ic { flex: none; }
.ndp-ic.ok { color: var(--green); }
.ndp-ic.bad { color: var(--red); }
.ndp-ic.skip { color: var(--ink-300); }
.ndp-label { flex: none; width: 44px; color: var(--ink-600); font-weight: 600; }
.ndp-val {
  min-width: 0; overflow: hidden; text-overflow: ellipsis; white-space: nowrap;
  color: var(--ink-400);
}
.ndp-val.bad { color: var(--red); }
.ndp-note { margin: 0; font-size: 11px; color: var(--ink-300); }
</style>
