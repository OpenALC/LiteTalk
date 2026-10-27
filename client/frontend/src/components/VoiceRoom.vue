<script setup lang="ts">
// VoiceRoom 语音房间（v0.1.2）：成员卡片网格 + 底部语音控制条（voice-bar）。
// voice-bar 仅在语音频道中时显示：真实 RTT 延迟 + 挂断；无频道名/人数
//（走查 1/2：延迟不常驻、卡片位于语音列表区下方）。
// 批次③：成员卡片右键菜单——他人=添加好友/调节播放音量/请求闭麦/请求静音，
// 自己=离开频道（走查 6）；请求经 voice.request_state 定向转发，对方接受后生效。
import { computed, ref, watch } from 'vue'
import {
  Activity, AudioLines, MicOff, HeadphoneOff, MoreHorizontal, UserPlus, Volume2, Info, PhoneOff,
  UserPlus2, Ear, Crown,
} from 'lucide-vue-next'
import SoundOrb from '../components/SoundOrb.vue'
import { useVoiceStore } from '../stores/voice'
import { useServersStore } from '../stores/data'
import { useNotifyStore } from '../stores/notify'
import { goApp } from '../api/bridge'
import type { PeerView } from '../api/types'
import ContextMenu, { type CtxItem } from './ContextMenu.vue'
import PromptDialog from './PromptDialog.vue'

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

// ---- 邀请卡（新版 invite 在脚本后部，含剪贴板兜底） ----

// hangup 挂断：离开当前语音频道（voice-bar 按钮）。
async function hangup() {
  try {
    await voice.leave()
  } catch (e) {
    notify.fromErr(e, '离开频道失败')
  }
}

// ---- 网络详情浮层（#49 B）----
// voice-bar 只常驻 RTT；丢包/抖动/缓冲深度按需点开，避免控制条信息过载。
// 离房时自动收起，重进房需要重新点开。
const netOpen = ref(false)
watch(() => voice.channelId, (id) => { if (!id) netOpen.value = false })

function fmtPct(v: number | undefined): string {
  return v === undefined || v === null ? '—' : `${v.toFixed(1)}%`
}
function fmtMS(v: number | undefined): string {
  return v === undefined || v === null ? '—' : `${Math.round(v)}ms`
}

// ---- 成员右键菜单（批次③，走查 6） ----
// 他人：添加好友 / 调节播放音量 / 请求闭麦 / 请求静音；
// 自己：离开频道（对他人=踢人，需权限模型，超范围——设计决策 5.9）。
const ctx = ref<{ x: number; y: number; member: PeerView } | null>(null)
const isSelf = (m: PeerView) => m.user_id === voice.selfId()
const ctxItems = computed<CtxItem[]>(() => {
  const c = ctx.value
  if (!c) return []
  if (isSelf(c.member)) {
    return [{ key: 'leave', label: '离开频道', icon: PhoneOff, danger: true }]
  }
  return [
    { key: 'add-friend', label: '添加好友', icon: UserPlus2 },
    { key: 'volume', label: '调节播放音量', icon: Volume2 },
    // 悄悄话（#45）：目标选择由本机维护，按住悄悄话热键时才真正只发给这些人。
    {
      key: 'whisper',
      label: voice.isWhisperTarget(c.member.user_id) ? '取消悄悄话目标' : '设为悄悄话目标',
      icon: Ear,
    },
    // 优先发言（#45）：需 ManageChannels，无权限时服务端 403 → toast。
    {
      key: 'priority',
      label: c.member.priority ? '取消优先发言' : '设为优先发言',
      icon: Crown,
    },
    { key: 'req-mute', label: '请求闭麦', icon: MicOff },
    { key: 'req-deafen', label: '请求静音', icon: HeadphoneOff },
  ]
})

function openMemberCtx(e: MouseEvent, m: PeerView) {
  ctx.value = { x: e.clientX, y: e.clientY, member: m }
}

// addTarget：添加好友弹层预填的昵称。
const addTarget = ref('')

function onCtxSelect(key: string) {
  const c = ctx.value
  if (!c) return
  const m = c.member
  switch (key) {
    case 'add-friend':
      addTarget.value = m.tag ? `${m.nickname}#${m.tag}` : m.nickname
      break
    case 'volume':
      toggleMore(m.user_id)
      break
    case 'whisper':
      void voice.toggleWhisperTarget(m.user_id)
      break
    case 'priority':
      void voice.setPrioritySpeaker(m.user_id, !m.priority)
      break
    case 'req-mute':
      // 请求≠强制：定向转发给对方，对方在 toast 里选择接受（设计决策 5.4）。
      voice.requestVoiceState(m.user_id, true, false).catch((e) => notify.fromErr(e, '请求失败'))
      break
    case 'req-deafen':
      voice.requestVoiceState(m.user_id, false, true).catch((e) => notify.fromErr(e, '请求失败'))
      break
    case 'leave':
      hangup()
      break
  }
}

