import { defineStore } from 'pinia'
import { computed, ref } from 'vue'
import { eventsOn, goApp } from '../api/bridge'
import type { ServerChannels, ServerDTO } from '../api/types'
import { useNotifyStore } from './notify'

// 频道/服务器/好友数据域。
// 服务器/频道的管理操作已接真实 REST（批次①）；频道列表经
// GET /servers/{id}/channels 在登录后逐服恢复（#12）——重启不再丢列表。
// 新建频道可显式指定分类（#72）：category_id 由服务端落库，本地 group 取分类名，
// 与 #16 的按名归组同一套显示语义；不指定时仍按类型默认组名过渡。

export const UNCATEGORIZED = '未分类'

export interface Channel {
  id: string
  name: string
  type: 'text' | 'voice'
  locked?: boolean
  members?: number
  group: string
  // #37 B2 频道选项：父频道（空 = 顶层）、是否设密码、语音人数上限（0 = 不限）。
  parentId?: string
  hasPassword?: boolean
  userLimit?: number
}

export interface Server {
  id: string
  name: string
  icon: string // Lucide 图标名（禁头像/上传图——硬性约束）
  channels: Channel[]
  // builtin 仅为类型兼容保留：内置示例服务器已移除，不再产生实例；
  // ServerRail 仍有针对它的守卫。
  builtin?: boolean
  // unavailable 频道列表加载失败（多为被踢出/成员数据异常的 403）：频道区据此
  // 给出解释而不是恒空（#12）。
  unavailable?: boolean
}

export interface Friend {
  id: string // friendship id（备注/分组/删除 REST 的目标；#31 字符串 wire）
  userId: string // 对端 user.id（presence:update 按它匹配，#11）
  name: string // 显示名 = remark || nickname（服务端 remark 仅发起方向可写，对方视角恒空回落昵称——预期行为）
  nickname: string // 原始昵称（添加好友/请求静音等场景用）
  remark: string // 备注原文（空 = 未设置；编辑弹层初值用）
  // 在线态收敛为两态（#11，真实数据源 presence）：idle/busy 无数据源，不造状态。
  status: 'online' | 'offline'
  sub: string // 状态行文案（在线/离线）
  inChannel?: boolean // 服务端暂无「好友所在频道」数据，恒 false（按钮隐藏）
  group: string // 好友分组（显示值，空归「好友」默认组；提交时映射回空串）
  incoming: boolean // 对方发起=我接受的行。服务端 PATCH 仅发起方向可写——
  // incoming=true 时备注/分组项必须禁用（review B1：否则 100% 弹 403）。
}

// isRealID 判断是否为服务端实体 ID（纯数字）；groupsOf 合成的 chan: 分组不是实体。
export function isRealID(id: string): boolean {
  return /^\d+$/.test(id)
}

// ChannelNode 频道树节点（#37 B2）：频道 + 其直接子频道（按输入顺序）。
export interface ChannelNode {
  ch: Channel
  children: Channel[]
}

// channelTree 把一个分类内的频道排成两级树（#37 B2 纯函数）：
// 根 = 无父 / 父不在本组（孤儿降级）/ 父自身带父（超两级的防御，不丢节点）；
// 其余挂到父下。保持输入顺序（服务端 position,id），不按 Snowflake 数字排序。
export function channelTree(channels: Channel[]): ChannelNode[] {
  const raw = new Map(channels.map((c) => [c.id, c]))
  const parentOf = (ch: Channel): string | undefined =>
    ch.parentId && ch.parentId !== ch.id ? ch.parentId : undefined
  const isRoot = (ch: Channel): boolean => {
    const p = parentOf(ch)
    if (!p || !raw.has(p)) return true // 无父 / 孤儿
    const parent = raw.get(p)!
    return !!parentOf(parent) // 父自身带父 → 本节点升根，不做成孙级
  }
  const roots: ChannelNode[] = []
  const byId = new Map<string, ChannelNode>()
  for (const ch of channels) {
    if (isRoot(ch)) {
      const node = { ch, children: [] as Channel[] }
      roots.push(node)
      byId.set(ch.id, node)
    }
  }
  for (const ch of channels) {
    const p = parentOf(ch)
    if (!isRoot(ch) && p) {
      byId.get(p)?.children.push(ch)
    }
  }
  return roots
}

