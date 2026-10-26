import { beforeEach, describe, expect, it, vi } from 'vitest'
import { createPinia, setActivePinia } from 'pinia'

// bridge 桩：bindEvents 在 node 环境无原生桥接会直接返回空取消函数，
// 用事件表重放才能验证「复位不动监听器」与 bind 幂等。
const mockEventsOn = vi.hoisted(() => {
  const handlers = new Map<string, Array<(...args: unknown[]) => void>>()
  return {
    handlers,
    on(name: string, cb: (...args: unknown[]) => void) {
      const list = handlers.get(name) ?? []
      list.push(cb)
      handlers.set(name, list)
      return () => {
        handlers.set(name, (handlers.get(name) ?? []).filter((h) => h !== cb))
      }
    },
    reset() { handlers.clear() },
    fire(name: string, data: unknown) {
      for (const h of handlers.get(name) ?? []) h(data)
    },
  }
})
const mockGoApp = vi.hoisted(() => ({
  JoinChannel: vi.fn(),
  GetVoiceMembers: vi.fn(async () => [] as PeerView[]),
  SetWhisperTargets: vi.fn(async () => {}),
  SetPrioritySpeaker: vi.fn(async () => {}),
}))
vi.mock('../api/bridge', () => ({ goApp: () => mockGoApp, eventsOn: mockEventsOn.on }))

import { useVoiceStore } from './voice'
import { useNotifyStore } from './notify'
import type { PeerView } from '../api/types'

