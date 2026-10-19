package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func mkChange(id, op, path, text string, level int) change {
	return change{ID: id, Operation: op, Path: path, Text: text, Level: level}
}

func mkEntry(id, op, path, text string) entry {
	return entry{ID: id, Operation: op, Path: path, Text: text, Line: 1}
}

const (
	idText  = "the `id` response's property `type/format` changed from `integer/int64` to `string` for status `200`"
	optText = "the response property `message` became optional for the status `400`"
)

func TestEvaluateAllowsExactMatch(t *testing.T) {
	changes := []change{mkChange("response-property-type-changed", "GET", "/api/v1/x", idText, levelErr)}
	entries := []entry{mkEntry("response-property-type-changed", "GET", "/api/v1/x", idText)}

	ev := evaluate(changes, entries, levelErr)
	if len(ev.unapproved) != 0 || len(ev.allowed) != 1 || len(ev.stale) != 0 {
		t.Fatalf("精确命中应放行：unapproved=%d allowed=%d stale=%d", len(ev.unapproved), len(ev.allowed), len(ev.stale))
	}
}

func TestEvaluateRequiresFullTupleMatch(t *testing.T) {
	cases := []struct {
		name  string
		entry entry
	}{
		{"描述不同", mkEntry("response-property-type-changed", "GET", "/api/v1/x", idText+"（旧描述）")},
		{"方法不同", mkEntry("response-property-type-changed", "POST", "/api/v1/x", idText)},
		{"路径不同", mkEntry("response-property-type-changed", "GET", "/api/v1/y", idText)},
		{"check id 不同", mkEntry("response-property-became-optional", "GET", "/api/v1/x", idText)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			changes := []change{mkChange("response-property-type-changed", "GET", "/api/v1/x", idText, levelErr)}
			ev := evaluate(changes, []entry{tc.entry}, levelErr)
			if len(ev.unapproved) != 1 {
				t.Fatalf("四元组不完全相等必须判为未批准，实际 unapproved=%d", len(ev.unapproved))
			}
			if len(ev.allowed) != 0 {
				t.Fatalf("不应命中允许清单，实际 allowed=%d", len(ev.allowed))
			}
			if len(ev.stale) != 1 {
				t.Fatalf("未命中的条目应报 stale，实际 stale=%d", len(ev.stale))
			}
		})
	}
}

func TestEvaluateWarnDoesNotBlock(t *testing.T) {
	changes := []change{mkChange("response-property-type-changed", "GET", "/api/v1/x", idText, levelWarn)}
	ev := evaluate(changes, nil, levelErr)
	if len(ev.unapproved) != 0 || len(ev.warnings) != 1 {
		t.Fatalf("WARN 低于门槛不应阻塞：unapproved=%d warnings=%d", len(ev.unapproved), len(ev.warnings))
	}
}

func TestEvaluateStaleEntryIsReportedNotFatal(t *testing.T) {
	entries := []entry{mkEntry("response-property-type-changed", "GET", "/api/v1/old", idText)}
	ev := evaluate(nil, entries, levelErr)
	if len(ev.stale) != 1 {
		t.Fatalf("未命中的允许清单条目应提示清理，实际 stale=%d", len(ev.stale))
	}
	if len(ev.unapproved) != 0 {
		t.Fatalf("stale 不允许导致失败，实际 unapproved=%d", len(ev.unapproved))
	}
}

func TestLoadAllowlistCommentsCRLFAndDedup(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "allow.txt")
	line := "response-property-type-changed\tGET\t/api/v1/x\t" + idText
	body := "# 注释\r\n\r\n  \r\n" + line + "\r\n"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	entries, err := loadAllowlist(path)
	if err != nil {
		t.Fatalf("合法清单不应报错: %v", err)
	}
	if len(entries) != 1 || entries[0].ID != "response-property-type-changed" {
		t.Fatalf("解析结果不符: %+v", entries)
	}
	if entries[0].Line != 4 {
		t.Fatalf("行号应为 4（含注释/空行），实际 %d", entries[0].Line)
	}

	if err := os.WriteFile(path, []byte(line+"\n"+line+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := loadAllowlist(path); err == nil || !strings.Contains(err.Error(), "重复") {
		t.Fatalf("重复条目必须报错，实际 err=%v", err)
	}
}

func TestLoadAllowlistRejectsMalformed(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "allow.txt")

	for _, body := range []string{
		"只有一列\n",
		"a\tb\tc\n",          // 3 列
		"a\tb\tc\td\te\n",    // 5 列
		"a\tb\tc\td\nmore\n", // 第 2 行格式错误
	} {
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := loadAllowlist(path); err == nil {
			t.Fatalf("格式错误应报错: %q", body)
		}
	}
}

