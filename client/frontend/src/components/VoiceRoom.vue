<script setup lang="ts">
// VoiceRoom 语音房间（v0.1.2）：成员卡片网格 + 底部语音控制条（voice-bar）。
// voice-bar 仅在语音频道中时显示：真实 RTT 延迟 + 挂断；无频道名/人数
//（走查 1/2：延迟不常驻、卡片位于语音列表区下方）。
// 批次①：成员卡片「更多」弹层（音量调节/查看资料）；邀请卡点击生成邀请码并复制。
import { ref } from 'vue'
import {
  AudioLines, MicOff, HeadphoneOff, MoreHorizontal, UserPlus, Volume2, Info, PhoneOff,
} from 'lucide-vue-next'
import SoundOrb from '../components/SoundOrb.vue'
import { useVoiceStore } from '../stores/voice'
import { useServersStore } from '../stores/data'
import { useNotifyStore } from '../stores/notify'
import { goApp } from '../api/bridge'

const voice = useVoiceStore()
const servers = useServersStore()
const notify = useNotifyStore()

// 状态胶囊（双关态优先——用户要求的合并样式）。
// 文案约定（实测走查 16）：麦克风关闭 = 已闭麦；扬声器关闭 = 已静音。
function chipFor(m: { mute: boolean; deafen: boolean; speaking: boolean }) {
  if (m.mute && m.deafen) return { cls: 'red dual', text: '已闭麦 已静音', dual: true }
  if (m.mute) return { cls: 'red', text: '已闭麦', icon: MicOff }
  if (m.deafen) return { cls: 'red', text: '已静音', icon: HeadphoneOff }
  if (m.speaking) return { cls: 'brand', text: '正在说话', icon: AudioLines }
  return null
}

// ---- 成员「更多」弹层（音量滑杆 + 查看资料） ----
// moreFor 记录打开弹层的成员 user_id；弹层挂在该卡片内（绝对定位右上角）。
// volumePct 初值取 voice.peerVolumes 缓存（本机最近设置），不再每次重置 100。
const moreFor = ref('')
const volumePct = ref(100)

function toggleMore(userID: string) {
  moreFor.value = moreFor.value === userID ? '' : userID
  volumePct.value = Math.round((voice.peerVolumes[userID] ?? 1) * 100)
}

// applyVolume 拖动结束时应用（@change 而非 @input，避免高频调 Go 侧）。
async function applyVolume(userID: string) {
  try {
    await voice.setPeerVolume(userID, volumePct.value / 100)
  } catch (e) {
    notify.fromErr(e, '音量调节失败')
  }
}

function showProfile(m: { user_id: string; nickname: string }) {
  moreFor.value = ''
  notify.show(`${m.nickname}（ID: ${m.user_id}）`)
}

// ---- 邀请卡：为当前服务器生成邀请码并复制（对齐原型：点击即复制） ----
async function invite() {
  const s = servers.currentServer
  if (s.builtin) {
    // 内置示例服务器不是服务端实体，无法签发邀请码。
    notify.show('示例服务器暂不支持邀请，请先通过 + 创建服务器', true)
    return
  }
  try {
    const code = await goApp().CreateInviteCode(Number(s.id))
    await navigator.clipboard.writeText(code)
    notify.show('邀请码已复制，发给朋友吧')
  } catch (e) {
    notify.fromErr(e)
  }
}

// hangup 挂断：离开当前语音频道（voice-bar 按钮）。
async function hangup() {
  try {
    await voice.leave()
  } catch (e) {
    notify.fromErr(e, '离开频道失败')
  }
}
</script>

<template>
  <div class="voice-room">
    <!-- data-ctx：成员卡片右键菜单（v0.1.2 批次③在此挂自定义菜单） -->
    <div class="voice-grid" data-ctx>
      <div v-for="m in voice.members" :key="m.user_id" class="vcard">
        <!-- 自己的卡片不弹「更多」（音量调节/资料都对他人有意义） -->
        <button v-if="m.user_id !== voice.selfId()" class="more" title="更多"
                @click="toggleMore(m.user_id)">
          <MoreHorizontal :size="16" />
        </button>
        <SoundOrb :user-id="m.user_id" :speaking="m.speaking"
                  :muted="m.mute" :deafen="m.deafen" />
        <div class="vname">
          {{ m.nickname }}
          <span v-if="m.user_id === voice.selfId()" class="me-tag">我</span>
        </div>
        <span v-if="chipFor(m)" class="chip" :class="chipFor(m)!.cls">
          <template v-if="chipFor(m)!.dual">
            <MicOff :size="12" />已闭麦<HeadphoneOff :size="12" />已静音
          </template>
          <template v-else>
            <component :is="chipFor(m)!.icon" :size="12" />{{ chipFor(m)!.text }}
          </template>
        </span>

        <!-- 成员操作弹层：音量滑杆 + 查看资料 -->
        <template v-if="moreFor === m.user_id">
          <div class="more-mask" @click="moreFor = ''" />
          <div class="more-pop">
            <div class="mp-row">
              <Volume2 :size="14" />
              <input v-model.number="volumePct" type="range" min="0" max="200" step="10"
                     @change="applyVolume(m.user_id)">
              <span class="mp-v">{{ volumePct }}%</span>
            </div>
            <button class="mp-item" @click="showProfile(m)">
              <Info :size="14" />查看资料
            </button>
          </div>
        </template>
      </div>

      <div class="vcard invite" @click="invite">
        <div class="i-circle"><UserPlus :size="20" /></div>
        <span>邀请朋友加入频道</span>
      </div>
    </div>

    <!-- 语音控制条（走查 1/2）：仅语音频道中显示；延迟 + 挂断，无频道名/人数 -->
    <div v-if="voice.channelId" class="voice-bar">
      <span class="vb-lat">
        <span class="dot" />延迟 {{ voice.latencyMs }}ms
      </span>
      <button class="vb-leave" title="挂断" @click="hangup">
        <PhoneOff :size="15" />挂断
      </button>
    </div>
  </div>
