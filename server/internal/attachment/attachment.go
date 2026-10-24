// Package attachment 附件校验与图片缩略图生成（#41 B1）。
// 只处理字节内容，不碰 blob 与数据库：调用方拿 Process 的产物分配 ID、生成 key，
// 再写 blob 与 Store。MIME 一律以内容嗅探为准，不信任客户端 filename/content-type。
package attachment

import (
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"image"
	_ "image/gif" // 注册 GIF 解码（首帧）
	"image/jpeg"
	_ "image/png" // 注册 PNG 解码
	"math"
	"net/http"
	"path"
	"strconv"
	"strings"
	"unicode/utf8"

	"golang.org/x/image/draw"
	_ "golang.org/x/image/webp" // 注册 WebP 解码（首帧）
)

// 默认限制与缩略图参数。
const (
	// DefaultMaxBytes 单附件默认上限（10 MiB）。
	DefaultMaxBytes int64 = 10 << 20
	// DefaultMaxDimension 图片单边默认上限（像素），防止解码巨图耗尽内存。
	DefaultMaxDimension = 8192
	// ThumbMaxEdge 缩略图最长边（像素）。
	ThumbMaxEdge = 320
	// thumbJPEGQuality 统一缩略图编码质量。
	thumbJPEGQuality = 82
	// maxFilenameBytes 与 attachments.filename VARCHAR(255) 对齐。
	maxFilenameBytes = 255
)

// DefaultAllowedTypes 默认白名单：常见图片与办公文档，不含可执行的 SVG/HTML。
var DefaultAllowedTypes = []string{
	"image/jpeg",
	"image/png",
	"image/gif",
	"image/webp",
	"application/pdf",
	"text/plain",
	"application/zip",
}

// 哨兵错误：B2 的 API 层据此映射 too_large / bad_type / bad_image。
var (
	// ErrTooLarge 文件超过 MaxBytes。
	ErrTooLarge = errors.New("attachment: too large")
	// ErrBadType 空文件或内容类型不在白名单。
	ErrBadType = errors.New("attachment: bad type")
	// ErrBadImage 图片解码失败、尺寸非法或超过 MaxDimension。
	ErrBadImage = errors.New("attachment: bad image")
)

// Config 校验参数；零值字段回落默认值（MaxBytes<=0 / MaxDimension<=0 / 白名单为空）。
type Config struct {
	MaxBytes     int64
	MaxDimension int
	AllowedTypes []string
}

// Processed 校验通过后的附件产物。
// Data 原样指向传入切片（不做拷贝）；Thumb 为 JPEG 缩略图，非图片为 nil。
// StorageKey/ThumbKey 需要附件 ID，由调用方在分配 ID 后经 Keys 填入。
type Processed struct {
	Data        []byte
	Thumb       []byte
	Filename    string
	ContentType string
	Size        int64
	Width       int
	Height      int
	SHA256      string
	StorageKey  string
	ThumbKey    string
	AccessKey   string
}

// Process 校验内容并生成缩略图。
// 顺序：大小 → 内容嗅探 + 白名单 → 图片尺寸上限/解码。
// 非图片（PDF/ZIP/文本）不生成缩略图，宽高为 0。
func Process(filename string, data []byte, cfg Config) (*Processed, error) {
	cfg = withDefaults(cfg)
	name := cleanFilename(filename)
	if len(data) == 0 {
		return nil, fmt.Errorf("%w: 文件内容为空", ErrBadType)
	}
	if int64(len(data)) > cfg.MaxBytes {
		return nil, fmt.Errorf("%w: 文件超过 %d 字节上限", ErrTooLarge, cfg.MaxBytes)
	}
	contentType := normalizeContentType(http.DetectContentType(data))
	if !allowedType(contentType, cfg.AllowedTypes) {
		return nil, fmt.Errorf("%w: 不支持的文件类型 %s", ErrBadType, contentType)
	}

	accessKey, err := NewAccessKey()
	if err != nil {
		return nil, err
	}
	sum := sha256.Sum256(data)
	out := &Processed{
		Data:        data,
		Filename:    name,
		ContentType: contentType,
		Size:        int64(len(data)),
		SHA256:      hex.EncodeToString(sum[:]),
		AccessKey:   accessKey,
	}
	if isImageType(contentType) {
		thumb, width, height, err := processImage(data, cfg.MaxDimension)
		if err != nil {
			return nil, err
		}
		out.Thumb, out.Width, out.Height = thumb, width, height
	}
	return out, nil
}

// NewAccessKey 生成 32 字节随机数的 hex，作为附件下载能力凭证。
func NewAccessKey() (string, error) {
	var raw [32]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", fmt.Errorf("attachment: 生成下载凭证: %w", err)
	}
	return hex.EncodeToString(raw[:]), nil
}

// Keys 由附件 ID 生成 blob 键。只含 [0-9a-z._-]，满足 blob 的 key 字符集；
// storage 与 thumb 不同后缀，不会互相覆盖。非图片也会生成 thumb 键，但不会写对象。
func Keys(id int64) (storageKey, thumbKey string) {
	base := strconv.FormatInt(id, 10)
	return base + ".bin", base + ".thumb.jpg"
}

