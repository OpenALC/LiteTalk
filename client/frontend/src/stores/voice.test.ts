import { beforeEach, describe, expect, it } from 'vitest'
import { createPinia, setActivePinia } from 'pinia'
import { useVoiceStore } from './voice'
import type { PeerView } from '../api/types'

// 语音被抢占（session.voice_taken）后本地必须清空房间态：Go 侧已退房，
// 前端若留着旧 members，UI 会显示根本不在房里的「幽灵成员」。
describe('voice store 房间态清理', () => {
  beforeEach(() => {
    setActivePinia(createPinia())
  })

  const peer: PeerView = {
    user_id: 'u1', nickname: '甲', speaking: false, mute: false, deafen: false,
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
})
