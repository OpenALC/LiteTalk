package updater

// 测试脚手架：假 runner / 假时钟 / 构造 tar.gz / 假 GitHub API。
// 全部离线可跑，不依赖 root、systemd 与外网。

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

// testLogger 静默日志：测试只关心返回值与副作用，不看输出。
func testLogger() *log.Logger { return log.New(io.Discard, "", 0) }

type fakeRunner struct {
	mu    sync.Mutex
	calls [][]string
	onRun func(name string, args []string)
	err   error
	block chan struct{} // 非 nil 时 Run 阻塞直到 close
}

func (f *fakeRunner) Run(_ context.Context, name string, args ...string) error {
	call := append([]string{name}, args...)
	f.mu.Lock()
	f.calls = append(f.calls, call)
	fn := f.onRun
	err := f.err
	block := f.block
	f.mu.Unlock()
	if fn != nil {
		fn(name, args)
	}
	if block != nil {
		<-block
	}
	return err
}

func (f *fakeRunner) snapshot() [][]string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([][]string(nil), f.calls...)
}

// fakeClock 单调可控时钟，让 healthz 超时路径不真实等待。
type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func newFakeClock() *fakeClock {
	return &fakeClock{t: time.Date(2026, 10, 21, 12, 0, 0, 0, time.UTC)}
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fakeClock) advance(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

// buildTar 构造只含给定文件的 tar.gz（name->内容）。
func buildTar(t *testing.T, files map[string]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for name, content := range files {
		hdr := &tar.Header{
			Name:     name,
			Mode:     0o755,
			Size:     int64(len(content)),
			Typeflag: tar.TypeReg,
		}
		if err := tw.WriteHeader(hdr); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write([]byte(content)); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

type tarEntry struct {
	header tar.Header
	body   []byte
}

// buildTarRaw 允许自定义 header（构造符号链接/穿越等恶意包）。
func buildTarRaw(t *testing.T, entries []tarEntry) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for _, e := range entries {
		hdr := e.header
		if err := tw.WriteHeader(&hdr); err != nil {
			t.Fatal(err)
		}
		if e.body != nil {
			if _, err := tw.Write(e.body); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func sha256Hex(b []byte) string {
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

func hmacHex(secret, body []byte) string {
	mac := hmac.New(sha256.New, secret)
	mac.Write(body)
	return hex.EncodeToString(mac.Sum(nil))
}

// fakeGitHub 假 release API + 资产下载。资产缺失/延迟/404 可编排。
type fakeGitHub struct {
	srv      *httptest.Server
	mu       sync.Mutex
	releases map[string]ghRelease // tag -> release
	blobs    map[string][]byte    // asset name -> 内容
	failCnt  int                  // 前 N 次 tag 查询返回 404
	hits     int
}

func newFakeGitHub(t *testing.T) *fakeGitHub {
	t.Helper()
	g := &fakeGitHub{
		releases: map[string]ghRelease{},
		blobs:    map[string][]byte{},
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/repos/", g.handle)
	mux.HandleFunc("/assets/", g.handle)
	g.srv = httptest.NewServer(mux)
	t.Cleanup(g.srv.Close)
	return g
}

func (g *fakeGitHub) handle(w http.ResponseWriter, r *http.Request) {
	g.mu.Lock()
	g.hits++
	fail := g.failCnt > 0
	if fail {
		g.failCnt--
	}
	g.mu.Unlock()
	if fail {
		http.NotFound(w, r)
		return
	}
	p := r.URL.Path
	if i := strings.Index(p, "/releases/tags/"); i >= 0 {
		tag := p[i+len("/releases/tags/"):]
		g.mu.Lock()
		rel, ok := g.releases[tag]
		g.mu.Unlock()
		if !ok {
			http.NotFound(w, r)
			return
		}
		writeJSON(w, http.StatusOK, rel)
		return
	}
	if strings.HasSuffix(p, "/releases") {
		g.mu.Lock()
		list := make([]ghRelease, 0, len(g.releases))
		for _, rel := range g.releases {
			list = append(list, rel)
		}
		g.mu.Unlock()
		writeJSON(w, http.StatusOK, list)
		return
	}
	name := filepath.Base(p)
	g.mu.Lock()
	blob, ok := g.blobs[name]
	g.mu.Unlock()
	if !ok {
		http.NotFound(w, r)
		return
	}
	_, _ = w.Write(blob)
}

func assetName() string {
	return "litetalk-server-linux-" + runtime.GOARCH + ".tar.gz"
}

// publish 登记一个 release 及其资产（返回 tar 校验和）。
func (g *fakeGitHub) publish(t *testing.T, tag, binary string, prerelease bool) string {
	t.Helper()
	tarBlob := buildTar(t, map[string]string{"litetalk-server": binary})
	sum := sha256Hex(tarBlob)
	sums := []byte(sum + "  " + assetName() + "\n")

	g.mu.Lock()
	defer g.mu.Unlock()
	g.blobs[assetName()] = tarBlob
	g.blobs["SHA256SUMS.txt"] = sums
	rel := ghRelease{TagName: tag, Prerelease: prerelease}
	rel.Assets = []ghAsset{
		{Name: assetName(), Size: int64(len(tarBlob)), BrowserDownloadURL: g.srv.URL + "/assets/" + assetName()},
		{Name: "SHA256SUMS.txt", Size: int64(len(sums)), BrowserDownloadURL: g.srv.URL + "/assets/SHA256SUMS.txt"},
	}
	g.releases[tag] = rel
	return sum
}

// fakeHealthz 可变版本号的 healthz 假服务。arm 设定"下次重启后回报的版本"。
type fakeHealthz struct {
	mu   sync.Mutex
	ver  string
	next string // restart 后切换到的版本
}

func (h *fakeHealthz) start(t *testing.T) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok", "version": h.get()})
	}))
	t.Cleanup(srv.Close)
	return srv.URL + "/api/v1/healthz"
}

func (h *fakeHealthz) get() string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.ver
}

func (h *fakeHealthz) set(v string) {
	h.mu.Lock()
	h.ver = v
	h.mu.Unlock()
}

// arm 注册"重启后"应报告的版本（假 restart 调用切换）。
func (h *fakeHealthz) arm(next string) {
	h.mu.Lock()
	h.next = next
	h.mu.Unlock()
}

func (h *fakeHealthz) onRestart() {
	h.mu.Lock()
	if h.next != "" {
		h.ver = h.next
	}
	h.mu.Unlock()
}

// waitFor 轮询直到 cond 成立（后台 worker 是异步的）。
func waitFor(t *testing.T, timeout time.Duration, cond func() bool, what string) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("等待超时：%s", what)
}

func mustRead(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o755); err != nil {
		t.Fatal(err)
	}
}
