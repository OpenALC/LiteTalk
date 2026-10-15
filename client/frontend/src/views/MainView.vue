<script setup lang="ts">
// MainView 三栏主壳（v0.1.2）：服务器栏 + 频道栏（介绍页隐藏）+ 舞台
//（语音房间/文字聊天页签）+ 好友栏。
// 延迟/挂断已迁入 VoiceRoom 底部控制条；设置独立为 /settings 路由；
// 休眠管控上提 App.vue（hibernated 时本视图整体不渲染）。
import { computed, onMounted, onUnmounted, ref } from 'vue'
import { AudioLines, MessageCircle, Users } from 'lucide-vue-next'
import { useRouter } from 'vue-router'
import ServerRail from '../components/ServerRail.vue'
import ChannelList from '../components/ChannelList.vue'
import FriendList from '../components/FriendList.vue'
import VoiceRoom from '../components/VoiceRoom.vue'
import TextChat from '../components/TextChat.vue'
import IntroView from './IntroView.vue'
import { useVoiceStore } from '../stores/voice'
import { useChatStore } from '../stores/chat'
import { useAuthStore } from '../stores/auth'
import { INTRO_ID, useServersStore } from '../stores/data'
import { useHotkeys } from '../composables/useHotkeys'

const router = useRouter()
const voice = useVoiceStore()
const chat = useChatStore()
const auth = useAuthStore()
const servers = useServersStore()

const tab = ref<'voice' | 'chat'>('voice')
const showFriends = ref(true)
const isIntro = computed(() => servers.currentServerId === INTRO_ID)

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

onMounted(() => {
  voice.bindEvents()
  chat.bindEvents()
  // UI 销毁重载后回填语音态（Go 会话未断）；未连接时为空态无副作用。
  voice.restoreFromGo().catch(() => {})
  // 首页偏好：介绍页默认首页；任意服务器可设为首页（config.json 持久化）。
  servers.initHome().then(() => {
    if (!servers.currentServerId) servers.selectServer(servers.homeId)
  })
  // 登录态下并入真实服务器列表（REST；内置示例保留作语音演练场）。
  // 失败静默：服务器列表不可用不阻断主界面。
  if (auth.loggedIn) {
    servers.loadRemoteServers().catch(() => {})
  }
})

onUnmounted(() => {
  voice.unbindEvents()
  chat.unbindEvents()
})
</script>

<template>
  <div class="app">
    <ServerRail />
    <ChannelList v-if="!isIntro" />

    <main class="stage">
      <!-- 介绍页：无 stage-head 页签、无频道栏，品牌卡占满舞台 -->
      <IntroView v-if="isIntro" />

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
