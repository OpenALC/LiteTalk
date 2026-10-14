<script setup lang="ts">
// SettingsView 设置页（§9.3：连接/账号/常规/音频设备/语音/快捷键/关于）。
// 「最小化时休眠界面」→ GetHibernateOnMinimize / SetHibernateOnMinimize（app.go 已有）。
// 「连接」→ ServerConnPanel 共享组件（与登录页弹层同源）。
import { onMounted, ref } from 'vue'
import { X, LogOut } from 'lucide-vue-next'
import { goApp, type AudioDeviceDTO } from '../api/bridge'
import type { AudioSettings } from '../api/types'
import { useAuthStore } from '../stores/auth'
import { useSettingsStore } from '../stores/data'
import { useHibernateStore } from '../stores/hibernate'
import { HOTKEYS } from '../composables/useHotkeys'
import ServerConnPanel from './ServerConnPanel.vue'
import Toast from './Toast.vue'

const auth = useAuthStore()
const settings = useSettingsStore()
const hibernateStore = useHibernateStore()
const version = ref('dev')
const hibernate = ref(false)
const closeToTray = ref(true)
const caps = ref<AudioDeviceDTO[]>([])
const plays = ref<AudioDeviceDTO[]>([])
const toast = ref<InstanceType<typeof Toast>>()

// 语音设置（码率/VBR/DTX/FEC/丢包率/降噪/增益/VAD 灵敏度），
// 打开设置页时从 Go 侧读持久化值，改动即热应用 + 落盘 config.json。
const audio = ref<AudioSettings>({
  bitrate_kbps: 32, vbr: true, dtx: true, fec: true,
  packet_loss: 10, denoise: true, gain: 10, vad_sens: 1,
})
const BITRATES = [16, 24, 32, 48, 64]
const VAD_LABELS = ['低', '中', '高']

const emit = defineEmits<{ (e: 'close'): void }>()

onMounted(async () => {
  version.value = await goApp().GetVersion()
  hibernate.value = await goApp().GetHibernateOnMinimize()
  closeToTray.value = await goApp().GetCloseToTray()
  audio.value = await goApp().GetAudioSettings()
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
  <div class="settings">
    <div class="s-head">
      <b>设置</b>
      <button class="icon-btn" title="关闭" @click="emit('close')"><X :size="18" /></button>
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
          <div class="k">麦克风降噪</div>
          <label class="switch">
            <input v-model="audio.denoise" type="checkbox" @change="applyAudio"><i />
          </label>
        </div>
        <div class="s-row">
          <div class="k">输入增益</div>
          <!-- min=0.1：0 与「未设置」在存储层同为 0 无法区分（omitempty），全静音用麦克风静音 -->
          <input v-model.number="audio.gain" type="range" min="1" max="20" step="1" @change="applyAudio">
          <span class="v">{{ (audio.gain / 10).toFixed(1) }}</span>
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
.settings {
  position: absolute; inset: 0; z-index: 25;
  background: var(--surface);
  display: flex; flex-direction: column;
}
.s-head {
  height: 56px; flex: none;
  display: flex; align-items: center; justify-content: space-between;
  padding: 0 22px; border-bottom: 1px solid var(--line);
}
.s-head b { font-size: 15px; }
.icon-btn {
  width: 32px; height: 32px;
  display: flex; align-items: center; justify-content: center;
  border-radius: 8px; color: var(--ink-400);
  transition: background 0.15s, color 0.15s;
}
.icon-btn:hover { background: var(--brand-50); color: var(--ink-900); }
.s-body { flex: 1; overflow-y: auto; padding: 22px; max-width: 560px; }
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

/* 关闭窗口时：迷你二选一（问题 4b） */
.seg-mini {
  display: flex; gap: 2px; padding: 3px;
  background: var(--brand-50); border-radius: 9px;
}
.seg-mini button {
  height: 28px; padding: 0 12px; border-radius: 7px;
  font-size: 12.5px; font-weight: 600; color: var(--ink-400);
  transition: background 0.2s var(--ease), color 0.2s var(--ease);
}
.seg-mini button.on { background: var(--surface); color: var(--brand-700); box-shadow: var(--shadow-1); }

/* 退出程序（不依赖托盘的显式出口，问题 4b） */
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
