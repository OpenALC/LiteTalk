// 弱网指标解析（#49 B）：voice:stats 新增丢包/抖动/缓冲深度与 per-peer
// 诊断；缺省字段必须回落为 0/空，不能把 undefined 带进 UI 计算。
import { beforeEach, describe, expect, it, vi } from 'vitest'
import { createPinia, setActivePinia } from 'pinia'

const mockEventsOn = vi.hoisted(() => {
  const handlers = new Map<string, Array<(...args: unknown[]) => void>>()
  return {
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
vi.mock('../api/bridge', () => ({
  goApp: () => ({}),
  eventsOn: mockEventsOn.on,
}))

import { useVoiceStore } from './voice'

describe('voice store 弱网指标', () => {
  beforeEach(() => {
    setActivePinia(createPinia())
    mockEventsOn.reset()
  })

  it('解析 voice:stats 的新字段与 per-peer 诊断', () => {
    const voice = useVoiceStore()
    voice.bindEvents()

    mockEventsOn.fire('voice:stats', JSON.stringify({
      latency_ms: 48,
      members: 2,
      peer_ids: ['u1'],
      enc_packets: 100,
      rtp_sent: 90,
      playback_buffered_ms: 20,
      playback_underruns: 0,
      playback_dropped: 0,
      loss_pct: 1.5,
      rtp_jitter_ms: 12.5,
      jitter_buffer_ms: 60,
      jitter_target_ms: 80,
      rtp_received: 197,
      rtp_lost: 3,
      peers: [{
        peer_id: 'u1',
        jitter_ms: 4,
        jitter_target_packets: 4,
        jitter_buffered_packets: 3,
        jitter_lost: 1,
        ring_samples: 480,
        plc_frames: 0,
        fec_frames: 1,
        dred_frames: 0,
        underrun_frames: 0,
        dropped_samples: 0,
        loss_pct: 1.5,
        rtp_jitter_ms: 12.5,
        buffer_ms: 60,
        target_ms: 80,
      }],
    }))

    expect(voice.latencyMs).toBe(48)
    expect(voice.stats.loss_pct).toBe(1.5)
    expect(voice.stats.rtp_jitter_ms).toBe(12.5)
    expect(voice.stats.jitter_buffer_ms).toBe(60)
    expect(voice.stats.jitter_target_ms).toBe(80)
    expect(voice.stats.rtp_received).toBe(197)
    expect(voice.stats.rtp_lost).toBe(3)

    const diag = voice.peerDiag('u1')
    expect(diag).toBeDefined()
    expect(diag?.loss_pct).toBe(1.5)
    expect(diag?.rtp_jitter_ms).toBe(12.5)
    expect(diag?.buffer_ms).toBe(60)
    expect(voice.peerDiag('missing')).toBeUndefined()
  })

  it('旧格式（无新字段）回落为 0 与空 peers', () => {
    const voice = useVoiceStore()
    voice.bindEvents()

    // 旧后端只发基础字段。
    mockEventsOn.fire('voice:stats', JSON.stringify({
      latency_ms: 30, members: 1, peer_ids: [], enc_packets: 0, rtp_sent: 0,
      playback_buffered_ms: 0, playback_underruns: 0, playback_dropped: 0,
    }))

    expect(voice.latencyMs).toBe(30)
    expect(voice.stats.loss_pct).toBeUndefined()
    expect(voice.stats.peers).toBeUndefined()
    expect(voice.peerDiag('u1')).toBeUndefined()
  })

  it('坏包不影响上一次快照', () => {
    const voice = useVoiceStore()
    voice.bindEvents()
    mockEventsOn.fire('voice:stats', JSON.stringify({ latency_ms: 55 }))
    mockEventsOn.fire('voice:stats', 'not-json')
    expect(voice.latencyMs).toBe(55)
  })

  it('离房清空统计，UI 不残留上一房间的丢包/抖动', () => {
    const voice = useVoiceStore()
    voice.bindEvents()
    mockEventsOn.fire('voice:stats', JSON.stringify({
      latency_ms: 99, loss_pct: 8, rtp_jitter_ms: 20, jitter_buffer_ms: 80,
      peers: [{ peer_id: 'u1', jitter_ms: 0, jitter_target_packets: 4, jitter_buffered_packets: 4,
        jitter_lost: 0, ring_samples: 0, plc_frames: 0, fec_frames: 0, dred_frames: 0,
        underrun_frames: 0, dropped_samples: 0, loss_pct: 8 }],
    }))
    voice.channelId = 'room1'

    voice.resetRoom()

    expect(voice.latencyMs).toBe(0)
    expect(voice.stats.loss_pct).toBeUndefined()
    expect(voice.stats.peers).toBeUndefined()
    expect(voice.peerDiag('u1')).toBeUndefined()
  })

  it('unbindEvents 后不再接收统计', () => {
    const voice = useVoiceStore()
    voice.bindEvents()
    voice.unbindEvents()

    mockEventsOn.fire('voice:stats', JSON.stringify({ latency_ms: 77 }))
    expect(voice.latencyMs).toBe(0)
  })
})
