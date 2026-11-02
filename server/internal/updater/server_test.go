package updater

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// testSecret 36 字节（≥MinSecretLen），与生产 openssl rand -hex 32 同口径。
const testSecret = "test-webhook-secret-0123456789abcdef"

func releaseBody(tag string, prerelease bool) []byte {
	pre := "false"
	if prerelease {
		pre = "true"
	}
	return []byte(`{
  "action": "published",
  "release": {"tag_name": "` + tag + `", "prerelease": ` + pre + `},
  "repository": {"full_name": "OpenALC/LiteTalk"}
}`)
}

// webhookEnv 组一套可测的 HTTP 面 + 假 GitHub + 可变 healthz。
type webhookEnv struct {
	srv    *httptest.Server
	server *Server
	st     *StateStore
	gh     *fakeGitHub
	health *fakeHealthz
	runner *fakeRunner
}

func newWebhookEnv(t *testing.T, mut func(*Config)) *webhookEnv {
	t.Helper()
	gh := newFakeGitHub(t)
	health := &fakeHealthz{ver: "v1.0.0-alpha"}
	cfg, _ := newTestConfig(t, gh.srv.URL, health.start(t))
	cfg.AllowPrerelease = false
	cfg.Secret = []byte(testSecret)
	cfg.Enabled = true
	cfg.HealthTimeout = 100 * time.Millisecond
	if mut != nil {
		mut(&cfg)
	}
	writeFile(t, filepath.Join(cfg.InstallDir, "litetalk-server"), "OLD-BINARY")
	gh.publish(t, "v1.1.0-alpha", "NEW-BINARY", true)
	gh.publish(t, "v1.1.0", "NEW-STABLE", false)

	runner := &fakeRunner{onRun: func(string, []string) { health.onRestart() }}
	st, err := OpenState(cfg.StateDir)
	if err != nil {
		t.Fatal(err)
	}
	up := New(cfg, runner, st, testLogger(), nil)
	up.FreeDisk = func(string) (uint64, error) { return 1 << 30, nil }
	// 虚拟时钟：healthz 超时路径瞬间走完，不真实等待
	clk := newFakeClock()
	up.Now = clk.Now
	up.Sleep = func(d time.Duration) { clk.advance(d) }
	up.HealthPoll = time.Millisecond
	server, err := NewServer(cfg, up, st, testLogger())
	if err != nil {
		t.Fatal(err)
	}
	env := &webhookEnv{server: server, st: st, gh: gh, health: health, runner: runner}
	env.srv = httptest.NewServer(server.Handler())
	t.Cleanup(env.srv.Close)
	return env
}

// post 发送 webhook 请求。
func (e *webhookEnv) post(t *testing.T, event, delivery string, body []byte, sign bool) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/hooks/github-release", bytes.NewReader(body))
	req.Header.Set("X-GitHub-Event", event)
	req.Header.Set("X-GitHub-Delivery", delivery)
	if sign {
		req.Header.Set("X-Hub-Signature-256", "sha256="+hmacHex([]byte(testSecret), body))
	}
	rec := httptest.NewRecorder()
	e.server.Handler().ServeHTTP(rec, req)
	return rec
}

func TestWebhookHMAC(t *testing.T) {
	env := newWebhookEnv(t, func(c *Config) { c.AllowPrerelease = true })
	body := releaseBody("v1.1.0-alpha", true)

	// 缺签名
	req := httptest.NewRequest(http.MethodPost, "/hooks/github-release", bytes.NewReader(body))
	req.Header.Set("X-GitHub-Event", "release")
	rec := httptest.NewRecorder()
	env.server.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("缺签名 = %d, want 401", rec.Code)
	}

	// 错误签名
	req2 := httptest.NewRequest(http.MethodPost, "/hooks/github-release", bytes.NewReader(body))
	req2.Header.Set("X-GitHub-Event", "release")
	req2.Header.Set("X-Hub-Signature-256", "sha256="+strings.Repeat("00", 32))
	rec2 := httptest.NewRecorder()
	env.server.Handler().ServeHTTP(rec2, req2)
	if rec2.Code != http.StatusUnauthorized {
		t.Fatalf("错签名 = %d, want 401", rec2.Code)
	}

	// 正确签名 → 202（更新异步跑，healthz 仍报旧版本会走回滚，这里只看接收）
	if rec3 := env.post(t, "release", "d-ok", body, true); rec3.Code != http.StatusAccepted {
		t.Fatalf("正确签名 = %d, want 202", rec3.Code)
	}
	waitFor(t, 2*time.Second, func() bool { return env.st.Get().LastResult != "" }, "更新结束")
}

