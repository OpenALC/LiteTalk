// chat store 复位与订阅生命周期（#85）：resetAll 清空消息缓存但不动监听器；
// bindEvents 幂等；unbindEvents 只注销本 store 的监听器，不按事件名整体清。
import { beforeEach, describe, expect, it, vi } from 'vitest'
import { createPinia, setActivePinia } from 'pinia'

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

import type { ChatMessage } from '../api/types'
import { useChatStore } from './chat'

const msg = (id: string, channelId = 'c1'): ChatMessage => ({
  id, channel_id: channelId, user_id: 'u1', nickname: '甲', content: 'hi',
  ts: '2026-01-01T00:00:00Z',
})

describe('chat store 复位', () => {
  beforeEach(() => {
    setActivePinia(createPinia())
    mockEventsOn.reset()
  })

  it('resetAll 清空全部频道缓存且幂等', () => {
    const chat = useChatStore()
    chat.messagesByChannel = { c1: [msg('m1')], c2: [msg('m2', 'c2')] }

    chat.resetAll()

    expect(chat.messagesByChannel).toEqual({})
    expect(chat.messagesFor('c1')).toEqual([])

    chat.resetAll()
    expect(chat.messagesByChannel).toEqual({})
  })

  it('resetAll 不注销事件订阅：复位后新消息仍入桶', () => {
    const chat = useChatStore()
    chat.bindEvents()

    chat.resetAll()
    mockEventsOn.fire('chat:message', msg('m1'))

    expect(chat.messagesFor('c1').map((m) => m.id)).toEqual(['m1'])
    chat.unbindEvents()
  })

  it('消息按 id 去重：nonce 幂等回显不重复展示', () => {
    const chat = useChatStore()
    chat.bindEvents()

    mockEventsOn.fire('chat:message', { ...msg('m1'), nonce: 'n-1' })
    mockEventsOn.fire('chat:message', { ...msg('m1'), nonce: 'n-1' })

    expect(chat.messagesFor('c1')).toHaveLength(1)
    chat.unbindEvents()
  })

  it('bindEvents 幂等，unbindEvents 只注销本 store 的监听器', () => {
    const chat = useChatStore()
    const other: ChatMessage[] = []
    mockEventsOn.on('chat:message', (d) => other.push(d as ChatMessage))

    chat.bindEvents()
    chat.bindEvents()
    // 1 个本 store + 1 个外部监听
    expect(mockEventsOn.handlers.get('chat:message')).toHaveLength(2)

    chat.unbindEvents()
    expect(mockEventsOn.handlers.get('chat:message')).toHaveLength(1)

    mockEventsOn.fire('chat:message', msg('m9'))
    expect(chat.messagesByChannel).toEqual({}) // 本 store 已注销
    expect(other.map((m) => m.id)).toEqual(['m9']) // 外部监听仍在
  })
})
