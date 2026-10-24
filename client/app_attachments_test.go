package main

import (
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/OpenALC/LiteTalk/client/core/signal"
)

// TestChatMessageAttachmentsBridge REST 消息 → 桥接视图：附件字段完整保留，
// 相对能力 URL 拼成当前基址的绝对地址；软删消息不渲染附件。
func TestChatMessageAttachmentsBridge(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`[` +
			`{"id":"9001","channel_id":"123","user_id":"7","content":"看图",` +
			`"created_at":"2026-10-24T12:00:00Z","sender_nickname":"甲","attachments":[` +
			`{"id":"501","filename":"pic.png","size":2048,"content_type":"image/png",` +
			`"width":640,"height":480,"url":"/api/v1/attachments/501?key=k",` +
			`"thumbnail_url":"/api/v1/attachments/501/thumbnail?key=k"}]},` +
			`{"id":"9002","channel_id":"123","user_id":"7","content":"",` +
			`"created_at":"2026-10-24T12:01:00Z","sender_nickname":"甲",` +
			`"deleted_at":"2026-10-24T12:02:00Z","attachments":[` +
			`{"id":"502","filename":"old.png","size":1,"content_type":"image/png",` +
			`"width":1,"height":1,"url":"/api/v1/attachments/502?key=k2"}]}]`))
	}))
	defer srv.Close()

	app := &App{api: newAppAPI(srv.URL)}
	out, err := app.ListMessages("123", 10)
	if err != nil {
		t.Fatalf("ListMessages: %v", err)
	}
	if len(out) != 2 {
		t.Fatalf("消息条数 = %d，期望 2", len(out))
	}
	atts := out[0].Attachments
	if len(atts) != 1 {
		t.Fatalf("附件数量 = %d，期望 1", len(atts))
	}
	got := atts[0]
	if got.ID != "501" || got.Filename != "pic.png" || got.Size != 2048 ||
		got.ContentType != "image/png" || got.Width != 640 || got.Height != 480 {
		t.Fatalf("附件字段错误: %+v", got)
	}
	if got.URL != srv.URL+"/api/v1/attachments/501?key=k" {
		t.Fatalf("url 未拼成绝对地址: %q", got.URL)
	}
	if got.ThumbnailURL != srv.URL+"/api/v1/attachments/501/thumbnail?key=k" {
		t.Fatalf("thumbnail_url 未拼成绝对地址: %q", got.ThumbnailURL)
	}
	if len(out[1].Attachments) != 0 {
		t.Fatalf("软删消息不应带附件: %+v", out[1].Attachments)
	}
}

// TestAttachmentViewsFromSignalResolveURLs WS chat.message 的附件走同一套
// 相对 → 绝对地址转换。
func TestAttachmentViewsFromSignalResolveURLs(t *testing.T) {
	app := &App{api: newAppAPI("https://example.com/")}
	out := app.attachmentViewsFromSignal([]signal.AttachmentInfo{{
		ID: "501", Filename: "a.txt", Size: 12, ContentType: "text/plain",
		URL: "/api/v1/attachments/501?key=k",
	}, {
		ID: "502", Filename: "pic.png", Size: 34, ContentType: "image/png",
		Width: 10, Height: 20,
		URL:          "https://cdn.example.com/pic.png",
		ThumbnailURL: "/api/v1/attachments/502/thumbnail?key=k",
	}})
	if len(out) != 2 {
		t.Fatalf("附件数量 = %d，期望 2", len(out))
	}
	if out[0].URL != "https://example.com/api/v1/attachments/501?key=k" {
		t.Fatalf("相对 url 未拼基址: %q", out[0].URL)
	}
	if out[1].URL != "https://cdn.example.com/pic.png" {
		t.Fatalf("绝对 url 被改写: %q", out[1].URL)
	}
	if out[1].ThumbnailURL != "https://example.com/api/v1/attachments/502/thumbnail?key=k" {
		t.Fatalf("缩略图 url 未拼基址: %q", out[1].ThumbnailURL)
	}
	if out[1].Width != 10 || out[1].Height != 20 || out[1].Size != 34 {
		t.Fatalf("附件尺寸字段错误: %+v", out[1])
	}
}

