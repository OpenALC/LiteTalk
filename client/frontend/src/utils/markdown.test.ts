// Markdown 子集解析（#42 B3）：块结构、行内节点、纯文本摘要与 XSS 安全性。
import { describe, expect, it } from 'vitest'
import { parseMarkdown, plainTextMarkdown, type Block, type InlineNode } from './markdown'

// inlineOf 取单一非代码块的行内节点，便于断言。
function inlineOf(input: string): InlineNode[] {
  const blocks = parseMarkdown(input)
  expect(blocks).toHaveLength(1)
  const b = blocks[0]
  if (b.type === 'code') throw new Error('expected inline block')
  return b.nodes
}

describe('parseMarkdown 段落', () => {
  it('普通段落保留换行', () => {
    expect(parseMarkdown('第一行\n第二行')).toEqual<Block[]>([
      { type: 'paragraph', nodes: [{ type: 'text', text: '第一行\n第二行' }] },
    ])
  })

  it('空行分隔多个段落，空输入不产出块', () => {
    expect(parseMarkdown('a\n\nb')).toEqual<Block[]>([
      { type: 'paragraph', nodes: [{ type: 'text', text: 'a' }] },
      { type: 'paragraph', nodes: [{ type: 'text', text: 'b' }] },
    ])
    expect(parseMarkdown('')).toEqual([])
    expect(parseMarkdown(' \n\n ')).toEqual([])
  })
})

describe('parseMarkdown 行内', () => {
  it('粗体拆成 bold 节点，两侧保留 text', () => {
    expect(inlineOf('这是 **重点** 内容')).toEqual<InlineNode[]>([
      { type: 'text', text: '这是 ' },
      { type: 'bold', text: '重点' },
      { type: 'text', text: ' 内容' },
    ])
  })

  it('未闭合 ** 按字面文本输出', () => {
    expect(inlineOf('**未闭合')).toEqual<InlineNode[]>([
      { type: 'text', text: '**未闭合' },
    ])
  })

  it('行内代码内部不再解析粗体与提及', () => {
    expect(inlineOf('用 `**not bold** @alice` 运行')).toEqual<InlineNode[]>([
      { type: 'text', text: '用 ' },
      { type: 'code', text: '**not bold** @alice' },
      { type: 'text', text: ' 运行' },
    ])
  })

  it('未闭合反引号按字面文本输出', () => {
    expect(inlineOf('a ` b')).toEqual<InlineNode[]>([
      { type: 'text', text: 'a ` b' },
    ])
  })

  it('普通提及与中文昵称识别为 mention', () => {
    expect(inlineOf('你好 @alice')).toEqual<InlineNode[]>([
      { type: 'text', text: '你好 ' },
      { type: 'mention', text: '@alice' },
    ])
    expect(inlineOf('@小王-1_2 你好')).toEqual<InlineNode[]>([
      { type: 'mention', text: '@小王-1_2' },
      { type: 'text', text: ' 你好' },
    ])
    expect(inlineOf('邮箱 a@b')).toEqual<InlineNode[]>([
      { type: 'text', text: '邮箱 a' },
      { type: 'mention', text: '@b' },
    ])
  })

  it('单独的 @ 不是提及', () => {
    expect(inlineOf('@ 你好')).toEqual<InlineNode[]>([
      { type: 'text', text: '@ 你好' },
    ])
  })

  it('粗体内部的提及拆成 mention 节点，其余仍是 bold', () => {
    expect(inlineOf('**@alice 你好**')).toEqual<InlineNode[]>([
      { type: 'mention', text: '@alice' },
      { type: 'bold', text: ' 你好' },
    ])
  })

  it('粗体内部不解析行内代码（不支持嵌套）', () => {
    expect(inlineOf('**看 `x`**')).toEqual<InlineNode[]>([
      { type: 'bold', text: '看 `x`' },
    ])
  })
})

