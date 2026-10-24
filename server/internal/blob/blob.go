// Package blob 附件二进制存储抽象（#41 B1）：本地目录与 S3 兼容对象存储两套实现，
// 由 Config.Driver 选择（空 = local）。上层只按不透明 key 读写，不关心落盘形态。
// 参考 mailer 的可插拔范式：外部依赖可替换、关闭时从简。
package blob

import (
	"context"
	"errors"
	"fmt"
	"log"
	"strings"
)

// ErrNotFound 对象不存在（Get 用；Delete 对不存在视为成功，保持幂等）。
var ErrNotFound = errors.New("blob: not found")

// Store 对象存储接口。key 是调用方生成的不透明标识，只允许 [A-Za-z0-9._-]。
type Store interface {
	// Put 写入对象，覆盖同 key 的旧内容；实现必须保证读方看不到半截对象。
	Put(ctx context.Context, key string, data []byte, contentType string) error
	// Get 读取对象；不存在返回 ErrNotFound。
	Get(ctx context.Context, key string) ([]byte, error)
	// Delete 删除对象；不存在视为成功（幂等）。
	Delete(ctx context.Context, key string) error
}

// Config 存储配置。Driver 为空或 "local" 用 Dir；"s3" 用 Endpoint/Bucket/凭证。
// 环境变量到本结构的映射由 cmd/server 负责，本包只消费已解析的值。
type Config struct {
	Driver string // "" | "local" | "s3"；空 = local
	Dir    string // local 根目录（必须显式给出，默认值由调用方决定）

	Endpoint  string // S3 兼容端点，如 minio.example.com:9000
	Bucket    string
	AccessKey string
	SecretKey string
	Region    string
	UseSSL    bool // 端点是否走 https
	PathStyle bool // true = 路径风格（MinIO/Ceph 等），false = 虚拟主机风格
}

// New 按配置构造存储实现，返回实现与驱动名（"local"/"s3"）。
// 只校验配置，不做网络探测：S3 不可达属于运行时错误，不应阻塞服务启动。
func New(cfg Config, lg *log.Logger) (Store, string, error) {
	switch strings.ToLower(strings.TrimSpace(cfg.Driver)) {
	case "", "local":
		return newLocal(cfg, lg)
	case "s3":
		return newS3(cfg, lg)
	default:
		return nil, "", fmt.Errorf("blob: 未知驱动 %q", cfg.Driver)
	}
}

// maxKeyLen 与 attachments.storage_key/thumb_key 的 VARCHAR(128) 对齐。
const maxKeyLen = 128

// validateKey 防路径穿越：key 不得含路径分隔符、不得是 "."/".." 或含 ".." 片段，
// 字符集只允许 [A-Za-z0-9._-]。local 实现据此把 key 直接拼到根目录下也安全；
// S3 实现同样校验，避免把客户端可控值当对象前缀使用。
func validateKey(key string) error {
	if key == "" {
		return errors.New("blob: key 为空")
	}
	if len(key) > maxKeyLen {
		return fmt.Errorf("blob: key 超过 %d 字节", maxKeyLen)
	}
	if key == "." || key == ".." || strings.Contains(key, "..") {
		return fmt.Errorf("blob: key %q 含路径穿越片段", key)
	}
	for i := 0; i < len(key); i++ {
		c := key[i]
		ok := c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' ||
			c == '.' || c == '_' || c == '-'
		if !ok {
			return fmt.Errorf("blob: key %q 含非法字符", key)
		}
	}
	return nil
}
