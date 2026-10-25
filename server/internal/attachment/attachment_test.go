package attachment

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"image"
	"image/color"
	"image/draw"
	"image/gif"
	"image/jpeg"
	"image/png"
	"strings"
	"testing"
	"unicode/utf8"
)

func solidImage(w, h int) *image.RGBA {
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	draw.Draw(img, img.Bounds(),
		&image.Uniform{C: color.RGBA{R: 0x4F, G: 0xA4, B: 0xF3, A: 0xFF}}, image.Point{}, draw.Src)
	return img
}

func encodePNG(t *testing.T, w, h int) []byte {
	t.Helper()
	var buf bytes.Buffer
	if err := png.Encode(&buf, solidImage(w, h)); err != nil {
		t.Fatalf("png.Encode: %v", err)
	}
	return buf.Bytes()
}

func encodeGIF(t *testing.T, w, h int) []byte {
	t.Helper()
	var buf bytes.Buffer
	if err := gif.Encode(&buf, solidImage(w, h), nil); err != nil {
		t.Fatalf("gif.Encode: %v", err)
	}
	return buf.Bytes()
}

// thumbBounds 解码 JPEG 缩略图并返回尺寸。
func thumbBounds(t *testing.T, thumb []byte) image.Rectangle {
	t.Helper()
	if !bytes.HasPrefix(thumb, []byte{0xFF, 0xD8, 0xFF}) {
		t.Fatalf("缩略图应是 JPEG（magic=% x）", thumb[:min(3, len(thumb))])
	}
	img, err := jpeg.Decode(bytes.NewReader(thumb))
	if err != nil {
		t.Fatalf("jpeg.Decode(缩略图): %v", err)
	}
	return img.Bounds()
}

func TestProcessImageThumbnail(t *testing.T) {
	data := encodePNG(t, 640, 480)
	p, err := Process("截图 1.PNG", data, Config{})
	if err != nil {
		t.Fatalf("Process: %v", err)
	}
	if p.ContentType != "image/png" {
		t.Fatalf("ContentType = %q，期望 image/png（按内容嗅探，不信扩展名）", p.ContentType)
	}
	if p.Width != 640 || p.Height != 480 {
		t.Fatalf("原图尺寸 = %dx%d，期望 640x480", p.Width, p.Height)
	}
	if p.Size != int64(len(data)) || !bytes.Equal(p.Data, data) {
		t.Fatalf("Data/Size 未原样保留: size=%d len=%d", p.Size, len(data))
	}
	if p.Filename != "截图 1.PNG" {
		t.Fatalf("Filename = %q", p.Filename)
	}
	sum := sha256.Sum256(data)
	if p.SHA256 != hex.EncodeToString(sum[:]) {
		t.Fatalf("SHA256 = %q，期望 %x", p.SHA256, sum)
	}
	if len(p.AccessKey) != 64 {
		t.Fatalf("AccessKey 长度 = %d，期望 64", len(p.AccessKey))
	}
	if p.StorageKey != "" || p.ThumbKey != "" {
		t.Fatalf("key 应由调用方按 ID 生成，Process 不应填: %q %q", p.StorageKey, p.ThumbKey)
	}

	b := thumbBounds(t, p.Thumb)
	if b.Dx() != ThumbMaxEdge || b.Dy() != 240 {
		t.Fatalf("缩略图尺寸 = %dx%d，期望 320x240", b.Dx(), b.Dy())
	}
}

func TestProcessSmallImageKeepsSize(t *testing.T) {
	p, err := Process("small.png", encodePNG(t, 200, 100), Config{})
	if err != nil {
		t.Fatalf("Process: %v", err)
	}
	b := thumbBounds(t, p.Thumb)
	if b.Dx() != 200 || b.Dy() != 100 {
		t.Fatalf("小图缩略图不应放大: %dx%d", b.Dx(), b.Dy())
	}
}

func TestProcessTallImage(t *testing.T) {
	p, err := Process("tall.png", encodePNG(t, 100, 1000), Config{})
	if err != nil {
		t.Fatalf("Process: %v", err)
	}
	b := thumbBounds(t, p.Thumb)
	if b.Dx() != 32 || b.Dy() != ThumbMaxEdge {
		t.Fatalf("竖图缩略图 = %dx%d，期望 32x320", b.Dx(), b.Dy())
	}
}

func TestProcessGIFFirstFrame(t *testing.T) {
	p, err := Process("anim.gif", encodeGIF(t, 400, 300), Config{})
	if err != nil {
		t.Fatalf("Process: %v", err)
	}
	if p.ContentType != "image/gif" || p.Width != 400 || p.Height != 300 {
		t.Fatalf("GIF 结果 = %q %dx%d", p.ContentType, p.Width, p.Height)
	}
	b := thumbBounds(t, p.Thumb)
	if b.Dx() != ThumbMaxEdge || b.Dy() != 240 {
		t.Fatalf("GIF 缩略图 = %dx%d，期望 320x240", b.Dx(), b.Dy())
	}
}

