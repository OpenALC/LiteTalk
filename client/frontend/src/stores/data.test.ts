// data store 频道恢复（#12）：登录后 ListChannels 填回频道/分类，重启不丢列表；
// 无分类频道归「未分类」；单服拉取失败不阻断其它服，但标记 unavailable 并提示
// （403/网络错误文案不同，不再静默）；分类显示序跟服务端 position。
import { beforeEach, describe, expect, it, vi } from 'vitest'
import { createPinia, setActivePinia } from 'pinia'

const mockGoApp = vi.hoisted(() => ({
  ListServers: vi.fn(),
  ListChannels: vi.fn(),
  CreateChannel: vi.fn(),
  RenameCategory: vi.fn(async () => {}),
  UpdateCategory: vi.fn(async (id: number, name: string, position: number) => ({
    id, server_id: 0, name: name || `分类${id}`, position: position < 0 ? 0 : position,
  })),
  GetHomeServer: vi.fn(async () => ''),
  GetIntroHidden: vi.fn(async () => false),
  ListFriends: vi.fn(async (): Promise<unknown[]> => []),
}))
// eventsOn 桩：记录各事件的监听器，测试直接调用处理器模拟事件到达；
// 返回按监听器的取消函数（与 bridge 约定一致）。
const mockEventsOn = vi.hoisted(() => {
  const handlers = new Map<string, Array<(...args: unknown[]) => void>>()
  return {
    handlers,
    on(name: string, cb: (...args: unknown[]) => void) {
      const list = handlers.get(name) ?? []
      list.push(cb)
      handlers.set(name, list)
      return () => {
        handlers.set(
          name,
          (handlers.get(name) ?? []).filter((h) => h !== cb),
        )
      }
    },
    reset() {
      handlers.clear()
    },
    fire(name: string, data: unknown) {
      for (const h of handlers.get(name) ?? []) h(data)
    },
  }
})
vi.mock('../api/bridge', () => ({ goApp: () => mockGoApp, eventsOn: mockEventsOn.on }))

import { useServersStore, useFriendsStore, UNCATEGORIZED } from './data'
import { useNotifyStore } from './notify'

// resetMock 每例前清桩：mockReset 连实现一起清，需补回默认实现。
function resetMock() {
  mockGoApp.ListServers.mockReset()
  mockGoApp.ListChannels.mockReset()
  mockGoApp.CreateChannel.mockReset()
  mockGoApp.RenameCategory.mockReset().mockResolvedValue(undefined)
  mockGoApp.UpdateCategory.mockReset().mockImplementation(async (id, name, position) => ({
    id, server_id: 0, name: name || `分类${id}`, position: position < 0 ? 0 : position,
  }))
  mockGoApp.ListFriends.mockReset().mockResolvedValue([])
  mockEventsOn.reset()
}

