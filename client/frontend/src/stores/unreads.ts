import { defineStore } from 'pinia'
import { computed, ref } from 'vue'
import { eventsOn, goApp } from '../api/bridge'
import { useConversationsStore } from './conversations'
import { useServersStore } from './data'

// Count 单个频道/服务器的未读与提及计数（缺省为 0）。
export interface UnreadCount {
  unread: number
  mentions: number
}

// 未读与提及计数（#40）。服务端是唯一事实源：
// - load() 拉全量快照，含 0 值项，整体替换本地两个映射（登录/续传补拉时用）；
// - chat:unread 是覆盖语义（按接收者精确计算，多设备同步），绝不能累加；
// - 服务器聚合按当前可见频道求和：会话频道（direct/group）不计入任何服务器。
// generation 会话代次保证 resetAll 后旧响应不写回新账号。
export const useUnreadsStore = defineStore('unreads', () => {
  const channels = ref<Record<string, UnreadCount>>({})
  const servers = ref<Record<string, UnreadCount>>({})

  let generation = 0
  // loadChain 串行化刷新：data:resync 可能在上一次快照仍在飞时到达，
  // 并发对账会让先发出的旧快照覆盖后到的新结果。
  let loadChain: Promise<void> = Promise.resolve()

  const zero = (): UnreadCount => ({ unread: 0, mentions: 0 })

  // channel 读取某频道的计数（缺省零值，不创建条目）。
  function channel(id: string): UnreadCount {
    return channels.value[id] ?? zero()
  }

  // server 读取某服务器的聚合计数（缺省零值）。
  function server(id: string): UnreadCount {
    return servers.value[id] ?? zero()
  }

  // dmTotals 私信入口红点：当前会话列表中全部会话频道的计数之和。
  const dmTotals = computed<UnreadCount>(() => {
    let unread = 0
    let mentions = 0
    for (const c of useConversationsStore().list) {
      const n = channels.value[c.id]
      if (!n) continue
      unread += n.unread
      mentions += n.mentions
    }
    return { unread, mentions }
  })

  // recomputeServer 重算某服务器聚合 = 该服各可见频道计数之和。
  // 会话频道不在任何服务器的 channels 列表里，天然不计入。
  function recomputeServer(serverID: string) {
    const srv = useServersStore().servers.find((s) => s.id === serverID)
    if (!srv) return
    let unread = 0
    let mentions = 0
    for (const ch of srv.channels) {
      const n = channels.value[ch.id]
      if (!n) continue
      unread += n.unread
      mentions += n.mentions
    }
    servers.value = { ...servers.value, [serverID]: { unread, mentions } }
  }

  // setChannel 覆盖单个频道的计数并同步其服务器聚合。找不到所属服务器
  //（私聊/群聊频道或暂未加载的服务器）时只更新频道本身。
  function setChannel(channelID: string, unread: number, mentions: number) {
    if (!channelID) return
    channels.value = { ...channels.value, [channelID]: { unread, mentions } }
    const srv = useServersStore().servers.find((s) => s.channels.some((c) => c.id === channelID))
    if (srv) recomputeServer(srv.id)
  }

  async function doLoad(gen: number) {
    if (gen !== generation) return
    const out = await goApp().ListUnreads()
    if (gen !== generation) return
    const nextChannels: Record<string, UnreadCount> = {}
    for (const c of out.channels ?? []) {
      nextChannels[c.channel_id] = { unread: c.unread_count, mentions: c.mention_count }
    }
    const nextServers: Record<string, UnreadCount> = {}
    for (const s of out.servers ?? []) {
      nextServers[s.server_id] = { unread: s.unread_count, mentions: s.mention_count }
    }
    // 整体替换：快照里的 0 值项也要写入，才能清掉已被读掉/删除频道的旧计数。
    channels.value = nextChannels
    servers.value = nextServers
  }

  function load(): Promise<void> {
    const gen = generation
    const run = loadChain.then(() => doLoad(gen))
    loadChain = run.catch(() => {})
    return run
  }

  // 事件订阅：chat:unread（#40）与 data:resync（#78）。eventsOn 返回按监听器
  // 的取消函数，卸载时逐个注销——不按事件名整体 Off。
  const offs: Array<() => void> = []

  // parseCount 校验事件计数字段：非负整数才接受（防坏包把 NaN/负数写进 UI）。
  function parseCount(v: unknown): number | null {
    return typeof v === 'number' && Number.isInteger(v) && v >= 0 ? v : null
  }

  function bindEvents() {
    if (offs.length) return // 幂等：重复绑定会叠监听
    offs.push(eventsOn('chat:unread', (d) => {
      const p = d as {
        channel_id?: unknown; unread_count?: unknown; mention_count?: unknown
      } | null
      if (typeof p?.channel_id !== 'string' || p.channel_id === '') return
      const unread = parseCount(p.unread_count)
      const mentions = parseCount(p.mention_count)
      if (unread === null || mentions === null) return
      setChannel(p.channel_id, unread, mentions)
    }))
    offs.push(eventsOn('data:resync', () => {
      load().catch(() => {})
    }))
  }

  function unbindEvents() {
    while (offs.length) offs.pop()!()
  }

  // resetAll 登出/被踢/换号时清空计数；订阅不在清理范围（MainView 卸载时
  // 另由 unbindEvents 注销）。generation 递增让在飞快照返回时丢弃。
  function resetAll() {
    generation++
    loadChain = Promise.resolve()
    channels.value = {}
    servers.value = {}
  }

  return {
    channels, servers, dmTotals,
    channel, server, setChannel, load,
    bindEvents, unbindEvents, resetAll,
  }
})

// unreadLabel 徽标 title 文案：「N 条未读，M 条提及」，0 值省略项。
// 组件共用，避免各处拼文案漂移。
export function unreadLabel(n: UnreadCount): string {
  const parts: string[] = []
  if (n.unread > 0) parts.push(`${n.unread} 条未读`)
  if (n.mentions > 0) parts.push(`${n.mentions} 条提及`)
  return parts.join('，')
}

// badgeText 徽标数字：超过 99 显示 99+（窄栏不撑破布局）。
export function badgeText(n: number): string {
  return n > 99 ? '99+' : String(n)
}
