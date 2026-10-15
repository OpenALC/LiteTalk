import { defineStore } from 'pinia'
import { computed, ref } from 'vue'
import { goApp } from '../api/bridge'

// 频道/服务器/好友数据域。
// 服务器/频道的管理操作已接真实 REST（批次①）；频道「列出」服务端暂无
// GET 端点，因此 REST 服务器的频道列表只在会话内可见（新建时拿到真实 ID），
// 重启后需服务端补 GET /servers/{id}/channels 才能恢复——见 store 内注释。

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

  // loadRemoteServers 登录后拉取真实服务器列表并入（数字 ID 与内置示例不冲突）。
  // 注意：服务端暂无「列出服务器频道」端点，REST 服务器的频道列表需会话内新建。
  async function loadRemoteServers() {
    const list = await goApp().ListServers()
    for (const s of list) {
      const id = String(s.id)
      if (!servers.value.some((x) => x.id === id)) {
        servers.value.push({ id, name: s.name, icon: iconFromServer(s.icon_name), channels: [] })
      }
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
  const extraGroups = ref<Record<string, { id: string; name: string }[]>>({})

  // groupsOf 某服务器的全部分类（频道 group 并集 + 空分类），保持首次出现序。
  function groupsOf(serverId: string, channels: Channel[]): { id: string; name: string }[] {
    const out: { id: string; name: string }[] = []
    const seen = new Set<string>()
    const put = (id: string, name: string) => {
      if (seen.has(name)) return
      seen.add(name)
      out.push({ id, name })
    }
    for (const c of channels) put(`chan:${c.group}`, c.group)
    for (const g of extraGroups.value[serverId] ?? []) put(g.id, g.name)
    return out
  }

  // createCategory 新建分类：真实服务器走 REST；内置示例仅本地（重启还原，注释已注明）。
  async function createCategory(serverId: string, name: string) {
    const s = findServer(serverId)
    if (!s) return
    if (s.builtin) {
      const list = extraGroups.value[serverId] ?? []
      extraGroups.value = { ...extraGroups.value, [serverId]: [...list, { id: `local-${name}`, name }] }
      return
    }
    const c = await goApp().CreateCategory(Number(serverId), name)
    const list = extraGroups.value[serverId] ?? []
    extraGroups.value = { ...extraGroups.value, [serverId]: [...list, { id: String(c.id), name: c.name }] }
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

  return {
    servers, currentServerId, currentChannelId, currentServer, currentChannel,
    homeId, introHidden, initHome, setHome, setIntroHidden,
    selectServer, selectChannel, findServer,
    loadRemoteServers, createServer, joinServer, renameServer, removeServer,
    createChannel, renameChannel, deleteChannel,
    extraGroups, groupsOf, createCategory, deleteCategory,
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
  const hibernateOnMinimize = ref(false)
  const captureDevice = ref('')
  const playbackDevice = ref('')

  // v0.1.2：设置改为独立路由页 /settings，原 panelOpen 浮层开关已移除。
  return { hibernateOnMinimize, captureDevice, playbackDevice }
})
