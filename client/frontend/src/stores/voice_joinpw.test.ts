// voice store 密码入房（#37 B2）：带/不带密码的桥接分流、bad_password 失败可观测。
import { beforeEach, describe, expect, it, vi } from 'vitest'
import { createPinia, setActivePinia } from 'pinia'

const mockGoApp = vi.hoisted(() => ({
  JoinChannel: vi.fn(async () => {}),
  JoinChannelWithPassword: vi.fn(async () => {}),
  GetVoiceMembers: vi.fn(async (): Promise<unknown[]> => []),
  LeaveChannel: vi.fn(async () => {}),
}))
// eventsOn 桩：记录处理器，测试 fire 模拟事件到达（与 data.test.ts 同法）。
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
vi.mock('../api/bridge', () => ({ goApp: () => mockGoApp, eventsOn: mockEventsOn.on }))

import { useVoiceStore } from './voice'

describe('join 带密码（#37 B2）', () => {
  beforeEach(() => {
    setActivePinia(createPinia())
    mockGoApp.JoinChannel.mockClear()
    mockGoApp.JoinChannelWithPassword.mockClear()
    mockGoApp.GetVoiceMembers.mockClear().mockResolvedValue([])
    mockGoApp.LeaveChannel.mockClear()
    mockEventsOn.reset()
  })

  it('带密码走 JoinChannelWithPassword；不带密码走 JoinChannel', async () => {
    const v = useVoiceStore()
    await v.join('room1', 'pw-1')
    expect(mockGoApp.JoinChannelWithPassword).toHaveBeenCalledWith('room1', 'pw-1')
    expect(mockGoApp.JoinChannel).not.toHaveBeenCalled()
    expect(v.channelId).toBe('room1')

    await v.join('room2')
    expect(mockGoApp.JoinChannel).toHaveBeenCalledWith('room2')
    // 只多调一次带密码的（room2 不带密码）。
    expect(mockGoApp.JoinChannelWithPassword).toHaveBeenCalledTimes(1)
    expect(v.channelId).toBe('room2')
  })

  it('join_failed 记 code 到 lastJoinFailed（bad_password 供密码框重试）', () => {
    const v = useVoiceStore()
    v.bindEvents()
    expect(v.lastJoinFailed).toBeNull()
    mockEventsOn.fire('voice:join_failed', { channel_id: 'room1', code: 'bad_password', message: '频道密码错误' })
    expect(v.lastJoinFailed).toEqual({ channelId: 'room1', code: 'bad_password' })
    v.unbindEvents()
    mockEventsOn.reset()
  })

  it('JoinChannelWithPassword 失败不置房间态', async () => {
    const v = useVoiceStore()
    mockGoApp.JoinChannelWithPassword.mockRejectedValueOnce(new Error('bad_password'))
    await expect(v.join('room3', 'wrong')).rejects.toThrow()
    expect(v.channelId).toBe('')
  })
})
