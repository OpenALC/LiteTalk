// 好友频道 presence（#92）：FriendEntry 快照映射、setPresence 频道切换
//（回归「已在线早退丢换频道」缺陷）、resolveFriendJoin 按 ID 定位。
import { beforeEach, describe, expect, it, vi } from 'vitest'
import { createPinia, setActivePinia } from 'pinia'

const mockGoApp = vi.hoisted(() => ({
  ListFriends: vi.fn(),
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

import { useFriendsStore, resolveFriendJoin, type Channel, type Server } from './data'

// dto 已接受好友的 FriendDTO 线上形态（channel_id/server_id 可选）。
const dto = (
  id: string,
  uid: string,
  online: boolean,
  channelId?: string,
  serverId?: string,
) => ({
  id,
  status: 'accepted',
  incoming: false,
  remark: '',
  group: '',
  user: { id: uid, nickname: `用户${uid}`, tag: '0001', status: 1, online },
  ...(channelId != null ? { channel_id: channelId } : {}),
  ...(serverId != null ? { server_id: serverId } : {}),
})

describe('好友频道快照映射', () => {
  beforeEach(() => {
    setActivePinia(createPinia())
    mockGoApp.ListFriends.mockReset()
    mockEventsOn.reset()
  })

  it('loadFriends 映射频道字段；离线即使带字段也不显示；半套/非法 ID 当无频道', async () => {
    mockGoApp.ListFriends.mockResolvedValue([
      dto('1', '10', true, '700', '800'),
      dto('2', '20', false, '700', '800'), // 离线：带字段也不显示
      dto('3', '30', true, '700'), // 缺 server_id
      dto('4', '40', true, 'abc', '800'), // 非法 ID
    ])
    const s = useFriendsStore()
    await s.loadFriends()

    const [a] = s.online.filter((f) => f.userId === '10')
    expect(a.channelId).toBe('700')
    expect(a.serverId).toBe('800')
    expect(a.inChannel).toBe(true)

    const [off] = s.offline.filter((f) => f.userId === '20')
    expect(off.channelId).toBeUndefined()
    expect(off.serverId).toBeUndefined()
    expect(off.inChannel).toBe(false)

    const [half] = s.online.filter((f) => f.userId === '30')
    expect(half.channelId).toBeUndefined()
    expect(half.inChannel).toBe(false)

    const [bad] = s.online.filter((f) => f.userId === '40')
    expect(bad.channelId).toBeUndefined()
    expect(bad.inChannel).toBe(false)
  })

  it('在线好友切换频道必须更新（回归早退缺陷）；离开/离线/坏包清空', async () => {
    mockGoApp.ListFriends.mockResolvedValue([dto('1', '10', true, '700', '800')])
    const s = useFriendsStore()
    await s.loadFriends()
    s.bindEvents()

    // 已在线 + 换频道：不得早退，频道字段要更新。
    mockEventsOn.fire('presence:update', {
      user_id: '10',
      online: true,
      channel_id: '701',
      server_id: '800',
    })
    let [f] = s.online
    expect(f.channelId).toBe('701')
    expect(f.serverId).toBe('800')
    expect(f.inChannel).toBe(true)

    // 离开频道（在线但无频道字段）→ 清空。
    mockEventsOn.fire('presence:update', { user_id: '10', online: true })
    ;[f] = s.online
    expect(f.channelId).toBeUndefined()
    expect(f.inChannel).toBe(false)

    // 坏包（只带 channel_id / 非法 ID）→ 当无频道，不抛错。
    mockEventsOn.fire('presence:update', {
      user_id: '10',
      online: true,
      channel_id: '702',
    })
    ;[f] = s.online
    expect(f.inChannel).toBe(false)
    mockEventsOn.fire('presence:update', {
      user_id: '10',
      online: true,
      channel_id: 'xx',
      server_id: '800',
    })
    ;[f] = s.online
    expect(f.inChannel).toBe(false)

    // 带频道字段的离线推送 → 清空并移组。
    mockEventsOn.fire('presence:update', {
      user_id: '10',
      online: false,
      channel_id: '702',
      server_id: '800',
    })
    expect(s.online).toEqual([])
    ;[f] = s.offline
    expect(f.channelId).toBeUndefined()
    expect(f.inChannel).toBe(false)
    expect(f.sub).toBe('离线')
  })
})

describe('resolveFriendJoin', () => {
  const ch = (id: string, name: string, extra?: Partial<Channel>): Channel => ({
    id,
    name,
    type: 'voice',
    group: '',
    ...extra,
  })
  const servers: Pick<Server, 'id' | 'channels'>[] = [
    {
      id: '1',
      channels: [
        ch('11', '开黑', { hasPassword: true }),
        ch('12', '开黑', { parentId: '11' }), // 同名子频道（频道树）
        ch('13', '文字', { type: 'text' }),
      ],
    },
    { id: '2', channels: [ch('21', '开黑'), ch('22', '大厅')] },
  ]

  it('同名频道跨服按 serverId 选对；带密码标志', () => {
    const r = resolveFriendJoin({ status: 'online', channelId: '21', serverId: '2' }, servers)
    expect(r.ok).toBe(true)
    if (!r.ok) return
    expect(r.channelId).toBe('21')
    expect(r.serverId).toBe('2')
    expect(r.hasPassword).toBe(false)
  })

  it('子频道按 id 命中（频道树不平铺）', () => {
    const r = resolveFriendJoin({ status: 'online', channelId: '12', serverId: '1' }, servers)
    expect(r.ok).toBe(true)
    if (!r.ok) return
    expect(r.channelName).toBe('开黑')
  })

  it('找不到服务器/频道禁用，且不返回频道名', () => {
    for (const f of [
      { status: 'online', channelId: '99', serverId: '1' } as const,
      { status: 'online', channelId: '11', serverId: '9' } as const,
    ]) {
      const r = resolveFriendJoin(f, servers)
      expect(r.ok).toBe(false)
      if (r.ok) return
      expect(r.reason).toBe('无法访问 TA 的频道')
      expect(r.channelName).toBeUndefined()
    }
  })

  it('已在同频道禁用；离线禁用；无 ID 禁用且不返回频道名', () => {
    const same = resolveFriendJoin(
      { status: 'online', channelId: '21', serverId: '2' },
      servers,
      '21',
    )
    expect(same.ok).toBe(false)
    if (!same.ok) expect(same.reason).toBe('你们已在同一频道')

    const off = resolveFriendJoin(
      { status: 'offline', channelId: '21', serverId: '2' },
      servers,
    )
    expect(off.ok).toBe(false)
    if (!off.ok) {
      expect(off.reason).toBe('TA 已离线')
      expect(off.channelName).toBeUndefined()
    }

    const none = resolveFriendJoin({ status: 'online' }, servers)
    expect(none.ok).toBe(false)
    if (!none.ok) {
      expect(none.reason).toBe('无法加入 TA 的频道')
      expect(none.channelName).toBeUndefined()
    }
  })
})
