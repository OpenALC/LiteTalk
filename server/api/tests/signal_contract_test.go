package tests

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// messageSchema messages/*.schema.json 的最小面：消息全集与 Go 命名都由 schema 驱动。
type messageSchema struct {
	Title      string `json:"title"`
	XWireType  string `json:"x-wire-type"`
	XGoConst   string `json:"x-go-const"`
	XGoPayload string `json:"x-go-payload"` // "none" 表示空载荷，不生成 Go 类型
}

// catalogConst catalog.json 里的命名常量条目（value 有字符串/整数两种）。
type catalogConst struct {
	GoName string          `json:"go_name"`
	Value  json.RawMessage `json:"value"`
}

type catalogDoc struct {
	ProtocolVersions struct {
		Min     int64 `json:"min"`
		Max     int64 `json:"max"`
		Current int64 `json:"current"`
	} `json:"protocol_versions"`
	CloseCodes  []catalogConst `json:"close_codes"`
	ErrorCodes  []catalogConst `json:"error_codes"`
	KickReasons []catalogConst `json:"kick_reasons"`
}

func (c catalogConst) strValue(t *testing.T) string {
	t.Helper()
	var s string
	if err := json.Unmarshal(c.Value, &s); err != nil {
		t.Fatalf("catalog 常量 %s 的 value 不是字符串: %v", c.GoName, err)
	}
	return s
}

func (c catalogConst) intValue(t *testing.T) int64 {
	t.Helper()
	var n int64
	if err := json.Unmarshal(c.Value, &n); err != nil {
		t.Fatalf("catalog 常量 %s 的 value 不是整数: %v", c.GoName, err)
	}
	return n
}

// loadMessages 读 api/signal/messages/*.schema.json，返回消息全集。
func loadMessages(t *testing.T, dir string) []messageSchema {
	t.Helper()
	files, err := filepath.Glob(filepath.Join(dir, "*.schema.json"))
	if err != nil || len(files) == 0 {
		t.Fatalf("glob %s: %v（消息 schema 为空？）", dir, err)
	}
	out := make([]messageSchema, 0, len(files))
	for _, f := range files {
		raw, err := os.ReadFile(f)
		if err != nil {
			t.Fatalf("读 %s: %v", f, err)
		}
		var m messageSchema
		if err := json.Unmarshal(raw, &m); err != nil {
			t.Fatalf("解析 %s: %v", f, err)
		}
		if m.XWireType == "" || m.XGoConst == "" {
			t.Errorf("%s 缺 x-wire-type / x-go-const——消息 schema 必须自带 wire 值与 Go 常量名", f)
			continue
		}
		out = append(out, m)
	}
	return out
}

// loadCatalog 读 api/signal/catalog.json（错误码/close code/踢线原因/协议版本）。
func loadCatalog(t *testing.T, path string) catalogDoc {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("读 %s: %v", path, err)
	}
	var c catalogDoc
	if err := json.Unmarshal(raw, &c); err != nil {
		t.Fatalf("解析 %s: %v", path, err)
	}
	return c
}

// normGenFile 去掉 package 行后返回正文：两份生成物除包名外必须逐字一致（§0.5）。
func normGenFile(t *testing.T, path string) string {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("读 %s: %v", path, err)
	}
	if !bytes.Contains(raw, []byte("DO NOT EDIT")) {
		t.Errorf("%s 没有 DO NOT EDIT 头——生成物头被改掉了", path)
	}
	var b strings.Builder
	for _, line := range strings.Split(string(raw), "\n") {
		if strings.HasPrefix(line, "package ") {
			continue
		}
		b.WriteString(line)
		b.WriteString("\n")
	}
	return b.String()
}

// strConsts 按前缀筛选生成物的字符串常量。
func (g *genSymbols) strConsts(prefix string) map[string]string {
	out := map[string]string{}
	for k, v := range g.Strings {
		if strings.HasPrefix(k, prefix) {
			out[k] = v
		}
	}
	return out
}

// intConsts 按前缀筛选生成物的整型常量。
func (g *genSymbols) intConsts(prefix string) map[string]int64 {
	out := map[string]int64{}
	for k, v := range g.Ints {
		if strings.HasPrefix(k, prefix) {
			out[k] = v
		}
	}
	return out
}

// diffStrConsts 双向比对「契约期望 ↔ 生成物」的字符串常量集合。
func diffStrConsts(t *testing.T, what string, want, got map[string]string) {
	t.Helper()
	for name, wv := range want {
		gv, ok := got[name]
		if !ok {
			t.Errorf("%s：%s 生成物缺失（契约有）", what, name)
			continue
		}
		if gv != wv {
			t.Errorf("%s：%s 生成物值 %q ≠ 契约值 %q", what, name, gv, wv)
		}
	}
	for name := range got {
		if _, ok := want[name]; !ok {
			t.Errorf("%s：%s 生成物多出（契约没有）——绕过 Schema 改生成物？", what, name)
		}
	}
}

