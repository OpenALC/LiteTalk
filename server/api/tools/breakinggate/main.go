// breakinggate 判定 oasdiff 的破坏性变更输出（#86）：只放行
// api/oasdiff-allowlist.txt 中精确列出的已批准条目，其余新增项一律失败。
//
// 用法（CI 与本地 dry-run 都走 server/api/tests/run_breaking_check.sh）：
//
//	oasdiff breaking <base> api/openapi.yaml --format json --flatten-allof > changes.json
//	go run ./api/tools/breakinggate -changes changes.json -allowlist ../api/oasdiff-allowlist.txt
//
// 匹配键是 check id + HTTP 方法 + 路径 + 描述四元组，不做前缀/通配：同一路径上
// 描述不同的新破坏性变更仍会失败，避免允许清单吞掉任意变更。
//
// 退出码：0 = 无未批准的破坏性变更；1 = 存在未批准项；2 = 输入或用法错误。
package main

import (
	"bufio"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
)

// oasdiff 的 level 数值：1=INFO，2=WARN，3=ERR（--flatten-allof 后一般为 3）。
const (
	levelInfo = 1
	levelWarn = 2
	levelErr  = 3

	// stalePrintLimit 限制「允许清单未命中」明细行数：release tag 成为基线后
	// 旧条目会整批失效，逐条打印只会淹没真正的 breaking 列表。
	stalePrintLimit = 20
)

// change 对应 oasdiff breaking --format json 数组中的一条；只取判定需要的字段。
type change struct {
	ID          string `json:"id"`
	Text        string `json:"text"`
	Level       int    `json:"level"`
	Operation   string `json:"operation"`
	OperationID string `json:"operationId"`
	Path        string `json:"path"`
	Fingerprint string `json:"fingerprint"`
}

func (c change) levelName() string {
	switch {
	case c.Level >= levelErr:
		return "ERR"
	case c.Level >= levelWarn:
		return "WARN"
	default:
		return "INFO"
	}
}

// entry 是允许清单中的一行；line 用于报表定位（注释/空行不计）。
type entry struct {
	ID        string
	Operation string
	Path      string
	Text      string
	Line      int
}

func (e entry) key() string { return matchKey(e.ID, e.Operation, e.Path, e.Text) }

func matchKey(id, operation, path, text string) string {
	// \x00 不会出现在 YAML 描述里，作为分隔符可避免字段内容拼接歧义。
	return id + "\x00" + operation + "\x00" + path + "\x00" + text
}

// evaluation 是一次比对的分类结果。
type evaluation struct {
	allowed    []change // 命中允许清单且达到阻塞级别
	warnings   []change // 未达到阻塞级别（默认 WARN/INFO，不失败）
	unapproved []change // 达到阻塞级别但不在允许清单中——CI 因它失败
	stale      []entry  // 允许清单里本次未命中的条目（提示清理，不失败）
	total      int
}

// loadChanges 读取 oasdiff JSON。空文件或 [] 均表示无变更。
func loadChanges(path string) ([]change, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("读取 oasdiff 输出: %w", err)
	}
	var changes []change
	if err := json.Unmarshal(raw, &changes); err != nil {
		return nil, fmt.Errorf("解析 oasdiff 输出 %s: %w", path, err)
	}
	for i, c := range changes {
		if c.ID == "" || c.Operation == "" || c.Path == "" || c.Text == "" {
			return nil, fmt.Errorf("oasdiff 输出第 %d 条缺少 id/operation/path/text: %+v", i+1, c)
		}
	}
	return changes, nil
}

// loadAllowlist 解析允许清单：制表符分隔 4 列，空行与 # 注释忽略，重复条目报错。
// 允许清单是安全边界，格式错误不能静默跳过——否则会变成「什么都没允许」或
// 「允许了预期之外的东西」两种难排查的失败。
func loadAllowlist(path string) ([]entry, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("打开允许清单: %w", err)
	}
	defer f.Close()

	var entries []entry
	seen := make(map[string]int)
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64*1024), 1024*1024) // 单条描述可能较长
	for lineNo := 1; sc.Scan(); lineNo++ {
		// Windows 本地 checkout 可能带 CRLF（.gitattributes 没覆盖 *.txt）。
		line := strings.TrimSuffix(sc.Text(), "\r")
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		parts := strings.Split(line, "\t")
		if len(parts) != 4 {
			return nil, fmt.Errorf("%s:%d: 需要 4 个制表符分隔的列（check id / 方法 / 路径 / 描述），实际 %d 列",
				path, lineNo, len(parts))
		}
		e := entry{ID: parts[0], Operation: parts[1], Path: parts[2], Text: parts[3], Line: lineNo}
		if prev, ok := seen[e.key()]; ok {
			return nil, fmt.Errorf("%s:%d: 与第 %d 行重复", path, lineNo, prev)
		}
		seen[e.key()] = lineNo
		entries = append(entries, e)
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("读取允许清单: %w", err)
	}
	return entries, nil
}

