<script setup lang="ts">
// MainView 三栏主壳（对齐 Main.html）：标题栏 + 服务器栏 + 频道栏 +
// 舞台（语音房间/文字聊天页签 + 浮动房间卡）+ 好友栏 + 设置浮层。
import { computed, onMounted, ref } from 'vue'
import {
  AudioLines, MessageCircle, Users, PhoneOff, UserRound,
} from 'lucide-vue-next'
import TitleBar from '../components/TitleBar.vue'
import ServerRail from '../components/ServerRail.vue'
import ChannelList from '../components/ChannelList.vue'
import FriendList from '../components/FriendList.vue'
import VoiceRoom from '../components/VoiceRoom.vue'
import TextChat from '../components/TextChat.vue'
import SettingsView from '../components/SettingsView.vue'
import { useVoiceStore } from '../stores/voice'
import { useChatStore } from '../stores/chat'
import { useAuthStore } from '../stores/auth'
import { useServersStore, useSettingsStore } from '../stores/data'
import { useHibernateStore } from '../stores/hibernate'
import { useHotkeys } from '../composables/useHotkeys'
import { goApp } from '../api/bridge'

const voice = useVoiceStore()
const chat = useChatStore()
const auth = useAuthStore()
const servers = useServersStore()
const settings = useSettingsStore()
const hibernate = useHibernateStore()

const tab = ref<'voice' | 'chat'>('voice')
// 设置浮层开关已入 settings store（panelOpen）：频道栏齿轮与快捷键共用入口。
const showFriends = ref(true)

// 快捷键：休眠态全部禁用。
const hotkeysActive = computed(() => !hibernate.hibernated)
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
    settings: () => settings.openPanel(),
  },
  hotkeysActive,
)

onMounted(() => {
  voice.bindEvents()
  chat.bindEvents()
  // 休眠态：卸载业务组件绑定；唤醒时恢复。
  hibernate.bind((h) => {
    if (h) {
      voice.unbindEvents()
      chat.unbindEvents()
    } else {
      voice.bindEvents()
      chat.bindEvents()
    }
  })
  hibernate.init()
  goApp().GetHibernateOnMinimize().then((v) => hibernate.setEnabled(v))
  // 登录态下并入真实服务器列表（REST；内置示例保留作语音演练场）。
  // 失败静默：服务器列表不可用不阻断主界面。
  if (auth.loggedIn) {
    servers.loadRemoteServers().catch(() => {})
  }
})

async function leave() {
  await voice.leave()
}
</script>

<template>
  <div class="app-shell" :class="{ hibernated: hibernate.hibernated }">
    <TitleBar />

    <div v-if="!hibernate.hibernated" class="app">
      <ServerRail />
      <ChannelList />

      <main class="stage">
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
            <span class="stat"><span class="dot" />延迟 <b>{{ voice.latencyMs }}ms</b></span>
            <span class="stat"><Users :size="14" /><b>{{ voice.members.length }}</b> 人在频道</span>
            <span class="vr" />
            <button class="icon-btn" :class="{ on: showFriends }" title="好友列表"
                    @click="showFriends = !showFriends">
              <Users :size="17" />
            </button>
          </div>
        </div>

        <div class="stage-body">
          <VoiceRoom v-if="tab === 'voice'" />
          <TextChat v-else />

          <!-- 浮动房间卡 -->
          <div v-if="voice.channelId" class="room-card">
            <div class="rc-top">
              <AudioLines :size="16" />
              <span class="rc-name">{{ voice.channelId }}</span>
              <span class="rc-live">已连接</span>
            </div>
            <div class="rc-bottom">
              <div class="rc-dots">
                <i v-for="m in voice.members.slice(0, 5)" :key="m.user_id"
                   :style="{ background: 'var(--brand-500)' }" />
              </div>
              <span class="rc-meta">{{ voice.members.length }} 人在线</span>
              <button class="rc-leave" title="离开频道" @click="leave">
                <PhoneOff :size="15" />
              </button>
            </div>
          </div>

          <SettingsView v-if="settings.panelOpen" @close="settings.closePanel()" />
        </div>
      </main>

      <FriendList v-if="showFriends" />
    </div>
  </div>
</template>

<style scoped>
.app-shell {
  height: 100vh;
  display: flex; flex-direction: column;
  background: var(--surface);
}
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
.stat { display: flex; align-items: center; gap: 6px; }
.stat .dot {
  width: 7px; height: 7px; border-radius: 50%; background: var(--green);
}
.stat b { color: var(--ink-600); font-weight: 600; }
.vr { width: 1px; height: 18px; background: var(--line); }
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
.room-card {
  position: absolute; right: 22px; bottom: 20px; z-index: 20;
  width: 250px;
  background: var(--surface); border: 1px solid var(--line);
  border-radius: 14px; box-shadow: var(--shadow-2);
  padding: 13px 14px;
  display: flex; flex-direction: column; gap: 11px;
}
.rc-top { display: flex; align-items: center; gap: 8px; }
.rc-top :deep(svg) { color: var(--brand-500); }
.rc-top .rc-name {
  font-size: 13.5px; font-weight: 600; flex: 1;
  white-space: nowrap; overflow: hidden; text-overflow: ellipsis;
}
.rc-live {
  display: flex; align-items: center; gap: 5px;
  font-size: 11.5px; font-weight: 600; color: var(--green);
}
.rc-live::before {
  content: ""; width: 7px; height: 7px; border-radius: 50%;
  background: var(--green);
  animation: pulse 2.4s var(--ease) infinite;
}
@keyframes pulse {
  0%, 100% { transform: scale(0.8); opacity: 0.45; }
  50% { transform: scale(1.15); opacity: 0.1; }
}
.rc-bottom { display: flex; align-items: center; gap: 8px; }
.rc-dots { display: flex; }
.rc-dots i {
  width: 22px; height: 22px; border-radius: 50%;
  border: 2px solid var(--surface);
  margin-left: -7px;
}
.rc-dots i:first-child { margin-left: 0; }
.rc-meta { flex: 1; font-size: 11.5px; color: var(--ink-400); }
.rc-leave {
  width: 32px; height: 32px; border-radius: 9px;
  background: rgba(232, 91, 91, 0.1); color: var(--red);
  display: flex; align-items: center; justify-content: center;
  transition: background 0.2s, transform 0.1s var(--ease);
}
.rc-leave:hover { background: rgba(232, 91, 91, 0.2); }
.rc-leave:active { transform: scale(0.9); }
</style>
