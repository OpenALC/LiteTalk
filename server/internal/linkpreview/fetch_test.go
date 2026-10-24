package linkpreview

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

// roundTripFunc 把函数适配成 http.RoundTripper（成功抓取无法用 httptest：
// 127.0.0.1 是被禁的，抓取器会拒绝拨号）。
type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// fakeResponse 构造固定响应的 RoundTripper；Request.URL 回填请求地址，
// 模拟跟随重定向后的最终页面地址。
func fakeResponse(t *testing.T, status int, contentType, body string) http.RoundTripper {
	t.Helper()
	return roundTripFunc(func(req *http.Request) (*http.Response, error) {
		h := http.Header{}
		if contentType != "" {
			h.Set("Content-Type", contentType)
		}
		return &http.Response{
			StatusCode: status,
			Header:     h,
			Body:       io.NopCloser(strings.NewReader(body)),
			Request:    &http.Request{URL: req.URL},
		}, nil
	})
}

func TestFetchSuccess(t *testing.T) {
	f := New(Config{Timeout: time.Second, MaxBytes: 32 << 10})
	body := `<html><head>
		<title>页面标题</title>
		<meta property="og:description" content="页面描述">
		<meta property="og:site_name" content="示例站">
		<meta property="og:image" content="/cover.png">
	</head></html>`
	var gotUA, gotAccept string
	f.client.Transport = roundTripFunc(func(req *http.Request) (*http.Response, error) {
		gotUA = req.Header.Get("User-Agent")
		gotAccept = req.Header.Get("Accept")
		return fakeResponse(t, http.StatusOK, "text/html; charset=utf-8", body).RoundTrip(req)
	})
	p, err := f.Fetch(context.Background(), "https://example.com/news/1")
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if p.URL != "https://example.com/news/1" || p.Title != "页面标题" ||
		p.Description != "页面描述" || p.SiteName != "示例站" ||
		p.ImageURL != "https://example.com/cover.png" {
		t.Fatalf("Preview 不符: %+v", p)
	}
	if !strings.HasPrefix(gotUA, "LiteTalkBot/") {
		t.Errorf("User-Agent = %q", gotUA)
	}
	if !strings.Contains(gotAccept, "text/html") {
		t.Errorf("Accept = %q", gotAccept)
	}
}

func TestFetchRejectsBlockedURLWithoutDial(t *testing.T) {
	f := New(Config{})
	dialed := false
	f.client.Transport = roundTripFunc(func(req *http.Request) (*http.Response, error) {
		dialed = true
		return nil, errors.New("不应请求")
	})
	for _, raw := range []string{
		"http://127.0.0.1/",
		"http://[::1]/",
		"http://169.254.169.254/latest/meta-data/",
		"file:///etc/passwd",
		"http://user:pass@example.com/",
	} {
		if _, err := f.Fetch(context.Background(), raw); !errors.Is(err, ErrBlocked) {
			t.Errorf("Fetch(%q) 错误 = %v，期望 ErrBlocked", raw, err)
		}
	}
	if dialed {
		t.Fatal("被禁 URL 不应该进入 HTTP 传输层")
	}
}

func TestFetchRejectsNonHTML(t *testing.T) {
	cases := []struct {
		name        string
		status      int
		contentType string
	}{
		{"非 200", http.StatusNotFound, "text/html"},
		{"纯文本", http.StatusOK, "text/plain"},
		{"JSON", http.StatusOK, "application/json"},
		{"缺 Content-Type", http.StatusOK, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := New(Config{})
			f.client.Transport = fakeResponse(t, tc.status, tc.contentType, `<title>x</title>`)
			if _, err := f.Fetch(context.Background(), "https://example.com/"); !errors.Is(err, ErrNoPreview) {
				t.Fatalf("错误 = %v，期望 ErrNoPreview", err)
			}
		})
	}
}

func TestFetchMaxBytes(t *testing.T) {
	f := New(Config{Timeout: time.Second, MaxBytes: 64})
	f.client.Transport = fakeResponse(t, http.StatusOK, "text/html", `<title>ok</title>`+strings.Repeat("a", 64))
	if _, err := f.Fetch(context.Background(), "https://example.com/"); !errors.Is(err, ErrNoPreview) {
		t.Fatalf("超限错误 = %v，期望 ErrNoPreview", err)
	}

	// 正好等于上限仍然接受。
	exact := `<title>ok</title>`
	padded := exact + strings.Repeat("a", 64-len(exact))
	f.client.Transport = fakeResponse(t, http.StatusOK, "text/html", padded)
	if _, err := f.Fetch(context.Background(), "https://example.com/"); err != nil {
		t.Fatalf("正好等于上限应成功: %v", err)
	}
}

func TestFetchTimeout(t *testing.T) {
	f := New(Config{Timeout: 30 * time.Millisecond})
	release := make(chan struct{})
	defer close(release)
	f.client.Transport = roundTripFunc(func(req *http.Request) (*http.Response, error) {
		select {
		case <-req.Context().Done():
			return nil, req.Context().Err()
		case <-release:
			return nil, errors.New("released")
		}
	})
	start := time.Now()
	_, err := f.Fetch(context.Background(), "https://example.com/")
	if !errors.Is(err, ErrNoPreview) {
		t.Fatalf("错误 = %v，期望 ErrNoPreview", err)
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("超时未生效，耗时 %v", elapsed)
	}
}

func TestFetchRedirectToPrivateBlocked(t *testing.T) {
	f := New(Config{Timeout: time.Second})
	f.client.Transport = roundTripFunc(func(req *http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusFound,
			Header:     http.Header{"Location": {"http://127.0.0.1/admin"}},
			Body:       io.NopCloser(strings.NewReader("")),
			Request:    &http.Request{URL: req.URL},
		}, nil
	})
	if _, err := f.Fetch(context.Background(), "https://example.com/"); !errors.Is(err, ErrBlocked) {
		t.Fatalf("错误 = %v，期望 ErrBlocked", err)
	}
}

func TestFetchTooManyRedirects(t *testing.T) {
	f := New(Config{Timeout: time.Second})
	f.client.Transport = roundTripFunc(func(req *http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusFound,
			Header:     http.Header{"Location": {"/loop"}},
			Body:       io.NopCloser(strings.NewReader("")),
			Request:    &http.Request{URL: req.URL},
		}, nil
	})
	if _, err := f.Fetch(context.Background(), "https://example.com/"); !errors.Is(err, ErrBlocked) {
		t.Fatalf("错误 = %v，期望 ErrBlocked", err)
	}
}
