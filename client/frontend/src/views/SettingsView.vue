<script setup lang="ts">
// SettingsView 设置页（独立路由 /settings，v0.1.2 起不再复用语音/文字舞台区域）。
// 分区：连接/账号/常规/音频设备/语音/快捷键/关于；改动即热应用 + 落盘 config.json。
// 语音默认值全部由 Go 侧 GetAudioSettings 回落（零值 → 默认），UI 只消费完整值。
import { onMounted, ref } from 'vue'
import { ArrowLeft, LogOut } from 'lucide-vue-next'
import { useRouter } from 'vue-router'
import { goApp, type AudioDeviceDTO } from '../api/bridge'
import { useAuthStore } from '../stores/auth'
import { useSettingsStore } from '../stores/data'
import { useHibernateStore } from '../stores/hibernate'
import { HOTKEYS } from '../composables/useHotkeys'
import ServerConnPanel from '../components/ServerConnPanel.vue'
import Toast from '../components/Toast.vue'

const router = useRouter()
const auth = useAuthStore()
const settings = useSettingsStore()
const hibernateStore = useHibernateStore()
const version = ref('dev')
const hibernate = ref(false)
const uiDestroy = ref(false)
const closeToTray = ref(true)
const caps = ref<AudioDeviceDTO[]>([])
const plays = ref<AudioDeviceDTO[]>([])
const toast = ref<InstanceType<typeof Toast>>()

// 语音设置：Go 侧返回已回落默认的完整值（码率/VBR/DTX/FEC/丢包/降噪强度/AGC/
// 麦克风/扬声器音量/VAD 五档/Opus 复杂度）。
const audio = ref({
  bitrate_kbps: 32, vbr: true, dtx: true, fec: true,
  packet_loss: 10, denoise: true, denoise_strength: 2, agc: false,
  mic_volume: 100, playback_volume: 100, vad_sens: 2, opus_complexity: 9,
})
const BITRATES = [16, 24, 32, 48, 64]
// VAD 五档（v0.1.2 扩展；旧三档由 Go 侧映射 0/2/4）。
const VAD_LABELS = ['很低', '低', '中', '高', '很高']
// 降噪强度四档：关（denoise=false）/弱/中/强（denoise_strength=1/2/3，
// Go 侧映射 rnnoise Aggressiveness 1.0/1.5/2.0）。
const DENOISE_LABELS = ['关', '弱', '中', '强']

// denoiseIdx 降噪强度当前档位（computed 双向：关↔denoise=false，其余↔strength）。
const denoiseIdx = ref(2)
function syncDenoiseIdx() {
  denoiseIdx.value = audio.value.denoise ? audio.value.denoise_strength : 0
}
function applyDenoiseIdx(i: number) {
  if (i === 0) {
    audio.value.denoise = false
  } else {
    audio.value.denoise = true
    audio.value.denoise_strength = i
  }
  void applyAudio()
}

onMounted(async () => {
  version.value = await goApp().GetVersion()
  hibernate.value = await goApp().GetHibernateOnMinimize()
  try { uiDestroy.value = await goApp().GetUIDestroyOnHibernate() } catch { /* 方法未就绪保持默认 */ }
  closeToTray.value = await goApp().GetCloseToTray()
  audio.value = await goApp().GetAudioSettings()
  syncDenoiseIdx()
  const devs = await goApp().ListAudioDevices()
  caps.value = devs.capture
  plays.value = devs.playback
})

// 关闭窗口时：最小化到托盘 / 退出程序（问题 4b，持久化 config.json）。
async function setCloseToTray(v: boolean) {
  closeToTray.value = v
  await goApp().SetCloseToTray(v)
}

async function toggleHibernate(v: boolean) {
  hibernate.value = v
  hibernateStore.setEnabled(v)
  await goApp().SetHibernateOnMinimize(v)
}

// toggleUIDestroy 休眠时卸载界面：最小化隐藏后整页重载释放 JS 堆，
// 唤醒时凭 Go 侧状态重建（实验性；依赖 sys:hibernate/sys:wake 链路）。
async function toggleUIDestroy(v: boolean) {
  uiDestroy.value = v
  await goApp().SetUIDestroyOnHibernate(v)
}

// 设备下拉即时应用：切换即热重启设备流（编解码/混音状态保留）。
async function applyDevice() {
  await goApp().SetAudioDevices(settings.captureDevice, settings.playbackDevice)
}

// 语音设置改动：热应用到当前会话（未连接仅落盘），失败弹提示。
async function applyAudio() {
  try {
    await goApp().SetAudioSettings({ ...audio.value })
  } catch (e) {
    toast.value?.show(`语音设置保存失败：${e}`, true)
  }
}
</script>

