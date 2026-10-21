package api

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestChannelOptionsWire #37 B2：CreateSubchannel/UpdateChannelOptions 的请求形状。
// UpdateChannelOptions 只发送需要改的字段——多发空字段会被 Merge Patch 当「清除」。
func TestChannelOptionsWire(t *testing.T) {
	var last struct {
		method, path, body string
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		last.method, last.path, last.body = r.Method, r.URL.Path, string(b)
		w.Header().Set("Content-Type", "application/json")
		if r.Method == http.MethodPatch {
			// 回显解码后的 body，便于断言字段。
			w.Write(b)
			return
		}
		w.WriteHeader(http.StatusCreated)
		w.Write([]byte(`{"id":"9","server_id":"1","name":"子","type":"voice","category_id":null,` +
			`"position":1,"created_at":"2026-01-01T00:00:00Z","parent_id":"5","has_password":false,"user_limit":0}`))
	}))
	defer srv.Close()
	c := NewAPIClient(srv.URL)
	c.SetToken("t")

	// CreateSubchannel：body 带 parent_id，类型随父。
	sub, err := c.CreateSubchannel("1", "子", "voice", "5")
	if err != nil || sub.ID != 9 {
		t.Fatalf("CreateSubchannel: %+v %v", sub, err)
	}
	if last.method != http.MethodPost || last.path != "/api/v1/servers/1/channels" {
		t.Fatalf("CreateSubchannel 请求形状: %+v", last)
	}
	var post map[string]any
	if err := json.Unmarshal([]byte(last.body), &post); err != nil {
		t.Fatal(err)
	}
	if post["parent_id"] != "5" || post["type"] != "voice" || post["category_id"] != nil {
		t.Fatalf("CreateSubchannel body: %s", last.body)
	}

	// 只带 user_limit：body 不得出现 password/parent_id。
	if _, err := c.UpdateChannelOptions("5", "", 7, "", false); err != nil {
		t.Fatalf("UpdateChannelOptions: %v", err)
	}
	var patch map[string]any
	if err := json.Unmarshal([]byte(last.body), &patch); err != nil {
		t.Fatal(err)
	}
	if len(patch) != 1 || patch["user_limit"] != float64(7) {
		t.Fatalf("只应发送 user_limit: %s", last.body)
	}

	// 清密码 + 提升顶层：password 为 JSON null，parent_id="0"；userLimit -1 不发。
	// map 解码会合并旧键，每次用新 map。
	if _, err := c.UpdateChannelOptions("5", "0", -1, "", true); err != nil {
		t.Fatalf("UpdateChannelOptions(清密码): %v", err)
	}
	patch = nil
	if err := json.Unmarshal([]byte(last.body), &patch); err != nil {
		t.Fatal(err)
	}
	if len(patch) != 2 || patch["password"] != nil || patch["parent_id"] != "0" {
		t.Fatalf("清密码/提升顶层 body: %s", last.body)
	}

	// 设密码 + 不动父：无 parent_id 键。
	if _, err := c.UpdateChannelOptions("5", "", -1, "pw", false); err != nil {
		t.Fatalf("UpdateChannelOptions(设密码): %v", err)
	}
	patch = nil
	if err := json.Unmarshal([]byte(last.body), &patch); err != nil {
		t.Fatal(err)
	}
	if len(patch) != 1 || patch["password"] != "pw" {
		t.Fatalf("设密码 body: %s", last.body)
	}
}
