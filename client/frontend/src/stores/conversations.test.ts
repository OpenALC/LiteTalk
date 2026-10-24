// conversations store（#38 B4b）：会话列表/私信模式/发起私聊/群聊管理/事件刷新。
// bridge 桩只覆盖 store 实际调用的绑定；事件用可手动触发的监听器表。
import { beforeEach, describe, expect, it, vi } from 'vitest'
import { createPinia, setActivePinia } from 'pinia'

const mockGoApp = vi.hoisted(() => ({
  ListConversations: vi.fn(),
  CreateDirectConversation: vi.fn(),
  CreateGroupConversation: vi.fn(),
  GetConversation: vi.fn(),
  RenameConversation: vi.fn(),
  DeleteConversation: vi.fn(),
  AddConversationMembers: vi.fn(),
  RemoveConversationMember: vi.fn(),
}))

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
vi.mock('../api/bridge', () => ({ goApp: () => mockGoApp, eventsOn: mockEventsOn.on }))

import type { Conversation, ConversationMember, UserProfile } from '../api/types'
import { useAuthStore } from './auth'
import { useConversationsStore } from './conversations'

const selfUser: UserProfile = {
  id: '1', nickname: '我', tag: '0001', email: 'me@example.com',
  email_verified: true, is_guest: false, status: 0, online: true,
  created_at: '2026-01-01T00:00:00Z', updated_at: '2026-01-01T00:00:00Z',
}

function member(id: string, nickname: string, role: 'owner' | 'member' = 'member'): ConversationMember {
  return { user_id: id, nickname, tag: '0001', role, joined_at: '2026-01-01T00:00:00Z' }
}

function conv(id: string, kind: 'direct' | 'group', name: string,
              members: ConversationMember[]): Conversation {
  return { id, kind, name, members, created_at: '2026-01-01T00:00:00Z' }
}

const me = member('1', '我')
const peer = member('2', '甲')
const direct = conv('100', 'direct', '', [me, peer])
const group = conv('300', 'group', '开黑群', [member('1', '我', 'owner'), peer, member('3', '乙')])

// login 会话身份：displayName/isOwner 按 auth.user.id 排除自己。
function login() {
  useAuthStore().user = selfUser
}

function resetMocks() {
  for (const fn of Object.values(mockGoApp)) fn.mockReset()
}

