// whenNativeReady 分环境超时语义：
// DEV（浏览器开发）超时按 mock 处理、不置失败标志；生产超时置失败标志
// （nativeInitFailed），绝不静默落 mock——生产 mock 会让「登录一律成功」。
// node 环境无 window/DOM：用最小 FakeWindow（EventTarget 子类）顶替。
import { afterEach, describe, expect, it, vi } from 'vitest'
import { __resetBridgeForTest, goApp, isNative, isNativeInitFailed, whenNativeReady } from './bridge'

class FakeWindow extends EventTarget {
  _wails?: { flags?: Record<string, unknown> }
}

function installWindow(): FakeWindow {
  const win = new FakeWindow()
  ;(globalThis as Record<string, unknown>).window = win
  return win
}

const env = import.meta.env as Record<string, unknown>

describe('whenNativeReady', () => {
  afterEach(() => {
    vi.useRealTimers()
    delete (globalThis as Record<string, unknown>).window
    env.DEV = true
    __resetBridgeForTest()
  })

  it('flags 已就位时立即就绪', async () => {
    const win = installWindow()
    win._wails = { flags: {} }
    await expect(whenNativeReady(50)).resolves.toBeUndefined()
    expect(isNative()).toBe(true)
  })

  it('DEV 环境超时按 mock 处理，不置失败标志', async () => {
    installWindow()
    env.DEV = true
    vi.useFakeTimers()
    const p = whenNativeReady(50)
    await vi.advanceTimersByTimeAsync(60)
    await p
    expect(isNativeInitFailed()).toBe(false)
  })

  it('生产环境超时置失败标志', async () => {
    installWindow()
    env.DEV = false
    vi.useFakeTimers()
    const p = whenNativeReady(50)
    await vi.advanceTimersByTimeAsync(60)
    await p
    expect(isNativeInitFailed()).toBe(true)
  })

  it('超时前事件到达则就绪，且不置失败标志', async () => {
    const win = installWindow()
    env.DEV = false
    vi.useFakeTimers()
    const p = whenNativeReady(1000)
    await vi.advanceTimersByTimeAsync(100)
    win._wails = { flags: {} }
    window.dispatchEvent(new Event('wails:runtime-config-ready'))
    await p
    expect(isNative()).toBe(true)
    expect(isNativeInitFailed()).toBe(false)
  })
})

// 浏览器 mock 的语音设置默认值：AEC 关闭字段缺省 = 开启，UI 依赖它初始化
// 「回声消除 AEC」开关（Go 侧 omitempty 不会下发 false）。
describe('语音设置 mock 默认值', () => {
  afterEach(() => {
    delete (globalThis as Record<string, unknown>).window
    env.DEV = true
  })

  it('默认开启回声消除', async () => {
    installWindow()
    env.DEV = true
    const s = await goApp().GetAudioSettings()
    expect(s.disable_aec).toBe(false)
  })
})
