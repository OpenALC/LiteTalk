// data store 频道恢复（#12）：登录后 ListChannels 填回频道/分类，重启不丢列表；
// 无分类频道归「未分类」；单服拉取失败不阻断其它服，但标记 unavailable 并提示
// （403/网络错误文案不同，不再静默）；分类显示序跟服务端 position。
import { beforeEach, describe, expect, it, vi } from 'vitest'
import { createPinia, setActivePinia } from 'pinia'

const mockGoApp = vi.hoisted(() => ({
  ListServers: vi.fn(),
  ListChannels: vi.fn(),
  CreateChannel: vi.fn(),
  CreateServer: vi.fn(),
  JoinServerByCode: vi.fn(),
  RenameCategory: vi.fn(async () => {}),
  UpdateCategory: vi.fn(async (id: string, name: string, position: number) => ({
    id, server_id: '0', name: name || `分类${id}`, position: position < 0 ? 0 : position,
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
  mockGoApp.CreateServer.mockReset()
  mockGoApp.JoinServerByCode.mockReset()
  mockGoApp.RenameCategory.mockReset().mockResolvedValue(undefined)
  mockGoApp.UpdateCategory.mockReset().mockImplementation(async (id, name, position) => ({
    id, server_id: '0', name: name || `分类${id}`, position: position < 0 ? 0 : position,
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

  // ListServers 失败不再静默（#12）：toast 提示，串行链不断，下一次加载照常。
  it('ListServers 失败时提示且不阻断后续加载', async () => {
    mockGoApp.ListServers.mockRejectedValueOnce(
      new Error('get /servers: dial tcp 127.0.0.1:8077: connectex: No connection could be made'))
    const s = useServersStore()
    const notify = useNotifyStore()

    await expect(s.loadRemoteServers()).resolves.toBeUndefined()
    expect(notify.visible).toBe(true)
    expect(notify.isError).toBe(true)
    expect(notify.text).toBe('服务器列表加载失败')
    // 列表保持为空，不再回落到示例服务器。
    expect(s.servers).toEqual([])

    mockGoApp.ListServers.mockResolvedValueOnce([
      { id: 7, name: '服七', owner_id: 1, icon_name: 'music', icon_color: '#4FA4F3' },
    ])
    mockGoApp.ListChannels.mockResolvedValue({ categories: [], channels: [] })
    await s.loadRemoteServers()
    expect(s.findServer('7')?.name).toBe('服七')
  })

  it('ListServers 成功时不弹提示', async () => {
    mockGoApp.ListServers.mockResolvedValue([])
    const s = useServersStore()
    const notify = useNotifyStore()

    await s.loadRemoteServers()

    expect(notify.visible).toBe(false)
  })

  it('旧会话在飞的 ListServers 失败不提示', async () => {
    let rejectList!: (e: Error) => void
    mockGoApp.ListServers.mockReturnValue(new Promise((_res, rej) => { rejectList = rej }))
    const s = useServersStore()
    const notify = useNotifyStore()

    const p = s.loadRemoteServers()
    // 让串行链启动、ListServers 真正在飞，再复位会话。
    await Promise.resolve()
    s.resetAll()
    rejectList(new Error('RuntimeError: get /servers: dial tcp: connection refused'))
    await expect(p).resolves.toBeUndefined()

    expect(notify.visible).toBe(false)
  })
})

describe('分类改名与排序', () => {
  beforeEach(() => {
    setActivePinia(createPinia())
    resetMock()
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

    expect(mockGoApp.RenameCategory).toHaveBeenCalledWith('211', '新名')
    expect(s.extraGroups['21'][0].name).toBe('新名')
    expect(s.findServer('21')!.channels[0].group).toBe('新名')
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

    mockGoApp.UpdateCategory.mockResolvedValue({ id: '3', server_id: '22', name: '丙', position: 0 })
    await s.reorderCategory('22', '3', 0)
    expect(mockGoApp.UpdateCategory).toHaveBeenCalledWith('3', '', 0)
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

  const friendDTO = (id: number | string, uid: number | string, online: boolean) => ({
    id: String(id),
    status: 'accepted',
    incoming: false,
    remark: '',
    group: '',
    user: { id: String(uid), nickname: `用户${uid}`, tag: '0001', status: 1, online },
  })

  it('loadFriends 按 user.online 分组，status 收敛为两态', async () => {
    mockGoApp.ListFriends.mockResolvedValue([
      friendDTO(1, 10, true),
      friendDTO(2, 20, false),
      { ...friendDTO(3, 30, true), status: 'pending', incoming: true },
    ])
    const s = useFriendsStore()
    await s.loadFriends()
    expect(s.online.map((f) => f.userId)).toEqual(['10'])
    expect(s.offline.map((f) => f.userId)).toEqual(['20'])
    expect(s.online[0].status).toBe('online')
    expect(s.online[0].sub).toBe('在线')
    expect(s.offline[0].status).toBe('offline')
    expect(s.offline[0].sub).toBe('离线')
    expect(s.pending.map((p) => p.id)).toEqual(['3'])
  })

  it('presence:update 增量移组并保持排序，未知用户忽略', async () => {
    mockGoApp.ListFriends.mockResolvedValue([friendDTO(1, 10, true), friendDTO(2, 20, false)])
    const s = useFriendsStore()
    await s.loadFriends()
    s.bindEvents()

    mockEventsOn.fire('presence:update', { user_id: '20', online: true })
    expect(s.online.map((f) => f.userId)).toEqual(['10', '20'])
    expect(s.offline).toEqual([])
    expect(s.online[1].sub).toBe('在线')

    mockEventsOn.fire('presence:update', { user_id: '10', online: false })
    expect(s.offline.map((f) => f.userId)).toEqual(['10'])
    expect(s.online.map((f) => f.userId)).toEqual(['20'])

    // 未知用户 / 坏包不抛错不改状态。
    mockEventsOn.fire('presence:update', { user_id: '999', online: true })
    mockEventsOn.fire('presence:update', null)
    expect(s.online.map((f) => f.userId)).toEqual(['20'])
    expect(s.offline.map((f) => f.userId)).toEqual(['10'])
  })

  it('unbindEvents 注销后不再收 presence:update', async () => {
    mockGoApp.ListFriends.mockResolvedValue([friendDTO(1, 10, false)])
    const s = useFriendsStore()
    await s.loadFriends()
    s.bindEvents()
    s.unbindEvents()
    mockEventsOn.fire('presence:update', { user_id: '10', online: true })
    expect(s.online).toEqual([])
    expect(s.offline.map((f) => f.userId)).toEqual(['10'])
  })
})

// data:resync（#78）：续传失败后 Go 侧提示 UI 重新拉取服务器/频道与好友视图。
describe('data:resync 刷新', () => {
  beforeEach(() => {
    setActivePinia(createPinia())
    resetMock()
  })

  const serverDTO = (id: number, name: string) => ({
    id: String(id), name, owner_id: '1', icon_name: 'music', icon_color: '#4FA4F3',
  })

  it('servers store 收到 data:resync 后重拉服务器/频道，更新名称并剔除已失去的服务器', async () => {
    mockGoApp.ListServers.mockResolvedValueOnce([serverDTO(7, '旧名'), serverDTO(8, '将被移出')])
    mockGoApp.ListChannels.mockResolvedValue({ categories: [], channels: [] })
    const s = useServersStore()
    await s.loadRemoteServers()
    s.selectServer('8')
    expect(s.findServer('8')).toBeTruthy()

    s.bindEvents()
    mockGoApp.ListServers.mockResolvedValueOnce([serverDTO(7, '新名')])
    mockEventsOn.fire('data:resync', null)

    await vi.waitFor(() => expect(mockGoApp.ListServers).toHaveBeenCalledTimes(2))
    await vi.waitFor(() => expect(s.findServer('7')?.name).toBe('新名'))
    await vi.waitFor(() => expect(s.findServer('8')).toBeUndefined())
    expect(mockGoApp.ListChannels).toHaveBeenLastCalledWith('7')
    // 当前选中的服务器被移出：回退到第一个可用服务器。
    expect(s.currentServerId).not.toBe('8')
    expect(s.findServer(s.currentServerId)).toBeTruthy()
    s.unbindEvents()
  })

  it('servers store unbindEvents 后不再响应 data:resync', async () => {
    mockGoApp.ListServers.mockResolvedValue([serverDTO(7, '服七')])
    mockGoApp.ListChannels.mockResolvedValue({ categories: [], channels: [] })
    const s = useServersStore()
    await s.loadRemoteServers()
    expect(mockGoApp.ListServers).toHaveBeenCalledTimes(1)

    s.bindEvents()
    s.unbindEvents()
    mockEventsOn.fire('data:resync', null)
    await Promise.resolve()
    expect(mockGoApp.ListServers).toHaveBeenCalledTimes(1)
  })

  it('friends store 收到 data:resync 后重拉好友列表', async () => {
    const friendDTO = (id: number, uid: number, online: boolean) => ({
      id: String(id), status: 'accepted', incoming: false, remark: '', group: '',
      user: { id: String(uid), nickname: `用户${uid}`, tag: '0001', status: 1, online },
    })
    mockGoApp.ListFriends.mockResolvedValue([friendDTO(1, 10, true)])
    const s = useFriendsStore()
    await s.loadFriends()
    s.bindEvents()

    mockGoApp.ListFriends.mockResolvedValue([friendDTO(2, 20, false)])
    mockEventsOn.fire('data:resync', null)
    await vi.waitFor(() => expect(mockGoApp.ListFriends).toHaveBeenCalledTimes(2))
    await vi.waitFor(() => expect(s.offline.map((f) => f.userId)).toEqual(['20']))
    s.unbindEvents()
  })

  it('friends store unbindEvents 同时注销 presence:update 与 data:resync', async () => {
    const s = useFriendsStore()
    s.bindEvents()
    s.unbindEvents()
    mockGoApp.ListFriends.mockResolvedValue([])
    mockEventsOn.fire('data:resync', null)
    await Promise.resolve()
    expect(mockGoApp.ListFriends).not.toHaveBeenCalled()
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
      async (serverID: string, name: string, typ: string, categoryID: string) => ({
        id: '3101', server_id: serverID, name, type: typ, category_id: categoryID || null,
      }),
    )
  })

  it('显式分类：category_id 带给服务端，group 取分类名即可落位', async () => {
    const s = useServersStore()
    await s.loadRemoteServers()

    await s.createChannel('31', '开黑', 'voice', '312')

    expect(mockGoApp.CreateChannel).toHaveBeenCalledWith('31', '开黑', 'voice', '312')
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

    expect(mockGoApp.CreateChannel).toHaveBeenCalledWith('31', '散装', 'voice', '')
    expect(s.findServer('31')!.channels[0].group).toBe('语音频道')
  })

  it('显式「未分类」不算缺省：group 落「未分类」，category_id 传空', async () => {
    const s = useServersStore()
    await s.loadRemoteServers()

    await s.createChannel('31', '野队', 'text', '')

    expect(mockGoApp.CreateChannel).toHaveBeenCalledWith('31', '野队', 'text', '')
    expect(s.findServer('31')!.channels[0].group).toBe(UNCATEGORIZED)
  })

  it('找不到分类名时归「未分类」，不造类型默认假分组', async () => {
    const s = useServersStore()
    await s.loadRemoteServers()

    await s.createChannel('31', '越界', 'voice', '999')

    expect(mockGoApp.CreateChannel).toHaveBeenCalledWith('31', '越界', 'voice', '999')
    expect(s.findServer('31')!.channels[0].group).toBe(UNCATEGORIZED)
  })
})

// 业务 store 统一复位（#85）：登出/被踢/换服后不能再残留上一账号的服务器、
// 好友与分类缓存；在飞的旧请求返回时也不得把刚清空的数据写回。
describe('store 统一复位', () => {
  beforeEach(() => {
    setActivePinia(createPinia())
    resetMock()
  })

  const serverDTO = (id: number, name: string) => ({
    id: String(id), name, owner_id: '1', icon_name: 'music', icon_color: '#4FA4F3',
  })

  it('servers.resetAll 清空服务器与会话选中/分类缓存，保留客户端偏好', async () => {
    mockGoApp.ListServers.mockResolvedValue([serverDTO(7, '服七')])
    mockGoApp.ListChannels.mockResolvedValue({
      categories: [{ id: 71, server_id: 7, name: '甲组', position: 0 }],
      channels: [{ id: 72, server_id: 7, name: '大厅', type: 'voice', category_id: 71, position: 0 }],
    })
    const s = useServersStore()
    await s.loadRemoteServers()
    s.selectServer('7')
    s.homeId = '7'
    s.introHidden = true

    s.resetAll()

    // 没有内置示例可回退：登出后列表为空，登录后再由 loadRemoteServers 填充。
    expect(s.servers).toEqual([])
    expect(s.findServer('7')).toBeUndefined()
    expect(s.currentServerId).toBe('')
    expect(s.currentChannelId).toBe('')
    expect(s.extraGroups).toEqual({})
    // homeId / introHidden 是 config.json 的客户端级偏好，不属于账号会话。
    expect(s.homeId).toBe('7')
    expect(s.introHidden).toBe(true)
  })

  it('servers.resetAll 幂等：重复调用不报错且仍为空列表', () => {
    const s = useServersStore()
    s.servers.push({ id: '7', name: '服七', icon: 'Music', channels: [] })
    s.selectServer('7')

    s.resetAll()
    s.resetAll()

    expect(s.servers).toEqual([])
    expect(s.currentServerId).toBe('')
    expect(s.currentChannelId).toBe('')
  })

  it('servers.resetAll 后旧账号在飞的 loadRemoteServers 不写回', async () => {
    let resolveList!: (v: unknown[]) => void
    mockGoApp.ListServers.mockReturnValue(new Promise((res) => { resolveList = res }))
    const s = useServersStore()

    const p = s.loadRemoteServers()
    s.resetAll()
    resolveList([serverDTO(9, '旧账号服务器')])
    await p

    expect(s.findServer('9')).toBeUndefined()
    expect(mockGoApp.ListChannels).not.toHaveBeenCalled()
  })

  it('friends.resetAll 清空在线/离线/待处理，幂等', async () => {
    const friendDTO = (id: number, uid: number, online: boolean) => ({
      id: String(id), status: 'accepted', incoming: false, remark: '', group: '',
      user: { id: String(uid), nickname: `用户${uid}`, tag: '0001', status: 1, online },
    })
    mockGoApp.ListFriends.mockResolvedValue([friendDTO(1, 10, true), friendDTO(2, 20, false)])
    const s = useFriendsStore()
    await s.loadFriends()
    s.pending = [{ id: '3', name: '待处理', outgoing: true }]

    s.resetAll()

    expect(s.online).toEqual([])
    expect(s.offline).toEqual([])
    expect(s.pending).toEqual([])

    s.resetAll() // 幂等
    expect(s.online).toEqual([])
  })

  it('friends.resetAll 后旧账号在飞的 loadFriends 不写回', async () => {
    let resolveList!: (v: unknown[]) => void
    mockGoApp.ListFriends.mockReturnValue(new Promise((res) => { resolveList = res }))
    const s = useFriendsStore()

    const p = s.loadFriends()
    s.resetAll()
    resolveList([{
      id: '1', status: 'accepted', incoming: false, remark: '', group: '',
      user: { id: '10', nickname: '用户10', tag: '0001', status: 1, online: true },
    }])
    await p

    expect(s.online).toEqual([])
    expect(s.offline).toEqual([])
    expect(s.pending).toEqual([])
  })
})

// 加入/创建服务器后立即加载频道与分类（#91）：服务端允许任意成员读取频道，
// 此前只有登录/重连会 loadChannels，好友加入后永远看不到频道/分类。
describe('加入/创建服务器后加载频道', () => {
  beforeEach(() => {
    setActivePinia(createPinia())
    resetMock()
  })

  const serverDTO = (id: number, name: string) => ({
    id, name, owner_id: 1, icon_name: 'music', icon_color: '#4FA4F3',
  })

  it('joinServer 成功后拉取频道与分类，并选中第一个频道', async () => {
    mockGoApp.JoinServerByCode.mockResolvedValue(serverDTO(41, '服41'))
    mockGoApp.ListChannels.mockResolvedValue({
      categories: [{ id: 411, server_id: 41, name: '甲组', position: 0 }],
      channels: [{ id: 4101, server_id: 41, name: '大厅', type: 'voice', category_id: 411, position: 0 }],
    })
    const s = useServersStore()

    await s.joinServer('invite-code')

    expect(mockGoApp.JoinServerByCode).toHaveBeenCalledWith('invite-code')
    expect(mockGoApp.ListChannels).toHaveBeenCalledWith('41')
    // #37 B2 起 Channel 带 parentId/hasPassword/userLimit（服务端未给 = 默认值）。
    expect(s.findServer('41')!.channels).toEqual([
      { id: '4101', name: '大厅', type: 'voice', group: '甲组', parentId: undefined, hasPassword: false, userLimit: 0 },
    ])
    expect(s.extraGroups['41'].map((g) => g.name)).toEqual(['甲组'])
    expect(s.currentServerId).toBe('41')
    expect(s.currentChannelId).toBe('4101')
  })

  it('重复加入（服务器已在本地）同样拉取频道', async () => {
    const s = useServersStore()
    s.servers.push({ id: '41', name: '旧名', icon: 'Music', channels: [] })
    mockGoApp.JoinServerByCode.mockResolvedValue(serverDTO(41, '服41'))
    mockGoApp.ListChannels.mockResolvedValue({
      categories: [],
      channels: [{ id: 4102, server_id: 41, name: '闲聊', type: 'text', category_id: 0, position: 0 }],
    })

    await s.joinServer('invite-code')

    expect(mockGoApp.ListChannels).toHaveBeenCalledWith('41')
    expect(s.findServer('41')!.channels.map((c) => c.id)).toEqual(['4102'])
    expect(s.currentChannelId).toBe('4102')
  })

  it('createServer 同样拉取频道与空分类', async () => {
    mockGoApp.CreateServer.mockResolvedValue(serverDTO(42, '新服'))
    mockGoApp.ListChannels.mockResolvedValue({
      categories: [{ id: 421, server_id: 42, name: '空组', position: 0 }],
      channels: [],
    })
    const s = useServersStore()

    await s.createServer('新服')

    expect(mockGoApp.CreateServer).toHaveBeenCalledWith('新服')
    expect(mockGoApp.ListChannels).toHaveBeenCalledWith('42')
    expect(s.findServer('42')!.channels).toEqual([])
    expect(s.extraGroups['42'].map((g) => g.name)).toEqual(['空组'])
    expect(s.currentServerId).toBe('42')
  })

  it('频道拉取失败不阻断加入：标记不可用并提示', async () => {
    mockGoApp.JoinServerByCode.mockResolvedValue(serverDTO(43, '服43'))
    mockGoApp.ListChannels.mockRejectedValue(new Error('dial tcp: connection refused'))
    const s = useServersStore()
    const notify = useNotifyStore()

    await expect(s.joinServer('invite-code')).resolves.toBeUndefined()

    expect(s.currentServerId).toBe('43')
    expect(s.findServer('43')!.unavailable).toBe(true)
    expect(notify.isError).toBe(true)
    expect(notify.text).toContain('频道加载失败')
  })

  it('resetAll 后列表为空，不出现示例服务器或示例频道', async () => {
    mockGoApp.ListServers.mockResolvedValue([serverDTO(7, '服七')])
    mockGoApp.ListChannels.mockResolvedValue({ categories: [], channels: [] })
    const s = useServersStore()
    await s.loadRemoteServers()
    expect(s.servers).toHaveLength(1)

    s.resetAll()

    expect(s.servers).toEqual([])
    expect(s.findServer('hq')).toBeUndefined()
  })
})