// 语音被抢占（session.voice_taken）后本地必须清空房间态：Go 侧已退房，
// 前端若留着旧 members，UI 会显示根本不在房里的「幽灵成员」。
describe('voice store 房间态清理', () => {
  beforeEach(() => {
    setActivePinia(createPinia())
    mockEventsOn.reset()
    mockGoApp.JoinChannel.mockReset()
    mockGoApp.GetVoiceMembers.mockReset().mockResolvedValue([])
    mockGoApp.SetWhisperTargets.mockReset().mockResolvedValue(undefined)
    mockGoApp.SetPrioritySpeaker.mockReset().mockResolvedValue(undefined)
  })

  const peer: PeerView = {
    user_id: 'u1', nickname: '甲', tag: '0001', speaking: false, mute: false, deafen: false,
    priority: false,
  }

  it('resetRoom 清空频道、成员与延迟', () => {
    const voice = useVoiceStore()
    voice.channelId = 'room1'
    voice.members = [peer]
    voice.latencyMs = 42
    voice.connected = true

    voice.resetRoom()

    expect(voice.channelId).toBe('')
    expect(voice.members).toEqual([])
    expect(voice.latencyMs).toBe(0)
    expect(voice.connected).toBe(false)
  })

  it('主动退房同样清空（与抢占路径同一套清理）', () => {
    const voice = useVoiceStore()
    voice.channelId = 'room1'
    voice.members = [peer]
    voice.latencyMs = 42

    // leave 会调 Go 侧 LeaveChannel（node 环境无原生桥接），只验证本地清理本身。
    voice.resetRoom()

    expect(voice.channelId).toBe('')
    expect(voice.members).toEqual([])
    expect(voice.latencyMs).toBe(0)
  })

  it('未在房时 resetRoom 是空操作', () => {
    const voice = useVoiceStore()
    voice.resetRoom()
    expect(voice.channelId).toBe('')
    expect(voice.members).toEqual([])
  })

  it('resetAll 清空全部状态（含 muted/deafened/peerVolumes）', () => {
    const voice = useVoiceStore()
    voice.channelId = 'room1'
    voice.members = [peer]
    voice.latencyMs = 42
    voice.connected = true
    voice.muted = true
    voice.deafened = true
    voice.peerVolumes = { u1: 1.5 }
    voice.setSelf('u1')

    voice.resetAll()

    expect(voice.channelId).toBe('')
    expect(voice.members).toEqual([])
    expect(voice.latencyMs).toBe(0)
    expect(voice.connected).toBe(false)
    expect(voice.muted).toBe(false)
    expect(voice.deafened).toBe(false)
    expect(voice.peerVolumes).toEqual({})
    expect(voice.selfId()).toBe('')
  })

  it('resetAll 幂等：重复调用不报错', () => {
    const voice = useVoiceStore()
    voice.channelId = 'room1'
    voice.muted = true
    voice.setSelf('u1')

    voice.resetAll()
    voice.resetAll()

    expect(voice.channelId).toBe('')
    expect(voice.muted).toBe(false)
    expect(voice.selfId()).toBe('')
  })

  it('resetAll 不注销事件订阅：复位后仍收 voice:peer-join', () => {
    const voice = useVoiceStore()
    voice.bindEvents()
    voice.resetAll()

    mockEventsOn.fire('voice:peer-join', peer)
    expect(voice.members.map((m) => m.user_id)).toEqual(['u1'])

    // 注销后不再入列，证明清理只停监听器、不动数据复位。
    voice.unbindEvents()
    mockEventsOn.fire('voice:peer-join', { ...peer, user_id: 'u2' })
    expect(voice.members.map((m) => m.user_id)).toEqual(['u1'])
  })

  it('bindEvents 幂等：重复绑定只注册一套监听，unbind 全部注销', () => {
    const voice = useVoiceStore()
    voice.bindEvents()
    const n = mockEventsOn.handlers.get('voice:peer-join')?.length ?? 0
    expect(n).toBeGreaterThan(0)

    voice.bindEvents()
    expect(mockEventsOn.handlers.get('voice:peer-join')).toHaveLength(n)

    voice.unbindEvents()
    expect(mockEventsOn.handlers.get('voice:peer-join')).toHaveLength(0)
  })

  // 语音没起来（#80）：Go 侧把用户可读的原因放在 payload.error，前端必须弹
  // toast——否则用户看着自己在房里，说话没人听得见且毫无提示。
  it('voice:unavailable 经 notify 显示错误提示', () => {
    const voice = useVoiceStore()
    const notify = useNotifyStore()
    voice.bindEvents()

    mockEventsOn.fire('voice:unavailable', { error: '音频设备启动失败，请检查麦克风和扬声器' })

    expect(notify.visible).toBe(true)
    expect(notify.isError).toBe(true)
    expect(notify.text).toBe('音频设备启动失败，请检查麦克风和扬声器')
  })

  it('voice:unavailable 缺原因时回落到通用文案', () => {
    const voice = useVoiceStore()
    const notify = useNotifyStore()
    voice.bindEvents()

    mockEventsOn.fire('voice:unavailable', null)
    expect(notify.text).toBe('语音不可用')
  })

  it('unbindEvents 逐个注销 voice:unavailable，注销后不再提示', () => {
    const voice = useVoiceStore()
    const notify = useNotifyStore()
    voice.bindEvents()
    voice.unbindEvents()

    expect(mockEventsOn.handlers.get('voice:unavailable')).toHaveLength(0)
    mockEventsOn.fire('voice:unavailable', { error: '音频设备启动失败' })
    expect(notify.visible).toBe(false)
  })

  // rtc:failed（#34）：ICE 失败重建时提示用户语音链路正在恢复；载荷 {channel_id}
  // 不参与文案。
  it('rtc:failed 经 notify 提示语音连接失败', () => {
    const voice = useVoiceStore()
    const notify = useNotifyStore()
    voice.bindEvents()

    mockEventsOn.fire('rtc:failed', { channel_id: 'room1' })

    expect(notify.visible).toBe(true)
    expect(notify.isError).toBe(true)
    expect(notify.text).toBe('语音连接失败')
    voice.unbindEvents()
  })

  it('unbindEvents 注销 rtc:failed，注销后不再提示', () => {
    const voice = useVoiceStore()
    const notify = useNotifyStore()
    voice.bindEvents()
    voice.unbindEvents()

    expect(mockEventsOn.handlers.get('rtc:failed')).toHaveLength(0)
    mockEventsOn.fire('rtc:failed', { channel_id: 'room1' })
    expect(notify.visible).toBe(false)
  })

  // 入房失败（voice:join_failed）：Go 侧对这条路径不再发 chat:error（joinHandled
  // 时 ErrorHandlerEx 直接返回），提示只能由本 store 弹；同时清房间态，否则
  // 左下角残留「正在频道中」与 0ms 延迟，看起来像人还在房里。
  it('voice:join_failed 清空频道/成员/延迟/连接态并弹出服务端原因', () => {
    const voice = useVoiceStore()
    const notify = useNotifyStore()
    voice.bindEvents()
    voice.channelId = 'room1'
    voice.members = [peer]
    voice.latencyMs = 42
    voice.connected = true

    mockEventsOn.fire('voice:join_failed', {
      channel_id: 'room1', code: 'join_failed', message: '频道不存在',
    })

    expect(voice.channelId).toBe('')
    expect(voice.members).toEqual([])
    expect(voice.latencyMs).toBe(0)
    expect(voice.connected).toBe(false)
    expect(notify.visible).toBe(true)
    expect(notify.isError).toBe(true)
    expect(notify.text).toBe('频道不存在')
    voice.unbindEvents()
  })

  it('voice:join_failed 缺 message 时回落到通用文案', () => {
    const voice = useVoiceStore()
    const notify = useNotifyStore()
    voice.bindEvents()
    voice.channelId = 'room1'

    mockEventsOn.fire('voice:join_failed', { channel_id: 'room1', code: 'join_failed' })

    expect(notify.text).toBe('进入语音频道失败')
    voice.unbindEvents()
  })

  // 失败事件可能早于 JoinChannel 的 Promise 返回：必须凭 pendingJoin 认领并让
  // 迟到的成功结果失效，否则 await 返回后会把刚失败的频道重新写回（幽灵房间）。
  it('join_failed 早于 JoinChannel 返回：成功后不写回幽灵房间，只提示一次', async () => {
    const voice = useVoiceStore()
    const notify = useNotifyStore()
    const show = vi.spyOn(notify, 'show')
    voice.bindEvents()

    let resolveJoin!: () => void
    mockGoApp.JoinChannel.mockReturnValue(new Promise<void>((res) => { resolveJoin = res }))

    const p = voice.join('room1')
    mockEventsOn.fire('voice:join_failed', {
      channel_id: 'room1', code: 'join_failed', message: '频道不存在',
    })

    expect(voice.channelId).toBe('')
    expect(show).toHaveBeenCalledTimes(1)
    expect(show).toHaveBeenCalledWith('频道不存在', true)

    resolveJoin()
    await p

    expect(voice.channelId).toBe('')
    expect(voice.members).toEqual([])
    // 失败已认领：不再拉成员，也不再弹第二条提示。
    expect(mockGoApp.GetVoiceMembers).not.toHaveBeenCalled()
    expect(show).toHaveBeenCalledTimes(1)
    voice.unbindEvents()
  })

  it('voice:join_failed 的迟到事件不影响已在的其它频道', () => {
    const voice = useVoiceStore()
    const notify = useNotifyStore()
    voice.bindEvents()
    voice.channelId = 'room1'
    voice.latencyMs = 42

    mockEventsOn.fire('voice:join_failed', { channel_id: 'room2', code: 'join_failed', message: '失败' })

    expect(voice.channelId).toBe('room1')
    expect(voice.latencyMs).toBe(42)
    expect(notify.visible).toBe(false)
    voice.unbindEvents()
  })

  it('离开后的迟到失败（无 pending、无 channelId）被忽略', () => {
    const voice = useVoiceStore()
    const notify = useNotifyStore()
    voice.bindEvents()

    mockEventsOn.fire('voice:join_failed', { channel_id: 'room1', code: 'join_failed', message: '失败' })

    expect(voice.channelId).toBe('')
    expect(notify.visible).toBe(false)
    voice.unbindEvents()
  })

  it('unbindEvents 注销 voice:join_failed，注销后不再清房间态', () => {
    const voice = useVoiceStore()
    voice.bindEvents()
    voice.channelId = 'room1'
    voice.unbindEvents()

    expect(mockEventsOn.handlers.get('voice:join_failed')).toHaveLength(0)
    mockEventsOn.fire('voice:join_failed', { channel_id: 'room1', code: 'join_failed' })
    expect(voice.channelId).toBe('room1')
  })
})

