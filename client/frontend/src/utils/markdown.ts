// 消息 Markdown 子集解析（#42 B3）：只产出结构化节点，绝不拼 HTML 字符串。
// 子集：**粗体**（不嵌套、不支持斜体）、`行内代码`、```围栏代码块```（忽略语言）、
// 连续 `> ` 引用行、其余为保留换行的段落。所有原文（包括 <script> 等）都只作为
// 文本节点的 text 保留，渲染侧用文本插值，不存在 HTML 执行路径。

export type InlineNode =
  | { type: 'text'; text: string }
  | { type: 'bold'; text: string }
  | { type: 'code'; text: string }
  | { type: 'mention'; text: string }

export type Block =
  | { type: 'paragraph'; nodes: InlineNode[] }
  | { type: 'quote'; nodes: InlineNode[] }
  | { type: 'code'; text: string }

// 提及沿用现有规则：@ + 字母/数字/下划线/连字符（中文昵称落在 \p{L}）。
const MENTION_RE = /@[\p{L}\p{N}_-]+/gu

// mentionAt 只在 at 位置正好是提及开头时返回完整提及（全局正则向后找，
// 命中下标不等于 at 说明当前位置不是 @ 或后面不构成昵称）。
function mentionAt(src: string, at: number): string {
  MENTION_RE.lastIndex = at
  const m = MENTION_RE.exec(src)
  return m && m.index === at ? m[0] : ''
}

// pushBold 把粗体内部切成 bold/mention 节点：提及仍需高亮，但不再解析代码或嵌套粗体。
function pushBold(out: InlineNode[], inner: string): void {
  let from = 0
  for (;;) {
    MENTION_RE.lastIndex = from
    const m = MENTION_RE.exec(inner)
    if (!m) break
    if (m.index > from) out.push({ type: 'bold', text: inner.slice(from, m.index) })
    out.push({ type: 'mention', text: m[0] })
    from = m.index + m[0].length
  }
  if (from < inner.length) out.push({ type: 'bold', text: inner.slice(from) })
}

// parseInline 行内解析。未闭合的 ** / 反引号整体按字面文本输出；
// 行内代码内部不做任何解析；粗体内部的 @提及 拆成独立 mention 节点。
function parseInline(src: string): InlineNode[] {
  const out: InlineNode[] = []
  let buf = ''
  const flush = () => {
    if (buf) {
      out.push({ type: 'text', text: buf })
      buf = ''
    }
  }
  let i = 0
  while (i < src.length) {
    const c = src[i]
    if (c === '`') {
      const close = src.indexOf('`', i + 1)
      // 空代码段（``）不算围栏，按字面保留，避免产出空 code 节点。
      if (close > i + 1) {
        flush()
        out.push({ type: 'code', text: src.slice(i + 1, close) })
        i = close + 1
        continue
      }
      buf += c
      i++
      continue
    }
    if (c === '*' && src[i + 1] === '*') {
      const close = src.indexOf('**', i + 2)
      // 空粗体（****）不成立，按字面保留。
      if (close > i + 2) {
        flush()
        pushBold(out, src.slice(i + 2, close))
        i = close + 2
        continue
      }
      buf += '**'
      i += 2
      continue
    }
    if (c === '@') {
      const m = mentionAt(src, i)
      if (m) {
        flush()
        out.push({ type: 'mention', text: m })
        i += m.length
        continue
      }
    }
    buf += c
    i++
  }
  flush()
  return out
}

// isFenceLine 围栏代码块起止行：以 ``` 开头即可，语言标识（```ts）一并忽略。
function isFenceLine(line: string): boolean {
  return line.startsWith('```')
}

// isQuoteLine 引用行：`>` 后可跟一个空格；不带空格的 `>引用` 也按引用处理。
function isQuoteLine(line: string): boolean {
  return line.startsWith('>')
}

function quoteContent(line: string): string {
  return line.replace(/^> ?/, '')
}

export function parseMarkdown(input: string): Block[] {
  const lines = input.replace(/\r\n?/g, '\n').split('\n')
  const blocks: Block[] = []
  let i = 0
  while (i < lines.length) {
    const line = lines[i]
    if (isFenceLine(line)) {
      // 未闭合围栏把剩余全部行都当作代码内容（不解析 inline）。
      const body: string[] = []
      i++
      while (i < lines.length && !isFenceLine(lines[i])) {
        body.push(lines[i])
        i++
      }
      if (i < lines.length) i++ // 跳过闭合围栏
      blocks.push({ type: 'code', text: body.join('\n') })
      continue
    }
    if (isQuoteLine(line)) {
      const body: string[] = []
      while (i < lines.length && isQuoteLine(lines[i])) {
        body.push(quoteContent(lines[i]))
        i++
      }
      blocks.push({ type: 'quote', nodes: parseInline(body.join('\n')) })
      continue
    }
    // 空白行只分隔段落，不产出空段落。
    if (line.trim() === '') {
      i++
      continue
    }
    const body: string[] = []
    while (
      i < lines.length
      && lines[i].trim() !== ''
      && !isFenceLine(lines[i])
      && !isQuoteLine(lines[i])
    ) {
      body.push(lines[i])
      i++
    }
    blocks.push({ type: 'paragraph', nodes: parseInline(body.join('\n')) })
  }
  return blocks
}

// plainTextMarkdown 去掉 Markdown 标记后的纯文本：摘要、回复预览与搜索高亮用。
// 块之间以换行连接；调用方通常还会折叠空白。
export function plainTextMarkdown(input: string): string {
  const parts: string[] = []
  for (const b of parseMarkdown(input)) {
    if (b.type === 'code') parts.push(b.text)
    else parts.push(b.nodes.map((n) => n.text).join(''))
  }
  return parts.join('\n')
}
