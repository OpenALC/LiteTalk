// data store 频道选项（#37 B2）：channelTree 纯函数、loadChannels 新字段映射、
// createSubchannel / updateChannelOptions 回填。桥接桩只含本文件触达的方法。
import { beforeEach, describe, expect, it, vi } from 'vitest'
import { createPinia, setActivePinia } from 'pinia'

const mockGoApp = vi.hoisted(() => ({
  ListServers: vi.fn(),
  ListChannels: vi.fn(),
  CreateSubchannel: vi.fn(),
  UpdateChannelOptions: vi.fn(),
  ListFriends: vi.fn(async (): Promise<unknown[]> => []),
}))
vi.mock('../api/bridge', () => ({ goApp: () => mockGoApp, eventsOn: () => () => {} }))

import { useServersStore, channelTree, normalizeChannelAudioOptions, type Channel } from './data'

function ch(id: string, name: string, parentId?: string, extra?: Partial<Channel>): Channel {
  return { id, name, type: 'voice', group: '甲组', parentId, ...extra }
}

describe('channelTree', () => {
  it('根 + 子频道：保持输入顺序', () => {
    const tree = channelTree([
      ch('1', '父A'),
      ch('2', '子A1', '1'),
      ch('3', '父B'),
      ch('4', '子A2', '1'),
    ])
    expect(tree.map((n) => n.ch.id)).toEqual(['1', '3'])
    expect(tree[0].children.map((c) => c.id)).toEqual(['2', '4'])
    expect(tree[1].children).toEqual([])
  })

  it('孤儿（父不在本组）降级为根', () => {
    const tree = channelTree([ch('1', '孤儿', '99'), ch('2', '顶层')])
    expect(tree.map((n) => n.ch.id)).toEqual(['1', '2'])
  })

  it('超两级（父自身带父）的孙级升根，不丢节点', () => {
    const tree = channelTree([
      ch('1', '根'),
      ch('2', '子', '1'),
      ch('3', '孙', '2'),
    ])
    expect(tree.map((n) => n.ch.id)).toEqual(['1', '3'])
    expect(tree[0].children.map((c) => c.id)).toEqual(['2'])
  })

  it('自指 parent 视同顶层', () => {
    const tree = channelTree([ch('1', '自指', '1')])
    expect(tree.map((n) => n.ch.id)).toEqual(['1'])
    expect(tree[0].children).toEqual([])
  })

  it('输入顺序不按 Snowflake 数字排序', () => {
    const tree = channelTree([
      ch('900000000000000001', '新'),
      ch('100000000000000002', '旧'),
    ])
    expect(tree.map((n) => n.ch.name)).toEqual(['新', '旧'])
  })
})

