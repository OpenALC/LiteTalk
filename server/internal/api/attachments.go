// 附件上传与下载（#41 B2）。
//
// 上传：multipart 单 file 字段 → attachment.Process（嗅探/白名单/缩略图）→ blob.Put
// → store.CreateAttachment。任一步失败都尽力删除已写对象，不留孤儿 blob。
// 下载：能力 URL（路径 id + query key），免 Bearer，供 img 标签直接加载；key 是
// 256 位随机 hex，ID 与 key 不匹配、附件不存在或所属消息已软删一律 404。
package api

import (
	"context"
	"crypto/subtle"
	"errors"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"path"
	"strconv"
	"strings"
	"time"

	"github.com/OpenALC/LiteTalk/server/internal/attachment"
	"github.com/OpenALC/LiteTalk/server/internal/blob"
	"github.com/OpenALC/LiteTalk/server/internal/snowflake"
	"github.com/OpenALC/LiteTalk/server/internal/store"
)

const (
	// uploadRateLimit 每用户每分钟上传次数（解码图片与写盘都相对昂贵）。
	uploadRateLimit  = 30
	uploadRateWindow = time.Minute
	// downloadMissRate 按 IP 的「失败下载」限流：只统计 404 路径，正常加载图片
	// （含同一 NAT 下多人共用出口 IP）不受影响，同时抬高扫描成本。
	downloadMissRateLimit = 60
	downloadMissWindow    = time.Minute
	// downloadCacheControl 能力 URL 指向的内容不可变（附件不可覆盖），可私有缓存一天。
	downloadCacheControl = "private, max-age=86400, immutable"
	// uploadCleanupTimeout 清理已写 blob 的超时：请求 ctx 可能已被取消。
	uploadCleanupTimeout = 5 * time.Second
)

// UploadAttachment POST /api/v1/attachments（multipart，登录可传）。
func (s *Server) UploadAttachment(ctx context.Context, request UploadAttachmentRequestObject) (UploadAttachmentResponseObject, error) {
	u := currentUser(ctx)
	if u == nil {
		return nil, errf(http.StatusUnauthorized, "unauthorized", "缺少访问令牌")
	}
	if request.Body == nil {
		return nil, errf(http.StatusBadRequest, "bad_request", "请求体不是合法的 multipart 表单")
	}
	if s.blob == nil {
		// 生产由 cmd/server 注入；测试漏注入时不暴露实现细节，也不浪费解码 CPU。
		s.log.Printf("[500] POST /api/v1/attachments: blob 存储未配置")
		return nil, errf(http.StatusInternalServerError, "internal", "服务器内部错误")
	}
	if e := s.allowRate(ctx, "attachment:"+strconv.FormatInt(u.UserID, 10),
		uploadRateLimit, uploadRateWindow, "上传过于频繁，请稍后再试"); e != nil {
		return nil, e
	}
	filename, data, e := readUploadFile(request.Body, s.upload.MaxBytes)
	if e != nil {
		return nil, e
	}
	proc, err := attachment.Process(filename, data, s.upload)
	if err != nil {
		return nil, s.uploadProcessErr(err)
	}

	id := snowflake.Next()
	storageKey, thumbKey := attachment.Keys(id)
	if err := s.blob.Put(ctx, storageKey, proc.Data, proc.ContentType); err != nil {
		s.log.Printf("[500] POST /api/v1/attachments: 写附件对象失败: %v", err)
		return nil, errf(http.StatusInternalServerError, "storage_error", "附件保存失败，请稍后重试")
	}
	written := []string{storageKey}
	thumbWritten := false
	if len(proc.Thumb) > 0 {
		if err := s.blob.Put(ctx, thumbKey, proc.Thumb, "image/jpeg"); err != nil {
			s.log.Printf("[500] POST /api/v1/attachments: 写缩略图对象失败: %v", err)
			s.deleteBlobs(ctx, written)
			return nil, errf(http.StatusInternalServerError, "storage_error", "附件保存失败，请稍后重试")
		}
		written = append(written, thumbKey)
		thumbWritten = true
	}

	rec := &store.Attachment{
		ID:          id,
		UploaderID:  u.UserID,
		Filename:    proc.Filename,
		ContentType: proc.ContentType,
		Size:        proc.Size,
		Width:       proc.Width,
		Height:      proc.Height,
		StorageKey:  storageKey,
		AccessKey:   proc.AccessKey,
		SHA256:      proc.SHA256,
	}
	if thumbWritten {
		rec.ThumbKey = thumbKey
	}
	if err := s.store.CreateAttachment(ctx, rec); err != nil {
		s.log.Printf("[500] POST /api/v1/attachments: 写元数据失败: %v", err)
		s.deleteBlobs(ctx, written)
		return nil, errf(http.StatusInternalServerError, "storage_error", "附件保存失败，请稍后重试")
	}
	return UploadAttachment201JSONResponse(attachmentView(rec)), nil
}

