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
vi.mock('../api/bridge', () => ({ goApp: () => ({}), eventsOn: mockEventsOn.on }))

import { useVoiceStore } from './voice'
import { useNotifyStore } from './notify'
import type { PeerView } from '../api/types'

// 语音被抢占（session.voice_taken）后本地必须清空房间态：Go 侧已退房，
// 前端若留着旧 members，UI 会显示根本不在房里的「幽灵成员」。
describe('voice store 房间态清理', () => {
  beforeEach(() => {
    setActivePinia(createPinia())
    mockEventsOn.reset()
  })

  const peer: PeerView = {
    user_id: 'u1', nickname: '甲', tag: '0001', speaking: false, mute: false, deafen: false,
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

  // 入房失败（voice:join_failed）：提示由 chat:error 负责（Go 侧同一次失败还会
  // 发一条 chat:error），voice store 只清房间态——否则左下角残留「正在频道中」
  // 与 0ms 延迟，看起来像人还在房里。
  it('voice:join_failed 清空频道/成员/延迟/连接态，且不重复弹 toast', () => {
    const voice = useVoiceStore()
    const notify = useNotifyStore()
    voice.bindEvents()
    voice.channelId = 'room1'
    voice.members = [peer]
    voice.latencyMs = 42
    voice.connected = true

    mockEventsOn.fire('voice:join_failed', {
      channel_id: 'room1', code: 'join_failed', message: '进入语音频道失败',
    })

    expect(voice.channelId).toBe('')
    expect(voice.members).toEqual([])
    expect(voice.latencyMs).toBe(0)
    expect(voice.connected).toBe(false)
    // 错误提示只由 chat:error 弹一次，voice store 不再 toast。
    expect(notify.visible).toBe(false)
    voice.unbindEvents()
  })

  it('voice:join_failed 的迟到事件不影响已在的其它频道', () => {
    const voice = useVoiceStore()
    voice.bindEvents()
    voice.channelId = 'room1'
    voice.latencyMs = 42

    mockEventsOn.fire('voice:join_failed', { channel_id: 'room2', code: 'join_failed', message: '失败' })

    expect(voice.channelId).toBe('room1')
    expect(voice.latencyMs).toBe(42)
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