</template>

<style scoped>
/* voice-room：网格占满、控制条贴底（卡片位于语音列表区下方） */
.voice-room {
  flex: 1; min-height: 0;
  display: flex; flex-direction: column;
}
.voice-grid {
  flex: 1; overflow-y: auto; padding: 22px;
  display: grid; gap: 16px; align-content: start;
  grid-template-columns: repeat(2, minmax(0, 1fr));
}
.voice-bar {
  flex: none;
  display: flex; align-items: center; justify-content: space-between;
  margin: 0 22px 20px; padding: 10px 14px;
  background: var(--surface); border: 1px solid var(--line);
  border-radius: 12px; box-shadow: var(--shadow-1);
}
.vb-lat {
  display: flex; align-items: center; gap: 7px;
  font-size: 12.5px; color: var(--ink-400);
}
.vb-lat .dot {
  width: 7px; height: 7px; border-radius: 50%; background: var(--green);
}
.vb-leave {
  display: inline-flex; align-items: center; gap: 6px;
  height: 32px; padding: 0 14px; border-radius: 9px;
  background: rgba(232, 91, 91, 0.1); color: var(--red);
  font-size: 13px; font-weight: 600;
  transition: background 0.2s, transform 0.1s var(--ease);
}
.vb-leave:hover { background: rgba(232, 91, 91, 0.2); }
.vb-leave:active { transform: scale(0.95); }
.vcard {
  position: relative; min-height: 172px;
  background: var(--surface); border: 1px solid var(--line);
  border-radius: 16px;
  display: flex; flex-direction: column; align-items: center; justify-content: center;
  gap: 10px; padding: 20px;
  transition: transform 0.25s var(--ease), box-shadow 0.25s var(--ease), border-color 0.25s var(--ease);
}
.vcard:hover {
  transform: translateY(-2px);
  box-shadow: var(--shadow-2); border-color: var(--brand-200);
}
.vcard .more {
  position: absolute; top: 10px; right: 10px;
  width: 28px; height: 28px; border-radius: 8px;
  display: flex; align-items: center; justify-content: center;
  color: var(--ink-300); opacity: 0;
  transition: opacity 0.2s, background 0.15s, color 0.15s;
}
.vcard:hover .more { opacity: 1; }
.vcard .more:hover { background: var(--brand-50); color: var(--ink-600); }
.vname { font-size: 15px; font-weight: 600; }
.me-tag {
  font-size: 11px; font-weight: 600;
  color: var(--brand-600); background: rgba(79, 164, 243, 0.12);
  padding: 1px 6px; border-radius: 6px; margin-left: 5px; vertical-align: 1px;
}
.chip {
  display: inline-flex; align-items: center; gap: 4px;
  padding: 3px 9px; border-radius: 99px;
  font-size: 11.5px; font-weight: 500;
}
.chip.red { background: rgba(232, 91, 91, 0.1); color: var(--red); }
.chip.brand { background: rgba(79, 164, 243, 0.1); color: var(--brand-600); }
/* 双关态胶囊：红底加深加重（区别于单关） */
.chip.red.dual { background: rgba(232, 91, 91, 0.16); font-weight: 600; }
.vcard.invite {
  border: 1.5px dashed var(--brand-200); background: transparent;
  cursor: pointer; color: var(--ink-400);
}
.vcard.invite:hover {
  border-color: var(--brand-500); color: var(--brand-600);
  box-shadow: none; background: rgba(79, 164, 243, 0.03);
}
.vcard.invite .i-circle {
  width: 44px; height: 44px; border-radius: 50%;
  background: rgba(79, 164, 243, 0.1);
  display: flex; align-items: center; justify-content: center;
  color: var(--brand-500);
  transition: transform 0.25s var(--ease);
}
.vcard.invite:hover .i-circle { transform: scale(1.08); }
.vcard.invite span { font-size: 13px; font-weight: 500; }

/* 成员「更多」弹层 */
.more-mask { position: fixed; inset: 0; z-index: 25; }
.more-pop {
  position: absolute; top: 42px; right: 10px; z-index: 30;
  width: 200px; padding: 10px;
  background: var(--surface); border: 1px solid var(--line);
  border-radius: 12px; box-shadow: var(--shadow-2);
}
.mp-row { display: flex; align-items: center; gap: 8px; padding: 2px 4px 8px; }
.mp-row :deep(svg) { color: var(--ink-400); flex: none; }
.mp-row input[type="range"] {
  flex: 1; min-width: 0; accent-color: var(--brand-500);
}
.mp-v { font-size: 11.5px; color: var(--ink-400); width: 36px; text-align: right; }
.mp-item {
  width: 100%; height: 32px; padding: 0 8px; border-radius: 8px;
  display: flex; align-items: center; gap: 8px;
  font-size: 13px; font-weight: 500; color: var(--ink-600);
  transition: background 0.15s, color 0.15s;
}
.mp-item:hover { background: var(--brand-50); color: var(--ink-900); }
</style>
