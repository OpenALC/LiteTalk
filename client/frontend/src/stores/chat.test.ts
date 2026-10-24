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
  // #39 消息编辑/删除/回复/提及桥接
  SendChatWithMeta: vi.fn(),
  // #41 附件消息发送
  SendChatWithAttachments: vi.fn(),
  EditMessage: vi.fn(),
  DeleteMessage: vi.fn(),
  GetMessage: vi.fn(),
  ListMentionCandidates: vi.fn(),
  // #40 已读上报
  MarkChannelRead: vi.fn(),
}))
vi.mock('../api/bridge', () => ({ goApp: () => mockGoApp, eventsOn: mockEventsOn.on }))

import type { ChatMessage, Conversation, UserProfile } from '../api/types'
import { isMessageDeleted, mergeHistory, useChatStore } from './chat'
import { useAuthStore } from './auth'
import { useConversationsStore } from './conversations'
import { useServersStore } from './data'
import { useNotifyStore } from './notify'
import { useUnreadsStore } from './unreads'

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

    // 没有「已加载」短路：同一频道第一次失败后再次打开会重试。
    mockGoApp.ListMessages.mockRejectedValueOnce(new Error('boom'))
    await expect(chat.loadHistory('c1')).rejects.toThrow('boom')

    mockGoApp.ListMessages.mockResolvedValueOnce([at('1', '1000')])
    await chat.loadHistory('c1')
    expect(chat.messagesFor('c1').map((m) => m.id)).toEqual(['1'])
  })

  it('loadHistory 并发调用只发一次；完成后再次调用重新拉取并合并', async () => {
    const chat = useChatStore()
    let resolve!: (v: ChatMessage[]) => void
    mockGoApp.ListMessages.mockReturnValueOnce(new Promise<ChatMessage[]>((r) => { resolve = r }))

    const p1 = chat.loadHistory('c1')
    const p2 = chat.loadHistory('c1')
    expect(mockGoApp.ListMessages).toHaveBeenCalledTimes(1)

    resolve([at('1', '1000')])
    await Promise.all([p1, p2])
    expect(chat.messagesFor('c1').map((m) => m.id)).toEqual(['1'])

    // 再次打开必须重新请求（断线窗口可能已错过新消息），结果按旧→新合并。
    mockGoApp.ListMessages.mockResolvedValueOnce([at('2', '2000')])
    await chat.loadHistory('c1')
    expect(mockGoApp.ListMessages).toHaveBeenCalledTimes(2)
    expect(chat.messagesFor('c1').map((m) => m.id)).toEqual(['1', '2'])
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

// #39：消息编辑/删除/回复/@提及的 store 行为。
describe('chat store 消息编辑/删除/回复/提及（#39）', () => {
  const baseMsg = (id: string, over: Partial<ChatMessage> = {}): ChatMessage => ({
    id, channel_id: 'c1', user_id: 'u2', nickname: '甲', content: 'hi', ts: id,
    ...over,
  })

  beforeEach(() => {
    setActivePinia(createPinia())
    mockEventsOn.reset()
    for (const fn of Object.values(mockGoApp)) fn.mockReset()
  })

  it('isMessageDeleted：deleted_at 或空正文都按墓碑（resync 回灌边界）；纯附件消息例外', () => {
    expect(isMessageDeleted({ content: '', deleted_at: '' })).toBe(true)
    expect(isMessageDeleted({ content: 'hi', deleted_at: '1700000000000' })).toBe(true)
    expect(isMessageDeleted({ content: 'hi' })).toBe(false)
    // #41 纯附件消息正文为空，但不是墓碑。
    expect(isMessageDeleted({
      content: '',
      attachments: [{
        id: '501', filename: 'a.png', size: 1, content_type: 'image/png',
        width: 1, height: 1, url: '/api/v1/attachments/501?key=k',
      }],
    })).toBe(false)
    // 已软删的消息即使仍带附件数据也按墓碑。
    expect(isMessageDeleted({
      content: '',
      deleted_at: '1700000000000',
      attachments: [{
        id: '501', filename: 'a.png', size: 1, content_type: 'image/png',
        width: 1, height: 1, url: '/api/v1/attachments/501?key=k',
      }],
    })).toBe(true)
  })

  it('sendWithMeta 透传回复与提及；send 保持空 meta 语义', async () => {
    const chat = useChatStore()

    await chat.sendWithMeta('c1', '你好', '9', ['u1'])
    expect(mockGoApp.SendChatWithAttachments).toHaveBeenCalledWith('c1', '你好', '9', ['u1'], [])

    await chat.send('c1', '普通')
    expect(mockGoApp.SendChatWithAttachments).toHaveBeenLastCalledWith('c1', '普通', '', [], [])
  })

  // #41：纯附件消息 content 为空串，附件 ID 按选择顺序透传。
  it('sendWithAttachments 透传附件 ID，正文可为空', async () => {
    const chat = useChatStore()

    await chat.sendWithAttachments('c1', '', '', [], ['501', '502'])
    expect(mockGoApp.SendChatWithAttachments).toHaveBeenCalledWith('c1', '', '', [], ['501', '502'])

    await chat.sendWithAttachments('c1', '看图', '9', ['u1'], ['501'])
    expect(mockGoApp.SendChatWithAttachments).toHaveBeenLastCalledWith('c1', '看图', '9', ['u1'], ['501'])
  })

  it('editMessage 用返回值就地替换并按 id 去重', async () => {
    const chat = useChatStore()
    chat.messagesByChannel = { c1: [baseMsg('1'), baseMsg('2')] }
    mockGoApp.EditMessage.mockResolvedValue(baseMsg('2', {
      content: '改后', edited_at: '1700000000000', mentions: ['u1'],
    }))

    const updated = await chat.editMessage('c1', '2', '改后', ['u1'])

    expect(mockGoApp.EditMessage).toHaveBeenCalledWith('c1', '2', '改后', ['u1'])
    expect(updated.content).toBe('改后')
    expect(chat.messagesFor('c1')).toHaveLength(2)
    expect(chat.messageById('c1', '2')?.content).toBe('改后')
    expect(chat.messageById('c1', '2')?.edited_at).toBe('1700000000000')
    expect(chat.messageById('c1', '2')?.mentions).toEqual(['u1'])
  })

  it('deleteMessage 成功后本地立墓碑，chat:deleted 幂等且保留首次时间', async () => {
    const chat = useChatStore()
    chat.bindEvents()
    chat.messagesByChannel = { c1: [baseMsg('1', { content: '原文' })] }
    mockGoApp.DeleteMessage.mockResolvedValue(undefined)

    await chat.deleteMessage('c1', '1')

    expect(mockGoApp.DeleteMessage).toHaveBeenCalledWith('c1', '1')
    const m = chat.messageById('c1', '1')!
    expect(m.content).toBe('')
    expect(m.deleted_at).toBeTruthy()
    const stamp = m.deleted_at

    mockEventsOn.fire('chat:deleted', { channel_id: 'c1', message_id: '1' })
    expect(chat.messagesFor('c1')).toHaveLength(1)
    expect(chat.messageById('c1', '1')?.deleted_at).toBe(stamp)
    chat.unbindEvents()
  })

  it('chat:edited 更新内容/时间/提及；未知消息忽略且不入桶', () => {
    const chat = useChatStore()
    chat.bindEvents()
    chat.messagesByChannel = { c1: [baseMsg('1', { mentions: ['u3'] })] }

    mockEventsOn.fire('chat:edited', {
      channel_id: 'c1', message_id: '1', content: '改后',
      edited_at: '1700000000000', mentions: ['u1'],
    })
    expect(chat.messageById('c1', '1')?.content).toBe('改后')
    expect(chat.messageById('c1', '1')?.edited_at).toBe('1700000000000')
    expect(chat.messageById('c1', '1')?.mentions).toEqual(['u1'])

    mockEventsOn.fire('chat:edited', {
      channel_id: 'c1', message_id: '999', content: '幽灵', edited_at: '1', mentions: [],
    })
    expect(chat.messagesFor('c1')).toHaveLength(1)
    expect(chat.messageById('c1', '999')).toBeUndefined()
    chat.unbindEvents()
  })

  it('chat:deleted 置空正文保留墓碑；未知消息忽略', () => {
    const chat = useChatStore()
    chat.bindEvents()
    chat.messagesByChannel = { c1: [baseMsg('1')] }

    mockEventsOn.fire('chat:deleted', { channel_id: 'c1', message_id: '1' })
    expect(chat.messageById('c1', '1')?.content).toBe('')
    expect(chat.messageById('c1', '1')?.deleted_at).toBeTruthy()

    mockEventsOn.fire('chat:deleted', { channel_id: 'c1', message_id: '404' })
    expect(chat.messagesFor('c1')).toHaveLength(1)
    chat.unbindEvents()
  })

  it('mergeMessage 按 id 覆盖旧副本并按 ts 旧→新排序', () => {
    const chat = useChatStore()
    chat.messagesByChannel = {
      c1: [baseMsg('3', { ts: '3000' }), baseMsg('1', { ts: '1000' })],
    }

    chat.mergeMessage('c1', baseMsg('2', { ts: '2000' }))
    expect(chat.messagesFor('c1').map((m) => m.id)).toEqual(['1', '2', '3'])

    chat.mergeMessage('c1', baseMsg('1', { ts: '1000', content: '回填后' }))
    expect(chat.messagesFor('c1')).toHaveLength(3)
    expect(chat.messageById('c1', '1')?.content).toBe('回填后')
  })

  it('@自己时用 notify 提示频道名；自己发的/未提及/重复推送不提示', () => {
    const chat = useChatStore()
    const notify = useNotifyStore()
    useAuthStore().user = { id: 'u1', nickname: '我' } as UserProfile
    useServersStore().servers = [{
      id: 's1', name: '服务器', icon: 'MessagesSquare',
      channels: [{ id: 'c1', name: '闲聊', type: 'text', group: '未分类' }],
    }]
    chat.bindEvents()

    mockEventsOn.fire('chat:message', baseMsg('1', { user_id: 'u2', mentions: ['u1'] }))
    expect(notify.visible).toBe(true)
    expect(notify.isError).toBe(false)
    expect(notify.text).toBe('有人在 #闲聊 @了你')

    // 同一条消息重推（ID 去重早退）不重复提醒
    notify.visible = false
    mockEventsOn.fire('chat:message', baseMsg('1', { user_id: 'u2', mentions: ['u1'] }))
    expect(notify.visible).toBe(false)

    // 自己发的消息即使 mentions 含自己也不提示
    mockEventsOn.fire('chat:message', baseMsg('2', { user_id: 'u1', mentions: ['u1'] }))
    expect(notify.visible).toBe(false)

    // 未提及自己不提示
    mockEventsOn.fire('chat:message', baseMsg('3', { user_id: 'u2', mentions: ['u9'] }))
    expect(notify.visible).toBe(false)
    chat.unbindEvents()
  })

  it('会话里被 @ 时用会话显示名提示', () => {
    const chat = useChatStore()
    const notify = useNotifyStore()
    useAuthStore().user = { id: 'u1', nickname: '我' } as UserProfile
    const conv: Conversation = {
      id: 'd1', kind: 'direct', name: '',
      members: [
        { user_id: 'u1', nickname: '我', tag: '0001', role: 'member', joined_at: '2026-01-01T00:00:00Z' },
        { user_id: 'u2', nickname: '乙', tag: '0002', role: 'member', joined_at: '2026-01-01T00:00:00Z' },
      ],
      created_at: '2026-01-01T00:00:00Z',
    }
    useConversationsStore().list = [conv]
    chat.bindEvents()

    mockEventsOn.fire('chat:message', baseMsg('5', {
      channel_id: 'd1', user_id: 'u2', mentions: ['u1'],
    }))

    expect(notify.text).toBe('有人在「乙」@了你')
    chat.unbindEvents()
  })

  it('loadMentionCandidates 按频道缓存，失败不缓存可重试', async () => {
    const chat = useChatStore()
    const cands = [{ user_id: 'u2', nickname: '乙', tag: '0002' }]
    mockGoApp.ListMentionCandidates.mockResolvedValueOnce(cands)

    expect(await chat.loadMentionCandidates('c1')).toEqual(cands)
    expect(await chat.loadMentionCandidates('c1')).toEqual(cands)
    expect(mockGoApp.ListMentionCandidates).toHaveBeenCalledTimes(1)
    expect(chat.mentionCandidatesFor('c1')).toEqual(cands)

    mockGoApp.ListMentionCandidates.mockRejectedValueOnce(new Error('boom'))
    expect(await chat.loadMentionCandidates('c2')).toEqual([])
    expect(chat.mentionCandidatesFor('c2')).toEqual([])

    mockGoApp.ListMentionCandidates.mockResolvedValueOnce(cands)
    expect(await chat.loadMentionCandidates('c2')).toEqual(cands)
    expect(mockGoApp.ListMentionCandidates).toHaveBeenCalledTimes(3)
  })

  it('resetAll 同时清空提及候选缓存', async () => {
    const chat = useChatStore()
    mockGoApp.ListMentionCandidates.mockResolvedValueOnce([
      { user_id: 'u2', nickname: '乙', tag: '0002' },
    ])
    await chat.loadMentionCandidates('c1')
    expect(chat.mentionCandidatesFor('c1')).toHaveLength(1)

    chat.resetAll()
    expect(chat.mentionCandidatesFor('c1')).toEqual([])
  })

  it('mergeHistory 去重时保留 #39 新字段', () => {
    const edited: ChatMessage = baseMsg('1', {
      ts: '1000',
      edited_at: '1700000000000',
      reply_to: '9',
      reply_preview: {
        id: '9', user_id: 'u3', nickname: '丙', content: '原文', deleted: false,
      },
      mentions: ['u1'],
    })

    const merged = mergeHistory([edited], [baseMsg('1', { ts: '1000', content: '实时' })])

    expect(merged).toHaveLength(1)
    expect(merged[0]).toMatchObject({
      content: 'hi',
      edited_at: '1700000000000',
      reply_to: '9',
      mentions: ['u1'],
    })
    expect(merged[0].reply_preview?.nickname).toBe('丙')
  })
})

// #40：已读上报（打开频道/会话时把最新一条消息标记为已读）。
describe('chat store 已读上报（#40）', () => {
  const m = (id: string, ts: string): ChatMessage => ({
    id, channel_id: 'c1', user_id: 'u1', nickname: '甲', content: 'hi', ts,
  })

  beforeEach(() => {
    setActivePinia(createPinia())
    mockEventsOn.reset()
    for (const fn of Object.values(mockGoApp)) fn.mockReset()
    mockGoApp.MarkChannelRead.mockResolvedValue({
      channel_id: 'c1', unread_count: 0, mention_count: 0,
    })
  })

  it('只在最新消息 ID 变化时上报', async () => {
    const chat = useChatStore()
    chat.messagesByChannel = { c1: [m('1', '1000'), m('2', '2000')] }

    await chat.markRead('c1')
    expect(mockGoApp.MarkChannelRead).toHaveBeenCalledWith('c1', '2')

    // 同一条最新消息重复触发（重渲染/重复 watch）不重复请求。
    await chat.markRead('c1')
    expect(mockGoApp.MarkChannelRead).toHaveBeenCalledTimes(1)

    chat.messagesByChannel = { c1: [m('1', '1000'), m('2', '2000'), m('3', '3000')] }
    await chat.markRead('c1')
    expect(mockGoApp.MarkChannelRead).toHaveBeenCalledTimes(2)
    expect(mockGoApp.MarkChannelRead).toHaveBeenLastCalledWith('c1', '3')
  })

  it('无消息不上报', async () => {
    const chat = useChatStore()
    await chat.markRead('c1')
    expect(mockGoApp.MarkChannelRead).not.toHaveBeenCalled()
  })

  it('成功后用返回计数覆盖 unreads store', async () => {
    const chat = useChatStore()
    const unreads = useUnreadsStore()
    unreads.channels = { c1: { unread: 5, mentions: 2 } }
    chat.messagesByChannel = { c1: [m('9', '9000')] }
    mockGoApp.MarkChannelRead.mockResolvedValue({
      channel_id: 'c1', unread_count: 0, mention_count: 0,
    })

    await chat.markRead('c1')

    expect(unreads.channel('c1')).toEqual({ unread: 0, mentions: 0 })
  })

  it('失败静默且不记录已上报：下次触发重试', async () => {
    const chat = useChatStore()
    chat.messagesByChannel = { c1: [m('1', '1000')] }
    mockGoApp.MarkChannelRead.mockRejectedValueOnce(new Error('boom'))

    await expect(chat.markRead('c1')).resolves.toBeUndefined()
    await chat.markRead('c1')

    expect(mockGoApp.MarkChannelRead).toHaveBeenCalledTimes(2)
  })

  it('resetAll 清空已上报记录，重新登录后同一条消息可再上报', async () => {
    const chat = useChatStore()
    chat.messagesByChannel = { c1: [m('1', '1000')] }
    await chat.markRead('c1')
    expect(mockGoApp.MarkChannelRead).toHaveBeenCalledTimes(1)

    chat.resetAll()
    chat.messagesByChannel = { c1: [m('1', '1000')] }
    await chat.markRead('c1')
    expect(mockGoApp.MarkChannelRead).toHaveBeenCalledTimes(2)
  })
})

// #42 B2：跨频道消息定位请求（SearchDialog 选中结果 → MainView 写入 → ChatPanel 消费）。
describe('chat store 跨频道定位（#42 B2）', () => {
  beforeEach(() => {
    setActivePinia(createPinia())
  })

  it('requestJump 记录目标并自增 seq，重复点击同一条也是新值', () => {
    const chat = useChatStore()

    chat.requestJump('c1', 'm1')
    const first = chat.jumpTarget
    expect(first).toEqual({ channelId: 'c1', messageId: 'm1', seq: 1 })

    chat.requestJump('c1', 'm1')
    expect(chat.jumpTarget).toEqual({ channelId: 'c1', messageId: 'm1', seq: 2 })
    expect(chat.jumpTarget).not.toBe(first) // 引用变化才能触发 watch
  })

  it('空 channelId / messageId 不产生请求', () => {
    const chat = useChatStore()
    chat.requestJump('', 'm1')
    chat.requestJump('c1', '')
    expect(chat.jumpTarget).toBeNull()
  })

  it('clearJump 清空；resetAll 一并清空', () => {
    const chat = useChatStore()
    chat.requestJump('c1', 'm1')
    chat.clearJump()
    expect(chat.jumpTarget).toBeNull()

    chat.requestJump('c1', 'm2')
    chat.resetAll()
    expect(chat.jumpTarget).toBeNull()
  })
})
