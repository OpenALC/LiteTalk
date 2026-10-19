package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log"
	"math"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
)

var errSentinel = errors.New("sentinel: db connection lost")

// TestWriteStoreErrLogs500 writeStoreErr default 分支必须记底层错误并返回 500（#21）。
// strict 迁移后返回 *apiErr 由 ResponseErrorHandlerFunc 写出，映射与日志行为不变。
func TestWriteStoreErrLogs500(t *testing.T) {
	var logBuf bytes.Buffer
	srv, _ := newTestServerMode(t, AuthModeFull)
	srv.log = log.New(&logBuf, "", 0)

	ctx := context.WithValue(context.Background(), reqPathKey, "/api/v1/users/me")
	err := srv.writeStoreErr(ctx, errSentinel)
	var e *apiErr
	if !errors.As(err, &e) {
		t.Fatalf("应返回 *apiErr，实际 %T", err)
	}
	if e.status != http.StatusInternalServerError {
		t.Fatalf("期望 500，实际 %d", e.status)
	}
	if !strings.Contains(logBuf.String(), "sentinel") {
		t.Fatalf("日志应包含底层错误，实际: %q", logBuf.String())
	}
	if !strings.Contains(logBuf.String(), "/api/v1/users/me") {
		t.Fatalf("日志应包含请求路径，实际: %q", logBuf.String())
	}
}

// TestDecodeJSONTooLarge 超过 1MB 的请求体返回 413 too_large（#21，含 chunked）。
func TestDecodeJSONTooLarge(t *testing.T) {
	srv, _ := newTestServerMode(t, AuthModeFull)
	h := srv.Handler()
	// 构造超过 1MB 的 JSON 字段。
	big := strings.Repeat("x", 2<<20)
	body := `{"nickname":"` + big + `"}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/auth/register", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(clientVersionHeader, testClientVersion)
	req.ContentLength = int64(len(body))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("期望 413，实际 %d body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "too_large") {
		t.Fatalf("响应应包含 too_large，实际 %s", rec.Body.String())
	}
}

// TestDecodeJSONTooLargeChunked chunked 编码（ContentLength 未知）同样返回 413。
func TestDecodeJSONTooLargeChunked(t *testing.T) {
	srv, _ := newTestServerMode(t, AuthModeFull)
	h := srv.Handler()
	big := strings.Repeat("x", 2<<20)
	body := `{"nickname":"` + big + `"}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/auth/register", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(clientVersionHeader, testClientVersion)
	req.ContentLength = -1 // 模拟 chunked：Content-Length 未知
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("chunked 期望 413，实际 %d body=%s", rec.Code, rec.Body.String())
	}
}

// TestValidPassword 密码 8–72 字节（#22）。
func TestValidPassword(t *testing.T) {
	valid := []string{"12345678", "a-very-long-but-acceptable-password-string-0123456789", strings.Repeat("x", 72)}
	for _, s := range valid {
		if !validPassword(s) {
			t.Fatalf("密码 %q 应合法", s[:20])
		}
	}
	invalid := []string{"", "1234567", strings.Repeat("x", 73)}
	for _, s := range invalid {
		if validPassword(s) {
			t.Fatalf("密码（%d 字节）应非法", len(s))
		}
	}
}

// TestPatchMeOK 合法昵称变更正常通过（409 路径由 writeStoreErr 单测覆盖）。
func TestPatchMeOK(t *testing.T) {
	srv, _ := newTestServerMode(t, AuthModeSimple)
	h := srv.Handler()
	// simple 模式直接注册。
	out := postJSON(t, h, "/api/v1/auth/register", "", map[string]string{
		"nickname": "conflict-user", "email": "conflict@test.com", "password": "password123",
	}, 201)
	tok, _ := out["access_token"].(string)
	if tok == "" {
		t.Fatalf("注册响应缺 access_token: %v", out)
	}

	body := map[string]any{"nickname": "new-name-ok"}
	buf := new(bytes.Buffer)
	_ = json.NewEncoder(buf).Encode(body)
	req := httptest.NewRequest(http.MethodPatch, "/api/v1/users/me", buf)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+tok)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("合法昵称变更应 200，实际 %d body=%s", rec.Code, rec.Body.String())
	}
}

// TestTagFromIDLargeSnowflake 大 Snowflake ID 生成的 tag 必须是非负 4 位十进制（#22）。
// 旧实现 ID*7919 在 1<<62 量级溢出成负数，MySQL 写 CHAR(4) 直接 500。
func TestTagFromIDLargeSnowflake(t *testing.T) {
	ids := []int64{
		int64(1) << 62, // 旧公式在此量级已溢出
		int64(1)<<62 + 1,
		int64(1)<<62 + 123456789,
		int64(1) << 61,
		math.MaxInt64,
		math.MaxInt64 - 7919,
		int64(1767225600000) << 22, // Snowflake 纪元对应的 ID 量级
	}
	for _, id := range ids {
		for attempt := 0; attempt < 5; attempt++ {
			tag := tagFromID(id, attempt)
			if len(tag) != 4 {
				t.Fatalf("id=%d attempt=%d tag=%q 不是 4 位", id, attempt, tag)
			}
			n, err := strconv.Atoi(tag)
			if err != nil || n < 0 || n >= 10000 {
				t.Fatalf("id=%d attempt=%d tag=%q 超出 0..9999", id, attempt, tag)
			}
		}
	}
}

