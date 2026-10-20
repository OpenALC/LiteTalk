package store

import (
	"io/fs"
	"reflect"
	"strings"
	"testing"

	"github.com/OpenALC/LiteTalk/server/migrations"
)

// mustSplit 切分并断言无错误（现有迁移与常规语法的测试用）。
func mustSplit(t *testing.T, s string) []string {
	t.Helper()
	stmts, err := splitSQL(s)
	if err != nil {
		t.Fatalf("splitSQL 报错: %v（原文 %q）", err, s)
	}
	return stmts
}

// normStmts 折叠句内空白，比较“含注释 / 去注释”两版切分结果是否等价。
func normStmts(stmts []string) []string {
	out := make([]string, len(stmts))
	for i, s := range stmts {
		out[i] = strings.Join(strings.Fields(s), " ")
	}
	return out
}

// TestSplitSQLCommentSemicolon #75：注释里的半角分号不得把语句切碎，
// 切分结果必须与去掉注释后的版本一致。
func TestSplitSQLCommentSemicolon(t *testing.T) {
	const with = `--顶部注释; 第一段; 第二段
CREATE TABLE t (
  id INTEGER -- 主键; 不要在这里切
);
/* 块注释; 也有 */
INSERT INTO t VALUES (1); -- 尾注;还有
#井号注释; 也有
INSERT INTO t VALUES (2);
`
	const without = `
CREATE TABLE t (
  id INTEGER
);
INSERT INTO t VALUES (1);
INSERT INTO t VALUES (2);
`
	got, want := mustSplit(t, with), mustSplit(t, without)
	if !reflect.DeepEqual(normStmts(got), normStmts(want)) {
		t.Fatalf("含注释与去注释切分不一致\ngot:  %q\nwant: %q", normStmts(got), normStmts(want))
	}
	if len(got) != 3 {
		t.Fatalf("应切出 3 条语句，实际 %d: %q", len(got), got)
	}
}

// TestSplitSQLStringLiteral 字符串与引号标识符内的 ;、-- 原样保留，不切分不剥离。
func TestSplitSQLStringLiteral(t *testing.T) {
	const raw = "INSERT INTO t VALUES ('a;b', 'x -- y');\n" +
		"INSERT INTO t VALUES (\"c;d\", `e;f`);\n" +
		"UPDATE t SET note = 'it''s; ok' WHERE id = 1;\n"
	got := mustSplit(t, raw)
	if len(got) != 3 {
		t.Fatalf("应切出 3 条语句，实际 %d: %q", len(got), got)
	}
	if got[0] != "INSERT INTO t VALUES ('a;b', 'x -- y')" {
		t.Fatalf("第一条被切坏: %q", got[0])
	}
	if !strings.Contains(got[1], "`e;f`") || !strings.Contains(got[2], "'it''s; ok'") {
		t.Fatalf("引号内容被改写: %q", got)
	}
}

// TestSplitSQLEscapesAndIdentifiers #75 P2：MySQL 反斜杠转义与 SQLite 方括号
// 标识符内的分号不切分；反引号标识符没有反斜杠转义（必须是反例，否则测试
// 无法区分「把 \ 当成通用转义」的错误实现）。
func TestSplitSQLEscapesAndIdentifiers(t *testing.T) {
	cases := []struct {
		name  string
		raw   string
		want  int
		first string // 空串 = 不校验首条内容
	}{
		{
			name:  "单引号反斜杠转义",
			raw:   `INSERT INTO t VALUES ('it\'s; ok'); SELECT 1;`,
			want:  2,
			first: `INSERT INTO t VALUES ('it\'s; ok')`,
		},
		{
			name:  "双引号反斜杠转义",
			raw:   `INSERT INTO t VALUES ("say \"hi; there\""); SELECT 2;`,
			want:  2,
			first: `INSERT INTO t VALUES ("say \"hi; there\"")`,
		},
		{
			name:  "反斜杠转义反斜杠后的分号仍在串内",
			raw:   `INSERT INTO t VALUES ('C:\\tmp;x'); SELECT 3;`,
			want:  2,
			first: `INSERT INTO t VALUES ('C:\\tmp;x')`,
		},
		{
			// 反引号里 \ 不是转义符：`a\` 在第二个反引号闭合，后面的 ; 正常切分。
			name:  "反引号无反斜杠转义",
			raw:   "SELECT `a\\`; SELECT 4;",
			want:  2,
			first: "SELECT `a\\`",
		},
		{
			name:  "方括号标识符内的分号不切分",
			raw:   `SELECT * FROM [a;b]; SELECT 5;`,
			want:  2,
			first: `SELECT * FROM [a;b]`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := mustSplit(t, tc.raw)
			if len(got) != tc.want {
				t.Fatalf("应切出 %d 条语句，实际 %d: %q", tc.want, len(got), got)
			}
			if tc.first != "" && got[0] != tc.first {
				t.Fatalf("首条被切坏: %q, want %q", got[0], tc.first)
			}
		})
	}
}

