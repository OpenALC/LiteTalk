package main

import (
	"fmt"
	"go/format"
	"sort"
	"strings"
)

// emit 生成 protocol_gen.go 全文（gofmt 已排版）。
// 输出与 package 无关：两侧文件字节相同（两边都是 package signal）。
func emit(cat *catalog, comps map[string]*typeDef, msgs []*typeDef) ([]byte, error) {
	var b strings.Builder
	b.WriteString(genHeader)

	fmt.Fprintf(&b, "// 协议版本（api/signal/catalog.json）。v2 起含网关事件面与 RFC 6464 说话状态；v3 起含同频道悄悄话与优先发言。\n")
	fmt.Fprintf(&b, "const (\n\tProtocolVersionMin = %d\n\tProtocolVersionMax = %d\n\tProtocolVersionCurrent = %d\n)\n\n",
		cat.ProtocolVersions.Min, cat.ProtocolVersions.Max, cat.ProtocolVersions.Current)

	b.WriteString("// 信令消息类型（wire 值见 api/signal/messages/*.schema.json 的 x-wire-type）。\nconst (\n")
	for _, m := range msgs {
		fmt.Fprintf(&b, "%s = %q // %s\n", m.GoConst, m.WireType, dirLabel(m))
	}
	b.WriteString(")\n\n")

	b.WriteString("// 下行 error 消息的 code 取值（api/signal/catalog.json error_codes）。\nconst (\n")
	for _, c := range cat.ErrorCodes {
		writeStrCode(&b, c)
	}
	b.WriteString(")\n\n")

	b.WriteString("// WS close code（4000~4999 为应用自定义区间；api/signal/catalog.json close_codes）。\nconst (\n")
	for _, c := range cat.CloseCodes {
		if c.Comment != "" {
			fmt.Fprintf(&b, "// %s %s\n", c.GoName, c.Comment)
		}
		fmt.Fprintf(&b, "%s = websocket.StatusCode(%d)\n", c.GoName, c.Value)
	}
	b.WriteString(")\n\n")

	for _, c := range cat.KickReasons {
		if c.Comment != "" {
			fmt.Fprintf(&b, "// %s %s\n", c.GoName, c.Comment)
		}
		fmt.Fprintf(&b, "const %s = %q\n\n", c.GoName, c.Value)
	}

	b.WriteString("// ---- 共享类型与各消息 data 结构（api/signal/components + messages） ----\n\n")

	names := make([]string, 0, len(comps))
	for n := range comps {
		names = append(names, n)
	}
	sort.Strings(names)
	emitted := map[string]bool{}
	for _, n := range names {
		if err := writeStruct(&b, comps[n]); err != nil {
			return nil, err
		}
		emitted[comps[n].Name] = true
	}
	for _, m := range msgs {
		if m.NoPayload || m.Ref != "" || emitted[m.Name] {
			continue
		}
		if err := writeStruct(&b, m); err != nil {
			return nil, err
		}
		emitted[m.Name] = true
	}

	src, err := format.Source([]byte(b.String()))
	if err != nil {
		return nil, fmt.Errorf("生成物语法错误: %w\n----\n%s", err, b.String())
	}
	return src, nil
}

// dirLabel 消息常量的尾注：方向 + since（since 1 从简不标）。
func dirLabel(m *typeDef) string {
	d := map[string]string{"C2S": "C→S", "S2C": "S→C", "BOTH": "双向"}[m.Direction]
	if d == "" {
		d = m.Direction
	}
	if m.Since > 1 {
		return fmt.Sprintf("%s · since %d", d, m.Since)
	}
	return d
}

// writeStrCode 写一个字符串常量（含可选注释）。
func writeStrCode(b *strings.Builder, c strCode) {
	if c.Comment != "" {
		fmt.Fprintf(b, "// %s %s\n", c.GoName, c.Comment)
	}
	fmt.Fprintf(b, "%s = %q\n", c.GoName, c.Value)
}
