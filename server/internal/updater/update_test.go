package updater

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// newTestConfig 测试默认配置：临时目录、假 API、放宽通道策略。
func newTestConfig(t *testing.T, ghURL, healthURL string) (Config, string) {
	t.Helper()
	dir := t.TempDir()
	cfg := DefaultConfig()
	cfg.GitHubAPI = ghURL
	cfg.HealthURL = healthURL
	cfg.InstallDir = filepath.Join(dir, "install")
	cfg.StateDir = filepath.Join(dir, "state")
	cfg.AllowPrerelease = true // 通道策略单独有测试；流水线测试默认放行
	cfg.HealthTimeout = 2 * time.Second
	if err := os.MkdirAll(cfg.InstallDir, 0o755); err != nil {
		t.Fatal(err)
	}
	return cfg, dir
}

func TestExecuteSuccessAtomicReplaceAndBackup(t *testing.T) {
	gh := newFakeGitHub(t)
	health := &fakeHealthz{ver: "v1.0.0-alpha"}
	cfg, _ := newTestConfig(t, gh.srv.URL, health.start(t))
	writeFile(t, filepath.Join(cfg.InstallDir, "litetalk-server"), "OLD-BINARY")

	wantSum := gh.publish(t, "v1.1.0-alpha", "NEW-BINARY", true)
	health.arm("v1.1.0-alpha")

	runner := &fakeRunner{onRun: func(name string, args []string) {
		health.onRestart()
	}}
	st, err := OpenState(cfg.StateDir)
	if err != nil {
		t.Fatal(err)
	}
	up := New(cfg, runner, st, testLogger(), nil)
	up.Now = time.Now
	up.FreeDisk = func(string) (uint64, error) { return 1 << 30, nil }

	res, err := up.RunOnce(context.Background(), "v1.1.0-alpha", "delivery-1")
	if err != nil {
		t.Fatalf("更新应成功: %v", err)
	}
	if res.Checksum != wantSum {
		t.Fatalf("校验和 %s != %s", res.Checksum, wantSum)
	}
	if res.OldVersion != "v1.0.0-alpha" {
		t.Fatalf("旧版本记录错误: %q", res.OldVersion)
	}
	if got := mustRead(t, filepath.Join(cfg.InstallDir, "litetalk-server")); got != "NEW-BINARY" {
		t.Fatalf("替换后内容错误: %q", got)
	}
	// 备份存在且内容是旧二进制
	baks, _ := filepath.Glob(filepath.Join(cfg.InstallDir, "litetalk-server.bak-*"))
	if len(baks) != 1 {
		t.Fatalf("备份数量 = %d, want 1", len(baks))
	}
	if got := mustRead(t, baks[0]); got != "OLD-BINARY" {
		t.Fatalf("备份内容错误: %q", got)
	}
	if !strings.Contains(filepath.Base(baks[0]), "v1.0.0-alpha") {
		t.Fatalf("备份名应含旧版本: %s", baks[0])
	}
	// 固定 exec 参数（不经 shell）
	calls := runner.snapshot()
	if len(calls) != 1 {
		t.Fatalf("重启调用次数 = %d, want 1: %v", len(calls), calls)
	}
	wantCall := []string{"sudo", "-n", "systemctl", "restart", "litetalk.service"}
	if strings.Join(calls[0], " ") != strings.Join(wantCall, " ") {
		t.Fatalf("exec 参数 = %v, want %v", calls[0], wantCall)
	}
	// 状态推进
	s := st.Get()
	if s.CurrentVersion != "v1.1.0-alpha" || s.LastResult != "success" || !s.HasDelivery("delivery-1") {
		t.Fatalf("状态错误: %+v", s)
	}
	// staging 残留清理
	if _, err := os.Stat(filepath.Join(cfg.InstallDir, "litetalk-server.new")); !os.IsNotExist(err) {
		t.Fatalf("staging 文件应不存在")
	}
}

