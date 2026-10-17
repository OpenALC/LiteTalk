// data store 频道恢复（#12）：登录后 ListChannels 填回频道/分类，重启不丢列表；
// 无分类频道归「未分类」；单服拉取失败只跳过（不重试）；分类显示序跟服务端 position。
import { beforeEach, describe, expect, it, vi } from 'vitest'
import { createPinia, setActivePinia } from 'pinia'

const mockGoApp = vi.hoisted(() => ({
  ListServers: vi.fn(),
  ListChannels: vi.fn(),
  GetHomeServer: vi.fn(async () => ''),
  GetIntroHidden: vi.fn(async () => false),
}))
vi.mock('../api/bridge', () => ({ goApp: () => mockGoApp }))

import { useServersStore, UNCATEGORIZED } from './data'

describe('频道列表恢复', () => {
  beforeEach(() => {
    setActivePinia(createPinia())
    mockGoApp.ListServers.mockReset()
    mockGoApp.ListChannels.mockReset()
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

  it('单服 ListChannels 失败只跳过不抛错', async () => {
    mockGoApp.ListServers.mockResolvedValue([
      { id: 9, name: '服九', owner_id: 1, icon_name: 'music', icon_color: '#4FA4F3' },
    ])
    mockGoApp.ListChannels.mockRejectedValue(new Error('403 forbidden'))
    const s = useServersStore()
    await expect(s.loadRemoteServers()).resolves.toBeUndefined()
    expect(s.findServer('9')?.channels).toEqual([])
  })
})
