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

// 浏览器 mock 的语音设置默认值：Beta APM 默认关闭；APMSupported 为 true
// 让 Beta 区在浏览器预览里可见可操作（原生 stub 平台由真实绑定返回 false）。
describe('Beta APM mock 默认值', () => {
  afterEach(() => {
    delete (globalThis as Record<string, unknown>).window
    env.DEV = true
  })

  it('APM 默认关闭', async () => {
    installWindow()
    env.DEV = true
    const s = await goApp().GetAudioSettings()
    expect(s.enable_apm).toBe(false)
  })

  it('DRED 默认开启', async () => {
    installWindow()
    env.DEV = true
    const s = await goApp().GetAudioSettings()
    expect(s.dred).toBe(true)
  })

  it('mock 平台报告支持 APM', async () => {
    installWindow()
    env.DEV = true
    await expect(goApp().APMSupported()).resolves.toBe(true)
  })

  // #117：DRED 开关与 APM 同口径，设置页据此禁用开关并提示不支持。
  it('mock 平台报告支持 DRED', async () => {
    installWindow()
    env.DEV = true
    await expect(goApp().DREDSupported()).resolves.toBe(true)
  })
})

// 游戏内悬浮层（#47，Beta，仅 Windows）：浏览器 mock 默认关闭、报告支持，
// 让 Beta 区在预览里可见可操作；真实平台能力由 Go 侧 Supported 报告。
describe('游戏内悬浮层 mock 默认值', () => {
  afterEach(() => {
    delete (globalThis as Record<string, unknown>).window
    env.DEV = true
  })

  it('默认关闭', async () => {
    installWindow()
    env.DEV = true
    const s = await goApp().GetWindowOverlaySettings()
    expect(s.enabled).toBe(false)
  })

  it('mock 平台报告支持', async () => {
    installWindow()
    env.DEV = true
    await expect(goApp().WindowOverlaySupported()).resolves.toBe(true)
  })

  it('保存为 no-op', async () => {
    installWindow()
    env.DEV = true
    await expect(goApp().SetWindowOverlaySettings({ enabled: true })).resolves.toBeUndefined()
  })
})
