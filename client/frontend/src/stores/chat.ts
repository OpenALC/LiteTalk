import { defineStore } from 'pinia'
import { ref } from 'vue'
import { eventsOn, goApp } from '../api/bridge'
import type { ChatMessage } from '../api/types'
import { cleanErrorText } from '../utils/errorText'
import { useNotifyStore } from './notify'

// mergeHistory 历史补拉与实时消息的合并（#38 B4b）：按服务端消息 ID 去重，
// 再按毫秒时间戳由旧到新排序（sort 稳定，同毫秒保持历史在前、实时在后的到达序）。
// 历史在前作为基线：断线期间错过的新消息由当前桶补齐。
export function mergeHistory(history: ChatMessage[], live: ChatMessage[]): ChatMessage[] {
  const seen = new Set<string>()
  const out: ChatMessage[] = []
  for (const m of [...history, ...live]) {
    if (m.id) {
      if (seen.has(m.id)) continue
      seen.add(m.id)
    }
    out.push(m)
  }
  return out.sort((a, b) => (Number(a.ts) || 0) - (Number(b.ts) || 0))
}

// chat 文字聊天状态。消息按 channel_id 分桶存储——此前是单一扁平数组，
// 切换频道后旧频道消息会被带到新频道（问题：聊天未隔离）。
export const useChatStore = defineStore('chat', () => {
  // messagesByChannel: channel_id → 消息列表（服务端广播到达即入桶）。
  // 用 ref 包一层保证深层替换触发渲染；每桶天然按到达顺序排列。
  const messagesByChannel = ref<Record<string, ChatMessage[]>>({})

  // generation 会话代次：resetAll（登出/被踢/换号）递增，在飞的补拉响应
  // 返回时若代次已变就丢弃——旧账号的消息不能写进新会话（#85 同款语义）。
  let generation = 0

  // messagesFor 取某频道的消息列表（无记录返回空数组，不创建空桶）。
  // 在模板渲染期读取 messagesByChannel.value，响应式追踪成立。
  function messagesFor(channelId: string): ChatMessage[] {
    return messagesByChannel.value[channelId] ?? []
  }

  // loadHistory 打开私聊/群聊时补拉最近历史（旧→新），与实时消息按 ID 合并。
  // 私聊/群聊的会话 ID 就是频道 ID。失败上抛由调用方静默，不阻断实时消息。
  async function loadHistory(channelId: string) {
    if (!channelId) return
    const gen = generation
    const history = await goApp().ListMessages(channelId, 50)
    if (gen !== generation) return
    const merged = mergeHistory(history, messagesFor(channelId))
    if (!merged.length) return
    // 补拉期间可能有实时消息到达：以合并后的完整列表写回，不覆盖新消息。
    messagesByChannel.value = { ...messagesByChannel.value, [channelId]: merged }
  }

  // send 发送消息。不做本地乐观回显——服务端会把 chat.message 广播回
  // 发送者本人（服务端 hub_test 已钉住该行为），本地再插一条就会双显
  // （问题：发送一条出现两条）。失败抛错由调用方处理。
  async function send(channelId: string, content: string) {
    await goApp().SendChat(channelId, content)
  }

  // 事件订阅。eventsOn 返回按监听器的取消函数，卸载时逐个注销——不按事件名
  // 整体 Off（会清掉别人注册的同名监听）。
  const offs: Array<() => void> = []

  function bindEvents() {
    if (offs.length) return // 幂等：重复绑定会叠监听
    const notify = useNotifyStore()
    offs.push(eventsOn('chat:message', (m) => {
      const msg = m as ChatMessage
      if (!msg.channel_id) return
      const arr = messagesByChannel.value[msg.channel_id] ?? []
      // 按服务端消息 ID 去重（重连补推/重发等场景兜底）
      if (msg.id && arr.some((x) => x.id === msg.id)) return
      messagesByChannel.value = {
        ...messagesByChannel.value,
        [msg.channel_id]: [...arr, msg],
      }
    }))
    // 服务端拒收 chat.send 的异步 error 帧（#34）：app.go 的载荷是 {code, message}，
    // 不含 channel_id。此前无人监听，消息被拒后输入框毫无反馈；message 优先展示
    // 服务端中文原因，缺省用统一文案（码本身不展示，见 #15）。
    offs.push(eventsOn('chat:error', (d) => {
      const p = d as { code?: string; message?: string } | null
      notify.show(cleanErrorText(p?.message, '消息发送失败'), true)
    }))
  }

  // resetAll 登出/被踢/换服时清空全部频道的本地缓存（#85）。当前频道本身
  // 记在 servers store（随其 resetAll 清空）。单频道缓存保留：切频道来回
  // 不产生重复拉取，语义为「本地会话缓存」。幂等；不动事件订阅
  //（MainView 卸载时另由 unbindEvents 注销）。
  function resetAll() {
    generation++
    messagesByChannel.value = {}
  }

  // unbindEvents 逐个注销本 store 注册的监听器。
  function unbindEvents() {
    while (offs.length) offs.pop()!()
  }

  return {
    messagesByChannel, messagesFor, send, loadHistory, bindEvents, unbindEvents, resetAll,
  }
})
