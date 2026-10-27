<script setup lang="ts">
// SettingsView 设置页（独立路由 /settings，v0.1.2 起不再复用语音/文字舞台区域）。
// 分区：连接/账号/常规/音频设备/语音/Beta 功能/快捷键/关于；改动即热应用 + 落盘 config.json。
// 语音默认值全部由 Go 侧 GetAudioSettings 回落（零值 → 默认），UI 只消费完整值。
import { onMounted, onUnmounted, ref, computed } from 'vue'
import { ArrowLeft, CircleAlert, Keyboard, LogOut, Mic, Volume2 } from 'lucide-vue-next'
import { useRouter } from 'vue-router'
import { goApp, eventsOn, type AudioDeviceDTO } from '../api/bridge'
import type { AudioLevel, AudioSettings, HotkeySettings } from '../api/types'
import { useAuthStore } from '../stores/auth'
import { useSettingsStore } from '../stores/data'
import { useHibernateStore } from '../stores/hibernate'
import { HOTKEYS, isTypingTarget } from '../composables/useHotkeys'
import {
  HOTKEY_ACTIONS, bindingFromKeyEvent, bindingFromMouseEvent, bindingLabel,
  findHotkeyConflicts, findInAppClashes, normalizeBinding, type HotkeyActionID,
} from '../utils/hotkeySpec'
import { cleanErrorText } from '../utils/errorText'
import ServerConnPanel from '../components/ServerConnPanel.vue'
import Toast from '../components/Toast.vue'

const router = useRouter()
const auth = useAuthStore()
const settings = useSettingsStore()
const hibernateStore = useHibernateStore()
const version = ref('dev')

// 登出确认弹层（文案克制：「退出登录？」/「取消」「退出」）
const confirmLogout = ref(false)

async function doLogout() {
  confirmLogout.value = false
  try {
    await auth.logout()
    router.push({ name: 'login' }).catch(() => {})
  } catch {
    toast.value?.show('登出失败', true)
  }
}
// 休眠模式三态（v0.1.2 合并旧版双开关）：
//   off      不休眠
//   hibernate 最小化/关窗时卸载界面组件——webview 进程仍在，内存下降有限
//   destroy  最小化/关窗时销毁渲染进程（里程碑②，Wails v3）——真正释放内存，
//            唤醒经托盘「显示主界面」自动重建界面，语音等后台不受影响
const HIB_LABELS = ['不休眠', '休眠界面', '彻底卸载（推荐省内存）'] as const
const HIB_MODES = ['off', 'hibernate', 'destroy'] as const
const hibMode = ref<'off' | 'hibernate' | 'destroy'>('off')
const closeToTray = ref(true)
const caps = ref<AudioDeviceDTO[]>([])
const plays = ref<AudioDeviceDTO[]>([])
// 系统默认设备名（下拉首项「系统默认（当前设备名）」用；找不到时退化纯「系统默认」）。
const capDefaultName = computed(() => caps.value.find((d) => d.is_default)?.name ?? '')
const playDefaultName = computed(() => plays.value.find((d) => d.is_default)?.name ?? '')
// 设备测试中状态（防抖：采样/播放期间禁用按钮）。
const testingMic = ref(false)
const testingSpk = ref(false)
const toast = ref<InstanceType<typeof Toast>>()

// 实时电平条（-60~0dBFS 刻度 + 说话门限标线）：仅麦克风测试期间由
// audio:level 事件驱动（Go 侧 100ms 节流）；空闲显示占位。
const lvl = ref<AudioLevel>({ peak: -60, rms: -60, threshold: -100 })
const lvlScale = computed(() => Math.min(1, Math.max(0, (lvl.value.rms + 60) / 60)))
const lvlMark = computed(() =>
  `${Math.min(100, Math.max(0, (lvl.value.threshold + 60) / 60) * 100)}%`)
