<script setup lang="ts">
// App 根组件（v0.1.2 重构）：统一标题栏 + 路由出口 + 全局提示 + 休眠管控。
// - TitleBar 上提：所有页面（主界面/设置/介绍页/鉴权页）共用窗口控制与拖拽区；
//   原 AuthLayout 内悬浮窗控随之上收（避免双份关闭按钮）。
// - 休眠：hibernated 时不渲染路由视图，仅显示占位（UI 销毁开关启用时
//   Go 侧直接 WindowReloadApp，重载后凭守卫 GetMe() 恢复会话）。
// - 原生初始化失败：生产环境等待原生标志超时（bridge 置 nativeInitFailed），
//   渲染错误页替代业务界面——此时绝不进入路由，mock 会让「登录一律成功」。
// - 全局禁用原生右键：捕获阶段拦截，仅 [data-ctx] 容器内的右键可用
//   （频道列表/服务器栏/好友列表/语音成员网格各自提供自定义菜单）。
import { onMounted, onUnmounted, ref } from 'vue'
import TitleBar from './components/TitleBar.vue'
import GlobalToast from './components/GlobalToast.vue'
import { useHibernateStore } from './stores/hibernate'
import { goApp, isNativeInitFailed } from './api/bridge'

const hibernate = useHibernateStore()
// nativeFailed 在 mount 时读取：main.ts 已在 mount 前 await 过 whenNativeReady，
// 模块级失败标志此刻已尘埃落定，无需再等待。
const nativeFailed = ref(isNativeInitFailed())

// onCtxMenu 全局右键拦截：data-ctx 白名单外一律屏蔽原生菜单（走查 7）。
// 捕获阶段执行，先于各组件的 contextmenu 处理。
// 例外：input/textarea/contenteditable 放行——密码框/聊天框的右键粘贴是刚需
//（review S4：一刀切会让所有输入框无法粘贴）。
function onCtxMenu(e: MouseEvent) {
  const t = e.target as HTMLElement | null
  if (!t || typeof t.closest !== 'function') return
  if (t.closest('input, textarea, [contenteditable="true"]')) return
  if (!t.closest('[data-ctx]')) e.preventDefault()
}

// retry 重试：整体重载页面。原生标志若只是迟到（慢启动），重载后即恢复；
// 真正损坏时重载回到本页，用户可经系统途径关闭窗口。
function retry() {
  window.location.reload()
}

onMounted(() => {
  window.addEventListener('contextmenu', onCtxMenu, true)
  if (nativeFailed.value) return
  hibernate.init()
  goApp().GetHibernateMode().then((m) => hibernate.setEnabled(m !== 'off'))
})
onUnmounted(() => {
  window.removeEventListener('contextmenu', onCtxMenu, true)
})
</script>

<template>
  <!-- 原生环境初始化失败：不渲染 TitleBar 与路由（桥接不可用，窗口控制
       按钮会全部失效），仅给出错误说明与重试入口。 -->
  <div v-if="nativeFailed" class="native-failed">
    <div class="nf-card">
      <h1 class="nf-title">原生环境初始化失败</h1>
      <p class="nf-desc">原生桥接未就绪，请重启应用；反复出现请检查安装完整性。</p>
      <button class="nf-retry" @click="retry">重试</button>
    </div>
  </div>
  <div v-else class="app-root">
    <TitleBar />
    <!-- 布局类挂包裹层而非 router-view：router-view 的 class 会透传合并到
         被路由组件的根元素（Vue 作用域 CSS 会把父组件 scope id 打在子根上），
         与 MainView .app 的横向 flex 冲突——v0.1.2 曾因此把主界面挤成竖排。 -->
    <div v-if="!hibernate.hibernated" class="app-main">
      <router-view />
    </div>
    <div v-else class="hibernate-tip">已休眠，从托盘打开窗口即可继续</div>
  </div>
  <GlobalToast />
</template>

<style scoped>
.app-root {
  height: 100vh;
  display: flex; flex-direction: column;
  background: var(--surface);
  /* 硬性防滚动：窗口内容区由内部各面板自行 overflow，任何子树超高
     都不应把整页顶出滚动条（桌面应用窗口无滚动语义）。 */
  overflow: hidden;
}
/* 路由视图占满 TitleBar 之下的剩余空间（MainView/SettingsView 均为 flex 列） */
.app-main { flex: 1; min-height: 0; display: flex; flex-direction: column; }
.hibernate-tip {
  flex: 1;
  display: flex; align-items: center; justify-content: center;
  font-size: 13px; color: var(--ink-300);
  background: var(--surface);
}
/* 原生初始化失败页（对齐登录页视觉：品牌蓝灰 + 居中卡片） */
.native-failed {
  height: 100vh;
  display: flex; align-items: center; justify-content: center;
  background: var(--brand-50);
}
.nf-card {
  width: 340px; padding: 36px 32px;
  background: var(--surface); border-radius: 16px;
  box-shadow: var(--shadow-2);
  text-align: center;
}
.nf-title { font-size: 17px; font-weight: 600; color: var(--ink-900); }
.nf-desc { margin: 12px 0 24px; font-size: 13px; line-height: 1.7; color: var(--ink-400); }
.nf-retry {
  width: 100%; height: 42px; border-radius: 10px;
  background: var(--brand-500); color: #fff;
  font-size: 14px; font-weight: 600;
  transition: background 0.2s var(--ease), transform 0.15s var(--ease);
}
.nf-retry:hover { background: var(--brand-600); }
.nf-retry:active { transform: scale(0.98); }
</style>
