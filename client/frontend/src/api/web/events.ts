// Web 版事件总线：signal/rtc 层把服务端信令转成与 Go 桌面端 EventsEmit 同名
// 同形的事件，bridge.eventsOn 在 web 构建下路由到这里。事件名与载荷形状是
// 与桌面端共享的前端契约（stores 依赖），两边都必须对齐 app.go 的 emit 口径。
type Listener = (data: unknown) => void

const listeners = new Map<string, Set<Listener>>()

// webEmit 派发一个事件（监听器异常互不影响，模仿 Wails EventsEmit 的隔离语义）。
export function webEmit(name: string, data?: unknown): void {
  const set = listeners.get(name)
  if (!set) return
  for (const fn of [...set]) {
    try {
      fn(data)
    } catch (e) {
      console.error(`[web] 事件 ${name} 监听器异常`, e)
    }
  }
}

// webEventsOn 订阅事件，返回按监听器的取消函数（与 bridge.eventsOn 契约一致）。
export function webEventsOn(name: string, cb: Listener): () => void {
  let set = listeners.get(name)
  if (!set) {
    set = new Set()
    listeners.set(name, set)
  }
  set.add(cb)
  return () => {
    set!.delete(cb)
  }
}

// webEventsOff 按事件名整体注销（休眠态清理用，对应 bridge.eventsOff）。
export function webEventsOff(name: string): void {
  listeners.delete(name)
}
