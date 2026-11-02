package linkpreview

import (
	"errors"
	"testing"
)

// TestCheckDialableBlocksInternalTargets SSRF 拦截三例（#52）：内网段、云元数据、
// 回环名。全部是字面量/名字判定，不做真实 DNS。多打几类保留段防回归。
func TestCheckDialableBlocksInternalTargets(t *testing.T) {
	blocked := []string{
		"http://10.0.0.1/hook",             // 私网段（任务书指定）
		"http://169.254.169.254/latest",    // 云元数据（任务书指定）
		"http://localhost/hook",            // 回环名（任务书指定）
		"https://127.0.0.1:8077/hook",      // 回环 IP
		"http://192.168.1.10/hook",         // 私网段
		"http://172.16.0.5/hook",           // 私网段
		"http://[::1]/hook",                // IPv6 回环
		"http://[::ffff:10.0.0.1]/hook",    // IPv4-mapped 私网
		"http://user:pass@10.0.0.1/hook",   // userinfo（凭据注入面）
		"ftp://example.com/hook",           // 非 http(s) scheme
		"http://metadata.google.internal/", // GCP 元数据主机名（.internal 不可解析 → fail-closed）
		"",                                 // 空 URL
	}
	for _, raw := range blocked {
		if err := CheckDialable(raw); !errors.Is(err, ErrBlocked) {
			t.Fatalf("CheckDialable(%q) 应拒绝，实际 %v", raw, err)
		}
	}
}

// TestCheckDialableShapeErrors 形状错误同样拒绝（与 validateURL 同口径）。
func TestCheckDialableShapeErrors(t *testing.T) {
	for _, raw := range []string{"not a url", "http://", "http://exa mple.com/"} {
		if err := CheckDialable(raw); err == nil {
			t.Fatalf("CheckDialable(%q) 应拒绝", raw)
		}
	}
}