describe('频道列表恢复', () => {
  beforeEach(() => {
    setActivePinia(createPinia())
    resetMock()
  })

  it('loadRemoteServers 逐服恢复频道与空分类', async () => {
    mockGoApp.ListServers.mockResolvedValue([
      { id: 7, name: '服七', owner_id: 1, icon_name: 'music', icon_color: '#4FA4F3' },
    ])
    mockGoApp.ListChannels.mockResolvedValue({
      categories: [
        { id: 3, server_id: 7, name: '乙组', position: 1 },
        { id: 2, server_id: 7, name: '甲组', position: 0 },
        { id: 4, server_id: 7, name: '空组', position: 2 },
      ],
      channels: [
        { id: 71, server_id: 7, name: '大厅', type: 'voice', category_id: 2, position: 0 },
        { id: 72, server_id: 7, name: '闲聊', type: 'text', category_id: 3, position: 1 },
      ],
    })
    const s = useServersStore()
    await s.loadRemoteServers()

    const srv = s.findServer('7')
    expect(srv?.channels.map((c) => c.group)).toEqual(['甲组', '乙组'])
    // 空分类不丢，且按服务端 position 排序。
    expect(s.groupsOf('7', srv!.channels).map((g) => g.name)).toEqual(['甲组', '乙组', '空组'])
  })

  it('无分类频道归「未分类」并沉底', async () => {
    mockGoApp.ListServers.mockResolvedValue([
      { id: 8, name: '服八', owner_id: 1, icon_name: 'music', icon_color: '#4FA4F3' },
    ])
    mockGoApp.ListChannels.mockResolvedValue({
      categories: [{ id: 9, server_id: 8, name: '有组', position: 0 }],
      channels: [
        { id: 81, server_id: 8, name: '散装', type: 'voice', category_id: 0, position: 0 },
        { id: 82, server_id: 8, name: '有主', type: 'voice', category_id: 9, position: 1 },
      ],
    })
    const s = useServersStore()
    await s.loadRemoteServers()

    const srv = s.findServer('8')!
    expect(srv.channels.find((c) => c.id === '81')?.group).toBe(UNCATEGORIZED)
    expect(s.groupsOf('8', srv.channels).map((g) => g.name)).toEqual(['有组', UNCATEGORIZED])
  })

  it('单服 ListChannels 403 不抛错但不再静默：标记不可用并提示被踢出', async () => {
    mockGoApp.ListServers.mockResolvedValue([
      { id: 9, name: '服九', owner_id: 1, icon_name: 'music', icon_color: '#4FA4F3' },
    ])
    // bridge 只传 message：含服务端错误码 forbidden（APIError.Error() 嵌 code）。
    mockGoApp.ListChannels.mockRejectedValue(
      new Error('get /servers/9/channels: forbidden: 不是该服务器成员'))
    const s = useServersStore()
    const notify = useNotifyStore()
    await expect(s.loadRemoteServers()).resolves.toBeUndefined()

    expect(s.findServer('9')?.channels).toEqual([])
    expect(s.findServer('9')?.unavailable).toBe(true)
    expect(notify.isError).toBe(true)
    expect(notify.text).toContain('不可访问')
    expect(notify.text).toContain('服九')
  })

  it('网络错误同样标记并提示，文案与 403 区分', async () => {
    mockGoApp.ListServers.mockResolvedValue([
      { id: 9, name: '服九', owner_id: 1, icon_name: 'music', icon_color: '#4FA4F3' },
    ])
    mockGoApp.ListChannels.mockRejectedValue(
      new Error('get /servers/9/channels: dial tcp: connection refused'))
    const s = useServersStore()
    const notify = useNotifyStore()
    await expect(s.loadRemoteServers()).resolves.toBeUndefined()

    expect(s.findServer('9')?.unavailable).toBe(true)
    expect(notify.isError).toBe(true)
    expect(notify.text).toContain('频道加载失败')
    expect(notify.text).not.toContain('不可访问')
  })

  it('加载成功清除 unavailable 标记', async () => {
    mockGoApp.ListServers.mockResolvedValue([
      { id: 9, name: '服九', owner_id: 1, icon_name: 'music', icon_color: '#4FA4F3' },
    ])
    mockGoApp.ListChannels.mockRejectedValue(new Error('forbidden: 不是该服务器成员'))
    const s = useServersStore()
    await s.loadRemoteServers()
    expect(s.findServer('9')?.unavailable).toBe(true)

    mockGoApp.ListChannels.mockResolvedValue({
      categories: [],
      channels: [{ id: 91, server_id: 9, name: '大厅', type: 'voice', category_id: 0, position: 0 }],
    })
    await s.loadRemoteServers()
    expect(s.findServer('9')?.unavailable).toBeFalsy()
    expect(s.findServer('9')?.channels).toHaveLength(1)
  })
})

