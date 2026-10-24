package api

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestUploadAttachmentMultipart 锁住上传 wire：POST /api/v1/attachments、
// multipart 只带 file 字段（filename 为展示名）、带 Bearer 与版本头；
// 201 响应解析为 AttachmentDTO。
func TestUploadAttachmentMultipart(t *testing.T) {
	var gotMethod, gotPath, gotAuth, gotVersion, gotFilename, gotContent string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod, gotPath = r.Method, r.URL.Path
		gotAuth = r.Header.Get("Authorization")
		gotVersion = r.Header.Get("X-LiteTalk-Client")
		if err := r.ParseMultipartForm(1 << 20); err != nil {
			t.Errorf("multipart 解析失败: %v", err)
			http.Error(w, "bad multipart", http.StatusBadRequest)
			return
		}
		f, hdr, err := r.FormFile("file")
		if err != nil {
			t.Errorf("缺少 file 字段: %v", err)
			http.Error(w, "missing file", http.StatusBadRequest)
			return
		}
		defer f.Close()
		gotFilename = hdr.Filename
		data, _ := io.ReadAll(f)
		gotContent = string(data)
		if len(r.MultipartForm.File) != 1 || len(r.MultipartForm.Value) != 0 {
			t.Errorf("multipart 只应有 file 字段，实际 files=%d values=%d",
				len(r.MultipartForm.File), len(r.MultipartForm.Value))
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"id":"501","filename":"pic.png","size":4,` +
			`"content_type":"image/png","width":10,"height":20,` +
			`"url":"/api/v1/attachments/501?key=abc",` +
			`"thumbnail_url":"/api/v1/attachments/501/thumbnail?key=abc"}`))
	}))
	defer srv.Close()

	c := NewAPIClient(srv.URL)
	c.SetClientVersion("v9.9.9")
	c.SetToken("tok1")
	att, err := c.UploadAttachment("pic.png", []byte("DATA"))
	if err != nil {
		t.Fatalf("UploadAttachment: %v", err)
	}
	if gotMethod != http.MethodPost || gotPath != "/api/v1/attachments" {
		t.Fatalf("请求 = %s %s", gotMethod, gotPath)
	}
	if gotAuth != "Bearer tok1" || gotVersion != "v9.9.9" {
		t.Fatalf("鉴权/版本头 = %q %q", gotAuth, gotVersion)
	}
	if gotFilename != "pic.png" || gotContent != "DATA" {
		t.Fatalf("文件字段 = %q %q", gotFilename, gotContent)
	}
	want := AttachmentDTO{
		ID: 501, Filename: "pic.png", Size: 4, ContentType: "image/png",
		Width: 10, Height: 20,
		URL:          "/api/v1/attachments/501?key=abc",
		ThumbnailURL: "/api/v1/attachments/501/thumbnail?key=abc",
	}
	if *att != want {
		t.Fatalf("响应解析 = %+v，期望 %+v", *att, want)
	}
}

// TestUploadAttachmentError 非 201 走 decodeError：机器码与状态码原样保留，
// 便于上层展示服务端中文原因（too_large / bad_type 等）。
func TestUploadAttachmentError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/problem+json")
		w.WriteHeader(http.StatusRequestEntityTooLarge)
		_, _ = w.Write([]byte(`{"type":"urn:litetalk:error:too_large","title":"请求体过大",` +
			`"status":413,"code":"too_large","detail":"附件不能超过 10MiB"}`))
	}))
	defer srv.Close()

	c := NewAPIClient(srv.URL)
	_, err := c.UploadAttachment("big.bin", []byte("x"))
	if err == nil {
		t.Fatal("413 应返回错误")
	}
	var ae *APIError
	if !errors.As(err, &ae) {
		t.Fatalf("错误类型 = %T，期望 *APIError", err)
	}
	if ae.Status != http.StatusRequestEntityTooLarge || ae.Code != "too_large" ||
		ae.Message != "附件不能超过 10MiB" {
		t.Fatalf("错误内容 = %+v", ae)
	}
}

// TestResolveURL 附件能力 URL 的相对路径拼接：尾斜杠归一、绝对地址原样、
// 基址为空时保持相对路径。
func TestResolveURL(t *testing.T) {
	c := NewAPIClient("https://example.com/")
	cases := map[string]string{
		"/api/v1/attachments/1?key=k":   "https://example.com/api/v1/attachments/1?key=k",
		"api/v1/attachments/1?key=k":    "https://example.com/api/v1/attachments/1?key=k",
		"http://cdn.example.com/a.png":  "http://cdn.example.com/a.png",
		"https://cdn.example.com/a.png": "https://cdn.example.com/a.png",
		"HTTPS://cdn.example.com/a.png": "HTTPS://cdn.example.com/a.png",
		"":                              "",
	}
	for in, want := range cases {
		if got := c.ResolveURL(in); got != want {
			t.Fatalf("ResolveURL(%q) = %q，期望 %q", in, got, want)
		}
	}

	// 基址热切换后拼接新地址（锁内读取）。
	c.SetBase("https://other.example.com")
	if got := c.ResolveURL("/x"); got != "https://other.example.com/x" {
		t.Fatalf("换基址后 ResolveURL = %q", got)
	}
	// 未配置基址时保持相对路径，前端仍可尝试加载。
	c.SetBase("")
	if got := c.ResolveURL("/x"); got != "/x" {
		t.Fatalf("空基址 ResolveURL = %q", got)
	}
}

// TestMessageDTODecodesAttachments 消息视图的 attachments 为可选字段：
// 有值时解析完整元数据，缺省时保持 nil（区分「无附件」）。
func TestMessageDTODecodesAttachments(t *testing.T) {
	raw := `{"id":"1","channel_id":"2","user_id":"3","content":"","created_at":"2026-10-25T00:00:00Z",` +
		`"attachments":[{"id":"501","filename":"a.txt","size":12,"content_type":"text/plain",` +
		`"width":0,"height":0,"url":"/api/v1/attachments/501?key=k"}]}`
	var m MessageDTO
	if err := json.Unmarshal([]byte(raw), &m); err != nil {
		t.Fatalf("解码失败: %v", err)
	}
	if m.Attachments == nil || len(*m.Attachments) != 1 {
		t.Fatalf("attachments 解析错误: %+v", m.Attachments)
	}
	att := (*m.Attachments)[0]
	if att.ID != 501 || att.Filename != "a.txt" || att.Size != 12 ||
		att.ContentType != "text/plain" || att.ThumbnailURL != "" {
		t.Fatalf("附件字段错误: %+v", att)
	}

	plain := `{"id":"1","channel_id":"2","user_id":"3","content":"hi","created_at":"2026-10-25T00:00:00Z"}`
	var none MessageDTO
	if err := json.Unmarshal([]byte(plain), &none); err != nil {
		t.Fatalf("无附件消息解码失败: %v", err)
	}
	if none.Attachments != nil {
		t.Fatalf("无附件时 Attachments 应为 nil，实际 %+v", *none.Attachments)
	}
}