// DownloadAttachment GET /api/v1/attachments/{id}?key=...（免 Bearer）。
func (s *Server) DownloadAttachment(ctx context.Context, request DownloadAttachmentRequestObject) (DownloadAttachmentResponseObject, error) {
	rec, e := s.lookupDownloadableAttachment(ctx, request.Id, request.Params.Key)
	if e != nil {
		return nil, e
	}
	data, e := s.readAttachmentBlob(ctx, rec.StorageKey)
	if e != nil {
		return nil, e
	}
	contentType := s.downloadContentType(rec.ContentType)
	return attachmentBytesResponse{
		Data:        data,
		ContentType: contentType,
		Filename:    rec.Filename,
		Inline:      isInlineContentType(contentType),
	}, nil
}

// DownloadAttachmentThumbnail GET /api/v1/attachments/{id}/thumbnail?key=...（免 Bearer）。
func (s *Server) DownloadAttachmentThumbnail(ctx context.Context, request DownloadAttachmentThumbnailRequestObject) (DownloadAttachmentThumbnailResponseObject, error) {
	rec, e := s.lookupDownloadableAttachment(ctx, request.Id, request.Params.Key)
	if e != nil {
		return nil, e
	}
	if rec.ThumbKey == "" {
		return nil, s.downloadMiss(ctx)
	}
	data, e := s.readAttachmentBlob(ctx, rec.ThumbKey)
	if e != nil {
		return nil, e
	}
	return attachmentBytesResponse{
		Data:        data,
		ContentType: "image/jpeg",
		Filename:    thumbnailFilename(rec.Filename),
		Inline:      true,
	}, nil
}

// attachmentBytesResponse 二进制下载响应：生成物默认写死 application/octet-stream，
// 这里实现生成的响应接口，自行写 Content-Type/Content-Disposition/缓存头。
type attachmentBytesResponse struct {
	Data        []byte
	ContentType string
	Filename    string
	Inline      bool
}

func (r attachmentBytesResponse) VisitDownloadAttachmentResponse(w http.ResponseWriter) error {
	return r.writeTo(w)
}

func (r attachmentBytesResponse) VisitDownloadAttachmentThumbnailResponse(w http.ResponseWriter) error {
	return r.writeTo(w)
}

func (r attachmentBytesResponse) writeTo(w http.ResponseWriter) error {
	h := w.Header()
	h.Set("Content-Type", r.ContentType)
	h.Set("Content-Length", strconv.Itoa(len(r.Data)))
	h.Set("Content-Disposition", contentDisposition(r.Inline, r.Filename))
	h.Set("Cache-Control", downloadCacheControl)
	h.Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(http.StatusOK)
	_, err := w.Write(r.Data)
	return err
}

// attachmentView 把库内元数据转成对外 DTO。url/thumbnail_url 携带能力 key；
// uploader_id/access_key/storage_key 绝不单独曝光。
func attachmentView(a *store.Attachment) Attachment {
	width, height := a.Width, a.Height
	view := Attachment{
		Id:          strconv.FormatInt(a.ID, 10),
		Filename:    a.Filename,
		Size:        a.Size,
		ContentType: a.ContentType,
		Width:       &width,
		Height:      &height,
		Url:         attachmentURL(a.ID, a.AccessKey),
	}
	if a.ThumbKey != "" {
		thumb := attachmentThumbnailURL(a.ID, a.AccessKey)
		view.ThumbnailUrl = &thumb
	}
	return view
}