async function onAddFriendConfirm(target: string) {
  addTarget.value = ''
  try {
    await goApp().SendFriendRequest(target)
    notify.show('好友请求已发送')
  } catch (e) {
    notify.fromErr(e)
  }
}

// invite 邀请卡的剪贴板兜底：clipboard 失败（权限/非安全上下文）时把码直接
// 展示在 toast 里，不让邀请码凭空丢失（批次① review 建议 8）。
async function invite() {
  const s = servers.currentServer
  try {
    const code = await goApp().CreateInviteCode(s.id)
    try {
      await navigator.clipboard.writeText(code)
      notify.show('邀请码已复制，发给朋友吧')
    } catch {
      notify.show(`邀请码：${code}（复制失败，请手动抄录）`, true)
    }
  } catch (e) {
    notify.fromErr(e)
  }
}
</script>

<template>
  <div class="voice-room">
    <div class="voice-grid">
      <div v-for="m in voice.members" :key="m.user_id" class="vcard"
           @contextmenu.prevent="openMemberCtx($event, m)">
        <!-- 自己的卡片不弹「更多」（音量调节/资料都对他人有意义） -->
        <button v-if="m.user_id !== voice.selfId()" class="more" title="更多"
                @click="toggleMore(m.user_id)">
          <MoreHorizontal :size="16" />
        </button>
        <SoundOrb :user-id="m.user_id" :speaking="m.speaking"
                  :muted="m.mute" :deafen="m.deafen" />
        <div class="vname">
          {{ m.nickname }}<span v-if="m.tag" class="vtag">#{{ m.tag }}</span>
          <span v-if="m.user_id === voice.selfId()" class="me-tag">我</span>
          <!-- 优先发言 / 悄悄话目标徽标（Lucide，不用 emoji） -->
          <span v-if="m.priority" class="vbadge prio" title="优先发言"><Crown :size="12" /></span>
          <span v-if="voice.isWhisperTarget(m.user_id)" class="vbadge ear"
                title="悄悄话目标"><Ear :size="12" /></span>
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
            <!-- per-peer 链路诊断（#49 B）：有下行统计才显示 -->
            <div v-if="voice.peerDiag(m.user_id)" class="mp-diag">
              <div class="mp-drow">
                <span>丢包</span><b>{{ fmtPct(voice.peerDiag(m.user_id)?.loss_pct ?? 0) }}</b>
              </div>
              <div class="mp-drow">
                <span>抖动</span><b>{{ fmtMS(voice.peerDiag(m.user_id)?.rtp_jitter_ms ?? 0) }}</b>
              </div>
              <div class="mp-drow">
                <span>缓冲</span><b>{{ fmtMS(voice.peerDiag(m.user_id)?.buffer_ms ?? 0) }}</b>
              </div>
            </div>
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
      <div class="vb-lat-wrap">
        <button class="vb-lat" title="查看网络详情" @click="netOpen = !netOpen">
          <Activity :size="13" />延迟 {{ voice.latencyMs }}ms
        </button>
        <!-- 网络详情浮层（#49 B）：丢包 / 抖动 / 抖动缓冲深度 -->
        <template v-if="netOpen">
          <div class="net-mask" @click="netOpen = false" />
          <div class="net-pop">
            <div class="np-row">
              <span>丢包</span><b>{{ fmtPct(voice.stats.loss_pct ?? 0) }}</b>
            </div>
            <div class="np-row">
              <span>抖动</span><b>{{ fmtMS(voice.stats.rtp_jitter_ms ?? 0) }}</b>
            </div>
            <div class="np-row">
              <span>缓冲</span>
              <b>{{ fmtMS(voice.stats.jitter_buffer_ms ?? 0) }}</b>
              <span class="np-dim">/ {{ fmtMS(voice.stats.jitter_target_ms ?? 0) }}</span>
            </div>
          </div>
        </template>
      </div>
      <!-- 悄悄话目标指示（#45）：按住热键且有目标时高亮；离房目标标「离线」 -->
      <span v-if="voice.whisperTargetViews.length" class="vb-whisper"
            :class="{ on: voice.whisperActive }"
            :title="voice.whisperActive ? '悄悄话发送中（仅目标可听）' : '按住悄悄话热键发送'">
        <Ear :size="13" />
        <span class="vb-wlabel">{{ voice.whisperActive ? '悄悄话中' : '悄悄话目标' }}</span>
        <span v-for="(t, i) in voice.whisperTargetViews" :key="t.user_id" class="vb-wt">
          {{ t.nickname }}<span v-if="!t.online" class="vb-off">离线</span><template
            v-if="i < voice.whisperTargetViews.length - 1">、</template>
        </span>
      </span>
      <button class="vb-leave" title="挂断" @click="hangup">
        <PhoneOff :size="15" />挂断
      </button>
    </div>

    <!-- 成员右键菜单 + 添加好友弹层（批次③） -->
    <ContextMenu v-if="ctx" :x="ctx.x" :y="ctx.y" :items="ctxItems"
                 @select="onCtxSelect" @close="ctx = null" />
    <PromptDialog v-if="addTarget" title="添加好友" placeholder="昵称#1234"
                  cta="发送请求" :initial="addTarget"
                  @confirm="onAddFriendConfirm" @close="addTarget = ''" />
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
.vb-lat-wrap { position: relative; }
.vb-lat {
  display: flex; align-items: center; gap: 7px;
  height: 26px; padding: 0 8px; margin-left: -8px; border-radius: 8px;
  font-size: 12.5px; color: var(--ink-400);
  transition: background 0.15s, color 0.15s;
}
.vb-lat:hover { background: var(--brand-50); color: var(--ink-600); }
.vb-lat svg { flex: none; color: var(--green); }
/* 网络详情浮层：控制条只常驻 RTT，其余指标按需展开 */
.net-mask { position: fixed; inset: 0; z-index: 25; }
.net-pop {
  position: absolute; bottom: 34px; left: 0; z-index: 30;
  width: 172px; padding: 10px 12px;
  display: flex; flex-direction: column; gap: 7px;
  background: var(--surface); border: 1px solid var(--line);
  border-radius: 12px; box-shadow: var(--shadow-2);
}
.np-row {
  display: flex; align-items: baseline; gap: 8px;
  font-size: 12px; color: var(--ink-400);
}
.np-row b { margin-left: auto; color: var(--ink-600); font-weight: 600; }
.np-dim { font-size: 11px; color: var(--ink-300); }
.vb-whisper {
  display: inline-flex; align-items: center; gap: 5px; min-width: 0;
  font-size: 12px; color: var(--ink-400, #8A9BB0);
  padding: 3px 9px; border-radius: 99px;
  background: rgba(79, 164, 243, 0.08);
}
.vb-whisper.on {
  color: var(--brand-600); background: rgba(79, 164, 243, 0.16);
}
.vb-wlabel { font-weight: 600; }
.vb-wt {
  max-width: 96px; overflow: hidden; text-overflow: ellipsis; white-space: nowrap;
}
.vb-off { font-size: 10.5px; color: var(--ink-400, #8A9BB0); margin-left: 2px; }
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
.vtag {
  font-size: 11px; font-weight: 400;
  color: var(--ink-400, #8A9BB0); margin-left: 3px; vertical-align: 1px;
}
.me-tag {
  font-size: 11px; font-weight: 600;
  color: var(--brand-600); background: rgba(79, 164, 243, 0.12);
  padding: 1px 6px; border-radius: 6px; margin-left: 5px; vertical-align: 1px;
}
.vbadge {
  display: inline-flex; align-items: center; justify-content: center;
  width: 18px; height: 18px; border-radius: 6px; margin-left: 4px;
  vertical-align: -3px;
}
.vbadge.prio { color: #B7791F; background: rgba(183, 121, 31, 0.12); }
.vbadge.ear { color: var(--brand-600); background: rgba(79, 164, 243, 0.12); }
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
/* per-peer 诊断（#49 B）：与操作项用分隔线区分，数值右对齐 */
.mp-diag {
  margin-top: 6px; padding: 8px 8px 2px;
  border-top: 1px solid var(--line);
  display: flex; flex-direction: column; gap: 6px;
}
.mp-drow {
  display: flex; align-items: baseline; gap: 8px;
  font-size: 11.5px; color: var(--ink-400);
}
.mp-drow b { margin-left: auto; color: var(--ink-600); font-weight: 600; }
</style>
