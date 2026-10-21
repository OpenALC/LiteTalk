package updater

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"time"
)

// Result 单次更新结果元数据（日志与状态文件共用，不含 secret）。
type Result struct {
	Tag        string
	Checksum   string
	OldVersion string
	RolledBack bool
	Duration   time.Duration
	DryRun     bool
}

// Updater 更新流水线。依赖全部经接口/函数注入，测试不碰网络与 systemd。
type Updater struct {
	cfg    Config
	gh     *GitHubClient
	runner Runner
	st     *StateStore
	log    *log.Logger
	hc     *http.Client

	// 测试注入点
	Now         func() time.Time
	FreeDisk    func(path string) (uint64, error)
	HealthPoll  time.Duration
	Sleep       func(time.Duration)
	RestartHook func(ctx context.Context) error // 为 nil 时走 runner.RestartService
}

// New 构造更新器。state 可为 nil（--dry-run 之类不写状态的场景）。
func New(cfg Config, runner Runner, st *StateStore, logger *log.Logger, ghClient *http.Client) *Updater {
	if runner == nil {
		runner = ExecRunner{}
	}
	if ghClient == nil {
		ghClient = &http.Client{Timeout: 60 * time.Second}
	}
	return &Updater{
		cfg:        cfg,
		runner:     runner,
		st:         st,
		log:        logger,
		hc:         ghClient,
		Now:        time.Now,
		FreeDisk:   freeDiskBytes,
		HealthPoll: time.Second,
		Sleep:      time.Sleep,
		gh: &GitHubClient{
			APIBase: cfg.GitHubAPI,
			Repo:    cfg.Repo,
			Client:  ghClient,
			Log:     logger,
		},
	}
}

// GH 暴露 GitHub 客户端供 --once 的 latest 解析复用。
func (u *Updater) GH() *GitHubClient { return u.gh }

func (u *Updater) now() time.Time {
	if u.Now != nil {
		return u.Now()
	}
	return time.Now()
}

func (u *Updater) logf(format string, args ...any) {
	if u.log != nil {
		u.log.Printf(format, args...)
	}
}

// RunOnce 执行一次完整更新并记录状态（webhook 后台 worker 与 --once 共用）。
// dry-run 不写状态；tag 为 latest 时先解析成具体 tag。
func (u *Updater) RunOnce(ctx context.Context, tag, delivery string) (Result, error) {
	start := u.now()
	res, err := u.Execute(ctx, tag)
	res.Duration = u.now().Sub(start)
	if u.st == nil || u.cfg.DryRun {
		return res, err
	}
	if err != nil {
		if recErr := u.st.RecordFailure(res.Tag, delivery, err, res.Duration); recErr != nil {
			u.logf("写失败状态出错: %v", recErr)
		}
		return res, err
	}
	if u.cfg.NoRestart {
		// 未重启即未生效：只记备份/替换完成，不推进 CurrentVersion（healthz 仍是旧版本）。
		return res, nil
	}
	if recErr := u.st.RecordSuccess(res.Tag, delivery, res.Checksum, res.Duration); recErr != nil {
		u.logf("写成功状态出错: %v", recErr)
	}
	return res, nil
}

// Execute 更新流水线：解析 tag → 磁盘检查 → 查资产 → 下载 → SHA256 → 安全解包
// → 原子替换 → 重启 → healthz 断言 → 失败回滚。
// dry-run 只走到解包校验，不改安装目录、不重启、不写状态。
func (u *Updater) Execute(ctx context.Context, tag string) (Result, error) {
	res := Result{Tag: tag, DryRun: u.cfg.DryRun}

	if tag == "latest" {
		t, err := u.gh.ResolveLatest(ctx, u.cfg.AllowPrerelease)
		if err != nil {
			return res, fmt.Errorf("解析 latest: %w", err)
		}
		tag = t
		res.Tag = tag
		u.logf("latest 解析为 %s", tag)
	}
	if _, ok := parseTag(tag); !ok {
		return res, fmt.Errorf("tag 不符合白名单形态: %q", tag)
	}
	// 通道闸再走一遍：--once 也必须显式开预发布才装 alpha/beta/rc
	if isPrereleaseTag(tag) && !u.cfg.AllowPrerelease {
		return res, fmt.Errorf("通道策略拒绝预发布 tag %q（ALLOW_PRERELEASE=0）", tag)
	}

	installDir := u.cfg.InstallDir
	if err := u.checkDisk(installDir); err != nil {
		return res, err
	}
	res.OldVersion = u.currentVersion()

	asset := u.cfg.AssetFileName(runtime.GOARCH)
	const sumsName = "SHA256SUMS.txt"
	rel, err := u.gh.FetchRelease(ctx, tag, asset, sumsName)
	if err != nil {
		return res, err
	}
	if rel.Prerelease && !u.cfg.AllowPrerelease {
		return res, fmt.Errorf("通道策略拒绝预发布 release %q", tag)
	}
	tarURL := assetURL(rel, asset)
	sumsURL := assetURL(rel, sumsName)
	if tarURL == "" || sumsURL == "" {
		return res, fmt.Errorf("release 缺少 %s / %s", asset, sumsName)
	}

	tmpDir, err := os.MkdirTemp("", "litetalk-updater-")
	if err != nil {
		return res, err
	}
	defer os.RemoveAll(tmpDir)

	u.logf("下载 tag=%s 资产=%s", tag, asset)
	gotSum, err := u.gh.Download(ctx, tarURL, filepath.Join(tmpDir, asset))
	if err != nil {
		return res, err
	}
	sumsPath := filepath.Join(tmpDir, sumsName)
	if _, err := u.gh.Download(ctx, sumsURL, sumsPath); err != nil {
		return res, err
	}
	sums, err := os.ReadFile(sumsPath)
	if err != nil {
		return res, err
	}
	wantSum, err := MatchSHA256(sums, asset)
	if err != nil {
		return res, err
	}
	// 校验和比对用常量时间比较（与 HMAC 同一习惯：不靠 == 短路）
	a, _ := hex.DecodeString(gotSum)
	b, _ := hex.DecodeString(wantSum)
	if !equalHex(a, b) {
		return res, fmt.Errorf("SHA256 校验失败：下载 %s，清单 %s", gotSum, wantSum)
	}
	res.Checksum = gotSum
	u.logf("SHA256 校验通过 tag=%s sum=%s", tag, gotSum)

	extracted, err := safeExtract(filepath.Join(tmpDir, asset), filepath.Join(tmpDir, "x"))
	if err != nil {
		return res, fmt.Errorf("解包失败: %w", err)
	}
	if u.cfg.DryRun {
		u.logf("dry-run 完成 tag=%s（未替换、未重启）", tag)
		return res, nil
	}

	return u.install(ctx, res, extracted["litetalk-server"], installDir, tag)
}