func TestProcessNonImage(t *testing.T) {
	cases := map[string][]byte{
		"application/pdf": []byte("%PDF-1.7\n%\xE2\xE3\xCF\xD3\n1 0 obj\n<<>>\nendobj\n"),
		"application/zip": {'P', 'K', 3, 4, 20, 0, 0, 0},
		"text/plain":      []byte("轻音 LiteTalk 日志\nline 2\n"),
	}
	for wantType, data := range cases {
		p, err := Process("doc", data, Config{})
		if err != nil {
			t.Fatalf("%s: Process: %v", wantType, err)
		}
		if p.ContentType != wantType {
			t.Fatalf("ContentType = %q，期望 %q", p.ContentType, wantType)
		}
		if p.Thumb != nil || p.Width != 0 || p.Height != 0 {
			t.Fatalf("%s：非图片不应有缩略图/宽高: %dx%d %d bytes", wantType, p.Width, p.Height, len(p.Thumb))
		}
	}
	// 正文中间提到 <svg 的普通文本不应按 SVG 拒绝（只检查开头）。
	if p, err := Process("notes.txt", []byte("日志里提到 <svg> 标签\n"), Config{}); err != nil || p.ContentType != "text/plain" {
		t.Fatalf("普通文本提到 SVG 应接受: %+v %v", p, err)
	}
}

func TestProcessRejects(t *testing.T) {
	pngData := encodePNG(t, 20, 10)
	corrupt := append([]byte{0x89, 0x50, 0x4E, 0x47, 0x0D, 0x0A, 0x1A, 0x0A}, []byte("broken")...)

	cases := []struct {
		name   string
		data   []byte
		cfg    Config
		target error
	}{
		{"空文件", nil, Config{}, ErrBadType},
		{"超大小", pngData, Config{MaxBytes: int64(len(pngData) - 1)}, ErrTooLarge},
		{"白名单外 HTML", []byte("<!DOCTYPE html><html><body>hi</body></html>"), Config{}, ErrBadType},
		{"SVG 裸根元素", []byte(`<svg xmlns="http://www.w3.org/2000/svg"><script>alert(1)</script></svg>`), Config{}, ErrBadType},
		{"XML 声明的 SVG", []byte("<?xml version=\"1.0\"?>\n<svg xmlns=\"http://www.w3.org/2000/svg\"/>"), Config{}, ErrBadType},
		{"损坏图片", corrupt, Config{}, ErrBadImage},
		{"图片超出单边限制", pngData, Config{MaxDimension: 15}, ErrBadImage},
		{"默认单边限制", encodePNG(t, DefaultMaxDimension+1, 1), Config{}, ErrBadImage},
	}
	for _, tc := range cases {
		if _, err := Process("x", tc.data, tc.cfg); !errors.Is(err, tc.target) {
			t.Fatalf("%s：错误 = %v，期望 %v", tc.name, err, tc.target)
		}
	}
}

func TestProcessAllowedTypesOverride(t *testing.T) {
	cfg := Config{AllowedTypes: []string{"Text/HTML; charset=utf-8"}}
	html := []byte("<!DOCTYPE html><html></html>")
	if p, err := Process("a.html", html, cfg); err != nil || p.ContentType != "text/html" {
		t.Fatalf("覆盖白名单后应接受 HTML: %+v %v", p, err)
	}
	if _, err := Process("a.png", encodePNG(t, 2, 2), cfg); !errors.Is(err, ErrBadType) {
		t.Fatalf("覆盖白名单后 PNG 应被拒绝，实际 %v", err)
	}
}

func TestCleanFilename(t *testing.T) {
	long := strings.Repeat("测", 200) // 600 字节，必须按 rune 截断
	cases := map[string]string{
		`C:\Users\me\截图 1.png`:          "截图 1.png",
		"../../etc/passwd":              "passwd",
		"a/b/c.txt":                     "c.txt",
		`a\b`:                           "b",
		"":                              "file",
		".":                             "file",
		"..":                            "file",
		"  name.txt  ":                  "name.txt",
		"a" + string(byte(1)) + "b.txt": "ab.txt",
	}
	for in, want := range cases {
		if got := cleanFilename(in); got != want {
			t.Fatalf("cleanFilename(%q) = %q，期望 %q", in, got, want)
		}
	}
	got := cleanFilename(long)
	if len(got) > maxFilenameBytes {
		t.Fatalf("长文件名 = %d 字节，超过 %d", len(got), maxFilenameBytes)
	}
	if !utf8.ValidString(got) {
		t.Fatalf("截断后不是合法 UTF-8: %q", got)
	}
	if !strings.HasPrefix(long, got) {
		t.Fatalf("截断应保留前缀: %q", got)
	}
}

func TestNewAccessKey(t *testing.T) {
	a, err := NewAccessKey()
	if err != nil {
		t.Fatalf("NewAccessKey: %v", err)
	}
	b, err := NewAccessKey()
	if err != nil {
		t.Fatalf("NewAccessKey: %v", err)
	}
	if len(a) != 64 {
		t.Fatalf("AccessKey 长度 = %d，期望 64", len(a))
	}
	if _, err := hex.DecodeString(a); err != nil {
		t.Fatalf("AccessKey 不是 hex: %v", err)
	}
	if a == b {
		t.Fatal("两次生成的 AccessKey 不应相同")
	}
}

func TestKeys(t *testing.T) {
	storage, thumb := Keys(123456789)
	if storage != "123456789.bin" || thumb != "123456789.thumb.jpg" {
		t.Fatalf("Keys = %q, %q", storage, thumb)
	}
	for _, key := range []string{storage, thumb} {
		if len(key) > 128 {
			t.Fatalf("key 超过 128 字节: %q", key)
		}
		for i := 0; i < len(key); i++ {
			c := key[i]
			ok := c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' ||
				c == '.' || c == '_' || c == '-'
			if !ok {
				t.Fatalf("key %q 含非法字符 %q", key, c)
			}
		}
	}
}