func TestWebhookEventFilterAndPing(t *testing.T) {
	env := newWebhookEnv(t, func(c *Config) { c.AllowPrerelease = true })

	// ping：202 并忽略
	if rec := env.post(t, "ping", "d-ping", []byte(`{}`), true); rec.Code != http.StatusAccepted {
		t.Fatalf("ping = %d, want 202", rec.Code)
	}
	// 其他事件：400
	if rec := env.post(t, "push", "d-push", []byte(`{}`), true); rec.Code != http.StatusBadRequest {
		t.Fatalf("push 事件 = %d, want 400", rec.Code)
	}
	// release 但 action 非 published：202 忽略
	body := []byte(`{"action":"edited","release":{"tag_name":"v1.1.0-alpha"},"repository":{"full_name":"OpenALC/LiteTalk"}}`)
	if rec := env.post(t, "release", "d-edited", body, true); rec.Code != http.StatusAccepted {
		t.Fatalf("edited = %d, want 202", rec.Code)
	}
	if calls := env.runner.snapshot(); len(calls) != 0 {
		t.Fatalf("过滤路径不得更新: %v", calls)
	}
}

func TestWebhookRepoAndTagWhitelist(t *testing.T) {
	env := newWebhookEnv(t, func(c *Config) { c.AllowPrerelease = true })

	body := []byte(`{"action":"published","release":{"tag_name":"v1.1.0-alpha"},"repository":{"full_name":"evil/repo"}}`)
	if rec := env.post(t, "release", "d-repo", body, true); rec.Code != http.StatusBadRequest {
		t.Fatalf("仓库白名单 = %d, want 400", rec.Code)
	}
	body2 := []byte(`{"action":"published","release":{"tag_name":"v1.1.0;rm -rf /"},"repository":{"full_name":"OpenALC/LiteTalk"}}`)
	if rec := env.post(t, "release", "d-tag", body2, true); rec.Code != http.StatusBadRequest {
		t.Fatalf("tag 白名单 = %d, want 400", rec.Code)
	}
	if calls := env.runner.snapshot(); len(calls) != 0 {
		t.Fatalf("白名单拒绝不得更新: %v", calls)
	}
}

func TestWebhookPrereleasePolicyDefaultRejects(t *testing.T) {
	// 默认 AllowPrerelease=false：alpha 202 忽略，不更新
	env := newWebhookEnv(t, nil)
	if rec := env.post(t, "release", "d-alpha", releaseBody("v1.1.0-alpha", true), true); rec.Code != http.StatusAccepted {
		t.Fatalf("默认通道 alpha = %d, want 202", rec.Code)
	}
	time.Sleep(50 * time.Millisecond)
	if calls := env.runner.snapshot(); len(calls) != 0 {
		t.Fatalf("默认通道不得更新预发布: %v", calls)
	}
	if env.st.Get().LastResult != "" {
		t.Fatalf("默认通道拒绝不应写状态: %+v", env.st.Get())
	}

	// 显式放行后同一 payload 会触发更新
	env2 := newWebhookEnv(t, func(c *Config) { c.AllowPrerelease = true })
	env2.health.arm("v1.1.0-alpha")
	if rec := env2.post(t, "release", "d-alpha", releaseBody("v1.1.0-alpha", true), true); rec.Code != http.StatusAccepted {
		t.Fatalf("放行后 = %d, want 202", rec.Code)
	}
	waitFor(t, 2*time.Second, func() bool { return env2.st.Get().CurrentVersion == "v1.1.0-alpha" }, "预发布更新完成")
}

func TestWebhookDisabledOnlyRecords(t *testing.T) {
	env := newWebhookEnv(t, func(c *Config) {
		c.Enabled = false
		c.AllowPrerelease = true
	})
	if rec := env.post(t, "release", "d-off", releaseBody("v1.1.0-alpha", true), true); rec.Code != http.StatusAccepted {
		t.Fatalf("ENABLED=0 = %d, want 202", rec.Code)
	}
	time.Sleep(50 * time.Millisecond)
	if calls := env.runner.snapshot(); len(calls) != 0 {
		t.Fatalf("ENABLED=0 不得更新: %v", calls)
	}
	if env.st.Get().CurrentVersion != "" {
		t.Fatalf("ENABLED=0 不得写成功状态: %+v", env.st.Get())
	}
	// 校验仍在：坏签名照样 401
	req := httptest.NewRequest(http.MethodPost, "/hooks/github-release", bytes.NewReader(releaseBody("v1.1.0-alpha", true)))
	req.Header.Set("X-GitHub-Event", "release")
	rec := httptest.NewRecorder()
	env.server.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("关闭状态下仍须校验签名: %d", rec.Code)
	}
}

