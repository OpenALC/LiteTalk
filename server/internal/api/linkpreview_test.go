package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/OpenALC/LiteTalk/server/internal/auth"
	"github.com/OpenALC/LiteTalk/server/internal/linkpreview"
	"github.com/OpenALC/LiteTalk/server/internal/store"
)

// #42 B4：GET /api/v1/link-preview 的鉴权后行为（频控/缓存/错误映射/开关）。
// 抓取本身由 linkpreview 包测试；这里只注入 fake fetcher。

type fakePreviewFetcher struct {
	mu      sync.Mutex
	calls   int
	urls    []string
	preview *linkpreview.Preview
	err     error
}

func (f *fakePreviewFetcher) Fetch(_ context.Context, rawURL string) (*linkpreview.Preview, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	f.urls = append(f.urls, rawURL)
	if f.err != nil {
		return nil, f.err
	}
	return f.preview, nil
}

func (f *fakePreviewFetcher) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

func newLinkPreviewWorld(t *testing.T) (*Server, http.Handler, string, *fakePreviewFetcher) {
	t.Helper()
	mem := store.NewMem()
	fetcher := &fakePreviewFetcher{preview: &linkpreview.Preview{
		URL:         "https://example.com/final",
		Title:       "示例标题",
		Description: "示例描述",
		SiteName:    "示例站",
		ImageURL:    "https://example.com/a.png",
	}}
	srv := New(Config{
		Store:       mem,
		JWT:         auth.NewJWT("test-secret"),
		Refresh:     auth.NewRefreshStore(mem),
		Codes:       auth.NewEmailCodes(mem),
		Limits:      auth.NewRateLimit(mem),
		Session:     auth.NewSessionIndex(mem, mem),
		KV:          store.NewMemKV(),
		LinkPreview: fetcher,
	})
	token, _ := addUser(t, srv, mem, "lp@example.com", "lpUser")
	return srv, srv.Handler(), token, fetcher
}

// linkPreviewPath 构造带编码 url 参数的请求路径。
func linkPreviewPath(raw string) string {
	return "/api/v1/link-preview?url=" + url.QueryEscape(raw)
}

func TestLinkPreviewSuccessAndCache(t *testing.T) {
	srv, h, token, fetcher := newLinkPreviewWorld(t)
	raw := "https://example.com/page?x=1"

	body, _ := getJSON(t, h, linkPreviewPath(raw), token, http.StatusOK).(map[string]any)
	if body["url"] != "https://example.com/final" || body["title"] != "示例标题" ||
		body["description"] != "示例描述" || body["site_name"] != "示例站" ||
		body["image_url"] != "https://example.com/a.png" {
		t.Fatalf("响应不符: %#v", body)
	}
	if fetcher.callCount() != 1 {
		t.Fatalf("fetcher 调用 %d 次，期望 1", fetcher.callCount())
	}
	// 命中正缓存：第二次不再抓取。
	getJSON(t, h, linkPreviewPath(raw), token, http.StatusOK)
	if fetcher.callCount() != 1 {
		t.Fatalf("正缓存未命中，fetcher 调用 %d 次", fetcher.callCount())
	}
	// 缓存值是可反序列化的 Preview JSON，不是负缓存标记。
	val, err := srv.kv.Get(context.Background(), linkPreviewCacheKey(raw))
	if err != nil || val == linkPreviewNegativeValue {
		t.Fatalf("正缓存内容异常: val=%q err=%v", val, err)
	}
	var cached linkpreview.Preview
	if err := json.Unmarshal([]byte(val), &cached); err != nil || cached.Title != "示例标题" {
		t.Fatalf("正缓存反序列化失败: %v %+v", err, cached)
	}
}

func TestLinkPreviewNegativeCache(t *testing.T) {
	srv, h, token, fetcher := newLinkPreviewWorld(t)
	fetcher.err = linkpreview.ErrNoPreview
	raw := "https://example.com/no-preview"

	body, _ := getJSON(t, h, linkPreviewPath(raw), token, http.StatusNotFound).(map[string]any)
	if body["code"] != "no_preview" {
		t.Fatalf("code = %v，期望 no_preview", body["code"])
	}
	getJSON(t, h, linkPreviewPath(raw), token, http.StatusNotFound)
	if fetcher.callCount() != 1 {
		t.Fatalf("负缓存未命中，fetcher 调用 %d 次", fetcher.callCount())
	}
	val, err := srv.kv.Get(context.Background(), linkPreviewCacheKey(raw))
	if err != nil || val != linkPreviewNegativeValue {
		t.Fatalf("负缓存内容异常: val=%q err=%v", val, err)
	}
}

