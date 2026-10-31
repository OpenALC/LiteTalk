import { defineConfig } from 'vite'
import vue from '@vitejs/plugin-vue'

// wails3 dev 经 WAILS_VITE_PORT 注入 vite 端口（wails v3 dev.go 默认 9245）；
// strictPort 防止端口被占用时 vite 静默换端口、导致 Go 侧连不上 dev server。
const port = process.env.WAILS_VITE_PORT ? Number(process.env.WAILS_VITE_PORT) : 9245

// CSP 头（#50）：只注入 web 构建（--mode web）。桌面构建由 Wails 资产通道
// 加载，注入 meta CSP 反而可能拦掉 webview 内部资源，所以桌面路径原样返回。
// style-src 'unsafe-inline' 是 Vue :style 绑定的硬需求——这是本批唯一接受的
// XSS 风险面（CSP 其余方向仍锁 'self'）。
const WEB_CSP = [
  "default-src 'self'",
  "connect-src 'self' ws: wss: https:",
  "img-src 'self' data: blob:",
  "media-src 'self' blob:",
  "style-src 'self' 'unsafe-inline'",
].join('; ')

function webOnlyCSP(): import('vite').Plugin {
  // transformIndexHtml 的 ctx 不带 mode：从 configResolved 捕获后再判别。
  let isWeb = false
  return {
    name: 'web-only-csp',
    apply: 'build',
    configResolved(cfg) {
      isWeb = cfg.mode === 'web'
    },
    transformIndexHtml(html) {
      if (!isWeb) return html
      return html.replace(
        '<head>',
        `<head>\n    <meta http-equiv="Content-Security-Policy" content="${WEB_CSP}">`,
      )
    },
  }
}

// Wails 嵌入产物需使用相对路径引用资源；web 构建（#50）由服务端托管，
// 必须绝对根路径 base（SPA 深链 /join/<code> 才能解析到 /assets/*）。
export default defineConfig(({ mode }) => ({
  plugins: [vue(), webOnlyCSP()],
  base: mode === 'web' ? '/' : './',
  server: {
    port,
    strictPort: true,
    // 必须绑 IPv4 回环：wails v3 Windows 的 asset 代理把 dev server 地址定为
    // wails.localhost（解析到 127.0.0.1），而 vite 默认按 localhost 解析只绑
    // [::1]（IPv6）——WebView2 请求 IPv4 会被拒（connectex 主动拒绝），
    // 表现为"应用启动了但页面永远加载不出"。
    host: '127.0.0.1',
  },
}))
