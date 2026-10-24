// 消息正文里的首个链接提取（#42 B4）。
// 只扫描 Markdown 的 paragraph/quote 块中 text/bold/mention 节点拼接出的文本：
// 围栏代码块与行内代码里的 URL 不算（用户是在展示代码，不是分享链接）。
// 仅做展示前的粗筛，真正的合法性由服务端抓取层校验。
import { parseMarkdown } from './markdown'

// URL_RE 不用全局标志，避免 lastIndex 在多次调用间残留。
const URL_RE = /https?:\/\/[^\s<>"']+/

// TRAILING_PUNCT 句末标点常被误吞进 URL：去掉末尾的英文/中文标点与右括号。
const TRAILING_PUNCT = /[.,;:!?)\]}。，；：！？]+$/

// extractFirstURL 返回正文里第一个 http(s) 链接（已裁掉句末标点）；没有则空串。
export function extractFirstURL(content: string): string {
  if (!content) return ''
  for (const block of parseMarkdown(content)) {
    if (block.type === 'code') continue
    const text = block.nodes
      .filter((n) => n.type === 'text' || n.type === 'bold' || n.type === 'mention')
      .map((n) => n.text)
      .join('')
    const m = URL_RE.exec(text)
    if (!m) continue
    const url = m[0].replace(TRAILING_PUNCT, '')
    if (url) return url
  }
  return ''
}
