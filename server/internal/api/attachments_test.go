// 附件上传/下载端点测试（#41 B2）：真实 multipart + 临时目录 local blob。
package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	_ "image/jpeg" // 缩略图 DecodeConfig 需要注册 JPEG

	"github.com/OpenALC/LiteTalk/server/internal/attachment"
	"github.com/OpenALC/LiteTalk/server/internal/auth"
	"github.com/OpenALC/LiteTalk/server/internal/blob"
	"github.com/OpenALC/LiteTalk/server/internal/store"
)

// newAttachmentTestServer 在内存 store 上注入临时目录 local blob 与上传配置。
func newAttachmentTestServer(t *testing.T, maxBytes int64) (*Server, *store.Mem, string) {
	t.Helper()
	mem := store.NewMem()
	dir := t.TempDir()
	b, _, err := blob.New(blob.Config{Driver: "local", Dir: dir}, nil)
	if err != nil {
		t.Fatalf("构造 local blob: %v", err)
	}
	srv := New(Config{
		Store:    mem,
		JWT:      auth.NewJWT("test-secret"),
		Refresh:  auth.NewRefreshStore(mem),
		Codes:    auth.NewEmailCodes(mem),
		Limits:   auth.NewRateLimit(mem),
		AuthMode: AuthModeFull,
		Session:  auth.NewSessionIndex(mem, mem),
		Presence: mem,
		Blob:     b,
		Upload:   attachment.Config{MaxBytes: maxBytes, MaxDimension: 4096},
	})
	return srv, mem, dir
}

// uploadPart 一个 multipart 字段；filename 为空时写普通表单字段。
type uploadPart struct {
	field    string
	filename string
	data     []byte
}

func multipartBody(t *testing.T, parts ...uploadPart) (*bytes.Buffer, string) {
	t.Helper()
	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)
	for _, p := range parts {
		var (
			pw  io.Writer
			err error
		)
		if p.filename != "" {
			pw, err = w.CreateFormFile(p.field, p.filename)
		} else {
			pw, err = w.CreateFormField(p.field)
		}
		if err != nil {
			t.Fatalf("构造 multipart: %v", err)
		}
		if _, err := pw.Write(p.data); err != nil {
			t.Fatalf("写 multipart: %v", err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatalf("关闭 multipart: %v", err)
	}
	return &buf, w.FormDataContentType()
}

func uploadAttachment(t *testing.T, h http.Handler, token string, parts ...uploadPart) *httptest.ResponseRecorder {
	t.Helper()
	body, ct := multipartBody(t, parts...)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/attachments", body)
	req.Header.Set("Content-Type", ct)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// downloadRaw 发不带 Authorization / X-LiteTalk-Client 的 GET（下载端点免鉴权口径）。
func downloadRaw(t *testing.T, h http.Handler, path string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func decodeJSONBody(t *testing.T, rec *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var out map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("解码响应 JSON: %v，body=%s", err, rec.Body.String())
	}
	return out
}

// testPNG 生成纯色 PNG（宽高可控，白底缩略图便于校验）。
func testPNG(t *testing.T, width, height int) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, width, height))
	for y := 0; y < height; y++ {
		for x := 0; x < width; x++ {
			img.Set(x, y, color.RGBA{R: uint8(x % 256), G: uint8(y % 256), B: 0x80, A: 0xff})
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatalf("编码 PNG: %v", err)
	}
	return buf.Bytes()
}

// flipKey 改掉能力 key 的首字符（长度不变），用于错误凭证用例。
func flipKey(t *testing.T, url string) string {
	t.Helper()
	parts := strings.SplitN(url, "key=", 2)
	if len(parts) != 2 || len(parts[1]) == 0 {
		t.Fatalf("url 不含 key: %q", url)
	}
	key := []byte(parts[1])
	if key[0] == '0' {
		key[0] = '1'
	} else {
		key[0] = '0'
	}
	return parts[0] + "key=" + string(key)
}

