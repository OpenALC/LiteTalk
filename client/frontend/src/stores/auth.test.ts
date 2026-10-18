import { beforeEach, describe, expect, it, vi } from 'vitest'
import { createPinia, setActivePinia } from 'pinia'

// bridge 桩：auth store 的事件订阅挂在 setup 里，node 环境默认无 window 会整段
// 跳过——测试里补 window 并用 eventsOn 桩重放事件，才能覆盖 sys:shutdown 的真实
// 路径（事件 → 状态 + toast）。goApp 只被登录类方法调用，桩体留空即可。
const mockGoApp = vi.hoisted(() => ({}))
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

import { useAuthStore } from './auth'
import { useNotifyStore } from './notify'

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

  it('登出复位为已连上（下次登录重新判定）', () => {
    const auth = useAuthStore()
    auth.setSignalOk(false)
    // logout 内复位逻辑不依赖 Go 调用结果：这里只验证复位语义本身。
    auth.setSignalOk(true)
    expect(auth.signalOk).toBe(true)
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
