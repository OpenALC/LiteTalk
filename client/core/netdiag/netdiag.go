// Package netdiag 客户端一键网络诊断（#49 C）：REST healthz、信令 WS 握手、
// STUN binding 与 TURN allocate 四项独立探测。只读当前配置做一次可达性/时延
// 体检，不新增服务端端点，也不修改任何连接状态。
package netdiag

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/OpenALC/LiteTalk/client/core/config"
	"github.com/OpenALC/LiteTalk/client/core/signal"
	"github.com/pion/logging"
	"github.com/pion/stun/v4"
	"github.com/pion/turn/v5"
)

// defaultTimeout 单项探测默认超时（4s：足够一次公网往返，又不至于让设置页
// 的「诊断中」超过用户耐心）。
const defaultTimeout = 4 * time.Second

// 候选回退（#117）：配置里常有多条 STUN / TURN URL（主备、IPv4/IPv6 双栈），
// 旧实现只看第一条，首条不可用就误报该类不可用。
const (
	// maxCandidates 单类别最多尝试的候选数：再多就是配置问题，诊断不该
	// 陪着把所有地址轮一遍。
	maxCandidates = 4
	// minCandidateBudget 单个候选的最短预算。预算不足时宁可少试几个候选，
	// 也不要给每个候选几十毫秒的假超时。
	minCandidateBudget = 400 * time.Millisecond
)

// ICEServer 与 core/api.ICEServer 同构；netdiag 不依赖 api 包，便于单测。
type ICEServer struct {
	URLs       []string
	Username   string
	Credential string
}

// Config 诊断输入。BaseURL/WSURL 来自当前生效配置；ICEServers 来自
// POST /rtc/credentials（缺失即跳过 STUN/TURN）。
type Config struct {
	BaseURL    string
	WSURL      string
	Token      string
	ICEServers []ICEServer
	// Timeout 单项探测超时，0 = defaultTimeout。
	Timeout time.Duration
	// WSConnected 当前信令已连接。同一 token 再拨号会被服务端按同 sid 顶替
	// （close 4000），因此已有活跃连接时不再拨号，直接报告「信令已连接」。
	WSConnected bool
}

// ProbeResult 单项探测结果。Skipped 表示当前没有可探测的配置（未配置 STUN/
// TURN 或缺凭证），不计为失败。
type ProbeResult struct {
	OK        bool   `json:"ok"`
	Skipped   bool   `json:"skipped,omitempty"`
	LatencyMS int64  `json:"latency_ms"`
	Version   string `json:"version,omitempty"`   // healthz 版本
	Address   string `json:"address,omitempty"`   // STUN 映射地址 / TURN relay 地址
	Candidate string `json:"candidate,omitempty"` // 实际成功的候选（脱敏 host:port）
	Note      string `json:"note,omitempty"`      // 补充说明（如「信令已连接」）
	Error     string `json:"error,omitempty"`     // 已脱敏的短错误
}

// Result 一次诊断的完整结果。OK 只统计实际执行的探测：未配置（Skipped）
// 的项不算失败。
type Result struct {
	OK   bool        `json:"ok"`
	REST ProbeResult `json:"rest"`
	WS   ProbeResult `json:"ws"`
	STUN ProbeResult `json:"stun"`
	TURN ProbeResult `json:"turn"`
}

// Run 并行执行四项探测（各带独立超时，互不影响）。STUN/TURN 按配置顺序逐个
// 尝试候选，任一成功即判该类可用（#117）；没有对应配置时返回 Skipped。
func Run(ctx context.Context, cfg Config) Result {
	timeout := cfg.Timeout
	if timeout <= 0 {
		timeout = defaultTimeout
	}
	var res Result
	var wg sync.WaitGroup
	wg.Add(4)
	go func() { defer wg.Done(); res.REST = Healthz(ctx, cfg.BaseURL, timeout) }()
	go func() { defer wg.Done(); res.WS = probeSignal(ctx, cfg, timeout) }()
	go func() { defer wg.Done(); res.STUN = probeSTUNConfig(ctx, cfg.ICEServers, timeout) }()
	go func() { defer wg.Done(); res.TURN = probeTURNConfig(ctx, cfg.ICEServers, timeout) }()
	wg.Wait()
	res.OK = res.REST.OK && res.WS.OK && probeOK(res.STUN) && probeOK(res.TURN)
	return res
}

