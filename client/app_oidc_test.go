package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// OidcAuthorize 桌面绑定（#118 P2-5）：测试环境 wails 恒为 nil，正好锁住
// 「REST 成功但桌面环境未就绪」与「URL 校验」两个分支——不真开浏览器。
func TestOidcAuthorizeBinding(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"url":"https://idp.example/authorize?state=s1"}`))
	}))
	defer srv.Close()

	app := &App{api: newAppAPI(srv.URL)}
	_, err := app.OidcAuthorize()
	if err == nil || !strings.Contains(err.Error(), "桌面环境未就绪") {
		t.Fatalf("wails 未就绪应报错，得到: %v", err)
	}
}

// TestOidcAuthorizeBindingBadURL 授权 URL 校验沿用 OpenAttachment 口径：
// 无 host 与非 http/https 各自拒绝，都在打开浏览器之前。
func TestOidcAuthorizeBindingBadURL(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		// 按 Host 校验命中顺序给两种非法值：无 host 的脚本 scheme、带 host 的 ftp。
		_, _ = w.Write([]byte(`{"url":"ftp://idp.example/authorize?state=s1"}`))
	}))
	defer srv.Close()

	app := &App{api: newAppAPI(srv.URL)}
	if _, err := app.OidcAuthorize(); err == nil || !strings.Contains(err.Error(), "http/https") {
		t.Fatalf("非 http(s) 授权地址应拒绝，得到: %v", err)
	}
}
