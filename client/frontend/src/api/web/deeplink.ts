// Web 深链适配（#50 验收：无客户端也能通过邀请链接进房）。
//
// 服务端已把 /join/<code> SPA fallback 到 index.html（static.go）；本模块在
// web 构建加载时从 pathname 摘出邀请码暂存，并把地址清回 /（hash 路由的
// 当前路由不动）。登录完成前暂存不能丢（未登录 → 登录/注册 → 回到主界面
// 才消费），所以用 sessionStorage 而不是模块变量。
//
// 消费方是 ServerRail（挂载时取码并打开预填的 ServerDialog）——进房确认
// 沿用既有弹层流程，深链不做静默入服。

// 暂存键与有效期。10 分钟：覆盖「打开链接 → 登录 → 进主界面」的合理时长，
// 也防止隔天的旧码在用户不知情时突然弹窗。
const PENDING_KEY = 'litetalk.web.pending_invite'
const PENDING_TTL_MS = 10 * 60 * 1000

// invitePathCode 识别 /join/<code>；code 与服务端 CreateInvite 产出口径一致
//（32 位小写 hex），宽松按 URL 段校验，非法值在提交时由服务端拒绝并提示。
export function invitePathCode(pathname: string): string | null {
  const m = /^\/join\/([A-Za-z0-9]+)\/?$/.exec(pathname)
  return m ? m[1] : null
}

// 模块加载即捕获：ServerRail 静态 import 本模块，加载先于任何挂载与路由
// 跳转，pathname 此刻仍是服务端 fallback 下来的 /join/<code>。
if (import.meta.env.MODE === 'web') {
  const code = invitePathCode(location.pathname)
  if (code) {
    try {
      sessionStorage.setItem(PENDING_KEY, JSON.stringify({ code, ts: Date.now() }))
    } catch {
      // sessionStorage 不可用：深链降级为手动输入，不阻断启动
    }
    // 清掉路径保留 hash：刷新不再重复弹窗，路由状态不受影响。
    history.replaceState(null, '', '/' + location.search + location.hash)
  }
}

// consumePendingInviteCode 取出并清除暂存的邀请码；过期/损坏返回 null。
export function consumePendingInviteCode(): string | null {
  try {
    const raw = sessionStorage.getItem(PENDING_KEY)
    if (!raw) return null
    sessionStorage.removeItem(PENDING_KEY)
    const saved = JSON.parse(raw) as { code?: string; ts?: number }
    if (!saved.code || typeof saved.ts !== 'number') return null
    if (Date.now() - saved.ts > PENDING_TTL_MS) return null
    return saved.code
  } catch {
    return null
  }
}
