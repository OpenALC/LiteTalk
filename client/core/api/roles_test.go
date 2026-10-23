package api

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestRolesAPIWire 角色/覆盖/审计方法的请求形状与响应解码（#35 B4 最小单测）。
// 只验 wire：路径、方法、body 字段与解码，不复述服务端业务规则。
func TestRolesAPIWire(t *testing.T) {
	type hit struct {
		method, path, query, body string
	}
	var last hit
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		last = hit{method: r.Method, path: r.URL.Path, query: r.URL.RawQuery, body: string(b)}
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/roles"):
			fmt.Fprint(w, `[{"id":"10","server_id":"1","name":"甲","color":"#4FA4F3","position":3,`+
				`"permissions":"3","builtin":false,"created_at":"2026-01-01T00:00:00Z","updated_at":"2026-01-01T00:00:00Z"}]`)
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/roles"):
			w.WriteHeader(http.StatusCreated)
			fmt.Fprint(w, `{"id":"11","server_id":"1","name":"乙","color":"","position":0,`+
				`"permissions":"8207","builtin":false,"created_at":"2026-01-01T00:00:00Z","updated_at":"2026-01-01T00:00:00Z"}`)
		case r.Method == http.MethodPut && strings.Contains(r.URL.Path, "/overwrites/"):
			fmt.Fprint(w, `{"channel_id":"5","target_type":"role","target_id":"10","allow":"3","deny":"0",`+
				`"updated_at":"2026-01-01T00:00:00Z"}`)
		case r.Method == http.MethodPut:
			fmt.Fprint(w, `{"status":"ok"}`)
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/overwrites"):
			fmt.Fprint(w, `[{"channel_id":"5","target_type":"role","target_id":"10","allow":"3","deny":"0",`+
				`"updated_at":"2026-01-01T00:00:00Z"}]`)
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/audit-log"):
			fmt.Fprint(w, `[{"id":"9","server_id":"1","actor_id":"2","action":"role.create","detail":"{}",`+
				`"created_at":"2026-01-01T00:00:00Z"}]`)
		case r.Method == http.MethodDelete:
			fmt.Fprint(w, `{"status":"ok"}`)
		default:
			w.WriteHeader(http.StatusNotFound)
			fmt.Fprint(w, `{"code":"not_found","detail":"x"}`)
		}
	}))
	defer srv.Close()
	c := NewAPIClient(srv.URL)
	c.SetToken("tok")

	roles, err := c.ListRoles("1")
	if err != nil || len(roles) != 1 || roles[0].ID != 10 || roles[0].Permissions != "3" || roles[0].Builtin {
		t.Fatalf("ListRoles 解码异常: %+v %v", roles, err)
	}
	if last.method != http.MethodGet || !strings.HasSuffix(last.path, "/servers/1/roles") {
		t.Fatalf("ListRoles 请求形状: %+v", last)
	}

	created, err := c.CreateRole("1", RoleWrite{Name: "乙", Permissions: "8207"})
	if err != nil || created.ID != 11 || created.Permissions != "8207" {
		t.Fatalf("CreateRole 解码异常: %+v %v", created, err)
	}
	if last.method != http.MethodPost || !strings.Contains(last.body, `"permissions":"8207"`) {
		t.Fatalf("CreateRole 请求形状: %+v", last)
	}

	if err := c.SetMemberRoles("1", "2", []string{"10", "11"}); err != nil {
		t.Fatalf("SetMemberRoles: %v", err)
	}
	if last.method != http.MethodPut || !strings.HasSuffix(last.path, "/servers/1/members/2/roles") {
		t.Fatalf("SetMemberRoles 路径: %+v", last)
	}
	var body struct {
		RoleIDs []string `json:"role_ids"`
	}
	if err := json.Unmarshal([]byte(last.body), &body); err != nil || len(body.RoleIDs) != 2 {
		t.Fatalf("SetMemberRoles body: %s %v", last.body, err)
	}

	ow, err := c.SetChannelOverwrite("5", "role", "10", "3", "0")
	if err != nil || ow.TargetType != "role" || ow.Allow != "3" {
		t.Fatalf("SetChannelOverwrite 解码异常: %+v %v", ow, err)
	}
	if !strings.HasSuffix(last.path, "/channels/5/overwrites/role/10") {
		t.Fatalf("SetChannelOverwrite 路径: %+v", last)
	}
	ows, err := c.ListChannelOverwrites("5")
	if err != nil || len(ows) != 1 || ows[0].TargetID != 10 {
		t.Fatalf("ListChannelOverwrites 解码异常: %+v %v", ows, err)
	}

	logs, err := c.ListAuditLog("1", "9", 20)
	if err != nil || len(logs) != 1 || logs[0].Action != "role.create" {
		t.Fatalf("ListAuditLog 解码异常: %+v %v", logs, err)
	}
	if last.query != "limit=20&before=9" && last.query != "before=9&limit=20" {
		// 实现是 limit 在前、before 追加；钉住防止悄悄改形状。
		if last.query != "limit=20&before=9" {
			t.Fatalf("ListAuditLog query: %q", last.query)
		}
	}

	if err := c.DeleteRole("11"); err != nil {
		t.Fatalf("DeleteRole: %v", err)
	}
	if last.method != http.MethodDelete || !strings.HasSuffix(last.path, "/roles/11") {
		t.Fatalf("DeleteRole 形状: %+v", last)
	}
	if err := c.DeleteChannelOverwrite("5", "role", "10"); err != nil {
		t.Fatalf("DeleteChannelOverwrite: %v", err)
	}
	if last.method != http.MethodDelete || !strings.HasSuffix(last.path, "/channels/5/overwrites/role/10") {
		t.Fatalf("DeleteChannelOverwrite 形状: %+v", last)
	}

	// #36 服务端强制闭麦：PUT .../server-mute，body {"muted":...}。
	if err := c.SetServerMuted("5", "2", true); err != nil {
		t.Fatalf("SetServerMuted: %v", err)
	}
	if last.method != http.MethodPut || !strings.HasSuffix(last.path, "/channels/5/members/2/server-mute") {
		t.Fatalf("SetServerMuted 路径: %+v", last)
	}
	if last.body != `{"muted":true}` {
		t.Fatalf("SetServerMuted body: %s", last.body)
	}
}
