package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"

	"github.com/OpenALC/LiteTalk/server/internal/store"
)

// patchRaw 直接发 PATCH（自定义 Content-Type 与原始体）。
func patchRaw(t *testing.T, h http.Handler, path, token, contentType, body string, wantStatus int) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPatch, path, strings.NewReader(body))
	req.Header.Set("Content-Type", contentType)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != wantStatus {
		t.Fatalf("%s: 期望 %d 实际 %d body=%s", path, wantStatus, rec.Code, rec.Body.String())
	}
	return rec
}

// TestMergePatchRFC7396 合并补丁三态（#31）：缺省不改、null 清空、值设置。
// 两种 Content-Type（application/json 与 application/merge-patch+json）同义。
func TestMergePatchRFC7396(t *testing.T) {
	srv, mem := newTestServer(t)
	h := srv.Handler()
	aAccess, _ := registerUser(t, h, mem, "mp-a@ex.com", "mpA", "password123")
	bAccess, _ := registerUser(t, h, mem, "mp-b@ex.com", "mpB", "password123")

	// A 加 B → B 接受 → A 持有关系行可设备注。
	fr := postJSON(t, h, "/api/v1/friends/requests", aAccess,
		map[string]string{"nickname": "mpB#" + memTag(t, mem, "mp-b@ex.com")}, http.StatusCreated)
	fid := asID(fr["id"])
	postJSON(t, h, fmt.Sprintf("/api/v1/friends/requests/%d/accept", fid), bAccess, nil, http.StatusOK)
	fURL := fmt.Sprintf("/api/v1/friends/%d", fid)

	// 值设置。
	rec := patchRaw(t, h, fURL, aAccess, "application/merge-patch+json",
		`{"remark":"同事小王","group":"同事"}`, http.StatusOK)
	var out map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	if out["remark"] != "同事小王" || out["group"] != "同事" {
		t.Fatalf("设置不生效：%v", out)
	}

	// 缺省 = 不改（只给 remark，group 保持）。
	rec = patchRaw(t, h, fURL, aAccess, "application/json", `{"remark":"改了"}`, http.StatusOK)
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	if out["remark"] != "改了" || out["group"] != "同事" {
		t.Fatalf("缺省字段应保持：%v", out)
	}

	// null = 清空（remark 归零，group 不动）——RFC 7396 核心语义。
	rec = patchRaw(t, h, fURL, aAccess, "application/merge-patch+json", `{"remark":null}`, http.StatusOK)
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	if out["remark"] != "" || out["group"] != "同事" {
		t.Fatalf("null 应清空 remark：%v", out)
	}

	// 空串 = 清空（旧口径兼容）。
	patchRaw(t, h, fURL, aAccess, "application/json", `{"group":""}`, http.StatusOK)
	_ = json.Unmarshal(rec.Body.Bytes(), &out)

	// name 不可清：null → 置空 → 校验拒 400。
	serverResp := postJSON(t, h, "/api/v1/servers", aAccess,
		map[string]string{"name": "合并补丁服", "icon_name": "music", "icon_color": "#4FA4F3"}, http.StatusCreated)
	serverID := asID(serverResp["id"])
	patchRaw(t, h, fmt.Sprintf("/api/v1/servers/%d", serverID), aAccess,
		"application/merge-patch+json", `{"name":null}`, http.StatusBadRequest)

	// position 不可清除 → 400。
	chResp := postJSON(t, h, fmt.Sprintf("/api/v1/servers/%d/channels", serverID), aAccess,
		map[string]string{"name": "闲聊", "type": "text"}, http.StatusCreated)
	chID := asID(chResp["id"])
	patchRaw(t, h, fmt.Sprintf("/api/v1/channels/%d", chID), aAccess,
		"application/merge-patch+json", `{"position":null}`, http.StatusBadRequest)

	// category_id 的 null = 移出分类（0 同义）。
	catResp := postJSON(t, h, fmt.Sprintf("/api/v1/servers/%d/categories", serverID), aAccess,
		map[string]string{"name": "甲组"}, http.StatusCreated)
	catID := asID(catResp["id"])
	patchRaw(t, h, fmt.Sprintf("/api/v1/channels/%d", chID), aAccess,
		"application/json", fmt.Sprintf(`{"category_id":%d}`, catID), http.StatusOK)
	rec = patchRaw(t, h, fmt.Sprintf("/api/v1/channels/%d", chID), aAccess,
		"application/merge-patch+json", `{"category_id":null}`, http.StatusOK)
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	if out["category_id"] != nil {
		t.Fatalf("null category_id 应移出分类：%v", out["category_id"])
	}
}

