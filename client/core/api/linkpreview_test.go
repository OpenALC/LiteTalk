package api

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
)

// #42 B4：GET /link-preview 的 query 编码、成功解码、404 no_preview 归一与错误透传。

func TestGetLinkPreviewQueryAndDecode(t *testing.T) {
	var gotPath, gotURL string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotURL = r.URL.Query().Get("url")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"url":"https://example.com/final","title":"标题",
			"description":"描述","site_name":"示例站","image_url":"https://example.com/a.png"}`))
	}))
	defer srv.Close()

	c := NewAPIClient(srv.URL)
	c.SetToken("tok")
	raw := "https://example.com/a?b=1&c=中文#frag"
	p, err := c.GetLinkPreview(raw)
	if err != nil {
		t.Fatalf("GetLinkPreview: %v", err)
	}
	if gotPath != "/api/v1/link-preview" {
		t.Fatalf("路径 = %q", gotPath)
	}
	// url.Values 编码：& 与中文不能把 query 拆坏。
	if gotURL != raw {
		t.Fatalf("url = %q，期望 %q", gotURL, raw)
	}
	if p == nil || p.URL != "https://example.com/final" || p.Title != "标题" ||
		p.Description != "描述" || p.SiteName != "示例站" || p.ImageURL != "https://example.com/a.png" {
		t.Fatalf("解码结果异常: %+v", p)
	}
}

func TestGetLinkPreviewNoPreviewIsNil(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/problem+json")
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"type":"urn:litetalk:error:no_preview","title":"Not Found",
			"status":404,"code":"no_preview","detail":"无法获取链接预览"}`))
	}))
	defer srv.Close()

	c := NewAPIClient(srv.URL)
	p, err := c.GetLinkPreview("https://example.com/")
	if err != nil {
		t.Fatalf("no_preview 应返回 nil,nil，实际错误: %v", err)
	}
	if p != nil {
		t.Fatalf("no_preview 应返回 nil DTO，实际 %+v", p)
	}
}

func TestGetLinkPreviewOther404IsError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/problem+json")
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"code":"not_found","detail":"资源不存在"}`))
	}))
	defer srv.Close()

	c := NewAPIClient(srv.URL)
	if _, err := c.GetLinkPreview("https://example.com/"); err == nil {
		t.Fatal("非 no_preview 的 404 应返回错误")
	}
}

func TestGetLinkPreviewErrors(t *testing.T) {
	cases := []struct {
		status int
		body   string
		code   string
	}{
		{http.StatusBadRequest, `{"code":"bad_url","detail":"链接地址无效"}`, "bad_url"},
		{http.StatusTooManyRequests, `{"code":"rate_limited","detail":"链接预览请求过于频繁"}`, "rate_limited"},
		{http.StatusInternalServerError, `{"code":"internal","detail":"服务器内部错误"}`, "internal"},
	}
	for _, tc := range cases {
		t.Run(tc.code, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/problem+json")
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(tc.body))
			}))
			defer srv.Close()

			c := NewAPIClient(srv.URL)
			_, err := c.GetLinkPreview("http://127.0.0.1/")
			var ae *APIError
			if !errors.As(err, &ae) {
				t.Fatalf("错误类型 = %T（%v），期望 *APIError", err, err)
			}
			if ae.Status != tc.status || ae.Code != tc.code {
				t.Fatalf("错误 = %+v，期望 status=%d code=%s", ae, tc.status, tc.code)
			}
		})
	}
}