func TestExecuteHealthzFailRollsBack(t *testing.T) {
	gh := newFakeGitHub(t)
	// healthz 始终报旧版本 → 断言失败 → 回滚
	health := &fakeHealthz{ver: "v1.0.0-alpha"}
	cfg, _ := newTestConfig(t, gh.srv.URL, health.start(t))
	cfg.HealthTimeout = 30 * time.Millisecond
	writeFile(t, filepath.Join(cfg.InstallDir, "litetalk-server"), "OLD-BINARY")
	gh.publish(t, "v1.1.0-alpha", "NEW-BINARY", true)

	runner := &fakeRunner{}
	st, _ := OpenState(cfg.StateDir)
	up := New(cfg, runner, st, testLogger(), nil)
	up.FreeDisk = func(string) (uint64, error) { return 1 << 30, nil }
	clk := newFakeClock()
	up.Now = clk.Now
	up.Sleep = func(d time.Duration) { clk.advance(d) }
	up.HealthPoll = time.Millisecond

	res, err := up.RunOnce(context.Background(), "v1.1.0-alpha", "delivery-2")
	if err == nil {
		t.Fatal("healthz 断言失败应返回错误")
	}
	if !res.RolledBack {
		t.Fatal("应标记 RolledBack")
	}
	if got := mustRead(t, filepath.Join(cfg.InstallDir, "litetalk-server")); got != "OLD-BINARY" {
		t.Fatalf("回滚后内容错误: %q", got)
	}
	// 两次 restart：更新一次 + 回滚一次
	if calls := runner.snapshot(); len(calls) != 2 {
		t.Fatalf("restart 次数 = %d, want 2: %v", len(calls), calls)
	}
	s := st.Get()
	if s.LastResult != "failed" || s.CurrentVersion != "" {
		t.Fatalf("失败状态错误: %+v", s)
	}
	if s.HasDelivery("delivery-2") {
		t.Fatal("失败不应记 delivery（留给重投重试）")
	}
}

func TestExecuteChecksumMismatchRejected(t *testing.T) {
	gh := newFakeGitHub(t)
	health := &fakeHealthz{ver: "v1.0.0-alpha"}
	cfg, _ := newTestConfig(t, gh.srv.URL, health.start(t))
	writeFile(t, filepath.Join(cfg.InstallDir, "litetalk-server"), "OLD-BINARY")

	// 篡改：登记资产后再改内容，校验和就不匹配
	sum := gh.publish(t, "v1.1.0-alpha", "NEW-BINARY", true)
	gh.mu.Lock()
	gh.blobs[assetName()] = buildTar(t, map[string]string{"litetalk-server": "EVIL-BINARY"})
	gh.mu.Unlock()
	_ = sum

	runner := &fakeRunner{}
	up := New(cfg, runner, nil, testLogger(), nil)
	up.FreeDisk = func(string) (uint64, error) { return 1 << 30, nil }

	_, err := up.Execute(context.Background(), "v1.1.0-alpha")
	if err == nil || !strings.Contains(err.Error(), "SHA256 校验失败") {
		t.Fatalf("应报 SHA256 校验失败，got %v", err)
	}
	if got := mustRead(t, filepath.Join(cfg.InstallDir, "litetalk-server")); got != "OLD-BINARY" {
		t.Fatalf("二进制不应被改动: %q", got)
	}
	if len(runner.snapshot()) != 0 {
		t.Fatal("不应触发重启")
	}
}

func TestExecuteAssetRetryUntilReady(t *testing.T) {
	gh := newFakeGitHub(t)
	health := &fakeHealthz{ver: "v1.0.0-alpha"}
	cfg, _ := newTestConfig(t, gh.srv.URL, health.start(t))

	// 前 2 次 404（资产晚到），第 3 次才成功
	gh.mu.Lock()
	gh.failCnt = 2
	gh.mu.Unlock()
	gh.publish(t, "v1.1.0-alpha", "NEW-BINARY", true)

	up := New(cfg, &fakeRunner{onRun: func(string, []string) { health.set("v1.1.0-alpha") }}, nil, testLogger(), nil)
	up.FreeDisk = func(string) (uint64, error) { return 1 << 30, nil }
	ghClient := up.GH()
	ghClient.RetryCount = 5
	ghClient.RetryDelay = time.Millisecond

	res, err := up.Execute(context.Background(), "v1.1.0-alpha")
	if err != nil {
		t.Fatalf("资产重试后应成功: %v", err)
	}
	if res.Checksum == "" {
		t.Fatal("应记录校验和")
	}
	if got := mustRead(t, filepath.Join(cfg.InstallDir, "litetalk-server")); got != "NEW-BINARY" {
		t.Fatalf("内容错误: %q", got)
	}
}

