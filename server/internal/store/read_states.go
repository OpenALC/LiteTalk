package store

import "strings"

// derivedUnion 生成 "(SELECT ? AS col UNION ALL SELECT ? ...)" 派生表。
// 未读批量统计把请求的用户/频道先展开成派生表再 LEFT JOIN：没有 read_states
// 行（或频道没有消息）的请求项在 GROUP BY 后仍然保留一行 0 值，与 Mem 实现
// 「请求项全量出现在结果 map」的语义一致，也避免按项循环发 N 条查询。
func derivedUnion(column string, n int) string {
	var b strings.Builder
	b.WriteString("(SELECT ? AS ")
	b.WriteString(column)
	for i := 1; i < n; i++ {
		b.WriteString(" UNION ALL SELECT ?")
	}
	b.WriteString(")")
	return b.String()
}
