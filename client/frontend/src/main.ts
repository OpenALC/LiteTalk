import { createApp } from 'vue'
import { createPinia } from 'pinia'
import App from './App.vue'
import router from './router'
import './style.css'

// 前端 console/异常转发到后端日志（%AppData%\LiteTalk\app.log），便于排障：
// 拦截 console.log/info/warn/error、window.onerror、unhandledrejection，
// 经 bridge 的 goApp() 转发（v3 走 Call.ByName，与 UI 同通道）。
// 非 Wails 环境（浏览器 mock）静默忽略；转发失败一律吞掉，绝不回灌 console。
import { goApp, isNative } from './api/bridge'

function installConsoleBridge() {
  const send = (level: string, args: unknown[]) => {
    if (!isNative()) return
    const text = args
      .map((a) => {
        if (typeof a === 'string') return a
        try {
          return JSON.stringify(a)
        } catch {
          return String(a)
        }
      })
      .join(' ')
    try {
      goApp().LogFromWeb(level, text).catch(() => {})
    } catch {
      /* 绑定不可用，忽略 */
    }
  }

  const orig = {
    log: console.log.bind(console),
    info: console.info.bind(console),
    warn: console.warn.bind(console),
    error: console.error.bind(console),
  }
  console.log = (...a: unknown[]) => { orig.log(...a); send('log', a) }
  console.info = (...a: unknown[]) => { orig.info(...a); send('info', a) }
  console.warn = (...a: unknown[]) => { orig.warn(...a); send('warn', a) }
  console.error = (...a: unknown[]) => { orig.error(...a); send('error', a) }

  window.addEventListener('error', (e) => {
    send('error', [`window.onerror: ${e.message} @ ${e.filename}:${e.lineno}:${e.colno}`])
  })
  window.addEventListener('unhandledrejection', (e) => {
    send('error', [`unhandledrejection: ${String(e.reason)}`])
  })
}
installConsoleBridge()

createApp(App).use(createPinia()).use(router).mount('#app')
