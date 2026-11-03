import { describe, expect, it, vi } from 'vitest'
import { useHibernateDestroyAvailability } from './hibernateAvailability'

describe('useHibernateDestroyAvailability', () => {
  it('查询未完成时隐藏 destroy，成功且支持后才显示', async () => {
    let resolve!: (value: boolean) => void
    const query = vi.fn(() => new Promise<boolean>((done) => { resolve = done }))
    const availability = useHibernateDestroyAvailability(query)
    const pending = availability.load()

    expect(availability.state.value).toBe('loading')
    expect(availability.destroyAvailable.value).toBe(false)

    resolve(true)
    await pending
    expect(availability.state.value).toBe('available')
    expect(availability.destroyAvailable.value).toBe(true)
    expect(query).toHaveBeenCalledTimes(1)
  })

  it('明确不支持时隐藏 destroy', async () => {
    const availability = useHibernateDestroyAvailability(vi.fn(async () => false))
    await availability.load()
    expect(availability.state.value).toBe('unavailable')
    expect(availability.destroyAvailable.value).toBe(false)
  })

  it('能力调用失败时保持隐藏，且不误报平台不支持', async () => {
    const availability = useHibernateDestroyAvailability(vi.fn(async () => {
      throw new Error('method unavailable')
    }))
    await availability.load()
    expect(availability.state.value).toBe('error')
    expect(availability.destroyAvailable.value).toBe(false)
  })
})
