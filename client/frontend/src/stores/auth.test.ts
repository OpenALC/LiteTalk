import { beforeEach, describe, expect, it, vi } from 'vitest'
import { createPinia, setActivePinia } from 'pinia'

// bridge 桩：auth store 的事件订阅挂在 setup 里，node 环境默认无 window 会整段
// 跳过——测试里补 window 并用 eventsOn 桩重放事件，才能覆盖 sys:shutdown 与
// auth:kicked 的真实路径（事件 → 状态 + 业务 store 复位）。goApp 只被登录/登出
// 方法调用，桩体按用例配置。
const mockGoApp = vi.hoisted(() => ({
  Logout: vi.fn(async () => {}),
  Login: vi.fn(),
}))
const mockEventsOn = vi.hoisted(() => {
  const handlers = new Map<string, Array<(...args: unknown[]) => void>>()
  return {
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
// handleKicked 会动态 import router 回登录页；node 环境没有 location/history，
// 用桩替代，顺带断言导航目标。
const mockRouter = vi.hoisted(() => ({
  push: vi.fn(async () => {}),
  currentRoute: { value: { name: 'main' } },
}))
vi.mock('../router', () => ({ default: mockRouter }))

import type { AuthResult, UserProfile } from '../api/types'
import { kickedBannerText, useAuthStore } from './auth'
import { useChatStore } from './chat'
import { useConversationsStore } from './conversations'
import { useFriendsStore, useServersStore } from './data'
import { useNotifyStore } from './notify'
import { useVoiceStore } from './voice'

// 信令状态提示条的数据源：MainView 据 signalOk 显示「信令连接中」，
// Go 侧经 auth:session 事件驱动（login 响应的 session_ok 只作初值）。
describe('auth store 信令状态', () => {
  beforeEach(() => {
    setActivePinia(createPinia())
  })

  it('默认已连上（不显示提示条）', () => {
    const auth = useAuthStore()
    expect(auth.signalOk).toBe(true)
  })

  it('setSignalOk(false) 置断线态', () => {
    const auth = useAuthStore()
    auth.setSignalOk(false)
    expect(auth.signalOk).toBe(false)
  })

  it('重连成功后恢复', () => {
    const auth = useAuthStore()
    auth.setSignalOk(false)
    auth.setSignalOk(true)
    expect(auth.signalOk).toBe(true)
  })

  it('登出复位为已连上（下次登录重新判定）', async () => {
    const auth = useAuthStore()
    auth.setSignalOk(false)
    // #14：这条必须真走 logout()——此前的写法手工把 signalOk 置回 true，
    // 与 logout 的复位逻辑无关，属于同义反复。
    await auth.logout()
    expect(auth.signalOk).toBe(true)
    expect(auth.loggedIn).toBe(false)
  })
})

// 被踢横幅的数据源：kickedReason 由 store 层 auth:kicked 监听写入，
// LoginView 进入时取走并清（横幅只活在本次登录页访问）。
describe('auth store 被踢原因', () => {
  beforeEach(() => {
    setActivePinia(createPinia())
  })

  it('默认无被踢原因', () => {
    expect(useAuthStore().kickedReason).toBe('')
  })

  it('setKickedReason 写入原因', () => {
    const auth = useAuthStore()
    auth.setKickedReason('login_elsewhere')
    expect(auth.kickedReason).toBe('login_elsewhere')
  })

  it('空原因回落 login_elsewhere（契约 §0.5 约定值）', () => {
    const auth = useAuthStore()
    auth.setKickedReason('')
    expect(auth.kickedReason).toBe('login_elsewhere')
  })

  it('clearKickedReason 清空（进入登录页取走后不留到下次）', () => {
    const auth = useAuthStore()
    auth.setKickedReason('login_elsewhere')
    auth.clearKickedReason()
    expect(auth.kickedReason).toBe('')
  })

  it('setSignalOk 不影响被踢原因（两态独立）', () => {
    const auth = useAuthStore()
    auth.setKickedReason('login_elsewhere')
    auth.setSignalOk(true)
    expect(auth.kickedReason).toBe('login_elsewhere')
    expect(auth.signalOk).toBe(true)
  })
})

// 登录页被踢横幅文案（#84 复审 4.1）：三种原因必须给出不同提示，
// login_expired（refresh_token 被吊销）不能显示成「账号已在其他设备登录」。
describe('kickedBannerText 被踢横幅文案', () => {
  it('server_changed 提示令牌不被新服务器接受', () => {
    expect(kickedBannerText('server_changed')).toBe('当前令牌不被新服务器接受，请重新登录')
  })

  it('login_expired 提示登录已过期', () => {
    expect(kickedBannerText('login_expired')).toBe('登录已过期，请重新登录')
  })

  it('login_elsewhere 与未知原因回落异地登录', () => {
    expect(kickedBannerText('login_elsewhere')).toBe('账号已在其他设备登录')
    expect(kickedBannerText('')).toBe('账号已在其他设备登录')
  })
})

// 服务端关闭（sys:shutdown，#21）：Go 侧已停自动重连，前端必须明示「服务端已
// 关闭」——否则信令断了只会显示「信令连接中」，用户以为还会自己回来。
describe('auth store 服务端关闭', () => {
  beforeEach(() => {
    mockEventsOn.reset()
    // 让 store setup 的 window 判定通过，事件订阅才会注册。
    ;(globalThis as { window?: unknown }).window = globalThis
    setActivePinia(createPinia())
  })

  it('默认未关闭', () => {
    expect(useAuthStore().shutdown).toBe('')
  })

  it('sys:shutdown 写入原因并弹提示', () => {
    const auth = useAuthStore()
    const notify = useNotifyStore()
    mockEventsOn.fire('sys:shutdown', { reason: 'shutdown' })
    expect(auth.shutdown).toBe('shutdown')
    expect(notify.visible).toBe(true)
    expect(notify.text).toBe('服务端已关闭')
  })

  it('缺 reason 回落 shutdown（载荷字段缺失也要有终态文案）', () => {
    const auth = useAuthStore()
    mockEventsOn.fire('sys:shutdown', {})
    expect(auth.shutdown).toBe('shutdown')
  })

  it('与信令态独立：断线 + 已关闭时提示条按关闭文案', () => {
    const auth = useAuthStore()
    auth.setSignalOk(false)
    mockEventsOn.fire('sys:shutdown', { reason: 'shutdown' })
    expect(auth.shutdown).toBe('shutdown')
    expect(auth.signalOk).toBe(false)
  })

  it('clearShutdown 清空（重新登录连上新会话后不留旧终态）', () => {
    const auth = useAuthStore()
    mockEventsOn.fire('sys:shutdown', { reason: 'shutdown' })
    auth.clearShutdown()
    expect(auth.shutdown).toBe('')
  })
})

// 业务 store 跨账号/跨服务器残留（#85，覆盖 #14 的登出复位与 #17 的换服复位）：
// 主动登出、被踢、换服（auth:kicked server_changed）与登录新账号前，servers/
// friends/voice/chat 必须回到登录前状态。
describe('auth store 业务 store 复位（#85）', () => {
  beforeEach(() => {
    mockEventsOn.reset()
    mockGoApp.Logout.mockReset().mockResolvedValue(undefined)
    mockGoApp.Login.mockReset()
    mockRouter.push.mockReset().mockResolvedValue(undefined)
    mockRouter.currentRoute.value.name = 'main'
    ;(globalThis as { window?: unknown }).window = globalThis
    setActivePinia(createPinia())
  })

  const userB: UserProfile = {
    id: '2', nickname: '乙', tag: '0002', email: 'b@example.com',
    email_verified: true, is_guest: false, status: 0, online: true,
    created_at: '2026-01-01T00:00:00Z', updated_at: '2026-01-01T00:00:00Z',
  }
  const loginB: AuthResult = {
    access_token: 'tok-b', refresh_token: 'ref-b', user: userB, session_ok: true,
  }

  // seed 填满 A 账号残留：真实服务器、分类缓存、好友、语音房间、聊天记录。
  function seed() {
    const servers = useServersStore()
    servers.servers.push({
      id: '100', name: 'A 的服务器', icon: 'Music',
      channels: [{ id: '1001', name: '开黑房', type: 'voice', group: '语音频道' }],
    })
    servers.selectServer('100')
    servers.extraGroups = { '100': [{ id: '101', name: '甲组', position: 0 }] }

    const friends = useFriendsStore()
    friends.online = [{
      id: 'f1', userId: '10', name: '好友甲', nickname: '好友甲', remark: '',
      status: 'online', sub: '在线', group: '好友', incoming: false,
    }]
    friends.offline = [{
      id: 'f2', userId: '20', name: '好友乙', nickname: '好友乙', remark: '',
      status: 'offline', sub: '离线', group: '好友', incoming: false,
    }]
    friends.pending = [{ id: 'p1', name: '待处理', outgoing: true }]

    const voice = useVoiceStore()
    voice.channelId = '1001'
    voice.members = [{
      user_id: '10', nickname: '好友甲', tag: '0001', speaking: false, mute: false, deafen: false,
    }]
    voice.connected = true
    voice.muted = true
    voice.deafened = true
    voice.latencyMs = 30
    voice.peerVolumes = { '10': 1.5 }
    voice.setSelf('10')

    useChatStore().messagesByChannel = {
      '1001': [{
        id: 'm1', channel_id: '1001', user_id: '10', nickname: '好友甲',
        content: 'hi', ts: '2026-01-01T00:00:00Z',
      }],
    }

    const conversations = useConversationsStore()
    conversations.list = [{
      id: '500', kind: 'direct', name: '', created_at: '2026-01-01T00:00:00Z',
      members: [{
        user_id: '10', nickname: '好友甲', tag: '0001',
        role: 'member', joined_at: '2026-01-01T00:00:00Z',
      }],
    }]
    conversations.currentId = '500'
    conversations.dmMode = true
  }

  function expectCleared() {
    const servers = useServersStore()
    // 示例服务器已移除：登出后列表为空，登录后由 loadRemoteServers 重新填充。
    expect(servers.servers).toEqual([])
    expect(servers.findServer('100')).toBeUndefined()
    expect(servers.currentServerId).toBe('')
    expect(servers.currentChannelId).toBe('')
    expect(servers.extraGroups).toEqual({})

    const friends = useFriendsStore()
    expect(friends.online).toEqual([])
    expect(friends.offline).toEqual([])
    expect(friends.pending).toEqual([])

    const voice = useVoiceStore()
    expect(voice.channelId).toBe('')
    expect(voice.members).toEqual([])
    expect(voice.connected).toBe(false)
    expect(voice.muted).toBe(false)
    expect(voice.deafened).toBe(false)
    expect(voice.latencyMs).toBe(0)
    expect(voice.peerVolumes).toEqual({})
    expect(voice.selfId()).toBe('')

    expect(useChatStore().messagesByChannel).toEqual({})

    const conversations = useConversationsStore()
    expect(conversations.list).toEqual([])
    expect(conversations.currentId).toBe('')
    expect(conversations.dmMode).toBe(false)
  }

  it('主动登出清空 servers/friends/voice/chat', async () => {
    const auth = useAuthStore()
    auth.loggedIn = true
    seed()

    await auth.logout()

    expect(auth.loggedIn).toBe(false)
    expect(auth.user).toBeNull()
    expectCleared()
  })

  it('Go 侧吊销失败（离线登出）也必须复位本地（#14）', async () => {
    const auth = useAuthStore()
    auth.loggedIn = true
    seed()
    mockGoApp.Logout.mockRejectedValueOnce(new Error('dial tcp: network unreachable'))

    await auth.logout()

    expect(auth.loggedIn).toBe(false)
    expectCleared()
  })

  it('auth:kicked login_elsewhere 被踢后清空', async () => {
    const auth = useAuthStore()
    auth.loggedIn = true
    seed()

    mockEventsOn.fire('auth:kicked', { reason: 'login_elsewhere' })

    await vi.waitFor(() => expect(auth.kickedReason).toBe('login_elsewhere'))
    expect(auth.loggedIn).toBe(false)
    expectCleared()
  })

  it('auth:kicked server_changed 换服同样清空（#17）', async () => {
    const auth = useAuthStore()
    auth.loggedIn = true
    seed()

    mockEventsOn.fire('auth:kicked', { reason: 'server_changed' })

    await vi.waitFor(() => expect(auth.kickedReason).toBe('server_changed'))
    expect(auth.loggedIn).toBe(false)
    expectCleared()
    expect(mockRouter.push).toHaveBeenCalledWith({ name: 'login' })
  })

  it('A 登出后 B 登录不残留 A 的服务器与好友', async () => {
    const auth = useAuthStore()
    auth.loggedIn = true
    seed()
    await auth.logout()

    mockGoApp.Login.mockResolvedValue(loginB)
    await auth.login('b@example.com', 'password')

    expect(auth.loggedIn).toBe(true)
    expect(auth.user?.id).toBe('2')
    expectCleared()
  })

  it('未走 logout 直接登录新账号（会话过期回落登录页）同样清空', async () => {
    seed()
    const auth = useAuthStore()
    mockGoApp.Login.mockResolvedValue(loginB)

    await auth.login('b@example.com', 'password')

    expect(auth.user?.id).toBe('2')
    expectCleared()
  })

  it('重复登出幂等：不报错、状态仍干净', async () => {
    seed()
    const auth = useAuthStore()

    await auth.logout()
    await auth.logout()

    expectCleared()
  })
})