func TestExecutePrereleasePolicy(t *testing.T) {
	gh := newFakeGitHub(t)
	health := &fakeHealthz{ver: "v1.0.0-alpha"}
	cfg, _ := newTestConfig(t, gh.srv.URL, health.start(t))
	cfg.AllowPrerelease = false // 默认通道：只收正式版
	gh.publish(t, "v1.1.0-alpha", "NEW-BINARY", true)

	up := New(cfg, &fakeRunner{}, nil, testLogger(), nil)
	up.FreeDisk = func(string) (uint64, error) { return 1 << 30, nil }

	if _, err := up.Execute(context.Background(), "v1.1.0-alpha"); err == nil {
		t.Fatal("默认通道应拒绝预发布 tag")
	}
	// 同一 tag 显式放行后可通过
	cfg.AllowPrerelease = true
	up2 := New(cfg, &fakeRunner{onRun: func(string, []string) { health.set("v1.1.0-alpha") }}, nil, testLogger(), nil)
	up2.FreeDisk = func(string) (uint64, error) { return 1 << 30, nil }
	if _, err := up2.Execute(context.Background(), "v1.1.0-alpha"); err != nil {
		t.Fatalf("放行后应通过: %v", err)
	}
}

func TestBackupPruneKeepsThree(t *testing.T) {
	gh := newFakeGitHub(t)
	health := &fakeHealthz{ver: "v1.0.0-alpha"}
	cfg, _ := newTestConfig(t, gh.srv.URL, health.start(t))
	writeFile(t, filepath.Join(cfg.InstallDir, "litetalk-server"), "OLD-BINARY")
	// 预置 5 份旧备份（mtime 递增）
	for i := 0; i < 5; i++ {
		p := filepath.Join(cfg.InstallDir, fmt.Sprintf("litetalk-server.bak-OLD-%d", i))
		writeFile(t, p, "OLD-BAK")
		mt := time.Now().Add(time.Duration(i) * time.Minute)
		if err := os.Chtimes(p, mt, mt); err != nil {
			t.Fatal(err)
		}
	}
	gh.publish(t, "v1.1.0-alpha", "NEW-BINARY", true)

	up := New(cfg, &fakeRunner{onRun: func(string, []string) { health.set("v1.1.0-alpha") }}, nil, testLogger(), nil)
	up.FreeDisk = func(string) (uint64, error) { return 1 << 30, nil }
	if _, err := up.Execute(context.Background(), "v1.1.0-alpha"); err != nil {
		t.Fatal(err)
	}
	baks, _ := filepath.Glob(filepath.Join(cfg.InstallDir, "litetalk-server.bak-*"))
	if len(baks) != BackupKeep {
		t.Fatalf("备份保留 %d 份, want %d: %v", len(baks), BackupKeep, baks)
	}
}

func TestDryRunTouchesNothing(t *testing.T) {
	gh := newFakeGitHub(t)
	health := &fakeHealthz{ver: "v1.0.0-alpha"}
	cfg, _ := newTestConfig(t, gh.srv.URL, health.start(t))
	cfg.DryRun = true
	writeFile(t, filepath.Join(cfg.InstallDir, "litetalk-server"), "OLD-BINARY")
	gh.publish(t, "v1.1.0-alpha", "NEW-BINARY", true)

	st, _ := OpenState(cfg.StateDir)
	runner := &fakeRunner{}
	up := New(cfg, runner, st, testLogger(), nil)
	up.FreeDisk = func(string) (uint64, error) { return 1 << 30, nil }

	res, err := up.RunOnce(context.Background(), "v1.1.0-alpha", "delivery-3")
	if err != nil {
		t.Fatalf("dry-run 应成功: %v", err)
	}
	if !res.DryRun {
		t.Fatal("结果应标记 DryRun")
	}
	if got := mustRead(t, filepath.Join(cfg.InstallDir, "litetalk-server")); got != "OLD-BINARY" {
		t.Fatalf("dry-run 不得替换二进制: %q", got)
	}
	if len(runner.snapshot()) != 0 {
		t.Fatal("dry-run 不得重启")
	}
	if st.Get().LastResult != "" {
		t.Fatalf("dry-run 不得写状态: %+v", st.Get())
	}
}