func TestLoadChangesValidatesRequiredFields(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "changes.json")

	if err := os.WriteFile(path, []byte(`[]`), 0o600); err != nil {
		t.Fatal(err)
	}
	if cs, err := loadChanges(path); err != nil || len(cs) != 0 {
		t.Fatalf("空数组应解析成功: cs=%v err=%v", cs, err)
	}

	if err := os.WriteFile(path, []byte(`[{"id":"x","operation":"GET","path":"/a"}]`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := loadChanges(path); err == nil || !strings.Contains(err.Error(), "缺少") {
		t.Fatalf("缺字段应报错，实际 err=%v", err)
	}
}

// TestRunEndToEnd 覆盖退出码、完整列表输出与 GitHub 注解。
func TestRunEndToEnd(t *testing.T) {
	dir := t.TempDir()
	changesPath := filepath.Join(dir, "changes.json")
	allowPath := filepath.Join(dir, "allow.txt")

	if err := os.WriteFile(changesPath, []byte(`[
		{"id":"response-property-type-changed","text":"`+idText+`","level":3,"operation":"GET","path":"/api/v1/x"},
		{"id":"response-property-became-optional","text":"`+optText+`","level":3,"operation":"POST","path":"/api/v1/y"}
	]`), 0o600); err != nil {
		t.Fatal(err)
	}
	// 第二行是本次未命中的条目：应提示清理但不影响退出码。
	if err := os.WriteFile(allowPath, []byte(
		"response-property-type-changed\tGET\t/api/v1/x\t"+idText+"\n"+
			"response-property-type-changed\tGET\t/api/v1/old\t"+idText+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	var out, errOut bytes.Buffer
	code := run([]string{
		"-changes", changesPath,
		"-allowlist", allowPath,
		"-baseline", "测试基线",
	}, &out, &errOut)
	if code != 1 {
		t.Fatalf("存在未批准项应返回 1，实际 %d；stderr=%s", code, errOut.String())
	}
	logs := out.String()
	for _, want := range []string{
		"测试基线",
		"未批准 1",
		"[已批准]",
		"[未批准]",
		"::error::未批准的破坏性变更 response-property-became-optional POST /api/v1/y",
		"第 2 行",
	} {
		if !strings.Contains(logs, want) {
			t.Errorf("日志缺少 %q\n---\n%s", want, logs)
		}
	}

	// 全部命中时应返回 0。
	if err := os.WriteFile(allowPath, []byte(
		"response-property-type-changed\tGET\t/api/v1/x\t"+idText+"\n"+
			"response-property-became-optional\tPOST\t/api/v1/y\t"+optText+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	out.Reset()
	if code := run([]string{"-changes", changesPath, "-allowlist", allowPath}, &out, &errOut); code != 0 {
		t.Fatalf("全部命中应返回 0，实际 %d；日志=%s", code, out.String())
	}
	if !strings.Contains(out.String(), "检查通过") {
		t.Fatalf("通过时应输出结论，实际：%s", out.String())
	}
}

func TestRunUsage(t *testing.T) {
	var out, errOut bytes.Buffer
	if code := run(nil, &out, &errOut); code != 2 {
		t.Fatalf("缺参数应返回 2，实际 %d", code)
	}
	if !strings.Contains(errOut.String(), "用法") {
		t.Fatalf("缺参数应打印用法，实际：%s", errOut.String())
	}
}
