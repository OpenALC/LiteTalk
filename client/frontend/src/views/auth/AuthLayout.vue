<script setup lang="ts">
// AuthLayout 认证壳：左品牌区（logo+标语+声波装饰）+ 右表单区（铺满窗口，无外层浮卡）。
// 右上角悬浮窗控（最小化/关闭，问题 5）；品牌区/表单空白可拖拽窗口。
// 首装引导：自动探活默认地址，连不上时展开「连接服务器」弹层（全 auth 页面共享一次探测）。
import { onMounted, ref } from 'vue'
import { AudioWaveform, Minus, Server, X } from 'lucide-vue-next'
import { goApp } from '../../api/bridge'
import { closeWithHint } from '../../composables/useWindowClose'
import ServerConnPanel from '../../components/ServerConnPanel.vue'
import Toast from '../../components/Toast.vue'

// 模块级标志：login/register/forgot 间切换 AuthLayout 会重挂载，探测只做一次。
let autoProbed = false

const showConn = ref(false)
const toast = ref<InstanceType<typeof Toast>>()

onMounted(async () => {
  if (autoProbed) return
  autoProbed = true
  // 首启动引导：默认地址连不上 → 自动展开连接弹层
  try {
    const cfg = await goApp().GetServerConfig()
    const r = await goApp().TestServerConnection(cfg.base_url)
    if (!r.ok) showConn.value = true
  } catch {
    showConn.value = true
  }
})
</script>

<template>
  <div class="auth-wrap">
    <!-- 悬浮窗控（问题 5：登录页也能最小化/关闭；close 走问题 4 兜底链） -->
    <div class="win-controls">
      <button title="最小化" @click="goApp().WindowMinimize()"><Minus :size="15" /></button>
      <button class="wc-close" title="关闭" @click="closeWithHint()"><X :size="15" /></button>
    </div>
    <Toast ref="toast" />

    <aside class="brand">
      <div class="brand-top">
        <div class="logo-mark"><AudioWaveform :size="26" /></div>
        <div class="brand-name">轻音<small>LITETALK</small></div>
      </div>
      <div class="brand-mid">
        <p class="brand-tag">轻量、清晰的桌面语音。<br>打开频道，即刻开聊。</p>
        <div class="waves" aria-hidden="true">
          <span v-for="i in 8" :key="i" :style="{ '--i': i - 1 }" />
        </div>
      </div>
      <div class="brand-foot">LiteTalk · OpenALC 开源项目 · MIT License</div>
    </aside>
    <main class="panel">
      <slot />
      <div class="conn-entry">
        <button class="conn-btn" @click="showConn = true">
          <Server :size="14" />连接服务器
        </button>
      </div>
    </main>

    <!-- 连接服务器弹层 -->
    <Transition name="fade">
      <div v-if="showConn" class="conn-overlay" @click.self="showConn = false">
        <div class="conn-modal">
          <div class="cm-head">
            <b>连接服务器</b>
            <button class="cm-close" title="关闭" @click="showConn = false"><X :size="16" /></button>
          </div>
          <p class="cm-sub">填入后端地址，保存后重启生效</p>
          <ServerConnPanel />
        </div>
      </div>
    </Transition>
  </div>
</template>

