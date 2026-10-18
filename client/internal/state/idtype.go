package state

import (
	"fmt"
	"strconv"
)

// ID REST wire 上的资源 ID（#31）：JSON 一律字符串（Snowflake ≈10^18 超出 JS
// Number 2^53，数字形态会掉精度）。Unmarshal 双接受数字与字符串（老服务端兼容），
// Marshal 只出字符串。与 server/internal/api 的 ID 类型语义一致，两模块不共享代码。
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

// String 十进制串（URL 拼接用）。
func (id ID) String() string { return strconv.FormatInt(int64(id), 10) }
