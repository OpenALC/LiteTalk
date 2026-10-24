package api

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/OpenALC/LiteTalk/server/internal/linkpreview"
	"github.com/OpenALC/LiteTalk/server/internal/store"
)

// #42 B4：链接预览 REST 面。
//
// 抓取在 linkpreview 包里完成（SSRF 防护是那里的责任）；本层只做鉴权、频控、
// 缓存与错误码映射。缓存按原始 URL（trim 后）的 sha256 存 KV：有预览 24h、
// 无预览 1h，避免反复打同一个不可预览的站点。KV 故障 fail-open——缓存只是
// 优化，不能因为 Redis/内存 KV 抖动让整个功能不可用。

// linkPreviewRateLimit 每用户每分钟抓取次数上限。
const linkPreviewRateLimit = 30

const (
	linkPreviewPositiveTTL   = 24 * time.Hour
	linkPreviewNegativeTTL   = time.Hour
	linkPreviewNegativeValue = "-"
	linkPreviewKeyPrefix     = "linkpreview:"
)

// GetLinkPreview GET /api/v1/link-preview?url=（#42 B4）
func (s *Server) GetLinkPreview(ctx context.Context, request GetLinkPreviewRequestObject) (GetLinkPreviewResponseObject, error) {
	// 功能关闭：不构造抓取器，也不消耗频控/缓存，任何请求统一 404。
	if s.linkPreview == nil {
		return nil, errf(http.StatusNotFound, "no_preview", "无法获取链接预览")
	}
	raw := strings.TrimSpace(request.Params.Url)
	if raw == "" || len(raw) > linkpreview.MaxURLLength {
		return nil, errf(http.StatusBadRequest, "bad_url", "链接地址无效")
	}
	c := currentUser(ctx)
	if e := s.allowRate(ctx, "linkpreview:"+strconv.FormatInt(c.UserID, 10),
		linkPreviewRateLimit, time.Minute, "链接预览请求过于频繁"); e != nil {
		return nil, e
	}
	key := linkPreviewCacheKey(raw)
	if p, hit := s.cachedLinkPreview(ctx, key); hit {
		if p == nil {
			return nil, errf(http.StatusNotFound, "no_preview", "无法获取链接预览")
		}
		return linkPreviewResponse(p), nil
	}
	p, err := s.linkPreview.Fetch(ctx, raw)
	if err != nil {
		switch {
		case errors.Is(err, linkpreview.ErrBlocked):
			// 不把内网解析细节回传给调用方（避免把接口当 DNS/内网探测器）。
			return nil, errf(http.StatusBadRequest, "bad_url", "链接地址无效")
		case errors.Is(err, linkpreview.ErrNoPreview):
			s.putLinkPreviewCache(ctx, key, linkPreviewNegativeValue, linkPreviewNegativeTTL)
			return nil, errf(http.StatusNotFound, "no_preview", "无法获取链接预览")
		default:
			s.log.Printf("[500] %s: 链接预览抓取失败: %v", reqPath(ctx), err)
			return nil, errf(http.StatusInternalServerError, "internal", "服务器内部错误")
		}
	}
	if raw, err := json.Marshal(p); err == nil {
		s.putLinkPreviewCache(ctx, key, string(raw), linkPreviewPositiveTTL)
	} else {
		s.log.Printf("[linkpreview] 缓存序列化失败: %v", err)
	}
	return linkPreviewResponse(p), nil
}

// linkPreviewResponse 把抓取结果映射为契约响应。空字符串的可选字段省略。
func linkPreviewResponse(p *linkpreview.Preview) GetLinkPreview200JSONResponse {
	out := GetLinkPreview200JSONResponse{
		Url:   p.URL,
		Title: p.Title,
	}
	if p.Description != "" {
		out.Description = &p.Description
	}
	if p.SiteName != "" {
		out.SiteName = &p.SiteName
	}
	if p.ImageURL != "" {
		out.ImageUrl = &p.ImageURL
	}
	return out
}

// linkPreviewCacheKey 缓存键：前缀 + sha256(原始 URL) 的 hex。URL 可能很长且
// 含任意字符，散列后键长度固定、可直接进 Redis/SQLite KV。
func linkPreviewCacheKey(rawURL string) string {
	sum := sha256.Sum256([]byte(rawURL))
	return linkPreviewKeyPrefix + hex.EncodeToString(sum[:])
}

// cachedLinkPreview 读缓存。hit=false 表示未命中或缓存不可用；hit=true 且
// p=nil 表示负缓存。任何 KV 故障都按未命中处理（fail-open），非「键不存在」
// 的错误记日志。
func (s *Server) cachedLinkPreview(ctx context.Context, key string) (p *linkpreview.Preview, hit bool) {
	if s.kv == nil {
		return nil, false
	}
	val, err := s.kv.Get(ctx, key)
	if err != nil {
		if !errors.Is(err, store.ErrKVNotFound) {
			s.log.Printf("[linkpreview] 缓存读取失败（跳过缓存）: %v", err)
		}
		return nil, false
	}
	if val == linkPreviewNegativeValue {
		return nil, true
	}
	var cached linkpreview.Preview
	if err := json.Unmarshal([]byte(val), &cached); err != nil {
		s.log.Printf("[linkpreview] 缓存内容损坏（忽略）: %v", err)
		return nil, false
	}
	return &cached, true
}

// putLinkPreviewCache 写缓存；KV 故障或未配置时静默跳过（fail-open）。
func (s *Server) putLinkPreviewCache(ctx context.Context, key, value string, ttl time.Duration) {
	if s.kv == nil {
		return
	}
	if err := s.kv.Set(ctx, key, value, ttl); err != nil {
		s.log.Printf("[linkpreview] 缓存写入失败（继续返回结果）: %v", err)
	}
}
