<script setup lang="ts">
// MainView 三栏主壳（v0.1.2）：服务器栏 + 频道栏（介绍页隐藏）+ 舞台
//（语音房间/文字聊天页签）+ 好友栏。
// 延迟/挂断已迁入 VoiceRoom 底部控制条；设置独立为 /settings 路由；
// 休眠管控上提 App.vue（hibernated 时本视图整体不渲染）。
import { computed, onMounted, onUnmounted, ref } from 'vue'
import { AudioLines, MessageCircle, Users, WifiOff } from 'lucide-vue-next'
import { useRouter } from 'vue-router'
import ServerRail from '../components/ServerRail.vue'
import ChannelList from '../components/ChannelList.vue'
import ConversationList from '../components/ConversationList.vue'
import FriendList from '../components/FriendList.vue'
import VoiceRoom from '../components/VoiceRoom.vue'
import TextChat from '../components/TextChat.vue'
import ConversationChat from '../components/ConversationChat.vue'
import IntroView from './IntroView.vue'
import { useVoiceStore } from '../stores/voice'
import { useChatStore } from '../stores/chat'
import { useAuthStore } from '../stores/auth'
import { useConversationsStore } from '../stores/conversations'
import { INTRO_ID, useFriendsStore, useServersStore } from '../stores/data'
import { useHotkeys } from '../composables/useHotkeys'
import { eventsOn } from '../api/bridge'

const router = useRouter()
const voice = useVoiceStore()
const chat = useChatStore()
const auth = useAuthStore()
const conversations = useConversationsStore()
const servers = useServersStore()
const friends = useFriendsStore()

const tab = ref<'voice' | 'chat'>('voice')
const showFriends = ref(true)
// 没有当前服务器（登录前/列表为空）时同样按介绍页处理：频道栏与舞台都依赖
// currentServer 的字段，不能在列表为空时渲染。私信模式独立于服务器选择，
// 此时必须压在介绍页之上（即使没有任何服务器）。
const isIntro = computed(() =>
  !conversations.dmMode
  && (servers.currentServerId === INTRO_ID || !servers.currentServer))

const hotkeysActive = computed(() => true)
useHotkeys(
  {
    mute: () => voice.setMute(!voice.muted),
    deafen: () => voice.setDeafen(!voice.deafened),
    dual: () => {
      const allOff = !voice.dualOff
      voice.setMute(allOff)
      voice.setDeafen(allOff)
    },
    search: () => {
      const el = document.querySelector<HTMLInputElement>('.f-search input')
      el?.focus()
    },
    // 设置已独立为路由页（v0.1.2）
    settings: () => router.push('/settings'),
  },
  hotkeysActive,
)

// auth:session：Go 侧重试连上/中途断线时翻提示条。
// eventsOn 返回按监听器的取消函数，卸载时逐个注销（不按事件名整体 Off）。
let offAuthSession: () => void = () => {}

onMounted(() => {
  voice.bindEvents()
  chat.bindEvents()
  // 服务器/频道与好友的事件订阅上提到主壳：data:resync（#78）与 presence:update
  // 必须在登录后立即生效，不能依赖 FriendList 是否展开（它是可隐藏的子组件）。
  servers.bindEvents()
  friends.bindEvents()
  conversations.bindEvents()
  offAuthSession = eventsOn('auth:session', (d) => {
    auth.setSignalOk((d as { ok?: boolean }).ok !== false)
  })
  // UI 销毁重载后回填语音态（Go 会话未断）；未连接时为空态无副作用。
  voice.restoreFromGo().catch(() => {})
  // 首页偏好：介绍页默认首页；任意服务器可设为首页（config.json 持久化）。
  servers.initHome().then(() => {
    if (!servers.currentServerId) servers.selectServer(servers.homeId)
  })
  // 登录态下拉取真实服务器列表（REST）。
  // 失败静默：服务器列表不可用不阻断主界面。
  if (auth.loggedIn) {
    servers.loadRemoteServers().catch(() => {})
    // 私信列表登录后预取：进入私信模式时不必等一次网络往返。失败静默。
    conversations.load().catch(() => {})
  }
})

onUnmounted(() => {
  voice.unbindEvents()
  chat.unbindEvents()
  servers.unbindEvents()
  friends.unbindEvents()
  conversations.unbindEvents()
  offAuthSession()
})
</script>