func attachmentURL(id int64, accessKey string) string {
	return fmt.Sprintf("/api/v1/attachments/%d?key=%s", id, accessKey)
}

func attachmentThumbnailURL(id int64, accessKey string) string {
	return fmt.Sprintf("/api/v1/attachments/%d/thumbnail?key=%s", id, accessKey)
}

// uploadProcessErr 把 attachment 包的哨兵错误映射为契约内错误码。
func (s *Server) uploadProcessErr(err error) *apiErr {
	switch {
	case errors.Is(err, attachment.ErrTooLarge):
		return errf(http.StatusRequestEntityTooLarge, "too_large", "文件超过大小上限")
	case errors.Is(err, attachment.ErrBadType):
		return errf(http.StatusBadRequest, "bad_type", "不支持的文件类型")
	case errors.Is(err, attachment.ErrBadImage):
		return errf(http.StatusBadRequest, "bad_image", "图片内容无效")
	default:
		s.log.Printf("[500] POST /api/v1/attachments: 内容校验失败: %v", err)
		return errf(http.StatusInternalServerError, "internal", "服务器内部错误")
	}
}

// readUploadFile 从 multipart 流中读取唯一的 file 字段。
// 只读 max+1 字节即可判定超限，不把超限文件整个吞进内存；其他字段或第二个
// file 一律 400，避免同一请求里塞入未定义的内容。
func readUploadFile(reader *multipart.Reader, max int64) (string, []byte, *apiErr) {
	if max <= 0 {
		max = attachment.DefaultMaxBytes
	}
	var (
		filename string
		data     []byte
		got      bool
	)
	for {
		part, err := reader.NextPart()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return "", nil, uploadReadErr(err)
		}
		if got || part.FormName() != "file" {
			_ = part.Close()
			return "", nil, errf(http.StatusBadRequest, "bad_request", "只支持单个 file 字段")
		}
		got = true
		filename = part.FileName()
		data, err = io.ReadAll(io.LimitReader(part, max+1))
		_ = part.Close()
		if err != nil {
			return "", nil, uploadReadErr(err)
		}
	}
	if !got {
		return "", nil, errf(http.StatusBadRequest, "bad_request", "缺少 file 字段")
	}
	if int64(len(data)) > max {
		return "", nil, errf(http.StatusRequestEntityTooLarge, "too_large", "文件超过大小上限")
	}
	return filename, data, nil
}

// uploadReadErr 读取 multipart 时的错误映射：体超限 413，其余按表单损坏 400。
func uploadReadErr(err error) *apiErr {
	var maxErr *http.MaxBytesError
	if errors.As(err, &maxErr) {
		return errf(http.StatusRequestEntityTooLarge, "too_large", "请求体过大")
	}
	return errf(http.StatusBadRequest, "bad_request", "请求体不是合法的 multipart 表单")
}

// lookupDownloadableAttachment 校验能力 URL 并回读附件元数据。
// 统一用 s.downloadMiss 处理「不存在/凭证错/消息已删」，不区分原因。
func (s *Server) lookupDownloadableAttachment(ctx context.Context, id int64, key string) (*store.Attachment, *apiErr) {
	if e := checkID(id); e != nil {
		return nil, e
	}
	if s.blob == nil {
		s.log.Printf("[500] GET %s: blob 存储未配置", reqPath(ctx))
		return nil, errf(http.StatusInternalServerError, "internal", "服务器内部错误")
	}
	rec, err := s.store.AttachmentByID(ctx, id)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return nil, s.downloadMiss(ctx)
		}
		s.log.Printf("[500] GET %s: 查询附件 %d 失败: %v", reqPath(ctx), id, err)
		return nil, errf(http.StatusInternalServerError, "internal", "服务器内部错误")
	}
	if !sameAccessKey(rec.AccessKey, key) {
		return nil, s.downloadMiss(ctx)
	}
	// 已关联消息的附件：软删消息的附件随消息一起失效（回复跳转仍 404）。
	// 未关联（MessageID=0）的附件按能力 URL 放行——只有上传者拿到过该 URL。
	if rec.MessageID != 0 {
		msg, err := s.store.MessageByID(ctx, rec.MessageID)
		if err != nil {
			if errors.Is(err, store.ErrNotFound) {
				return nil, s.downloadMiss(ctx)
			}
			s.log.Printf("[500] GET %s: 查询附件所属消息 %d 失败: %v", reqPath(ctx), rec.MessageID, err)
			return nil, errf(http.StatusInternalServerError, "internal", "服务器内部错误")
		}
		if msg.DeletedAt != nil {
			return nil, s.downloadMiss(ctx)
		}
	}
	return rec, nil
}

