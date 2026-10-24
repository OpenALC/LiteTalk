import { defineStore } from 'pinia'
import { computed, ref } from 'vue'
import { eventsOn, goApp } from '../api/bridge'
import type { Conversation } from '../api/types'
import { useAuthStore } from './auth'
import { useFriendsStore } from './data'

// 私聊/群聊会话状态（#38 B4b）。会话与服务器频道同构：会话 ID 即频道 ID，
// 消息走 chat.send / chat:message，这里只管理会话列表、当前选择与私信模式。
//
// 变更通知走 conversation:changed（只带会话 ID 与 action），收到后按 ID 重新
// 拉取列表；当前会话还在列表中就重拉详情，已不在（被删/自己被移出）则清空
// 当前选择，留在私信列表，不把用户踢回服务器界面。
export const useConversationsStore = defineStore('conversations', () => {
  const list = ref<Conversation[]>([])
  const currentId = ref('')
  const dmMode = ref(false)
  const loading = ref(false)

  // generation 会话代次：resetAll（登出/被踢/换号）递增，在飞请求返回后
  // 代次变了就放弃写回——旧账号的会话不能出现在新账号的列表里。
  let generation = 0
  // loadChain 串行化列表刷新：conversation:changed 可能连续到达，并发对账
  // 会用先前的快照覆盖后到的结果。
  let loadChain: Promise<void> = Promise.resolve()

  const current = computed<Conversation | null>(
    () => list.value.find((c) => c.id === currentId.value) ?? null,
  )

  function selfId(): string {
    return useAuthStore().user?.id ?? ''
  }

  // displayName 列表/头部显示名：群聊优先群名，空名回退成员昵称拼接（排除
  // 自己，私聊显示对方昵称）；direct 的 name 服务端不落库，必须按成员推导。
  function displayName(c: Conversation | null | undefined): string {
    if (!c) return ''
    const sid = selfId()
    if (c.kind === 'group') {
      const name = c.name.trim()
      if (name) return name
      const names = c.members
        .filter((m) => m.user_id !== sid)
        .map((m) => m.nickname)
        .filter(Boolean)
      if (names.length) return names.join('、')
      const all = c.members.map((m) => m.nickname).filter(Boolean)
      return all.length ? all.join('、') : '群聊'
    }
    return c.members.find((m) => m.user_id !== sid)?.nickname || '私聊'
  }

  function memberCount(c: Conversation | null | undefined): number {
    return c?.members.length ?? 0
  }

  // onlineCount 当前会话成员中在线好友数（会话成员无在线字段，按好友列表
  // presence 交叉计数；非好友成员不计入——列表副标题只是粗略提示）。
  function onlineCount(c: Conversation | null | undefined): number {
    if (!c) return 0
    const online = new Set(useFriendsStore().online.map((f) => f.userId))
    return c.members.filter((m) => online.has(m.user_id)).length
  }

  function isOwner(c: Conversation | null | undefined): boolean {
    if (!c) return false
    const sid = selfId()
    return c.members.some((m) => m.user_id === sid && m.role === 'owner')
  }

  function upsert(c: Conversation) {
    const i = list.value.findIndex((x) => x.id === c.id)
    if (i >= 0) list.value.splice(i, 1, c)
    else list.value.unshift(c) // 服务端按 created_at 倒序，新会话在顶部
  }

  function drop(id: string) {
    list.value = list.value.filter((c) => c.id !== id)
    if (currentId.value === id) currentId.value = ''
  }

  async function doLoad(gen: number) {
    if (gen !== generation) return
    loading.value = true
    try {
      const out = await goApp().ListConversations()
      if (gen !== generation) return
      list.value = out
      // 当前会话已被删除/自己已不在成员中：清空选择，留在私信列表。
      if (currentId.value && !out.some((c) => c.id === currentId.value)) {
        currentId.value = ''
      }
    } finally {
      if (gen === generation) loading.value = false
    }
  }

  function load(): Promise<void> {
    const gen = generation
    const run = loadChain.then(() => doLoad(gen))
    loadChain = run.catch(() => {})
    return run
  }

  function enterDM() {
    dmMode.value = true
    load().catch(() => {})
  }

  function exitDM() {
    dmMode.value = false
    currentId.value = ''
  }

  // open 选中会话；列表里没有（例如刚收到其他端的 created 通知）就拉详情。
  async function open(id: string) {
    const gen = generation
    dmMode.value = true
    currentId.value = id
    if (!list.value.some((c) => c.id === id)) {
      try {
        const c = await goApp().GetConversation(id)
        if (gen !== generation) return
        upsert(c)
      } catch (e) {
        if (gen === generation && currentId.value === id) currentId.value = ''
        throw e
      }
    }
  }

  // openDirect 从好友列表发起私聊：已有会话服务端幂等返回原会话。
  async function openDirect(userId: string): Promise<Conversation> {
    const gen = generation
    const c = await goApp().CreateDirectConversation(userId)
    if (gen === generation) {
      upsert(c)
      dmMode.value = true
      currentId.value = c.id
    }
    return c
  }

  async function createGroup(name: string, userIds: string[]): Promise<Conversation> {
    const gen = generation
    const c = await goApp().CreateGroupConversation(name, userIds)
    if (gen === generation) {
      upsert(c)
      dmMode.value = true
      currentId.value = c.id
    }
    return c
  }

  async function rename(id: string, name: string) {
    const gen = generation
    const c = await goApp().RenameConversation(id, name)
    if (gen === generation) upsert(c)
  }

  async function addMembers(id: string, userIds: string[]) {
    const gen = generation
    const c = await goApp().AddConversationMembers(id, userIds)
    if (gen === generation) upsert(c)
  }

  // removeMember 移除成员或退出群聊。自己退出时服务端返回剩余会话（或最后一人
  // 退出返回 null），但本端已不是成员——两种情况都必须从本地列表移除。
  async function removeMember(id: string, userId: string) {
    const gen = generation
    const c = await goApp().RemoveConversationMember(id, userId)
    if (gen !== generation) return
    if (!c || userId === selfId()) drop(id)
    else upsert(c)
  }

  async function leave(id: string) {
    const sid = selfId()
    if (!sid) return
    await removeMember(id, sid)
  }

  async function deleteConversation(id: string) {
    const gen = generation
    await goApp().DeleteConversation(id)
    if (gen === generation) drop(id)
  }

  // applyChanged conversation:changed 事件处理：先刷新列表，当前会话仍存在时
  // 重拉详情（成员/群名变化）；已不在列表则清空当前选择。
  async function applyChanged(conversationId: string, action: string) {
    const gen = generation
    await load()
    if (gen !== generation) return
    if (conversationId !== currentId.value) return
    if (!list.value.some((c) => c.id === conversationId)) {
      currentId.value = ''
      return
    }
    if (action === 'deleted') {
      currentId.value = ''
      return
    }
    try {
      const c = await goApp().GetConversation(conversationId)
      if (gen === generation && currentId.value === conversationId) upsert(c)
    } catch {
      if (gen === generation && currentId.value === conversationId) currentId.value = ''
    }
  }

  // 事件订阅：conversation:changed（#38 B3）与 data:resync（#78）。
  // eventsOn 返回按监听器的取消函数，卸载时逐个注销。
  const offs: Array<() => void> = []

  function bindEvents() {
    if (offs.length) return // 幂等：重复绑定会叠监听
    offs.push(eventsOn('conversation:changed', (d) => {
      const p = d as { conversation_id?: unknown; action?: unknown } | null
      if (typeof p?.conversation_id !== 'string' || p.conversation_id === '') return
      const action = typeof p.action === 'string' ? p.action : ''
      applyChanged(p.conversation_id, action).catch(() => {})
    }))
    offs.push(eventsOn('data:resync', () => {
      load().catch(() => {})
    }))
  }

  function unbindEvents() {
    while (offs.length) offs.pop()!()
  }

  // resetAll 登出/被踢/换号时清空会话与私信模式；订阅不在清理范围
  //（MainView 卸载时另由 unbindEvents 注销）。
  function resetAll() {
    generation++
    loadChain = Promise.resolve()
    list.value = []
    currentId.value = ''
    dmMode.value = false
    loading.value = false
  }

  return {
    list, currentId, dmMode, loading, current,
    displayName, memberCount, onlineCount, isOwner,
    load, enterDM, exitDM, open, openDirect, createGroup,
    rename, addMembers, removeMember, leave, delete: deleteConversation,
    applyChanged, bindEvents, unbindEvents, resetAll,
  }
})
