import { defineStore } from 'pinia'
import { computed, ref } from 'vue'
import { goApp } from '../api/bridge'
import type { ServerChannels } from '../api/types'
import { useNotifyStore } from './notify'

// 频道/服务器/好友数据域。
// 服务器/频道的管理操作已接真实 REST（批次①）；频道列表经
// GET /servers/{id}/channels 在登录后逐服恢复（#12）——重启不再丢列表。
// 服务端已建频道挂到分类下，但客户端新建频道暂不带 category_id（issue 未覆盖），
// 会话内新建的频道落在类型默认组，重启后随服务端数据归位（见 loadChannels）。

export const UNCATEGORIZED = '未分类'

export interface Channel {
  id: string
  name: string
  type: 'text' | 'voice'
  locked?: boolean
  members?: number
  group: string
}

export interface Server {
  id: string
  name: string
  icon: string // Lucide 图标名（禁头像/上传图——硬性约束）
  channels: Channel[]
  // builtin 内置示例服务器：WS 频道 ID 为不透明字符串，可直接加入通话，
  // 是语音联调的演练场；它们不是服务端实体，管理操作（重命名/删除/邀请）
  // 对其一律提示不可编辑。REST 创建的频道会获得数字 ID，WS 同样可加入。
  builtin?: boolean
  // unavailable 频道列表加载失败（多为被踢出/成员数据异常的 403）：频道区据此
  // 给出解释而不是恒空（#12）。
  unavailable?: boolean
}

export interface Friend {
  id: number // friendship id（备注/分组/删除 REST 的目标）
  name: string // 显示名 = remark || nickname（服务端 remark 仅发起方向可写，对方视角恒空回落昵称——预期行为）
  nickname: string // 原始昵称（添加好友/请求静音等场景用）
  remark: string // 备注原文（空 = 未设置；编辑弹层初值用）
  status: 'online' | 'idle' | 'busy' | 'offline'
  sub: string // 状态行文案（在线/离开/忙碌/离线）
  inChannel?: boolean // 服务端暂无「好友所在频道」数据，恒 false（按钮隐藏）
  group: string // 好友分组（显示值，空归「好友」默认组；提交时映射回空串）
  incoming: boolean // 对方发起=我接受的行。服务端 PATCH 仅发起方向可写——
  // incoming=true 时备注/分组项必须禁用（review B1：否则 100% 弹 403）。
}

