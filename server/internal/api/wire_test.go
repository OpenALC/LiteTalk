package api

import (
	"encoding/json"
	"testing"
)

// TestIDMarshal 响应侧只出字符串。
func TestIDMarshal(t *testing.T) {
	b, err := json.Marshal(struct {
		ID ID `json:"id"`
	}{ID: 105271540102201344})
	if err != nil {
		t.Fatal(err)
	}
	if string(b) != `{"id":"105271540102201344"}` {
		t.Fatalf("响应应为字符串 ID，实际 %s", b)
	}
}

// TestIDUnmarshalDual 请求侧数字/字符串双接受（老客户端兼容）。
func TestIDUnmarshalDual(t *testing.T) {
	for _, in := range []string{
		`{"id":"105271540102201344"}`,
		`{"id":105271540102201344}`,
	} {
		var v struct {
			ID ID `json:"id"`
		}
		if err := json.Unmarshal([]byte(in), &v); err != nil {
			t.Fatalf("%s: %v", in, err)
		}
		if int64(v.ID) != 105271540102201344 {
			t.Fatalf("%s: 解析为 %d", in, v.ID)
		}
	}
	var v struct {
		ID ID `json:"id"`
	}
	if err := json.Unmarshal([]byte(`{"id":null}`), &v); err != nil || v.ID != 0 {
		t.Fatalf("null 应为 0：%v %d", err, v.ID)
	}
	if err := json.Unmarshal([]byte(`{"id":"x"}`), &v); err == nil {
		t.Fatal("非法 ID 应报错")
	}
}

// TestIDPointer 可空字段（category_id）同样双接受。
func TestIDPointer(t *testing.T) {
	var v struct {
		CategoryID *ID `json:"category_id"`
	}
	if err := json.Unmarshal([]byte(`{"category_id":"7"}`), &v); err != nil || *v.CategoryID != 7 {
		t.Fatalf("字符串可空 ID：%v", err)
	}
	if err := json.Unmarshal([]byte(`{"category_id":0}`), &v); err != nil || *v.CategoryID != 0 {
		t.Fatalf("数字 0：%v", err)
	}
}
