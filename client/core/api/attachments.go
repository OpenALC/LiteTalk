package api

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"strings"
	"time"
)

// uploadTimeout 附件上传的超时：慢速链路上的大文件远超普通 REST 的 15s，
// 但不应该放宽其它 API 的默认超时，所以上传走单独的 http.Client。
const uploadTimeout = 2 * time.Minute

// AttachmentDTO 附件元数据（#41，与契约 Attachment 对齐）。
// url/thumbnail_url 是带下载能力 key 的相对路径（/api/v1/attachments/{id}?key=...），
// 免 Bearer；前端展示前须经 ResolveURL 拼成绝对地址。
// Width/Height 仅图片非零，ThumbnailURL 仅图片且缩略图生成成功时有值。
type AttachmentDTO struct {
	ID           ID     `json:"id"`
	Filename     string `json:"filename"`
	Size         int64  `json:"size"`
	ContentType  string `json:"content_type"`
	Width        int    `json:"width"`
	Height       int    `json:"height"`
	URL          string `json:"url"`
	ThumbnailURL string `json:"thumbnail_url,omitempty"`
}

// ResolveURL 把服务端返回的相对资源路径拼到当前 REST 基址上（附件能力 URL 用）。
// 已是 http(s):// 的绝对地址原样返回。基址在锁内读取，兼容换服热切换。
func (c *APIClient) ResolveURL(rel string) string {
	rel = strings.TrimSpace(rel)
	if rel == "" {
		return ""
	}
	lower := strings.ToLower(rel)
	if strings.HasPrefix(lower, "http://") || strings.HasPrefix(lower, "https://") {
		return rel
	}
	c.baseMu.RLock()
	base := c.base
	c.baseMu.RUnlock()
	if base == "" {
		return rel
	}
	return strings.TrimRight(base, "/") + "/" + strings.TrimLeft(rel, "/")
}

// UploadAttachment 上传单个附件（multipart 只带 file 字段），返回服务端元数据。
// 非 201 一律 decodeError（bad_type/bad_image/too_large/rate_limited 等机器码
// 由调用方原样展示）。上传不参与 401 刷新重试：大文件重放代价高，令牌过期时
// 让用户重试一次即可（下一次普通请求会先刷新）。
func (c *APIClient) UploadAttachment(filename string, data []byte) (*AttachmentDTO, error) {
	if filename == "" {
		filename = "attachment"
	}
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	part, err := mw.CreateFormFile("file", filename)
	if err != nil {
		return nil, fmt.Errorf("upload attachment: %w", err)
	}
	if _, err := part.Write(data); err != nil {
		return nil, fmt.Errorf("upload attachment: %w", err)
	}
	if err := mw.Close(); err != nil {
		return nil, fmt.Errorf("upload attachment: %w", err)
	}

	base := c.Base()
	ctx, cancel := context.WithTimeout(context.Background(), uploadTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, base+"/api/v1/attachments", &buf)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", mw.FormDataContentType())
	c.setVersionHeader(req)
	if tok := c.Token(); tok != "" {
		req.Header.Set("Authorization", "Bearer "+tok)
	}
	client := c.uploadHTTP
	if client == nil {
		client = c.http
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("upload attachment: %w", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusCreated {
		// 429 的 Retry-After 只在响应头上，与 do() 一样在传输层带进 APIError。
		if resp.StatusCode == http.StatusTooManyRequests {
			if ae, ok := decodeError(resp.StatusCode, body).(*APIError); ok {
				ae.RetryAfter = retryAfterFromHeader(resp.Header)
				return nil, ae
			}
		}
		return nil, decodeError(resp.StatusCode, body)
	}
	var att AttachmentDTO
	if err := json.Unmarshal(body, &att); err != nil {
		return nil, fmt.Errorf("decode attachment: %w", err)
	}
	return &att, nil
}
