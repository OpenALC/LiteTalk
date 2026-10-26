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

  // 悄悄话目标（#45）：选择由 UI 维护（同频道成员多选），Go 侧只在按住热键时
  // 真正下发。目标离房后保留选择并标记离线（服务端会静默过滤）。
  // names 用于成员离开后仍能展示目标名字。
  const whisperTargets = ref<string[]>([])
  const whisperTargetNames = ref<Record<string, string>>({})
  const whisperPressed = ref(false) // 悄悄话键按住中
  const whisperActive = ref(false) // 按住且有目标（Go 侧已开麦并下发）

  // whisperTargetViews 指示条展示用：目标名 + 当前是否在线（仍在房间成员里）。
  const whisperTargetViews = computed(() =>
    whisperTargets.value.map((id) => ({
      user_id: id,
      nickname: whisperTargetNames.value[id] ?? id,
      online: members.value.some((m) => m.user_id === id),
    })),
  )

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

  // pendingJoin 在飞入房：voice:join_failed 可能早于 JoinChannel 的 Promise 返回，
  // 处理器凭它认领失败并递增 joinSeq；join() 返回后只有 seq 仍匹配才写 channelId，
  // 否则会把已失败的房间重新写回（幽灵房间）。
  let pendingJoin: { id: string; seq: number } | null = null
  let joinSeq = 0

  // lastJoinFailed 最近一次入房失败（#37 B2）：ChannelList 据 code
  // （bad_password/forbidden）重新弹密码框——覆盖 hasPassword 过期的情况。
  // 只留最后一次；消费方自行比对 channelId。
  const lastJoinFailed = ref<{ channelId: string; code: string } | null>(null)

  // join 入房（#37 B2）：带密码走 JoinChannelWithPassword，否则保持 JoinChannel。
  async function join(id: string, password?: string) {
    const gen = generation
    const seq = ++joinSeq
    pendingJoin = { id, seq }
    // 先不写 channelId——JoinChannel 失败时不置位，守卫不会拦死重试（B1）
    try {
      if (password) {
        await goApp().JoinChannelWithPassword(id, password)
      } else {
        await goApp().JoinChannel(id)
      }
    } catch (e) {
      if (pendingJoin?.seq === seq) pendingJoin = null
      throw e
    }
    // 失败事件已认领（pending 被清、seq 前移）或会话已复位：丢弃这次迟到的成功。
    if (gen !== generation || pendingJoin?.seq !== seq) return
    pendingJoin = null
    // 入房（含切房）意味着目标集合换了：旧的悄悄话选择作废（Go 侧 JoinChannel 已清）。
    whisperTargets.value = []
    whisperTargetNames.value = {}
    whisperPressed.value = false
    whisperActive.value = false
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

  // resetRoom 本地清空房间态（主动退房 / 语音被抢占 / 入房失败）。不发
  // LeaveChannel——抢占路径下 Go 侧已本地退房，再发会把抢占端也带离房间。
  function resetRoom() {
    channelId.value = ''
    members.value = []
    latencyMs.value = 0 // Q4：挂断即归零，避免 voice-bar 重开瞬间残留旧延迟
    connected.value = false
    // 悄悄话选择属于房间内状态：离房即清（Go 侧在 Leave/断线时也已清旁路门）。
    whisperTargets.value = []
    whisperTargetNames.value = {}
    whisperPressed.value = false
    whisperActive.value = false
    // 在飞的 join 不得在复位后把房间态写回。
    pendingJoin = null
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

  // ---- 悄悄话目标（#45） ----

  function isWhisperTarget(userId: string): boolean {
    return whisperTargets.value.includes(userId)
  }

  // toggleWhisperTarget 切换某成员的悄悄话目标。选择只在按住热键时生效；
  // 未按住时调用 Go 侧只保存选择，不产生信令。
  async function toggleWhisperTarget(userId: string) {
    if (!channelId.value || !userId || userId === _selfId) return
    const member = members.value.find((m) => m.user_id === userId)
    const set = new Set(whisperTargets.value)
    if (set.has(userId)) {
      set.delete(userId)
      const names = { ...whisperTargetNames.value }
      delete names[userId]
      whisperTargetNames.value = names
    } else {
      set.add(userId)
      if (member) {
        whisperTargetNames.value = { ...whisperTargetNames.value, [userId]: member.nickname }
      }
    }
    whisperTargets.value = [...set]
    try {
      await goApp().SetWhisperTargets(whisperTargets.value)
    } catch (e) {
      useNotifyStore().fromErr(e, '悄悄话设置失败')
    }
  }

  // setPrioritySpeaker 授予/撤销优先发言（#45）。结果由服务端 voice.priority
  // 广播回填；无 ManageChannels 权限时 Go 侧返回 403，这里只提示。
  async function setPrioritySpeaker(userId: string, enabled: boolean) {
    if (!channelId.value) return
    try {
      await goApp().SetPrioritySpeaker(channelId.value, userId, enabled)
    } catch (e) {
      useNotifyStore().fromErr(e, '优先发言设置失败')
    }
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
      // channel.joined 全量重发也会走到这里：已存在则整体刷新（priority 等
      // 字段可能在重进房后变化），不能只忽略重复。
      const idx = members.value.findIndex((m) => m.user_id === peer.user_id)
      if (idx >= 0) members.value[idx] = { ...members.value[idx], ...peer }
      else members.value.push(peer)
    }))
    offs.push(eventsOn('voice:peer-leave', (id) => {
      members.value = members.value.filter((m) => m.user_id !== (id as string))
      // 悄悄话选择保留：目标可能只是暂时离线，重新入房即恢复；指示条标离线。
    }))
    // voice:priority 优先发言实时变更（#45）：频道覆盖变更后服务端下发，
    // 前端就地更新徽标；角色变更不实时推，重进房由 channel.joined 刷新。
    offs.push(eventsOn('voice:priority', (d) => {
      const p = d as { user_id?: string; priority?: boolean } | null
      if (!p?.user_id) return
      const m = members.value.find((x) => x.user_id === p.user_id)
      if (m) m.priority = !!p.priority
    }))
    // hotkey:whisper 悄悄话键按住指示（#45）：active = 按住且有目标（已开麦）。
    offs.push(eventsOn('hotkey:whisper', (d) => {
      const p = d as { pressed?: boolean; active?: boolean } | null
      whisperPressed.value = !!p?.pressed
      whisperActive.value = !!p?.active
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
    // 入房失败（voice:join_failed）：Go 侧已放弃入房。清掉房间态，左下角
    // 「正在频道中」与延迟条随之消失。Go 侧对这条失败路径不再发 chat:error
    //（joinHandled=true 时 ErrorHandlerEx 直接返回），提示只能在这里弹。
    offs.push(eventsOn('voice:join_failed', (d) => {
      const p = d as { channel_id?: string; code?: string; message?: string } | null
      const failedId = p?.channel_id
      // #37 B2：先记失败（密码门的重试入口），再走房间态回滚。
      if (failedId) lastJoinFailed.value = { channelId: failedId, code: p?.code ?? '' }
      if (pendingJoin) {
        // 有新的在飞 join 时，旧频道的迟到失败不能把它误伤。
        if (failedId && pendingJoin.id !== failedId) return
        // 事件先于 JoinChannel 的 Promise 返回：认领失败并递增 seq，
        // 让在飞 Promise 回来后不再把 channelId 写回（幽灵房间）。
        pendingJoin = null
        joinSeq++
        resetRoom()
      } else if (failedId && channelId.value === failedId) {
        // 已在该频道：Go 侧已放弃入房，清房间态。
        resetRoom()
      } else {
        // 没有在飞 join、也不在当前失败频道：离开后的迟到失败，忽略。
        return
      }
      notify.show(p?.message?.trim() || '进入语音频道失败', true)
    }))
    // 音频不可用（管线初始化失败，或入房后采集/播放启动失败，#80）：Go 侧对
    // 同一故障周期只上报一次，这里直接提示。toast 是瞬时的（notify 自动隐藏），
    // 重入房重试成功后无需清除状态；若再次失败，Go 侧会在新故障周期重新上报。
    offs.push(eventsOn('voice:unavailable', (d) => {
      const msg = (d as { error?: string } | null)?.error
      notify.show(msg?.trim() || '语音不可用', true)
    }))
    // ICE 失败重建（rtc.failed，#19/#34）：Go 侧已销毁 PC 并自动重发 join，
    // 前端只提示链路正在重建。载荷是 {channel_id}，不参与文案（用户已在房里，
    // 频道名在界面上可见）；重建成功后无需清除，toast 本身就是瞬时的。
    offs.push(eventsOn('rtc:failed', () => {
      notify.show('语音连接失败', true)
    }))
  }

  // unbindEvents 休眠态清理：逐个注销本 store 注册的监听器。
  function unbindEvents() {
    while (offs.length) offs.pop()!()
  }

  return {
    channelId, members, muted, deafened, dualOff, latencyMs, connected, peerVolumes,
    whisperTargets, whisperTargetNames, whisperPressed, whisperActive, whisperTargetViews,
    lastJoinFailed,
    selfId, setSelf, join, leave, resetRoom, resetAll, setMute, setDeafen, setPeerVolume,
    requestVoiceState, isWhisperTarget, toggleWhisperTarget, setPrioritySpeaker,
    restoreFromGo, bindEvents, unbindEvents,
  }
})