// TestOpenAttachmentSchemeValidation 只允许 http/https：脚本、本地文件与
// 相对/空地址一律拒绝，不落到系统浏览器。
func TestOpenAttachmentSchemeValidation(t *testing.T) {
	app := &App{}
	for _, raw := range []string{
		"javascript:alert(1)", "file:///C:/Windows/System32/calc.exe",
		"ftp://example.com/a.txt", "data:text/html,hi", "",
		"/api/v1/attachments/1?key=k", "//example.com/a.png",
	} {
		if err := app.OpenAttachment(raw); err == nil {
			t.Fatalf("OpenAttachment(%q) 应报错", raw)
		}
	}
	// 合法地址在测试环境（无 Wails 应用）报桌面环境未就绪，而不是 scheme 错误。
	err := app.OpenAttachment("https://example.com/a.png")
	if err == nil || !strings.Contains(err.Error(), "桌面环境") {
		t.Fatalf("合法地址应报桌面环境未就绪，实际 %v", err)
	}
}

// TestUploadAttachmentPaths 文件选择后的上传流程：读取文件 → multipart 上传 →
// 相对 url 转绝对。空选择返回空切片；超 10 个/不存在/目录在读取前报错。
func TestUploadAttachmentPaths(t *testing.T) {
	var gotFilename, gotContent string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseMultipartForm(1 << 20); err != nil {
			t.Errorf("multipart 解析失败: %v", err)
			http.Error(w, "bad", http.StatusBadRequest)
			return
		}
		f, hdr, err := r.FormFile("file")
		if err != nil {
			t.Errorf("缺少 file 字段: %v", err)
			http.Error(w, "missing", http.StatusBadRequest)
			return
		}
		defer f.Close()
		gotFilename = hdr.Filename
		data, _ := io.ReadAll(f)
		gotContent = string(data)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"id":"501","filename":"` + hdr.Filename + `","size":5,` +
			`"content_type":"text/plain","width":0,"height":0,` +
			`"url":"/api/v1/attachments/501?key=k"}`))
	}))
	defer srv.Close()

	path := filepath.Join(t.TempDir(), "note.txt")
	if err := os.WriteFile(path, []byte("hello"), 0o644); err != nil {
		t.Fatal(err)
	}
	app := &App{api: newAppAPI(srv.URL)}
	out, err := app.uploadAttachmentPaths([]string{path})
	if err != nil {
		t.Fatalf("uploadAttachmentPaths: %v", err)
	}
	if gotFilename != "note.txt" || gotContent != "hello" {
		t.Fatalf("上传文件 = %q %q", gotFilename, gotContent)
	}
	if len(out) != 1 || out[0].ID != 501 || out[0].URL != srv.URL+"/api/v1/attachments/501?key=k" {
		t.Fatalf("上传结果错误: %+v", out)
	}

	// 取消选择：空列表不是错误。
	out, err = app.uploadAttachmentPaths(nil)
	if err != nil || len(out) != 0 {
		t.Fatalf("空选择应返回空切片: %v %v", out, err)
	}
	// 超过 10 个在读取前拒绝。
	many := make([]string, maxAttachmentsPerMessage+1)
	for i := range many {
		many[i] = "missing"
	}
	if _, err := app.uploadAttachmentPaths(many); err == nil ||
		!strings.Contains(err.Error(), "最多") {
		t.Fatalf("超 10 个应报错，实际 %v", err)
	}
	// 不存在的文件与目录在读取前报错。
	if _, err := app.uploadAttachmentPaths([]string{filepath.Join(t.TempDir(), "nope.txt")}); err == nil {
		t.Fatal("不存在的文件应报错")
	}
	if _, err := app.uploadAttachmentPaths([]string{t.TempDir()}); err == nil {
		t.Fatal("目录应报错")
	}
}

// TestPickAndUploadAttachmentsUnavailable 未接入桌面环境时给出明确错误，
// 不是空指针崩溃。
func TestPickAndUploadAttachmentsUnavailable(t *testing.T) {
	if _, err := (&App{}).PickAndUploadAttachments(); err == nil {
		t.Fatal("无 wails 引用时应报错")
	}
}