// install 落盘替换 + 重启 + healthz 断言；失败回滚。
func (u *Updater) install(ctx context.Context, res Result, newBin, installDir, tag string) (Result, error) {
	if err := u.checkDisk(installDir); err != nil {
		return res, err
	}
	final := filepath.Join(installDir, "litetalk-server")
	staging := filepath.Join(installDir, "litetalk-server.new")

	// 同目录 staging：rename 原子替换要求同一文件系统
	if err := copyFile(newBin, staging, 0o755); err != nil {
		os.Remove(staging)
		return res, fmt.Errorf("暂存新二进制: %w", err)
	}

	backup := ""
	if _, err := os.Stat(final); err == nil {
		backup = filepath.Join(installDir, fmt.Sprintf("litetalk-server.bak-%s-%s",
			sanitizeVer(res.OldVersion), u.now().Format("20060102-150405")))
		if err := copyFile(final, backup, 0o755); err != nil {
			os.Remove(staging)
			return res, fmt.Errorf("备份旧二进制: %w", err)
		}
	}
	pruneBackups(u.logf, installDir)

	if err := os.Rename(staging, final); err != nil {
		os.Remove(staging)
		return res, fmt.Errorf("原子替换失败: %w", err)
	}
	fsyncDir(installDir)
	u.logf("已替换二进制 tag=%s 旧版本=%s 备份=%s", tag, res.OldVersion, filepath.Base(backup))

	if u.cfg.NoRestart {
		u.logf("--no-restart：跳过重启与 healthz 断言 tag=%s", tag)
		return res, nil
	}

	if err := u.restart(ctx); err != nil {
		return u.rollback(ctx, res, final, backup, installDir, tag, fmt.Errorf("重启失败: %w", err))
	}
	if err := u.waitHealthy(ctx, tag); err != nil {
		return u.rollback(ctx, res, final, backup, installDir, tag, err)
	}
	u.logf("更新成功 tag=%s 旧版本=%s", tag, res.OldVersion)
	return res, nil
}

// rollback 恢复备份并再重启，返回带 RolledBack 标记的结果。
// 回滚失败只记录：原错误已返回，人工介入路径见 README。
func (u *Updater) rollback(ctx context.Context, res Result, final, backup, installDir, tag string, cause error) (Result, error) {
	res.RolledBack = true
	u.logf("更新失败 tag=%s，回滚到 %s: %v", tag, res.OldVersion, cause)
	if backup == "" {
		u.logf("无备份可回滚（首次安装？）tag=%s", tag)
		return res, cause
	}
	if err := os.Rename(backup, final); err != nil {
		u.logf("回滚恢复失败（需人工处理）tag=%s: %v", tag, err)
		return res, fmt.Errorf("%w（且恢复备份失败: %v）", cause, err)
	}
	fsyncDir(installDir)
	if err := u.restart(ctx); err != nil {
		u.logf("回滚后重启失败（需人工处理）tag=%s: %v", tag, err)
		return res, fmt.Errorf("%w（且重启失败: %v）", cause, err)
	}
	if err := u.waitHealthy(ctx, res.OldVersion); err != nil {
		u.logf("回滚后健康检查未恢复旧版本 %s: %v", res.OldVersion, err)
		return res, fmt.Errorf("%w（回滚后健康检查: %v）", cause, err)
	}
	u.logf("回滚完成 tag=%s 恢复版本=%s", tag, res.OldVersion)
	return res, cause
}