// probeOK 跳过与成功都算通过；真正的失败才拉低整体结果。
func probeOK(p ProbeResult) bool { return p.Skipped || p.OK }

// Healthz GET <base>/api/v1/healthz（3~5s 超时）。行为与设置页「测试连接」
// 完全一致：非法地址立即失败；可达但响应格式异常仍算连通。
func Healthz(ctx context.Context, baseURL string, timeout time.Duration) ProbeResult {
	if err := config.ValidateBase(baseURL); err != nil {
		return ProbeResult{Error: err.Error()}
	}
	if timeout <= 0 {
		timeout = defaultTimeout
	}
	client := &http.Client{Timeout: timeout}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, baseURL+"/api/v1/healthz", nil)
	if err != nil {
		return ProbeResult{Error: "服务器地址无效"}
	}
	start := time.Now()
	resp, err := client.Do(req)
	if err != nil {
		return ProbeResult{Error: "连接失败"}
	}
	defer resp.Body.Close()
	latency := time.Since(start).Milliseconds()
	if resp.StatusCode != http.StatusOK {
		return ProbeResult{LatencyMS: latency, Error: fmt.Sprintf("服务端返回 %d", resp.StatusCode)}
	}
	var body struct {
		Status  string `json:"status"`
		Version string `json:"version"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 4096)).Decode(&body); err != nil {
		return ProbeResult{OK: true, LatencyMS: latency} // 可达但格式异常：仍算连通
	}
	return ProbeResult{OK: true, LatencyMS: latency, Version: body.Version}
}

// probeSignal WS 握手探测。已有活跃连接时不再拨号：同一 token 的第二次
// 握手会被服务端按同 sid 顶替，踢掉正在用的那条连接。现有连接本身即证明
// 信令可达，按成功上报并注明未重新拨号。
func probeSignal(ctx context.Context, cfg Config, timeout time.Duration) ProbeResult {
	if cfg.WSConnected {
		return ProbeResult{OK: true, Note: "信令已连接（未重复拨号）"}
	}
	if strings.TrimSpace(cfg.WSURL) == "" {
		return ProbeResult{Skipped: true, Note: "未配置"}
	}
	lat, err := signal.Probe(ctx, cfg.WSURL, cfg.Token, timeout)
	res := ProbeResult{LatencyMS: lat.Milliseconds()}
	if err != nil {
		res.Error = err.Error()
		return res
	}
	res.OK = true
	return res
}

// probeSTUNConfig 按候选顺序逐个尝试 stun: binding 探测，任一成功即算该类
// 可用，并在 Candidate 里标出实际成功的地址（#117）。只有 stuns:（TLS）时
// 不视为未配置——明确报告不支持，避免用户误以为 STUN 正常。
func probeSTUNConfig(ctx context.Context, servers []ICEServer, timeout time.Duration) ProbeResult {
	if timeout <= 0 {
		timeout = defaultTimeout
	}
	out := probeableCandidates(ctx, iceCandidates(servers, "stun"), timeout,
		func(c iceCandidate, budget time.Duration) (string, error) {
			return stunBinding(ctx, c.raw, budget)
		})
	if out.attempted > 0 {
		return out.result
	}
	if _, rawTLS, hasTLS := firstICE(servers, "stuns"); hasTLS {
		return ProbeResult{Error: "暂不支持 stuns: 探测（" + hostOnly(rawTLS) + "）"}
	}
	return ProbeResult{Skipped: true, Note: "未配置"}
}

// probeTURNConfig 按候选顺序逐个尝试 turn:/turns: allocate 探测，任一成功
// 即算该类可用（#117）。无凭证或缺配置照旧明确说明，不当作可用。
func probeTURNConfig(ctx context.Context, servers []ICEServer, timeout time.Duration) ProbeResult {
	if timeout <= 0 {
		timeout = defaultTimeout
	}
	all := iceCandidates(servers, "turn", "turns")
	if len(all) == 0 {
		return ProbeResult{Skipped: true, Note: "未配置"}
	}
	// 缺凭证的条目无法 allocate，直接剔除；全部缺凭证才报「未配置凭证」。
	withCred := make([]iceCandidate, 0, len(all))
	for _, c := range all {
		if c.srv.Username != "" && c.srv.Credential != "" {
			withCred = append(withCred, c)
		}
	}
	if len(withCred) == 0 {
		return ProbeResult{Skipped: true, Note: "未配置凭证"}
	}
	out := probeableCandidates(ctx, withCred, timeout,
		func(c iceCandidate, budget time.Duration) (string, error) {
			return turnAllocate(ctx, c.raw, c.srv.Username, c.srv.Credential, budget)
		})
	return out.result
}

// probeOutcome 候选回退的结果：已尝试过的候选数与探测结果。
type probeOutcome struct {
	attempted int
	result    ProbeResult
}

// probeableCandidates 依次尝试候选直到成功或预算耗尽。成功与失败的判定都
// 归集到同一个 ProbeResult：成功带 Address/Candidate，失败带首个错误摘要。
//
// 总耗时受 timeout 约束：每个候选按 candidateBudget 分配预算，且该预算同时
// 写进 ctx deadline 与探测实现自身（两条路径都生效，不会出现外层不守时）。
func probeableCandidates(ctx context.Context, cands []iceCandidate, timeout time.Duration,
	probe func(c iceCandidate, budget time.Duration) (string, error)) probeOutcome {
	var out probeOutcome
	start := time.Now()
	var firstErr string
	for i, c := range cands {
		budget := candidateBudget(timeout, time.Since(start), len(cands)-i)
		if budget <= 0 {
			break
		}
		out.attempted++
		addr, err := probe(c, budget)
		if err != nil {
			if firstErr == "" {
				firstErr = sanitize(err.Error(), c.srv)
			}
			continue
		}
		out.result = ProbeResult{
			OK:        true,
			LatencyMS: time.Since(start).Milliseconds(),
			Address:   addr,
			Candidate: hostOnly(c.raw),
		}
		if out.attempted > 1 {
			out.result.Note = fmt.Sprintf("第 %d 个候选可用", out.attempted)
		}
		return out
	}
	out.result = ProbeResult{
		LatencyMS: time.Since(start).Milliseconds(),
		Error:     firstErr,
	}
	if out.attempted > 1 {
		out.result.Note = fmt.Sprintf("已尝试 %d 个候选", out.attempted)
	}
	return out
}

// firstICE 找第一个指定 scheme 的候选（仅用于给出诊断文案）。
func firstICE(servers []ICEServer, schemes ...string) (ICEServer, string, bool) {
	cands := iceCandidates(servers, schemes...)
	if len(cands) == 0 {
		return ICEServer{}, "", false
	}
	return cands[0].srv, cands[0].raw, true
}

// iceCandidate 一个待探测的 ICE 地址（附它所属的服务器条目，TURN 需要凭证）。
type iceCandidate struct {
	srv ICEServer
	raw string
}

// iceCandidates 按配置顺序收集指定 scheme 的 URL，去重并截断到 maxCandidates。
// 同一条目内的多个 URL 与多个条目间的 URL 一视同仁：主备配置通常写在同一条
// ICEServer 里。
func iceCandidates(servers []ICEServer, schemes ...string) []iceCandidate {
	out := make([]iceCandidate, 0, len(servers))
	seen := make(map[string]struct{}, len(servers))
	for _, srv := range servers {
		for _, raw := range srv.URLs {
			raw = strings.TrimSpace(raw)
			u, err := url.Parse(raw)
			if err != nil {
				continue
			}
			matched := false
			for _, scheme := range schemes {
				if strings.EqualFold(u.Scheme, scheme) {
					matched = true
					break
				}
			}
			if !matched {
				continue
			}
			// 去重按完整 URL：同一服务器写成 stun:a 与 stun:a?transport=udp 时
			// 只保留先出现的那条。
			key := strings.ToLower(raw)
			if _, dup := seen[key]; dup {
				continue
			}
			seen[key] = struct{}{}
			out = append(out, iceCandidate{srv: srv, raw: raw})
			if len(out) >= maxCandidates {
				return out
			}
		}
	}
	return out
}

// candidateBudget 单个候选的预算：把总预算平均分给剩余候选，不少于
// minCandidateBudget，也不超过剩余总预算。返回 0 表示剩余预算已经不够再试。
//
// 分配是动态的：首条快速失败（连接被拒/域名解析失败）时剩余候选自动拿到
// 更多时间，总耗时不会超过总预算。
func candidateBudget(total, elapsed time.Duration, remaining int) time.Duration {
	left := total - elapsed
	if left <= 0 || remaining <= 0 {
		return 0
	}
	per := total / time.Duration(remaining)
	if per < minCandidateBudget {
		per = minCandidateBudget
	}
	if per > left {
		per = left
	}
	return per
}

// iceHostPort 从 stun:/turn: URL 取 host:port。Go 的 url.Parse 对无 // 的
// ICE URL 把地址放在 Opaque 里；两种写法都要兼容。
func iceHostPort(raw, defaultPort string) (string, string, error) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return "", "", errors.New("地址无效")
	}
	host := u.Host
	if host == "" {
		host = u.Opaque
	}
	host = strings.TrimPrefix(host, "//")
	if host == "" {
		return "", "", errors.New("地址无效")
	}
	if h, p, err := net.SplitHostPort(host); err == nil {
		return h, p, nil
	}
	return strings.Trim(host, "[]"), defaultPort, nil
}

// transportOf ICE URL 的 ?transport= 参数（小写）。
func transportOf(raw string) string {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return ""
	}
	return strings.ToLower(u.Query().Get("transport"))
}

// hostOnly 仅用于错误/说明文案，避免把凭证带进 UI。
func hostOnly(raw string) string {
	host, port, err := iceHostPort(raw, "")
	if err != nil {
		return "已配置"
	}
	if port == "" {
		return host
	}
	return net.JoinHostPort(host, port)
}

// stunBinding 一次 STUN binding 探测（UDP）。最多重试一次，避免单个 UDP
// 包丢失就判定网络不可用。
func stunBinding(ctx context.Context, raw string, timeout time.Duration) (string, error) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || !strings.EqualFold(u.Scheme, "stun") {
		return "", errors.New("STUN 地址无效")
	}
	if transportOf(raw) == "tcp" {
		return "", errors.New("暂不支持 STUN TCP 探测")
	}
	host, port, err := iceHostPort(raw, "3478")
	if err != nil {
		return "", err
	}
	if timeout <= 0 {
		timeout = defaultTimeout
	}
	deadline := time.Now().Add(timeout)
	if dl, ok := ctx.Deadline(); ok && dl.Before(deadline) {
		deadline = dl
	}
	d := net.Dialer{Deadline: deadline}
	conn, err := d.DialContext(ctx, "udp", net.JoinHostPort(host, port))
	if err != nil {
		return "", errors.New("无法连接 STUN 服务器")
	}
	defer conn.Close()

	attempts := 2
	for i := 0; i < attempts; i++ {
		attemptDeadline := deadline
		// 首次尝试最多给一半预算，留一次重试机会。
		if half := time.Now().Add(time.Until(deadline) / 2); i == 0 && half.Before(attemptDeadline) {
			attemptDeadline = half
		}
		_ = conn.SetDeadline(attemptDeadline)
		req := stun.MustBuild(stun.TransactionID, stun.BindingRequest)
		if _, err := conn.Write(req.Raw); err != nil {
			return "", errors.New("STUN 请求发送失败")
		}
		buf := make([]byte, 1500)
		n, err := conn.Read(buf)
		if err != nil {
			if isTimeout(err) && i == 0 {
				continue
			}
			if isTimeout(err) {
				return "", errors.New("STUN 响应超时")
			}
			return "", errors.New("STUN 响应读取失败")
		}
		res := &stun.Message{Raw: append([]byte(nil), buf[:n]...)}
		if err := res.Decode(); err != nil || res.Type != stun.BindingSuccess {
			return "", errors.New("STUN 响应无效")
		}
		var xor stun.XORMappedAddress
		if err := xor.GetFrom(res); err == nil {
			return xor.String(), nil
		}
		var mapped stun.MappedAddress
		if err := mapped.GetFrom(res); err == nil {
			return mapped.String(), nil
		}
		return "", errors.New("STUN 响应缺少映射地址")
	}
	return "", errors.New("STUN 响应超时")
}

func isTimeout(err error) bool {
	var ne net.Error
	return errors.As(err, &ne) && ne.Timeout()
}

// turnAllocate 一次 TURN allocate 探测：拿到 relay 地址后立即释放。
// 当前仅支持 UDP 传输（pion/turn Client 基于 PacketConn）；tcp/tls 明确报错。
func turnAllocate(ctx context.Context, raw, username, credential string, timeout time.Duration) (string, error) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return "", errors.New("TURN 地址无效")
	}
	switch strings.ToLower(u.Scheme) {
	case "turn":
	case "turns":
		return "", errors.New("暂不支持 turns: 探测")
	default:
		return "", errors.New("TURN 地址无效")
	}
	transport := transportOf(raw)
	if transport != "" && transport != "udp" {
		return "", fmt.Errorf("暂不支持 TURN %s 传输", transport)
	}
	host, port, err := iceHostPort(raw, "3478")
	if err != nil {
		return "", err
	}
	ip, err := resolveHost(ctx, host)
	if err != nil {
		return "", errors.New("TURN 域名解析失败")
	}
	serverAddr := net.JoinHostPort(ip, port)

	// TURN 客户端基于 PacketConn，并按本地地址族推断 allocation 地址族：
	// 双栈 "udp" 会拿到 [::]，向 IPv4 relay 申请时服务端可能回 508。
	network, laddr := "udp4", &net.UDPAddr{IP: net.IPv4zero}
	if v4 := net.ParseIP(ip).To4(); v4 == nil {
		network, laddr = "udp6", &net.UDPAddr{IP: net.IPv6unspecified}
	}
	conn, err := net.ListenUDP(network, laddr)
	if err != nil {
		return "", errors.New("本地 UDP 套接字创建失败")
	}
	defer conn.Close()

	client, err := turn.NewClient(&turn.ClientConfig{
		STUNServerAddr: serverAddr,
		TURNServerAddr: serverAddr,
		Conn:           conn,
		Username:       username,
		Password:       credential,
		Software:       "LiteTalk netdiag",
		LoggerFactory:  quietLoggerFactory(),
	})
	if err != nil {
		return "", errors.New("TURN 客户端初始化失败")
	}
	defer client.Close()
	if err := client.Listen(); err != nil {
		return "", errors.New("TURN 客户端启动失败")
	}
	if timeout <= 0 {
		timeout = defaultTimeout
	}
	actx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	relay, err := client.AllocateWithContext(actx)
	if err != nil {
		if actx.Err() != nil {
			return "", errors.New("TURN allocate 超时")
		}
		return "", fmt.Errorf("TURN allocate 失败: %s", err)
	}
	defer relay.Close()
	return relay.LocalAddr().String(), nil
}

// resolveHost 带 ctx 的域名解析（turn.NewClient 内部的解析不受 ctx 约束），
// 优先 IPv4，避免双栈环境选到不可达的 IPv6。
func resolveHost(ctx context.Context, host string) (string, error) {
	if ip := net.ParseIP(host); ip != nil {
		return ip.String(), nil
	}
	ips, err := net.DefaultResolver.LookupIPAddr(ctx, host)
	if err != nil || len(ips) == 0 {
		return "", errors.New("解析失败")
	}
	for _, ip := range ips {
		if v4 := ip.IP.To4(); v4 != nil {
			return v4.String(), nil
		}
	}
	return ips[0].IP.String(), nil
}

// quietLoggerFactory TURN 客户端默认把错误写到 stderr；诊断是用户主动触发
// 的探测，失败已经回传给 UI，不再污染应用日志。
func quietLoggerFactory() logging.LoggerFactory {
	return &logging.DefaultLoggerFactory{
		Writer:          io.Discard,
		DefaultLogLevel: logging.LogLevelError,
		ScopeLevels:     map[string]logging.LogLevel{},
	}
}

// sanitize 错误摘要脱敏：凭证绝不能随诊断结果离开进程。
func sanitize(msg string, srv ICEServer) string {
	if srv.Credential != "" {
		msg = strings.ReplaceAll(msg, srv.Credential, "***")
	}
	if srv.Username != "" {
		msg = strings.ReplaceAll(msg, srv.Username, "***")
	}
	if len(msg) > 160 {
		// 按字符截断：中文错误信息按字节切会留下半个 UTF-8 序列。
		r := []rune(msg)
		if len(r) > 160 {
			r = r[:160]
		}
		msg = string(r) + "…"
	}
	return msg
}
