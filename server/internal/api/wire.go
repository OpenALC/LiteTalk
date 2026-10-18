package api

import (
	"fmt"
	"strconv"
)

// ID REST wire 上的资源 ID（#31）：JSON 一律字符串，避免 JS Number 精度丢失
// （Snowflake ≈10^18 超出 2^53）。Unmarshal 双接受数字与字符串——请求侧老客户端
// 仍发数字不报错；响应侧只出字符串。
// 路径/查询参数保持十进制整数口径（URL 无精度问题），不在 JSON 范围内。
type ID int64

// MarshalJSON 输出十进制字符串。
func (id ID) MarshalJSON() ([]byte, error) {
	return []byte(`"` + strconv.FormatInt(int64(id), 10) + `"`), nil
}

// UnmarshalJSON 接受 123、"123"、null（→0）；其它形态报错。
func (id *ID) UnmarshalJSON(b []byte) error {
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
	*id = ID(n)
	return nil
}

// idOf int64 → ID（DTO 组装用）。
func idOf(v int64) ID { return ID(v) }

// idPtr 可空 ID（category_id 等）；0/缺省语义由调用方解释。
func idPtr(v int64) *ID {
	id := ID(v)
	return &id
}