func TestWebhookDedupAndDowngrade(t *testing.T) {
	env := newWebhookEnv(t, func(c *Config) {
		c.Enabled = true
		c.AllowPrerelease = true
	})
	env.health.arm("v1.1.0-alpha")
	if rec := env.post(t, "release", "d-1", releaseBody("v1.1.0-alpha", true), true); rec.Code != http.StatusAccepted {
		t.Fatalf("首次 = %d", rec.Code)
	}
	waitFor(t, 2*time.Second, func() bool { return env.st.Get().CurrentVersion == "v1.1.0-alpha" }, "更新完成")

	// 同一 delivery 重投：200 no-op
	if rec := env.post(t, "release", "d-1", releaseBody("v1.1.0-alpha", true), true); rec.Code != http.StatusOK {
		t.Fatalf("重复投递 = %d, want 200", rec.Code)
	}
	// 不同 delivery 但同 tag（已是当前）：200 no-op
	if rec := env.post(t, "release", "d-2", releaseBody("v1.1.0-alpha", true), true); rec.Code != http.StatusOK {
		t.Fatalf("同 tag = %d, want 200", rec.Code)
	}
	// 旧 tag 重投：防降级 200 no-op
	if rec := env.post(t, "release", "d-3", releaseBody("v1.0.0-alpha", true), true); rec.Code != http.StatusOK {
		t.Fatalf("降级投递 = %d, want 200", rec.Code)
	}
	time.Sleep(50 * time.Millisecond)
	if calls := env.runner.snapshot(); len(calls) != 1 {
		t.Fatalf("去重/防降级后不应再更新: %v", calls)
	}
}