// evaluate 将变更分为允许/警告/未批准三类，并找出未命中的允许清单条目。
// failLevel 为阻塞门槛：level >= failLevel 的变更必须命中允许清单。
func evaluate(changes []change, entries []entry, failLevel int) evaluation {
	var ev evaluation
	ev.total = len(changes)

	byKey := make(map[string]int, len(entries))
	for i, e := range entries {
		byKey[e.key()] = i
	}
	used := make([]bool, len(entries))

	sorted := make([]change, len(changes))
	copy(sorted, changes)
	sort.SliceStable(sorted, func(i, j int) bool {
		a, b := sorted[i], sorted[j]
		if a.ID != b.ID {
			return a.ID < b.ID
		}
		if a.Path != b.Path {
			return a.Path < b.Path
		}
		if a.Operation != b.Operation {
			return a.Operation < b.Operation
		}
		return a.Text < b.Text
	})

	for _, c := range sorted {
		if c.Level < failLevel {
			ev.warnings = append(ev.warnings, c)
			continue
		}
		if idx, ok := byKey[matchKey(c.ID, c.Operation, c.Path, c.Text)]; ok {
			used[idx] = true
			ev.allowed = append(ev.allowed, c)
			continue
		}
		ev.unapproved = append(ev.unapproved, c)
	}
	for i, e := range entries {
		if !used[i] {
			ev.stale = append(ev.stale, e)
		}
	}
	return ev
}

// report 输出完整 breaking 列表与允许清单命中情况（#86 要求 CI 日志可审计）。
func report(w io.Writer, ev evaluation, baseline string) {
	if baseline != "" {
		fmt.Fprintf(w, "oasdiff 基线: %s\n", baseline)
	}
	fmt.Fprintf(w, "oasdiff 破坏性变更: %d 条（允许清单命中 %d，警告 %d，未批准 %d）\n",
		ev.total, len(ev.allowed), len(ev.warnings), len(ev.unapproved))

	if len(ev.allowed) > 0 {
		fmt.Fprintln(w, "\n[已批准] 命中 api/oasdiff-allowlist.txt：")
		for _, c := range ev.allowed {
			fmt.Fprintf(w, "  %-4s %s %s %s :: %s\n", c.levelName(), c.ID, c.Operation, c.Path, c.Text)
		}
	}
	if len(ev.warnings) > 0 {
		fmt.Fprintf(w, "\n[警告] 低于阻塞级别（%s），不失败：\n", "level < ERR")
		for _, c := range ev.warnings {
			fmt.Fprintf(w, "  %-4s %s %s %s :: %s\n", c.levelName(), c.ID, c.Operation, c.Path, c.Text)
		}
	}
	if len(ev.unapproved) > 0 {
		fmt.Fprintln(w, "\n[未批准] 需要 issue 批准并加入允许清单：")
		for _, c := range ev.unapproved {
			fmt.Fprintf(w, "  %-4s %s %s %s :: %s\n", c.levelName(), c.ID, c.Operation, c.Path, c.Text)
			// GitHub Actions 注解：PR/日志里直接定位到这一条。
			fmt.Fprintf(w, "::error::未批准的破坏性变更 %s %s %s: %s\n", c.ID, c.Operation, c.Path, c.Text)
		}
	}
	if len(ev.stale) > 0 {
		fmt.Fprintf(w, "\n[提示] 允许清单有 %d 条本次未命中（基线切换或契约回退后可清理）：\n", len(ev.stale))
		shown := len(ev.stale)
		if shown > stalePrintLimit {
			shown = stalePrintLimit
		}
		for _, e := range ev.stale[:shown] {
			fmt.Fprintf(w, "  第 %d 行: %s %s %s :: %s\n", e.Line, e.ID, e.Operation, e.Path, e.Text)
		}
		if len(ev.stale) > shown {
			fmt.Fprintf(w, "  ...其余 %d 条省略（同上，可清理）\n", len(ev.stale)-shown)
		}
	}

	switch {
	case len(ev.unapproved) > 0:
		fmt.Fprintf(w, "\n::error::契约出现 %d 条未批准的破坏性变更——如确属有意，先在 issue 中批准，"+
			"再把精确条目加入 api/oasdiff-allowlist.txt（条目必须引用该 issue）\n", len(ev.unapproved))
	default:
		fmt.Fprintf(w, "\n契约破坏性变更检查通过：%d 条全部在允许清单中。\n", len(ev.allowed))
	}
}

// run 是 main 的可测试入口；返回进程退出码。
func run(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("breakinggate", flag.ContinueOnError)
	fs.SetOutput(stderr)
	changesPath := fs.String("changes", "", "oasdiff breaking --format json 的输出文件")
	allowPath := fs.String("allowlist", "", "已批准条目清单（制表符分隔 4 列）")
	baseline := fs.String("baseline", "", "基线描述，仅用于日志（如 release tag 或快照文件）")
	failLevel := fs.Int("fail-level", levelErr, "阻塞门槛：level >= 该值必须命中允许清单（1=INFO 2=WARN 3=ERR）")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *changesPath == "" || *allowPath == "" {
		fmt.Fprintln(stderr, "用法: breakinggate -changes <oasdiff.json> -allowlist <allowlist.txt> [-baseline <描述>]")
		return 2
	}

	changes, err := loadChanges(*changesPath)
	if err != nil {
		fmt.Fprintf(stderr, "breakinggate: %v\n", err)
		return 2
	}
	entries, err := loadAllowlist(*allowPath)
	if err != nil {
		fmt.Fprintf(stderr, "breakinggate: %v\n", err)
		return 2
	}

	ev := evaluate(changes, entries, *failLevel)
	report(stdout, ev, *baseline)
	if len(ev.unapproved) > 0 {
		return 1
	}
	return 0
}

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}