<template>
  <!-- 信令未连上（登录后连接失败重试中 / 中途断线）：顶部提示条。
       服务端主动关闭（sys:shutdown）时改口明示——Go 侧已停重连，
       继续说「信令连接中」等于骗用户它还会回来。 -->
  <div v-if="auth.loggedIn && (auth.shutdown || !auth.signalOk)"
       class="signal-bar" :class="{ steady: !!auth.shutdown }" role="status">
    <WifiOff :size="14" />
    <span>{{ auth.shutdown ? '服务端已关闭' : '信令连接中，语音暂不可用' }}</span>
  </div>
  <div class="app">
    <ServerRail />
    <ConversationList v-if="conversations.dmMode" />
    <ChannelList v-else-if="!isIntro" />

    <main class="stage">
      <!-- 介绍页：无 stage-head 页签、无频道栏，品牌卡占满舞台 -->
      <IntroView v-if="isIntro" />

      <!-- 私信模式：第二栏为会话列表，舞台只保留标题与好友栏开关 -->
      <template v-else-if="conversations.dmMode">
        <div class="stage-head">
          <div class="dm-title">
            <MessageCircle :size="15" />私信
          </div>
          <div class="stage-stats">
            <button class="icon-btn" :class="{ on: showFriends }" title="好友列表"
                    @click="showFriends = !showFriends">
              <Users :size="17" />
            </button>
          </div>
        </div>

        <div class="stage-body">
          <ConversationChat />
        </div>
      </template>

      <template v-else>
        <div class="stage-head">
          <div class="tabs">
            <button :class="{ on: tab === 'voice' }" @click="tab = 'voice'">
              <AudioLines :size="15" />语音房间
            </button>
            <button :class="{ on: tab === 'chat' }" @click="tab = 'chat'">
              <MessageCircle :size="15" />文字聊天
            </button>
          </div>
          <div class="stage-stats">
            <button class="icon-btn" :class="{ on: showFriends }" title="好友列表"
                    @click="showFriends = !showFriends">
              <Users :size="17" />
            </button>
          </div>
        </div>

        <div class="stage-body">
          <VoiceRoom v-if="tab === 'voice'" />
          <TextChat v-else />
        </div>
      </template>
    </main>

    <FriendList v-if="showFriends && !isIntro" />
  </div>
</template>

<style scoped>
/* 信令提示条：置顶悬浮，不扰动三栏布局；动画只用 opacity（规范约束）。 */
.signal-bar {
  position: fixed;
  top: 0; left: 0; right: 0;
  z-index: 60;
  height: 30px;
  display: flex; align-items: center; justify-content: center; gap: 7px;
  background: var(--brand-500);
  color: var(--surface);
  font-size: 12.5px; font-weight: 500;
  animation: signal-pulse 1.6s ease-in-out infinite;
}
@keyframes signal-pulse {
  0%, 100% { opacity: 1; }
  50% { opacity: 0.72; }
}
/* steady 终态（服务端已关闭）：脉冲暗示「正在重试」，这里没有重试可等。 */
.signal-bar.steady { animation: none; }
.app {
  flex: 1; min-height: 0;
  display: flex;
  background: var(--surface);
}
.stage {
  flex: 1; min-width: 0;
  display: flex; flex-direction: column;
  background: var(--brand-50);
  position: relative;
}
.stage-head {
  height: 56px; flex: none;
  display: flex; align-items: center; gap: 16px;
  padding: 0 18px;
  background: var(--surface);
  border-bottom: 1px solid var(--line);
}
.tabs {
  display: flex;
  background: var(--brand-50); border-radius: 10px;
  padding: 3px; gap: 2px;
}
.tabs button {
  display: flex; align-items: center; gap: 6px;
  height: 32px; padding: 0 14px; border-radius: 8px;
  font-size: 13px; font-weight: 600; color: var(--ink-400);
  transition: all 0.2s var(--ease);
}
.tabs button.on {
  background: var(--surface); color: var(--brand-700);
  box-shadow: var(--shadow-1);
}
/* 私信模式标题：与页签同高同字重，不提供切换（私信只有会话视图） */
.dm-title {
  display: flex; align-items: center; gap: 6px;
  height: 32px; padding: 0 14px; border-radius: 8px;
  background: var(--brand-50);
  font-size: 13px; font-weight: 600; color: var(--brand-700);
}
.dm-title :deep(svg) { color: var(--brand-600); }
.stage-stats {
  margin-left: auto;
  display: flex; align-items: center; gap: 16px;
  font-size: 12.5px; color: var(--ink-400);
}
.icon-btn {
  width: 32px; height: 32px;
  display: flex; align-items: center; justify-content: center;
  border-radius: 8px; color: var(--ink-600);
  transition: background 0.15s, color 0.15s;
}
.icon-btn:hover { background: rgba(79, 164, 243, 0.12); color: var(--brand-700); }
.icon-btn.on { background: rgba(79, 164, 243, 0.12); color: var(--brand-700); }
.stage-body {
  flex: 1; min-height: 0;
  position: relative;
  display: flex; flex-direction: column;
}
</style>
