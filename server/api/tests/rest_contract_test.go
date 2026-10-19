package tests

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/getkin/kin-openapi/openapi3"
)

// contractExemptions 契约外路由的显式豁免（issue #77 收编中）。
// main.go 挂的两条不在 openapi.yaml 里（healthz 被部署脚本依赖，路径与方法都不能变）。
// 本表必须恰好等于「实现有、契约无」的差集：#77 收编完成后不删表就会红，反向提醒收口。
var contractExemptions = map[string]string{
	"GET /api/v1/healthz":          "健康检查挂 main.go，#77 收编前豁免",
	"POST /api/v1/rtc/credentials": "TURN 短期凭证挂 main.go，#77 收编前豁免",
}

// specOperations 从 openapi.yaml 提取 (method, path) 全集——契约侧，schema 驱动。
func specOperations(t *testing.T, path string) map[string]bool {
	t.Helper()
	loader := openapi3.NewLoader()
	doc, err := loader.LoadFromFile(path)
	if err != nil {
		t.Fatalf("加载 %s: %v", path, err)
	}
	if doc.Paths == nil {
		t.Fatalf("%s 没有 paths", path)
	}
	out := map[string]bool{}
	for p, item := range doc.Paths.Map() {
		if item == nil {
			continue
		}
		for method := range item.Operations() {
			out[strings.ToUpper(method)+" "+p] = true
		}
	}
	return out
}

// splitPattern 把 "METHOD /path" 拆开；无空格是子树挂载（method 为空）。
func splitPattern(pat string) (method, path string) {
	if i := strings.Index(pat, " "); i >= 0 {
		return strings.ToUpper(pat[:i]), pat[i+1:]
	}
	return "", pat
}

// restOps 过滤出 REST 操作面：有 method 且路径在 /api/ 下。
// 子树挂载只允许 /api/v1/（strict-server 的挂载点）；非 /api/ 路径（GET /ws）
// 归信令契约面（api/signal/），不在 OpenAPI 比对范围。
func restOps(t *testing.T, routes []routeInfo) map[string]routeInfo {
	t.Helper()
	out := map[string]routeInfo{}
	for _, r := range routes {
		method, path := splitPattern(r.Pattern)
		if method == "" {
			if strings.HasPrefix(path, "/api/") && path != "/api/v1/" {
				t.Errorf("契约外的子树挂载 %s（%s）：要么补进 openapi.yaml，要么显式豁免", r.Pattern, r.Position)
			}
			continue
		}
		if !strings.HasPrefix(path, "/api/") {
			continue
		}
		key := method + " " + path
		if prev, dup := out[key]; dup {
			t.Errorf("路由重复注册 %s（%s 与 %s）", key, prev.Position, r.Position)
		}
		out[key] = r
	}
	return out
}

// TestRESTContractRouteParity 契约与路由双向比对（§14 最低要求 1）。
// 实现加了路由但契约没写 → 红；契约写了但没实现 → 红。
// 实现侧 = AST 全量提取（覆盖生成面与 main.go 手写面），不是 grep 数 pattern。
func TestRESTContractRouteParity(t *testing.T) {
	root := findRoot(t)
	spec := specOperations(t, filepath.Join(root, "api", "openapi.yaml"))
	impl := restOps(t, astRoutes(t, filepath.Join(root, "server")))

	for key := range impl {
		if _, ok := spec[key]; ok {
			continue
		}
		if reason, ok := contractExemptions[key]; ok {
			t.Logf("契约外豁免生效：%s（%s）", key, reason)
			continue
		}
		r := impl[key]
		t.Errorf("实现注册了但契约没有：%s（%s）——补进 api/openapi.yaml 或加豁免", key, r.Position)
	}
	for key := range spec {
		if _, ok := impl[key]; !ok {
			t.Errorf("契约声明了但实现没注册：%s——补路由或改契约", key)
		}
	}
	// 豁免表必须恰好有效：条目失效（进了契约或已删路由）都要求立刻收口，不留过期豁免。
	for key, reason := range contractExemptions {
		_, inImpl := impl[key]
		_, inSpec := spec[key]
		switch {
		case !inImpl:
			t.Errorf("豁免项已不在实现里：%s（%s）——请从 contractExemptions 删除", key, reason)
		case inSpec:
			t.Errorf("豁免项已进契约：%s（%s）——#77 收编完成，请从 contractExemptions 删除", key, reason)
		}
	}
	if t.Failed() {
		t.Logf("规模参考：契约 %d 个操作，实现 %d 个操作（/api/ 面）", len(spec), len(impl))
	}
	t.Logf("契约 %d 个操作 ↔ 实现 %d 个操作（/api/ 面），豁免 %d 条",
		len(spec), len(impl), len(contractExemptions))
}

// TestGeneratedRoutesSeenByAST 运行时注册表 ⊆ AST 提取：两个提取器互证。
// 生成器换了 pattern 拼接写法而 evalPattern 没跟上时，这里第一时间红，
// 免得 REST 比对拿着残缺的实现集合得出假结论。
func TestGeneratedRoutesSeenByAST(t *testing.T) {
	root := findRoot(t)
	astSet := map[string]bool{}
	for _, r := range astRoutes(t, filepath.Join(root, "server")) {
		astSet[r.Pattern] = true
	}
	gen := generatedRoutes()
	if len(gen) == 0 {
		t.Fatal("运行时注册表为空：HandlerFromMux 没注册任何路由")
	}
	for _, r := range gen {
		if !astSet[r.Pattern] {
			t.Errorf("运行时注册了但 AST 提取缺失：%s——evalPattern 需要跟上生成器写法", r.Pattern)
		}
	}
}
