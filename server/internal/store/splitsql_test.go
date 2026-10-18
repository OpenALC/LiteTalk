package store

import (
	"io/fs"
	"reflect"
	"strings"
	"testing"

	"github.com/OpenALC/LiteTalk/server/migrations"
)

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
	got, want := splitSQL(with), splitSQL(without)
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
	got := splitSQL(raw)
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

// TestSplitSQLKeepExecComment MySQL 执行注释 /*!…*/ 有真语义：整体保留，
// 其中的分号不切分；普通块注释则剥掉。
func TestSplitSQLKeepExecComment(t *testing.T) {
	got := splitSQL("/*!40101 SET NAMES utf8mb4 */;\nSELECT 1;\n")
	if len(got) != 2 || got[0] != "/*!40101 SET NAMES utf8mb4 */" || got[1] != "SELECT 1" {
		t.Fatalf("执行注释被破坏: %q", got)
	}
}

// TestSplitSQLExistingMigrations 现有迁移（MySQL 与 SQLite 两套）切分稳定：
// 每条语句非空，且再次切分仍是同一条。
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
			stmts := splitSQL(string(body))
			if len(stmts) == 0 {
				t.Errorf("%s/%s: 没切出语句", dir, e.Name())
			}
			for _, stmt := range stmts {
				if again := splitSQL(stmt); len(again) != 1 || again[0] != stmt {
					t.Errorf("%s/%s: 语句再切分不稳定: %q -> %q", dir, e.Name(), stmt, again)
				}
			}
		}
	}
}
