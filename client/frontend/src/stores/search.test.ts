// 全局消息搜索 store（#42 B2）：成功/空/错误、旧响应代次丢弃、close/resetAll。
import { beforeEach, describe, expect, it, vi } from 'vitest'
import { createPinia, setActivePinia } from 'pinia'

const mockGoApp = vi.hoisted(() => ({ SearchMessages: vi.fn() }))
vi.mock('../api/bridge', () => ({ goApp: () => mockGoApp, eventsOn: () => () => {} }))

import type { ChatMessage } from '../api/types'
import { useSearchStore } from './search'

const msg = (id: string, channelId = 'c1', content = 'hi'): ChatMessage => ({
  id, channel_id: channelId, user_id: 'u1', nickname: '甲', content, ts: '1767225600000',
})

function deferred<T>() {
  let resolve!: (v: T) => void
  let reject!: (e: unknown) => void
  const promise = new Promise<T>((res, rej) => {
    resolve = res
    reject = rej
  })
  return { promise, resolve, reject }
}

describe('search store', () => {
  beforeEach(() => {
    setActivePinia(createPinia())
    mockGoApp.SearchMessages.mockReset()
  })

  it('成功时写回结果，并记录产生结果的查询词', async () => {
    mockGoApp.SearchMessages.mockResolvedValue([msg('m1')])
    const s = useSearchStore()

    await s.searchNow('  你好  ', 20)

    expect(mockGoApp.SearchMessages).toHaveBeenCalledWith('你好', 20)
    expect(s.results.map((m) => m.id)).toEqual(['m1'])
    expect(s.query).toBe('你好')
    expect(s.resultQuery).toBe('你好')
    expect(s.loading).toBe(false)
    expect(s.error).toBe('')
  })

  it('空数组保持 []，空查询不发请求并清空旧结果', async () => {
    mockGoApp.SearchMessages.mockResolvedValue([])
    const s = useSearchStore()
    await s.searchNow('旧词')
    expect(s.results).toEqual([])

    s.results = [msg('m1')]
    await s.searchNow('   ')
    expect(mockGoApp.SearchMessages).toHaveBeenCalledTimes(1)
    expect(s.results).toEqual([])
    expect(s.resultQuery).toBe('')
    expect(s.loading).toBe(false)
  })

  it('失败时在面板内给清洗后的错误文案，不保留半截结果', async () => {
    mockGoApp.SearchMessages.mockRejectedValue(
      new Error('RuntimeError: get /search: rate_limited: 搜索过于频繁'),
    )
    const s = useSearchStore()

    await s.searchNow('你好')

    expect(s.results).toEqual([])
    expect(s.resultQuery).toBe('')
    expect(s.error).toBe('搜索过于频繁')
    expect(s.loading).toBe(false)
  })

  it('旧响应晚到不覆盖新查询（请求序号保护）', async () => {
    const first = deferred<ChatMessage[]>()
    const second = deferred<ChatMessage[]>()
    mockGoApp.SearchMessages
      .mockReturnValueOnce(first.promise)
      .mockReturnValueOnce(second.promise)
    const s = useSearchStore()

    const p1 = s.searchNow('旧')
    const p2 = s.searchNow('新')
    second.resolve([msg('new')])
    await p2
    expect(s.results.map((m) => m.id)).toEqual(['new'])
    expect(s.loading).toBe(false)

    // 旧请求此刻才返回：结果与 loading 都不得被改写。
    first.resolve([msg('old')])
    await p1
    expect(s.results.map((m) => m.id)).toEqual(['new'])
    expect(s.resultQuery).toBe('新')
    expect(s.loading).toBe(false)
  })

  it('旧请求后失败同样被丢弃，不把错误盖到新结果上', async () => {
    const first = deferred<ChatMessage[]>()
    const second = deferred<ChatMessage[]>()
    mockGoApp.SearchMessages
      .mockReturnValueOnce(first.promise)
      .mockReturnValueOnce(second.promise)
    const s = useSearchStore()

    const p1 = s.searchNow('旧')
    const p2 = s.searchNow('新')
    second.resolve([msg('new')])
    await p2
    first.reject(new Error('boom'))
    await p1

    expect(s.error).toBe('')
    expect(s.results.map((m) => m.id)).toEqual(['new'])
  })

  it('close 关闭面板并清空，在飞响应返回后丢弃', async () => {
    const pending = deferred<ChatMessage[]>()
    mockGoApp.SearchMessages.mockReturnValueOnce(pending.promise)
    const s = useSearchStore()
    s.openDialog()
    expect(s.open).toBe(true)

    const p = s.searchNow('你好')
    s.close()
    expect(s.open).toBe(false)
    expect(s.query).toBe('')
    expect(s.loading).toBe(false)

    pending.resolve([msg('m1')])
    await p
    expect(s.results).toEqual([])
  })

  it('resetAll（登出/被踢）关闭面板并清空', async () => {
    mockGoApp.SearchMessages.mockResolvedValue([msg('m1')])
    const s = useSearchStore()
    s.openDialog()
    await s.searchNow('你好')
    expect(s.results).toHaveLength(1)

    s.resetAll()

    expect(s.open).toBe(false)
    expect(s.query).toBe('')
    expect(s.resultQuery).toBe('')
    expect(s.results).toEqual([])
    expect(s.error).toBe('')
  })
})