describe('分类改名与排序', () => {
  beforeEach(() => {
    setActivePinia(createPinia())
    resetMock()
  })

  it('内置示例改名只改本地（含归组频道的 group），不调 REST', async () => {
    const s = useServersStore()
    await s.createCategory('design', '本地组')
    s.findServer('design')!.channels.push({ id: 'c1', name: '房', type: 'voice', group: '本地组' })
    const cat = s.extraGroups['design'].find((g) => g.name === '本地组')!

    await s.renameCategory('design', cat.id, '改过名')

    expect(s.extraGroups['design'][0].name).toBe('改过名')
    expect(s.findServer('design')!.channels[0].group).toBe('改过名')
    expect(mockGoApp.RenameCategory).not.toHaveBeenCalled()
  })

  it('真实分类改名走 PATCH 并同步频道 group', async () => {
    mockGoApp.ListServers.mockResolvedValue([
      { id: 21, name: '服21', owner_id: 1, icon_name: 'music', icon_color: '#4FA4F3' },
    ])
    mockGoApp.ListChannels.mockResolvedValue({
      categories: [{ id: 211, server_id: 21, name: '旧名', position: 0 }],
      channels: [{ id: 2101, server_id: 21, name: '房', type: 'voice', category_id: 211, position: 0 }],
    })
    const s = useServersStore()
    await s.loadRemoteServers()

    await s.renameCategory('21', '211', '新名')

    expect(mockGoApp.RenameCategory).toHaveBeenCalledWith(211, '新名')
    expect(s.extraGroups['21'][0].name).toBe('新名')
    expect(s.findServer('21')!.channels[0].group).toBe('新名')
  })

  it('本地排序：移到目标序并重排 position，不调 REST', async () => {
    const s = useServersStore()
    await s.createCategory('design', '甲')
    await s.createCategory('design', '乙')
    await s.createCategory('design', '丙')
    const ids = s.extraGroups['design'].map((g) => g.id)

    await s.reorderCategory('design', ids[2], 0)

    expect(s.extraGroups['design'].map((g) => g.name)).toEqual(['丙', '甲', '乙'])
    expect(s.extraGroups['design'].map((g) => g.position)).toEqual([0, 1, 2])
    expect(mockGoApp.UpdateCategory).not.toHaveBeenCalled()
  })

  it('真实分类排序乐观更新，失败回滚顺序', async () => {
    mockGoApp.ListServers.mockResolvedValue([
      { id: 22, name: '服22', owner_id: 1, icon_name: 'music', icon_color: '#4FA4F3' },
    ])
    mockGoApp.ListChannels.mockResolvedValue({
      categories: [
        { id: 1, server_id: 22, name: '甲', position: 0 },
        { id: 2, server_id: 22, name: '乙', position: 1 },
        { id: 3, server_id: 22, name: '丙', position: 2 },
      ],
      channels: [],
    })
    const s = useServersStore()
    await s.loadRemoteServers()

    mockGoApp.UpdateCategory.mockRejectedValue(new Error('500'))
    await expect(s.reorderCategory('22', '3', 0)).rejects.toThrow('500')
    expect(s.extraGroups['22'].map((g) => g.name)).toEqual(['甲', '乙', '丙'])

    mockGoApp.UpdateCategory.mockResolvedValue({ id: 3, server_id: 22, name: '丙', position: 0 })
    await s.reorderCategory('22', '3', 0)
    expect(mockGoApp.UpdateCategory).toHaveBeenCalledWith(3, '', 0)
    expect(s.extraGroups['22'].map((g) => g.name)).toEqual(['丙', '甲', '乙'])
    expect(s.groupsOf('22', []).map((g) => g.name)).toEqual(['丙', '甲', '乙'])
  })
})

// 好友在线点（#11）：user.online 是唯一数据源（两态），presence:update 增量移组。
describe('好友在线态', () => {
  beforeEach(() => {
    setActivePinia(createPinia())
    resetMock()
  })

  const friendDTO = (id: number, uid: number, online: boolean) => ({
    id,
    status: 'accepted',
    incoming: false,
    remark: '',
    group: '',
    user: { id: uid, nickname: `用户${uid}`, tag: '0001', status: 1, online },
  })

  it('loadFriends 按 user.online 分组，status 收敛为两态', async () => {
    mockGoApp.ListFriends.mockResolvedValue([
      friendDTO(1, 10, true),
      friendDTO(2, 20, false),
      { ...friendDTO(3, 30, true), status: 'pending', incoming: true },
    ])
    const s = useFriendsStore()
    await s.loadFriends()
    expect(s.online.map((f) => f.userId)).toEqual([10])
    expect(s.offline.map((f) => f.userId)).toEqual([20])
    expect(s.online[0].status).toBe('online')
    expect(s.online[0].sub).toBe('在线')
    expect(s.offline[0].status).toBe('offline')
    expect(s.offline[0].sub).toBe('离线')
    expect(s.pending.map((p) => p.id)).toEqual([3])
  })

  it('presence:update 增量移组并保持排序，未知用户忽略', async () => {
    mockGoApp.ListFriends.mockResolvedValue([friendDTO(1, 10, true), friendDTO(2, 20, false)])
    const s = useFriendsStore()
    await s.loadFriends()
    s.bindEvents()

    mockEventsOn.fire('presence:update', { user_id: '20', online: true })
    expect(s.online.map((f) => f.userId)).toEqual([10, 20])
    expect(s.offline).toEqual([])
    expect(s.online[1].sub).toBe('在线')

    mockEventsOn.fire('presence:update', { user_id: '10', online: false })
    expect(s.offline.map((f) => f.userId)).toEqual([10])
    expect(s.online.map((f) => f.userId)).toEqual([20])

    // 未知用户 / 坏包不抛错不改状态。
    mockEventsOn.fire('presence:update', { user_id: '999', online: true })
    mockEventsOn.fire('presence:update', null)
    expect(s.online.map((f) => f.userId)).toEqual([20])
    expect(s.offline.map((f) => f.userId)).toEqual([10])
  })

  it('unbindEvents 注销后不再收 presence:update', async () => {
    mockGoApp.ListFriends.mockResolvedValue([friendDTO(1, 10, false)])
    const s = useFriendsStore()
    await s.loadFriends()
    s.bindEvents()
    s.unbindEvents()
    mockEventsOn.fire('presence:update', { user_id: '10', online: true })
    expect(s.online).toEqual([])
    expect(s.offline.map((f) => f.userId)).toEqual([10])
  })
})

