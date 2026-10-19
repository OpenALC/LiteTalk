package main

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
)

// TestResolveJWTSecretGeneratesAndReuses 生产首次启动生成 jwt.secret（0600），
// 再次启动读取同一密钥（#27：`--domain` 一条命令首启，重启后旧令牌不失效）。
func TestResolveJWTSecretGeneratesAndReuses(t *testing.T) {
	dir := t.TempDir()
	secret, generated, err := resolveJWTSecret("", dir, false)
	if err != nil {
		t.Fatalf("首次启动应自动生成密钥: %v", err)
	}
	if !generated {
		t.Fatal("首次启动应报告 generated=true")
	}
	if len(secret) != 64 {
		t.Fatalf("应生成 64 位 hex 密钥，实际 %d 位", len(secret))
	}

	path := filepath.Join(dir, jwtSecretFile)
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("密钥文件应落盘: %v", err)
	}
	if strings.TrimSpace(string(b)) != secret {
		t.Fatalf("文件内容与返回密钥不一致: %q != %q", strings.TrimSpace(string(b)), secret)
	}
	// Windows 不维护 Unix 权限位（Go 只映射只读属性），跳过权限断言。
	if runtime.GOOS != "windows" {
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if perm := info.Mode().Perm(); perm != 0o600 {
			t.Fatalf("jwt.secret 权限应为 0600，实际 %o", perm)
		}
	}

	again, generatedAgain, err := resolveJWTSecret("", dir, false)
	if err != nil {
		t.Fatalf("重启读取失败: %v", err)
	}
	if generatedAgain {
		t.Fatal("已有密钥文件不应重新生成")
	}
	if again != secret {
		t.Fatal("重启后应复用同一密钥")
	}
}

// TestResolveJWTSecretEnvExplicit 显式 env 优先：直接采用、不创建文件；
// 生产弱密钥仍拒绝，dev 保持放行。
func TestResolveJWTSecretEnvExplicit(t *testing.T) {
	dir := t.TempDir()
	const explicit = "0123456789abcdef0123456789abcdef"
	secret, generated, err := resolveJWTSecret(explicit, dir, false)
	if err != nil || generated || secret != explicit {
		t.Fatalf("显式密钥应原样返回: secret=%q generated=%v err=%v", secret, generated, err)
	}
	if _, err := os.Stat(filepath.Join(dir, jwtSecretFile)); !os.IsNotExist(err) {
		t.Fatalf("显式 env 不应创建密钥文件，实际 err=%v", err)
	}

	if _, _, err := resolveJWTSecret("CHANGE_ME", dir, false); err == nil {
		t.Fatal("生产模式显式弱密钥应拒绝")
	}
	if secret, _, err := resolveJWTSecret("CHANGE_ME", dir, true); err != nil || secret != "CHANGE_ME" {
		t.Fatalf("dev 模式显式密钥应放行，实际 secret=%q err=%v", secret, err)
	}
}

// TestResolveJWTSecretDevUnchanged dev 构建不生成文件、空串交给 auth.NewJWT 回落。
func TestResolveJWTSecretDevUnchanged(t *testing.T) {
	dir := t.TempDir()
	secret, generated, err := resolveJWTSecret("", dir, true)
	if err != nil || generated || secret != "" {
		t.Fatalf("dev 空密钥应保持空串回落，实际 secret=%q generated=%v err=%v", secret, generated, err)
	}
	if _, err := os.Stat(filepath.Join(dir, jwtSecretFile)); !os.IsNotExist(err) {
		t.Fatalf("dev 模式不应生成密钥文件，实际 err=%v", err)
	}
}

// TestResolveJWTSecretRejectsWeakFile 预置/损坏的密钥文件同样过生产弱密钥校验。
func TestResolveJWTSecretRejectsWeakFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, jwtSecretFile)
	if err := os.WriteFile(path, []byte("short\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := resolveJWTSecret("", dir, false); err == nil || !strings.Contains(err.Error(), path) {
		t.Fatalf("过短的持久化密钥应拒绝并指明文件，实际 %v", err)
	}
}

// TestResolveJWTSecretConcurrentFirstStart 并发首启只有一个写入者，其余复用同一密钥。
func TestResolveJWTSecretConcurrentFirstStart(t *testing.T) {
	dir := t.TempDir()
	const n = 8
	secrets := make([]string, n)
	errs := make([]error, n)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			secrets[i], _, errs[i] = resolveJWTSecret("", dir, false)
		}(i)
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("并发解析 #%d 失败: %v", i, err)
		}
		if secrets[i] != secrets[0] {
			t.Fatalf("并发首启应得到同一密钥: #%d=%q != #0=%q", i, secrets[i], secrets[0])
		}
	}
	persisted, err := readJWTSecretFile(filepath.Join(dir, jwtSecretFile))
	if err != nil {
		t.Fatal(err)
	}
	if persisted != secrets[0] {
		t.Fatalf("磁盘密钥 %q 与内存密钥 %q 不一致", persisted, secrets[0])
	}
}