const lvlText = computed(() =>
  lvl.value.rms <= -59.5 ? '—' : `${lvl.value.rms.toFixed(0)} dB`)
let cancelLevel: (() => void) | null = null

// LevelBridge Go 侧 Start/StopLevelMonitor（bridge.ts 类型未含，本地扩展；
// 浏览器 mock 桩无此方法 → 可选调用静默跳过）。
type LevelBridge = { StartLevelMonitor?(): Promise<void>; StopLevelMonitor?(): Promise<void> }
function levelBridge() {
  return goApp() as ReturnType<typeof goApp> & LevelBridge
}

// 语音设置：Go 侧返回已回落默认的完整值（码率/VBR/DTX/FEC/丢包/降噪强度/AGC/
// 麦克风/扬声器音量/VAD 五档/Opus 复杂度/输入声道/前置增益）。
const audio = ref<AudioSettings>({
  bitrate_kbps: 32, vbr: true, dtx: true, fec: true,
  packet_loss: 10, denoise: true, denoise_strength: 2, agc: true,
  mic_volume: 100, playback_volume: 100, vad_sens: 2, opus_complexity: 9,
  input_mode: 'auto', pre_gain_db: 0, enable_apm: false,
})
const BITRATES = [16, 24, 32, 48, 64]
// VAD 五档（v0.1.2 扩展；旧三档由 Go 侧映射 0/2/4）。
const VAD_LABELS = ['很低', '低', '中', '高', '很高']
// 降噪强度四档：关（denoise=false）/弱/中/强（denoise_strength=1/2/3，
// Go 侧映射 rnnoise Aggressiveness 1.0/1.5/2.0）。
const DENOISE_LABELS = ['关', '弱', '中', '强']
// 输入声道下混：auto=自动选最强声道（反相锁定单声道），left/right 固定，mix 平均。
const INPUT_LABELS: Record<NonNullable<AudioSettings['input_mode']>, string> = {
  auto: '自动', left: '左声道', right: '右声道', mix: '混合',
}
function setInputMode(e: Event) {
  const v = (e.target as HTMLSelectElement).value
  audio.value.input_mode = (v in INPUT_LABELS ? v : 'auto') as AudioSettings['input_mode']
  void applyAudio()
}

// denoiseIdx 降噪强度当前档位（0=关，1/2/3=弱/中/强）：加载设置时由
// syncDenoiseIdx 对齐，点击时由 applyDenoiseIdx 同步，模板高亮只读它。
const denoiseIdx = ref(2)
function syncDenoiseIdx() {
  denoiseIdx.value = audio.value.denoise ? audio.value.denoise_strength : 0
}
// Beta APM：默认关闭；平台不支持时禁用开关并提示（原生 stub 平台返回 false）。
const apmSupported = ref(true)
// 显式 setter：先改值再保存；保存/创建失败时回滚 UI 勾选，避免与 Go 侧实际状态不一致。
async function setEnableAPM(on: boolean) {
  if (on && !apmSupported.value) return
  audio.value.enable_apm = on
  if (!(await applyAudio())) {
    audio.value.enable_apm = !on
  }
}
function applyDenoiseIdx(i: number) {
  denoiseIdx.value = i
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
  hibMode.value = (await goApp().GetHibernateMode()) as typeof hibMode.value
  closeToTray.value = await goApp().GetCloseToTray()
  // 机密存储模式：先订阅（ServiceStartup 已推过也能兜到后续变化），再补拉当前值。
  cancelSecret = eventsOn('sys:secret', (d) => {
    const p = d as { mode?: string } | null
    if (typeof p?.mode === 'string') secretMode.value = p.mode
  })
  secretMode.value = await goApp().SecretStorageMode().catch(() => '')
  audio.value = await goApp().GetAudioSettings()
  // omitempty 字段（0/空值不序列化）在 UI 侧补默认，避免滑杆/下拉空值。
  audio.value.input_mode ??= 'auto'
  audio.value.pre_gain_db ??= 0
  audio.value.enable_apm ??= false
  apmSupported.value = await goApp().APMSupported().catch(() => false)
  if (!apmSupported.value) audio.value.enable_apm = false
  // 热键读取单独兜异常：Go 侧绑定缺失时只回落默认值，不连带弄崩下面设备列表的加载。
  try {
    hk.value = await goApp().GetHotkeySettings()
    hk.value.tail_ms ??= 250
    hkAvailable.value = await goApp().GetHotkeyAvailability()
    // 可用性可能在首屏之后才确定（如等待系统权限）：hotkey:available 确定后推一次。
    cancelHkAvail = eventsOn('hotkey:available', (d) => {
      const p = d as { available?: boolean; reason?: string } | null
      if (typeof p?.available === 'boolean') hkAvailable.value = p.available
      hkReason.value = p?.reason ?? ''
    })
  } catch {
    // 保持默认值（等价 Go 侧 DefaultSettings）；改动时 applyHotkey 会提示保存失败
  }
  syncDenoiseIdx()
  const devs = await goApp().ListAudioDevices()
  caps.value = devs.capture
  plays.value = devs.playback
  // 设备下拉初值：空串 = 系统默认（下拉首项显式提供「系统默认」）。
  const sel = await goApp().GetAudioDeviceSelection().catch(() => null)
  settings.captureDevice = sel?.capture_id ?? ''
  settings.playbackDevice = sel?.playback_id ?? ''
})