func TestNoRestartDoesNotAdvanceVersion(t *testing.T) {
	gh := newFakeGitHub(t)
	health := &fakeHealthz{ver: "v1.0.0-alpha"}
	cfg, _ := newTestConfig(t, gh.srv.URL, health.start(t))
	cfg.NoRestart = true
	writeFile(t, filepath.Join(cfg.InstallDir, "litetalk-server"), "OLD-BINARY")
	gh.publish(t, "v1.1.0-alpha", "NEW-BINARY", true)

	st, _ := OpenState(cfg.StateDir)
	runner := &fakeRunner{}
	up := New(cfg, runner, st, testLogger(), nil)
	up.FreeDisk = func(string) (uint64, error) { return 1 << 30, nil }

	if _, err := up.RunOnce(context.Background(), "v1.1.0-alpha", ""); err != nil {
		t.Fatalf("--no-restart 应成功: %v", err)
	}
	if got := mustRead(t, filepath.Join(cfg.InstallDir, "litetalk-server")); got != "NEW-BINARY" {
		t.Fatalf("应已替换二进制: %q", got)
	}
	if len(runner.snapshot()) != 0 {
		t.Fatal("--no-restart 不得重启")
	}
	if st.Get().CurrentVersion != "" {
		t.Fatal("--no-restart 不得推进 CurrentVersion（服务里跑的仍是旧版本）")
	}
}

func TestInsufficientDiskFails(t *testing.T) {
	gh := newFakeGitHub(t)
	health := &fakeHealthz{ver: "v1.0.0-alpha"}
	cfg, _ := newTestConfig(t, gh.srv.URL, health.start(t))
	gh.publish(t, "v1.1.0-alpha", "NEW-BINARY", true)

	up := New(cfg, &fakeRunner{}, nil, testLogger(), nil)
	up.FreeDisk = func(string) (uint64, error) { return 10 << 20, nil } // 10MB < 200MB

	if _, err := up.Execute(context.Background(), "v1.1.0-alpha"); err == nil ||
		!strings.Contains(err.Error(), "磁盘可用空间不足") {
		t.Fatalf("应报空间不足，got %v", err)
	}
}

func TestOnceAllowsDowngradeExplicitly(t *testing.T) {
	gh := newFakeGitHub(t)
	health := &fakeHealthz{ver: "v2.0.0-alpha"}
	cfg, _ := newTestConfig(t, gh.srv.URL, health.start(t))
	writeFile(t, filepath.Join(cfg.InstallDir, "litetalk-server"), "OLD-BINARY")
	gh.publish(t, "v1.0.0-alpha", "OLD-AGAIN", true)

	// --once 是运维显式回退通道：不走 webhook 的防降级闸
	st, _ := OpenState(cfg.StateDir)
	_ = st.Update(func(s *State) { s.CurrentVersion = "v2.0.0-alpha" })
	up := New(cfg, &fakeRunner{onRun: func(string, []string) { health.set("v1.0.0-alpha") }}, st, testLogger(), nil)
	up.FreeDisk = func(string) (uint64, error) { return 1 << 30, nil }

	if _, err := up.RunOnce(context.Background(), "v1.0.0-alpha", ""); err != nil {
		t.Fatalf("显式 --once 降级应放行: %v", err)
	}
	if st.Get().CurrentVersion != "v1.0.0-alpha" {
		t.Fatalf("状态应推进到显式目标: %+v", st.Get())
	}
}