describe('频道选项字段映射与回填', () => {
  beforeEach(() => {
    setActivePinia(createPinia())
    mockGoApp.ListServers.mockReset()
    mockGoApp.ListChannels.mockReset()
    mockGoApp.CreateSubchannel.mockReset()
    mockGoApp.UpdateChannelOptions.mockReset()
    mockGoApp.ListFriends.mockReset().mockResolvedValue([])
  })

  it('loadChannels 映射 parentId/hasPassword/userLimit/音质并保持服务端顺序', async () => {
    mockGoApp.ListServers.mockResolvedValue([
      { id: 1, name: '服一', owner_id: 1, icon_name: 'music', icon_color: '#4FA4F3' },
    ])
    mockGoApp.ListChannels.mockResolvedValue({
      categories: [{ id: 3, server_id: 1, name: '甲组', position: 0 }],
      channels: [
        { id: 11, server_id: 1, name: '父', type: 'voice', category_id: 3, position: 0, has_password: true, user_limit: 5, audio_bitrate_kbps: 32, audio_frame_ms: 10 },
        { id: 12, server_id: 1, name: '子', type: 'voice', category_id: 3, position: 1, parent_id: '11', has_password: false, user_limit: 0, audio_bitrate_kbps: 0, audio_frame_ms: 0 },
      ],
    })
    const s = useServersStore()
    await s.loadRemoteServers()
    const cs = s.findServer('1')!.channels
    expect(cs.map((c) => c.id)).toEqual(['11', '12'])
    expect(cs[0].parentId).toBeUndefined()
    expect(cs[0].hasPassword).toBe(true)
    expect(cs[0].userLimit).toBe(5)
    expect(cs[0].audioBitrateKbps).toBe(32)
    expect(cs[0].audioFrameMs).toBe(10)
    expect(cs[1].parentId).toBe('11')
    expect(cs[1].hasPassword).toBe(false)
    expect(cs[1].userLimit).toBe(0)
    expect(cs[1].audioBitrateKbps).toBe(0)
    expect(cs[1].audioFrameMs).toBe(0)
  })

  it('createSubchannel 回填 parentId/hasPassword/userLimit/音质', async () => {
    mockGoApp.ListServers.mockResolvedValue([
      { id: 1, name: '服一', owner_id: 1, icon_name: 'music', icon_color: '#4FA4F3' },
    ])
    mockGoApp.ListChannels.mockResolvedValue({
      categories: [{ id: 3, server_id: 1, name: '甲组', position: 0 }],
      channels: [
        { id: 11, server_id: 1, name: '父', type: 'voice', category_id: 3, position: 0, user_limit: 5 },
      ],
    })
    mockGoApp.CreateSubchannel.mockResolvedValue({
      id: '12', server_id: '1', name: '子', type: 'voice', category_id: 3,
      position: 1, created_at: '2026-01-01T00:00:00Z', parent_id: '11',
      has_password: false, user_limit: 0, audio_bitrate_kbps: 24, audio_frame_ms: 10,
    })
    const s = useServersStore()
    await s.loadRemoteServers()
    await s.createSubchannel('1', '子', '11')
    // bridge 收到父 id 与随父类型。
    expect(mockGoApp.CreateSubchannel).toHaveBeenCalledWith('1', '子', 'voice', '11')
    const child = s.findServer('1')!.channels.find((c) => c.id === '12')!
    expect(child.parentId).toBe('11')
    expect(child.group).toBe('甲组')
    expect(child.hasPassword).toBe(false)
    expect(child.userLimit).toBe(0)
    expect(child.audioBitrateKbps).toBe(24)
    expect(child.audioFrameMs).toBe(10)
  })

  it('updateChannelOptions 回填五字段（清除密码 → hasPassword=false）', async () => {
    mockGoApp.ListServers.mockResolvedValue([
      { id: 1, name: '服一', owner_id: 1, icon_name: 'music', icon_color: '#4FA4F3' },
    ])
    mockGoApp.ListChannels.mockResolvedValue({
      categories: [],
      channels: [
        { id: 11, server_id: 1, name: '父', type: 'voice', category_id: 0, position: 0, has_password: true, user_limit: 5 },
      ],
    })
    mockGoApp.UpdateChannelOptions.mockResolvedValue({
      id: '11', server_id: '1', name: '父', type: 'voice', category_id: null,
      position: 0, created_at: '2026-01-01T00:00:00Z', parent_id: null,
      has_password: false, user_limit: 8, audio_bitrate_kbps: 48, audio_frame_ms: 10,
    })
    const s = useServersStore()
    await s.loadRemoteServers()
    await s.updateChannelOptions('1', '11', '0', 8, 48, 10, '', true)
    expect(mockGoApp.UpdateChannelOptions).toHaveBeenCalledWith('11', '0', 8, 48, 10, '', true)
    const c = s.findServer('1')!.channels[0]
    expect(c.parentId).toBeUndefined()
    expect(c.hasPassword).toBe(false)
    expect(c.userLimit).toBe(8)
    expect(c.audioBitrateKbps).toBe(48)
    expect(c.audioFrameMs).toBe(10)
  })
})

// #46 音质选项映射：白名单外的值回落 0（跟随设置/默认帧长），不把服务端会
// 400 的值发出去。
describe('normalizeChannelAudioOptions', () => {
  it('合法组合原样保留', () => {
    for (const b of [0, 16, 24, 32, 48, 64, 96, 128]) {
      expect(normalizeChannelAudioOptions(b, b === 32 ? 10 : 0)).toEqual({
        bitrateKbps: b,
        frameMS: b === 32 ? 10 : 0,
      })
    }
  })

  it('非法码率/帧长回落 0', () => {
    expect(normalizeChannelAudioOptions(7, 15)).toEqual({ bitrateKbps: 0, frameMS: 0 })
    expect(normalizeChannelAudioOptions(129, 20)).toEqual({ bitrateKbps: 0, frameMS: 0 })
    expect(normalizeChannelAudioOptions(-1, -10)).toEqual({ bitrateKbps: 0, frameMS: 0 })
    expect(normalizeChannelAudioOptions(Number.NaN, Number.NaN)).toEqual({ bitrateKbps: 0, frameMS: 0 })
  })
})