// catDropIndex 把拖拽落点换算成 reorderCategory 的插入序（#16）。落点语义是
// 「插到目标分类之前」（.drop-target 顶边线），而 reorderCategory 的 position
// 是删掉拖拽项之后的插入序——向下拖（源在目标前）时删前下标比删后大 1，直接用
// 目标下标会落到目标之后，与高亮提示相反。targetIndex 负数/越界表示落点在
// 非分类条目上（未分类/内置自带组），取分类区末尾。
export function catDropIndex(list: { id: string }[], dragId: string, targetIndex: number): number {
  if (targetIndex < 0 || targetIndex >= list.length) return list.length
  const from = list.findIndex((x) => x.id === dragId)
  return from >= 0 && from < targetIndex ? targetIndex - 1 : targetIndex
}

// INTRO_ID 介绍页伪服务器 ID：不是服务端实体，无频道栏（v0.1.2 首页体系）。
export const INTRO_ID = '__intro__'

// iconFromServer 服务端 icon_name（kebab-case 预设名）→ 前端 Lucide 注册键。
// 与 server/internal/api/servers.go 的 allowedIcons 保持一致。
function iconFromServer(name: string): string {
  const map: Record<string, string> = {
    'messages-square': 'MessagesSquare',
    'gamepad-2': 'Gamepad2',
    music: 'Music',
    palette: 'Palette',
    'book-open': 'BookOpen',
    coffee: 'Coffee',
    mic: 'Mic',
    'graduation-cap': 'GraduationCap',
    heart: 'Heart',
    rocket: 'Rocket',
    camera: 'Camera',
    dumbbell: 'Dumbbell',
  }
  return map[name] ?? 'MessagesSquare'
}