// #45：悄悄话目标选择、优先发言徽标与按住指示。
describe('voice store 悄悄话与优先发言', () => {
  beforeEach(() => {
    setActivePinia(createPinia())
    mockEventsOn.reset()
    mockGoApp.JoinChannel.mockReset().mockResolvedValue(undefined)
    mockGoApp.GetVoiceMembers.mockReset().mockResolvedValue([])
    mockGoApp.SetWhisperTargets.mockReset().mockResolvedValue(undefined)
    mockGoApp.SetPrioritySpeaker.mockReset().mockResolvedValue(undefined)
  })

  const peer: PeerView = {
    user_id: 'u1', nickname: '甲', tag: '0001', speaking: false, mute: false, deafen: false,
    priority: false,
  }

  it('切换悄悄话目标：下发 Go、离房目标标离线、再点取消', async () => {
    const voice = useVoiceStore()
    voice.channelId = 'room1'
    voice.members = [peer]
    voice.setSelf('me')

    await voice.toggleWhisperTarget('u1')
    expect(voice.isWhisperTarget('u1')).toBe(true)
    expect(mockGoApp.SetWhisperTargets).toHaveBeenCalledWith(['u1'])
    expect(voice.whisperTargetViews).toEqual([{ user_id: 'u1', nickname: '甲', online: true }])

    // 目标离房：选择保留，指示条标记离线。
    voice.members = []
    expect(voice.whisperTargetViews[0].online).toBe(false)

    await voice.toggleWhisperTarget('u1')
    expect(voice.isWhisperTarget('u1')).toBe(false)
    expect(voice.whisperTargetViews).toEqual([])
    expect(mockGoApp.SetWhisperTargets).toHaveBeenLastCalledWith([])
  })

  it('不能把自己设为悄悄话目标', async () => {
    const voice = useVoiceStore()
    voice.channelId = 'room1'
    voice.members = [{ ...peer, user_id: 'me' }]
    voice.setSelf('me')
    await voice.toggleWhisperTarget('me')
    expect(voice.whisperTargets).toEqual([])
    expect(mockGoApp.SetWhisperTargets).not.toHaveBeenCalled()
  })

  it('resetRoom 清空悄悄话选择与按住态', () => {
    const voice = useVoiceStore()
    voice.channelId = 'room1'
    voice.members = [peer]
    voice.whisperTargets = ['u1']
    voice.whisperTargetNames = { u1: '甲' }
    voice.whisperPressed = true
    voice.whisperActive = true

    voice.resetRoom()

    expect(voice.whisperTargets).toEqual([])
    expect(voice.whisperTargetNames).toEqual({})
    expect(voice.whisperPressed).toBe(false)
    expect(voice.whisperActive).toBe(false)
  })

  it('voice:priority 事件就地更新成员徽标', () => {
    const voice = useVoiceStore()
    voice.bindEvents()
    voice.members = [peer]

    mockEventsOn.fire('voice:priority', { user_id: 'u1', priority: true })
    expect(voice.members[0].priority).toBe(true)
    mockEventsOn.fire('voice:priority', { user_id: 'u1', priority: false })
    expect(voice.members[0].priority).toBe(false)
    voice.unbindEvents()
  })

  it('hotkey:whisper 事件驱动按住/生效指示', () => {
    const voice = useVoiceStore()
    voice.bindEvents()

    mockEventsOn.fire('hotkey:whisper', { pressed: true, active: true })
    expect(voice.whisperPressed).toBe(true)
    expect(voice.whisperActive).toBe(true)
    mockEventsOn.fire('hotkey:whisper', { pressed: false, active: false })
    expect(voice.whisperPressed).toBe(false)
    expect(voice.whisperActive).toBe(false)
    voice.unbindEvents()
  })

  it('peer-join 对已存在成员整体刷新（priority 变化不能丢）', () => {
    const voice = useVoiceStore()
    voice.bindEvents()
    voice.members = [peer]

    mockEventsOn.fire('voice:peer-join', { ...peer, priority: true, speaking: true })
    expect(voice.members).toHaveLength(1)
    expect(voice.members[0].priority).toBe(true)
    expect(voice.members[0].speaking).toBe(true)
    voice.unbindEvents()
  })

  it('setPrioritySpeaker 调用 Go 绑定（403 由 Go 侧错误提示）', async () => {
    const voice = useVoiceStore()
    voice.channelId = 'room1'
    await voice.setPrioritySpeaker('u1', true)
    expect(mockGoApp.SetPrioritySpeaker).toHaveBeenCalledWith('room1', 'u1', true)
  })

  it('加入频道清空旧的悄悄话选择', async () => {
    const voice = useVoiceStore()
    voice.channelId = 'room1'
    voice.whisperTargets = ['u1']
    voice.whisperTargetNames = { u1: '甲' }

    await voice.join('room2')

    expect(voice.channelId).toBe('room2')
    expect(voice.whisperTargets).toEqual([])
    expect(voice.whisperTargetNames).toEqual({})
  })
})