func TestWebhookConcurrentConflict(t *testing.T) {
	env := newWebhookEnv(t, func(c *Config) {
		c.Enabled = true
		c.AllowPrerelease = true
	})
	env.health.arm("v1.1.0-alpha")
	// 首个更新占住单飞锁：restart 阻塞
	block := make(chan struct{})
	env.runner.block = block

	if rec := env.post(t, "release", "d-c1", releaseBody("v1.1.0-alpha", true), true); rec.Code != http.StatusAccepted {
		t.Fatalf("首个 = %d", rec.Code)
	}
	waitFor(t, 2*time.Second, func() bool { return len(env.runner.snapshot()) > 0 }, "更新进入 restart")
	// 更新进行中并发投递 → 409
	if rec := env.post(t, "release", "d-c2", releaseBody("v1.1.0-alpha", true), true); rec.Code != http.StatusConflict {
		t.Fatalf("并发 = %d, want 409", rec.Code)
	}
	// /status 显示 updating
	req := httptest.NewRequest(http.MethodGet, "/status", nil)
	req.RemoteAddr = "127.0.0.1:1234"
	rec := httptest.NewRecorder()
	env.server.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"updating":true`) {
		t.Fatalf("status = %d %s", rec.Code, rec.Body.String())
	}
	close(block)
	waitFor(t, 2*time.Second, func() bool { return env.st.Get().CurrentVersion == "v1.1.0-alpha" }, "更新完成")
}

func TestWebhookExternalLockConflict(t *testing.T) {
	env := newWebhookEnv(t, func(c *Config) { c.AllowPrerelease = true })
	lock, err := AcquireLock(env.server.cfg.StateDir)
	if err != nil {
		t.Fatalf("预占锁失败: %v", err)
	}
	defer lock.Release()

	// 外部进程（--once/另一个实例）持锁：webhook 必须 409，且不得启动更新
	if rec := env.post(t, "release", "d-ext", releaseBody("v1.1.0-alpha", true), true); rec.Code != http.StatusConflict {
		t.Fatalf("外部锁 = %d, want 409", rec.Code)
	}
	time.Sleep(50 * time.Millisecond)
	if calls := env.runner.snapshot(); len(calls) != 0 {
		t.Fatalf("外部锁下不得更新: %v", calls)
	}
	if env.st.Get().LastResult != "" {
		t.Fatalf("锁拒绝不应写状态: %+v", env.st.Get())
	}

	// 锁释放后同一 delivery 应被正常受理（拒绝路径不记 delivery）
	lock.Release()
	env.health.arm("v1.1.0-alpha")
	if rec := env.post(t, "release", "d-ext", releaseBody("v1.1.0-alpha", true), true); rec.Code != http.StatusAccepted {
		t.Fatalf("释放后 = %d, want 202", rec.Code)
	}
	waitFor(t, 2*time.Second, func() bool { return env.st.Get().CurrentVersion == "v1.1.0-alpha" }, "更新完成")
}

func TestWebhookReleasesLockAfterUpdate(t *testing.T) {
	env := newWebhookEnv(t, func(c *Config) { c.AllowPrerelease = true })
	env.health.arm("v1.1.0-alpha")
	if rec := env.post(t, "release", "d-lockrel", releaseBody("v1.1.0-alpha", true), true); rec.Code != http.StatusAccepted {
		t.Fatalf("首次 = %d, want 202", rec.Code)
	}
	// 状态写入可能比 worker 的 defer 释锁早几微秒：轮询直到能再次取锁
	waitFor(t, 2*time.Second, func() bool {
		l, err := AcquireLock(env.server.cfg.StateDir)
		if err != nil {
			return false
		}
		l.Release()
		return true
	}, "更新结束后锁释放")
}

func TestStatusLoopbackOnly(t *testing.T) {
	env := newWebhookEnv(t, nil)
	req := httptest.NewRequest(http.MethodGet, "/status", nil)
	req.RemoteAddr = "203.0.113.9:5555"
	rec := httptest.NewRecorder()
	env.server.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("非环回 = %d, want 403", rec.Code)
	}

	req2 := httptest.NewRequest(http.MethodGet, "/status", nil)
	req2.RemoteAddr = "127.0.0.1:5555"
	rec2 := httptest.NewRecorder()
	env.server.Handler().ServeHTTP(rec2, req2)
	if rec2.Code != http.StatusOK {
		t.Fatalf("环回 = %d, want 200", rec2.Code)
	}
	body := rec2.Body.String()
	for _, want := range []string{`"enabled":true`, `"allow_prerelease":false`} {
		if !strings.Contains(body, want) {
			t.Fatalf("status 缺 %s: %s", want, body)
		}
	}
}

func TestHealthzEndpoint(t *testing.T) {
	env := newWebhookEnv(t, nil)
	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	rec := httptest.NewRecorder()
	env.server.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"status":"ok"`) {
		t.Fatalf("healthz = %d %s", rec.Code, rec.Body.String())
	}
}

func TestBodyLimit(t *testing.T) {
	env := newWebhookEnv(t, nil)
	big := make([]byte, MaxBodyBytes+10)
	for i := range big {
		big[i] = 'a'
	}
	req := httptest.NewRequest(http.MethodPost, "/hooks/github-release", bytes.NewReader(big))
	req.Header.Set("X-GitHub-Event", "release")
	req.Header.Set("X-Hub-Signature-256", "sha256="+hmacHex([]byte(testSecret), big))
	rec := httptest.NewRecorder()
	env.server.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("超大 body = %d, want 413", rec.Code)
	}
}

