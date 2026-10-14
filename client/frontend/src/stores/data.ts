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
  name: string
  status: 'online' | 'idle' | 'busy' | 'offline'
  sub: string
  inChannel?: boolean
}

// isRealID 判断是否为服务端实体 ID（纯数字）；内置示例为字符串 ID。
export function isRealID(id: string): boolean {
  return /^\d+$/.test(id)
}

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
  const currentServerId = ref('hq')
  const currentChannelId = ref('ch-hall')

  const currentServer = computed(() =>
    servers.value.find((s) => s.id === currentServerId.value) ?? servers.value[0])
  const currentChannel = computed(() =>
    currentServer.value.channels.find((c) => c.id === currentChannelId.value))

  function selectServer(id: string) {
    currentServerId.value = id
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
  async function removeServer(id: string) {
    const s = findServer(id)
    if (!s) return
    if (s.builtin) throw new Error('示例服务器不可删除')
    await goApp().DeleteServer(Number(id))
    servers.value = servers.value.filter((x) => x.id !== id)
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

  return {
    servers, currentServerId, currentChannelId, currentServer, currentChannel,
    selectServer, selectChannel, findServer,
    loadRemoteServers, createServer, joinServer, renameServer, removeServer,
    createChannel, renameChannel, deleteChannel,
  }
})

export const useFriendsStore = defineStore('friends', () => {
  const online = ref<Friend[]>([
    { name: 'Yuki', status: 'online', sub: '休闲大厅', inChannel: true },
    { name: 'Milo', status: 'online', sub: '休闲大厅', inChannel: true },
    { name: 'Nael', status: 'idle', sub: '离开' },
    { name: 'Sora', status: 'online', sub: '在线' },
    { name: 'Lena', status: 'busy', sub: '忙碌' },
    { name: 'Riva', status: 'online', sub: '在线' },
    { name: 'Zeph', status: 'online', sub: '竞技室 #1', inChannel: true },
    { name: 'Dax', status: 'online', sub: '在线' },
  ])
  const offline = ref<Friend[]>([
    { name: 'Quinn', status: 'offline', sub: '离线' },
    { name: 'Tara', status: 'offline', sub: '离线' },
  ])

  // sendRequest 发送好友请求（REST；target 形如 昵称#1234，服务端拆分）。
  async function sendRequest(target: string) {
    await goApp().SendFriendRequest(target)
  }

  return { online, offline, sendRequest }
})

export const useSettingsStore = defineStore('settings', () => {
  const hibernateOnMinimize = ref(false)
  const captureDevice = ref('')
  const playbackDevice = ref('')

  // panelOpen 设置浮层开关（ChannelList 底部齿轮 / MainView 快捷键共用入口）。
  const panelOpen = ref(false)
  function openPanel() { panelOpen.value = true }
  function closePanel() { panelOpen.value = false }

  return { hibernateOnMinimize, captureDevice, playbackDevice, panelOpen, openPanel, closePanel }
})
