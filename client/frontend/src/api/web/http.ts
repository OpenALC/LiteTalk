// Web 版 REST 传输层：fetch 封装 + Bearer 注入 + 401 单飞刷新重试一次。
// 语义对照 Go 侧 client/core/api/api.go 的 APIClient（do/refreshOnce/doRefresh），
// 差异点：基址恒为同源（Web 客户端由服务端托管，无换服/热切换基址的路径），
// 因此不存在 Go 侧 errBaseChanged 一族的基址快照逻辑。
import { webEmit } from './events'
import { WEB_CLIENT_VERSION } from './version'

// localStorage 键名（批次任务书定死；CSP 允许 localStorage，XSS 风险口径见 index.html 注释）。
const TOKEN_KEY = 'litetalk.web.token'
const REFRESH_KEY = 'litetalk.web.refresh'

// 同源 API 前缀。web 构建恒与服务端同源部署；dev 预览走 mock，不经本模块。
const API_PREFIX = '/api/v1'

// 请求超时：与 Go 客户端一致（15s）。大文件上传（P2 未做）届时再放宽。
const REQUEST_TIMEOUT_MS = 15_000

// APIError 服务端错误（RFC 9457 problem+json 的 {code,detail} + HTTP 状态）。
// 与 Go 侧 APIError 字段同口径：Message 取 detail，兼容旧 message 字段。
export class APIError extends Error {
  readonly code: string
  readonly status: number
  readonly retryAfter: number

  constructor(status: number, code: string, message: string, retryAfter = 0) {
    super(message)
    this.name = 'APIError'
    this.status = status
    this.code = code
    this.retryAfter = retryAfter
  }
}

let accessToken = ''
let refreshToken = ''

// restoreTokens 从 localStorage 恢复持久化登录态。
export function restoreTokens(): void {
  try {
    accessToken = localStorage.getItem(TOKEN_KEY) ?? ''
    refreshToken = localStorage.getItem(REFRESH_KEY) ?? ''
  } catch {
    // localStorage 不可用（隐私模式极端情况）：按未登录处理，不用持久化就不续期。
    accessToken = ''
    refreshToken = ''
  }
}

// 模块加载即恢复：路由守卫的 GetMe（刷新后免重登路径）可能在模块加载后立即
// 发请求，令牌必须先于首个请求就位。
restoreTokens()

// getAccessToken 当前 access token（WS 拨号复用同一来源，保证 REST 刷新后重连带新值）。
export function getAccessToken(): string {
  return accessToken
}

// getRefreshToken 当前 refresh token（Logout 请求体用；吊销凭据在 body 不走头）。
export function getRefreshToken(): string {
  return refreshToken
}

export function hasSession(): boolean {
  return accessToken !== '' || refreshToken !== ''
}

// persistTokens 落盘。写失败不阻断登录（内存态仍可用，仅刷新续期依赖落盘）。
function persistTokens(): void {
  try {
    if (accessToken) localStorage.setItem(TOKEN_KEY, accessToken)
    else localStorage.removeItem(TOKEN_KEY)
    if (refreshToken) localStorage.setItem(REFRESH_KEY, refreshToken)
    else localStorage.removeItem(REFRESH_KEY)
  } catch {
    // 隐私模式等写失败：忽略，会话只活在本页生命周期。
  }
}

// setTokens 安装登录/刷新得到的令牌对并持久化。
export function setTokens(access: string, refresh: string): void {
  accessToken = access
  refreshToken = refresh
  persistTokens()
}

// clearTokens 清空登录态（登出/被踢）。不调 /auth/logout——吊销由调用方显式做。
export function clearTokens(): void {
  accessToken = ''
  refreshToken = ''
  persistTokens()
}

// decodeError 解析 problem+json；解析不出 code 时给出可展示的兜底文案。
function decodeError(status: number, body: string): APIError {
  try {
    const raw = JSON.parse(body) as { code?: string; detail?: string; message?: string }
    const msg = raw.detail || raw.message || ''
    if (raw.code || msg) return new APIError(status, raw.code ?? 'unknown', msg)
  } catch {
    // 非 JSON 响应体（反代 502 页面等）
  }
  return new APIError(status, 'network', `HTTP ${status}`)
}

// retryAfterSeconds 解析 429 响应头 Retry-After（服务端固定写秒整数）。
function retryAfterSeconds(res: Response): number {
  const n = Number.parseInt(res.headers.get('Retry-After') ?? '', 10)
  return Number.isFinite(n) && n > 0 ? n : 0
}

// isSessionReplaced 判定 401 属会话顶替/重放（头与 body code 同值，任一命中即可）。
function isSessionReplaced(res: Response, body: string): boolean {
  const h = res.headers.get('X-LiteTalk-Auth')
  if (h === 'session_replaced' || h === 'session_replayed') return true
  try {
    const code = (JSON.parse(body) as { code?: string }).code
    return code === 'session_replaced' || code === 'session_replayed'
  } catch {
    return false
  }
}

// ---- 单飞刷新（对照 Go refreshOnce：并发 401 合并成一次 /auth/refresh）----

let refreshInflight: Promise<void> | null = null

