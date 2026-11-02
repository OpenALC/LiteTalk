import { describe, expect, it, vi } from 'vitest'
import { LevelMonitorScope } from './levelMonitorScope'

describe('LevelMonitorScope', () => {
  it('卸载后才拿到启动 token 时立即释放，并拒绝继续自检', async () => {
    let resolveStart!: (token: string) => void
    const startNative = vi.fn(() => new Promise<string>((resolve) => { resolveStart = resolve }))
    const stopNative = vi.fn(async (_token: string) => {})
    const scope = new LevelMonitorScope(startNative, stopNative)
    const started = scope.start()

    await scope.dispose()
    resolveStart('old')

    expect(await started).toBe(false)
    expect(stopNative).toHaveBeenCalledExactlyOnceWith('old')
    expect(scope.closed).toBe(true)
    expect(await scope.start()).toBe(false)
    expect(startNative).toHaveBeenCalledTimes(1)
  })

  it('卸载与正常结束同时停止时只释放一次', async () => {
    const stopNative = vi.fn(async (_token: string) => {})
    const scope = new LevelMonitorScope(async () => 'current', stopNative)
    expect(await scope.start()).toBe(true)

    await Promise.all([scope.dispose(), scope.stop()])
    expect(stopNative).toHaveBeenCalledExactlyOnceWith('current')
  })

  it('B 先启动后，A 的迟到启动只归还 A 的 token', async () => {
    let resolveOld!: (token: string) => void
    const stopNative = vi.fn(async (_token: string) => {})
    const oldScope = new LevelMonitorScope(
      () => new Promise<string>((resolve) => { resolveOld = resolve }), stopNative,
    )
    const newScope = new LevelMonitorScope(async () => 'B', stopNative)
    const pendingOld = oldScope.start()
    await oldScope.dispose()
    expect(await newScope.start()).toBe(true)

    resolveOld('A')
    expect(await pendingOld).toBe(false)
    expect(stopNative).toHaveBeenCalledExactlyOnceWith('A')
    await newScope.stop()
    expect(stopNative).toHaveBeenNthCalledWith(2, 'B')
  })

  it('无原生监视的浏览器桩仍允许原有麦克风自检', async () => {
    const stopNative = vi.fn(async (_token: string) => {})
    const scope = new LevelMonitorScope(() => undefined, stopNative)
    expect(await scope.start()).toBe(true)
    await scope.dispose()
    expect(stopNative).not.toHaveBeenCalled()
  })
})
