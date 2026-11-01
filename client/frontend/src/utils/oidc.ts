// OIDC 回调工具（#118 P2-5）：redirect_url 由部署者在服务端配置，指向本 Web
// 客户端的回调路由。参数可能落在两个位置，取决于 redirect_url 的形态：
//   - https://host/#/oidc/callback → IdP 把 ?code&state 追加在 hash 段；
//   - https://host/oidc/callback（SPA fallback 回壳）→ 参数在 search，hash 为空。
// 两处都解析，hash 优先；search 形态靠下面的模块级改写接住。
import { cleanErrorText } from './errorText'

export interface OidcCallbackParams {
  code: string
  state: string
}

// extractOidcCallbackParams 从 search 与 hash 里取 code/state（hash 优先）。
// 纯函数：OidcCallbackView 与测试共用，不允许再摸全局 location。
export function extractOidcCallbackParams(search: string, hash: string): OidcCallbackParams {
  const fromHash = new URLSearchParams(hash.includes('?') ? hash.slice(hash.indexOf('?') + 1) : '')
  const fromSearch = new URLSearchParams(search)
  return {
    code: fromHash.get('code') ?? fromSearch.get('code') ?? '',
    state: fromHash.get('state') ?? fromSearch.get('state') ?? '',
  }
}

// oidcCallbackErrorText 回调失败文案（区分 state 失效与其余交换失败）：
// bad_state（state 无效/过期/重放）给「重新发起」的明确指引；其余优先展示
// 清洗后的服务端正文（bad_code/oidc_upstream 的 detail 已足够具体），取不到
// 正文才回落通用文案。桌面错误串与 web APIError 都归一到这里。
export function oidcCallbackErrorText(e: unknown): string {
  const code = (e as { code?: unknown } | null)?.code
  if (code === 'bad_state') return '登录状态无效或已过期，请回到登录页重新发起'
  return cleanErrorText(e, '') || '登录交换失败，请回到登录页重新发起'
}

// web 构建且以路径形 redirect_url 回跳（/oidc/callback?code=…）时，SPA fallback
// 送回的页面 hash 为空，vue-router（hash 模式）看不到路径会走 catch-all 重定向，
// 参数就丢了。必须在路由初始化前把地址改写成 hash 路由并原样保留 query；
// 与 deeplink.ts 同款「模块加载即捕获」时序（router 静态 import 本模块的使用方，
// 求值先于首次路由解析）。非 web 构建没有部署方 redirect_url，不启用。
if (
  import.meta.env.MODE === 'web' &&
  /^\/oidc\/callback\/?$/.test(location.pathname) &&
  !location.hash.startsWith('#/oidc/callback')
) {
  history.replaceState(null, '', `/#/oidc/callback${location.search}`)
}