func TestOnceIgnoresEnabledFlag(t *testing.T) {
	gh := newFakeGitHub(t)
	health := &fakeHealthz{ver: "v1.0.0-alpha"}
	cfg, _ := newTestConfig(t, gh.srv.URL, health.start(t))
	cfg.Enabled = false // webhook 关闭；--once 是运维显式操作，不受此闸影响
	writeFile(t, filepath.Join(cfg.InstallDir, "litetalk-server"), "OLD-BINARY")
	gh.publish(t, "v1.1.0-alpha", "NEW-BINARY", true)

	up := New(cfg, &fakeRunner{onRun: func(string, []string) { health.set("v1.1.0-alpha") }}, nil, testLogger(), nil)
	up.FreeDisk = func(string) (uint64, error) { return 1 << 30, nil }

	if _, err := up.RunOnce(context.Background(), "v1.1.0-alpha", ""); err != nil {
		t.Fatalf("ENABLED=0 时 --once 仍应可用: %v", err)
	}
	if got := mustRead(t, filepath.Join(cfg.InstallDir, "litetalk-server")); got != "NEW-BINARY" {
		t.Fatalf("内容错误: %q", got)
	}
}

func TestLatestResolvesAndApplies(t *testing.T) {
	gh := newFakeGitHub(t)
	health := &fakeHealthz{ver: "v1.0.0-alpha"}
	cfg, _ := newTestConfig(t, gh.srv.URL, health.start(t))
	gh.publish(t, "v1.1.0-alpha", "NEW-BINARY", true)

	up := New(cfg, &fakeRunner{onRun: func(string, []string) { health.set("v1.1.0-alpha") }}, nil, testLogger(), nil)
	up.FreeDisk = func(string) (uint64, error) { return 1 << 30, nil }

	res, err := up.Execute(context.Background(), "latest")
	if err != nil {
		t.Fatalf("latest 应解析并更新: %v", err)
	}
	if res.Tag != "v1.1.0-alpha" {
		t.Fatalf("解析 tag = %q", res.Tag)
	}
}

// TestLatestPicksHighestAcrossList latest 走 fakeGitHub 的 releases 列表（map 遍历顺序随机），
// 必须取 semver 最高者而不是"列表第一条"。
func TestLatestPicksHighestAcrossList(t *testing.T) {
	gh := newFakeGitHub(t)
	health := &fakeHealthz{ver: "v1.0.0-alpha"}
	cfg, _ := newTestConfig(t, gh.srv.URL, health.start(t))
	for _, tag := range []string{"v1.2.0-alpha", "v1.9.0-alpha", "v1.10.0-alpha", "v1.3.1-alpha"} {
		gh.publish(t, tag, "BIN-"+tag, true)
	}
	up := New(cfg, &fakeRunner{onRun: func(string, []string) { health.set("v1.10.0-alpha") }}, nil, testLogger(), nil)
	up.FreeDisk = func(string) (uint64, error) { return 1 << 30, nil }

	// 多跑几轮：map 遍历顺序每轮不同，若实现依赖顺序会间歇性失败
	for i := 0; i < 8; i++ {
		res, err := up.Execute(context.Background(), "latest")
		if err != nil {
			t.Fatalf("第 %d 轮 latest 解析失败: %v", i, err)
		}
		if res.Tag != "v1.10.0-alpha" {
			t.Fatalf("第 %d 轮解析 tag = %q, want v1.10.0-alpha", i, res.Tag)
		}
		health.set("v1.0.0-alpha") // 复位，下一轮从头开始
	}
}