<template>
  <div class="settings-page">
    <div class="s-head">
      <button class="icon-btn" title="返回" @click="router.push('/')">
        <ArrowLeft :size="18" />
      </button>
      <b>设置</b>
      <span class="s-ver">{{ version }}</span>
    </div>

    <div class="s-body">
      <section class="s-sec">
        <h3>连接</h3>
        <ServerConnPanel @saved="toast?.show('已保存，重启后生效')" />
      </section>

      <section class="s-sec">
        <h3>账号</h3>
        <div class="s-row">
          <div><div class="k">昵称</div><div class="v">{{ auth.user?.nickname ?? '—' }}</div></div>
          <div><div class="k">邮箱</div><div class="v">{{ auth.user?.email ?? '—' }}</div></div>
        </div>
      </section>

      <section class="s-sec">
        <h3>常规</h3>
        <div class="s-row">
          <div class="k">最小化时休眠界面（省内存）</div>
          <label class="switch">
            <input type="checkbox" :checked="hibernate" @change="toggleHibernate(($event.target as HTMLInputElement).checked)">
            <i />
          </label>
        </div>
        <div class="s-row">
          <div class="k">休眠时卸载界面（彻底释放内存，实验性）</div>
          <label class="switch">
            <input type="checkbox" :checked="uiDestroy" @change="toggleUIDestroy(($event.target as HTMLInputElement).checked)">
            <i />
          </label>
        </div>
        <!-- 关闭窗口时：最小化到托盘（默认）/ 退出程序（问题 4b，用户可选） -->
        <div class="s-row">
          <div class="k">关闭窗口时</div>
          <div class="seg-mini">
            <button :class="{ on: closeToTray }" @click="setCloseToTray(true)">最小化到托盘</button>
            <button :class="{ on: !closeToTray }" @click="setCloseToTray(false)">退出程序</button>
          </div>
        </div>
        <div class="s-row">
          <button class="quit-btn" @click="goApp().QuitApp()">
            <LogOut :size="14" class="btn-icon" />退出程序
          </button>
        </div>
      </section>

      <section class="s-sec">
        <h3>音频设备</h3>
        <div class="s-row">
          <div class="k">麦克风</div>
          <select v-model="settings.captureDevice" @change="applyDevice">
            <option v-for="d in caps" :key="d.id" :value="d.id">{{ d.name }}</option>
          </select>
        </div>
        <div class="s-row">
          <div class="k">扬声器</div>
          <select v-model="settings.playbackDevice" @change="applyDevice">
            <option v-for="d in plays" :key="d.id" :value="d.id">{{ d.name }}</option>
          </select>
        </div>
      </section>

      <section class="s-sec">
        <h3>语音</h3>
        <div class="s-row">
          <div class="k">麦克风降噪强度（RNNoise）</div>
          <div class="seg-mini">
            <button v-for="(label, i) in DENOISE_LABELS" :key="i"
                    :class="{ on: denoiseIdx === i }"
                    @click="applyDenoiseIdx(i)">{{ label }}</button>
          </div>
        </div>
        <div class="s-row">
          <div class="k">麦克风音量</div>
          <input v-model.number="audio.mic_volume" type="range" min="0" max="120" step="5" @change="applyAudio">
          <span class="v">{{ audio.mic_volume }}</span>
        </div>
        <div class="s-row">
          <div class="k">扬声器音量</div>
          <input v-model.number="audio.playback_volume" type="range" min="0" max="120" step="5" @change="applyAudio">
          <span class="v">{{ audio.playback_volume }}</span>
        </div>
        <div class="s-row">
          <div class="k">自动增益 AGC（实验性）</div>
          <label class="switch">
            <input v-model="audio.agc" type="checkbox" @change="applyAudio"><i />
          </label>
        </div>
        <div class="s-row">
          <div class="k">语音检测灵敏度</div>
          <div class="seg-mini">
            <button
              v-for="(label, i) in VAD_LABELS" :key="i"
              :class="{ on: audio.vad_sens === i }"
              @click="audio.vad_sens = i; applyAudio()"
            >{{ label }}</button>
          </div>
        </div>
        <div class="s-row">
          <div class="k">Opus 复杂度（0 最省 CPU，10 最佳质量）</div>
          <input v-model.number="audio.opus_complexity" type="range" min="0" max="10" step="1" @change="applyAudio">
          <span class="v">{{ audio.opus_complexity }}</span>
        </div>
        <div class="s-row">
          <div class="k">码率</div>
          <div class="seg-mini">
            <button
              v-for="b in BITRATES" :key="b"
              :class="{ on: audio.bitrate_kbps === b }"
              @click="audio.bitrate_kbps = b; applyAudio()"
            >{{ b }}k</button>
          </div>
        </div>
        <div class="s-row">
          <div class="k">VBR 可变码率</div>
          <label class="switch">
            <input v-model="audio.vbr" type="checkbox" @change="applyAudio"><i />
          </label>
        </div>
        <div class="s-row">
          <div class="k">DTX 静音省流</div>
          <label class="switch">
            <input v-model="audio.dtx" type="checkbox" @change="applyAudio"><i />
          </label>
        </div>
        <div class="s-row">
          <div class="k">FEC 丢包补偿</div>
          <label class="switch">
            <input v-model="audio.fec" type="checkbox" @change="applyAudio"><i />
          </label>
        </div>
        <!-- 仅 FEC 开启时显示：告知编码器的预期丢包率，影响纠错强度 -->
        <div v-if="audio.fec" class="s-row">
          <div class="k">预期丢包率</div>
          <input v-model.number="audio.packet_loss" type="range" min="5" max="20" step="5" @change="applyAudio">
          <span class="v">{{ audio.packet_loss }}%</span>
        </div>
      </section>

      <section class="s-sec">
        <h3>快捷键</h3>
        <div v-for="h in HOTKEYS" :key="h.id" class="s-row">
          <div class="k">{{ h.label }}</div><span class="v">{{ h.combo }}</span>
        </div>
      </section>

      <section class="s-sec">
        <h3>关于</h3>
        <div class="s-row"><div class="k">版本</div><span class="v">{{ version }}</span></div>
      </section>
    </div>
    <Toast ref="toast" />
  </div>
