package main

import (
	"encoding/json"
	"fmt"
	"strings"
)

// writeStruct 生成一个 Go 结构体。
func writeStruct(b *strings.Builder, td *typeDef) error {
	if err := validate(td); err != nil {
		return err
	}
	if td.Description != "" {
		fmt.Fprintf(b, "// %s %s\n", td.Name, oneLine(td.Description))
	} else {
		fmt.Fprintf(b, "// %s 由 api/signal 生成。\n", td.Name)
	}
	fmt.Fprintf(b, "type %s struct {\n", td.Name)
	for _, p := range td.Properties {
		goType, err := resolveType(p.P)
		if err != nil {
			return fmt.Errorf("%s.%s: %w", td.Name, p.Name, err)
		}
		tag := p.Name
		if p.P.GoOmitEmpty {
			tag += ",omitempty"
		}
		fmt.Fprintf(b, "%s %s `json:%q`", goName(p.Name), goType, tag)
		if p.P.Description != "" {
			fmt.Fprintf(b, " // %s", oneLine(p.P.Description))
		}
		b.WriteString("\n")
	}
	b.WriteString("}\n\n")
	return nil
}

// validate required 字段必须在 properties 里声明。
func validate(td *typeDef) error {
	have := map[string]bool{}
	for _, p := range td.Properties {
		have[p.Name] = true
	}
	for r := range td.Required {
		if !have[r] {
			return fmt.Errorf("%s: required 字段 %q 未在 properties 声明", td.Name, r)
		}
	}
	return nil
}

// resolveType 属性 → Go 类型。优先级：x-go-type > $ref > array/object > 标量。
func resolveType(p propSchema) (string, error) {
	if p.GoType != "" {
		if p.GoPointer && !strings.HasPrefix(p.GoType, "*") {
			return "*" + p.GoType, nil
		}
		return p.GoType, nil
	}
	if p.Ref != "" {
		name := refName(p.Ref)
		if p.GoPointer {
			return "*" + name, nil
		}
		return name, nil
	}
	switch p.Type {
	case "array":
		item, err := itemType(p.Items)
		if err != nil {
			return "", err
		}
		return "[]" + item, nil
	case "object":
		return "map[string]any", nil
	case "string":
		return scalar(p, "string"), nil
	case "integer":
		// int64：与现状 TS 字段、time.UnixMilli() 赋值一致；JSON 整数无界，int 不可移植。
		return scalar(p, "int64"), nil
	case "number":
		return scalar(p, "float64"), nil
	case "boolean":
		return scalar(p, "bool"), nil
	}
	return "", fmt.Errorf("无法推导 Go 类型（type=%q）", p.Type)
}

func scalar(p propSchema, base string) string {
	if p.GoPointer {
		return "*" + base
	}
	return base
}

// itemType 数组元素类型：$ref → 组件名；空/无约束 → any。
func itemType(raw []byte) (string, error) {
	s := strings.TrimSpace(string(raw))
	if s == "" || s == "{}" {
		return "any", nil
	}
	var it struct {
		Ref  string `json:"$ref"`
		Type string `json:"type"`
	}
	if err := json.Unmarshal([]byte(s), &it); err != nil {
		return "", err
	}
	if it.Ref != "" {
		return refName(it.Ref), nil
	}
	switch it.Type {
	case "":
		return "any", nil
	case "string":
		return "string", nil
	case "integer":
		return "int64", nil
	case "number":
		return "float64", nil
	case "boolean":
		return "bool", nil
	case "object":
		return "map[string]any", nil
	}
	return "", fmt.Errorf("无法推导数组元素类型（type=%q）", it.Type)
}

// refName $ref → Go 类型名：components.schema.json#/$defs/UserBrief → UserBrief。
func refName(ref string) string {
	if i := strings.IndexByte(ref, '#'); i >= 0 {
		ref = ref[i+1:]
	}
	ref = strings.TrimPrefix(ref, "/$defs/")
	ref = strings.TrimPrefix(ref, "$defs/")
	if i := strings.LastIndexByte(ref, '/'); i >= 0 {
		ref = ref[i+1:]
	}
	return ref
}

// oneLine 把多行描述压成单行注释。
func oneLine(s string) string {
	return strings.Join(strings.Fields(s), " ")
}
