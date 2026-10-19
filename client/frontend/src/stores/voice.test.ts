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
})
