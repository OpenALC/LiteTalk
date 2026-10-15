import { createRouter, createWebHashHistory } from 'vue-router'
import MainView from '../views/MainView.vue'
import SettingsView from '../views/SettingsView.vue'
import LoginView from '../views/auth/LoginView.vue'
import RegisterView from '../views/auth/RegisterView.vue'
import ForgotView from '../views/auth/ForgotView.vue'
import { useAuthStore } from '../stores/auth'
import { goApp } from '../api/bridge'

// Wails 嵌入场景使用 hash 路由，避免刷新 404。
const router = createRouter({
  history: createWebHashHistory(),
  routes: [
    { path: '/', name: 'main', component: MainView },
    { path: '/settings', name: 'settings', component: SettingsView },
    { path: '/login', name: 'login', component: LoginView },
    { path: '/register', name: 'register', component: RegisterView },
    { path: '/forgot', name: 'forgot', component: ForgotView },
    { path: '/:pathMatch(.*)*', redirect: '/' },
  ],
})

// 鉴权守卫：未登录先尝试 GetMe() 会话恢复——UI 销毁重载（WindowReloadApp）
// 后内存态丢失但 Go 侧令牌仍在，恢复成功即免重登（v0.1.2）。
// 恢复失败才落登录页；已登录访问鉴权页时回主界面。
router.beforeEach(async (to) => {
  const auth = useAuthStore()
  const authPages = ['login', 'register', 'forgot']
  if (!auth.loggedIn && !authPages.includes(to.name as string)) {
    try {
      auth.user = await goApp().GetMe()
      auth.loggedIn = true
    } catch {
      return { name: 'login' }
    }
  }
  if (auth.loggedIn && authPages.includes(to.name as string)) {
    return { name: 'main' }
  }
})

export default router
