import { describe, expect, it } from 'vitest'
import { canSendMessage, formatFileSize, isImageAttachment } from './attachment'

describe('附件展示纯函数（#41）', () => {
  it('formatFileSize 按 B/KB/MB 分档', () => {
    expect(formatFileSize(0)).toBe('0 B')
    expect(formatFileSize(-1)).toBe('0 B')
    expect(formatFileSize(Number.NaN)).toBe('0 B')
    expect(formatFileSize(512)).toBe('512 B')
    expect(formatFileSize(2048)).toBe('2.0 KB')
    expect(formatFileSize(1536)).toBe('1.5 KB')
    expect(formatFileSize(10 * 1024 * 1024)).toBe('10.0 MB')
  })

  it('isImageAttachment 按 content_type 或缩略图判定', () => {
    expect(isImageAttachment({ content_type: 'image/png' })).toBe(true)
    expect(isImageAttachment({ content_type: 'IMAGE/JPEG' })).toBe(true)
    expect(isImageAttachment({ content_type: 'text/plain' })).toBe(false)
    expect(isImageAttachment({ content_type: 'text/plain', thumbnail_url: '/t.jpg' })).toBe(true)
  })

  it('canSendMessage 正文为空但带附件仍可发送', () => {
    expect(canSendMessage('', 0)).toBe(false)
    expect(canSendMessage('   ', 0)).toBe(false)
    expect(canSendMessage('hi', 0)).toBe(true)
    expect(canSendMessage('', 1)).toBe(true)
  })
})
