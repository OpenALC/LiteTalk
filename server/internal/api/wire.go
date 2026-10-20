package api

import (
	"encoding/json"
	"fmt"
	"hash/fnv"
	"strconv"
	"time"
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

// Time wire 时间（#31）：一律 RFC 3339 UTC（去掉纳秒）。Go 的 time.Time 默认
// 序列化带本地偏移且是 RFC3339Nano，出参必须经本类型收口。
type Time time.Time

// MarshalJSON 输出 RFC 3339 UTC（秒精度）。
func (t Time) MarshalJSON() ([]byte, error) {
	return []byte(`"` + time.Time(t).UTC().Format(time.RFC3339) + `"`), nil
}

// UnmarshalJSON 接受 RFC 3339（含偏移，内部归一 UTC）。
func (t *Time) UnmarshalJSON(b []byte) error {
	s := string(b)
	if s == "null" {
		*t = Time{}
		return nil
	}
	if len(s) >= 2 && s[0] == '"' && s[len(s)-1] == '"' {
		s = s[1 : len(s)-1]
	}
	parsed, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return fmt.Errorf("非法时间 %q: %w", s, err)
	}
	*t = Time(parsed.UTC())
	return nil
}

// timeOf store time.Time → wire Time。
func timeOf(v time.Time) Time { return Time(v) }

// contentETag 对响应 DTO 做内容哈希弱 ETag（#31 协商缓存）。marshal 走 wire
// 类型的序列化（ID 字符串、UTC 时间），形状变化都会改哈希。
func contentETag(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		return `W/"0"`
	}
	h := fnv.New64a()
	_, _ = h.Write(b)
	return fmt.Sprintf(`W/"%x"`, h.Sum64())
}

// ---- JSON Merge Patch 可选字段（RFC 7396，#31）----
// 缺省 = 不改；null = 清除/复位；值 = 设置。Go 的 *string 区分不了缺省与 null，
// 三态封装后由 handler 按字段语义解释「清除」。

// OptString 合并补丁字符串字段。
type OptString struct {
	Set  bool
	Null bool
	Val  string
}

// UnmarshalJSON 三态解析：缺省（不调用）、null、字符串值。
func (o *OptString) UnmarshalJSON(b []byte) error {
	o.Set = true
	if string(b) == "null" {
		o.Null = true
		o.Val = ""
		return nil
	}
	return json.Unmarshal(b, &o.Val)
}

// Get 有值返回 (值, true)；null/缺省返回 false。
func (o OptString) Get() (string, bool) {
	if !o.Set || o.Null {
		return "", false
	}
	return o.Val, true
}

// Clearing 是否显式要求清除（null）。
func (o OptString) Clearing() bool { return o.Set && o.Null }

// OptInt 合并补丁整数字段。
type OptInt struct {
	Set  bool
	Null bool
	Val  int
}

// UnmarshalJSON 三态解析。
func (o *OptInt) UnmarshalJSON(b []byte) error {
	o.Set = true
	if string(b) == "null" {
		o.Null = true
		o.Val = 0
		return nil
	}
	return json.Unmarshal(b, &o.Val)
}

// Get 有值返回 (值, true)。
func (o OptInt) Get() (int, bool) {
	if !o.Set || o.Null {
		return 0, false
	}
	return o.Val, true
}

// Clearing 是否显式要求清除（null）。
func (o OptInt) Clearing() bool { return o.Set && o.Null }

// OptID 合并补丁 ID 字段（数字/字符串双接受；null = 清除）。
type OptID struct {
	Set  bool
	Null bool
	Val  ID
}

// UnmarshalJSON 三态解析。
func (o *OptID) UnmarshalJSON(b []byte) error {
	o.Set = true
	if string(b) == "null" {
		o.Null = true
		o.Val = 0
		return nil
	}
	return o.Val.UnmarshalJSON(b)
}

// Get 有值返回 (值, true)。
func (o OptID) Get() (ID, bool) {
	if !o.Set || o.Null {
		return 0, false
	}
	return o.Val, true
}

// Clearing 是否显式要求清除（null）。
func (o OptID) Clearing() bool { return o.Set && o.Null }