// TestAttachmentUploadDownload 上传 PNG → 下载原图与缩略图 → 错误 key/ID 404。
func TestAttachmentUploadDownload(t *testing.T) {
	srv, mem, _ := newAttachmentTestServer(t, 1<<20)
	h := srv.Handler()
	access, _ := registerUser(t, h, mem, "att@ex.com", "uploader", "password123")

	raw := testPNG(t, 400, 300)
	rec := uploadAttachment(t, h, access, uploadPart{"file", "图片.png", raw})
	if rec.Code != http.StatusCreated {
		t.Fatalf("上传期望 201 实际 %d body=%s", rec.Code, rec.Body.String())
	}
	got := decodeJSONBody(t, rec)
	if got["filename"] != "图片.png" || got["content_type"] != "image/png" {
		t.Fatalf("元数据不符: %v", got)
	}
	if int64(got["size"].(float64)) != int64(len(raw)) {
		t.Fatalf("size=%v 期望 %d", got["size"], len(raw))
	}
	if int(got["width"].(float64)) != 400 || int(got["height"].(float64)) != 300 {
		t.Fatalf("宽高不符: %v x %v", got["width"], got["height"])
	}
	if _, leaked := got["access_key"]; leaked {
		t.Fatal("响应不得单独暴露 access_key")
	}
	url, _ := got["url"].(string)
	thumbURL, _ := got["thumbnail_url"].(string)
	if !strings.HasPrefix(url, "/api/v1/attachments/") || !strings.Contains(url, "?key=") {
		t.Fatalf("url 形状不符: %q", url)
	}
	if !strings.Contains(thumbURL, "/thumbnail?key=") {
		t.Fatalf("thumbnail_url 形状不符: %q", thumbURL)
	}

	// 原图：不要求 Bearer、不要求 X-LiteTalk-Client。
	dl := downloadRaw(t, h, url)
	if dl.Code != http.StatusOK {
		t.Fatalf("下载期望 200 实际 %d body=%s", dl.Code, dl.Body.String())
	}
	if ct := dl.Header().Get("Content-Type"); ct != "image/png" {
		t.Fatalf("Content-Type=%q", ct)
	}
	if cl := dl.Header().Get("Content-Length"); cl != strconv.Itoa(len(raw)) {
		t.Fatalf("Content-Length=%q 期望 %d", cl, len(raw))
	}
	if cd := dl.Header().Get("Content-Disposition"); !strings.HasPrefix(cd, "inline;") {
		t.Fatalf("图片应 inline，实际 %q", cd)
	} else if !strings.Contains(cd, "filename*=utf-8''") {
		t.Fatalf("中文文件名应按 RFC 2231 转义，实际 %q", cd)
	}
	if cc := dl.Header().Get("Cache-Control"); cc != downloadCacheControl {
		t.Fatalf("Cache-Control=%q", cc)
	}
	if dl.Header().Get("X-Content-Type-Options") != "nosniff" {
		t.Fatal("缺少 X-Content-Type-Options: nosniff")
	}
	if !bytes.Equal(dl.Body.Bytes(), raw) {
		t.Fatal("下载内容与上传不一致")
	}

	// 缩略图：image/jpeg，最长边 320（400x300 → 320x240）。
	thumb := downloadRaw(t, h, thumbURL)
	if thumb.Code != http.StatusOK {
		t.Fatalf("缩略图期望 200 实际 %d body=%s", thumb.Code, thumb.Body.String())
	}
	if ct := thumb.Header().Get("Content-Type"); ct != "image/jpeg" {
		t.Fatalf("缩略图 Content-Type=%q", ct)
	}
	cfg, _, err := image.DecodeConfig(bytes.NewReader(thumb.Body.Bytes()))
	if err != nil {
		t.Fatalf("解码缩略图: %v", err)
	}
	if cfg.Width != 320 || cfg.Height != 240 {
		t.Fatalf("缩略图尺寸 %dx%d，期望 320x240", cfg.Width, cfg.Height)
	}

	// 错误 key（长度不变、内容不同）→ 404。
	if bad := downloadRaw(t, h, flipKey(t, url)); bad.Code != http.StatusNotFound {
		t.Fatalf("错误 key 期望 404 实际 %d", bad.Code)
	}
	// 错误 ID（key 正确）→ 404。
	id := asID(got["id"])
	key := url[strings.Index(url, "key=")+len("key="):]
	if bad := downloadRaw(t, h, "/api/v1/attachments/"+strconv.FormatInt(id+1, 10)+"?key="+key); bad.Code != http.StatusNotFound {
		t.Fatalf("错误 ID 期望 404 实际 %d", bad.Code)
	}
	// key 缺失 → 生成层 400（必填 query）。
	if bad := downloadRaw(t, h, "/api/v1/attachments/"+strconv.FormatInt(id, 10)); bad.Code != http.StatusBadRequest {
		t.Fatalf("缺 key 期望 400 实际 %d", bad.Code)
	}
}

