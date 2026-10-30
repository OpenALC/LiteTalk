import { describe, expect, it, vi } from 'vitest'
import { useWindowOverlay, type WindowOverlayBridge } from './windowOverlay'

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
})