// diffIntConsts 双向比对整型常量集合。
func diffIntConsts(t *testing.T, what string, want, got map[string]int64) {
	t.Helper()
	for name, wv := range want {
		gv, ok := got[name]
		if !ok {
			t.Errorf("%s：%s 生成物缺失（契约有）", what, name)
			continue
		}
		if gv != wv {
			t.Errorf("%s：%s 生成物值 %d ≠ 契约值 %d", what, name, gv, wv)
		}
	}
	for name := range got {
		if _, ok := want[name]; !ok {
			t.Errorf("%s：%s 生成物多出（契约没有）——绕过 Schema 改生成物？", what, name)
		}
	}
}

// componentTypes 读 components.schema.json 的 $defs 标题（共享类型名）。
func componentTypes(t *testing.T, path string) map[string]bool {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("读 %s: %v", path, err)
	}
	var doc struct {
		Defs map[string]struct {
			Title string `json:"title"`
		} `json:"$defs"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("解析 %s: %v", path, err)
	}
	out := map[string]bool{}
	for name, def := range doc.Defs {
		if def.Title == "" {
			t.Errorf("%s: $defs.%s 缺 title——生成器靠 title 定 Go 类型名", path, name)
			continue
		}
		out[def.Title] = true
	}
	return out
}

// TestSignalGenMatchesContract 信令防手改（§14 最低要求 2）。
// api/signal/ 的消息全集与常量表 ↔ 两侧 protocol_gen.go，双向必须一致：
// 只改生成物不动 Schema、或只改 Schema 不重新生成，都会在这里红。
// 全集来自 messages/*.schema.json 与 catalog.json，不硬编码消息清单。
func TestSignalGenMatchesContract(t *testing.T) {
	root := findRoot(t)
	signalDir := filepath.Join(root, "api", "signal")
	msgs := loadMessages(t, filepath.Join(signalDir, "messages"))
	cat := loadCatalog(t, filepath.Join(signalDir, "catalog.json"))

	wantMsg := map[string]string{}
	wantTypes := componentTypes(t, filepath.Join(signalDir, "components.schema.json"))
	for _, m := range msgs {
		wantMsg[m.XGoConst] = m.XWireType
		// x-go-payload=none 的空载荷消息不生成 Go 类型（如 session.voice_taken）。
		if m.XGoPayload != "none" && m.Title != "" {
			wantTypes[m.Title] = true
		}
	}
	wantErr := map[string]string{}
	wantKick := map[string]string{}
	for _, c := range cat.ErrorCodes {
		wantErr[c.GoName] = c.strValue(t)
	}
	for _, c := range cat.KickReasons {
		wantKick[c.GoName] = c.strValue(t)
	}
	wantClose := map[string]int64{}
	for _, c := range cat.CloseCodes {
		wantClose[c.GoName] = c.intValue(t)
	}
	// ProtocolVersion* 三个名字是生成器固定输出（不是增长集合），值取 catalog。
	wantVer := map[string]int64{
		"ProtocolVersionMin":     cat.ProtocolVersions.Min,
		"ProtocolVersionMax":     cat.ProtocolVersions.Max,
		"ProtocolVersionCurrent": cat.ProtocolVersions.Current,
	}

	sides := []struct {
		name string
		path string
	}{
		{"server", filepath.Join(root, "server", "internal", "signal", "protocol_gen.go")},
		{"client", filepath.Join(root, "client", "internal", "signal", "protocol_gen.go")},
	}
	for _, side := range sides {
		t.Run(side.name, func(t *testing.T) {
			g, err := parseGenFile(side.path)
			if err != nil {
				t.Fatal(err)
			}
			diffStrConsts(t, "消息类型", wantMsg, g.strConsts("Type"))
			diffStrConsts(t, "错误码", wantErr, g.strConsts("ErrCode"))
			diffStrConsts(t, "踢线原因", wantKick, g.strConsts("KickReason"))
			diffIntConsts(t, "close code", wantClose, g.intConsts("CloseCode"))
			diffIntConsts(t, "协议版本", wantVer, g.intConsts("ProtocolVersion"))
			gotTypes := map[string]bool{}
			for name := range g.Types {
				gotTypes[name] = true
			}
			for name := range wantTypes {
				if !gotTypes[name] {
					t.Errorf("类型：%s 生成物缺失（schema title 有）", name)
				}
			}
			for name := range gotTypes {
				if !wantTypes[name] {
					t.Errorf("类型：%s 生成物多出（schema title 没有）——绕过 Schema 改生成物？", name)
				}
			}
		})
	}
}

// TestSignalGenSidesIdentical 两侧 protocol_gen.go 除 package 行外逐字一致（§0.5 信令双侧一致性）。
// 生成器对两侧输出同一份内容；不一致说明有一侧被单独手改，或生成器跑了一半。
func TestSignalGenSidesIdentical(t *testing.T) {
	root := findRoot(t)
	serverSide := normGenFile(t, filepath.Join(root, "server", "internal", "signal", "protocol_gen.go"))
	clientSide := normGenFile(t, filepath.Join(root, "client", "internal", "signal", "protocol_gen.go"))
	if serverSide != clientSide {
		// 只报差异存在，不把两份 200 行文件灌进日志；定位用 diff 命令。
		t.Errorf("两侧 protocol_gen.go 正文不一致（除 package 行）：\n  diff server/internal/signal/protocol_gen.go client/internal/signal/protocol_gen.go\n  server %d 行，client %d 行",
			strings.Count(serverSide, "\n"), strings.Count(clientSide, "\n"))
	}
}