func TestLinkPreviewBlocked(t *testing.T) {
	_, h, token, fetcher := newLinkPreviewWorld(t)
	fetcher.err = linkpreview.ErrBlocked
	raw := "http://127.0.0.1/admin"

	body, _ := getJSON(t, h, linkPreviewPath(raw), token, http.StatusBadRequest).(map[string]any)
	if body["code"] != "bad_url" {
		t.Fatalf("code = %v，期望 bad_url", body["code"])
	}
	if msg, _ := body["detail"].(string); strings.Contains(msg, "127.0.0.1") || strings.Contains(msg, "私网") {
		t.Fatalf("错误文案泄露解析细节: %q", msg)
	}
	// 被拒请求不写缓存（无预览负缓存只用于 ErrNoPreview）。
	getJSON(t, h, linkPreviewPath(raw), token, http.StatusBadRequest)
	if fetcher.callCount() != 2 {
		t.Fatalf("被拒地址不应缓存，fetcher 调用 %d 次", fetcher.callCount())
	}
}

func TestLinkPreviewDisabled(t *testing.T) {
	srv, mem := newTestServer(t)
	token, _ := addUser(t, srv, mem, "lp-off@example.com", "lpOff")
	srv.linkPreview = nil
	// 功能关闭时任何（甚至非法的）url 都统一 404 no_preview。
	for _, raw := range []string{"https://example.com/", "nonsense"} {
		body, _ := getJSON(t, srv.Handler(), linkPreviewPath(raw), token, http.StatusNotFound).(map[string]any)
		if body["code"] != "no_preview" {
			t.Fatalf("关闭时 code = %v，期望 no_preview", body["code"])
		}
	}
}

func TestLinkPreviewWithoutKV(t *testing.T) {
	srv, mem := newTestServer(t)
	token, _ := addUser(t, srv, mem, "lp-nokv@example.com", "lpNoKV")
	fetcher := &fakePreviewFetcher{preview: &linkpreview.Preview{URL: "https://example.com/", Title: "t"}}
	srv.linkPreview = fetcher // KV 保持 nil：缓存是功能的一部分，按不可用处理。
	body, _ := getJSON(t, srv.Handler(), linkPreviewPath("https://example.com/"), token, http.StatusNotFound).(map[string]any)
	if body["code"] != "no_preview" {
		t.Fatalf("KV 未注入时 code = %v，期望 no_preview", body["code"])
	}
	if fetcher.callCount() != 0 {
		t.Fatalf("KV 未注入时不应抓取，实际 %d 次", fetcher.callCount())
	}
}

func TestLinkPreviewInvalidURL(t *testing.T) {
	_, h, token, fetcher := newLinkPreviewWorld(t)
	long := "https://example.com/" + strings.Repeat("a", linkpreview.MaxURLLength)
	for _, raw := range []string{"", long} {
		body, _ := getJSON(t, h, linkPreviewPath(raw), token, http.StatusBadRequest).(map[string]any)
		if body["code"] != "bad_url" {
			t.Fatalf("url=%q code = %v，期望 bad_url", raw, body["code"])
		}
	}
	if fetcher.callCount() != 0 {
		t.Fatalf("非法 URL 不应触发抓取，实际 %d 次", fetcher.callCount())
	}
}

func TestLinkPreviewRateLimit(t *testing.T) {
	_, h, token, _ := newLinkPreviewWorld(t)
	// 每次用不同 url 保证都在抓取路径上（正/负缓存都不命中）。
	for i := 0; i < linkPreviewRateLimit; i++ {
		raw := "https://example.com/page/" + strings.Repeat("x", i+1)
		getJSON(t, h, linkPreviewPath(raw), token, http.StatusOK)
	}
	body, _ := getJSON(t, h, linkPreviewPath("https://example.com/over"), token, http.StatusTooManyRequests).(map[string]any)
	if body["code"] != "rate_limited" {
		t.Fatalf("code = %v，期望 rate_limited", body["code"])
	}
}

// failingKV 模拟 KV 故障：读/写都报错，接口必须 fail-open 继续抓取。
type failingKV struct{ store.KV }

func (failingKV) Get(context.Context, string) (string, error) { return "", errors.New("kv down") }
func (failingKV) Set(context.Context, string, string, time.Duration) error {
	return errors.New("kv down")
}

func TestLinkPreviewKVFailOpen(t *testing.T) {
	srv, h, token, fetcher := newLinkPreviewWorld(t)
	srv.kv = failingKV{}
	raw := "https://example.com/kv-down"
	getJSON(t, h, linkPreviewPath(raw), token, http.StatusOK)
	getJSON(t, h, linkPreviewPath(raw), token, http.StatusOK)
	if fetcher.callCount() != 2 {
		t.Fatalf("KV 故障时应每次重新抓取，fetcher 调用 %d 次", fetcher.callCount())
	}
	_ = srv
}
