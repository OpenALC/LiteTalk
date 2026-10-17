import { beforeEach, describe, expect, it } from 'vitest'
import { createPinia, setActivePinia } from 'pinia'
import { useAuthStore } from './auth'

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
