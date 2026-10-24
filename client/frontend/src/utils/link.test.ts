// 链接提取（#42 B4）：纯文本/粗体、引用、代码块排除、多 URL 取首个、标点裁剪。
import { describe, expect, it } from 'vitest'
import { extractFirstURL } from './link'

describe('extractFirstURL', () => {
  it('纯文本里的链接', () => {
    expect(extractFirstURL('看这个 https://example.com/a?b=1 不错')).toBe('https://example.com/a?b=1')
  })

  it('http 与裸域名（裸域名不算）', () => {
    expect(extractFirstURL('http://example.com/x')).toBe('http://example.com/x')
    expect(extractFirstURL('example.com 不算链接')).toBe('')
  })

  it('粗体节点里的链接', () => {
    expect(extractFirstURL('**https://example.com/bold**')).toBe('https://example.com/bold')
  })

  it('引用块里的链接', () => {
    expect(extractFirstURL('> https://example.com/quote')).toBe('https://example.com/quote')
  })

  it('围栏代码块内的链接不算', () => {
    expect(extractFirstURL('```\nhttps://example.com/code\n```')).toBe('')
  })

  it('行内代码内的链接不算', () => {
    expect(extractFirstURL('`https://example.com/inline` 只是代码')).toBe('')
  })

  it('代码块之后正文里的链接仍可提取', () => {
    expect(extractFirstURL('```\nhttps://example.com/code\n```\n正文 https://example.com/real')).toBe(
      'https://example.com/real',
    )
  })

  it('多个 URL 取第一个', () => {
    expect(extractFirstURL('https://a.example/1 和 https://b.example/2')).toBe('https://a.example/1')
  })

  it('裁掉句末中英文标点与右括号', () => {
    expect(extractFirstURL('见 https://example.com/a。')).toBe('https://example.com/a')
    expect(extractFirstURL('（https://example.com/b)')).toBe('https://example.com/b')
    expect(extractFirstURL('https://example.com/c),')).toBe('https://example.com/c')
    expect(extractFirstURL('https://example.com/d?x=1!?')).toBe('https://example.com/d?x=1')
  })

  it('引号/尖括号终止 URL，不会吞进后续文字', () => {
    expect(extractFirstURL('"https://example.com/e" 后面')).toBe('https://example.com/e')
  })

  it('无链接与空正文返回空串', () => {
    expect(extractFirstURL('')).toBe('')
    expect(extractFirstURL('没有链接的普通消息')).toBe('')
  })
})
