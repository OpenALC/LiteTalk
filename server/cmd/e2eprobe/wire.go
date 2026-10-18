package main

import (
	"fmt"
	"strconv"
)

// jsonID REST wire 资源 ID（#31）：JSON 一律字符串（Snowflake 超出 JS/float64
// 精度，数字形态解码进 int64 前会掉精度）。解码双接受数字与字符串（老服务端兼容）。
// 探针内部继续用 int64 运算，仅 JSON 边界走本类型。
type jsonID int64

// UnmarshalJSON 接受 123、"123"、null（→0）；其它形态报错。
func (id *jsonID) UnmarshalJSON(b []byte) error {
	s := string(b)
	if s == "null" {
		*id = 0
		return nil
	}
	if len(s) >= 2 && s[0] == '"' && s[len(s)-1] == '"' {
		s = s[1 : len(s)-1]
	}
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		return fmt.Errorf("非法 ID %q: %w", s, err)
	}
	*id = jsonID(n)
	return nil
}

// String 十进制串（URL 拼接用）。
func (id jsonID) String() string { return strconv.FormatInt(int64(id), 10) }
