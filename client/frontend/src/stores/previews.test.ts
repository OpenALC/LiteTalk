// 链接预览 store（#42 B4）：去重、负缓存、错误静默与 resetAll 代次保护。
import { beforeEach, describe, expect, it, vi } from 'vitest'
import { createPinia, setActivePinia } from 'pinia'

const mockGoApp = vi.hoisted(() => ({ GetLinkPreview: vi.fn() }))
vi.mock('../api/bridge', () => ({ goApp: () => mockGoApp }))

import type { LinkPreview } from '../api/types'
import { usePreviewStore } from './previews'

const preview = (url: string): LinkPreview => ({
  url,
  title: '标题',
  description: '描述',
  site_name: '示例站',
  image_url: 'https://example.com/a.png',
})

function deferred<T>() {
  let resolve!: (v: T) => void
  let reject!: (e: unknown) => void
  const promise = new Promise<T>((res, rej) => {
    resolve = res
    reject = rej
  })
  return { promise, resolve, reject }
}

describe('preview store', () => {
  beforeEach(() => {
    setActivePinia(createPinia())
    mockGoApp.GetLinkPreview.mockReset()
  })

  it('成功结果进入缓存，重复 ensure 不再请求', async () => {
    mockGoApp.GetLinkPreview.mockResolvedValue(preview('https://example.com/'))
    const s = usePreviewStore()

    await s.ensure(' https://example.com/ ')
    expect(mockGoApp.GetLinkPreview).toHaveBeenCalledWith('https://example.com/')
    expect(s.get('https://example.com/')).toMatchObject({ title: '标题' })
    expect(s.isLoading('https://example.com/')).toBe(false)

    await s.ensure('https://example.com/')
    expect(mockGoApp.GetLinkPreview).toHaveBeenCalledTimes(1)
  })

  it('在飞请求去重：并发 ensure 只发一次', async () => {
    const d = deferred<LinkPreview | null>()
    mockGoApp.GetLinkPreview.mockReturnValue(d.promise)
    const s = usePreviewStore()

    const first = s.ensure('https://example.com/x')
    const second = s.ensure('https://example.com/x')
    expect(mockGoApp.GetLinkPreview).toHaveBeenCalledTimes(1)
    expect(s.isLoading('https://example.com/x')).toBe(true)

    d.resolve(preview('https://example.com/x'))
    await Promise.all([first, second])
    expect(mockGoApp.GetLinkPreview).toHaveBeenCalledTimes(1)
    expect(s.get('https://example.com/x')).toMatchObject({ title: '标题' })
  })

  it('null 与错误都落负缓存且静默', async () => {
    const s = usePreviewStore()
    mockGoApp.GetLinkPreview.mockResolvedValueOnce(null)
    await s.ensure('https://none.example/')
    expect(s.get('https://none.example/')).toBe('none')

    mockGoApp.GetLinkPreview.mockRejectedValueOnce(new Error('bad_url: 链接地址无效'))
    await s.ensure('https://bad.example/')
    expect(s.get('https://bad.example/')).toBe('none')

    await s.ensure('https://bad.example/')
    expect(mockGoApp.GetLinkPreview).toHaveBeenCalledTimes(2)
  })

  it('空 URL 不发请求', async () => {
    const s = usePreviewStore()
    await s.ensure('   ')
    expect(mockGoApp.GetLinkPreview).not.toHaveBeenCalled()
  })

  it('resetAll 递增代次：晚到的旧响应丢弃，新会话重新请求', async () => {
    const d = deferred<LinkPreview | null>()
    mockGoApp.GetLinkPreview.mockReturnValueOnce(d.promise)
    const s = usePreviewStore()

    const pending = s.ensure('https://old.example/')
    s.resetAll()
    expect(s.get('https://old.example/')).toBeUndefined()
    expect(s.isLoading('https://old.example/')).toBe(false)

    d.resolve(preview('https://old.example/'))
    await pending
    expect(s.get('https://old.example/')).toBeUndefined()

    mockGoApp.GetLinkPreview.mockResolvedValueOnce(preview('https://old.example/'))
    await s.ensure('https://old.example/')
    expect(mockGoApp.GetLinkPreview).toHaveBeenCalledTimes(2)
    expect(s.get('https://old.example/')).toMatchObject({ title: '标题' })
  })
})
