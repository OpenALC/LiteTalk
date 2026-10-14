import { defineStore } from 'pinia'
import { computed, ref } from 'vue'
import { eventsOff, eventsOn, goApp } from '../api/bridge'
import type { PeerView } from '../api/types'

// voice 语音房状态：房间成员、说话/静音/耳聋、统计。
export const useVoiceStore = defineStore('voice', () => {
  const channelId = ref('') // 当前所在语音频道
  const members = ref<PeerView[]>([]) // 含自己
  const muted = ref(false)
  const deafened = ref(false)
  const latencyMs = ref(0)
  const connected = ref(false)

  // 双关态（静音+耳聋同时开启）——用户要求的独立样式依据
  const dualOff = computed(() => muted.value && deafened.value)

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
  }

  // 事件订阅（app 层 EventsEmit）。
  function bindEvents() {
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
  }

  // unbindEvents 休眠态清理：解除全部事件订阅。
  function unbindEvents() {
    for (const n of ['voice:self', 'voice:peer-join', 'voice:peer-leave', 'voice:speaking', 'voice:stats']) {
      eventsOff(n)
    }
  }

  return {
    channelId, members, muted, deafened, dualOff, latencyMs, connected,
    selfId, setSelf, join, leave, setMute, setDeafen, setPeerVolume, bindEvents, unbindEvents,
  }
})
