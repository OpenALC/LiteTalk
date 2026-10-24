package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
)

// LinkPreviewDTO 链接预览（#42 B4，与契约 LinkPreview 对齐）。
// 文本已由服务端清洗；ImageURL 为绝对地址，可直接放进 img 标签。
type LinkPreviewDTO struct {
	URL         string `json:"url"`
	Title       string `json:"title"`
	Description string `json:"description,omitempty"`
	SiteName    string `json:"site_name,omitempty"`
	ImageURL    string `json:"image_url,omitempty"`
}

// GetLinkPreview 抓取链接预览。url 原样交给服务端（用 url.Values 编码，避免
// &/中文等字符把 query 拆坏）。404 no_preview 返回 (nil, nil)：无预览是正常
// 状态，UI 静默隐藏卡片即可；其余错误按 decodeError 上抛。
func (c *APIClient) GetLinkPreview(rawURL string) (*LinkPreviewDTO, error) {
	params := url.Values{}
	params.Set("url", rawURL)
	status, data, err := c.get("/link-preview?" + params.Encode())
	if err != nil {
		return nil, err
	}
	if status == http.StatusNotFound {
		var e APIError
		if json.Unmarshal(data, &e) == nil && e.Code == "no_preview" {
			return nil, nil
		}
		return nil, decodeError(status, data)
	}
	if status != http.StatusOK {
		return nil, decodeError(status, data)
	}
	var p LinkPreviewDTO
	if err := json.Unmarshal(data, &p); err != nil {
		return nil, fmt.Errorf("decode link preview: %w", err)
	}
	return &p, nil
}
