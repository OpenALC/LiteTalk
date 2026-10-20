<script setup lang="ts">
// ServerConnPanel 服务器地址配置面板（设置页「连接」组与登录页弹层共用）。
// 地址输入 + 来源徽标 + 测试连接（healthz 探活）+ 保存（热切换即时生效）。
import { onMounted, ref } from 'vue'
import { Server, Wifi, RefreshCw } from 'lucide-vue-next'
import { goApp } from '../api/bridge'
import type { ConnConfigView } from '../api/types'
import { cleanErrorText } from '../utils/errorText'

const props = withDefaults(defineProps<{ autoTest?: boolean }>(), { autoTest: false })
const emit = defineEmits<{ (e: 'saved'): void; (e: 'unreachable'): void }>()

const conn = ref<ConnConfigView>({ base_url: '', ws_url: '', source: 'default' })
const serverInput = ref('')
const testing = ref(false)
const testResult = ref('')
const testOk = ref(false)
const savedHint = ref(false)

const SOURCE_LABEL: Record<string, string> = { env: '环境变量', file: '配置文件', default: '默认' }

onMounted(async () => {
  conn.value = await goApp().GetServerConfig()
  serverInput.value = conn.value.base_url
  if (props.autoTest) {
    // 首启动引导：默认地址连不上时通知外层展开引导
    const r = await goApp().TestServerConnection(conn.value.base_url)
    if (!r.ok) emit('unreachable')
  }
})

// 测试连接：对当前输入框地址（可未保存）做 healthz 探活。
async function testConn() {
  testing.value = true
  testResult.value = ''
  savedHint.value = false
  try {
    const r = await goApp().TestServerConnection(serverInput.value.trim())
    testOk.value = r.ok
    testResult.value = r.ok ? `${r.latency_ms}ms · ${r.version || 'ok'}` : (r.error || '连接失败')
  } finally {
    testing.value = false
  }
}

// 保存：探活通过后落盘并热切换（REST 重定向 + 信令重建），即时生效。
async function saveConn() {
  try {
    await goApp().SetServerConfig(serverInput.value.trim())
    conn.value = await goApp().GetServerConfig()
    testResult.value = '' // 清掉上次测试结果，让「已保存」提示露出来
    savedHint.value = true
    emit('saved')
  } catch (e) {
    // Go 侧失败会带原因（如「切换失败，已恢复原地址: …」），清洗机器前缀后展示，
    // 不吞成固定文案（#15）。
    savedHint.value = false
    testOk.value = false
    testResult.value = cleanErrorText(e, '地址无效')
  }
}
</script>

<template>
  <div class="scp">
    <div class="scp-row">
      <Server :size="16" class="scp-icon" />
      <input
        v-model="serverInput"
        class="scp-input"
        :readonly="conn.source === 'env'"
        placeholder="https://your-domain.com"
      >
      <span class="scp-badge">{{ SOURCE_LABEL[conn.source] }}</span>
    </div>
    <div v-if="conn.source === 'env'" class="scp-hint">由环境变量指定</div>
    <div v-else class="scp-hint">当前：{{ conn.ws_url }}</div>
    <div class="scp-row">
      <button class="scp-btn" :disabled="testing" @click="testConn">
        <RefreshCw :size="14" class="scp-btn-icon" :class="{ spin: testing }" />测试连接
      </button>
      <button class="scp-btn primary" @click="saveConn">保存</button>
      <span v-if="testResult" class="scp-result" :class="{ ok: testOk }">
        <Wifi :size="13" class="scp-btn-icon" />{{ testResult }}
      </span>
      <span v-else-if="savedHint" class="scp-result ok">已保存</span>
    </div>
  </div>
</template>

<style scoped>
.scp { display: flex; flex-direction: column; }
.scp-row { display: flex; align-items: center; gap: 10px; }
.scp-icon { color: var(--ink-400); flex: none; }
.scp-input {
  flex: 1; min-width: 0; height: 32px; border-radius: 8px;
  border: 1.5px solid var(--brand-200); padding: 0 10px;
  font-size: 13px; color: var(--ink-900); background: var(--surface);
  outline: none; transition: border-color 0.2s var(--ease), box-shadow 0.2s var(--ease);
}
.scp-input:focus { border-color: var(--brand-500); box-shadow: 0 0 0 3px rgba(79, 164, 243, 0.15); }
.scp-input[readonly] { background: var(--brand-50); color: var(--ink-400); }
.scp-badge {
  flex: none; font-size: 11px; font-weight: 600;
  padding: 2px 8px; border-radius: 99px;
  background: var(--brand-50); color: var(--ink-400);
}
.scp-hint { font-size: 11.5px; color: var(--ink-400); margin: 2px 0 8px 26px; }
.scp-btn {
  display: inline-flex; align-items: center; gap: 6px;
  height: 32px; padding: 0 14px; border-radius: 8px;
  font-size: 13px; font-weight: 600;
  background: var(--brand-50); color: var(--ink-600);
  transition: background 0.2s var(--ease), color 0.2s var(--ease);
}
.scp-btn:hover:not(:disabled) { background: var(--brand-200); color: var(--ink-900); }
.scp-btn:disabled { opacity: 0.6; cursor: not-allowed; }
.scp-btn.primary { background: var(--brand-500); color: #fff; }
.scp-btn.primary:hover { background: var(--brand-600); color: #fff; }
.scp-btn-icon { flex: none; }
.spin { animation: spin 0.8s linear infinite; }
@keyframes spin { to { transform: rotate(360deg); } }
.scp-result {
  display: inline-flex; align-items: center; gap: 5px;
  font-size: 12px; color: var(--red); margin-left: auto;
}
.scp-result.ok { color: var(--green); }
</style>