// isRealID 判断是否为服务端实体 ID（纯数字）；内置示例为字符串 ID。
export function isRealID(id: string): boolean {
  return /^\d+$/.test(id)
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
  const servers = ref<Server[]>([
    {
      id: 'hq', name: '语音总部', icon: 'MessagesSquare', builtin: true,
      channels: [
        { id: 'notice', name: '通知公告', type: 'text', group: '文字频道' },
        { id: 'chat', name: '闲聊广场', type: 'text', group: '文字频道' },
        { id: 'tech', name: '技术讨论', type: 'text', group: '文字频道' },
        { id: 'ch-hall', name: '休闲大厅', type: 'voice', members: 4, group: '语音频道' },
        { id: 'ch-game1', name: '竞技室 #1', type: 'voice', members: 2, group: '语音频道' },
        { id: 'ch-game2', name: '竞技室 #2', type: 'voice', group: '语音频道' },
        { id: 'ch-private', name: '私密会议室', type: 'voice', locked: true, members: 1, group: '语音频道' },
        { id: 'ch-audience', name: '观众席', type: 'voice', members: 8, group: '特殊频道' },
      ],
    },
    { id: 'game', name: '竞技开黑', icon: 'Gamepad2', builtin: true, channels: [
      { id: 'game-hall', name: '开黑大厅', type: 'voice', members: 3, group: '语音频道' },
    ] },
    { id: 'music', name: '音乐电台', icon: 'Music', builtin: true, channels: [
      { id: 'radio', name: '电台直播间', type: 'voice', group: '语音频道' },
    ] },
    { id: 'design', name: '设计工坊', icon: 'Palette', builtin: true, channels: [] },
    { id: 'book', name: '读书会', icon: 'BookOpen', builtin: true, channels: [] },
  ])
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

  // loadRemoteServers 登录后拉取真实服务器并恢复各自频道列表（数字 ID 与内置示例不冲突）。
  // 逐服调 ListChannels；单服失败标记 unavailable 并提示（#12，不重试），不阻断其它服。
  async function loadRemoteServers() {
    const list = await goApp().ListServers()
    for (const s of list) {
      const id = String(s.id)
      if (!servers.value.some((x) => x.id === id)) {
        servers.value.push({ id, name: s.name, icon: iconFromServer(s.icon_name), channels: [] })
      }
      await loadChannels(id)
    }
  }

  // loadChannels 拉服务器的分类与频道：channel.group 取分类名（无分类归「未分类」），
  // 空分类也进 extraGroups（空分类重启后不丢）。失败不再静默（#12）：标记
  // unavailable 并 toast——被踢出（403）与网络错误都给解释，频道区不能恒空。
  async function loadChannels(serverId: string) {
    const s = findServer(serverId)
    if (!s || s.builtin) return
    let out: ServerChannels
    try {
      out = await goApp().ListChannels(Number(serverId))
    } catch (e) {
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
    s.unavailable = false
    const nameOf = new Map<number, string>()
    for (const c of out.categories ?? []) nameOf.set(c.id, c.name)
    s.channels = (out.channels ?? []).map((c) => ({
      id: String(c.id),
      name: c.name,
      type: c.type,
      group: (c.category_id ? nameOf.get(c.category_id) : undefined) || UNCATEGORIZED,
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

  // createServer 创建服务器（REST），成功即选中。
  async function createServer(name: string) {
    const s = await goApp().CreateServer(name)
    servers.value.push({ id: String(s.id), name: s.name, icon: iconFromServer(s.icon_name), channels: [] })
    selectServer(String(s.id))
  }

  // joinServer 凭邀请码加入（REST），成功即选中；已是成员时服务端原样返回，本地去重。
  async function joinServer(code: string) {
    const s = await goApp().JoinServerByCode(code)
    const id = String(s.id)
    if (!servers.value.some((x) => x.id === id)) {
      servers.value.push({ id, name: s.name, icon: iconFromServer(s.icon_name), channels: [] })
    }
    selectServer(id)
  }

  // renameServer 重命名（REST；内置示例抛错由 UI 提示）。
  async function renameServer(id: string, name: string) {
    const s = findServer(id)
    if (!s) return
    if (s.builtin) throw new Error('示例服务器不可编辑')
    await goApp().RenameServer(Number(id), name)
    s.name = name
  }

  // removeServer 删除服务器（REST，仅 owner）；删除当前所在服务器时回退到第一个。
  // S8：删掉的正是首页服务器时 homeId 回落介绍页，避免下次启动选中幽灵 id。
  async function removeServer(id: string) {
    const s = findServer(id)
    if (!s) return
    if (s.builtin) throw new Error('示例服务器不可删除')
    await goApp().DeleteServer(Number(id))
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

  // createChannel 新建频道（REST），拿到真实数字 ID 后按类型入对应分组。
  async function createChannel(serverId: string, name: string, type: 'text' | 'voice') {
    const s = findServer(serverId)
    if (!s) return
    if (s.builtin) throw new Error('示例服务器不可新建频道')
    const c = await goApp().CreateChannel(Number(serverId), name, type)
    s.channels.push({
      id: String(c.id), name: c.name, type,
      group: type === 'voice' ? '语音频道' : '文字频道',
    })
  }

  // renameChannel 重命名频道：真实频道（数字 ID）同步服务端；
  // 内置示例频道仅本地生效（服务端无对应实体，重启还原）。
  async function renameChannel(serverId: string, chId: string, name: string) {
    const ch = findServer(serverId)?.channels.find((c) => c.id === chId)
    if (!ch) return
    if (isRealID(chId)) {
      await goApp().RenameChannel(Number(chId), name)
    }
    ch.name = name
  }

  // deleteChannel 删除频道（语义同 renameChannel）。调用方负责先离开语音频道。
  async function deleteChannel(serverId: string, chId: string) {
    const s = findServer(serverId)
    if (!s) return
    if (isRealID(chId)) {
      await goApp().DeleteChannel(Number(chId))
    }
    s.channels = s.channels.filter((c) => c.id !== chId)
    if (currentChannelId.value === chId) {
      currentChannelId.value = s.channels[0]?.id ?? ''
    }
  }

  // ---- 频道分类（v0.1.2：服务端 categories 实体 + 内置示例本地语义） ----
  // extraGroups：服务器 ID → 分类列表（含空分类；内置服务器 id 为 'local-<name>'）。
  // position 为显示序（真实分类 = 服务端 position，重排后 0..n-1）。
  const extraGroups = ref<Record<string, { id: string; name: string; position: number }[]>>({})

  // groupsOf 某服务器的全部分类。Channel.group 是分类名——按名归组（同名分类会
  // 合并显示，过渡方案）。排序：显式分类按 position；无分类条目的分组里「未分类」
  // 沉底，其余（内置示例自带组）按频道首次出现序排在显式分类之前。sort 稳定。
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

  // createCategory 新建分类：真实服务器走 REST；内置示例仅本地（重启还原，注释已注明）。
  async function createCategory(serverId: string, name: string) {
    const s = findServer(serverId)
    if (!s) return
    if (s.builtin) {
      const list = extraGroups.value[serverId] ?? []
      extraGroups.value = {
        ...extraGroups.value,
        [serverId]: [...list, { id: `local-${name}`, name, position: list.length }],
      }
      return
    }
    const c = await goApp().CreateCategory(Number(serverId), name)
    const list = extraGroups.value[serverId] ?? []
    extraGroups.value = {
      ...extraGroups.value,
      [serverId]: [...list, { id: String(c.id), name: c.name, position: c.position ?? list.length }],
    }
  }

  // deleteCategory 删除分类及其下频道（二次确认由 UI 负责）：
  // 真实服务器走 REST 级联删除；内置示例逐频道本地删 + 移除空分类。
  async function deleteCategory(serverId: string, catId: string, catName: string) {
    const s = findServer(serverId)
    if (!s) return
    if (!s.builtin && isRealID(catId)) {
      await goApp().DeleteCategory(Number(catId), true) // with_channels=1 级联
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

  // renameCategory 重命名分类：真实服务器走 PATCH；内置示例仅本地（重启还原）。
  // 本地同步分类名与其下频道的 group——Channel.group 是分类名，按名归组的过渡方案
  // 必须改名即同步，否则归组失联（同名分类仍会合并显示，见 groupsOf 注释）。
  // 未分类/内置自带组（chan: id）只改本地标签，重启后还原。
  async function renameCategory(serverId: string, catId: string, name: string) {
    const s = findServer(serverId)
    if (!s) return
    const list = extraGroups.value[serverId] ?? []
    const g = list.find((x) => x.id === catId)
    const old = g ? g.name : catId.startsWith('chan:') ? catId.slice(5) : ''
    if (!old) return
    if (!s.builtin && isRealID(catId)) {
      await goApp().RenameCategory(Number(catId), name)
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
  // 内置示例/本地分类只改本地状态。
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
    if (!s || s.builtin || !isRealID(catId)) return
    try {
      await goApp().UpdateCategory(Number(catId), '', to)
    } catch (e) {
      extraGroups.value = { ...extraGroups.value, [serverId]: prev }
      throw e
    }
  }

  return {
    servers, currentServerId, currentChannelId, currentServer, currentChannel,
    homeId, introHidden, initHome, setHome, setIntroHidden,
    selectServer, selectChannel, findServer,
    loadRemoteServers, createServer, joinServer, renameServer, removeServer,
    createChannel, renameChannel, deleteChannel,
    extraGroups, groupsOf, createCategory, deleteCategory, renameCategory, reorderCategory,
  }
})

export const useFriendsStore = defineStore('friends', () => {
  // v0.1.2：mock 已删，全部来自 ListFriends REST。accepted 按 user.status
  // 分 online/offline 两组；pending+incoming 进「待处理」区（带接受按钮）。
  const online = ref<Friend[]>([])
  const offline = ref<Friend[]>([])
  // pending：待处理的好友请求。incoming=对方发给我（可接受）；
  // !incoming=我发出待对方回应（仅展示，S3：此前完全不可见）。
  const pending = ref<{ id: number; name: string; outgoing: boolean }[]>([])

  const STATUS_TEXT = { online: '在线', idle: '离开', busy: '忙碌', offline: '离线' } as const

  // loadFriends 拉取并归组。服务端 friendships.remark/group 仅「发起方向」可写，
  // 对方视角恒为空——显示名回落 nickname 属预期，不补双侧字段（服务端规格如此）。
  async function loadFriends() {
    const list = await goApp().ListFriends()
    const on: Friend[] = []
    const off: Friend[] = []
    const pend: { id: number; name: string; outgoing: boolean }[] = []
    for (const f of list) {
      if (f.status === 'pending') {
        pend.push({ id: f.id, name: f.user.nickname, outgoing: !f.incoming })
        continue
      }
      if (f.status !== 'accepted') continue
      const key: Friend['status'] =
        f.user.status === 1 ? 'online' : f.user.status === 2 ? 'idle' : f.user.status === 3 ? 'busy' : 'offline'
      const entry: Friend = {
        id: f.id,
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

  // sendRequest 发送好友请求（REST；target 形如 昵称#1234，服务端拆分）。
  async function sendRequest(target: string) {
    await goApp().SendFriendRequest(target)
  }

  // acceptRequest 接受好友请求后刷新列表。
  async function acceptRequest(id: number) {
    await goApp().AcceptFriend(id)
    await loadFriends()
  }

  // removeFriend 删除好友（UI 已做二次确认）。
  async function removeFriend(id: number) {
    await goApp().RemoveFriend(id)
    await loadFriends()
  }

  // setMeta 改备注或分组（UpdateFriend 全量写 remark+group，调用方传原值保持另一字段）。
  // 默认组显示值「好友」提交时映射回空串——服务端空=默认组，持久化字面量会
  // 把「空」语义写脏且回不去（review S2）。
  async function setMeta(id: number, remark: string, group: string) {
    await goApp().UpdateFriend(id, remark, group === '好友' ? '' : group)
    await loadFriends()
  }

  // findById 列表内查条目（改备注/分组时取原值用）。
  function findById(id: number): Friend | undefined {
    return [...online.value, ...offline.value].find((f) => f.id === id)
  }

  return { online, offline, pending, loadFriends, sendRequest, acceptRequest, removeFriend, setMeta, findById }
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