</template>

<style scoped>
/* 独立页：占满 TitleBar 之下的全部区域（不再是 stage 浮层）。
   内容列居中：此前 max-width 560 贴左、右半窗留白，滚动条悬在
   内容右缘像一根游离蓝条（用户走查反馈），居中后读作列滚动条。 */
.settings-page {
  flex: 1; min-height: 0;
  display: flex; flex-direction: column;
  background: var(--surface);
}
.s-head {
  height: 56px; flex: none;
  display: flex; align-items: center; gap: 12px;
  padding: 0 22px; border-bottom: 1px solid var(--line);
}
.s-head b { font-size: 15px; }
.s-ver { margin-left: auto; font-size: 12px; color: var(--ink-300); }
.icon-btn {
  width: 32px; height: 32px;
  display: flex; align-items: center; justify-content: center;
  border-radius: 8px; color: var(--ink-400);
  transition: background 0.15s, color 0.15s;
}
.icon-btn:hover { background: var(--brand-50); color: var(--ink-900); }
.s-body {
  flex: 1; overflow-y: auto; padding: 22px; max-width: 560px; width: 100%;
  /* 列居中：贴左时右半窗留白、滚动条悬在内容右缘像游离蓝条（用户走查反馈） */
  margin: 0 auto;
}
.s-sec { margin-bottom: 26px; }
.s-sec h3 {
  font-size: 12px; font-weight: 700; letter-spacing: 1.5px;
  color: var(--ink-400); margin-bottom: 12px;
}
.s-row {
  display: flex; align-items: center; gap: 12px;
  padding: 10px 14px; border-radius: 10px;
  background: var(--brand-50); margin-bottom: 8px;
}
.k { font-size: 13.5px; font-weight: 500; color: var(--ink-600); flex: 1; }
.v { font-size: 13px; color: var(--ink-400); }
select {
  height: 32px; border-radius: 8px; border: 1.5px solid var(--brand-200);
  padding: 0 8px; font-size: 13px; color: var(--ink-900);
  background: var(--surface); outline: none;
}
input[type="range"] { flex: 0 0 140px; accent-color: var(--brand-500); }
.switch { position: relative; width: 36px; height: 20px; flex: none; }
.switch input { display: none; }
.switch i {
  position: absolute; inset: 0;
  border-radius: 10px; background: var(--brand-200);
  transition: background 0.2s;
}
.switch i::after {
  content: ""; position: absolute; left: 2px; top: 2px;
  width: 16px; height: 16px; border-radius: 50%;
  background: #fff; box-shadow: var(--shadow-1);
  transition: transform 0.25s var(--ease);
}
.switch input:checked + i { background: var(--brand-500); }
.switch input:checked + i::after { transform: translateX(16px); }

.seg-mini {
  display: flex; gap: 2px; padding: 3px;
  background: var(--brand-50); border-radius: 9px;
}
.s-body .seg-mini { background: rgba(79, 164, 243, 0.08); }
.seg-mini button {
  height: 28px; padding: 0 12px; border-radius: 7px;
  font-size: 12.5px; font-weight: 600; color: var(--ink-400);
  transition: background 0.2s var(--ease), color 0.2s var(--ease);
}
.seg-mini button.on { background: var(--surface); color: var(--brand-700); box-shadow: var(--shadow-1); }

.quit-btn {
  display: inline-flex; align-items: center; gap: 6px;
  height: 32px; padding: 0 14px; border-radius: 8px;
  font-size: 13px; font-weight: 600;
  color: var(--red); background: rgba(232, 91, 91, 0.1);
  transition: background 0.2s var(--ease);
}
.quit-btn:hover { background: rgba(232, 91, 91, 0.18); }
.btn-icon { flex: none; }
</style>
