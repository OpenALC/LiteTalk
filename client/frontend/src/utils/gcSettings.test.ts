import { describe, expect, it, vi } from 'vitest'
import type { RuntimeGCSettings } from '../api/bridge'
import { useGCSettings } from './gcSettings'

const desktop = (level = 3, extra: Partial<RuntimeGCSettings> = {}): RuntimeGCSettings => ({
  supported: true, level, gogc_overridden: false, memory_limit_overridden: false,
  effective_gogc: 'off', effective_memory_limit: '18MiB', ...extra,
})

function deferred<T>() {
  let resolve!: (value: T) => void
  let reject!: (error: Error) => void
  const promise = new Promise<T>((yes, no) => { resolve = yes; reject = no })
  return { promise, resolve, reject }
}

describe('useGCSettings', () => {
  it('加载默认档并以 Go 回包确认热应用结果', async () => {
    const set = vi.fn(async (level: number) => desktop(level))
    const gc = useGCSettings({ GetGCSettings: async () => desktop(), SetGCLevel: set })
    await gc.load()
    expect(gc.level.value).toBe(3)
    expect(gc.label.value).toBe('默认')
    await gc.setLevel(5)
    expect(set).toHaveBeenCalledWith(5)
    expect(gc.level.value).toBe(5)
    expect(gc.label.value).toBe('最高')
  })

  it('Web 不支持与双环境覆盖时禁止保存，单覆盖仍可调整', async () => {
    const set = vi.fn(async (level: number) => desktop(level))
    const web = useGCSettings({ GetGCSettings: async () => desktop(3, { supported: false }), SetGCLevel: set })
    await web.load()
    await web.setLevel(5)
    expect(web.disabled.value).toBe(true)

    const both = useGCSettings({ GetGCSettings: async () => desktop(3, {
      gogc_overridden: true, memory_limit_overridden: true,
    }), SetGCLevel: set })
    await both.load()
    await both.setLevel(5)
    expect(both.disabled.value).toBe(true)
    expect(set).not.toHaveBeenCalled()

    const one = useGCSettings({ GetGCSettings: async () => desktop(3, { gogc_overridden: true }), SetGCLevel: set })
    await one.load()
    expect(one.disabled.value).toBe(false)
    await one.setLevel(4)
    expect(set).toHaveBeenCalledWith(4)
  })

  it('读取失败保持禁用并显示原因；保存失败回滚到已确认档位', async () => {
    const read = useGCSettings({ GetGCSettings: async () => { throw new Error('read denied') }, SetGCLevel: vi.fn() })
    await read.load()
    expect(read.disabled.value).toBe(true)
    expect(read.error.value).toContain('read denied')

    const gc = useGCSettings({
      GetGCSettings: async () => desktop(3),
      SetGCLevel: async () => { throw new Error('disk full') },
    })
    await gc.load()
    await gc.setLevel(5)
    expect(gc.level.value).toBe(3)
    expect(gc.error.value).toContain('disk full')
  })

  it('连续拖动串行保存，只发送未发出的最新档；先完成的回包不覆盖最新选择', async () => {
    const first = deferred<RuntimeGCSettings>()
    const last = deferred<RuntimeGCSettings>()
    const set = vi.fn()
      .mockReturnValueOnce(first.promise)
      .mockReturnValueOnce(last.promise)
    const gc = useGCSettings({ GetGCSettings: async () => desktop(3), SetGCLevel: set })
    await gc.load()
    const saving = gc.setLevel(2)
    void gc.setLevel(4)
    void gc.setLevel(5)
    expect(set).toHaveBeenCalledTimes(1)
    expect(gc.level.value).toBe(5)
    first.resolve(desktop(2))
    await Promise.resolve()
    expect(gc.level.value).toBe(5)
    expect(set).toHaveBeenCalledTimes(2)
    expect(set).toHaveBeenLastCalledWith(5)
    last.resolve(desktop(5))
    await saving
    expect(gc.level.value).toBe(5)
    expect(gc.saving.value).toBe(false)
  })

  it('前一个请求失败但已有新档位时继续保存，新请求失败则回滚最后确认值', async () => {
    const first = deferred<RuntimeGCSettings>()
    const set = vi.fn()
      .mockReturnValueOnce(first.promise)
      .mockRejectedValueOnce(new Error('second failed'))
    const gc = useGCSettings({ GetGCSettings: async () => desktop(3), SetGCLevel: set })
    await gc.load()
    const saving = gc.setLevel(2)
    void gc.setLevel(4)
    first.reject(new Error('first failed'))
    await saving
    expect(set).toHaveBeenNthCalledWith(2, 4)
    expect(gc.level.value).toBe(3)
    expect(gc.error.value).toContain('second failed')
  })
})
