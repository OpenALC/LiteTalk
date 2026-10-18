package main

import "strings"

// acronyms 整段大写的常见缩写（小写入表）。min/max 不入表：要的是 Min/Max 而非 MIN/MAX。
var acronyms = map[string]bool{
	"id": true, "ids": true, "ts": true, "sdp": true, "url": true, "uri": true,
	"api": true, "ws": true, "wss": true, "rtc": true, "ice": true, "uid": true,
	"sid": true, "dev": true, "cpu": true, "db": true, "http": true, "https": true,
	"json": true, "rtp": true, "rtt": true, "ttl": true, "ip": true,
}

// goName 把 wire 字段名转成 Go 导出字段名。
// 规则：按 _ 与 camelCase 边界切词；词在缩写表 → 全大写；否则首字母大写。
// 例：user_id→UserID、sdpMid→SDPMid、sdpMLineIndex→SDPMLineIndex、
// usernameFragment→UsernameFragment、ts→TS。
func goName(wire string) string {
	var b strings.Builder
	for _, tok := range splitWords(wire) {
		if tok == "" {
			continue
		}
		low := strings.ToLower(tok)
		if acronyms[low] {
			b.WriteString(strings.ToUpper(tok))
			continue
		}
		b.WriteString(strings.ToUpper(tok[:1]))
		if len(tok) > 1 {
			b.WriteString(tok[1:])
		}
	}
	return b.String()
}

// splitWords 切词：先按 _ 分段，每段再按 camelCase 边界切
// （小写/数字→大写处切；连续大写后跟小写时，切在最后一个大写前，如 MLine → M|Line）。
func splitWords(s string) []string {
	var out []string
	for _, part := range strings.Split(s, "_") {
		out = append(out, splitCamel(part)...)
	}
	return out
}

func splitCamel(s string) []string {
	var out []string
	start := 0
	for i := 1; i < len(s); i++ {
		prev, cur := s[i-1], s[i]
		switch {
		case isUpper(cur) && !isUpper(prev):
			out = append(out, s[start:i])
			start = i
		case isUpper(cur) && isUpper(prev) && i+1 < len(s) && !isUpper(s[i+1]):
			out = append(out, s[start:i])
			start = i
		}
	}
	if start < len(s) {
		out = append(out, s[start:])
	}
	return out
}

func isUpper(c byte) bool { return c >= 'A' && c <= 'Z' }
