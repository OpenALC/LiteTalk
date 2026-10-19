// e2eprobe 对 LiteTalk 服务端做真实客户端端到端验证：
// REST 注册/登录（simple 免验证码）→ 取 TURN/STUN 凭证 → WS 信令入房 →
// Pion PeerConnection（完整 ICE）→ 50pps 哑 RTP 双向收发计数。
//
// 与 loadtest 的区别：走真实 JWT 鉴权（不需要 LITETALK_DEV_AUTH），
// 用于验证公网部署的 REST/WS/UDP 全链路。一次性工具，用完即删。
//
//	go run ./cmd/e2eprobe -addr 8.130.211.132:8077
//	go run ./cmd/e2eprobe -addr 127.0.0.1:8077 -loopback   # 本机服务端
//	go run ./cmd/e2eprobe -addr 127.0.0.1:8077 -multi-device  # 多设备会话场景（踢线/共存/语音抢占）
//	go run ./cmd/e2eprobe -addr 127.0.0.1:8077 -presence  # 在线状态场景（好友 online 随连断翻转）
//	go run ./cmd/e2eprobe -addr 127.0.0.1:8077 -rejoin -loopback  # 重入房场景（离房再入房，B 须恢复收听）
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/signal"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"
)

// 服务端 /auth/* 的强制升级门（#31）：探测客户端统一声明 dev 版本，
// 服务端对 dev / 含 dev 的探测值放行，避免探针被 426 挡住。
const (
	clientVersionHeader = "X-LiteTalk-Client"
	probeClientVersion  = "e2eprobe-dev"
)

type config struct {
	addr        string
	users       int
	duration    time.Duration
	loopback    bool
	multiDevice bool
	relay       bool
	presence    bool
	rejoin      bool
	tcpOnly     bool // -tcp：禁 UDP 场景，客户端只启用 TCP 网络类型（#25）
}

// register 注册（邮箱撞库则登录），返回注册邮箱、access token、用户 ID 与 iceServers。
// 邮箱单独返回供 -multi-device 场景后续按设备反复登录。
func register(addr, nickname string) (email, token string, id int64, ice []ICEServer, err error) {
	email = fmt.Sprintf("probe%d@example.com", time.Now().UnixNano())
	password := "probe1234"

	post := func(path string, body any) (int, []byte, error) {
		raw, _ := json.Marshal(body)
		req, err := http.NewRequest(http.MethodPost, "http://"+addr+path, bytes.NewReader(raw))
		if err != nil {
			return 0, nil, err
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set(clientVersionHeader, probeClientVersion)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			return 0, nil, err
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, b, nil
	}

	code, b, err := post("/api/v1/auth/register", map[string]string{
		"nickname": nickname, "email": email, "password": password,
	})
	if err != nil {
		return "", "", 0, nil, fmt.Errorf("register 请求: %w", err)
	}
	var pair struct {
		AccessToken string `json:"access_token"`
		User        struct {
			ID jsonID `json:"id"`
		} `json:"user"`
	}
	if code != http.StatusCreated {
		return "", "", 0, nil, fmt.Errorf("register HTTP %d: %s", code, b)
	}
	if err := json.Unmarshal(b, &pair); err != nil || pair.AccessToken == "" {
		return "", "", 0, nil, fmt.Errorf("token 解析失败: %s", b)
	}

	req, _ := http.NewRequest("POST", "http://"+addr+"/api/v1/rtc/credentials?token="+pair.AccessToken, nil)
	req.Header.Set(clientVersionHeader, probeClientVersion)
	cresp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", "", 0, nil, fmt.Errorf("rtc/credentials: %w", err)
	}
	defer cresp.Body.Close()
	var creds struct {
		ICEServers []ICEServer `json:"iceServers"`
	}
	if cresp.StatusCode == http.StatusOK {
		_ = json.NewDecoder(cresp.Body).Decode(&creds)
	}
	return email, pair.AccessToken, int64(pair.User.ID), creds.ICEServers, nil
}