export const useServersStore = defineStore('servers', () => {
  // 登录前为空列表；登录后由 loadRemoteServers 从服务端填充。
  const servers = ref<Server[]>([])
  const currentServerId = ref('')
  const currentChannelId = ref('')

  // ---- 首页 / 介绍页（客户端级偏好，持久化 config.json；v0.1.2） ----
  // homeId：进入应用默认选中的服务器（'' / INTRO_ID 表示介绍页为首页）。
  // introHidden：介绍页被用户从服务器栏删除（可恢复）。
  const homeId = ref(INTRO_ID)
  const introHidden = ref(false)

  async function initHome() {
    try {
      const h = await goApp().GetHomeServer()
      homeId.value = h || INTRO_ID
      introHidden.value = await goApp().GetIntroHidden()
    } catch {
      // 读取失败保持默认：介绍页为首页、未隐藏。
    }
  }

  async function setHome(id: string) {
    await goApp().SetHomeServer(id === INTRO_ID ? '' : id)
    homeId.value = id
  }

  async function setIntroHidden(v: boolean) {
    await goApp().SetIntroHidden(v)
    introHidden.value = v
  }

  // 列表为空时 currentServer 为 undefined（登录前/无服务器）：消费方必须先
  // 确认存在当前服务器（MainView 用 isIntro 兜底），不能无条件取字段。
  const currentServer = computed(() =>
    servers.value.find((s) => s.id === currentServerId.value) ?? servers.value[0])
  const currentChannel = computed(() =>
    currentServer.value.channels.find((c) => c.id === currentChannelId.value))

  function selectServer(id: string) {
    currentServerId.value = id
    if (id === INTRO_ID) {
      currentChannelId.value = '' // 介绍页无频道
      return
    }
    const first = servers.value.find((s) => s.id === id)?.channels[0]
    currentChannelId.value = first?.id ?? ''
  }

  function selectChannel(id: string) {
    currentChannelId.value = id
  }

  function findServer(id: string) {
    return servers.value.find((s) => s.id === id)
  }

  // ---- 真实 REST（批次①接线） ----

  // loadRemoteServers 登录后拉取真实服务器并恢复各自频道列表。
  // 逐服调 ListChannels；单服失败标记 unavailable 并提示（#12，不重试），不阻断其它服。
  // ListServers 失败不再静默（#12）：toast 一次后保留当前界面，其余操作照常；
  // 与 loadChannels 一致在内部消化失败，调用方无需再 catch。
  // 快照语义（#78）：续传失败后也会再调一次，已存在的服务器更新名称/图标，
  // 服务端已不存在的真实服务器（被移出/解散）从本地剔除——否则视图永远停在
  // 断线前的陈旧列表。
  // 串行化：data:resync 可能在上一次加载仍在飞时到达，并发对账会用过期的
  // ids 集合误删另一次刚拉到的服务器。
  let loadChain: Promise<void> = Promise.resolve()

  // generation 会话代次：resetAll（登出/被踢/换服）递增，在飞请求的每个 await
  // 之后比对，代次变了就放弃写回——否则旧账号的响应返回时会把刚清空的列表
  // 重新填上，B 登录后仍看到 A 的服务器（#85）。
  let generation = 0

  function loadRemoteServers(): Promise<void> {
    // 代次在入队时捕获：resetAll 之前排队的请求即使延后执行，也属于旧会话。
    const gen = generation
    const run = loadChain.then(() => doLoadRemoteServers(gen))
    loadChain = run.catch(() => {})
    return run
  }

  async function doLoadRemoteServers(gen: number) {
    if (gen !== generation) return
    let list: ServerDTO[]
    try {
      list = await goApp().ListServers()
    } catch {
      // 会话已复位：旧账号的失败提示也不该出现在新会话里。
      if (gen !== generation) return
      useNotifyStore().show('服务器列表加载失败', true)
      return
    }
    if (gen !== generation) return
    const ids = new Set<string>()
    for (const s of list) {
      const id = String(s.id)
      ids.add(id)
      const existing = servers.value.find((x) => x.id === id)
      if (existing) {
        existing.name = s.name
        existing.icon = iconFromServer(s.icon_name)
      } else {
        servers.value.push({ id, name: s.name, icon: iconFromServer(s.icon_name), channels: [] })
      }
      await loadChannels(gen, id)
      if (gen !== generation) return
    }
    servers.value = servers.value.filter((x) => ids.has(x.id))
    // 当前服务器已被移出/解散：回退到第一个可用服务器（介绍页不是服务器实体，
    // 不能因对账把它顶掉）。
    if (currentServerId.value && currentServerId.value !== INTRO_ID
        && !servers.value.some((x) => x.id === currentServerId.value)) {
      selectServer(servers.value[0]?.id ?? '')
    }
  }

  // loadChannels 拉服务器的分类与频道：channel.group 取分类名（无分类归「未分类」），
  // 空分类也进 extraGroups（空分类重启后不丢）。失败不再静默（#12）：标记
  // unavailable 并 toast——被踢出（403）与网络错误都给解释，频道区不能恒空。
  async function loadChannels(gen: number, serverId: string) {
    const s = findServer(serverId)
    if (!s) return
    let out: ServerChannels
    try {
      out = await goApp().ListChannels(serverId)
    } catch (e) {
      // 会话已复位：旧账号的失败提示也不该出现在新会话里。
      if (gen !== generation) return
      s.unavailable = true
      // 403 判定走服务端错误码前缀（bridge 只传 message 字符串，与 RegisterView 同法）。
      const msg = String((e as Error)?.message ?? e)
      const notify = useNotifyStore()
      if (msg.includes('forbidden')) {
        notify.show(`「${s.name}」不可访问，可能已退出该服务器`, true)
      } else {
        notify.show(`「${s.name}」频道加载失败`, true)
      }
      return
    }
    if (gen !== generation) return
    s.unavailable = false
    const nameOf = new Map<string, string>()
    for (const c of out.categories ?? []) nameOf.set(c.id, c.name)
    s.channels = (out.channels ?? []).map((c) => ({
      id: String(c.id),
      name: c.name,
      type: c.type,
      group: (c.category_id ? nameOf.get(c.category_id) : undefined) || UNCATEGORIZED,
      // #37 B2：新字段映射，顺序沿用服务端（position,id）不重排。
      parentId: c.parent_id == null || c.parent_id === '' ? undefined : String(c.parent_id),
      hasPassword: !!c.has_password,
      userLimit: typeof c.user_limit === 'number' ? c.user_limit : 0,
    }))
    extraGroups.value = {
      ...extraGroups.value,
      [serverId]: (out.categories ?? [])
        .map((c) => ({ id: String(c.id), name: c.name, position: c.position ?? 0 }))
        .sort((a, b) => a.position - b.position),
    }
    if (currentChannelId.value && !s.channels.some((c) => c.id === currentChannelId.value)) {
      currentChannelId.value = s.channels[0]?.id ?? ''
    }
  }

  // createServer 创建服务器（REST），成功即选中并拉取分类/频道。
  // 拉取失败由 loadChannels 内部提示，不阻断创建结果。
  async function createServer(name: string) {
    const gen = generation
    const s = await goApp().CreateServer(name)
    if (gen !== generation) return
    const id = String(s.id)
    servers.value.push({ id, name: s.name, icon: iconFromServer(s.icon_name), channels: [] })
    selectServer(id)
    await loadChannels(gen, id)
    // 服务端带默认频道时补选第一个；用户已切走则不抢选中。
    if (gen === generation && currentServerId.value === id && !currentChannelId.value) {
      selectServer(id)
    }
  }

  // joinServer 凭邀请码加入（REST），成功即选中并拉取分类/频道；已是成员时
  // 服务端原样返回，本地去重后同样拉取（此前只在登录时拉，好友加入后永远
  // 看不到频道/分类，#91）。
  async function joinServer(code: string) {
    const gen = generation
    const s = await goApp().JoinServerByCode(code)
    if (gen !== generation) return
    const id = String(s.id)
    if (!servers.value.some((x) => x.id === id)) {
      servers.value.push({ id, name: s.name, icon: iconFromServer(s.icon_name), channels: [] })
    }
    selectServer(id)
    await loadChannels(gen, id)
    // 拉取频道前 selectServer 只能选中空频道：回来后补选第一个。
    if (gen === generation && currentServerId.value === id && !currentChannelId.value) {
      selectServer(id)
    }
  }

  // renameServer 重命名（REST）。
  async function renameServer(id: string, name: string) {
    const s = findServer(id)
    if (!s) return
    await goApp().RenameServer(id, name)
    s.name = name
  }

  // removeServer 删除服务器（REST，仅 owner）；删除当前所在服务器时回退到第一个。
  // S8：删掉的正是首页服务器时 homeId 回落介绍页，避免下次启动选中幽灵 id。
  async function removeServer(id: string) {
    const s = findServer(id)
    if (!s) return
    await goApp().DeleteServer(id)
    servers.value = servers.value.filter((x) => x.id !== id)
    if (homeId.value === id) {
      // 回落介绍页；若介绍页此前已被隐藏（S6：幽灵首页），改落第一个真实服务器。
      if (introHidden.value) {
        const first = servers.value.find((x) => x.id !== INTRO_ID)
        homeId.value = first?.id ?? INTRO_ID
        goApp().SetHomeServer(homeId.value === INTRO_ID ? '' : homeId.value).catch(() => {})
      } else {
        homeId.value = INTRO_ID
        goApp().SetHomeServer('').catch(() => {})
      }
    }
    if (currentServerId.value === id) {
      selectServer(servers.value[0]?.id ?? '')
    }
  }

  // createChannel 新建频道（REST）。categoryId 显式指定归属：分类 id，'' = 未分类；
  // 不传 = 不指定，留给调用方走旧路径。显式指定优先——group 取该分类名，groupsOf
  // 的按名归组立刻把它挂对（#72），不用等重启后 ListChannels 对账；缺省才退回
  // 类型默认组名（#16 按名归组的过渡语义，两种入口不要打架）。
  async function createChannel(serverId: string, name: string, type: 'text' | 'voice', categoryId?: string) {
    const s = findServer(serverId)
    if (!s) return
    const catId = categoryId ?? ''
    const c = await goApp().CreateChannel(serverId, name, type, catId)
    const cat = categoryId ? (extraGroups.value[serverId] ?? []).find((g) => g.id === categoryId) : undefined
    s.channels.push({
      id: String(c.id), name: c.name, type,
      // 显式指定但找不到分类名：归「未分类」等下次加载修正，别塞类型默认组造假分组。
      group: categoryId === undefined
        ? (type === 'voice' ? '语音频道' : '文字频道')
        : (cat?.name ?? UNCATEGORIZED),
    })
  }

  // renameChannel 重命名频道（REST）。
  async function renameChannel(serverId: string, chId: string, name: string) {
    const ch = findServer(serverId)?.channels.find((c) => c.id === chId)
    if (!ch) return
    await goApp().RenameChannel(chId, name)
    ch.name = name
  }

  // createSubchannel 建子频道（#37 B2）：类型继承父、分类由服务端随父；
  // 成功后按返回 DTO 回填 parentId/hasPassword/userLimit。
  async function createSubchannel(serverId: string, name: string, parentId: string) {
    const s = findServer(serverId)
    if (!s) return
    const parent = s.channels.find((c) => c.id === parentId)
    const typ = parent?.type ?? 'voice'
    const c = await goApp().CreateSubchannel(serverId, name, typ, parentId)
    s.channels.push({
      id: String(c.id), name: c.name, type: typ,
      group: parent?.group ?? UNCATEGORIZED,
      parentId: String(c.parent_id ?? parentId),
      hasPassword: !!c.has_password,
      userLimit: c.user_limit ?? 0,
    })
  }

  // updateChannelOptions 更新频道选项（#37 B2）：只发变更字段（bridge 侧同约定）；
  // 成功后用返回 DTO 回填 parentId/hasPassword/userLimit。
  async function updateChannelOptions(serverId: string, chId: string, parentId: string | null, userLimit: number, password: string, clearPassword: boolean) {
    const s = findServer(serverId)
    const ch = s?.channels.find((c) => c.id === chId)
    if (!ch) return
    // parentId null = 不改；'0' = 提升顶层；其余 = 设置父。
    const p = parentId === null ? '' : parentId
    const c = await goApp().UpdateChannelOptions(chId, p, userLimit, password, clearPassword)
    if (c.parent_id == null || c.parent_id === '') ch.parentId = undefined
    else ch.parentId = String(c.parent_id)
    ch.hasPassword = !!c.has_password
    ch.userLimit = typeof c.user_limit === 'number' ? c.user_limit : 0
    // 挂父/换父后分类不变（服务端限父子同分类），group 无需重算。
  }

  // deleteChannel 删除频道（REST）。调用方负责先离开语音频道。
  async function deleteChannel(serverId: string, chId: string) {
    const s = findServer(serverId)
    if (!s) return
    await goApp().DeleteChannel(chId)
    s.channels = s.channels.filter((c) => c.id !== chId)
    if (currentChannelId.value === chId) {
      currentChannelId.value = s.channels[0]?.id ?? ''
    }
  }

  // ---- 频道分类（v0.1.2：服务端 categories 实体） ----
  // extraGroups：服务器 ID → 分类列表（含空分类）。
  // position 为显示序（真实分类 = 服务端 position，重排后 0..n-1）。
  const extraGroups = ref<Record<string, { id: string; name: string; position: number }[]>>({})

  // resetAll 恢复登录前状态（#85）：主动登出、被踢、跨域换服与登录新账号前
  // 都由 auth store 调用。服务器列表清空（登录前为空），选中的服务器/频道与
  // 分类缓存全清；事件订阅不在清理范围——data:resync 监听在 MainView 卸载时
  // 另由 unbindEvents 按监听器注销，登录后重新 bind。幂等。
  // homeId / introHidden 属于 config.json 里的客户端级偏好，不是账号会话态，
  // 保留；若 homeId 指向新账号不可见的服务器，下一次 loadRemoteServers 会经
  // 「当前服务器不在列表」分支回退到可用项。
  function resetAll() {
    generation++
    loadChain = Promise.resolve()
    servers.value = []
    currentServerId.value = ''
    currentChannelId.value = ''
    extraGroups.value = {}
  }

  // groupsOf 某服务器的全部分类。Channel.group 是分类名——按名归组（同名分类会
  // 合并显示，过渡方案）。排序：显式分类按 position；无分类条目的分组里「未分类」
  // 沉底，其余（按名归组的过渡分组）按频道首次出现序排在显式分类之前。sort 稳定。
  function groupsOf(serverId: string, channels: Channel[]): { id: string; name: string }[] {
    const cats = extraGroups.value[serverId] ?? []
    const byName = new Map(cats.map((g) => [g.name, g]))
    const out: { id: string; name: string; key: number }[] = []
    const seen = new Set<string>()
    const put = (id: string, name: string, key: number) => {
      if (seen.has(name)) return
      seen.add(name)
      out.push({ id, name, key })
    }
    let rank = 0
    for (const c of channels) {
      const g = byName.get(c.group)
      if (g) {
        put(g.id, g.name, g.position)
      } else if (c.group === UNCATEGORIZED) {
        put(`chan:${c.group}`, c.group, Number.MAX_SAFE_INTEGER)
      } else {
        put(`chan:${c.group}`, c.group, -1000 + rank++)
      }
    }
    for (const g of cats) put(g.id, g.name, g.position)
    out.sort((a, b) => a.key - b.key)
    return out.map(({ id, name }) => ({ id, name }))
  }

  // createCategory 新建分类（REST）。
  async function createCategory(serverId: string, name: string) {
    const s = findServer(serverId)
    if (!s) return
    const c = await goApp().CreateCategory(serverId, name)
    const list = extraGroups.value[serverId] ?? []
    extraGroups.value = {
      ...extraGroups.value,
      [serverId]: [...list, { id: String(c.id), name: c.name, position: c.position ?? list.length }],
    }
  }

  // deleteCategory 删除分类及其下频道（二次确认由 UI 负责）：
  // 真实分类走 REST 级联删除；合成组（chan:）逐频道删除。
  async function deleteCategory(serverId: string, catId: string, catName: string) {
    const s = findServer(serverId)
    if (!s) return
    if (isRealID(catId)) {
      await goApp().DeleteCategory(catId, true) // with_channels=1 级联
      s.channels = s.channels.filter((c) => c.group !== catName)
    } else {
      for (const ch of [...s.channels.filter((c) => c.group === catName)]) {
        await deleteChannel(serverId, ch.id)
      }
    }
    const list = (extraGroups.value[serverId] ?? []).filter((g) => g.name !== catName)
    extraGroups.value = { ...extraGroups.value, [serverId]: list }
    if (currentChannelId.value && !s.channels.some((c) => c.id === currentChannelId.value)) {
      currentChannelId.value = s.channels[0]?.id ?? ''
    }
  }

  // renameCategory 重命名分类：真实分类走 PATCH；合成组（chan: id）只改本地标签，
  // 下一次 loadChannels 会按服务端数据重建。
  // 本地同步分类名与其下频道的 group——Channel.group 是分类名，按名归组的过渡方案
  // 必须改名即同步，否则归组失联（同名分类仍会合并显示，见 groupsOf 注释）。
  async function renameCategory(serverId: string, catId: string, name: string) {
    const s = findServer(serverId)
    if (!s) return
    const list = extraGroups.value[serverId] ?? []
    const g = list.find((x) => x.id === catId)
    const old = g ? g.name : catId.startsWith('chan:') ? catId.slice(5) : ''
    if (!old) return
    if (isRealID(catId)) {
      await goApp().RenameCategory(catId, name)
    }
    if (g) {
      extraGroups.value = {
        ...extraGroups.value,
        [serverId]: list.map((x) => (x.id === catId ? { ...x, name } : x)),
      }
    }
    s.channels = s.channels.map((c) => (c.group === old ? { ...c, group: name } : c))
  }

  // reorderCategory 把分类移到目标序（0-based，相对 extraGroups 列表）：乐观改本地
  // 顺序与 position，落盘 PATCH（position = 最终序）；失败回滚快照并抛给 UI。
  // 合成组（chan:）不是服务端实体，只改本地状态。
  async function reorderCategory(serverId: string, catId: string, position: number) {
    const prev = (extraGroups.value[serverId] ?? []).map((g) => ({ ...g }))
    const from = prev.findIndex((g) => g.id === catId)
    if (from < 0) return
    const list = [...prev]
    const [moved] = list.splice(from, 1)
    const to = Math.max(0, Math.min(position, list.length))
    if (from === to) return
    list.splice(to, 0, moved)
    extraGroups.value = {
      ...extraGroups.value,
      [serverId]: list.map((g, i) => ({ ...g, position: i })),
    }
    const s = findServer(serverId)
    if (!s || !isRealID(catId)) return
    try {
      await goApp().UpdateCategory(catId, '', to)
    } catch (e) {
      extraGroups.value = { ...extraGroups.value, [serverId]: prev }
      throw e
    }
  }

  // 事件订阅（data:resync，#78）：续传失败后 Go 侧只发提示，服务器/频道视图
  // 由本 store 经绑定重新拉取。eventsOn 返回按监听器的取消函数，卸载时逐个
  // 注销；重复 bind 幂等（避免重入叠监听）。
  const offs: Array<() => void> = []

  function bindEvents() {
    if (offs.length) return
    offs.push(
      eventsOn('data:resync', () => {
        // 拉取失败静默：与登录后首次加载一致，不打断主界面。
        loadRemoteServers().catch(() => {})
      }),
    )
  }

  function unbindEvents() {
    while (offs.length) offs.pop()!()
  }

  return {
    servers, currentServerId, currentChannelId, currentServer, currentChannel,
    homeId, introHidden, initHome, setHome, setIntroHidden,
    selectServer, selectChannel, findServer,
    loadRemoteServers, createServer, joinServer, renameServer, removeServer,
    createChannel, renameChannel, deleteChannel,
    createSubchannel, updateChannelOptions,
    extraGroups, groupsOf, createCategory, deleteCategory, renameCategory, reorderCategory,
    resetAll, bindEvents, unbindEvents,
  }
})