// TestAttachmentUploadValidation 各类非法上传：字段、类型、损坏图片、超限。
func TestAttachmentUploadValidation(t *testing.T) {
	srv, mem, _ := newAttachmentTestServer(t, 1024)
	h := srv.Handler()
	access, _ := registerUser(t, h, mem, "bad@ex.com", "baduploader", "password123")

	brokenPNG := append([]byte{0x89, 'P', 'N', 'G', '\r', '\n', 0x1a, '\n'}, []byte("not a real png")...)
	cases := []struct {
		name  string
		parts []uploadPart
		want  int
		code  string
	}{
		{"缺 file 字段", []uploadPart{{field: "other", data: []byte("x")}}, http.StatusBadRequest, "bad_request"},
		{"空表单", nil, http.StatusBadRequest, "bad_request"},
		{"两个 file", []uploadPart{
			{field: "file", filename: "a.txt", data: []byte("a")},
			{field: "file", filename: "b.txt", data: []byte("b")},
		}, http.StatusBadRequest, "bad_request"},
		{"HTML 拒绝", []uploadPart{{field: "file", filename: "x.html", data: []byte("<html><body>hi</body></html>")}},
			http.StatusBadRequest, "bad_type"},
		{"SVG 拒绝", []uploadPart{{field: "file", filename: "x.svg",
			data: []byte(`<svg xmlns="http://www.w3.org/2000/svg"><script>alert(1)</script></svg>`)}},
			http.StatusBadRequest, "bad_type"},
		{"损坏 PNG", []uploadPart{{field: "file", filename: "x.png", data: brokenPNG}},
			http.StatusBadRequest, "bad_image"},
		{"超过上限", []uploadPart{{field: "file", filename: "big.txt", data: bytes.Repeat([]byte("a"), 2048)}},
			http.StatusRequestEntityTooLarge, "too_large"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := uploadAttachment(t, h, access, tc.parts...)
			if rec.Code != tc.want {
				t.Fatalf("期望 %d 实际 %d body=%s", tc.want, rec.Code, rec.Body.String())
			}
			if body := decodeJSONBody(t, rec); body["code"] != tc.code {
				t.Fatalf("错误码=%v 期望 %q", body["code"], tc.code)
			}
		})
	}

	// 未登录 → 401（上传端点要求 Bearer）。
	rec := uploadAttachment(t, h, "", uploadPart{"file", "a.txt", []byte("hello")})
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("未登录上传期望 401 实际 %d", rec.Code)
	}
}

// TestAttachmentNonImageDownload 非图片：无缩略图、按附件下载（不留 inline）。
func TestAttachmentNonImageDownload(t *testing.T) {
	srv, mem, _ := newAttachmentTestServer(t, 1<<20)
	h := srv.Handler()
	access, _ := registerUser(t, h, mem, "txt@ex.com", "textuploader", "password123")

	data := []byte("hello 轻音\n")
	rec := uploadAttachment(t, h, access, uploadPart{"file", "说明.txt", data})
	if rec.Code != http.StatusCreated {
		t.Fatalf("上传期望 201 实际 %d body=%s", rec.Code, rec.Body.String())
	}
	got := decodeJSONBody(t, rec)
	if _, ok := got["thumbnail_url"]; ok {
		t.Fatalf("非图片不应有 thumbnail_url: %v", got)
	}
	if int(got["width"].(float64)) != 0 || int(got["height"].(float64)) != 0 {
		t.Fatalf("非图片宽高应为 0: %v x %v", got["width"], got["height"])
	}

	url := got["url"].(string)
	dl := downloadRaw(t, h, url)
	if dl.Code != http.StatusOK {
		t.Fatalf("下载期望 200 实际 %d", dl.Code)
	}
	if ct := dl.Header().Get("Content-Type"); ct != "text/plain" {
		t.Fatalf("Content-Type=%q", ct)
	}
	if cd := dl.Header().Get("Content-Disposition"); !strings.HasPrefix(cd, "attachment;") {
		t.Fatalf("非图片应 attachment，实际 %q", cd)
	}
	if !bytes.Equal(dl.Body.Bytes(), data) {
		t.Fatal("下载内容不一致")
	}

	// 缩略图端点：无缩略图 → 404。
	id := asID(got["id"])
	key := url[strings.Index(url, "key=")+len("key="):]
	thumb := downloadRaw(t, h, "/api/v1/attachments/"+strconv.FormatInt(id, 10)+"/thumbnail?key="+key)
	if thumb.Code != http.StatusNotFound {
		t.Fatalf("非图片缩略图期望 404 实际 %d", thumb.Code)
	}
}

