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
const mockGoApp = vi.hoisted(() => ({
  ListMessages: vi.fn(),
}))
vi.mock('../api/bridge', () => ({ goApp: () => mockGoApp, eventsOn: mockEventsOn.on }))

import type { ChatMessage } from '../api/types'
import { mergeHistory, useChatStore } from './chat'
import { useNotifyStore } from './notify'

const msg = (id: string, channelId = 'c1'): ChatMessage => ({
  id, channel_id: channelId, user_id: 'u1', nickname: '甲', content: 'hi',
  ts: '2026-01-01T00:00:00Z',
})

describe('chat store 复位', () => {
  beforeEach(() => {
    setActivePinia(createPinia())
    mockEventsOn.reset()
    mockGoApp.ListMessages.mockReset().mockResolvedValue([])
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

  // chat:error（#34）：服务端拒收 chat.send 的异步 error 帧必须给用户反馈，
  // 载荷 {code, message} 与 app.go 的 emit 口径一致。
  it('chat:error 用 notify 显示服务端原因', () => {
    const chat = useChatStore()
    const notify = useNotifyStore()
    chat.bindEvents()

    mockEventsOn.fire('chat:error', { code: 'forbidden', message: '无权向该频道发送消息' })

    expect(notify.visible).toBe(true)
    expect(notify.isError).toBe(true)
    expect(notify.text).toBe('无权向该频道发送消息')
    chat.unbindEvents()
  })

  it('chat:error 缺 message 时回落统一文案，带机器前缀则清洗', () => {
    const chat = useChatStore()
    const notify = useNotifyStore()
    chat.bindEvents()

    mockEventsOn.fire('chat:error', { code: 'internal' })
    expect(notify.isError).toBe(true)
    expect(notify.text).toBe('消息发送失败')

    mockEventsOn.fire('chat:error', { code: 'forbidden', message: 'forbidden: 无权向该频道发送消息' })
    expect(notify.text).toBe('无权向该频道发送消息')
    chat.unbindEvents()
  })

  it('unbindEvents 注销 chat:error 后不再提示', () => {
    const chat = useChatStore()
    const notify = useNotifyStore()
    chat.bindEvents()
    chat.unbindEvents()

    expect(mockEventsOn.handlers.get('chat:error')).toHaveLength(0)
    mockEventsOn.fire('chat:error', { code: 'forbidden', message: '无权向该频道发送消息' })
    expect(notify.visible).toBe(false)
  })
})

describe('chat store 历史补拉（#38 B4b）', () => {
  beforeEach(() => {
    setActivePinia(createPinia())
    mockEventsOn.reset()
    mockGoApp.ListMessages.mockReset().mockResolvedValue([])
  })

  const at = (id: string, ts: string, content = 'hi'): ChatMessage => ({
    id, channel_id: 'c1', user_id: 'u1', nickname: '甲', content, ts,
  })

  it('mergeHistory 按 id 去重并按 ts 旧→新（同毫秒保留先到顺序）', () => {
    const history = [at('1', '1000'), at('3', '3000'), at('4', '3000', 'same-ms')]
    const live = [at('2', '2000'), at('3', '3000')]

    const merged = mergeHistory(history, live)

    expect(merged.map((m) => m.id)).toEqual(['1', '2', '3', '4'])
    expect(merged.map((m) => m.content)).toEqual(['hi', 'hi', 'hi', 'same-ms'])
  })

  it('loadHistory 把历史并入当前桶，实时消息不丢', async () => {
    const chat = useChatStore()
    chat.messagesByChannel = { c1: [at('3', '3000', '实时')] }
    mockGoApp.ListMessages.mockResolvedValue([at('1', '1000', '旧'), at('2', '2000', '中')])

    await chat.loadHistory('c1')

    expect(mockGoApp.ListMessages).toHaveBeenCalledWith('c1', 50)
    expect(chat.messagesFor('c1').map((m) => m.id)).toEqual(['1', '2', '3'])
    expect(chat.messagesFor('c1')[2].content).toBe('实时')
  })

  it('loadHistory 空结果不创建空桶；失败向上抛给调用方静默', async () => {
    const chat = useChatStore()
    await chat.loadHistory('c1')
    expect(chat.messagesByChannel).toEqual({})

    mockGoApp.ListMessages.mockRejectedValueOnce(new Error('boom'))
    await expect(chat.loadHistory('c1')).rejects.toThrow('boom')
  })

  it('resetAll 后旧的补拉响应不写回（会话代次保护）', async () => {
    const chat = useChatStore()
    let resolve!: (v: ChatMessage[]) => void
    mockGoApp.ListMessages.mockReturnValue(new Promise<ChatMessage[]>((r) => { resolve = r }))

    const pending = chat.loadHistory('c1')
    chat.resetAll()
    resolve([at('1', '1000')])
    await pending

    expect(chat.messagesByChannel).toEqual({})
  })
})