func (u *Updater) restart(ctx context.Context) error {
	if u.RestartHook != nil {
		return u.RestartHook(ctx)
	}
	// 服务名二次校验后才进 exec 参数：固定 sudo -n systemctl restart，绝不经 shell
	if !serviceRe.MatchString(u.cfg.ServiceName) {
		return fmt.Errorf("服务名单元非法: %q", u.cfg.ServiceName)
	}
	return u.runner.Run(ctx, "sudo", "-n", "systemctl", "restart", u.cfg.ServiceName)
}

// waitHealthy 轮询 healthz 直到 version == tag（或超时）。fail-closed：版本不符视为失败。
func (u *Updater) waitHealthy(ctx context.Context, tag string) error {
	deadline := u.now().Add(u.cfg.HealthTimeout)
	var lastVer, lastErr string
	for {
		ver, err := u.healthzVersion(ctx)
		switch {
		case err == nil && ver == tag:
			return nil
		case err != nil:
			lastErr = err.Error()
		default:
			lastVer = ver
			lastErr = ""
		}
		if !u.now().Before(deadline) {
			return fmt.Errorf("healthz 未在 %s 内报告版本 %q（最后版本=%q 错误=%s）",
				u.cfg.HealthTimeout, tag, lastVer, lastErr)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
			u.Sleep(u.HealthPoll)
		}
	}
}

func (u *Updater) healthzVersion(ctx context.Context) (string, error) {
	reqCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, u.cfg.HealthURL, nil)
	if err != nil {
		return "", err
	}
	resp, err := u.hc.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("healthz HTTP %d", resp.StatusCode)
	}
	var body struct {
		Status  string `json:"status"`
		Version string `json:"version"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 4096)).Decode(&body); err != nil {
		return "", fmt.Errorf("解析 healthz: %w", err)
	}
	if body.Status != "ok" {
		return "", fmt.Errorf("healthz status=%q", body.Status)
	}
	return body.Version, nil
}

// currentVersion 替换前探一下旧版本（备份命名与日志用）；失败不阻断更新。
func (u *Updater) currentVersion() string {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	ver, err := u.healthzVersion(ctx)
	if err != nil || ver == "" {
		return "unknown"
	}
	return ver
}

func (u *Updater) checkDisk(path string) error {
	if _, err := os.Stat(path); err != nil {
		// 安装目录还没建也算过：首次安装路径
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("访问安装目录: %w", err)
	}
	free, err := u.FreeDisk(path)
	if err != nil {
		return fmt.Errorf("磁盘空间检查失败: %w", err)
	}
	if free < MinFreeBytes {
		return fmt.Errorf("磁盘可用空间不足（需 ≥200MB，可用 %dMB）", free>>20)
	}
	return nil
}

// copyFile 复制并 fsync；目标 0755（可执行）。src 直接 rename 进目标目录才行，
// 跨文件系统时（下载在 /tmp）必须复制。
func copyFile(src, dst string, mode os.FileMode) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, mode)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	if err := out.Sync(); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}

// fsyncDir 目录 fsync：确保 rename 本身落盘（掉电不丢替换结果）。
func fsyncDir(dir string) {
	d, err := os.Open(dir)
	if err != nil {
		return
	}
	defer d.Close()
	_ = d.Sync()
}

// pruneBackups 只留最近 BackupKeep 份备份。按 mtime 排序（备份名里带版本号，
// 版本段不同时按名字排序会得到错误顺序）。
func pruneBackups(logf func(string, ...any), dir string) {
	matches, err := filepath.Glob(filepath.Join(dir, "litetalk-server.bak-*"))
	if err != nil {
		return
	}
	type item struct {
		path string
		mt   time.Time
	}
	items := make([]item, 0, len(matches))
	for _, m := range matches {
		fi, err := os.Stat(m)
		if err != nil {
			continue
		}
		items = append(items, item{m, fi.ModTime()})
	}
	sort.Slice(items, func(i, j int) bool { return items[i].mt.After(items[j].mt) })
	for i, it := range items {
		if i < BackupKeep {
			continue
		}
		if err := os.Remove(it.path); err != nil {
			logf("清理旧备份 %s 失败: %v", filepath.Base(it.path), err)
		}
	}
}

// sanitizeVer 版本串进文件名前清洗（只保留安全字符）。
func sanitizeVer(v string) string {
	if v == "" {
		return "unknown"
	}
	var b strings.Builder
	for _, r := range v {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '.', r == '-', r == '_':
			b.WriteRune(r)
		default:
			b.WriteByte('_')
		}
	}
	return b.String()
}

// equalHex 常量时间比较两个等长字节串。
func equalHex(a, b []byte) bool {
	if len(a) != len(b) {
		return false
	}
	var v byte
	for i := range a {
		v |= a[i] ^ b[i]
	}
	return v == 0
}
