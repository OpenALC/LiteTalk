import { defineStore } from 'pinia'
import { ref } from 'vue'
import { eventsOn, goApp } from '../api/bridge'
import type { ChatMessage, MentionCandidate } from '../api/types'
import { cleanErrorText } from '../utils/errorText'
import { useAuthStore } from './auth'
import { useConversationsStore } from './conversations'
import { useServersStore } from './data'
import { useNotifyStore } from './notify'
import { useUnreadsStore } from './unreads'

// mergeHistory 历史补拉与实时消息的合并（#38 B4b）：按服务端消息 ID 去重，
// 再按毫秒时间戳由旧到新排序（sort 稳定，同毫秒保持历史在前、实时在后的到达序）。
// 历史在前作为基线：断线期间错过的新消息由当前桶补齐。
// #39 的 edited_at/deleted_at/reply_to/reply_preview/mentions 随消息对象整体保留。
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

// isMessageDeleted 墓碑判定（#39）：deleted_at 有值即软删；resync 回灌的软删
// 消息可能只带空正文而没有 deleted_at，同样按墓碑显示——但 #41 的纯附件消息
// 正文也是空串，带附件时不能误判为墓碑。
export function isMessageDeleted(
  m: { content: string; deleted_at?: string; attachments?: ChatMessage['attachments'] },
): boolean {
  if (m.deleted_at) return true
  return m.content === '' && !m.attachments?.length
}

// mentionNotice 被 @ 时的提示文案（#39）：服务器频道显示频道名，会话回退到
// 显示名（私聊为对方昵称、群聊为群名），都解析不到时用通用「会话」。
function mentionNotice(channelId: string): string {
  for (const s of useServersStore().servers) {
    const ch = s.channels.find((c) => c.id === channelId)
    if (ch) return `有人在 #${ch.name} @了你`
  }
  const conversations = useConversationsStore()
  const conv = conversations.list.find((c) => c.id === channelId)
  const name = conv ? conversations.displayName(conv) : ''
  if (name) return `有人在「${name}」@了你`
  return '有人在会话中 @了你'
}