export const useFriendsStore = defineStore('friends', () => {
  // v0.1.2：mock 已删，全部来自 ListFriends REST。accepted 按 user.online
  // 分 online/offline 两组；pending+incoming 进「待处理」区（带接受按钮）。
  const online = ref<Friend[]>([])
  const offline = ref<Friend[]>([])
  // pending：待处理的好友请求。incoming=对方发给我（可接受）；
  // !incoming=我发出待对方回应（仅展示，S3：此前完全不可见）。
  const pending = ref<{ id: string; name: string; outgoing: boolean }[]>([])

  // 在线态只有两态（#11）：数据源是 presence:{uid}，不再造 idle/busy。
  const STATUS_TEXT = { online: '在线', offline: '离线' } as const

  // generation 同 servers：resetAll 后旧账号在飞的 loadFriends 不得写回（#85）。
  let generation = 0

  // loadFriends 拉取并归组。服务端 friendships.remark/group 仅「发起方向」可写，
  // 对方视角恒为空——显示名回落 nickname 属预期，不补双侧字段（服务端规格如此）。
  async function loadFriends() {
    const gen = generation
    const list = await goApp().ListFriends()
    if (gen !== generation) return
    const on: Friend[] = []
    const off: Friend[] = []
    const pend: { id: string; name: string; outgoing: boolean }[] = []
    for (const f of list) {
      if (f.status === 'pending') {
        pend.push({ id: f.id, name: f.user.nickname, outgoing: !f.incoming })
        continue
      }
      if (f.status !== 'accepted') continue
      const key: Friend['status'] = f.user.online ? 'online' : 'offline'
      const entry: Friend = {
        id: f.id,
        userId: f.user.id,
        name: f.remark || f.user.nickname,
        nickname: f.user.nickname,
        remark: f.remark,
        status: key,
        sub: STATUS_TEXT[key],
        group: f.group || '好友',
        incoming: f.incoming,
      }
      ;(key === 'offline' ? off : on).push(entry)
    }
    // 组内按显示名排序，保证重启/刷新后顺序稳定。
    const byName = (a: Friend, b: Friend) => a.name.localeCompare(b.name, 'zh')
    online.value = on.sort(byName)
    offline.value = off.sort(byName)
    pending.value = pend
  }

  // setPresence presence:update 增量刷新单个好友的在线态（#11）：组间移动并保持排序。
  // 不在「来源组」= 状态已一致或本就不在好友列表，直接忽略（去抖）。
  function setPresence(userId: string, isOnline: boolean) {
    const uid = String(userId)
    const from = isOnline ? offline : online
    const to = isOnline ? online : offline
    const idx = from.value.findIndex((f) => f.userId === uid)
    if (idx < 0) return
    const [f] = from.value.splice(idx, 1)
    f.status = isOnline ? 'online' : 'offline'
    f.sub = STATUS_TEXT[f.status]
    to.value.push(f)
    to.value.sort((a, b) => a.name.localeCompare(b.name, 'zh'))
  }

  // sendRequest 发送好友请求（REST；target 形如 昵称#1234，服务端拆分）。
  async function sendRequest(target: string) {
    await goApp().SendFriendRequest(target)
  }

  // acceptRequest 接受好友请求后刷新列表。
  async function acceptRequest(id: string) {
    await goApp().AcceptFriend(id)
    await loadFriends()
  }

  // removeFriend 删除好友（UI 已做二次确认）。
  async function removeFriend(id: string) {
    await goApp().RemoveFriend(id)
    await loadFriends()
  }

  // setMeta 改备注或分组（UpdateFriend 全量写 remark+group，调用方传原值保持另一字段）。
  // 默认组显示值「好友」提交时映射回空串——服务端空=默认组，持久化字面量会
  // 把「空」语义写脏且回不去（review S2）。
  async function setMeta(id: string, remark: string, group: string) {
    await goApp().UpdateFriend(id, remark, group === '好友' ? '' : group)
    await loadFriends()
  }

  // findById 列表内查条目（改备注/分组时取原值用）。
  function findById(id: string): Friend | undefined {
    return [...online.value, ...offline.value].find((f) => f.id === id)
  }

  // resetAll 清空好友列表与待处理请求（#85，登出/被踢/换服/登录新账号前）。
  // 只清会话数据，不动 presence:update / data:resync 订阅（MainView 卸载时
  // 另由 unbindEvents 注销）；幂等。
  function resetAll() {
    generation++
    online.value = []
    offline.value = []
    pending.value = []
  }

  // 事件订阅（presence:update，#11；data:resync，#78）。eventsOn 返回按监听器
  // 的取消函数，卸载时逐个注销——不按事件名整体 Off（会清掉别人注册的同名监听）。
  const offs: Array<() => void> = []

  function bindEvents() {
    if (offs.length) return // 幂等：重复绑定会叠监听
    offs.push(
      eventsOn('presence:update', (d) => {
        const p = d as { user_id?: string; online?: boolean } | null
        if (p?.user_id == null || typeof p.online !== 'boolean') return
        setPresence(p.user_id, p.online)
      }),
      // 续传失败（#78）：好友列表整体重拉——presence 增量之外，断线期间
      // 可能错过好友请求/接受/删除。
      eventsOn('data:resync', () => {
        loadFriends().catch(() => {})
      }),
    )
  }

  function unbindEvents() {
    while (offs.length) offs.pop()!()
  }

  return {
    online, offline, pending, loadFriends, sendRequest, acceptRequest, removeFriend, setMeta, findById,
    setPresence, resetAll, bindEvents, unbindEvents,
  }
})

export const useSettingsStore = defineStore('settings', () => {
  // 设备下拉当前选择（空串 = 系统默认）；持久化在 Go 侧 config.json，
  // 设置页 onMounted 用 GetAudioDeviceSelection 回填初值。
  const captureDevice = ref('')
  const playbackDevice = ref('')

  // v0.1.2：设置改为独立路由页 /settings，原 panelOpen 浮层开关已移除；
  // 休眠模式（hibernate_mode 三态）由 hibernate store 直接对接 Go，不经本 store。
  return { captureDevice, playbackDevice }
})