// 关闭窗口时：最小化到托盘 / 退出程序（问题 4b，持久化 config.json）。
async function setCloseToTray(v: boolean) {
  closeToTray.value = v
  await goApp().SetCloseToTray(v)
}

// setHibernateMode 休眠模式三态切换：即时生效（emitHibernate 按模式分支，互不依赖）。
async function setHibernateMode(mode: typeof hibMode.value) {
  hibMode.value = mode
  hibernateStore.setEnabled(mode !== 'off')
  await goApp().SetHibernateMode(mode)
}

// 设备下拉即时应用：切换即热重启设备流（编解码/混音状态保留）。
async function applyDevice() {
  await goApp().SetAudioDevices(settings.captureDevice, settings.playbackDevice)
}

// testMic 麦克风自检：Go 侧用当前选择的设备采样 2 秒，按 RMS 判定设备可用
// （门限 150 低于说话判定的自适应底噪+12dB，避免轻声说话假阴性）。
// 测试期间 StartLevelMonitor 推送 audio:level（100ms 节流）驱动实时电平条，
// 结束（含异常）必定 StopLevelMonitor 并注销监听。
async function testMic() {
  if (testingMic.value) return
  testingMic.value = true
  cancelLevel = eventsOn('audio:level', (data) => {
    lvl.value = data as AudioLevel
  })
  try {
    await levelBridge().StartLevelMonitor?.()
  } catch {
    // 监视启动失败（如设备忙）不阻断 2 秒自检本身
  }
  try {
    const r = await goApp().TestMicrophone()
    console.log('[device-test] mic rms=%d peak=%d', r.rms, r.peak)
    if (r.rms >= 150) {
      const db = 20 * Math.log10(r.rms / 32767)
      toast.value?.show(`麦克风正常（输入电平 ${db.toFixed(0)} dB）`)
    } else {
      toast.value?.show('未检测到声音——测试时请对着麦克风说话；若已说话仍如此，请检查麦克风权限或所选设备', true)
    }
  } catch (e) {
    toast.value?.show(`麦克风测试失败：${cleanErrorText(e, '未知错误')}`, true)
  } finally {
    try {
      await levelBridge().StopLevelMonitor?.()
    } catch {
      // 幂等停止，失败无须提示
    }
    cancelLevel?.()
    cancelLevel = null
    testingMic.value = false
  }
}

