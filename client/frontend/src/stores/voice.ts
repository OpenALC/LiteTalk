import { defineStore } from 'pinia'
import { computed, ref } from 'vue'
import { eventsOff, eventsOn, goApp } from '../api/bridge'
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

  async function join(id: string) {
    // 先不写 channelId——JoinChannel 失败时不置位，守卫不会拦死重试（B1）
    await goApp().JoinChannel(id)
    channelId.value = id
    members.value = await goApp().GetVoiceMembers()
  }

  async function leave() {
    if (!channelId.value) return
    channelId.value = ''
    members.value = []
    await goApp().LeaveChannel()
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
    try {
      const st = await goApp().GetVoiceState()
      if (!st.channel_id) return
      channelId.value = st.channel_id
      muted.value = st.muted
      deafened.value = st.deafened
      members.value = await goApp().GetVoiceMembers()
      applySelf()
    } catch {
      // 未连接或方法未就绪：保持空态。
    }
  }

  // 事件订阅（app 层 EventsEmit）。
  function bindEvents() {
    const notify = useNotifyStore()
    eventsOn('voice:self', (id) => {
      _selfId = id as string
    })
    eventsOn('voice:peer-join', (p) => {
      const peer = p as PeerView
      if (!members.value.some((m) => m.user_id === peer.user_id)) {
        members.value.push(peer)
      }
    })
    eventsOn('voice:peer-leave', (id) => {
      members.value = members.value.filter((m) => m.user_id !== (id as string))
    })
    eventsOn('voice:speaking', (p) => {
      const d = p as PeerView
      const m = members.value.find((x) => x.user_id === d.user_id)
      if (m) {
        m.speaking = d.speaking
        m.mute = d.mute
        m.deafen = d.deafen
      }
    })
    eventsOn('voice:stats', (s) => {
      try {
        const st = JSON.parse(s as string)
        latencyMs.value = st.latency_ms ?? 0
      } catch { /* 忽略坏包 */ }
    })
    // voice.state_request：他人请求我闭麦/静音——toast 带「接受」按钮，由我决定执行。
    eventsOn('voice:state_request', (d) => {
      const req = d as { nickname?: string; mute?: boolean; deafen?: boolean }
      const name = req.nickname ?? '有成员'
      const what = req.mute ? '闭麦' : req.deafen ? '静音' : '调整状态'
      notify.showAction(`${name} 请求你${what}`, '接受', () => {
        if (req.mute) void setMute(true)
        if (req.deafen) void setDeafen(true)
      })
    })
  }

  // unbindEvents 休眠态清理：解除全部事件订阅。
  function unbindEvents() {
    for (const n of ['voice:self', 'voice:peer-join', 'voice:peer-leave', 'voice:speaking', 'voice:stats', 'voice:state_request']) {
      eventsOff(n)
    }
  }

  return {
    channelId, members, muted, deafened, dualOff, latencyMs, connected, peerVolumes,
    selfId, setSelf, join, leave, setMute, setDeafen, setPeerVolume,
    requestVoiceState, restoreFromGo, bindEvents, unbindEvents,
  }
})
