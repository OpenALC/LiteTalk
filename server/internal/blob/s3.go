package blob

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
)

// maxS3GetBytes 防御性读取上限（64 MiB）：正常附件在 API 层已限制大小，这里兜底
// 防止桶里被塞入超大对象时把服务端内存打满。
const maxS3GetBytes = 64 << 20

// s3Store S3 兼容对象存储实现（AWS S3 / MinIO / Ceph 等，支持路径风格）。
type s3Store struct {
	cli    *minio.Client
	bucket string
}

func newS3(cfg Config, lg *log.Logger) (Store, string, error) {
	endpoint := strings.TrimSpace(cfg.Endpoint)
	bucket := strings.TrimSpace(cfg.Bucket)
	accessKey := strings.TrimSpace(cfg.AccessKey)
	secretKey := strings.TrimSpace(cfg.SecretKey)
	switch {
	case endpoint == "":
		return nil, "", errors.New("blob: s3 驱动缺少 Endpoint")
	case bucket == "":
		return nil, "", errors.New("blob: s3 驱动缺少 Bucket")
	case accessKey == "":
		return nil, "", errors.New("blob: s3 驱动缺少 AccessKey")
	case secretKey == "":
		return nil, "", errors.New("blob: s3 驱动缺少 SecretKey")
	}
	lookup := minio.BucketLookupAuto
	if cfg.PathStyle {
		lookup = minio.BucketLookupPath
	}
	// minio.New 只解析端点/构造签名器，不发网络请求；桶是否存在留到首次读写时暴露。
	cli, err := minio.New(endpoint, &minio.Options{
		Creds:        credentials.NewStaticV4(accessKey, secretKey, ""),
		Secure:       cfg.UseSSL,
		Region:       strings.TrimSpace(cfg.Region),
		BucketLookup: lookup,
	})
	if err != nil {
		return nil, "", fmt.Errorf("blob: 初始化 S3 客户端: %w", err)
	}
	if lg != nil {
		lg.Printf("[blob] s3 endpoint=%s bucket=%s path_style=%v", endpoint, bucket, cfg.PathStyle)
	}
	return &s3Store{cli: cli, bucket: bucket}, "s3", nil
}

// Put 上传对象（同 key 覆盖）。
func (s *s3Store) Put(ctx context.Context, key string, data []byte, contentType string) error {
	if err := validateKey(key); err != nil {
		return err
	}
	if contentType == "" {
		contentType = "application/octet-stream"
	}
	if _, err := s.cli.PutObject(ctx, s.bucket, key, bytes.NewReader(data), int64(len(data)),
		minio.PutObjectOptions{ContentType: contentType}); err != nil {
		return fmt.Errorf("blob: 上传对象 %q: %w", key, err)
	}
	return nil
}

// Get 读取对象；不存在返回 ErrNotFound，超过上限的错误不返回部分内容。
func (s *s3Store) Get(ctx context.Context, key string) ([]byte, error) {
	if err := validateKey(key); err != nil {
		return nil, err
	}
	obj, err := s.cli.GetObject(ctx, s.bucket, key, minio.GetObjectOptions{})
	if err != nil {
		return nil, s3Error(key, err)
	}
	defer obj.Close()
	// GetObject 是惰性的，先 Stat 一次把 NoSuchKey 等错误提前暴露，避免读半截。
	if _, err := obj.Stat(); err != nil {
		return nil, s3Error(key, err)
	}
	var buf bytes.Buffer
	n, err := io.Copy(&buf, io.LimitReader(obj, maxS3GetBytes+1))
	if err != nil {
		return nil, s3Error(key, err)
	}
	if n > maxS3GetBytes {
		return nil, fmt.Errorf("blob: 对象 %q 超过读取上限 %d 字节", key, maxS3GetBytes)
	}
	return buf.Bytes(), nil
}

// Delete 删除对象；S3 的 DeleteObject 本身对不存在对象返回成功，保持幂等。
func (s *s3Store) Delete(ctx context.Context, key string) error {
	if err := validateKey(key); err != nil {
		return err
	}
	if err := s.cli.RemoveObject(ctx, s.bucket, key, minio.RemoveObjectOptions{}); err != nil {
		return fmt.Errorf("blob: 删除对象 %q: %w", key, err)
	}
	return nil
}

// s3Error 把对象不存在归一成 ErrNotFound，其余错误保留原始链。
func s3Error(key string, err error) error {
	resp := minio.ToErrorResponse(err)
	if resp.Code == "NoSuchKey" || resp.Code == "NoSuchObject" || resp.Code == "NotFound" ||
		resp.StatusCode == http.StatusNotFound {
		return ErrNotFound
	}
	return fmt.Errorf("blob: 对象 %q: %w", key, err)
}