// chat 文字聊天状态。消息按 channel_id 分桶存储——此前是单一扁平数组，
// 切换频道后旧频道消息会被带到新频道（问题：聊天未隔离）。
export const useChatStore = defineStore('chat', () => {
  // messagesByChannel: channel_id → 消息列表（服务端广播到达即入桶）。
  // 用 ref 包一层保证深层替换触发渲染；每桶天然按到达顺序排列。
  const messagesByChannel = ref<Record<string, ChatMessage[]>>({})
  // @提及候选按频道缓存（#39）：同一频道重复输入 @ 不反复请求；失败不缓存，
  // 下次输入再试。候选不随消息变化——会话/频道成员变动后重新进入会重建。
  const mentionCandidatesByChannel = ref<Record<string, MentionCandidate[]>>({})

  // generation 会话代次：resetAll（登出/被踢/换号）递增，在飞的补拉响应
  // 返回时若代次已变就丢弃——旧账号的消息不能写进新会话（#85 同款语义）。
  let generation = 0

  // mentionInflight 同频道的并发候选请求合并成一个：输入 @ 后的每个按键都会
  // 触发一次加载检查，没有它就会打出一串重复请求。
  const mentionInflight = new Map<string, Promise<MentionCandidate[]>>()

  // historyInflight 合并同频道的并发补拉：ChatPanel 与 ConversationChat 同时
  // 打开同一频道时只发一次。不做「已加载」永久短路——gateway.invalid_session
  // 的补拉只覆盖 to seenChans（本会话收过实时消息的频道），只经 loadHistory
  // 打开过的频道若在断线期间有新消息，再次打开必须重新拉取才能补齐。
  const historyInflight = new Map<string, Promise<void>>()

  // lastReportedByChannel 已上报已读水位的最后一条消息 ID（#40）：同一轮不
  // 重复 PUT；服务端水位只前进，重复上报只是白耗请求。
  const lastReportedByChannel = new Map<string, string>()

  // messagesFor 取某频道的消息列表（无记录返回空数组，不创建空桶）。
  // 在模板渲染期读取 messagesByChannel.value，响应式追踪成立。
  function messagesFor(channelId: string): ChatMessage[] {
    return messagesByChannel.value[channelId] ?? []
  }

  // messageById 在当前频道桶内查一条消息（回复跳转、编辑态回填用）。
  function messageById(channelId: string, id: string): ChatMessage | undefined {
    return messagesFor(channelId).find((m) => m.id === id)
  }

  // mergeMessage 单条消息写入桶：按 ID 去重（新副本覆盖旧副本，纳入编辑/软删
  // 等后续状态），再按 ts 旧→新排序。回复跳转经 GetMessage 回填时用。
  function mergeMessage(channelId: string, msg: ChatMessage) {
    const next = messagesFor(channelId).filter((m) => m.id !== msg.id)
    next.push(msg)
    next.sort((a, b) => (Number(a.ts) || 0) - (Number(b.ts) || 0))
    messagesByChannel.value = { ...messagesByChannel.value, [channelId]: next }
  }

  // loadHistory 打开频道/会话时补拉最近历史（旧→新），与实时消息按 ID 合并。
  // 私聊/群聊的会话 ID 就是频道 ID。每次调用都重新拉取（断线窗口可能已有
  // 新消息），同频道在飞时复用同一个 Promise，避免 ChatPanel 与
  // ConversationChat 同时打开时重复请求。失败上抛由调用方静默，不阻断实时消息。
  function loadHistory(channelId: string): Promise<void> {
    if (!channelId) return Promise.resolve()
    const inflight = historyInflight.get(channelId)
    if (inflight) return inflight
    const gen = generation
    // then 链而非 async 包装：finally 必须晚于 historyInflight.set 执行，
    // 否则同步完成的请求会在登记前就把自己从表里删掉。
    let p: Promise<void>
    p = goApp().ListMessages(channelId, 50)
      .then((history) => {
        if (gen !== generation) return
        const merged = mergeHistory(history, messagesFor(channelId))
        if (!merged.length) return
        // 补拉期间可能有实时消息到达：以合并后的完整列表写回，不覆盖新消息。
        messagesByChannel.value = { ...messagesByChannel.value, [channelId]: merged }
      })
      .finally(() => {
        // 只清理自己这一笔：resetAll 后可能有新一代请求占用了同一个键。
        if (historyInflight.get(channelId) === p) historyInflight.delete(channelId)
      })
    historyInflight.set(channelId, p)
    return p
  }

  // markRead 上报频道已读到最新一条消息（#40）。取桶内排序后的最后一项；
  // 无消息或与上次已上报 ID 相同则跳过。成功用返回计数覆盖 unreads store；
  // 失败静默（切频道时自动上报，弹 toast 会刷屏；水位未推进，下条消息再试）。
  async function markRead(channelId: string): Promise<void> {
    const arr = messagesFor(channelId)
    const last = arr.length ? arr[arr.length - 1] : undefined
    if (!last?.id) return
    if (lastReportedByChannel.get(channelId) === last.id) return
    const gen = generation
    try {
      const res = await goApp().MarkChannelRead(channelId, last.id)
      if (gen !== generation) return
      lastReportedByChannel.set(channelId, last.id)
      useUnreadsStore().setChannel(channelId, res.unread_count, res.mention_count)
    } catch {
      // 静默：下次消息到达或重新进入频道时再报。
    }
  }

  // jumpTarget 跨频道消息定位请求（#42 B2）：搜索面板选中结果后由 MainView 写入，
  // 目标频道的 ChatPanel 消费（滚动 + 高亮）后 clearJump。seq 自增让重复点击同一
  // 条消息也产生新对象，watch 不会因引用相同而跳过。
  const jumpTarget = ref<{ channelId: string; messageId: string; seq: number } | null>(null)
  let jumpSeq = 0

  function requestJump(channelId: string, messageId: string) {
    if (!channelId || !messageId) return
    jumpSeq++
    jumpTarget.value = { channelId, messageId, seq: jumpSeq }
  }

  function clearJump() {
    jumpTarget.value = null
  }

  // send 发送普通消息（无回复、无提及）。不做本地乐观回显——服务端会把
  // chat.message 广播回发送者本人（服务端 hub_test 已钉住该行为），本地再插
  // 一条就会双显（问题：发送一条出现两条）。失败抛错由调用方处理。
  async function send(channelId: string, content: string) {
    await sendWithMeta(channelId, content)
  }

  // sendWithMeta 发送带回复与 @提及的消息（#39）：replyTo 空串 = 普通消息，
  // mentions 空数组 = 无提及。委托 sendWithAttachments（无附件）。
  async function sendWithMeta(channelId: string, content: string, replyTo = '', mentions: string[] = []) {
    await sendWithAttachments(channelId, content, replyTo, mentions, [])
  }

  // sendWithAttachments 发送带附件的消息（#41）：content 可为空串（纯附件消息），
  // attachmentIds 为已上传附件的 ID 列表。服务端同样广播回发送者，不做本地插入。
  async function sendWithAttachments(
    channelId: string, content: string, replyTo = '', mentions: string[] = [], attachmentIds: string[] = [],
  ) {
    await goApp().SendChatWithAttachments(channelId, content, replyTo, mentions, attachmentIds)
  }

  // editMessage 编辑消息（仅作者，#39）。mentions 整体替换：调用方传原消息的
  // 提及集合保持提及不变。返回更新后的消息并就地替换桶内旧副本；事件随后到达
  // 时内容一致，重复应用无副作用。
  async function editMessage(channelId: string, messageId: string, content: string, mentions: string[] = []) {
    const updated = await goApp().EditMessage(channelId, messageId, content, mentions)
    mergeMessage(channelId, updated)
    return updated
  }

  // applyDeleted 本地墓碑：保留行供回复引用，清空正文并置删除时间。
  // chat:deleted 事件不带时间戳，本地删除成功时先落一个时间，事件到达后保留原值。
  function applyDeleted(channelId: string, messageId: string) {
    const arr = messagesByChannel.value[channelId]
    if (!arr) return
    const i = arr.findIndex((m) => m.id === messageId)
    if (i < 0) return
    const next = arr.slice()
    next[i] = {
      ...arr[i],
      content: '',
      deleted_at: arr[i].deleted_at || String(Date.now()),
    }
    messagesByChannel.value = { ...messagesByChannel.value, [channelId]: next }
  }

  // deleteMessage 软删除（仅作者；管理入口不在前端暴露）。成功后本地先显示
  // 墓碑，chat:deleted 广播到达时再做一次幂等更新。
  async function deleteMessage(channelId: string, messageId: string) {
    await goApp().DeleteMessage(channelId, messageId)
    applyDeleted(channelId, messageId)
  }

  // applyEdited chat:edited 载荷落地：只改正文字段，不重建成员表
  //（昵称等发送时快照保持不变）。找不到消息就忽略——历史未加载的消息
  // 由补拉获取最新状态。
  function applyEdited(channelId: string, messageId: string, content: string, editedAt: string, mentions?: string[]) {
    const arr = messagesByChannel.value[channelId]
    if (!arr) return
    const i = arr.findIndex((m) => m.id === messageId)
    if (i < 0) return
    const next = arr.slice()
    next[i] = {
      ...arr[i],
      content,
      edited_at: editedAt || next[i].edited_at || String(Date.now()),
      mentions: mentions ?? next[i].mentions ?? [],
    }
    messagesByChannel.value = { ...messagesByChannel.value, [channelId]: next }
  }

  // mentionCandidatesFor 读取已缓存的候选（未加载/失败返回空数组，不触发请求）。
  function mentionCandidatesFor(channelId: string): MentionCandidate[] {
    return mentionCandidatesByChannel.value[channelId] ?? []
  }

  // loadMentionCandidates 拉取并缓存某频道的 @提及候选（#39）。失败静默返回
  // 空数组且不缓存——下一次输入 @ 会重试；调用方不因候选不可用阻塞输入。
  function loadMentionCandidates(channelId: string): Promise<MentionCandidate[]> {
    if (!channelId) return Promise.resolve([])
    const cached = mentionCandidatesByChannel.value[channelId]
    if (cached) return Promise.resolve(cached)
    const inflight = mentionInflight.get(channelId)
    if (inflight) return inflight
    const gen = generation
    // 用 then 链而不是 async 包装：finally 必须晚于 inflight.set 执行，
    // 否则同步完成的请求会在登记前就把自己从表里删掉/留下悬空引用。
    let p: Promise<MentionCandidate[]>
    p = goApp().ListMentionCandidates(channelId)
      .then((list) => {
        if (gen !== generation) return []
        const next = list ?? []
        mentionCandidatesByChannel.value = {
          ...mentionCandidatesByChannel.value,
          [channelId]: next,
        }
        return next
      })
      .catch(() => [])
      .finally(() => {
        // 只清理自己这一笔：resetAll 后可能有新一代请求占用了同一个键。
        if (mentionInflight.get(channelId) === p) mentionInflight.delete(channelId)
      })
    mentionInflight.set(channelId, p)
    return p
  }

  // notifyMention 收到新消息时判断是否 @ 了自己（#39）。发送者是自己不提示；
  // 同一消息的重推已在入桶前按 ID 去重，不会重复提醒。
  function notifyMention(msg: ChatMessage) {
    const mentions = msg.mentions ?? []
    if (!mentions.length) return
    const me = useAuthStore().user?.id
    if (!me || msg.user_id === me) return
    if (!mentions.includes(me)) return
    useNotifyStore().show(mentionNotice(msg.channel_id))
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
      notifyMention(msg)
    }))
    // 消息编辑/软删广播（#39）：只更新当前桶内已有消息，历史未加载的忽略。
    offs.push(eventsOn('chat:edited', (d) => {
      const p = d as {
        channel_id?: unknown; message_id?: unknown; content?: unknown
        edited_at?: unknown; mentions?: unknown
      } | null
      if (typeof p?.channel_id !== 'string' || p.channel_id === '') return
      if (typeof p.message_id !== 'string' || p.message_id === '') return
      const content = typeof p.content === 'string' ? p.content : ''
      const editedAt = typeof p.edited_at === 'string' ? p.edited_at : ''
      const mentions = Array.isArray(p.mentions)
        ? p.mentions.filter((x): x is string => typeof x === 'string')
        : undefined
      applyEdited(p.channel_id, p.message_id, content, editedAt, mentions)
    }))
    offs.push(eventsOn('chat:deleted', (d) => {
      const p = d as { channel_id?: unknown; message_id?: unknown } | null
      if (typeof p?.channel_id !== 'string' || p.channel_id === '') return
      if (typeof p.message_id !== 'string' || p.message_id === '') return
      applyDeleted(p.channel_id, p.message_id)
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
    mentionCandidatesByChannel.value = {}
    mentionInflight.clear()
    historyInflight.clear()
    lastReportedByChannel.clear()
    clearJump()
  }

  // unbindEvents 逐个注销本 store 注册的监听器。
  function unbindEvents() {
    while (offs.length) offs.pop()!()
  }

  return {
    messagesByChannel, mentionCandidatesByChannel,
    messagesFor, messageById, mergeMessage, send, sendWithMeta, sendWithAttachments,
    editMessage, deleteMessage, loadHistory, markRead,
    mentionCandidatesFor, loadMentionCandidates,
    jumpTarget, requestJump, clearJump,
    bindEvents, unbindEvents, resetAll,
  }
})