// refreshAccess 单飞刷新一次。刷新被服务端明确拒绝（401/403）→ 清登录态并派
// auth:kicked(login_expired)，上层走回登录页；会话被顶替 → auth:kicked(login_elsewhere)。
// 传输层失败（网络/5xx/429）原样上抛，调用方按需退避——令牌本身未必失效。
export function refreshAccess(): Promise<void> {
  if (!refreshInflight) {
    refreshInflight = doRefresh().finally(() => {
      refreshInflight = null
    })
  }
  return refreshInflight
}

async function doRefresh(): Promise<void> {
  if (!refreshToken) throw new APIError(401, 'unauthorized', '缺少刷新令牌')
  const res = await fetch(`${API_PREFIX}/auth/refresh`, {
    method: 'POST',
    headers: {
      'Content-Type': 'application/json',
      // 刷新请求也带版本头：服务端升级门对 /auth/* 全量生效。
      'X-LiteTalk-Client': WEB_CLIENT_VERSION,
    },
    body: JSON.stringify({ refresh_token: refreshToken }),
    signal: AbortSignal.timeout(REQUEST_TIMEOUT_MS),
  })
  const body = await res.text()
  if (!res.ok) {
    if (isSessionReplaced(res, body)) {
      const e = new APIError(res.status, 'session_replaced', '账号已在其他设备登录')
      clearTokens()
      webEmit('auth:kicked', { reason: 'login_elsewhere' })
      throw e
    }
    // 401/403 = refresh_token 被明确拒绝（吊销/过期）：刷新救不回，按致命处理。
    if (res.status === 401 || res.status === 403) {
      clearTokens()
      webEmit('auth:kicked', { reason: 'login_expired' })
    }
    throw decodeError(res.status, body)
  }
  const t = JSON.parse(body) as { access_token: string; refresh_token: string; user?: unknown }
  accessToken = t.access_token
  refreshToken = t.refresh_token
  persistTokens()
  // 响应带回 user（Go 侧游客续期依赖它免一次 /users/me 往返）；web 侧当前无消费方，
  // 暂不上抛——需要时从响应取。
}

// request 统一请求入口。auth=false 的端点（登录/注册/config）不带 Bearer 也不做
// 401 刷新重试；auth=true 的请求 401 时刷新一次原样重放（对照 Go doOnce 的 retried 防递归）。
export async function request<T = unknown>(
  method: 'GET' | 'POST' | 'PATCH' | 'PUT' | 'DELETE',
  path: string,
  opts: { body?: unknown; auth?: boolean; timeoutMs?: number } = {},
): Promise<T> {
  const auth = opts.auth !== false
  const doOnce = async (retried: boolean): Promise<Response> => {
    const headers: Record<string, string> = {
      // 升级门（#31）：/auth/* 缺失该头直接 426，值必须可解析为 >=1.5。
      'X-LiteTalk-Client': WEB_CLIENT_VERSION,
    }
    if (opts.body !== undefined) headers['Content-Type'] = 'application/json'
    if (auth && accessToken) headers.Authorization = `Bearer ${accessToken}`
    return fetch(`${API_PREFIX}${path}`, {
      method,
      headers,
      body: opts.body !== undefined ? JSON.stringify(opts.body) : undefined,
      signal: AbortSignal.timeout(opts.timeoutMs ?? REQUEST_TIMEOUT_MS),
    })
  }

  let res = await doOnce(false)
  let body = await res.text()
  if (res.status === 401 && auth) {
    if (isSessionReplaced(res, body)) {
      clearTokens()
      webEmit('auth:kicked', { reason: 'login_elsewhere' })
      throw new APIError(res.status, 'session_replaced', '账号已在其他设备登录')
    }
    if (refreshToken) {
      // 401 刷新重试预算一次（对照 Go doOnce 的 retried 防递归）。
      await refreshAccess()
      res = await doOnce(true)
      body = await res.text()
    }
  }
  if (res.status === 429) {
    throw new APIError(res.status, codeOf(body), messageOf(body), retryAfterSeconds(res))
  }
  if (!res.ok) throw decodeError(res.status, body)
  if (!body) return undefined as T
  return JSON.parse(body) as T
}

function codeOf(body: string): string {
  try {
    return (JSON.parse(body) as { code?: string }).code ?? 'rate_limited'
  } catch {
    return 'rate_limited'
  }
}

function messageOf(body: string): string {
  try {
    const raw = JSON.parse(body) as { detail?: string; message?: string }
    return raw.detail || raw.message || ''
  } catch {
    return ''
  }
}

// ---- 便捷封装（语义对齐 Go APIClient 的 get/post/patch/del）----

export function apiGet<T>(path: string, timeoutMs?: number): Promise<T> {
  return request<T>('GET', path, { timeoutMs })
}

export function apiPost<T>(path: string, body?: unknown, opts: { auth?: boolean } = {}): Promise<T> {
  return request<T>('POST', path, { body, auth: opts.auth })
}

export function apiPatch<T>(path: string, body?: unknown): Promise<T> {
  return request<T>('PATCH', path, { body })
}

export function apiPut<T>(path: string, body?: unknown): Promise<T> {
  return request<T>('PUT', path, { body })
}

// apiDel DELETE：204 无响应体返回 undefined；200（如会话成员移除的剩余会话）
// 返回解析后的 JSON。调用方按「undefined = 空响应」处理。
export function apiDel<T = void>(path: string): Promise<T | undefined> {
  return request<T | undefined>('DELETE', path, {})
}