describe('conversations store', () => {
  beforeEach(() => {
    setActivePinia(createPinia())
    mockEventsOn.reset()
    resetMocks()
    login()
  })

  it('load 填充列表；当前会话不在新列表时清空选择', async () => {
    mockGoApp.ListConversations.mockResolvedValue([direct, group])
    const s = useConversationsStore()

    await s.load()
    expect(s.list.map((c) => c.id)).toEqual(['100', '300'])

    s.currentId = '100'
    mockGoApp.ListConversations.mockResolvedValue([group])
    await s.load()
    expect(s.currentId).toBe('')
  })

  it('enterDM 进入私信模式并刷新列表；exitDM 退出并清空当前会话', async () => {
    mockGoApp.ListConversations.mockResolvedValue([direct])
    const s = useConversationsStore()

    s.enterDM()
    expect(s.dmMode).toBe(true)
    await vi.waitFor(() => expect(s.list).toHaveLength(1))

    s.currentId = '100'
    s.exitDM()
    expect(s.dmMode).toBe(false)
    expect(s.currentId).toBe('')
  })

  it('displayName：direct 显示对方昵称；group 空名回退成员拼接；isOwner 按自身角色', () => {
    const s = useConversationsStore()

    expect(s.displayName(direct)).toBe('甲')
    expect(s.displayName(conv('101', 'group', '', [me, peer, member('3', '乙')]))).toBe('甲、乙')
    expect(s.displayName(conv('102', 'group', '  ', [me]))).toBe('我')
    expect(s.displayName(group)).toBe('开黑群')
    expect(s.isOwner(group)).toBe(true)
    expect(s.isOwner(direct)).toBe(false)
    expect(s.memberCount(group)).toBe(3)
  })

  it('openDirect 创建/复用会话并切到私信模式当前会话', async () => {
    mockGoApp.CreateDirectConversation.mockResolvedValue(direct)
    const s = useConversationsStore()

    const c = await s.openDirect('2')

    expect(mockGoApp.CreateDirectConversation).toHaveBeenCalledWith('2')
    expect(c.id).toBe('100')
    expect(s.dmMode).toBe(true)
    expect(s.currentId).toBe('100')
    expect(s.list.map((x) => x.id)).toEqual(['100'])
  })

  it('createGroup 创建群聊并选中', async () => {
    mockGoApp.CreateGroupConversation.mockResolvedValue(group)
    const s = useConversationsStore()

    await s.createGroup('开黑群', ['2', '3'])

    expect(mockGoApp.CreateGroupConversation).toHaveBeenCalledWith('开黑群', ['2', '3'])
    expect(s.dmMode).toBe(true)
    expect(s.currentId).toBe('300')
    expect(s.list).toHaveLength(1)
  })

  it('open 列表缺失时拉详情；失败清空当前选择并上抛', async () => {
    const s = useConversationsStore()
    mockGoApp.GetConversation.mockResolvedValueOnce(group)
    await s.open('300')
    expect(s.currentId).toBe('300')
    expect(s.list.map((c) => c.id)).toEqual(['300'])

    mockGoApp.GetConversation.mockRejectedValueOnce(new Error('not_found'))
    await expect(s.open('999')).rejects.toThrow('not_found')
    expect(s.currentId).toBe('')
  })

  it('rename/addMembers 用服务端返回更新列表', async () => {
    mockGoApp.ListConversations.mockResolvedValue([group])
    const s = useConversationsStore()
    await s.load()

    mockGoApp.RenameConversation.mockResolvedValue({ ...group, name: '新群名' })
    await s.rename('300', '新群名')
    expect(s.list[0].name).toBe('新群名')

    const added = { ...group, members: [...group.members, member('4', '丙')] }
    mockGoApp.AddConversationMembers.mockResolvedValue(added)
    await s.addMembers('300', ['4'])
    expect(s.memberCount(s.list[0])).toBe(4)
  })

  it('removeMember 移除他人更新列表；自己退出后即使返回会话也移除', async () => {
    mockGoApp.ListConversations.mockResolvedValue([group])
    const s = useConversationsStore()
    await s.load()
    s.currentId = '300'

    const withoutPeer = { ...group, members: [group.members[0], group.members[2]] }
    mockGoApp.RemoveConversationMember.mockResolvedValue(withoutPeer)
    await s.removeMember('300', '2')
    expect(s.memberCount(s.list[0])).toBe(2)
    expect(s.currentId).toBe('300')

    // 自己退出：服务端返回剩余会话（本端已非成员），本地必须移除并清空当前。
    mockGoApp.RemoveConversationMember.mockResolvedValue({ ...group, members: [peer, member('3', '乙')] })
    await s.leave('300')
    expect(s.list).toEqual([])
    expect(s.currentId).toBe('')
  })

  it('delete 移除会话并清空当前选择', async () => {
    mockGoApp.ListConversations.mockResolvedValue([direct, group])
    mockGoApp.DeleteConversation.mockResolvedValue(undefined)
    const s = useConversationsStore()
    await s.load()
    s.currentId = '100'

    await s.delete('100')

    expect(mockGoApp.DeleteConversation).toHaveBeenCalledWith('100')
    expect(s.list.map((c) => c.id)).toEqual(['300'])
    expect(s.currentId).toBe('')
  })

  it('applyChanged deleted：重拉列表后清空已删的当前会话', async () => {
    const s = useConversationsStore()
    s.list = [direct]
    s.currentId = '100'
    s.dmMode = true
    mockGoApp.ListConversations.mockResolvedValue([])

    await s.applyChanged('100', 'deleted')

    expect(s.list).toEqual([])
    expect(s.currentId).toBe('')
    expect(s.dmMode).toBe(true) // 留在私信列表，不踢回服务器界面
  })

  it('applyChanged member_removed 自己被移出：列表消失且清空当前', async () => {
    const s = useConversationsStore()
    s.list = [group]
    s.currentId = '300'
    mockGoApp.ListConversations.mockResolvedValue([])

    await s.applyChanged('300', 'member_removed')

    expect(s.currentId).toBe('')
    expect(s.list).toEqual([])
  })

  it('applyChanged updated：当前会话重拉详情', async () => {
    const s = useConversationsStore()
    s.list = [group]
    s.currentId = '300'
    const renamed = { ...group, name: '新群名' }
    mockGoApp.ListConversations.mockResolvedValue([group])
    mockGoApp.GetConversation.mockResolvedValue(renamed)

    await s.applyChanged('300', 'updated')

    expect(mockGoApp.GetConversation).toHaveBeenCalledWith('300')
    expect(s.list[0].name).toBe('新群名')
  })

  it('bindEvents 幂等；conversation:changed/data:resync 触发刷新；unbind 逐个注销', async () => {
    const s = useConversationsStore()
    mockGoApp.ListConversations.mockResolvedValue([])

    s.bindEvents()
    s.bindEvents()
    expect(mockEventsOn.handlers.get('conversation:changed')).toHaveLength(1)
    expect(mockEventsOn.handlers.get('data:resync')).toHaveLength(1)

    mockEventsOn.fire('data:resync', null)
    await vi.waitFor(() => expect(mockGoApp.ListConversations).toHaveBeenCalledTimes(1))

    mockEventsOn.fire('conversation:changed', { conversation_id: '100', action: 'created' })
    await vi.waitFor(() => expect(mockGoApp.ListConversations).toHaveBeenCalledTimes(2))

    s.unbindEvents()
    expect(mockEventsOn.handlers.get('conversation:changed')).toHaveLength(0)
    expect(mockEventsOn.handlers.get('data:resync')).toHaveLength(0)
  })

  it('resetAll 清空会话/私信模式/当前选择，幂等', async () => {
    const s = useConversationsStore()
    s.list = [direct, group]
    s.currentId = '100'
    s.dmMode = true

    s.resetAll()
    expect(s.list).toEqual([])
    expect(s.currentId).toBe('')
    expect(s.dmMode).toBe(false)

    s.resetAll()
    expect(s.list).toEqual([])
  })
})
