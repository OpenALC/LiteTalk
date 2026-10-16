// loadtest 是 LiteTalk SFU 的无头压测工具：模拟 rooms×users 个真实客户端
// （WS 信令 + Pion PeerConnection + 50pps 哑 RTP），量化转发性能。
//
// 用法（需先启动服务端，dev-token 模式）：
//
//	LITETALK_DEV_AUTH=1 go run ./cmd/server
//	go run ./cmd/loadtest -rooms 10 -users 10 -duration 5m -addr 127.0.0.1:8077 -token dev-load
//
// 退出码：0 = 达标；1 = 丢包率超阈值或连接失败；2 = 参数/环境错误。
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"runtime"
	"sort"
	"sync"
	"syscall"
	"time"
)

type config struct {
	addr     string
	token    string // dev-token 前缀，用户 ID 为 <base>-<room>-<idx>
	rooms    int
	users    int // 每房间人数（含自己）
	duration time.Duration

	maxLossPct float64 // 丢包率阈值（%），超过则退出码 1
	interval   time.Duration
	loopback   bool
}

func (c *config) base() string {
	// dev-<base> → base；便于多机压测时 ID 不冲突（各机传不同 -token）
	if len(c.token) > 4 && c.token[:4] == "dev-" {
		return c.token[4:]
	}
	return c.token
}

