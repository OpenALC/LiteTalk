// 错误文案清洗（#15）：Wails 绑定把 Go 的 error 包成 RuntimeError，String(err)
// 会带上 "RuntimeError: " 前缀；Go 侧 APIError.Error() 又是「<机器码>: <中文说明>」，
// 调用链还会再包一层方法+路径。直接展示会让用户看到一串机器前缀：
//   RuntimeError: get /servers/9/channels: forbidden: 不是该服务器成员
//   → 不是该服务器成员
// 只剥已知的机器前缀，正文原样保留——底层网络错误等有用信息不能被吞掉。

const RUNTIME_PREFIX = /^(?:RuntimeError|Error)\s*:\s*/
// 机器码（服务端 code 的形态是 snake_case）：forbidden: / bad_credentials: …
// 限定小写开头并带空白，避免误伤正常英文句子与 http:// 之类的内容。
const CODE_PREFIX = /^[a-z][a-z0-9_]*:\s+/
// Go 侧操作包装：get /servers/9/channels: / decode servers: / marshal payload: …
const OPERATION_PREFIX = /^(?:get|post|put|patch|delete|decode|encode|marshal)\b[^:]*?:\s+/i

// messageOf 取可展示的原始文案：Error / Wails RuntimeError 取 message（String(err)
// 会拼上错误名），字符串原样使用，其余对象只在有 string message 时采用。
function messageOf(e: unknown): string {
  if (typeof e === 'string') return e
  if (e && typeof e === 'object') {
    const m = (e as { message?: unknown }).message
    return typeof m === 'string' ? m : '' // 无 message 的对象（如 rejection 载荷）交给 fallback
  }
  return e == null ? '' : String(e)
}

// cleanErrorText 逐层剥掉运行时前缀、Go 操作包装与机器码，保留服务端正文。
// 清洗后为空时回落 fallback；正文里的非中文原因（如 dial tcp: connection refused）
// 原样返回，不为了「好看」把它藏掉。
export function cleanErrorText(e: unknown, fallback = ''): string {
  let text = messageOf(e).trim()
  let prev = ''
  // 前缀可能叠三层（RuntimeError + 方法路径 + code），循环到不再变化为止。
  while (text && text !== prev) {
    prev = text
    text = text
      .replace(RUNTIME_PREFIX, '')
      .replace(OPERATION_PREFIX, '')
      .replace(CODE_PREFIX, '')
      .trim()
  }
  return text || fallback
}