// 离开设置页：注销电平监听并停止监视（进程退出路径由 Go 侧兜底）。
onUnmounted(() => {
  cancelLevel?.()
  cancelLevel = null
  stopRecord()
  cancelHkAvail?.()
  cancelHkAvail = null
  cancelSecret?.()
  cancelSecret = null
  void levelBridge().StopLevelMonitor?.().catch(() => {})
})

// testSpeaker 扬声器自检：播放 880Hz 测试音 1.5 秒（Go 阻塞至播完）。
async function testSpeaker() {
  if (testingSpk.value) return
  testingSpk.value = true
  try {
    await goApp().TestSpeaker()
    toast.value?.show('已播放测试音，听到蜂鸣说明扬声器正常')
  } catch (e) {
    toast.value?.show(`扬声器测试失败：${cleanErrorText(e, '未知错误')}`, true)
  } finally {
    testingSpk.value = false
  }
}

// 语音设置改动：热应用到当前会话（未连接仅落盘），失败弹提示并返回 false。
async function applyAudio(): Promise<boolean> {
  try {
    await goApp().SetAudioSettings({ ...audio.value })
    return true
  } catch (e) {
    toast.value?.show(`语音设置保存失败：${cleanErrorText(e, '未知错误')}`, true)
    return false
  }
}

// 全局热键（按键说话）：Go 侧返回已 Sanitize 的值；omitempty 字段在 UI 侧补默认。
const hk = ref<HotkeySettings>({ enabled: false, mode: 0, tail_ms: 250, cue_sound: false })
// 平台可用性：false 时明确告知「已退化为语音激活」，不是把控件灰掉了事。
const hkAvailable = ref(true)
const hkReason = ref('')
// 说话模式三档（与 Go 侧 hotkey.Mode 对齐）：0 按住说话 / 1 语音激活 / 2 常开。
const HK_MODE_LABELS = ['按住说话', '语音激活', '常开'] as const
// 录制态：正在录制的动作 id（空串 = 未录制）。
const recording = ref('')
// 冲突组合：同一规范化组合被多个动作占用（UI 标红 + 禁止静默覆盖）。
const hkConflicts = computed(() => findHotkeyConflicts(hk.value))
// 与应用内快捷键（Ctrl+M 等）撞车的全局绑定：窗口聚焦下两条路径都会触发，
// 净效果取决于 OS 钩子与 webview keydown 的到达顺序，可能一次翻转也可能互相
// 抵消。运行时无法可靠去重，只能提示用户改绑。
const hkInAppClashes = computed(() => findInAppClashes(hk.value))
let cancelHkAvail: (() => void) | null = null

// 机密存储模式（keyring / file / dpapi）：Linux 无密钥环时令牌落明文文件，
// 必须明示提示（issue #8，不静默降级）。sys:secret 在 ServiceStartup 推一次，
// 本页挂载多半晚于它 → 主动调 SecretStorageMode 补拉，事件仅兜底。
const secretMode = ref('')
let cancelSecret: (() => void) | null = null

function setHkMode(i: number) {
  hk.value.mode = (i >= 0 && i <= 2 ? i : 0) as HotkeySettings['mode']
  void applyHotkey()
}

// applyHotkey 热保存：有冲突先提示且不落盘——两个动作绑同一组合时后写会静默
// 顶掉前一个，用户看不出少了一个热键。
async function applyHotkey() {
  const conflicts = findHotkeyConflicts(hk.value)
  if (conflicts.length) {
    toast.value?.show(`热键冲突：${conflicts.map(bindingLabel).join('、')} 被多个动作占用`, true)
    return
  }
  try {
    await goApp().SetHotkeySettings({ ...hk.value })
  } catch (e) {
    toast.value?.show(`按键说话设置保存失败：${cleanErrorText(e, '未知错误')}`, true)
  }
}

function isConflict(id: HotkeyActionID): boolean {
  const b = normalizeBinding(hk.value[id] ?? '')
  return !!b && hkConflicts.value.includes(b)
}