// TestSplitSQLUnterminated 未闭合的引号 / 方括号 / 块注释必须报错：
// 静默把剩余内容当作合法 SQL 会让迁移少执行语句却看似成功。
func TestSplitSQLUnterminated(t *testing.T) {
	cases := []struct {
		name string
		raw  string
	}{
		{"单引号", "SELECT 'abc"},
		{"双引号", `SELECT "abc`},
		{"反引号", "SELECT `abc"},
		{"方括号标识符", "SELECT * FROM [abc"},
		{"块注释", "SELECT 1; /* 未闭合"},
		{"执行注释", "SELECT 1; /*!40101 SET NAMES utf8mb4"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if stmts, err := splitSQL(tc.raw); err == nil {
				t.Fatalf("未闭合输入应报错，实际切出 %q", stmts)
			}
		})
	}
}

// TestSplitSQLRejectsTrigger 触发器体 BEGIN…END 内的分号无法与语句分隔符
// 区分，splitSQL 明确拒绝；只有列名/表名里出现 trigger 的普通 DDL 不受影响。
func TestSplitSQLRejectsTrigger(t *testing.T) {
	cases := []struct {
		name    string
		raw     string
		wantErr bool
	}{
		{
			name: "BEGIN…END 触发器体",
			raw: `CREATE TRIGGER trg BEFORE INSERT ON t FOR EACH ROW BEGIN
  SET @a = 1;
  SET @b = 2;
END;`,
			wantErr: true,
		},
		{
			name:    "带 DEFINER 的触发器",
			raw:     "CREATE DEFINER=`root`@`localhost` TRIGGER trg AFTER UPDATE ON t FOR EACH ROW SET @x = 1;",
			wantErr: true,
		},
		{
			name:    "表名含 trigger 的普通 DDL",
			raw:     "CREATE TABLE trigger_log (id INT);",
			wantErr: false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			stmts, err := splitSQL(tc.raw)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("触发器脚本应报错，实际切出 %q", stmts)
				}
				return
			}
			if err != nil {
				t.Fatalf("普通 DDL 不应报错: %v", err)
			}
			if len(stmts) != 1 {
				t.Fatalf("应切出 1 条语句，实际 %d: %q", len(stmts), stmts)
			}
		})
	}
}

// TestSplitSQLKeepExecComment MySQL 执行注释 /*!…*/ 有真语义：整体保留，
// 其中的分号不切分；普通块注释则剥掉。
func TestSplitSQLKeepExecComment(t *testing.T) {
	got := mustSplit(t, "/*!40101 SET NAMES utf8mb4 */;\nSELECT 1;\n")
	if len(got) != 2 || got[0] != "/*!40101 SET NAMES utf8mb4 */" || got[1] != "SELECT 1" {
		t.Fatalf("执行注释被破坏: %q", got)
	}
}

// TestSplitSQLExistingMigrations 现有迁移（MySQL 与 SQLite 两套）切分稳定：
// 不报错、每条语句非空，且再次切分仍是同一条。
func TestSplitSQLExistingMigrations(t *testing.T) {
	sqliteFS, err := fs.Sub(migrations.FS, "sqlite")
	if err != nil {
		t.Fatal(err)
	}
	for dir, fsys := range map[string]fs.FS{"": migrations.FS, "sqlite": sqliteFS} {
		entries, err := fs.ReadDir(fsys, ".")
		if err != nil {
			t.Fatal(err)
		}
		for _, e := range entries {
			if e.IsDir() || !strings.HasSuffix(e.Name(), ".sql") {
				continue
			}
			body, err := fs.ReadFile(fsys, e.Name())
			if err != nil {
				t.Fatal(err)
			}
			stmts, err := splitSQL(string(body))
			if err != nil {
				t.Errorf("%s/%s: %v", dir, e.Name(), err)
				continue
			}
			if len(stmts) == 0 {
				// 注释型 no-op 迁移（如 SQLite 侧的 0009_users_nickname_bin）合法：
				// 整份文件只有 -- 注释，loader 只记录版本号。真正的语句不可能被
				// 静默吞成 0 条——引号或块注释未闭合时 splitSQL 会报错。
				if !commentOnly(string(body)) {
					t.Errorf("%s/%s: 没切出语句", dir, e.Name())
				}
				continue
			}
			for _, stmt := range stmts {
				again, err := splitSQL(stmt)
				if err != nil {
					t.Errorf("%s/%s: 语句再切分报错: %q: %v", dir, e.Name(), stmt, err)
					continue
				}
				if len(again) != 1 || again[0] != stmt {
					t.Errorf("%s/%s: 语句再切分不稳定: %q -> %q", dir, e.Name(), stmt, again)
				}
			}
		}
	}
}

// commentOnly 迁移文件是否只有 -- 行注释与空行（注释型 no-op 迁移）。
func commentOnly(s string) bool {
	for _, line := range strings.Split(s, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "--") {
			continue
		}
		return false
	}
	return true
}
