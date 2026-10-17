import { defineConfig } from 'vite'
import vue from '@vitejs/plugin-vue'

// wails3 dev 经 WAILS_VITE_PORT 注入 vite 端口（wails v3 dev.go 默认 9245）；
// strictPort 防止端口被占用时 vite 静默换端口、导致 Go 侧连不上 dev server。
const port = process.env.WAILS_VITE_PORT ? Number(process.env.WAILS_VITE_PORT) : 9245

// Wails 嵌入产物需使用相对路径引用资源
export default defineConfig({
  plugins: [vue()],
  base: './',
  server: {
    port,
    strictPort: true,
    // 必须绑 IPv4 回环：wails v3 Windows 的 asset 代理把 dev server 地址定为
    // wails.localhost（解析到 127.0.0.1），而 vite 默认按 localhost 解析只绑
    // [::1]（IPv6）——WebView2 请求 IPv4 会被拒（connectex 主动拒绝），
    // 表现为"应用启动了但页面永远加载不出"。
    host: '127.0.0.1',
  },
})
