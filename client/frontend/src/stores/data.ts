import { defineStore } from 'pinia'
import { computed, ref } from 'vue'

// 频道/服务器/好友数据域。M3 服务端 CRUD 由并行同学实现（§5.4），
// 当前先内置与原型一致的结构化数据，REST 就绪后替换 fetch 路径即可。

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
}

export interface Friend {
  name: string
  status: 'online' | 'idle' | 'busy' | 'offline'
  sub: string
  inChannel?: boolean
}

export const useServersStore = defineStore('servers', () => {
  const servers = ref<Server[]>([
    {
      id: 'hq', name: '语音总部', icon: 'MessagesSquare',
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
    { id: 'game', name: '竞技开黑', icon: 'Gamepad2', channels: [
      { id: 'game-hall', name: '开黑大厅', type: 'voice', members: 3, group: '语音频道' },
    ] },
    { id: 'music', name: '音乐电台', icon: 'Music', channels: [
      { id: 'radio', name: '电台直播间', type: 'voice', group: '语音频道' },
    ] },
    { id: 'design', name: '设计工坊', icon: 'Palette', channels: [] },
    { id: 'book', name: '读书会', icon: 'BookOpen', channels: [] },
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

  return { servers, currentServerId, currentChannelId, currentServer, currentChannel, selectServer, selectChannel }
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
  return { online, offline }
})

export const useSettingsStore = defineStore('settings', () => {
  const hibernateOnMinimize = ref(false)
  const captureDevice = ref('')
  const playbackDevice = ref('')

  return { hibernateOnMinimize, captureDevice, playbackDevice }
})