// TestAttachmentDownloadSoftDeletedMessage 消息软删后附件不可下载（原图与缩略图）。
func TestAttachmentDownloadSoftDeletedMessage(t *testing.T) {
	srv, mem, _ := newAttachmentTestServer(t, 1<<20)
	h := srv.Handler()
	access, _ := registerUser(t, h, mem, "del@ex.com", "deleter", "password123")

	rec := uploadAttachment(t, h, access, uploadPart{"file", "photo.png", testPNG(t, 32, 16)})
	if rec.Code != http.StatusCreated {
		t.Fatalf("上传期望 201 实际 %d body=%s", rec.Code, rec.Body.String())
	}
	got := decodeJSONBody(t, rec)
	url := got["url"].(string)
	thumbURL := got["thumbnail_url"].(string)
	id := asID(got["id"])

	me := getJSON(t, h, "/api/v1/users/me", access, http.StatusOK).(map[string]any)
	uid := asID(me["id"])
	serverResp := postJSON(t, h, "/api/v1/servers", access, map[string]string{
		"name": "附件服", "icon_name": "music", "icon_color": "#4FA4F3",
	}, http.StatusCreated)
	chResp := postJSON(t, h, "/api/v1/servers/"+strconv.FormatInt(asID(serverResp["id"]), 10)+"/channels", access,
		map[string]string{"name": "附件频道", "type": "text"}, http.StatusCreated)
	chID := asID(chResp["id"])

	msg := &store.Message{ChannelID: chID, UserID: uid, Content: "带附件", AttachmentIDs: []int64{id}}
	if err := mem.CreateMessage(context.Background(), msg); err != nil {
		t.Fatalf("CreateMessage 认领附件: %v", err)
	}
	if dl := downloadRaw(t, h, url); dl.Code != http.StatusOK {
		t.Fatalf("软删前应可下载，实际 %d", dl.Code)
	}

	if err := mem.SoftDeleteMessage(context.Background(), msg.ID, time.Now()); err != nil {
		t.Fatalf("SoftDeleteMessage: %v", err)
	}
	for name, target := range map[string]string{"原图": url, "缩略图": thumbURL} {
		dl := downloadRaw(t, h, target)
		if dl.Code != http.StatusNotFound {
			t.Fatalf("软删后%s期望 404 实际 %d", name, dl.Code)
		}
		if body := decodeJSONBody(t, dl); body["code"] != "not_found" {
			t.Fatalf("软删后%s错误码=%v", name, body["code"])
		}
	}
}

// TestAttachmentUploadRateLimit 每用户 30 次/分钟，第 31 次 429。
func TestAttachmentUploadRateLimit(t *testing.T) {
	srv, mem, _ := newAttachmentTestServer(t, 1<<20)
	h := srv.Handler()
	access, _ := registerUser(t, h, mem, "rate@ex.com", "rateuploader", "password123")
	raw := testPNG(t, 4, 4)

	for i := 1; i <= uploadRateLimit; i++ {
		if rec := uploadAttachment(t, h, access, uploadPart{"file", "tiny.png", raw}); rec.Code != http.StatusCreated {
			t.Fatalf("第 %d 次上传期望 201 实际 %d body=%s", i, rec.Code, rec.Body.String())
		}
	}
	rec := uploadAttachment(t, h, access, uploadPart{"file", "tiny.png", raw})
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("第 %d 次期望 429 实际 %d", uploadRateLimit+1, rec.Code)
	}
	if body := decodeJSONBody(t, rec); body["code"] != "rate_limited" {
		t.Fatalf("错误码=%v", body["code"])
	}
}

// failPutBlob 模拟对象存储写入失败。
type failPutBlob struct{}

func (failPutBlob) Put(context.Context, string, []byte, string) error {
	return errors.New("put boom")
}
func (failPutBlob) Get(context.Context, string) ([]byte, error) { return nil, blob.ErrNotFound }
func (failPutBlob) Delete(context.Context, string) error        { return nil }

// createFailStore 统计 CreateAttachment 调用并固定失败，用于验证元数据失败时清理 blob。
type createFailStore struct {
	*store.Mem
	creates int
}

func (s *createFailStore) CreateAttachment(context.Context, *store.Attachment) error {
	s.creates++
	return errors.New("db boom")
}