function bindingText(id: HotkeyActionID): string {
  return bindingLabel(hk.value[id]) || '未绑定'
}

// 录制态：挂 window 的 keydown / mouseup（capture）直到绑定成功、Esc 或窗口失焦。
// mouseup 先于 click 派发，故在 click 处理器里同步挂监听收不到启动那一次点击；
// recStartedAt 再兜一层：早于进入录制态的事件一律忽略。
let recStartedAt = 0

function startRecord(id: string) {
  stopRecord()
  recording.value = id
  recStartedAt = performance.now()
  window.addEventListener('keydown', onRecKey, true)
  window.addEventListener('mouseup', onRecMouse, true)
  // 录制态屏蔽系统右键菜单，否则绑定 mouse2 时会弹菜单
  window.addEventListener('contextmenu', onRecCtx, true)
  window.addEventListener('blur', onRecBlur)
}

function stopRecord() {
  if (!recording.value) return
  recording.value = ''
  window.removeEventListener('keydown', onRecKey, true)
  window.removeEventListener('mouseup', onRecMouse, true)
  window.removeEventListener('contextmenu', onRecCtx, true)
  window.removeEventListener('blur', onRecBlur)
}

function onRecBlur() {
  stopRecord()
}

function onRecCtx(e: Event) {
  e.preventDefault()
}

function onRecKey(e: KeyboardEvent) {
  // Esc 取消优先于一切（含焦点在输入框内），否则输入框里退不出录制态
  if (e.key === 'Escape') {
    e.preventDefault()
    stopRecord()
    return
  }
  if (isTypingTarget(e.target)) return
  const spec = bindingFromKeyEvent(e)
  if (!spec) return // 纯修饰键 / 不可绑定键：继续等待
  e.preventDefault()
  // capture 阶段就拦断，避免应用内快捷键（useHotkeys 在冒泡阶段监听）被连带触发
  e.stopPropagation()
  bindRecording(spec)
}

function onRecMouse(e: MouseEvent) {
  if (e.timeStamp < recStartedAt) return
  const spec = bindingFromMouseEvent(e)
  if (!spec) return
  e.preventDefault()
  bindRecording(spec)
}

