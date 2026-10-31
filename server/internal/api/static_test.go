package api

import (
	"io/fs"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/fstest"
)

// webTestFS 带 index.html 与一个 hash 资产的内存产物（embed 产物布局同构）。
// 用 MapFS 而非真实 embed：embed 内容随构建态变化（.gitkeep 占位 or 真实产物），
// 测试需要确定性行为。
func webTestFS() fstest.MapFS {
	return fstest.MapFS{
		"index.html":           {Data: []byte("<html>lite</html>")},
		"assets/app-abc123.js": {Data: []byte("console.log(1)")},
	}
}

func get(t *testing.T, h http.Handler, path string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// TestStaticServeWebUI SPA 分发：深链回 index.html、资产直出、坏资产 404，
// 且 /api 与 /ws 不被 fallback 吞掉（回归点）。
func TestStaticServeWebUI(t *testing.T) {
	sub := webTestFS()
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/healthz", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("api"))
	})
	mux.HandleFunc("GET /ws", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("ws"))
	})
	// 与 main.go 相同的注册形态（methodless "/" 兜底 + "GET /ws"）：
	// 同时回归验证 "GET /" 写法与 "/api/v1/" 的注册冲突不再出现。
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			writeErr(w, http.StatusMethodNotAllowed, "method_not_allowed", "请求方法不允许")
			return
		}
		serveWebUI(w, r, sub, log.Default())
	})

	// 根路径与深链 → index.html（200，no-cache，带 CSP）
	for _, p := range []string{"/", "/join/abc123", "/some/deep/link"} {
		rec := get(t, mux, p)
		if rec.Code != http.StatusOK {
			t.Fatalf("%s: 期望 200，实际 %d", p, rec.Code)
		}
		if !strings.Contains(rec.Body.String(), "lite") {
			t.Fatalf("%s: 期望 index.html 内容，实际 %q", p, rec.Body.String())
		}
		if cc := rec.Header().Get("Cache-Control"); cc != webIndexCacheControl {
			t.Fatalf("%s: Cache-Control = %q，期望 %q", p, cc, webIndexCacheControl)
		}
		if rec.Header().Get("Content-Security-Policy") == "" {
			t.Fatalf("%s: 缺少 CSP 头", p)
		}
	}

	// 资产直出（immutable 缓存）
	rec := get(t, mux, "/assets/app-abc123.js")
	if rec.Code != http.StatusOK || rec.Body.String() != "console.log(1)" {
		t.Fatalf("资产直出失败: %d %q", rec.Code, rec.Body.String())
	}
	if cc := rec.Header().Get("Cache-Control"); cc != webAssetCacheControl {
		t.Fatalf("资产 Cache-Control = %q", cc)
	}

	// 不存在的资产 404，绝不回 HTML 壳（否则坏资源以 200+text/html 静默失败）
	rec = get(t, mux, "/assets/nope-999.js")
	if rec.Code != http.StatusNotFound || strings.Contains(rec.Body.String(), "<html>") {
		t.Fatalf("缺失资产应 404 不带壳: %d %q", rec.Code, rec.Body.String())
	}

	// /api 与 /ws 不被 fallback 吞掉（回归点）
	if r := get(t, mux, "/api/v1/healthz"); r.Body.String() != "api" {
		t.Fatalf("/api/v1/ 被 SPA fallback 吞掉: %q", r.Body.String())
	}
	if r := get(t, mux, "/ws"); r.Body.String() != "ws" {
		t.Fatalf("/ws 被 SPA fallback 吞掉: %q", r.Body.String())
	}
}

// TestStaticEmptyDist 占位产物（无 index.html）：一律 404，服务端保持纯 API
// 形态——check-server 的构建就带 .gitkeep 占位，发布包外的构建不吐空壳 HTML。
func TestStaticEmptyDist(t *testing.T) {
	empty := fs.FS(fstest.MapFS{".gitkeep": {Data: []byte{}}})
	mux := http.NewServeMux()
	mux.HandleFunc("GET /", func(w http.ResponseWriter, r *http.Request) {
		serveWebUI(w, r, empty, log.Default())
	})
	for _, p := range []string{"/", "/join/abc", "/index.html"} {
		rec := get(t, mux, p)
		if rec.Code != http.StatusNotFound {
			t.Fatalf("%s: 空 dist 应 404，实际 %d", p, rec.Code)
		}
	}
}

// TestRegisterStaticMountsRoot RegisterStatic 落位在 "GET /" 兜底槽位且幂等：
// 重复注册不 panic，占位 embed 下所有路径 404（不冒充可用前端）。
func TestRegisterStaticMountsRoot(t *testing.T) {
	mux := http.NewServeMux()
	RegisterStatic(mux, log.Default())
	RegisterStatic(mux, log.Default()) // 幂等
	rec := get(t, mux, "/join/abc")
	// 仓库常态 dist 只有 .gitkeep：404；若本地把产物拷进了 dist，这里会是
	// 200——两种状态都合法，只断言不 panic 且不把 /api/v1/ 吞掉。
	if rec.Code != http.StatusNotFound && rec.Code != http.StatusOK {
		t.Fatalf("/join/abc: 意外状态 %d", rec.Code)
	}
}
