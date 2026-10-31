package api

import (
	"bytes"
	"io"
	"io/fs"
	"log"
	"net/http"
	"path"
	"strings"
	"time"

	"github.com/OpenALC/LiteTalk/server/internal/webui"
)

// Web 客户端静态托管（#50）。路由注册到顶层 mux 的 "GET /" 兜底槽位：
// /api/v1/ 与 GET /ws 的模式更具体，ServeMux 按最长匹配优先，永远先于本
// fallback 命中——不得把 API/WS 吞进 SPA 回退（回归点）。

// webCSP Web 客户端响应的安全头。与前端 vite 构建注入的 <meta> 同值：
// style-src 'unsafe-inline' 是 Vue :style 绑定的硬需求（本批唯一接受的 XSS
// 风险面）；connect-src 放行 ws/wss/https（信令与远程 TURN）。
const webCSP = "default-src 'self'; connect-src 'self' ws: wss: https:; " +
	"img-src 'self' data: blob:; media-src 'self' blob:; style-src 'self' 'unsafe-inline'"

// webIndexCacheControl HTML 入口不缓存：每次部署后深链与刷新都要立刻拿到新壳。
const webIndexCacheControl = "no-cache"

// webAssetCacheControl 带 hash 的 /assets/* 产物可永久缓存。
const webAssetCacheControl = "public, max-age=31536000, immutable"

// zeroTime http.ServeContent 的固定 ModTime（embed 产物没有有意义的修改时间）。
var zeroTime = time.Unix(0, 0)

// RegisterStatic 把 Web 客户端产物挂到 mux 的根路径（幂等，可重复调用）。
//
// 注册用无方法限定 "/" 而非 "GET /"：ServeMux 的冲突规则要求更具体的模式在
// 「方法×路径」两个维度上都更具体才不打架——"GET /" 与既有 methodless 的
// "/api/v1/" 在 GET /api/v1/* 上双方各赢一维，注册直接 panic。"/"（全方法）
// 是 "/api/v1/" 与 "GET /ws" 的严格超集，后两者按最具体优先照常赢，fallback
// 只兜真正未注册的路径；非 GET/HEAD 请求落到本 handler 用 405 拒绝。
//
// dist 无产物（只有 .gitkeep）时路由照样注册，但所有请求都 404：
// 开发/测试构建的服务端保持「纯 API/WS」形态，不吐空壳 HTML。
func RegisterStatic(mux *http.ServeMux, lg *log.Logger) {
	sub, err := fs.Sub(webui.Dist(), "dist")
	if err != nil {
		// embed 布局是编译期固定的，运行期不可能失败；防御性兜底仅记日志。
		lg.Printf("webui: 内嵌产物不可用: %v", err)
		return
	}
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			writeErr(w, http.StatusMethodNotAllowed, "method_not_allowed", "请求方法不允许")
			return
		}
		serveWebUI(w, r, sub, lg)
	})
}

// serveWebUI 分发一个静态请求：
//   - /assets/*（构建产物，文件名含 hash）：直出，缺失 404——错误路径不回
//     HTML 壳，否则坏资源会以 200 + text/html 形式静默失败；
//   - 其余未知路径（SPA 深链，如 /join/<code>）与 /：回 index.html；
//   - dist 无产物：一律 404。
func serveWebUI(w http.ResponseWriter, r *http.Request, sub fs.FS, lg *log.Logger) {
	h := w.Header()
	h.Set("Content-Security-Policy", webCSP)
	h.Set("X-Content-Type-Options", "nosniff")
	// Referer 不外泄：页面全同源，对第三方跳转（附件新开页）收敛来源信息。
	h.Set("Referrer-Policy", "no-referrer")

	p := path.Clean("/" + r.URL.Path)
	if strings.HasPrefix(p, "/assets/") {
		h.Set("Cache-Control", webAssetCacheControl)
		f, err := sub.Open(strings.TrimPrefix(p, "/"))
		if err != nil {
			writeErr(w, http.StatusNotFound, "not_found", "资源不存在")
			return
		}
		defer f.Close()
		if stat, serr := f.Stat(); serr != nil || stat.IsDir() {
			writeErr(w, http.StatusNotFound, "not_found", "资源不存在")
			return
		}
		serveFSFile(w, r, path.Base(p), f)
		return
	}

	h.Set("Cache-Control", webIndexCacheControl)
	index, err := sub.Open("index.html")
	if err != nil {
		// dist 无产物（.gitkeep 占位构建）：按 404 处理，保持纯 API 形态。
		lg.Printf("webui: index.html 不存在（发布包未包含前端产物?）: %v", err)
		writeErr(w, http.StatusNotFound, "not_found", "资源不存在")
		return
	}
	defer index.Close()
	if stat, serr := index.Stat(); serr != nil || stat.IsDir() {
		writeErr(w, http.StatusNotFound, "not_found", "资源不存在")
		return
	}
	serveFSFile(w, r, "index.html", index)
}

// serveFSFile 输出 embed 产物。embed 的文件实现 io.ReadSeeker，直接走
// ServeContent（带 Range/Content-Type 推断）；类型不符时整体读入兜底。
func serveFSFile(w http.ResponseWriter, r *http.Request, name string, f fs.File) {
	if rs, ok := f.(io.ReadSeeker); ok {
		http.ServeContent(w, r, name, zeroTime, rs)
		return
	}
	data, err := io.ReadAll(f)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal", "服务器内部错误")
		return
	}
	http.ServeContent(w, r, name, zeroTime, bytes.NewReader(data))
}
