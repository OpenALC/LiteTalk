// 搜索面板纯函数（#42 B2）：频道标签、摘要高亮切片、时间格式化。
// #42 B3：摘要走 Markdown 纯文本，标记不进入搜索结果。
import { describe, expect, it } from 'vitest'
import { channelLabel, excerptSegments, formatSearchTime, type ChannelLabelSource } from './search'

const source: ChannelLabelSource = {
  conversations: [{ id: 'c1', name: '小王' }, { id: 'c2', name: '' }],
  servers: [{ name: '开发组', channels: [{ id: 's1', name: '闲聊' }] }],
}

describe('channelLabel', () => {
  it('会话 ID 命中用显示名', () => {
    expect(channelLabel('c1', source)).toBe('小王')
  })
  it('会话显示名为空时回退「会话」', () => {
    expect(channelLabel('c2', source)).toBe('会话')
  })
  it('服务器频道拼「服务器名 / #频道名」', () => {
    expect(channelLabel('s1', source)).toBe('开发组 / #闲聊')
  })
  it('都找不到时给中性占位，不暴露裸 ID', () => {
    expect(channelLabel('999', source)).toBe('#频道')
    expect(channelLabel('', source)).toBe('#频道')
  })
})

describe('excerptSegments', () => {
  it('折叠空白并截断到上限，超出追加省略号', () => {
    const text = `  第一行\n\n第二行  ${'长'.repeat(140)}`
    const segs = excerptSegments(text, '')
    const joined = segs.map((s) => s.text).join('')
    expect(joined.startsWith('第一行 第二行 ')).toBe(true)
    expect(joined.length).toBe(121) // 120 字符 + 省略号
    expect(joined.endsWith('…')).toBe(true)
    expect(segs.every((s) => !s.hit)).toBe(true)
  })

  it('按查询词切片高亮，可多处命中且大小写不敏感', () => {
    const segs = excerptSegments('Hello world, hello Vue', 'hello')
    expect(segs.map((s) => s.text).join('')).toBe('Hello world, hello Vue')
    expect(segs.filter((s) => s.hit).map((s) => s.text)).toEqual(['Hello', 'hello'])
  })

  it('中文子串命中', () => {
    const segs = excerptSegments('今天去钓鱼，钓到一条鱼', '钓鱼')
    expect(segs.filter((s) => s.hit).map((s) => s.text)).toEqual(['钓鱼'])
  })

  it('查询词为空或未命中时返回单段未高亮文本', () => {
    expect(excerptSegments('abc', '   ')).toEqual([{ text: 'abc', hit: false }])
    expect(excerptSegments('abc', 'zzz')).toEqual([{ text: 'abc', hit: false }])
  })

  it('空正文返回空数组', () => {
    expect(excerptSegments('   \n ', 'x')).toEqual([])
  })

  it('查询词长于正文时不误切片', () => {
    expect(excerptSegments('短', '很长很长的查询词')).toEqual([{ text: '短', hit: false }])
  })

  it('摘要基于 Markdown 纯文本，不显示粗体/行内代码标记', () => {
    const segs = excerptSegments('查看 **重点** 与 `code`', '重点')
    expect(segs.map((s) => s.text).join('')).toBe('查看 重点 与 code')
    expect(segs.filter((s) => s.hit).map((s) => s.text)).toEqual(['重点'])
  })

  it('围栏代码块与引用行去掉标记后进入摘要', () => {
    const segs = excerptSegments('> 引用行\n```\nconst a = 1\n```', '')
    expect(segs.map((s) => s.text).join('')).toBe('引用行 const a = 1')
  })
})

describe('formatSearchTime', () => {
  const now = new Date(2026, 9, 25, 18, 0, 0).getTime()

  it('今天显示 HH:MM', () => {
    const ts = String(new Date(2026, 9, 25, 9, 5, 0).getTime())
    expect(formatSearchTime(ts, now)).toBe('09:05')
  })

  it('同年显示 M月D日 HH:MM', () => {
    const ts = String(new Date(2026, 2, 3, 8, 7, 0).getTime())
    expect(formatSearchTime(ts, now)).toBe('3月3日 08:07')
  })

  it('跨年只显示日期', () => {
    const ts = String(new Date(2025, 11, 31, 23, 59, 0).getTime())
    expect(formatSearchTime(ts, now)).toBe('2025年12月31日')
  })

  it('非法时间戳返回空串', () => {
    expect(formatSearchTime('', now)).toBe('')
    expect(formatSearchTime('abc', now)).toBe('')
    expect(formatSearchTime('0', now)).toBe('')
  })
})