function bindRecording(spec: string) {
  const id = recording.value as HotkeyActionID
  stopRecord()
  if (!id) return
  hk.value[id] = spec
  void applyHotkey()
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
        <ServerConnPanel @saved="toast?.show('已保存')" />
      </section>

      <section class="s-sec">
        <h3>账号</h3>
        <div class="s-row">
          <div><div class="k">昵称</div><div class="v">{{ auth.user?.nickname ?? '—' }}</div></div>
          <div><div class="k">邮箱</div><div class="v">{{ auth.user?.email ?? '—' }}</div></div>
        </div>
        <div class="s-row">
          <button class="logout-btn" @click="confirmLogout = true">
            <LogOut :size="14" class="btn-icon" />退出登录
          </button>
        </div>
        <!-- Linux 无密钥环时的降级（issue #8）：密码不保存、游客令牌回落明文文件，
             两者都必须明示，不静默降级 -->
        <p v-if="secretMode === 'file'" class="s-note s-warn">
          <CircleAlert :size="13" />密钥环不可用：登录密码不会被记住；游客令牌以明文文件存储
        </p>
      </section>

      <section class="s-sec">
        <h3>常规</h3>
        <div class="s-row">
          <div class="k">最小化时</div>
          <div class="seg-mini">
            <button v-for="(label, i) in HIB_LABELS" :key="HIB_MODES[i]"
                    :class="{ on: hibMode === HIB_MODES[i] }"
                    @click="setHibernateMode(HIB_MODES[i])">{{ label }}</button>
          </div>
        </div>
        <!-- 休眠界面=卸载界面组件（webview 进程保留）；彻底卸载=销毁渲染进程。
             两种模式都由托盘「显示主界面」唤醒，语音通话等后台不受影响。 -->
        <p class="s-note">休眠界面保留 webview 进程（内存下降有限）；彻底卸载销毁渲染进程，唤醒时自动重建</p>
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
            <option value="">{{ capDefaultName ? `系统默认（${capDefaultName}）` : '系统默认' }}</option>
            <option v-for="d in caps" :key="d.id" :value="d.id">{{ d.name }}</option>
          </select>
          <button class="mini-btn" :disabled="testingMic" @click="testMic">
            <Mic :size="13" />{{ testingMic ? '请说话…' : '测试' }}
          </button>
        </div>
        <!-- 输入声道下混：单声道有信号的耳麦在 auto 下不再被各声道平均压掉 -->
        <div class="s-row">
          <div class="k">输入声道</div>
          <select :value="audio.input_mode ?? 'auto'" @change="setInputMode">
            <option v-for="(label, mode) in INPUT_LABELS" :key="mode" :value="mode">{{ label }}</option>
          </select>
        </div>
        <!-- 实时电平条（-60~0dBFS）：测试期间更新；细线为说话进入门限（只读） -->
        <div class="s-row">
          <div class="k">输入电平</div>
          <div class="lvl" role="img" :aria-label="`输入电平 ${lvlText}`">
            <div class="lvl-fill" :style="{ transform: `scaleX(${lvlScale})` }" />
            <div v-if="lvl.threshold > -60" class="lvl-mark" :style="{ left: lvlMark }" />
          </div>
          <span class="v">{{ lvlText }}</span>
        </div>
        <div class="s-row">
          <div class="k">扬声器</div>
          <select v-model="settings.playbackDevice" @change="applyDevice">
            <option value="">{{ playDefaultName ? `系统默认（${playDefaultName}）` : '系统默认' }}</option>
            <option v-for="d in plays" :key="d.id" :value="d.id">{{ d.name }}</option>
          </select>
          <button class="mini-btn" :disabled="testingSpk" @click="testSpeaker">
            <Volume2 :size="13" />{{ testingSpk ? '播放中…' : '测试' }}
          </button>
        </div>
      </section>

      <section class="s-sec">
        <h3>语音</h3>
        <div class="s-row">
          <div class="k">降噪强度（RNNoise）</div>
          <div class="seg-mini">
            <button v-for="(label, i) in DENOISE_LABELS" :key="i"
                    :class="{ on: denoiseIdx === i }"
                    @click="applyDenoiseIdx(i)">{{ label }}</button>
          </div>
        </div>
        <!-- 前置增益：施加在降噪之前，补偿 -40dBFS 级弱麦；输出音量在下面的「麦克风音量」 -->
        <div class="s-row">
          <div class="k">前置增益（弱麦补偿）</div>
          <input v-model.number="audio.pre_gain_db" type="range" min="-12" max="24" step="1" @change="applyAudio">
          <span class="v">{{ audio.pre_gain_db }} dB</span>
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
          <div class="k">自动增益 AGC</div>
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
        <h3>Beta 功能</h3>
        <div class="s-row">
          <div class="k">回声消除（WebRTC APM）</div>
          <label class="switch" :class="{ disabled: !apmSupported }">
            <input :checked="audio.enable_apm" type="checkbox" :disabled="!apmSupported"
                   @change="setEnableAPM(($event.target as HTMLInputElement).checked)"><i />
          </label>
        </div>
        <p class="s-note">启用 AEC / AGC2 / 高通；CPU 占用增加，默认关闭</p>
        <p v-if="!apmSupported" class="s-note s-warn">
          <CircleAlert :size="13" />当前平台不支持 WebRTC APM
        </p>
      </section>

      <section class="s-sec">
        <h3>快捷键</h3>
        <div class="s-sub">应用内</div>
        <div v-for="h in HOTKEYS" :key="h.id" class="s-row">
          <div class="k">{{ h.label }}</div><span class="v">{{ h.combo }}</span>
        </div>

        <div class="s-sub">全局热键</div>
        <div class="s-row">
          <div class="k">启用全局热键</div>
          <label class="switch">
            <input v-model="hk.enabled" type="checkbox" @change="applyHotkey"><i />
          </label>
        </div>
        <!-- 平台不支持时如实说明降级结果；控件保持可用，配置照常落盘 -->
        <p v-if="!hkAvailable" class="s-note s-warn">
          <CircleAlert :size="13" />当前平台不支持全局热键，已退化为语音激活<span v-if="hkReason">（{{ hkReason }}）</span>
        </p>
        <div class="s-row">
          <div class="k">说话模式</div>
          <div class="seg-mini">
            <button v-for="(label, i) in HK_MODE_LABELS" :key="i"
                    :class="{ on: hk.mode === i }"
                    @click="setHkMode(i)">{{ label }}</button>
          </div>
        </div>
        <!-- 尾包延迟只有按住说话有意义：松手后继续发包的缓冲窗，避免话音被切掉尾巴 -->
        <div v-if="hk.mode === 0" class="s-row">
          <div class="k">尾包延迟</div>
          <input v-model.number="hk.tail_ms" type="range" min="0" max="1000" step="50" @change="applyHotkey">
          <span class="v">{{ hk.tail_ms ?? 250 }} ms</span>
        </div>
        <div class="s-row">
          <div class="k">按下提示音</div>
          <label class="switch">
            <input v-model="hk.cue_sound" type="checkbox" @change="applyHotkey"><i />
          </label>
        </div>
        <div v-for="a in HOTKEY_ACTIONS" :key="a.id" class="s-row">
          <div class="k" :class="{ bad: isConflict(a.id) }">{{ a.label }}</div>
          <button class="mini-btn bind" :class="{ rec: recording === a.id, bad: isConflict(a.id) }"
                  :title="recording === a.id ? '按 Esc 取消' : '点击后按下任意键绑定'"
                  @click="recording === a.id ? stopRecord() : startRecord(a.id)">
            <Keyboard :size="13" />
            {{ recording === a.id ? '按下任意键…' : bindingText(a.id) }}
          </button>
        </div>
        <p v-if="hkConflicts.length" class="s-note s-warn">
          <CircleAlert :size="13" />{{ hkConflicts.map(bindingLabel).join('、') }} 被多个动作占用，请改绑其中一个
        </p>
        <p v-if="hkInAppClashes.length" class="s-note s-warn">
          <CircleAlert :size="13" />{{ hkInAppClashes.map(bindingLabel).join('、') }} 与应用内快捷键相同，窗口聚焦时会重复触发，请改绑
        </p>
      </section>

      <section class="s-sec">
        <h3>关于</h3>
        <div class="s-row"><div class="k">版本</div><span class="v">{{ version }}</span></div>
      </section>
    </div>
    <Toast ref="toast" />
    <!-- 登出确认弹层（文案克制） -->
    <div v-if="confirmLogout" class="logout-overlay" @click.self="confirmLogout = false">
      <div class="logout-card">
        <p>退出登录？</p>
        <div class="logout-acts">
          <button class="logout-cancel" @click="confirmLogout = false">取消</button>
          <button class="logout-ok" @click="doLogout">退出</button>
        </div>
      </div>
    </div>
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
  /* 设备行：下拉与测试按钮并存时收缩，窄窗允许整行换行不挤爆 */
  max-width: 260px; min-width: 0;
}
.s-row { flex-wrap: wrap; }
input[type="range"] { flex: 0 0 140px; accent-color: var(--brand-500); }
.switch { position: relative; width: 36px; height: 20px; flex: none; }
.switch.disabled { opacity: 0.45; cursor: not-allowed; }
.switch.disabled i { cursor: not-allowed; }
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

