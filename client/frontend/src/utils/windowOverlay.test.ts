import { describe, expect, it, vi } from 'vitest'
import { useWindowOverlay, type OverlayState, type WindowOverlayBridge } from './windowOverlay'

function fakeBridge(over: Partial<WindowOverlayBridge> = {}): WindowOverlayBridge {
  return {
    WindowOverlaySupported: vi.fn(async () => true),
    GetWindowOverlaySettings: vi.fn(async () => ({ enabled: false })),
    SetWindowOverlaySettings: vi.fn(async () => {}),
    ...over,
  }
}

describe('useWindowOverlay', () => {
  it('默认关闭', async () => {
    const o = useWindowOverlay(fakeBridge())
    await o.load()
    expect(o.supported.value).toBe(true)
    expect(o.enabled.value).toBe(false)
  })

  it('平台不支持时即使配置为开也显示关闭', async () => {
    const bridge = fakeBridge({
      WindowOverlaySupported: vi.fn(async () => false),
      GetWindowOverlaySettings: vi.fn(async () => ({ enabled: true })),
    })
    const o = useWindowOverlay(bridge)
    await o.load()
    expect(o.supported.value).toBe(false)
    expect(o.enabled.value).toBe(false)
    // 开关已禁用，不应发出保存请求。
    const err = await o.setEnabled(true)
    expect(err).toContain('仅 Windows')
    expect(o.enabled.value).toBe(false)
    expect(bridge.SetWindowOverlaySettings).not.toHaveBeenCalled()
  })

  it('保存失败回滚勾选', async () => {
    const bridge = fakeBridge({
      SetWindowOverlaySettings: vi.fn(async () => {
        throw new Error('disk full')
      }),
    })
    const o = useWindowOverlay(bridge)
    await o.load()
    const err = await o.setEnabled(true)
    expect(err).toBe('disk full')
    expect(o.enabled.value).toBe(false)
    // 失败原因同时挂到 note，设置页在没有 toast 时也能看到（#117）。
    expect(o.note.value).toBe('disk full')
  })

  it('保存成功保持勾选', async () => {
    const bridge = fakeBridge()
    const o = useWindowOverlay(bridge)
    await o.load()
    await expect(o.setEnabled(true)).resolves.toBeNull()
    expect(o.enabled.value).toBe(true)
    expect(bridge.SetWindowOverlaySettings).toHaveBeenCalledWith({ enabled: true })
  })

  it('读取绑定缺失时按不支持处理', async () => {
    const bridge = fakeBridge({
      WindowOverlaySupported: vi.fn(async () => {
        throw new Error('method not found')
      }),
    })
    const o = useWindowOverlay(bridge)
    await o.load()
    expect(o.supported.value).toBe(false)
    expect(o.enabled.value).toBe(false)
  })

  // #117：ServiceStartup 里启用失败时 Go 侧回滚配置并推 sys:overlay，
  // 开关必须跟着变回关闭并给出原因。
  it('启动失败回滚事件把开关关掉并记录原因', async () => {
    const holder: { emit: ((s: OverlayState) => void) | null } = { emit: null }
    const bridge = fakeBridge({
      GetWindowOverlaySettings: vi.fn(async () => ({ enabled: true })),
      Subscribe: vi.fn((cb: (s: OverlayState) => void) => {
        holder.emit = cb
        return () => {
          holder.emit = null
        }
      }),
    })
    const o = useWindowOverlay(bridge)
    const cancel = await o.load()
    expect(o.enabled.value).toBe(true)
    expect(bridge.Subscribe).toHaveBeenCalledTimes(1)

    holder.emit?.({ enabled: false, error: 'overlay: CreateWindowExW: 拒绝访问' })
    expect(o.enabled.value).toBe(false)
    expect(o.note.value).toContain('CreateWindowExW')

    // 取消订阅后事件不再影响状态。
    cancel()
    expect(holder.emit).toBeNull()
  })

  it('平台不支持时回滚事件也保持关闭', async () => {
    const holder: { emit: ((s: OverlayState) => void) | null } = { emit: null }
    const bridge = fakeBridge({
      WindowOverlaySupported: vi.fn(async () => false),
      Subscribe: vi.fn((cb: (s: OverlayState) => void) => {
        holder.emit = cb
        return () => {}
      }),
    })
    const o = useWindowOverlay(bridge)
    await o.load()
    holder.emit?.({ enabled: true })
    expect(o.enabled.value).toBe(false)
  })

  it('没有 Subscribe 时 load 返回可安全调用的取消函数', async () => {
    const o = useWindowOverlay(fakeBridge())
    const cancel = await o.load()
    expect(() => cancel()).not.toThrow()
  })
})
