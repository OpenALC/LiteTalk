package push

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestNtfyRequestShape httptest 桩测请求形状（CI 唯一可真测的通道）：
// POST 根 URL、application/json、{"topic","title","message","click"}。
func TestNtfyRequestShape(t *testing.T) {
	var gotMethod, gotCT, gotBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		gotCT = r.Header.Get("Content-Type")
		buf := make([]byte, 4096)
		n, _ := r.Body.Read(buf)
		gotBody = string(buf[:n])
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	n := &Ntfy{URL: srv.URL, base: srv.URL}
	if err := n.Push(context.Background(),
		[]Device{{Platform: "android", Token: "topic-xyz"}},
		Notification{Title: "张三", Body: "在 #general 提到你", Route: "channel:7"}); err != nil {
		t.Fatalf("投递不应失败: %v", err)
	}
	if gotMethod != http.MethodPost {
		t.Fatalf("应 POST，实际 %s", gotMethod)
	}
	if !strings.HasPrefix(gotCT, "application/json") {
		t.Fatalf("Content-Type 应 application/json，实际 %q", gotCT)
	}
	var payload map[string]any
	if err := json.Unmarshal([]byte(gotBody), &payload); err != nil {
		t.Fatalf("body 不是 JSON: %q", gotBody)
	}
	if payload["topic"] != "topic-xyz" || payload["title"] != "张三" ||
		payload["message"] != "在 #general 提到你" || payload["click"] != "channel:7" {
		t.Fatalf("载荷不符: %v", payload)
	}
}

// TestNtfyFailureMasksToken 投递失败日志只出现打码后的 token。
func TestNtfyFailureMasksToken(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	lg, buf := captureLog(t)
	n := &Ntfy{URL: srv.URL, base: srv.URL, Log: lg}
	token := "supersecrettopic0123456789"
	_ = n.Push(context.Background(), []Device{{Platform: "android", Token: token}}, Notification{})
	out := buf.String()
	if strings.Contains(out, token) {
		t.Fatalf("日志泄露完整 token: %q", out)
	}
	if !strings.Contains(out, MaskToken(token)) {
		t.Fatalf("日志应含打码 token: %q", out)
	}
}

// TestNtfyPartialFailure 一台失败不影响其余设备（逐设备独立投递）。
func TestNtfyPartialFailure(t *testing.T) {
	var hits int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var p ntfyPayload
		_ = json.NewDecoder(r.Body).Decode(&p)
		if p.Topic == "bad-topic" {
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		hits++
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	n := &Ntfy{URL: srv.URL, base: srv.URL}
	err := n.Push(context.Background(), []Device{
		{Platform: "android", Token: "bad-topic"},
		{Platform: "android", Token: "good-topic"},
	}, Notification{})
	if err != nil {
		t.Fatalf("部分失败不应整体失败: %v", err)
	}
	if hits != 1 {
		t.Fatalf("好设备应投递 1 次，实际 %d", hits)
	}
}
