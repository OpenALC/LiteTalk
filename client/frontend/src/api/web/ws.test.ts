// ws.ts 的 seq 门控（Tracker）与事件总线的单测。
// 语义对齐 Go 侧 client/core/gateway/tracker_test.go 的核心用例：
// 连续段投递、乱序缓冲回填、同 seq 去重、invalid_session 全量补拉、溢出兜底。
import { describe, expect, it, vi } from 'vitest'
import { webEmit, webEventsOn } from './events'
import { Sig, Tracker } from './ws'

// env 构造带 seq 的事件面信封；无 seq（语音面）用 mkPlain。
function env(seq: number, type = 'chat.message'): any {
  return { type, ts: 1, seq, data: { n: seq } }
}

describe('Tracker（#32 事件面 seq 门控）', () => {
  it('连续段按序投递并推进 lastSeq', () => {
    const t = new Tracker()
    expect(t.gate(env(1)).map((e) => e.seq)).toEqual([1])
    expect(t.gate(env(2)).map((e) => e.seq)).toEqual([2])
  })

  it('缺口先缓冲，补齐后一次释放连续段', () => {
    const t = new Tracker()
    expect(t.gate(env(3))).toEqual([]) // 1、2 缺失：缓冲
    expect(t.gate(env(2))).toEqual([])
    const out = t.gate(env(1))
    expect(out.map((e) => e.seq)).toEqual([1, 2, 3])
  })

  it('同 seq 重复投递去重（补发与实时重叠）', () => {
    const t = new Tracker()
    t.gate(env(1))
    expect(t.gate(env(1))).toEqual([])
    expect(t.gate(env(2)).map((e) => e.seq)).toEqual([2])
  })

  it('seq<=0（语音面/连接控制）直通不进门控', () => {
    const t = new Tracker()
    const plain = { type: Sig.RTCIce, ts: 1, data: { candidate: 'c' } }
    expect(t.gate(plain)).toEqual([plain])
    // 乱序窗口内也直通
    expect(t.gate(env(5))).toEqual([])
    expect(t.gate(plain)).toEqual([plain])
  })

  it('invalid_session 触发全量补拉回调并把基准对齐 latest_seq', () => {
    const t = new Tracker()
    const resync = vi.fn()
    t.onResync = resync
    t.gate(env(1)) // 已投递（lastSeq=1）
    t.gate(env(3)) // 2 缺失：在途缓冲
    const inv = { type: Sig.GatewayInvalidSession, ts: 1, data: { reason: 'seq_expired', latest_seq: 10 } }
    const out = t.gate(inv)
    // invalid_session 本身不回投；只回投缓冲中的在途事件（已投递的 1 不重复）
    expect(out.map((e) => e.seq)).toEqual([3])
    expect(resync).toHaveBeenCalledWith('invalid_session')
    // 基准已对齐 10：后续 11 起连续投递
    expect(t.gate(env(11)).map((e) => e.seq)).toEqual([11])
  })

  it('pending 溢出走 awaitBase 兜底：以下一条 seq 定基准', () => {
    const t = new Tracker()
    const resync = vi.fn()
    t.onResync = resync
    t.gate(env(1))
    // 从 3 开始灌满 513 条缺口事件，溢出触发补拉
    for (let i = 3; i <= 515; i++) t.gate(env(i))
    expect(resync).toHaveBeenCalledWith('pending_overflow')
    // awaitBase：下一条见到的 seq 定基准并投递
    const out = t.gate(env(900))
    expect(out.map((e) => e.seq)).toEqual([900])
    expect(t.gate(env(901)).map((e) => e.seq)).toEqual([901])
    expect(t.gate(env(515))).toEqual([]) // 基准之前的旧 seq 丢弃
  })

  it('onServerHello：同会话返回 resume 载荷，会话变更触发补拉', () => {
    const t = new Tracker()
    t.gate(env(1))
    t.gate(env(2))
    // 同会话：lastSeq 带入 resume
    expect(t.onServerHello('sid-1')).toEqual({ sessionID: 'sid-1', lastSeq: 2 })
    // 无 session_id（旧令牌）：不续传
    expect(t.onServerHello('')).toBeNull()
    // 会话被换：基准作废 + 补拉
    const resync = vi.fn()
    t.onResync = resync
    expect(t.onServerHello('sid-2')).toBeNull()
    expect(resync).toHaveBeenCalledWith('session_changed')
    // 新会话 seq 从头计
    expect(t.gate(env(1)).map((e) => e.seq)).toEqual([1])
  })
})

describe('web 事件总线', () => {
  it('按名分发、取消函数只注销自己、监听器异常互不影响', () => {
    const got: unknown[] = []
    const boom = vi.fn(() => {
      throw new Error('listener error')
    })
    const off1 = webEventsOn('x', (d) => got.push(d))
    const off2 = webEventsOn('x', boom)
    webEmit('x', 1)
    // boom 抛异常不影响其他监听器收到事件
    expect(got).toEqual([1])
    expect(boom).toHaveBeenCalledTimes(1)
    off1()
    off2()
    webEmit('x', 2)
    // 取消后两个监听器都不再收到
    expect(got).toEqual([1])
    expect(boom).toHaveBeenCalledTimes(1)
  })
})
