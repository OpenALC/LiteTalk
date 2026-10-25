package linkpreview

import (
	"bytes"
	"net/url"
	"strings"
	"unicode"

	"golang.org/x/net/html"
)

// 元数据文本上限（rune）：标题 200、描述 500、站点名 100。
const (
	maxTitleRunes = 200
	maxDescRunes  = 500
	maxSiteRunes  = 100
)

// pageMeta 收集到的原始元数据；优先级在 titles/descriptions 里合并。
type pageMeta struct {
	ogTitle      string
	title        string
	ogDesc       string
	desc         string
	ogSite       string
	ogImage      string
	twitterImage string
}

// parseHTML 从 HTML 字节提取预览。页面无标题也无描述时返回 ErrNoPreview。
// 图片地址相对页面 URL 解析，非法（非 http(s)/被禁 IP/userinfo）直接丢弃。
func parseHTML(pageURL *url.URL, body []byte) (*Preview, error) {
	doc, err := html.Parse(bytes.NewReader(body))
	if err != nil {
		return nil, ErrNoPreview
	}
	var meta pageMeta
	var walk func(*html.Node)
	walk = func(n *html.Node) {
		if n.Type == html.ElementNode {
			switch n.Data {
			case "title":
				if meta.title == "" {
					meta.title = textContent(n)
				}
			case "meta":
				key := strings.ToLower(strings.TrimSpace(attrValue(n, "property")))
				if key == "" {
					key = strings.ToLower(strings.TrimSpace(attrValue(n, "name")))
				}
				content := attrValue(n, "content")
				switch key {
				case "og:title":
					if meta.ogTitle == "" {
						meta.ogTitle = content
					}
				case "description":
					if meta.desc == "" {
						meta.desc = content
					}
				case "og:description":
					if meta.ogDesc == "" {
						meta.ogDesc = content
					}
				case "og:site_name":
					if meta.ogSite == "" {
						meta.ogSite = content
					}
				case "og:image", "og:image:url":
					if meta.ogImage == "" {
						meta.ogImage = content
					}
				case "twitter:image", "twitter:image:src":
					if meta.twitterImage == "" {
						meta.twitterImage = content
					}
				}
			}
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
	}
	walk(doc)

	title := cleanText(firstNonEmpty(meta.ogTitle, meta.title), maxTitleRunes)
	desc := cleanText(firstNonEmpty(meta.ogDesc, meta.desc), maxDescRunes)
	site := cleanText(meta.ogSite, maxSiteRunes)
	image := firstNonEmpty(strings.TrimSpace(meta.ogImage), strings.TrimSpace(meta.twitterImage))
	// 无标题且无描述视为没有预览；标题的回退（站点名/host）只用于展示，
	// 不把每个空页面都变成「有预览」。
	if title == "" && desc == "" {
		return nil, ErrNoPreview
	}
	if title == "" {
		if site != "" {
			title = site
		} else {
			title = cleanText(pageURL.Hostname(), maxTitleRunes)
		}
	}
	return &Preview{
		URL:         pageURL.String(),
		Title:       title,
		Description: desc,
		SiteName:    site,
		ImageURL:    resolveImageURL(pageURL, image),
	}, nil
}

// attrValue 取元素属性值（HTML 属性名由解析器小写化）。
func attrValue(n *html.Node, key string) string {
	for _, a := range n.Attr {
		if a.Key == key {
			return a.Val
		}
	}
	return ""
}

// textContent 拼接元素下全部文本节点（title 内不会嵌套标记，够用即可）。
func textContent(n *html.Node) string {
	var b strings.Builder
	var walk func(*html.Node)
	walk = func(n *html.Node) {
		if n.Type == html.TextNode {
			b.WriteString(n.Data)
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
	}
	walk(n)
	return b.String()
}

// cleanText 折叠空白、去掉控制字符并按 rune 截断。C0/C1 控制符先换成空格再
// 交给 Fields 折叠；其余 Unicode 控制符直接丢弃。
func cleanText(s string, maxRunes int) string {
	s = strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return ' '
		}
		if unicode.IsControl(r) {
			return -1
		}
		return r
	}, s)
	s = strings.Join(strings.Fields(s), " ")
	if maxRunes > 0 {
		runes := []rune(s)
		if len(runes) > maxRunes {
			s = string(runes[:maxRunes])
		}
	}
	return s
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}