func TestNewServerRequiresSecret(t *testing.T) {
	st, err := OpenState(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	cfg := DefaultConfig()
	cfg.SecretFile = filepath.Join(t.TempDir(), "missing.secret")
	if _, err := NewServer(cfg, nil, st, testLogger()); err == nil {
		t.Fatal("secret 缺失应拒绝启动")
	}
	p := filepath.Join(t.TempDir(), "empty.secret")
	writeSecretFile(t, p, "  \n", 0o640)
	cfg.SecretFile = p
	if _, err := NewServer(cfg, nil, st, testLogger()); err == nil {
		t.Fatal("空 secret 应拒绝启动")
	}
	// 短 secret 与 JWT/TURN 同口径拒绝（16 字节 < 32）
	cfg.Secret = []byte("short-16-bytes!!")
	cfg.SecretFile = ""
	if _, err := NewServer(cfg, nil, st, testLogger()); err == nil {
		t.Fatal("短于 32 字节的 secret 应拒绝启动")
	}
	// 直接注入 ≥32 字节 secret（测试/嵌入场景）可启动
	cfg.Secret = []byte(testSecret)
	if _, err := NewServer(cfg, nil, st, testLogger()); err != nil {
		t.Fatalf("注入 secret 应可启动: %v", err)
	}
}

// TestNewServerSecretFilePermissions：secret 文件 other 位必须为 0（0640/0600）。
// Windows ACL 不映射到 Unix 权限位，该平台只断言可启动；Linux CI 覆盖拒绝路径。
func TestNewServerSecretFilePermissions(t *testing.T) {
	st, err := OpenState(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	content := strings.Repeat("a", 64) // 64 hex 字符 = 32 字节，长度达标

	wide := filepath.Join(dir, "wide.secret")
	writeSecretFile(t, wide, content, 0o644)
	cfg := DefaultConfig()
	cfg.SecretFile = wide
	_, err = NewServer(cfg, nil, st, testLogger())
	if runtime.GOOS == "windows" {
		if err != nil {
			t.Fatalf("Windows 不做权限断言，0644 应可启动: %v", err)
		}
	} else if err == nil {
		t.Fatal("0644 secret 文件（other 可读）应拒绝启动")
	}

	for _, mode := range []os.FileMode{0o640, 0o600} {
		p := filepath.Join(dir, "ok-"+mode.String()+".secret")
		writeSecretFile(t, p, content, mode)
		cfg.SecretFile = p
		if _, err := NewServer(cfg, nil, st, testLogger()); err != nil {
			t.Fatalf("权限 %04o 的 secret 文件应可启动: %v", mode, err)
		}
	}
}

// writeSecretFile 写 secret 文件并强制权限（os.WriteFile 的结果会被 umask 削位）。
func writeSecretFile(t *testing.T, path, content string, mode os.FileMode) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), mode); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, mode); err != nil {
		t.Fatal(err)
	}
}

// readSpy 记录 body 是否被读过：限流前置的关键断言（#102）。
type readSpy struct {
	r     io.Reader
	reads int
}

func (s *readSpy) Read(p []byte) (int, error) {
	s.reads++
	return s.r.Read(p)
}

// TestWebhookRateLimitBeforeBodyRead 已超限的 IP 在读 body 与算 HMAC 之前就被 429 拒掉，
// 且正常请求（未超限 IP / 正确签名）完全不受影响。
func TestWebhookRateLimitBeforeBodyRead(t *testing.T) {
	env := newWebhookEnv(t, nil)
	clk := newFakeClock()
	env.server.now = clk.Now
	body := releaseBody("v1.1.0-alpha", true)

	post := func(ip string, spy *readSpy, sign bool) int {
		req := httptest.NewRequest(http.MethodPost, "/hooks/github-release", nil)
		req.Body = io.NopCloser(spy)
		req.RemoteAddr = ip + ":4444"
		req.Header.Set("X-GitHub-Event", "release")
		req.Header.Set("X-GitHub-Delivery", "d-"+ip)
		if sign {
			req.Header.Set("X-Hub-Signature-256", "sha256="+hmacHex([]byte(testSecret), body))
		} else {
			req.Header.Set("X-Hub-Signature-256", "sha256="+strings.Repeat("00", 32))
		}
		rec := httptest.NewRecorder()
		env.server.Handler().ServeHTTP(rec, req)
		return rec.Code
	}

	const victim = "203.0.113.50"
	// 打满配额（前 invalidMaxPerIP 次 401，第 invalidMaxPerIP+1 次起 429）
	for i := 0; i <= invalidMaxPerIP; i++ {
		if code := post(victim, &readSpy{r: bytes.NewReader(body)}, false); code != http.StatusUnauthorized && code != http.StatusTooManyRequests {
			t.Fatalf("第 %d 次签名失败 = %d", i+1, code)
		}
	}

	// 超限后：body 不得被读取
	spy := &readSpy{r: bytes.NewReader(body)}
	if code := post(victim, spy, false); code != http.StatusTooManyRequests {
		t.Fatalf("超限 IP = %d，want 429", code)
	}
	if spy.reads != 0 {
		t.Fatalf("超限后仍读了 body %d 次（限流必须前置到读 body 之前）", spy.reads)
	}

	// 正确签名也照样被限流挡在前面（限流按 IP，不看签名对错）
	spyOK := &readSpy{r: bytes.NewReader(body)}
	if code := post(victim, spyOK, true); code != http.StatusTooManyRequests {
		t.Fatalf("超限 IP 的正确签名 = %d，want 429", code)
	}

	// 其他 IP 完全不受影响：正常读 body 并走到签名校验
	spyOther := &readSpy{r: bytes.NewReader(body)}
	if code := post("203.0.113.51", spyOther, true); code == http.StatusTooManyRequests {
		t.Fatal("其他 IP 不应被限流")
	}
	if spyOther.reads == 0 {
		t.Fatal("未超限 IP 应正常读取 body")
	}

	// 窗口过期后恢复
	clk.advance(invalidWindowSize)
	spyAfter := &readSpy{r: bytes.NewReader(body)}
	if code := post(victim, spyAfter, true); code == http.StatusTooManyRequests {
		t.Fatal("窗口过期后不应再限流")
	}
}