// TestOnceLatestDoesNotDowngrade 已装更高版本时 --once --tag latest 不降级：
// 空操作（Skipped），不下载不替换不重启，也不写失败状态。
func TestOnceLatestDoesNotDowngrade(t *testing.T) {
	gh := newFakeGitHub(t)
	health := &fakeHealthz{ver: "v2.0.0-alpha"}
	cfg, _ := newTestConfig(t, gh.srv.URL, health.start(t))
	writeFile(t, filepath.Join(cfg.InstallDir, "litetalk-server"), "CURRENT-BINARY")
	gh.publish(t, "v1.9.0-alpha", "OLDER-BINARY", true)

	st, _ := OpenState(cfg.StateDir)
	// 状态记录的已装版本高于 release 列表里的最高版本
	if err := st.Update(func(s *State) { s.CurrentVersion = "v3.0.0-alpha" }); err != nil {
		t.Fatal(err)
	}
	runner := &fakeRunner{}
	up := New(cfg, runner, st, testLogger(), nil)
	up.FreeDisk = func(string) (uint64, error) { return 1 << 30, nil }

	res, err := up.RunOnce(context.Background(), "latest", "")
	if err != nil {
		t.Fatalf("latest 不降级应是空操作而非失败: %v", err)
	}
	if !res.Skipped {
		t.Fatalf("应标记 Skipped: %+v", res)
	}
	if res.Tag != "v1.9.0-alpha" {
		t.Fatalf("应记录解析出的 tag: %q", res.Tag)
	}
	if got := mustRead(t, filepath.Join(cfg.InstallDir, "litetalk-server")); got != "CURRENT-BINARY" {
		t.Fatalf("不得替换二进制: %q", got)
	}
	if calls := runner.snapshot(); len(calls) != 0 {
		t.Fatalf("不得重启: %v", calls)
	}
	// 不写失败状态：LastResult 保持空
	s := st.Get()
	if s.CurrentVersion != "v3.0.0-alpha" {
		t.Fatalf("已装版本不得被改写: %+v", s)
	}
	if s.LastResult != "" {
		t.Fatalf("防降级是空操作，不应写状态: %+v", s)
	}
}

// TestOnceLatestAppliesWhenNewer 已装版本更低时 latest 正常更新（闸不误伤）。
func TestOnceLatestAppliesWhenNewer(t *testing.T) {
	gh := newFakeGitHub(t)
	health := &fakeHealthz{ver: "v1.9.0-alpha"}
	cfg, _ := newTestConfig(t, gh.srv.URL, health.start(t))
	writeFile(t, filepath.Join(cfg.InstallDir, "litetalk-server"), "OLD-BINARY")
	gh.publish(t, "v1.10.0-alpha", "NEW-BINARY", true)

	st, _ := OpenState(cfg.StateDir)
	if err := st.Update(func(s *State) { s.CurrentVersion = "v1.9.0-alpha" }); err != nil {
		t.Fatal(err)
	}
	up := New(cfg, &fakeRunner{onRun: func(string, []string) { health.set("v1.10.0-alpha") }}, st, testLogger(), nil)
	up.FreeDisk = func(string) (uint64, error) { return 1 << 30, nil }

	res, err := up.RunOnce(context.Background(), "latest", "")
	if err != nil {
		t.Fatalf("有更新的 latest 应正常更新: %v", err)
	}
	if res.Skipped {
		t.Fatalf("不应跳过: %+v", res)
	}
	if res.Tag != "v1.10.0-alpha" {
		t.Fatalf("解析 tag = %q", res.Tag)
	}
	if st.Get().CurrentVersion != "v1.10.0-alpha" {
		t.Fatalf("状态应推进: %+v", st.Get())
	}
}

// TestOnceSameVersionLatestIsNoop 同版本号的 latest 也是空操作（不算降级但无需重装）。
func TestOnceSameVersionLatestIsNoop(t *testing.T) {
	gh := newFakeGitHub(t)
	health := &fakeHealthz{ver: "v1.6.2-alpha"}
	cfg, _ := newTestConfig(t, gh.srv.URL, health.start(t))
	writeFile(t, filepath.Join(cfg.InstallDir, "litetalk-server"), "CURRENT-BINARY")
	gh.publish(t, "v1.6.2-alpha", "SAME-BINARY", true)

	st, _ := OpenState(cfg.StateDir)
	if err := st.Update(func(s *State) { s.CurrentVersion = "v1.6.2-alpha" }); err != nil {
		t.Fatal(err)
	}
	runner := &fakeRunner{}
	up := New(cfg, runner, st, testLogger(), nil)
	up.FreeDisk = func(string) (uint64, error) { return 1 << 30, nil }

	res, err := up.RunOnce(context.Background(), "latest", "")
	if err != nil {
		t.Fatalf("同版本 latest 应是空操作: %v", err)
	}
	if !res.Skipped {
		t.Fatalf("应标记 Skipped: %+v", res)
	}
	if calls := runner.snapshot(); len(calls) != 0 {
		t.Fatalf("不得重启: %v", calls)
	}
}
