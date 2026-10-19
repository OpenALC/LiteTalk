import { defineStore } from 'pinia'
import { computed, ref } from 'vue'
import { eventsOn, goApp } from '../api/bridge'
import type { PeerView } from '../api/types'
import { useNotifyStore } from './notify'

// voice 语音房状态：房间成员、说话/闭麦/静音、统计。
export const useVoiceStore = defineStore('voice', () => {
  const channelId = ref('') // 当前所在语音频道
  const members = ref<PeerView[]>([]) // 含自己
  const muted = ref(false)
  const deafened = ref(false)
  const latencyMs = ref(0)
  const connected = ref(false)

  // 双关态（闭麦+静音同时开启）——用户要求的独立样式依据
  const dualOff = computed(() => muted.value && deafened.value)

  // peerVolumes 本机各成员播放音量缓存（0.0–2.0）：无读取端点，
  // 打开「更多」弹层时以本缓存为初值，不再每次重置 100（走查建议）。
  const peerVolumes = ref<Record<string, number>>({})

  function selfId(): string {
    // 由 channel.joined 的 self 事件回填；此处从 members 里没有 self 字段，
    // 用 SetSelf 由 MainView 注入。
    return _selfId
  }
  let _selfId = ''

  function setSelf(id: string) {
    _selfId = id
  }

  // generation 会话代次：resetAll（登出/被踢/换服）递增；在飞的 join/restoreFromGo
  // 返回后比对，代次变了就丢弃结果——否则登出后房间态会被旧响应重新写回，
  // 前端又显示「在房内」（#85）。
  let generation = 0

  async function join(id: string) {
    const gen = generation
    // 先不写 channelId——JoinChannel 失败时不置位，守卫不会拦死重试（B1）
    await goApp().JoinChannel(id)
    if (gen !== generation) return
    channelId.value = id
    const list = await goApp().GetVoiceMembers()
    if (gen !== generation) return
    members.value = list
  }

  async function leave() {
    if (!channelId.value) return
    resetRoom()
    await goApp().LeaveChannel()
  }

  // resetRoom 本地清空房间态（主动退房 / 语音被抢占）。不发 LeaveChannel——
  // 抢占路径下 Go 侧已本地退房，再发会把抢占端也带离房间。
  function resetRoom() {
    channelId.value = ''
    members.value = []
    latencyMs.value = 0 // Q4：挂断即归零，避免 voice-bar 重开瞬间残留旧延迟
    connected.value = false
  }

  // resetAll 登出/被踢/换服时全量清理（含 resetRoom 不覆盖的 muted/deafened/
  // peerVolumes）。setup 风格 store 无 $reset（Pinia 只给 option store 注入），
  // 必须显式清理；幂等。事件订阅保留到 MainView 卸载时 unbindEvents。
  function resetAll() {
    generation++
    resetRoom()
    muted.value = false
    deafened.value = false
    peerVolumes.value = {}
    _selfId = ''
  }

  async function setMute(v: boolean) {
    muted.value = v
    await goApp().SetMute(v)
    applySelf()
  }

  async function setDeafen(v: boolean) {
    deafened.value = v
    await goApp().SetDeafen(v)
    applySelf()
  }

  function applySelf() {
    const me = members.value.find((m) => m.user_id === _selfId)
    if (me) {
      me.mute = muted.value
      me.deafen = deafened.value
    }
  }

  async function setPeerVolume(userId: string, gain: number) {
    await goApp().SetPeerVolume(userId, gain)
    peerVolumes.value = { ...peerVolumes.value, [userId]: gain }
  }

  // requestVoiceState 「请求静音」：请目标闭麦/静音（定向信令，对方接受后生效，无强制）。
  async function requestVoiceState(targetId: string, mute: boolean, deafen: boolean) {
    if (!channelId.value) return
    await goApp().RequestVoiceState(channelId.value, targetId, mute, deafen)
  }

  // restoreFromGo UI 销毁重载后回填语音态（Go 侧会话未断；未连接时为空态）。
  async function restoreFromGo() {
    const gen = generation
    try {
      const st = await goApp().GetVoiceState()
      if (gen !== generation || !st.channel_id) return
      channelId.value = st.channel_id
      muted.value = st.muted
      deafened.value = st.deafened
      const list = await goApp().GetVoiceMembers()
      if (gen !== generation) return
      members.value = list
      applySelf()
    } catch {
      // 未连接或方法未就绪：保持空态。
    }
  }

  // 事件订阅（app 层 EventsEmit）。eventsOn 返回按监听器的取消函数，
  // 卸载时逐个注销——不按事件名整体 Off（会清掉别人注册的同名监听）。
  const offs: Array<() => void> = []

  function bindEvents() {
    if (offs.length) return // 幂等：重复绑定会叠监听（登录后 MainView 可能重建）
    const notify = useNotifyStore()
    offs.push(eventsOn('voice:self', (id) => {
      _selfId = id as string
    }))
    // 全局热键 / 托盘在 webview 之外改闭麦静音，前端只靠自己那一路的乐观更新
    // 会停在旧值。Go 侧三条路径统一推这个事件，这里只覆写状态、不回写 Go。
    offs.push(eventsOn('voice:state', (d) => {
      const p = d as { muted?: boolean; deafened?: boolean } | null
      if (typeof p?.muted === 'boolean') muted.value = p.muted
      if (typeof p?.deafened === 'boolean') deafened.value = p.deafened
      applySelf()
    }))
    offs.push(eventsOn('voice:peer-join', (p) => {
      const peer = p as PeerView
      if (!members.value.some((m) => m.user_id === peer.user_id)) {
        members.value.push(peer)
      }
    }))
    offs.push(eventsOn('voice:peer-leave', (id) => {
      members.value = members.value.filter((m) => m.user_id !== (id as string))
    }))
    offs.push(eventsOn('voice:speaking', (p) => {
      const d = p as PeerView
      const m = members.value.find((x) => x.user_id === d.user_id)
      if (m) {
        m.speaking = d.speaking
        m.mute = d.mute
        m.deafen = d.deafen
      }
    }))
    offs.push(eventsOn('voice:stats', (s) => {
      try {
        const st = JSON.parse(s as string)
        latencyMs.value = st.latency_ms ?? 0
      } catch { /* 忽略坏包 */ }
    }))
    // voice.state_request：他人请求我闭麦/静音——toast 带「接受」按钮，由我决定执行。
    offs.push(eventsOn('voice:state_request', (d) => {
      const req = d as { nickname?: string; mute?: boolean; deafen?: boolean }
      const name = req.nickname ?? '有成员'
      const what = req.mute ? '闭麦' : req.deafen ? '静音' : '调整状态'
      notify.showAction(`${name} 请求你${what}`, '接受', () => {
        if (req.mute) void setMute(true)
        if (req.deafen) void setDeafen(true)
      })
    }))
    // 语音被抢占（session.voice_taken）：同账号另一设备进了语音房，服务端已把
    // 本端摘出。Go 侧同步做了本地退房，这里清房间态（否则成员列表留着幽灵），
    // 并说明原因——不说明的话用户只会看到房间突然空了。
    offs.push(eventsOn('voice:taken', () => {
      resetRoom()
      notify.show('已在其他设备加入语音')
    }))
    // 音频不可用（管线初始化失败，或入房后采集/播放启动失败，#80）：Go 侧对
    // 同一故障周期只上报一次，这里直接提示。toast 是瞬时的（notify 自动隐藏），
    // 重入房重试成功后无需清除状态；若再次失败，Go 侧会在新故障周期重新上报。
    offs.push(eventsOn('voice:unavailable', (d) => {
      const msg = (d as { error?: string } | null)?.error
      notify.show(msg?.trim() || '语音不可用', true)
    }))
  }

  // unbindEvents 休眠态清理：逐个注销本 store 注册的监听器。
  function unbindEvents() {
    while (offs.length) offs.pop()!()
  }

  return {
    channelId, members, muted, deafened, dualOff, latencyMs, connected, peerVolumes,
    selfId, setSelf, join, leave, resetRoom, resetAll, setMute, setDeafen, setPeerVolume,
    requestVoiceState, restoreFromGo, bindEvents, unbindEvents,
  }
})