// TestWebhookInvalidSignatureRateLimit：同一 IP 1 分钟窗口内前 20 次 401、第 21 次 429；
// 其他 IP 不受影响，正确签名不受影响，窗口过期后恢复。
func TestWebhookInvalidSignatureRateLimit(t *testing.T) {
	env := newWebhookEnv(t, nil)
	clk := newFakeClock()
	env.server.now = clk.Now
	body := releaseBody("v1.1.0-alpha", true)

	bad := func(ip string) int {
		req := httptest.NewRequest(http.MethodPost, "/hooks/github-release", bytes.NewReader(body))
		req.RemoteAddr = ip + ":4444"
		req.Header.Set("X-GitHub-Event", "release")
		req.Header.Set("X-Hub-Signature-256", "sha256="+strings.Repeat("00", 32))
		rec := httptest.NewRecorder()
		env.server.Handler().ServeHTTP(rec, req)
		return rec.Code
	}

	for i := 1; i <= invalidMaxPerIP; i++ {
		if code := bad("203.0.113.7"); code != http.StatusUnauthorized {
			t.Fatalf("第 %d 次签名失败 = %d, want 401", i, code)
		}
	}
	if code := bad("203.0.113.7"); code != http.StatusTooManyRequests {
		t.Fatalf("第 %d 次签名失败 = %d, want 429", invalidMaxPerIP+1, code)
	}
	// 限流按 IP 隔离：另一个 IP 仍然 401
	if code := bad("203.0.113.8"); code != http.StatusUnauthorized {
		t.Fatalf("其他 IP 签名失败 = %d, want 401", code)
	}

	// #102：限流已前置到读 body 之前，所以超出配额的 IP 在该窗口内连正确签名也拿 429。
	// 这是有意的取舍：正常投递远达不到阈值，而放行正确签名会让攻击者用伪造的
	// 合法请求把预检变成可绕过的空转（body 与 HMAC 的计算开销照样付）。
	req := httptest.NewRequest(http.MethodPost, "/hooks/github-release", bytes.NewReader([]byte(`{}`)))
	req.RemoteAddr = "203.0.113.7:4444"
	req.Header.Set("X-GitHub-Event", "ping")
	req.Header.Set("X-GitHub-Delivery", "d-rl-ping")
	req.Header.Set("X-Hub-Signature-256", "sha256="+hmacHex([]byte(testSecret), []byte(`{}`)))
	rec := httptest.NewRecorder()
	env.server.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("超限 IP 上的正确签名 = %d, want 429", rec.Code)
	}

	// 窗口过后重新计数：正确签名恢复正常受理，同一 IP 的失败计数也重置
	clk.advance(invalidWindowSize)
	rec2 := httptest.NewRecorder()
	req2 := httptest.NewRequest(http.MethodPost, "/hooks/github-release", bytes.NewReader([]byte(`{}`)))
	req2.RemoteAddr = "203.0.113.7:4444"
	req2.Header.Set("X-GitHub-Event", "ping")
	req2.Header.Set("X-GitHub-Delivery", "d-rl-ping-2")
	req2.Header.Set("X-Hub-Signature-256", "sha256="+hmacHex([]byte(testSecret), []byte(`{}`)))
	env.server.Handler().ServeHTTP(rec2, req2)
	if rec2.Code != http.StatusAccepted {
		t.Fatalf("窗口过期后的正确签名 = %d, want 202", rec2.Code)
	}
	if code := bad("203.0.113.7"); code != http.StatusUnauthorized {
		t.Fatalf("窗口过期后 = %d, want 401", code)
	}
}