/* 登出按钮（账号分区）：主色调低调，与退出程序区分 */
.logout-btn {
  display: inline-flex; align-items: center; gap: 6px;
  height: 32px; padding: 0 14px; border-radius: 8px;
  font-size: 13px; font-weight: 600;
  color: var(--brand-700); background: var(--brand-50);
  transition: background 0.2s var(--ease);
}
.logout-btn:hover { background: var(--brand-200); }

/* 登出确认弹层 */
.logout-overlay {
  position: fixed; inset: 0; z-index: 200;
  display: flex; align-items: center; justify-content: center;
  background: rgba(23, 50, 75, 0.25);
}
.logout-card {
  background: var(--surface); border-radius: 12px; padding: 24px 28px;
  min-width: 220px; text-align: center;
  box-shadow: var(--shadow-2, 0 8px 32px rgba(23,50,75,0.18));
}
.logout-card p { margin: 0 0 16px; font-size: 15px; font-weight: 600; color: var(--ink-900); }
.logout-acts { display: flex; gap: 10px; justify-content: center; }
.logout-cancel, .logout-ok {
  height: 32px; padding: 0 18px; border-radius: 8px;
  font-size: 13px; font-weight: 600;
  transition: background 0.2s var(--ease);
}
.logout-cancel { color: var(--ink-600); background: var(--brand-50); }
.logout-cancel:hover { background: var(--brand-200); }
.logout-ok { color: #fff; background: var(--brand-500); }
.logout-ok:hover { background: var(--brand-700); }

/* 说明小字（如休眠「暂不生效」注记）：11px 灰，贴近上一行 */
.s-note {
  margin: -4px 0 10px;
  font-size: 11px; color: var(--ink-300);
}

/* 分区内次级标签（应用内 / 全局热键） */
.s-sub {
  margin: 16px 0 8px;
  font-size: 12px; font-weight: 600; color: var(--ink-400);
}
.s-sec h3 + .s-sub { margin-top: 0; }

/* 降级 / 冲突提示：图标与文字同色，跟随小字排版 */
.s-warn {
  display: flex; align-items: center; gap: 6px;
  color: var(--orange);
}
.s-warn svg { flex: none; }

/* 冲突行：行名与按钮同时标红，避免只靠提示小字被忽略 */
.bad { color: var(--red); }

/* 录制态：实心主色，明确「正在等待按键」 */
.mini-btn.rec { background: var(--brand-500); color: var(--surface); }
/* 绑定按钮：等宽，四个动作的按钮对齐，录制态文字变长不跳版 */
.mini-btn.bind { min-width: 116px; justify-content: center; }

/* 设备测试小按钮：次级弱化的行内按钮，与下拉并排 */
.mini-btn {
  flex: none;
  display: inline-flex; align-items: center; gap: 4px;
  height: 30px; padding: 0 10px; border-radius: 8px;
  font-size: 12px; font-weight: 500;
  color: var(--ink-600); background: var(--brand-50);
  transition: background 0.15s, color 0.15s, opacity 0.15s;
}
.mini-btn:hover { background: var(--brand-200); color: var(--ink-900); }
.mini-btn:disabled { opacity: 0.55; cursor: default; }

/* 实时电平条：-60~0dBFS；填充用 transform: scaleX（动画只走 transform） */
.lvl {
  position: relative; flex: 0 0 160px; height: 8px;
  border-radius: 4px; background: var(--surface);
  border: 1px solid var(--line); overflow: hidden;
}
.lvl-fill {
  position: absolute; inset: 0;
  transform-origin: left center; transform: scaleX(0);
  background: var(--brand-500);
  transition: transform 0.1s linear;
}
/* 说话进入门限标线（只读） */
.lvl-mark {
  position: absolute; top: 0; bottom: 0; width: 2px;
  margin-left: -1px; background: var(--ink-400);
}
</style>
