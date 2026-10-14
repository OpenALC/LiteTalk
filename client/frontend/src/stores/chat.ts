import { defineStore } from 'pinia'
import { ref } from 'vue'
import { eventsOff, eventsOn, goApp } from '../api/bridge'
import type { ChatMessage } from '../api/types'

// chat 文字聊天状态。消息按 channel_id 分桶存储——此前是单一扁平数组，
// 切换频道后旧频道消息会被带到新频道（问题：聊天未隔离）。
export const useChatStore = defineStore('chat', () => {
  // messagesByChannel: channel_id → 消息列表（服务端广播到达即入桶）。
  // 用 ref 包一层保证深层替换触发渲染；每桶天然按到达顺序排列。
  const messagesByChannel = ref<Record<string, ChatMessage[]>>({})

  // messagesFor 取某频道的消息列表（无记录返回空数组，不创建空桶）。
  // 在模板渲染期读取 messagesByChannel.value，响应式追踪成立。
  function messagesFor(channelId: string): ChatMessage[] {
    return messagesByChannel.value[channelId] ?? []
  }

  // send 发送消息。不做本地乐观回显——服务端会把 chat.message 广播回
  // 发送者本人（服务端 hub_test 已钉住该行为），本地再插一条就会双显
  // （问题：发送一条出现两条）。失败抛错由调用方处理。
  async function send(channelId: string, content: string) {
    await goApp().SendChat(channelId, content)
  }

  function bindEvents() {
    eventsOn('chat:message', (m) => {
      const msg = m as ChatMessage
      if (!msg.channel_id) return
      const arr = messagesByChannel.value[msg.channel_id] ?? []
      // 按服务端消息 ID 去重（重连补推/重发等场景兜底）
      if (msg.id && arr.some((x) => x.id === msg.id)) return
      messagesByChannel.value = {
        ...messagesByChannel.value,
        [msg.channel_id]: [...arr, msg],
      }
    })
  }

  // clear 清空全部频道的本地缓存（登出时调用）。
  // 单频道缓存保留：切频道来回不产生重复拉取，语义为「本地会话缓存」。
  function clear() {
    messagesByChannel.value = {}
  }

  // unbindEvents 休眠态清理：解除事件订阅。
  function unbindEvents() {
    eventsOff('chat:message')
  }

  return { messagesByChannel, messagesFor, send, bindEvents, unbindEvents, clear }
})