// readAttachmentBlob 读取对象内容；对象丢失按 404（不算 500，避免误报存储故障）。
func (s *Server) readAttachmentBlob(ctx context.Context, key string) ([]byte, *apiErr) {
	data, err := s.blob.Get(ctx, key)
	if err != nil {
		if errors.Is(err, blob.ErrNotFound) {
			s.log.Printf("[warn] GET %s: blob 对象 %s 丢失", reqPath(ctx), key)
			return nil, s.downloadMiss(ctx)
		}
		s.log.Printf("[500] GET %s: 读取 blob 对象 %s 失败: %v", reqPath(ctx), key, err)
		return nil, errf(http.StatusInternalServerError, "internal", "服务器内部错误")
	}
	return data, nil
}

// downloadMiss 失败下载按 IP 限流：先记一次，超限返回 429，否则统一 404。
func (s *Server) downloadMiss(ctx context.Context) *apiErr {
	if e := s.allowRate(ctx, "attachment_miss:"+reqIP(ctx),
		downloadMissRateLimit, downloadMissWindow, "下载请求过于频繁，请稍后再试"); e != nil {
		return e
	}
	return errf(http.StatusNotFound, "not_found", "资源不存在")
}

// sameAccessKey 常量时间比较下载凭证；长度不等直接失败（不进入比较函数）。
func sameAccessKey(want, got string) bool {
	if want == "" || len(want) != len(got) {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(want), []byte(got)) == 1
}

// downloadContentType 只有库内值仍在当前白名单里才原样下发；否则降级为
// application/octet-stream（防止历史配置/脏数据把 HTML 等类型当图片内联）。
func (s *Server) downloadContentType(stored string) string {
	ct := canonicalContentType(stored)
	for _, allowed := range s.upload.AllowedTypes {
		if canonicalContentType(allowed) == ct {
			return ct
		}
	}
	return "application/octet-stream"
}

// canonicalContentType 去参数并转小写（与 attachment 包的白名单比对口径一致）。
func canonicalContentType(ct string) string {
	if i := strings.IndexByte(ct, ';'); i >= 0 {
		ct = ct[:i]
	}
	return strings.ToLower(strings.TrimSpace(ct))
}

// isInlineContentType 仅栅格图片内联展示；SVG 等可执行类型即使被配置进白名单
// 也按下载处理（nosniff 之外再加一层）。
func isInlineContentType(ct string) bool {
	switch ct {
	case "image/jpeg", "image/png", "image/gif", "image/webp":
		return true
	}
	return false
}

// contentDisposition 生成 Content-Disposition 值：非 ASCII 文件名按 RFC 2231
// （filename*=UTF-8”...）转义；控制字符先剔除，避免响应头注入。
func contentDisposition(inline bool, filename string) string {
	disp := "attachment"
	if inline {
		disp = "inline"
	}
	name := strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return -1
		}
		return r
	}, filename)
	name = strings.TrimSpace(name)
	if name == "" {
		name = "file"
	}
	if v := mime.FormatMediaType(disp, map[string]string{"filename": name}); v != "" {
		return v
	}
	return disp
}

// thumbnailFilename 缩略图统一 JPEG：去掉原扩展名后加 .jpg。
func thumbnailFilename(filename string) string {
	base := strings.TrimSuffix(filename, path.Ext(filename))
	if base == "" {
		base = "file"
	}
	return base + ".jpg"
}

// deleteBlobs 尽力清理已写对象；用独立超时，避免请求取消导致清理被跳过。
func (s *Server) deleteBlobs(ctx context.Context, keys []string) {
	cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), uploadCleanupTimeout)
	defer cancel()
	for _, key := range keys {
		if err := s.blob.Delete(cleanup, key); err != nil {
			s.log.Printf("[blob] 清理对象 %s 失败: %v", key, err)
		}
	}
}
