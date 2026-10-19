package tests

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"net/http"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/OpenALC/LiteTalk/server/internal/api"
)

// routeInfo 一条注册点：pattern 原文（"METHOD /path" 或纯路径挂载）+ 溯源位置。
type routeInfo struct {
	Pattern  string
	Position string // 文件:行，失败信息用
	FromGen  bool   // 由生成器的运行时注册表确认过
}

// recordingMux 实现 api.ServeMux，只登记 HandleFunc 的 pattern，不真正路由。
// 这是「运行时注册表」提取：生成面（api.gen.go 的 HandlerWithOptions）注册什么就记什么，
// 不解析源码文本，生成器换了拼接写法也不受影响。
type recordingMux struct {
	seen []routeInfo
}

func (r *recordingMux) HandleFunc(pattern string, _ func(http.ResponseWriter, *http.Request)) {
	r.seen = append(r.seen, routeInfo{Pattern: pattern, FromGen: true})
}

func (r *recordingMux) ServeHTTP(http.ResponseWriter, *http.Request) {}

// generatedRoutes 经 HandlerFromMux 拿生成路由全集。
// si 传 nil：注册阶段只存 handler 闭包，不调用 ServerInterface 方法。
func generatedRoutes() []routeInfo {
	rec := &recordingMux{}
	api.HandlerFromMux(nil, rec)
	return rec.seen
}

// astRoutes AST 扫描 server/ 全部非测试 .go 文件的 Handle/HandleFunc 注册点。
// 生成文件也扫（生成器写的 pattern 是拼接表达式，靠 evalPattern 静态求值），
// 这样「实现注册了什么」与源码一一对应，main.go 里挂在契约外的路由也能看见。
// 求值失败直接报错：不静默跳过，防止新的注册写法悄悄绕过比对。
func astRoutes(t *testing.T, serverDir string) []routeInfo {
	t.Helper()
	var out []routeInfo
	fset := token.NewFileSet()
	err := filepath.WalkDir(serverDir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if d.Name() == "testdata" {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		file, err := parser.ParseFile(fset, path, nil, parser.SkipObjectResolution)
		if err != nil {
			return fmt.Errorf("解析 %s: %w", path, err)
		}
		rel, _ := filepath.Rel(serverDir, path)
		found, err := scanHandleCalls(fset, rel, file)
		if err != nil {
			return err
		}
		out = append(out, found...)
		return nil
	})
	if err != nil {
		t.Fatalf("AST 扫描 %s: %v", serverDir, err)
	}
	return out
}

// scanHandleCalls 提取单文件里 Handle/HandleFunc 的 pattern 实参。
// 闭包参数栈用于识别「转发型注册」（authMux.HandleFunc 把调用方给的 pattern 原样下传）——
// 那不是路由起点，真正的注册点在传 literal 的调用方。
//
// 帧同步：Inspect 对每个入栈节点都配一次 f(nil)，所以对每个非 nil 节点记一个
// 「本次是否进了函数帧」的标记，f(nil) 时对称弹出；返回 false 的节点没有 f(nil)，
// 也就不能入栈（见 CallExpr 求值失败分支）。
func scanHandleCalls(fset *token.FileSet, rel string, file *ast.File) ([]routeInfo, error) {
	var out []routeInfo
	var scanErr error
	var paramStack []map[string]bool
	var entered []bool
	paramsOf := func(fn *ast.FuncType) map[string]bool {
		set := map[string]bool{}
		if fn != nil && fn.Params != nil {
			for _, f := range fn.Params.List {
				for _, n := range f.Names {
					set[n.Name] = true
				}
			}
		}
		return set
	}
	inParams := func(name string) bool {
		for _, set := range paramStack {
			if set[name] {
				return true
			}
		}
		return false
	}
	ast.Inspect(file, func(n ast.Node) bool {
		if n == nil {
			top := len(entered) - 1
			if top >= 0 {
				if entered[top] {
					paramStack = paramStack[:len(paramStack)-1]
				}
				entered = entered[:top]
			}
			return true
		}
		pushed := false
		switch n := n.(type) {
		case *ast.FuncDecl:
			paramStack = append(paramStack, paramsOf(n.Type))
			pushed = true
		case *ast.FuncLit:
			paramStack = append(paramStack, paramsOf(n.Type))
			pushed = true
		case *ast.CallExpr:
			sel, ok := n.Fun.(*ast.SelectorExpr)
			if !ok || len(n.Args) == 0 {
				break
			}
			if sel.Sel.Name != "Handle" && sel.Sel.Name != "HandleFunc" {
				break
			}
			// 转发型：pattern 是本函数（或外层函数）的形参，注册点在调用方。
			if id, ok := n.Args[0].(*ast.Ident); ok && inParams(id.Name) {
				break
			}
			pat, err := evalPattern(n.Args[0])
			if err != nil {
				// 新的注册写法必须先让比对器认识它，绝不静默放过。
				scanErr = fmt.Errorf("%s:%d pattern 无法静态求值: %w",
					rel, fset.Position(n.Pos()).Line, err)
				return false // 未入栈，无需弹帧
			}
			out = append(out, routeInfo{
				Pattern:  pat,
				Position: fmt.Sprintf("%s:%d", rel, fset.Position(n.Pos()).Line),
			})
		}
		entered = append(entered, pushed)
		return true
	})
	return out, scanErr
}

// httpMethods net/http 的方法常量（生成器写 pattern 时用 http.MethodX 拼接）。
var httpMethods = map[string]string{
	"MethodGet":     http.MethodGet,
	"MethodPost":    http.MethodPost,
	"MethodPut":     http.MethodPut,
	"MethodPatch":   http.MethodPatch,
	"MethodDelete":  http.MethodDelete,
	"MethodHead":    http.MethodHead,
	"MethodOptions": http.MethodOptions,
	"MethodTrace":   http.MethodTrace,
}

// evalPattern 静态求值 pattern 表达式。覆盖三种真实写法：
// 字面量（手写路由）、字符串拼接 + http.MethodX（api.gen.go）、BaseURL 参与拼接。
// BaseURL 在本仓库恒为空（router.go 的 StdHTTPServerOptions 不设）；
// 若哪天设了非空前缀，运行时注册表那侧会与静态结果对不上，测试会红。
func evalPattern(e ast.Expr) (string, error) {
	switch e := e.(type) {
	case *ast.BasicLit:
		if e.Kind != token.STRING {
			return "", fmt.Errorf("非常量字面量 %v", e.Kind)
		}
		return strconv.Unquote(e.Value)
	case *ast.ParenExpr:
		return evalPattern(e.X)
	case *ast.BinaryExpr:
		if e.Op != token.ADD {
			return "", fmt.Errorf("不支持的运算 %v", e.Op)
		}
		l, err := evalPattern(e.X)
		if err != nil {
			return "", err
		}
		r, err := evalPattern(e.Y)
		if err != nil {
			return "", err
		}
		return l + r, nil
	case *ast.SelectorExpr:
		if e.Sel.Name == "BaseURL" {
			return "", nil
		}
		if id, ok := e.X.(*ast.Ident); ok && id.Name == "http" {
			if m, ok := httpMethods[e.Sel.Name]; ok {
				return m, nil
			}
		}
		return "", fmt.Errorf("不支持的选择器 %v.%v", e.X, e.Sel.Name)
	}
	return "", fmt.Errorf("不支持的表达式 %T", e)
}