// TestAttachmentUploadStorageFailure 存储/元数据失败 → 500 storage_error，不留孤儿对象。
func TestAttachmentUploadStorageFailure(t *testing.T) {
	t.Run("blob 写入失败", func(t *testing.T) {
		srv, mem, dir := newAttachmentTestServer(t, 1<<20)
		h := srv.Handler()
		access, _ := registerUser(t, h, mem, "pf@ex.com", "putfail", "password123")
		srv.blob = failPutBlob{}

		rec := uploadAttachment(t, h, access, uploadPart{"file", "x.png", testPNG(t, 8, 8)})
		if rec.Code != http.StatusInternalServerError {
			t.Fatalf("期望 500 实际 %d body=%s", rec.Code, rec.Body.String())
		}
		if body := decodeJSONBody(t, rec); body["code"] != "storage_error" {
			t.Fatalf("错误码=%v", body["code"])
		}
		assertDirEmpty(t, dir)
	})

	t.Run("元数据写入失败回滚 blob", func(t *testing.T) {
		srv, mem, dir := newAttachmentTestServer(t, 1<<20)
		h := srv.Handler()
		access, _ := registerUser(t, h, mem, "df@ex.com", "dbfail", "password123")
		failing := &createFailStore{Mem: mem}
		srv.store = failing

		rec := uploadAttachment(t, h, access, uploadPart{"file", "x.png", testPNG(t, 8, 8)})
		if rec.Code != http.StatusInternalServerError {
			t.Fatalf("期望 500 实际 %d body=%s", rec.Code, rec.Body.String())
		}
		if body := decodeJSONBody(t, rec); body["code"] != "storage_error" {
			t.Fatalf("错误码=%v", body["code"])
		}
		if failing.creates != 1 {
			t.Fatalf("CreateAttachment 调用 %d 次，期望 1", failing.creates)
		}
		assertDirEmpty(t, dir)
	})
}

// assertDirEmpty 目录下不应残留临时文件或对象（local 驱动的 key 不含子目录）。
func assertDirEmpty(t *testing.T, dir string) {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("读上传目录: %v", err)
	}
	if len(entries) != 0 {
		names := make([]string, 0, len(entries))
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Fatalf("上传目录应清空，残留 %v", names)
	}
}

// TestAttachmentDownloadMissRateLimit 失败下载按 IP 限流：60 次 404 后 429。
func TestAttachmentDownloadMissRateLimit(t *testing.T) {
	srv, _, _ := newAttachmentTestServer(t, 1<<20)
	h := srv.Handler()
	key := strings.Repeat("a", 64)
	for i := 0; i < downloadMissRateLimit; i++ {
		path := fmt.Sprintf("/api/v1/attachments/%d?key=%s", 900000+i, key)
		if rec := downloadRaw(t, h, path); rec.Code != http.StatusNotFound {
			t.Fatalf("第 %d 次失败下载期望 404 实际 %d", i+1, rec.Code)
		}
	}
	rec := downloadRaw(t, h, fmt.Sprintf("/api/v1/attachments/999999?key=%s", key))
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("超过失败下载限流期望 429 实际 %d", rec.Code)
	}
	if body := decodeJSONBody(t, rec); body["code"] != "rate_limited" {
		t.Fatalf("错误码=%v", body["code"])
	}
}

// TestAttachmentDownloadBlobMissing 元数据在但对象丢失 → 404（不是 500）。
func TestAttachmentDownloadBlobMissing(t *testing.T) {
	srv, mem, dir := newAttachmentTestServer(t, 1<<20)
	h := srv.Handler()
	access, _ := registerUser(t, h, mem, "miss@ex.com", "missingblob", "password123")
	rec := uploadAttachment(t, h, access, uploadPart{"file", "x.png", testPNG(t, 8, 8)})
	if rec.Code != http.StatusCreated {
		t.Fatalf("上传期望 201 实际 %d", rec.Code)
	}
	got := decodeJSONBody(t, rec)
	id := asID(got["id"])
	// local 驱动：原图对象是 <id>.bin。
	if err := os.Remove(filepath.Join(dir, strconv.FormatInt(id, 10)+".bin")); err != nil {
		t.Fatalf("删除 blob 对象: %v", err)
	}
	if dl := downloadRaw(t, h, got["url"].(string)); dl.Code != http.StatusNotFound {
		t.Fatalf("对象丢失期望 404 实际 %d", dl.Code)
	}
}