// TestTagFromIDDistribution 同一 ID 的不同重试序号应覆盖尽量多的取值，
// 不同大 ID 生成的 tag 在 10 个千位段上分布均匀（#22）。
func TestTagFromIDDistribution(t *testing.T) {
	const samples = 10000
	base := int64(1) << 62
	var buckets [10]int
	seen := map[string]struct{}{}
	for i := 0; i < samples; i++ {
		tag := tagFromID(base+int64(i)*104729, i%5)
		n, err := strconv.Atoi(tag)
		if err != nil || n < 0 || n >= 10000 {
			t.Fatalf("第 %d 个样本 tag=%q 非法", i, tag)
		}
		buckets[n/1000]++
		seen[tag] = struct{}{}
	}
	for i, c := range buckets {
		if c < 500 || c > 1500 {
			t.Fatalf("千位段 %d 分布异常：%d 次（期望约 1000）", i, c)
		}
	}
	// 1 万样本落在 1 万个桶的期望去重数约 6321；低于 5000 说明混合退化。
	if len(seen) < 5000 {
		t.Fatalf("tag 去重数过低：%d", len(seen))
	}
	// 同一 ID 的前 100 个重试序号应有足够变化（冲突重试才有意义）。
	attempts := map[string]struct{}{}
	for a := 0; a < 100; a++ {
		attempts[tagFromID(base, a)] = struct{}{}
	}
	if len(attempts) < 50 {
		t.Fatalf("同一 ID 的重试 tag 变化过少：%d/100", len(attempts))
	}
}

// TestPatchMeTagReachableAfterRename 改昵称后 tag 仍是 4 位合法值，
// 且新 昵称#tag 能被其他用户用于加好友（#22：负数 tag 会让用户从此不可被添加）。
func TestPatchMeTagReachableAfterRename(t *testing.T) {
	srv, _ := newTestServerMode(t, AuthModeSimple)
	h := srv.Handler()

	target := postJSON(t, h, "/api/v1/auth/register", "", map[string]string{
		"nickname": "target-user", "email": "target@test.com", "password": "password123",
	}, 201)
	targetTok, _ := target["access_token"].(string)
	if targetTok == "" {
		t.Fatalf("注册响应缺 access_token: %v", target)
	}
	patchJSON(t, h, "/api/v1/users/me", targetTok,
		map[string]any{"nickname": "renamed-user"}, http.StatusOK)

	me, ok := getJSON(t, h, "/api/v1/users/me", targetTok, http.StatusOK).(map[string]any)
	if !ok {
		t.Fatal("/users/me 响应不是对象")
	}
	nickname, _ := me["nickname"].(string)
	tag, _ := me["tag"].(string)
	if len(tag) != 4 {
		t.Fatalf("改名后 tag=%q 不是 4 位", tag)
	}
	if n, err := strconv.Atoi(tag); err != nil || n < 0 || n >= 10000 {
		t.Fatalf("改名后 tag=%q 超出 0..9999", tag)
	}

	requester := postJSON(t, h, "/api/v1/auth/register", "", map[string]string{
		"nickname": "requester", "email": "requester@test.com", "password": "password123",
	}, 201)
	reqTok, _ := requester["access_token"].(string)
	postJSON(t, h, "/api/v1/friends/requests", reqTok,
		map[string]any{"nickname": nickname + "#" + tag}, http.StatusCreated)
}

// TestRateLimitCodesUnified 频控超限一律 429 rate_limited（#22 / §0.1.3）。
func TestRateLimitCodesUnified(t *testing.T) {
	srv, _ := newTestServerMode(t, AuthModeFull)
	h := srv.Handler()
	// 登录频控 5 次/min：第 6 次应 429 且 code=rate_limited。
	for i := 0; i < 6; i++ {
		body := map[string]any{"email": "rl@test.com", "password": "wrong-password-123"}
		buf := new(bytes.Buffer)
		_ = json.NewEncoder(buf).Encode(body)
		req := httptest.NewRequest(http.MethodPost, "/api/v1/auth/login", buf)
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set(clientVersionHeader, testClientVersion)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if i == 5 {
			if rec.Code != http.StatusTooManyRequests {
				t.Fatalf("第 6 次应 429，实际 %d", rec.Code)
			}
			if !strings.Contains(rec.Body.String(), "rate_limited") {
				t.Fatalf("code 应为 rate_limited，实际 %s", rec.Body.String())
			}
		}
	}
}

// TestContentTypeCharset 响应头与旧 writeJSON 同口径：application/json 必带 charset
// （strict 迁移后生成 Visit 写裸 application/json，由 contentTypeFixer 改写）。
func TestContentTypeCharset(t *testing.T) {
	srv, _ := newTestServerMode(t, AuthModeFull)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/config", nil)
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("期望 200，实际 %d", rec.Code)
	}
	const want = "application/json; charset=utf-8"
	if got := rec.Header().Get("Content-Type"); got != want {
		t.Fatalf("Content-Type 期望 %q，实际 %q", want, got)
	}
}