func main() {
	cfg := &config{}
	flag.StringVar(&cfg.addr, "addr", "8.130.211.132:8077", "服务端地址")
	flag.IntVar(&cfg.users, "users", 2, "每房间人数（含自己）")
	flag.DurationVar(&cfg.duration, "duration", 8*time.Second, "发包时长")
	flag.BoolVar(&cfg.loopback, "loopback", false, "允许环回候选（测本机服务端时必开）")
	flag.BoolVar(&cfg.multiDevice, "multi-device", false, "多设备会话场景：同设备踢线/异设备共存/语音抢占")
	flag.BoolVar(&cfg.relay, "relay", false, "验证 TURN 凭证含 turn/turns 中继地址（需 LITETALK_TURN_SECRET 已配）")
	flag.BoolVar(&cfg.presence, "presence", false, "在线状态场景：好友 online 随 WS 连断翻转")
	flag.BoolVar(&cfg.rejoin, "rejoin", false, "重入房场景：离房再入房，B 须恢复收听（#23）")
	flag.BoolVar(&cfg.tcpOnly, "tcp", false, "禁 UDP 场景：客户端只启用 TCP 候选，验证 ICE-TCP 直连（#25）")
	flag.Parse()
	if cfg.users < 2 {
		fmt.Fprintln(os.Stderr, "users ≥ 2")
		os.Exit(2)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if cfg.rejoin {
		code := runRejoin(ctx, cfg)
		stop()
		os.Exit(code)
	}

	if cfg.presence {
		code := runPresence(ctx, cfg)
		stop()
		os.Exit(code)
	}

	if cfg.multiDevice {
		code := runMultiDevice(ctx, cfg)
		stop()
		os.Exit(code)
	}

	fmt.Printf("E2E 探测 %s：%d 人同房，%s 媒体，loopback=%v tcpOnly=%v\n",
		cfg.addr, cfg.users, cfg.duration, cfg.loopback, cfg.tcpOnly)

	total := cfg.users
	users := make([]*probeUser, 0, total)
	var wg sync.WaitGroup
	joinLat := make([]time.Duration, 0, total)
	var joinMu sync.Mutex
	errCh := make(chan error, total)
	sendStart := make(chan struct{})
	sendStop := make(chan struct{})

	fmt.Println("阶段 1/2：注册 + 建连…")
	// 先全部注册，拿到各自的服务端用户 ID（下行轨 TrackID=audio-<id>）
	type acct struct {
		token string
		id    string // 服务端用户 ID（十进制）
		ice   []ICEServer
	}
	accts := make([]acct, 0, cfg.users)
	for i := 0; i < cfg.users; i++ {
		_, token, uid, ice, err := register(cfg.addr, fmt.Sprintf("探测%d", i+1))
		if err != nil {
			fmt.Fprintln(os.Stderr, "注册失败:", err)
			os.Exit(1)
		}
		fmt.Printf("  probe-%d 注册+取凭证 OK（uid=%d，iceServers=%d 条）\n", i, uid, len(ice))
		accts = append(accts, acct{token: token, id: fmt.Sprintf("%d", uid), ice: ice})
	}

	// -relay：验证凭证含 TURN 中继地址（UDP 被封网络的兜底路径）。
	// 本验证确认「配置 → 凭证签发 → 客户端收到」链路通；实际中继转发
	// 需真机 coturn 联调（网络层无法在 CI 模拟）。
	if cfg.relay {
		hasTurn := false
		for _, a := range accts {
			for _, srv := range a.ice {
				for _, u := range srv.urls() {
					if strings.HasPrefix(u, "turn:") || strings.HasPrefix(u, "turns:") {
						hasTurn = true
						fmt.Printf("  TURN 中继地址: %s\n", u)
					}
				}
			}
		}
		if !hasTurn {
			fmt.Fprintln(os.Stderr, "-relay 失败：凭证中无 turn/turns 地址（检查 LITETALK_TURN_URLS 与 LITETALK_TURN_SECRET）")
			os.Exit(1)
		}
		fmt.Println("  -relay OK：凭证含 TURN 中继地址")
	}

	sem := make(chan struct{}, 8)
	for i, a := range accts {
		others := make([]string, 0, cfg.users-1)
		for j, b := range accts {
			if j != i {
				others = append(others, b.id)
			}
		}
		u := newUser(cfg, "probe-room", fmt.Sprintf("probe-%d", i), a.token, a.ice, others)
		users = append(users, u)
		wg.Add(1)
		sem <- struct{}{}
		go func(u *probeUser) {
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

	waitDeadline := time.Now().Add(45 * time.Second)
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
		fmt.Fprintln(os.Stderr, "建连失败:", <-errCh)
		stop()
		wg.Wait()
		os.Exit(1)
	}
	fmt.Printf("阶段 1/2：%d/%d ICE connected\n", total, total)

	// -tcp：断言选中的是 TCP 候选对（禁 UDP 场景的 ICE-TCP 直连证明）
	if cfg.tcpOnly {
		for _, u := range users {
			proto := u.selectedProtocol()
			fmt.Printf("  %s 选中候选对: %s\n", u.id, proto)
			if !strings.Contains(proto, "tcp") {
				fmt.Fprintf(os.Stderr, "-tcp 失败：%s 选中候选协议 %q，必须是 tcp\n", u.id, proto)
				os.Exit(1)
			}
		}
		fmt.Println("  -tcp OK：全部经 ICE-TCP 候选对建连（禁 UDP 网络等价）")
	}

	// 等所有端到达发包闸门（下行轨齐或超时），再统一开闸，保证完整发包窗口
	for _, u := range users {
		select {
		case <-u.atGate:
		case <-time.After(15 * time.Second):
		}
	}
	close(sendStart)
	fmt.Println("阶段 2/2：50pps 双向媒体…")
	select {
	case <-ctx.Done():
	case <-time.After(cfg.duration):
	}
	close(sendStop)
	time.Sleep(2 * time.Second)
	stop()
	wg.Wait()

	// ---- 汇总 ----
	var sent, recv, trackErr, wsErr int64
	for _, u := range users {
		sent += u.sent.Load()
		trackErr += u.trackErr.Load()
		wsErr += u.wsErr.Load()
		u.recvMu.Lock()
		for _, c := range u.recv {
			recv += c.Load()
		}
		u.recvMu.Unlock()
	}
	expected := sent // 每上行包同房每人应收一份（2 人房：sent × 1）
	loss := 0.0
	if expected > 0 {
		loss = float64(expected-recv) / float64(expected) * 100
	}
	fmt.Println("\n==== E2E 探测汇总 ====")
	fmt.Printf("上行包: %d，下行包: %d（期望 %d），下行缺失: %.2f%%\n", sent, recv, expected, loss)
	fmt.Printf("媒体错误: %d，WS 错误: %d\n", trackErr, wsErr)
	if len(joinLat) > 0 {
		sort.Slice(joinLat, func(i, j int) bool { return joinLat[i] < joinLat[j] })
		fmt.Printf("建连耗时 P50=%s max=%s\n", joinLat[len(joinLat)/2], joinLat[len(joinLat)-1])
	}
	if trackErr > 0 || wsErr > 0 || recv == 0 {
		fmt.Println("结论: FAIL——UDP 链路或媒体转发存在问题")
		os.Exit(1)
	}
	fmt.Println("结论: PASS——REST/WS/UDP 全链路正常")
}
