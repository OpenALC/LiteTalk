package updater

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"strings"
	"time"
)

// 资产可能晚于 release 事件可见（action-gh-release 先建 release 再传资产）。
const (
	assetRetryCount = 12
	assetRetryDelay = 5 * time.Second
	downloadMaxSize = 512 << 20
)

type ghAsset struct {
	Name               string `json:"name"`
	Size               int64  `json:"size"`
	BrowserDownloadURL string `json:"browser_download_url"`
}

type ghRelease struct {
	TagName    string    `json:"tag_name"`
	Prerelease bool      `json:"prerelease"`
	Assets     []ghAsset `json:"assets"`
}

// GitHubClient 只读调 GitHub REST API（标准库 HTTPS，不装 gh CLI）。
type GitHubClient struct {
	APIBase string
	Repo    string
	Client  *http.Client // API 查询（短超时）
	Log     *log.Logger

	// 测试注入点
	RetryCount int
	RetryDelay time.Duration
	Now        func() time.Time
}

func (g *GitHubClient) http() *http.Client {
	if g.Client != nil {
		return g.Client
	}
	return http.DefaultClient
}

// downloadClient 资产下载用独立客户端：API 的 60s 整体超时会把慢链路上的大文件掐断，
// 下载放宽到 10 分钟（外层仍有 ctx 可取消）。
func (g *GitHubClient) downloadClient() *http.Client {
	return &http.Client{Timeout: 10 * time.Minute}
}

func (g *GitHubClient) now() time.Time {
	if g.Now != nil {
		return g.Now()
	}
	return time.Now()
}

func (g *GitHubClient) retryCount() int {
	if g.RetryCount > 0 {
		return g.RetryCount
	}
	return assetRetryCount
}

func (g *GitHubClient) retryDelay() time.Duration {
	if g.RetryDelay > 0 {
		return g.RetryDelay
	}
	return assetRetryDelay
}

func (g *GitHubClient) logf(format string, args ...any) {
	if g.Log != nil {
		g.Log.Printf(format, args...)
	}
}

// FetchRelease 按 tag 查 release，直到所需资产就绪（12×5s）。
// 404/资产缺失都重试：webhook 可能早于资产上传完成。
func (g *GitHubClient) FetchRelease(ctx context.Context, tag, wantAsset, wantSums string) (*ghRelease, error) {
	u := fmt.Sprintf("%s/repos/%s/releases/tags/%s", g.APIBase, g.Repo, tag)
	var lastErr error
	for i := 0; i < g.retryCount(); i++ {
		if i > 0 {
			g.logf("等待资产就绪（%d/%d）tag=%s", i, g.retryCount(), tag)
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(g.retryDelay()):
			}
		}
		rel, err := g.fetchOnce(ctx, u)
		if err != nil {
			lastErr = err
			continue
		}
		if rel.TagName != tag {
			lastErr = fmt.Errorf("API 返回 tag %q 与目标 %q 不符", rel.TagName, tag)
			continue
		}
		if assetURL(rel, wantAsset) != "" && assetURL(rel, wantSums) != "" {
			return rel, nil
		}
		lastErr = fmt.Errorf("release %s 资产未就绪（缺 %s / %s）", tag, wantAsset, wantSums)
	}
	return nil, fmt.Errorf("查询 release 资产失败（重试 %d 次）: %w", g.retryCount(), lastErr)
}

func (g *GitHubClient) fetchOnce(ctx context.Context, u string) (*ghRelease, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	resp, err := g.http().Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		// 只读状态码，不倾倒响应体（可能含 API 细节）
		return nil, fmt.Errorf("GitHub API %s: HTTP %d", u, resp.StatusCode)
	}
	var rel ghRelease
	if err := json.NewDecoder(io.LimitReader(resp.Body, 4<<20)).Decode(&rel); err != nil {
		return nil, fmt.Errorf("解析 release JSON: %w", err)
	}
	return &rel, nil
}

// ResolveLatest 取最新发布 tag（含预发布；releases/latest 接口不返回预发布，改用列表）。
// 按通道策略过滤：未开预发布时跳过带后缀的 tag。
func (g *GitHubClient) ResolveLatest(ctx context.Context, allowPrerelease bool) (string, error) {
	u := fmt.Sprintf("%s/repos/%s/releases?per_page=20", g.APIBase, g.Repo)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	resp, err := g.http().Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("GitHub API %s: HTTP %d", u, resp.StatusCode)
	}
	var rels []ghRelease
	if err := json.NewDecoder(io.LimitReader(resp.Body, 8<<20)).Decode(&rels); err != nil {
		return "", fmt.Errorf("解析 releases JSON: %w", err)
	}
	for _, r := range rels {
		if _, ok := parseTag(r.TagName); !ok {
			continue
		}
		if (r.Prerelease || isPrereleaseTag(r.TagName)) && !allowPrerelease {
			continue
		}
		return r.TagName, nil
	}
	return "", fmt.Errorf("没有符合通道策略的 release")
}

// Download 下载资产到 destPath，边下边算 SHA256。要求 HTTP 200 + 响应体不超限。
func (g *GitHubClient) Download(ctx context.Context, url, destPath string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Accept", "application/octet-stream")
	resp, err := g.downloadClient().Do(req)
	if err != nil {
		return "", fmt.Errorf("下载 %s: %w", url, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("下载 %s: HTTP %d", url, resp.StatusCode)
	}
	f, err := os.OpenFile(destPath, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return "", err
	}
	h := sha256.New()
	n, err := io.Copy(io.MultiWriter(f, h), io.LimitReader(resp.Body, downloadMaxSize+1))
	if err != nil {
		f.Close()
		os.Remove(destPath)
		return "", fmt.Errorf("写 %s: %w", destPath, err)
	}
	if err := f.Sync(); err != nil {
		f.Close()
		os.Remove(destPath)
		return "", err
	}
	if err := f.Close(); err != nil {
		os.Remove(destPath)
		return "", err
	}
	if n > downloadMaxSize {
		os.Remove(destPath)
		return "", fmt.Errorf("下载体积超限（>%d 字节）", downloadMaxSize)
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// assetURL 找到资产下载地址；不存在返回空串。
func assetURL(rel *ghRelease, name string) string {
	for _, a := range rel.Assets {
		if a.Name == name {
			return a.BrowserDownloadURL
		}
	}
	return ""
}

// MatchSHA256 在 SHA256SUMS.txt 里找 asset 的校验行。
// 只接受与资产名完全相同的行（防把别的文件的校验和蒙混过关），重复行报错。
func MatchSHA256(sums []byte, asset string) (string, error) {
	var found string
	for _, line := range strings.Split(string(sums), "\n") {
		line = strings.TrimRight(line, "\r")
		if line == "" {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) != 2 {
			continue
		}
		name := strings.TrimPrefix(fields[1], "*")
		if name != asset {
			continue
		}
		sum := strings.ToLower(fields[0])
		if len(sum) != sha256.Size*2 {
			return "", fmt.Errorf("SHA256SUMS 行哈希长度非法")
		}
		if _, err := hex.DecodeString(sum); err != nil {
			return "", fmt.Errorf("SHA256SUMS 行哈希非法")
		}
		if found != "" && found != sum {
			return "", fmt.Errorf("SHA256SUMS 中 %s 有多个不一致校验和", asset)
		}
		found = sum
	}
	if found == "" {
		return "", fmt.Errorf("SHA256SUMS 缺少 %s 的校验行", asset)
	}
	return found, nil
}