<style scoped>
/* 铺满窗口：桌面窗口自身即容器，不再套外层浮卡 */
.auth-wrap {
  height: 100vh; display: grid; grid-template-columns: 1fr 460px;
  background: var(--surface);
  /* 整页可拖拽窗口（问题 5b）；交互元素在下方统一 no-drag。
     Wails v2 拖拽属性为 --wails-draggable（WebView2 不认 -webkit-app-region） */
  --wails-draggable: drag;
}
/* 悬浮窗控（问题 5a/5c）：与 MainView TitleBar 同款反馈，不占布局 */
.win-controls {
  position: fixed; top: 0; right: 0; z-index: 40;
  display: flex; --wails-draggable: no-drag;
}
.win-controls button {
  width: 42px; height: 38px;
  display: flex; align-items: center; justify-content: center;
  color: var(--ink-400);
  transition: background 0.15s, color 0.15s;
}
.win-controls button:hover { background: var(--brand-50); color: var(--ink-900); }
.win-controls .wc-close:hover { background: var(--red); color: #fff; }
/* 交互元素一律 no-drag（输入/按钮/链接/勾选/分段/弹层遮罩） */
.auth-wrap :deep(input),
.auth-wrap :deep(button),
.auth-wrap :deep(a),
.auth-wrap :deep(label),
.auth-wrap :deep(select),
.conn-overlay { --wails-draggable: no-drag; }
.brand {
  position: relative; padding: 56px 48px;
  display: flex; flex-direction: column;
  background: linear-gradient(160deg, var(--brand-50), #fff 70%);
}
.brand-top { display: flex; align-items: center; gap: 14px; }
.logo-mark {
  width: 48px; height: 48px; border-radius: 14px;
  background: var(--brand-500);
  display: flex; align-items: center; justify-content: center;
  box-shadow: 0 6px 16px rgba(79, 164, 243, 0.35);
}
.logo-mark :deep(svg) { color: #fff; }
.brand-name { font-size: 20px; font-weight: 700; letter-spacing: 0.5px; }
.brand-name small {
  display: block; font-size: 11px; font-weight: 500;
  color: var(--ink-400); letter-spacing: 2.5px; margin-top: 2px;
}
.brand-mid { flex: 1; display: flex; flex-direction: column; justify-content: center; gap: 28px; }
.brand-tag { font-size: 15px; color: var(--ink-600); line-height: 1.8; text-wrap: pretty; }
.waves { display: flex; align-items: center; gap: 10px; height: 64px; }
.waves span {
  width: 5px; border-radius: 3px; background: var(--brand-200);
  animation: wave 2.4s var(--ease) infinite;
  animation-delay: calc(var(--i) * 0.14s);
}
.waves span:nth-child(3n) { background: var(--brand-500); }
.waves span:nth-child(1) { height: 22px; }
.waves span:nth-child(2) { height: 38px; }
.waves span:nth-child(3) { height: 56px; }
.waves span:nth-child(4) { height: 34px; }
.waves span:nth-child(5) { height: 48px; }
.waves span:nth-child(6) { height: 26px; }
.waves span:nth-child(7) { height: 40px; }
.waves span:nth-child(8) { height: 18px; }
@keyframes wave { 0%, 100% { transform: scaleY(0.45); } 50% { transform: scaleY(1); } }
.brand-foot { font-size: 12px; color: var(--ink-300); }
.panel { padding: 44px 48px; display: flex; flex-direction: column; background: var(--surface); min-width: 0; }

/* 连接服务器入口（表单区底部，克制风格） */
.conn-entry {
  margin-top: auto; padding-top: 18px;
  display: flex; justify-content: center;
}
.conn-btn {
  display: inline-flex; align-items: center; gap: 6px;
  font-size: 12.5px; font-weight: 500; color: var(--ink-400);
  padding: 6px 12px; border-radius: 8px;
  transition: color 0.2s var(--ease), background 0.2s var(--ease);
}
.conn-btn:hover { color: var(--brand-600); background: var(--brand-50); }

/* 弹层 */
.conn-overlay {
  position: fixed; inset: 0; z-index: 50;
  background: rgba(23, 50, 75, 0.28);
  display: flex; align-items: center; justify-content: center;
}
.conn-modal {
  width: 420px; max-width: calc(100vw - 48px);
  background: var(--surface); border-radius: 16px;
  box-shadow: var(--shadow-3); padding: 20px 22px 22px;
}
.cm-head { display: flex; align-items: center; justify-content: space-between; }
.cm-head b { font-size: 15px; }
.cm-close {
  width: 28px; height: 28px; border-radius: 8px;
  display: flex; align-items: center; justify-content: center;
  color: var(--ink-400); transition: background 0.15s, color 0.15s;
}
.cm-close:hover { background: var(--brand-50); color: var(--ink-900); }
.cm-sub { font-size: 12.5px; color: var(--ink-400); margin: 4px 0 14px; }
.fade-enter-active, .fade-leave-active { transition: opacity 0.25s var(--ease); }
.fade-enter-active .conn-modal, .fade-leave-active .conn-modal {
  transition: transform 0.25s var(--ease);
}
.fade-enter-from, .fade-leave-to { opacity: 0; }
.fade-enter-from .conn-modal, .fade-leave-to .conn-modal { transform: translateY(8px) scale(0.97); }

@media (max-width: 880px) {
  .auth-wrap { grid-template-columns: 1fr; height: 100vh; }
  .brand { padding: 20px 24px; flex-direction: row; align-items: center; gap: 12px; }
  .brand-mid, .brand-foot { display: none; }
  .panel { padding: 28px 24px; overflow: auto; }
}
</style>
