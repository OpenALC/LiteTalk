// 轻音 LiteTalk 服务端自动更新器入口（#97）。
// 服务模式：8088 明文 HTTP 收 GitHub release webhook（公网仅此入口；/status 仅 loopback）。
// 443 与证书签发归主服务，本进程不做 TLS。
// --once 模式：手工/SSH 兜底执行同一条更新流水线后退出（update.sh 转发到这里）。
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/OpenALC/LiteTalk/server/internal/updater"
)

// version 构建时 -X main.version= 注入，默认 dev
var version = "dev"

func main() {
	logger := log.New(os.Stderr, "litetalk-updater ", log.LstdFlags)

	cfg := updater.DefaultConfig()
	if err := cfg.LoadEnv(os.Getenv); err != nil {
		logger.Printf("环境变量非法: %v", err)
		os.Exit(2)
	}

	fs := flag.NewFlagSet("litetalk-updater", flag.ContinueOnError)
	once := fs.Bool("once", false, "执行一次更新后退出（update.sh / SSH 兜底）")
	tag := fs.String("tag", "", "目标 tag（--once 必填，可为 latest）")
	dryRun := fs.Bool("dry-run", false, "只下载校验解包，不替换二进制、不重启、不写状态")
	noRestart := fs.Bool("no-restart", false, "替换二进制但不重启服务（不推进已发布版本）")
	showVersion := fs.Bool("version", false, "打印版本号后退出")
	fs.BoolVar(&cfg.Enabled, "enabled", cfg.Enabled, "0 时 webhook 只校验记录不执行更新（--once 不受影响）")
	fs.BoolVar(&cfg.AllowPrerelease, "allow-prerelease", cfg.AllowPrerelease, "接受 -alpha/-beta/-rc 预发布 tag")
	fs.StringVar(&cfg.Listen, "listen", cfg.Listen, "明文 HTTP 监听地址")
	fs.StringVar(&cfg.SecretFile, "secret-file", cfg.SecretFile, "webhook secret 文件路径")
	fs.StringVar(&cfg.Repo, "repo", cfg.Repo, "仓库白名单 owner/name")
	fs.StringVar(&cfg.AssetName, "asset", cfg.AssetName, "资产名覆盖（默认 litetalk-server-linux-<arch>.tar.gz）")
	fs.StringVar(&cfg.InstallDir, "install-dir", cfg.InstallDir, "主服务安装目录")
	fs.StringVar(&cfg.ServiceName, "service", cfg.ServiceName, "systemd 服务名")
	fs.StringVar(&cfg.HealthURL, "health-url", cfg.HealthURL, "主服务 healthz URL")
	fs.StringVar(&cfg.StateDir, "state-dir", cfg.StateDir, "状态目录")
	fs.StringVar(&cfg.GitHubAPI, "github-api", cfg.GitHubAPI, "GitHub API 基址（一般不用改）")
	fs.DurationVar(&cfg.HealthTimeout, "timeout", cfg.HealthTimeout, "healthz 断言超时")
	if err := fs.Parse(os.Args[1:]); err != nil {
		os.Exit(2)
	}
	if *showVersion {
		fmt.Println(version)
		return
	}
	cfg.DryRun = *dryRun
	cfg.NoRestart = *noRestart

	if err := cfg.Validate(*once); err != nil {
		logger.Printf("配置非法: %v", err)
		os.Exit(2)
	}

	if *once {
		if *tag == "" {
			logger.Print("--once 必须指定 --tag（如 --tag v1.6.2-alpha 或 --tag latest）")
			os.Exit(2)
		}
		os.Exit(runOnce(cfg, *tag, logger))
	}
	runService(cfg, logger)
}

// runOnce 手工路径：不看 Enabled（运维显式操作即授权），完整流水线后按结果退出。
func runOnce(cfg updater.Config, tag string, logger *log.Logger) int {
	st, err := updater.OpenStateWithLogger(cfg.StateDir, logger)
	if err != nil {
		logger.Printf("打开状态目录失败: %v", err)
		return 1
	}
	up := updater.New(cfg, updater.ExecRunner{Stdout: os.Stdout, Stderr: os.Stderr}, st, logger, nil)
	ctx, cancel := context.WithTimeout(context.Background(), cfg.HealthTimeout+15*time.Minute)
	defer cancel()
	res, err := up.RunOnce(ctx, tag, "")
	if err != nil {
		// 75 = EX_TEMPFAIL：锁被占用是暂时状态，脚本/cron 可稍后重试；
		// 与真正的更新失败（1）、参数/配置错误（2）区分开。
		if errors.Is(err, updater.ErrLocked) {
			logger.Printf("另一个更新进程正在运行（锁 %s/updater.lock 被占用），本次未执行；请稍后重试或先确认没有并行更新", cfg.StateDir)
			return 75
		}
		logger.Printf("更新失败 tag=%s 回滚=%v 耗时=%s: %v", res.Tag, res.RolledBack, res.Duration.Round(time.Millisecond), err)
		return 1
	}
	// latest 不新于已装版本：已是最新，不算失败也不改状态，退出码 0（cron/SSH 兜底不算错）
	if res.Skipped {
		logger.Printf("已是最新 tag=%s，未更新", res.Tag)
		return 0
	}
	logger.Printf("更新完成 tag=%s 校验和=%s 耗时=%s%s", res.Tag, res.Checksum,
		res.Duration.Round(time.Millisecond), dryNote(cfg))
	return 0
}

func dryNote(cfg updater.Config) string {
	if cfg.DryRun {
		return "（dry-run：未替换）"
	}
	if cfg.NoRestart {
		return "（--no-restart：未重启）"
	}
	return ""
}

func runService(cfg updater.Config, logger *log.Logger) {
	st, err := updater.OpenStateWithLogger(cfg.StateDir, logger)
	if err != nil {
		logger.Fatalf("打开状态目录失败: %v", err)
	}
	up := updater.New(cfg, updater.ExecRunner{}, st, logger, nil)
	srv, err := updater.NewServer(cfg, up, st, logger)
	if err != nil {
		logger.Fatalf("初始化失败: %v", err)
	}

	// 公网明文入口的慢速攻击面收口：webhook body 上限 1MiB，30s 读写足够；
	// 空闲连接 120s 回收，避免占着连接不走的慢速滥用。
	httpSrv := &http.Server{
		Addr:              cfg.Listen,
		Handler:           srv.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       120 * time.Second,
	}

	errCh := make(chan error, 1)
	logger.Printf("监听 http://%s（明文；公网仅 webhook，/status 仅 loopback/SSH 隧道）", cfg.Listen)
	go func() { errCh <- httpSrv.ListenAndServe() }()

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	select {
	case sig := <-sigCh:
		logger.Printf("收到 %s，退出中", sig)
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = httpSrv.Shutdown(ctx)
	case err := <-errCh:
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			logger.Fatalf("HTTP 服务退出: %v", err)
		}
	}
}
