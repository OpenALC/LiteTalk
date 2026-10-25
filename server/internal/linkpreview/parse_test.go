package linkpreview

import (
	"errors"
	"net/url"
	"strings"
	"testing"
)

func mustURL(t *testing.T, raw string) *url.URL {
	t.Helper()
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatalf("测试夹具 URL 非法: %v", err)
	}
	return u
}

func TestParseHTMLOpenGraph(t *testing.T) {
	page := mustURL(t, "https://example.com/news/1")
	body := `<!doctype html><html><head>
		<title>兜底标题</title>
		<meta property="og:title" content="  OG   标题&#10;第二行 ">
		<meta property="og:description" content="描述正文">
		<meta property="og:site_name" content="示例站">
		<meta property="og:image" content="/img/cover.png">
		<meta name="description" content="普通描述（应被 OG 覆盖）">
	</head><body></body></html>`
	p, err := parseHTML(page, []byte(body))
	if err != nil {
		t.Fatalf("parseHTML: %v", err)
	}
	if p.URL != "https://example.com/news/1" {
		t.Errorf("URL = %q", p.URL)
	}
	if p.Title != "OG 标题 第二行" {
		t.Errorf("Title = %q", p.Title)
	}
	if p.Description != "描述正文" {
		t.Errorf("Description = %q", p.Description)
	}
	if p.SiteName != "示例站" {
		t.Errorf("SiteName = %q", p.SiteName)
	}
	if p.ImageURL != "https://example.com/img/cover.png" {
		t.Errorf("ImageURL = %q", p.ImageURL)
	}
}

func TestParseHTMLTitleOnly(t *testing.T) {
	page := mustURL(t, "https://example.com/")
	p, err := parseHTML(page, []byte(`<html><head><title>只有一个标题</title></head></html>`))
	if err != nil {
		t.Fatalf("parseHTML: %v", err)
	}
	if p.Title != "只有一个标题" || p.Description != "" || p.ImageURL != "" {
		t.Fatalf("Preview 不符: %+v", p)
	}
}

func TestParseHTMLMetaDescriptionOnly(t *testing.T) {
	page := mustURL(t, "https://example.com/a")
	p, err := parseHTML(page, []byte(`<html><head><meta name="description" content="只有描述"></head></html>`))
	if err != nil {
		t.Fatalf("parseHTML: %v", err)
	}
	// 标题为空时回退 host。
	if p.Title != "example.com" || p.Description != "只有描述" {
		t.Fatalf("Preview 不符: %+v", p)
	}
}

func TestParseHTMLTwitterImageFallback(t *testing.T) {
	page := mustURL(t, "https://example.com/a")
	body := `<html><head>
		<title>推文</title>
		<meta name="twitter:image" content="//cdn.example.com/t.png">
	</head></html>`
	p, err := parseHTML(page, []byte(body))
	if err != nil {
		t.Fatalf("parseHTML: %v", err)
	}
	if p.ImageURL != "https://cdn.example.com/t.png" {
		t.Fatalf("ImageURL = %q", p.ImageURL)
	}
}

func TestParseHTMLDropsUnsafeImage(t *testing.T) {
	page := mustURL(t, "https://example.com/a")
	for _, raw := range []string{
		"http://127.0.0.1/x.png",
		"http://169.254.169.254/x.png",
		"javascript:alert(1)",
		"file:///etc/passwd",
	} {
		body := `<html><head><title>t</title><meta property="og:image" content="` + raw + `"></head></html>`
		p, err := parseHTML(page, []byte(body))
		if err != nil {
			t.Fatalf("parseHTML(%q): %v", raw, err)
		}
		if p.ImageURL != "" {
			t.Errorf("危险图片地址 %q 未被丢弃: %q", raw, p.ImageURL)
		}
	}
}

func TestParseHTMLNoMetadata(t *testing.T) {
	page := mustURL(t, "https://example.com/")
	for _, body := range []string{
		``,
		`<html><head></head><body>纯文本</body></html>`,
		`<html><head><title></title><meta name="description" content="   "></head></html>`,
	} {
		if _, err := parseHTML(page, []byte(body)); !errors.Is(err, ErrNoPreview) {
			t.Fatalf("body=%q: 错误 = %v，期望 ErrNoPreview", body, err)
		}
	}
}

func TestParseHTMLTruncatesAndStripsControl(t *testing.T) {
	page := mustURL(t, "https://example.com/")
	body := `<html><head><title>` + strings.Repeat("标题", 150) + "\x00\x07" +
		`</title><meta name="description" content="` + strings.Repeat("描", 600) + `"></head></html>`
	p, err := parseHTML(page, []byte(body))
	if err != nil {
		t.Fatalf("parseHTML: %v", err)
	}
	if n := len([]rune(p.Title)); n > maxTitleRunes {
		t.Errorf("Title rune 数 %d 超过上限 %d", n, maxTitleRunes)
	}
	if n := len([]rune(p.Description)); n > maxDescRunes {
		t.Errorf("Description rune 数 %d 超过上限 %d", n, maxDescRunes)
	}
	for _, r := range p.Title + p.Description {
		if r < 0x20 {
			t.Fatalf("文本残留控制字符 %q", r)
		}
	}
}

func TestCleanText(t *testing.T) {
	got := cleanText("  a\n\t b  c\x00de  ", 0)
	if got != "a b c de" {
		t.Fatalf("cleanText = %q", got)
	}
	if got := cleanText("一二三四", 2); got != "一二" {
		t.Fatalf("截断 = %q", got)
	}
}