describe('parseMarkdown 围栏代码块', () => {
  it('语言标识忽略，内容保留换行且不解析行内标记', () => {
    expect(parseMarkdown('```ts\nconst a = **1**\n@alice\n```')).toEqual<Block[]>([
      { type: 'code', text: 'const a = **1**\n@alice' },
    ])
  })

  it('未闭合围栏把剩余全部行当作代码', () => {
    expect(parseMarkdown('```\nline1\nline2')).toEqual<Block[]>([
      { type: 'code', text: 'line1\nline2' },
    ])
  })

  it('空代码块', () => {
    expect(parseMarkdown('```\n```')).toEqual<Block[]>([
      { type: 'code', text: '' },
    ])
  })
})

describe('parseMarkdown 引用', () => {
  it('连续 > 行合并成一个 quote 块', () => {
    expect(parseMarkdown('> 第一行\n> **第二行**\n普通段落')).toEqual<Block[]>([
      {
        type: 'quote',
        nodes: [
          { type: 'text', text: '第一行\n' },
          { type: 'bold', text: '第二行' },
        ],
      },
      { type: 'paragraph', nodes: [{ type: 'text', text: '普通段落' }] },
    ])
  })

  it('空行分隔的引用拆成两个块', () => {
    expect(parseMarkdown('> a\n\n> b')).toEqual<Block[]>([
      { type: 'quote', nodes: [{ type: 'text', text: 'a' }] },
      { type: 'quote', nodes: [{ type: 'text', text: 'b' }] },
    ])
  })
})

describe('parseMarkdown 混合结构', () => {
  it('段落、引用、代码块与结尾段落按顺序产出', () => {
    expect(parseMarkdown('标题\n\n> 引用 **重点**\n\n```js\ncode\n```\n结尾')).toEqual<Block[]>([
      { type: 'paragraph', nodes: [{ type: 'text', text: '标题' }] },
      {
        type: 'quote',
        nodes: [
          { type: 'text', text: '引用 ' },
          { type: 'bold', text: '重点' },
        ],
      },
      { type: 'code', text: 'code' },
      { type: 'paragraph', nodes: [{ type: 'text', text: '结尾' }] },
    ])
  })
})

describe('plainTextMarkdown', () => {
  it('去掉粗体、行内代码、引用与围栏标记', () => {
    const src = '**粗体** 和 `code`\n> 引用\n```\nblock\n```'
    expect(plainTextMarkdown(src)).toBe('粗体 和 code\n引用\nblock')
  })

  it('提及文本保留 @ 前缀', () => {
    expect(plainTextMarkdown('你好 **@alice**')).toBe('你好 @alice')
  })

  it('空输入返回空串', () => {
    expect(plainTextMarkdown('')).toBe('')
  })
})

describe('XSS 安全', () => {
  const payloads = [
    '<script>alert(1)</script>',
    '<img src=x onerror=alert(1)>',
    '[点我](javascript:alert(1))',
    '<b>加粗标签</b>',
    '&lt;script&gt;alert(1)&lt;/script&gt;',
  ]

  it.each(payloads)('HTML 载荷只作为文本节点保留：%s', (payload) => {
    const blocks = parseMarkdown(payload)
    expect(blocks).toEqual<Block[]>([
      { type: 'paragraph', nodes: [{ type: 'text', text: payload }] },
    ])
    // 结构里不存在 html 类型，渲染侧只能走文本插值。
    expect(JSON.stringify(blocks)).not.toContain('"html"')
  })

  it('引用内的 HTML 载荷同样是纯文本', () => {
    expect(parseMarkdown('> <img src=x onerror=alert(1)>')).toEqual<Block[]>([
      { type: 'quote', nodes: [{ type: 'text', text: '<img src=x onerror=alert(1)>' }] },
    ])
  })

  it('行内代码中的 HTML 载荷按代码文本保留', () => {
    expect(inlineOf('`<script>alert(1)</script>`')).toEqual<InlineNode[]>([
      { type: 'code', text: '<script>alert(1)</script>' },
    ])
  })
})
