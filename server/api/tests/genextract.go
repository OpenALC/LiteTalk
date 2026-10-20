package tests

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"strconv"
)

// genSymbols 从 protocol_gen.go 这类生成物里抽出的符号表（go/ast，不 grep）。
type genSymbols struct {
	Strings map[string]string // 字符串常量名 → 值
	Ints    map[string]int64  // 整型常量名 → 值
	Types   map[string]bool   // 类型声明名
}

// parseGenFile 解析生成物的顶层常量与类型。
// 整型常量允许 websocket.StatusCode(4000) 这种「单参转换调用」写法（close code 现状）。
func parseGenFile(path string) (*genSymbols, error) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, path, nil, parser.SkipObjectResolution)
	if err != nil {
		return nil, fmt.Errorf("解析 %s: %w", path, err)
	}
	out := &genSymbols{
		Strings: map[string]string{},
		Ints:    map[string]int64{},
		Types:   map[string]bool{},
	}
	for _, decl := range file.Decls {
		switch d := decl.(type) {
		case *ast.GenDecl:
			switch d.Tok {
			case token.CONST:
				for _, spec := range d.Specs {
					vs := spec.(*ast.ValueSpec)
					for i, name := range vs.Names {
						if i >= len(vs.Values) {
							return nil, fmt.Errorf("%s: 常量 %s 无显式值", path, name.Name)
						}
						if err := out.addConst(name.Name, vs.Values[i]); err != nil {
							return nil, fmt.Errorf("%s: %w", path, err)
						}
					}
				}
			case token.TYPE:
				for _, spec := range d.Specs {
					ts := spec.(*ast.TypeSpec)
					out.Types[ts.Name.Name] = true
				}
			}
		}
	}
	return out, nil
}

// addConst 录入单个常量：字符串或整型（含 StatusCode(n) 转换）。
// 其他类型（布尔、浮点）在生成物里不存在，出现即报错——不静默吞。
func (g *genSymbols) addConst(name string, val ast.Expr) error {
	if call, ok := val.(*ast.CallExpr); ok {
		if len(call.Args) != 1 {
			return fmt.Errorf("常量 %s 的转换调用参数不为 1", name)
		}
		val = call.Args[0]
	}
	switch v := val.(type) {
	case *ast.BasicLit:
		switch v.Kind {
		case token.STRING:
			s, err := strconv.Unquote(v.Value)
			if err != nil {
				return fmt.Errorf("常量 %s 解引号失败: %w", name, err)
			}
			g.Strings[name] = s
			return nil
		case token.INT:
			n, err := strconv.ParseInt(v.Value, 0, 64)
			if err != nil {
				return fmt.Errorf("常量 %s 解析整型失败: %w", name, err)
			}
			g.Ints[name] = n
			return nil
		}
		return fmt.Errorf("常量 %s 是不支持的字面量 %v", name, v.Kind)
	}
	return fmt.Errorf("常量 %s 的值不是字面量（%T）——生成物被手改？", name, val)
}
