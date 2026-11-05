// Package emailnorm 提供账号查找和唯一键共用的邮箱规范化规则。
package emailnorm

import "strings"

// Normalize 保留既有规则：去掉空格和制表符，再按 Go Unicode 规则转小写。
func Normalize(s string) string {
	out := make([]rune, 0, len(s))
	for _, r := range s {
		if r != ' ' && r != '\t' {
			out = append(out, r)
		}
	}
	return strings.ToLower(string(out))
}