// memTag 从内存 store 取用户 tag（测试拼 昵称#tag 用）。
func memTag(t *testing.T, mem *store.Mem, email string) string {
	t.Helper()
	u, err := mem.UserByEmail(t.Context(), email)
	if err != nil {
		t.Fatal(err)
	}
	return u.Tag
}

// TestETagNegotiation 成员/频道列表 ETag 协商（#31）：命中 If-None-Match 回 304。
func TestETagNegotiation(t *testing.T) {
	srv, mem := newTestServer(t)
	h := srv.Handler()
	aAccess, _ := registerUser(t, h, mem, "et-a@ex.com", "etA", "password123")
	serverResp := postJSON(t, h, "/api/v1/servers", aAccess,
		map[string]string{"name": "缓存服", "icon_name": "music", "icon_color": "#4FA4F3"}, http.StatusCreated)
	serverID := asID(serverResp["id"])

	get := func(path, inm string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		req.Header.Set("Authorization", "Bearer "+aAccess)
		if inm != "" {
			req.Header.Set("If-None-Match", inm)
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec
	}

	// 频道列表：首访带 ETag 与 body。
	rec := get(fmt.Sprintf("/api/v1/servers/%d/channels", serverID), "")
	if rec.Code != http.StatusOK {
		t.Fatalf("首访应 200，实际 %d", rec.Code)
	}
	etag := rec.Header().Get("ETag")
	if etag == "" {
		t.Fatal("缺 ETag")
	}
	// 带回 ETag → 304 无 body。
	rec = get(fmt.Sprintf("/api/v1/servers/%d/channels", serverID), etag)
	if rec.Code != http.StatusNotModified || rec.Body.Len() != 0 {
		t.Fatalf("命中应 304 空 body：%d %q", rec.Code, rec.Body.String())
	}
	// 弱前缀与 * 同样命中。
	rec = get(fmt.Sprintf("/api/v1/servers/%d/channels", serverID), "W/"+etag)
	if rec.Code != http.StatusNotModified {
		t.Fatalf("W/ 前缀应命中：%d", rec.Code)
	}

	// 成员列表同语义。
	rec = get(fmt.Sprintf("/api/v1/servers/%d/members", serverID), "")
	metag := rec.Header().Get("ETag")
	if rec.Code != http.StatusOK || metag == "" {
		t.Fatalf("成员列表应 200+ETag：%d %q", rec.Code, metag)
	}
	rec = get(fmt.Sprintf("/api/v1/servers/%d/members", serverID), metag)
	if rec.Code != http.StatusNotModified {
		t.Fatalf("成员列表命中应 304，实际 %d", rec.Code)
	}

	// 数据变了 ETag 应变。
	postJSON(t, h, fmt.Sprintf("/api/v1/servers/%d/channels", serverID), aAccess,
		map[string]string{"name": "新频道", "type": "text"}, http.StatusCreated)
	rec = get(fmt.Sprintf("/api/v1/servers/%d/channels", serverID), etag)
	if rec.Code != http.StatusOK || rec.Header().Get("ETag") == etag {
		t.Fatalf("数据变更后不应命中：%d", rec.Code)
	}
}

// TestTimeUTCRFC3339 时间出参一律 RFC 3339 UTC（#31）：秒精度、Z 后缀。
func TestTimeUTCRFC3339(t *testing.T) {
	srv, mem := newTestServer(t)
	h := srv.Handler()
	aAccess, _ := registerUser(t, h, mem, "utc-a@ex.com", "utcA", "password123")
	rec := httptest.NewRequest(http.MethodGet, "/api/v1/users/me", nil)
	rec.Header.Set("Authorization", "Bearer "+aAccess)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, rec)
	var me map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &me)
	created, _ := me["created_at"].(string)
	if !regexp.MustCompile(`^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}Z$`).MatchString(created) {
		t.Fatalf("created_at 应为 RFC3339 UTC 秒精度，实际 %q", created)
	}
}