// 新建频道归入分类（#72）：显式 category_id 必须带给服务端，本地 group 同时取
// 分类名——#16 的按名归组靠 group 名落位，只带 id 不改名仍会掉进类型默认组。
describe('新建频道归入分类', () => {
  beforeEach(() => {
    setActivePinia(createPinia())
    resetMock()
    mockGoApp.ListServers.mockResolvedValue([
      { id: 31, name: '服31', owner_id: 1, icon_name: 'music', icon_color: '#4FA4F3' },
    ])
    mockGoApp.ListChannels.mockResolvedValue({
      categories: [
        { id: 311, server_id: 31, name: '甲组', position: 0 },
        { id: 312, server_id: 31, name: '乙组', position: 1 },
      ],
      channels: [],
    })
    mockGoApp.CreateChannel.mockImplementation(
      async (serverID: number, name: string, typ: string, categoryID: number) => ({
        id: 3101, server_id: serverID, name, type: typ, category_id: categoryID || null,
      }),
    )
  })

  it('显式分类：category_id 带给服务端，group 取分类名即可落位', async () => {
    const s = useServersStore()
    await s.loadRemoteServers()

    await s.createChannel('31', '开黑', 'voice', '312')

    expect(mockGoApp.CreateChannel).toHaveBeenCalledWith(31, '开黑', 'voice', 312)
    const ch = s.findServer('31')!.channels[0]
    expect(ch.group).toBe('乙组')
    // 不重启就已在目标分类下（groupsOf 按名归组命中真实分类）。
    expect(s.groupsOf('31', s.findServer('31')!.channels).map((g) => g.name))
      .toEqual(['甲组', '乙组'])
  })

  it('缺省分类走旧路径：不带 category_id，落类型默认组名', async () => {
    const s = useServersStore()
    await s.loadRemoteServers()

    await s.createChannel('31', '散装', 'voice')

    expect(mockGoApp.CreateChannel).toHaveBeenCalledWith(31, '散装', 'voice', 0)
    expect(s.findServer('31')!.channels[0].group).toBe('语音频道')
  })

  it('显式「未分类」不算缺省：group 落「未分类」，category_id 传 0', async () => {
    const s = useServersStore()
    await s.loadRemoteServers()

    await s.createChannel('31', '野队', 'text', '')

    expect(mockGoApp.CreateChannel).toHaveBeenCalledWith(31, '野队', 'text', 0)
    expect(s.findServer('31')!.channels[0].group).toBe(UNCATEGORIZED)
  })

  it('找不到分类名时归「未分类」，不造类型默认假分组', async () => {
    const s = useServersStore()
    await s.loadRemoteServers()

    await s.createChannel('31', '越界', 'voice', '999')

    expect(mockGoApp.CreateChannel).toHaveBeenCalledWith(31, '越界', 'voice', 999)
    expect(s.findServer('31')!.channels[0].group).toBe(UNCATEGORIZED)
  })
})
