// 未读与提及计数 store（#40）：快照整体替换、事件覆盖不累加、服务器聚合重算、
// 会话频道不计入服务器、resetAll 后旧响应丢弃。
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
const mockGoApp = vi.hoisted(() => ({
  ListUnreads: vi.fn(),
}))
vi.mock('../api/bridge', () => ({ goApp: () => mockGoApp, eventsOn: mockEventsOn.on }))

import type { Unreads } from '../api/types'
import { badgeText, unreadLabel, useUnreadsStore } from './unreads'
import { useServersStore } from './data'
import { useConversationsStore } from './conversations'

const emptySnapshot = (): Unreads => ({ channels: [], servers: [] })

function seedServer() {
  const servers = useServersStore()
  servers.servers = [{
    id: 's1', name: '服务器', icon: 'MessagesSquare',
    channels: [
      { id: 'c1', name: '闲聊', type: 'text', group: '未分类' },
      { id: 'c2', name: '公告', type: 'text', group: '未分类' },
      { id: 'v1', name: '大厅', type: 'voice', group: '未分类' },
    ],
  }]
  return servers
}

describe('unreads store 快照与聚合（#40）', () => {
  beforeEach(() => {
    setActivePinia(createPinia())
    mockEventsOn.reset()
    mockGoApp.ListUnreads.mockReset().mockResolvedValue(emptySnapshot())
  })

  it('load 整体替换频道与服务器映射（含 0 值项）', async () => {
    const unreads = useUnreadsStore()
    unreads.channels = { stale: { unread: 9, mentions: 9 } }
    unreads.servers = { stale: { unread: 9, mentions: 9 } }
    mockGoApp.ListUnreads.mockResolvedValue({
      channels: [
        { channel_id: 'c1', unread_count: 0, mention_count: 0 },
        { channel_id: 'c2', unread_count: 4, mention_count: 2 },
      ],
      servers: [{ server_id: 's1', unread_count: 4, mention_count: 2 }],
    } satisfies Unreads)

    await unreads.load()

    expect(mockGoApp.ListUnreads).toHaveBeenCalledTimes(1)
    expect(unreads.channels).toEqual({
      c1: { unread: 0, mentions: 0 },
      c2: { unread: 4, mentions: 2 },
    })
    expect(unreads.servers).toEqual({ s1: { unread: 4, mentions: 2 } })
  })

  it('chat:unread 覆盖本地计数，不累加；坏包忽略', () => {
    const unreads = useUnreadsStore()
    unreads.bindEvents()

    mockEventsOn.fire('chat:unread', { channel_id: 'c1', unread_count: 5, mention_count: 2 })
    expect(unreads.channel('c1')).toEqual({ unread: 5, mentions: 2 })

    // 覆盖语义：同一频道再来 2 就是 2，不是 7。
    mockEventsOn.fire('chat:unread', { channel_id: 'c1', unread_count: 2, mention_count: 0 })
    expect(unreads.channel('c1')).toEqual({ unread: 2, mentions: 0 })

    // 非法载荷：缺 ID、负数、非整数、字符串、null——全部忽略，计数保持不变。
    mockEventsOn.fire('chat:unread', { unread_count: 9, mention_count: 9 })
    mockEventsOn.fire('chat:unread', { channel_id: '', unread_count: 9, mention_count: 9 })
    mockEventsOn.fire('chat:unread', { channel_id: 'c1', unread_count: -1, mention_count: 0 })
    mockEventsOn.fire('chat:unread', { channel_id: 'c1', unread_count: 1.5, mention_count: 0 })
    mockEventsOn.fire('chat:unread', { channel_id: 'c1', unread_count: '1', mention_count: 0 })
    mockEventsOn.fire('chat:unread', null)
    expect(unreads.channel('c1')).toEqual({ unread: 2, mentions: 0 })

    // 0 值事件是合法的：读掉后徽标要消失。
    mockEventsOn.fire('chat:unread', { channel_id: 'c1', unread_count: 0, mention_count: 0 })
    expect(unreads.channel('c1')).toEqual({ unread: 0, mentions: 0 })

    unreads.unbindEvents()
  })

  it('setChannel 重算所属服务器聚合；语音频道也计入服务端快照但 UI 不展示', () => {
    seedServer()
    const unreads = useUnreadsStore()

    unreads.setChannel('c1', 2, 1)
    expect(unreads.server('s1')).toEqual({ unread: 2, mentions: 1 })

    unreads.setChannel('c2', 3, 0)
    expect(unreads.server('s1')).toEqual({ unread: 5, mentions: 1 })

    // 覆盖 c1 为 0：聚合随之下降，不会累加。
    unreads.setChannel('c1', 0, 0)
    expect(unreads.server('s1')).toEqual({ unread: 3, mentions: 0 })
  })

  it('会话频道只更新自身与 dmTotals，不计入服务器聚合', () => {
    seedServer()
    const conversations = useConversationsStore()
    conversations.list = [{
      id: 'd1', kind: 'direct', name: '',
      members: [
        { user_id: '1', nickname: '我', tag: '0001', role: 'member', joined_at: '2026-01-01T00:00:00Z' },
        { user_id: '2', nickname: '乙', tag: '0002', role: 'member', joined_at: '2026-01-01T00:00:00Z' },
      ],
      created_at: '2026-01-01T00:00:00Z',
    }]
    const unreads = useUnreadsStore()

    unreads.setChannel('c1', 1, 0)
    unreads.setChannel('d1', 4, 3)

    expect(unreads.channel('d1')).toEqual({ unread: 4, mentions: 3 })
    expect(unreads.dmTotals).toEqual({ unread: 4, mentions: 3 })
    expect(unreads.server('s1')).toEqual({ unread: 1, mentions: 0 })
  })

  it('resetAll 后旧快照响应不写回', async () => {
    const unreads = useUnreadsStore()
    let resolve!: (v: Unreads) => void
    mockGoApp.ListUnreads.mockReturnValue(new Promise<Unreads>((r) => { resolve = r }))

    const pending = unreads.load()
    unreads.resetAll()
    resolve({
      channels: [{ channel_id: 'c1', unread_count: 3, mention_count: 1 }],
      servers: [{ server_id: 's1', unread_count: 3, mention_count: 1 }],
    })
    await pending

    expect(unreads.channels).toEqual({})
    expect(unreads.servers).toEqual({})
  })

  it('data:resync 重新拉取快照，订阅按监听器逐个注销', async () => {
    const unreads = useUnreadsStore()
    unreads.bindEvents()
    await unreads.load()
    expect(mockGoApp.ListUnreads).toHaveBeenCalledTimes(1)

    mockEventsOn.fire('data:resync', null)
    // load 是串行链，事件里异步触发；等到结果落地再断言。
    await vi.waitFor(() => expect(mockGoApp.ListUnreads).toHaveBeenCalledTimes(2))

    unreads.unbindEvents()
    mockEventsOn.fire('data:resync', null)
    await Promise.resolve()
    await Promise.resolve()
    expect(mockGoApp.ListUnreads).toHaveBeenCalledTimes(2)
  })
})

describe('unreads 文案（#40）', () => {
  it('unreadLabel 只列非零项', () => {
    expect(unreadLabel({ unread: 3, mentions: 1 })).toBe('3 条未读，1 条提及')
    expect(unreadLabel({ unread: 0, mentions: 2 })).toBe('2 条提及')
    expect(unreadLabel({ unread: 2, mentions: 0 })).toBe('2 条未读')
    expect(unreadLabel({ unread: 0, mentions: 0 })).toBe('')
  })

  it('badgeText 超过 99 显示 99+', () => {
    expect(badgeText(1)).toBe('1')
    expect(badgeText(99)).toBe('99')
    expect(badgeText(100)).toBe('99+')
  })
})