func main() {
	cfg := &config{}
	flag.StringVar(&cfg.addr, "addr", "127.0.0.1:8077", "服务端地址")
	flag.StringVar(&cfg.token, "token", "dev-load", "dev-token 前缀（用户 ID = <base>-<room>-<idx>）")
	flag.IntVar(&cfg.rooms, "rooms", 10, "房间数")
	flag.IntVar(&cfg.users, "users", 10, "每房间人数（含自己，上限即 SFU MaxMembers）")
	flag.DurationVar(&cfg.duration, "duration", 5*time.Minute, "压测时长")
	flag.Float64Var(&cfg.maxLossPct, "max-loss", 1.0, "丢包率阈值（%%），超过退出码 1")
	flag.DurationVar(&cfg.interval, "interval", 10*time.Second, "统计打印间隔")
	flag.BoolVar(&cfg.loopback, "loopback", true, "允许环回候选（服务端 NAT1To1=127.0.0.1 时必须）")
	flag.Parse()

	if cfg.rooms <= 0 || cfg.users < 2 {
		fmt.Fprintln(os.Stderr, "参数非法：rooms ≥ 1 且 users ≥ 2")
		os.Exit(2)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	total := cfg.rooms * cfg.users
	fmt.Printf("LiteTalk SFU 压测：%d 房 × %d 人 = %d 连接，时长 %s，目标 %s\n",
		cfg.rooms, cfg.users, total, cfg.duration, cfg.addr)

	// CPU/内存基线
	baseCPU := cpuTime()
	var baseMem runtime.MemStats
	runtime.ReadMemStats(&baseMem)

	users := make([]*loadUser, 0, total)
	joinLat := make([]time.Duration, 0, total)
	var joinMu sync.Mutex
	var wg sync.WaitGroup
	sendStart := make(chan struct{})
	sendStop := make(chan struct{})
	errCh := make(chan error, total)

	// 阶段 1：全部建连（第 2 人起覆盖 SFU 重协商路径）
	fmt.Println("阶段 1/2：建连中…")
	sem := make(chan struct{}, 32) // 限并发防信令风暴
	for r := 0; r < cfg.rooms; r++ {
		for i := 0; i < cfg.users; i++ {
			id := fmt.Sprintf("%s-%d-%d", cfg.base(), r, i)
			room := fmt.Sprintf("lt-%d", r)
			u := newUser(cfg, room, r, id)
			users = append(users, u)
			wg.Add(1)
			sem <- struct{}{}
			go func(u *loadUser) {
				defer wg.Done()
				defer func() { <-sem }()
				if err := u.run(ctx, sendStart, sendStop); err != nil {
					errCh <- err
					return
				}
				joinMu.Lock()
				joinLat = append(joinLat, u.connectedAt.Sub(u.joinStart))
				joinMu.Unlock()
			}(u)
		}
	}

	// 等全部 connected（或有失败）：30s 超时兜底在 run 内，此处等 run 完成到发包阶段。
	// 简化：等所有 goroutine 进入等待 sendStart——用粗粒度判定（全部 connected + 轨道就绪）。
	waitDeadline := time.Now().Add(60 * time.Second)
	for {
		if len(errCh) > 0 {
			break
		}
		connected := 0
		for _, u := range users {
			select {
			case <-u.connected:
				connected++
			default:
			}
		}
		if connected == total || time.Now().After(waitDeadline) {
			break
		}
		time.Sleep(200 * time.Millisecond)
	}
	if len(errCh) > 0 {
		err := <-errCh
		fmt.Fprintf(os.Stderr, "建连失败：%v\n", err)
		stop()
		wg.Wait()
		os.Exit(1)
	}
	fmt.Printf("阶段 1/2：%d/%d 连接就绪\n", total, total)

	// 阶段 2：开始发包
	close(sendStart)
	fmt.Println("阶段 2/2：50pps 发包中…（Ctrl+C 提前汇总）")

	ticker := time.NewTicker(cfg.interval)
	defer ticker.Stop()
	start := time.Now()
	for {
		select {
		case <-ctx.Done():
			goto summary
		case <-time.After(cfg.duration):
			goto summary
		case <-ticker.C:
			printInterval(users, start)
		}
	}

summary:
	close(sendStop)
	// 给收包留 2s 排空（末尾在途包）
	time.Sleep(2 * time.Second)
	stop()
	wg.Wait()
	os.Exit(report(cfg, users, joinLat, baseCPU, baseMem))
}

// ---- 统计 ----

type totals struct {
	sent, recv, trackErr, wsIn, wsOut, wsErr int64
}

func aggregate(users []*loadUser) totals {
	var t totals
	for _, u := range users {
		t.sent += u.sent.Load()
		t.trackErr += u.trackErr.Load()
		t.wsIn += u.wsIn.Load()
		t.wsOut += u.wsOut.Load()
		t.wsErr += u.wsErr.Load()
		u.recvMu.Lock()
		for _, c := range u.recv {
			t.recv += c.Load()
		}
		u.recvMu.Unlock()
	}
	return t
}

func printInterval(users []*loadUser, start time.Time) {
	t := aggregate(users)
	elapsed := time.Since(start).Seconds()
	var m runtime.MemStats
	runtime.ReadMemStats(&m)
	fmt.Printf("[%6.0fs] sent=%d recv=%d ws=%d/%d heap=%.1fMB cpu=%.1fs\n",
		elapsed, t.sent, t.recv, t.wsIn, t.wsOut,
		float64(m.HeapAlloc)/1e6, cpuTime().Seconds())
}

func report(cfg *config, users []*loadUser, joinLat []time.Duration, baseCPU time.Duration, baseMem runtime.MemStats) int {
	t := aggregate(users)
	var m runtime.MemStats
	runtime.ReadMemStats(&m)

	// 期望收包 = 每用户 sent × (users-1)（同房互收）
	expected := t.sent * int64(cfg.users-1)
	lossPct := 0.0
	if expected > 0 {
		lossPct = float64(expected-t.recv) / float64(expected) * 100
	}
	cpuDelta := cpuTime() - baseCPU

	fmt.Println("\n==== 压测汇总 ====")
	fmt.Printf("连接数:      %d（%d 房 × %d 人）\n", len(users), cfg.rooms, cfg.users)
	fmt.Printf("上行包:      %d\n", t.sent)
	fmt.Printf("下行包:      %d（期望 %d）\n", t.recv, expected)
	fmt.Printf("丢包率:      %.4f%%（阈值 %.2f%%）\n", lossPct, cfg.maxLossPct)
	fmt.Printf("WS 消息:     上行 %d / 下行 %d / 错误 %d\n", t.wsOut, t.wsIn, t.wsErr)
	fmt.Printf("媒体错误:    %d\n", t.trackErr)
	if len(joinLat) > 0 {
		sort.Slice(joinLat, func(i, j int) bool { return joinLat[i] < joinLat[j] })
		fmt.Printf("建连耗时:    P50=%s P95=%s max=%s\n",
			percentile(joinLat, 50), percentile(joinLat, 95), joinLat[len(joinLat)-1])
	}
	fmt.Printf("进程 CPU:    %.1fs（压测期累计）\n", cpuDelta.Seconds())
	fmt.Printf("Go 堆:       %.1fMB（Sys %.1fMB）\n",
		float64(m.HeapAlloc)/1e6, float64(m.Sys)/1e6)

	if t.trackErr > 0 || t.wsErr > 0 {
		fmt.Println("结论: FAIL（存在媒体/信令错误）")
		return 1
	}
	if lossPct > cfg.maxLossPct {
		fmt.Println("结论: FAIL（丢包率超阈值）")
		return 1
	}
	fmt.Println("结论: PASS")
	return 0
}

func percentile(sorted []time.Duration, p int) time.Duration {
	if len(sorted) == 0 {
		return 0
	}
	idx := p * len(sorted) / 100
	if idx >= len(sorted) {
		idx = len(sorted) - 1
	}
	return sorted[idx]
}