func withDefaults(cfg Config) Config {
	if cfg.MaxBytes <= 0 {
		cfg.MaxBytes = DefaultMaxBytes
	}
	if cfg.MaxDimension <= 0 {
		cfg.MaxDimension = DefaultMaxDimension
	}
	if len(cfg.AllowedTypes) == 0 {
		cfg.AllowedTypes = append([]string(nil), DefaultAllowedTypes...)
	}
	return cfg
}

// normalizeContentType 去掉 "; charset=..." 等参数并转小写，保证白名单比对稳定。
func normalizeContentType(ct string) string {
	if i := strings.IndexByte(ct, ';'); i >= 0 {
		ct = ct[:i]
	}
	return strings.ToLower(strings.TrimSpace(ct))
}

func allowedType(contentType string, allowed []string) bool {
	for _, want := range allowed {
		if normalizeContentType(want) == contentType {
			return true
		}
	}
	return false
}

func isImageType(contentType string) bool {
	switch contentType {
	case "image/jpeg", "image/png", "image/gif", "image/webp":
		return true
	}
	return false
}

// processImage 先用 DecodeConfig 拿尺寸（避免为超大图分配完整像素），再解码整图生成缩略图。
// 返回原图宽高，缩略图尺寸不对外暴露。
func processImage(data []byte, maxDimension int) ([]byte, int, int, error) {
	ic, _, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil {
		return nil, 0, 0, fmt.Errorf("%w: 图片头解析失败: %v", ErrBadImage, err)
	}
	if ic.Width <= 0 || ic.Height <= 0 {
		return nil, 0, 0, fmt.Errorf("%w: 图片尺寸非法 %dx%d", ErrBadImage, ic.Width, ic.Height)
	}
	if ic.Width > maxDimension || ic.Height > maxDimension {
		return nil, 0, 0, fmt.Errorf("%w: 图片尺寸 %dx%d 超过单边上限 %d", ErrBadImage, ic.Width, ic.Height, maxDimension)
	}
	src, _, err := image.Decode(bytes.NewReader(data))
	if err != nil {
		return nil, 0, 0, fmt.Errorf("%w: 图片解码失败: %v", ErrBadImage, err)
	}
	thumb, err := encodeThumb(src)
	if err != nil {
		return nil, 0, 0, err
	}
	return thumb, ic.Width, ic.Height, nil
}

// encodeThumb 等比缩放到最长边 320（原本不超则保持原尺寸），统一 JPEG quality 82。
// JPEG 无 alpha 通道：先铺白底再合成，避免透明 PNG/GIF 的缩略图出现黑块。
func encodeThumb(src image.Image) ([]byte, error) {
	b := src.Bounds()
	w, h := b.Dx(), b.Dy()
	nw, nh := w, h
	if w > ThumbMaxEdge || h > ThumbMaxEdge {
		if w >= h {
			nw = ThumbMaxEdge
			nh = int(math.Round(float64(h) * ThumbMaxEdge / float64(w)))
		} else {
			nh = ThumbMaxEdge
			nw = int(math.Round(float64(w) * ThumbMaxEdge / float64(h)))
		}
	}
	if nw < 1 {
		nw = 1
	}
	if nh < 1 {
		nh = 1
	}
	dst := image.NewRGBA(image.Rect(0, 0, nw, nh))
	draw.Draw(dst, dst.Bounds(), image.White, image.Point{}, draw.Src)
	draw.CatmullRom.Scale(dst, dst.Bounds(), src, b, draw.Over, nil)
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, dst, &jpeg.Options{Quality: thumbJPEGQuality}); err != nil {
		return nil, fmt.Errorf("%w: 缩略图编码失败: %v", ErrBadImage, err)
	}
	return buf.Bytes(), nil
}

// cleanFilename 只保留「展示用」文件名：两种平台的分隔符都按路径拆掉，再去控制字符、
// 收尾空白；空串/点目录回落到 "file"，最后按 rune 截断到 255 字节（不切坏 UTF-8）。
// 该名字只用于展示与下载时的文件名，绝不用于拼磁盘路径。
func cleanFilename(name string) string {
	name = strings.ReplaceAll(name, `\`, "/")
	name = path.Base(name)
	var b strings.Builder
	for _, r := range name {
		if r < 0x20 || r == 0x7f || r == '/' || r == '\\' {
			continue
		}
		b.WriteRune(r)
	}
	cleaned := strings.TrimSpace(b.String())
	if cleaned == "" || cleaned == "." || cleaned == ".." {
		cleaned = "file"
	}
	return truncateBytes(cleaned, maxFilenameBytes)
}

// truncateBytes 按 rune 边界把字符串截到不超过 max 字节。
func truncateBytes(s string, max int) string {
	if len(s) <= max {
		return s
	}
	var b strings.Builder
	for _, r := range s {
		n := utf8.RuneLen(r)
		if b.Len()+n > max {
			break
		}
		b.WriteRune(r)
	}
	if b.Len() == 0 {
		return "file"
	}
	return b.String()
}
