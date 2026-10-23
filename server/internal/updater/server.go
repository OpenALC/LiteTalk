package updater

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"runtime"
	"strings"
	"sync"
	"time"
)

// 签名失败限流参数：1 分钟窗口内每 IP 最多 20 次失败。
// 8088 直接对公网监听，暴力/慢速滥用会白白消耗 body 读与 HMAC 计算。
const (
	invalidWindowSize = time.Minute
	invalidMaxPerIP   = 20
)

// invalidCounter 某客户端 IP 在当前窗口内的签名失败次数。
type invalidCounter struct {
	count int
	start time.Time
}

// releaseEvent 只取需要的字段：action / tag / 预发布标记 / 仓库全名。
type releaseEvent struct {
	Action  string `json:"action"`
	Release struct {
		TagName    string `json:"tag_name"`
		Prerelease bool   `json:"prerelease"`
	} `json:"release"`
	Repository struct {
		FullName string `json:"full_name"`
	} `json:"repository"`
}

// Server webhook/状态 HTTP 面。fail-closed：签名、事件、仓库、tag、通道任一不符即拒。
type Server struct {
	cfg    Config
	up     *Updater
	st     *StateStore
	log    *log.Logger
	secret []byte

	mu   sync.Mutex
	busy bool

	// 签名失败限流：按客户端 IP 计数，防公网暴力/慢速滥用。
	// 只在签名失败时写；成功请求不读不计，正常投递不会被误伤。
	invalidMu sync.Mutex
	invalid   map[string]invalidCounter
	lastSweep time.Time
	now       func() time.Time // 测试注入时钟；生产为 time.Now
}

// NewServer 构造 HTTP 面。secret 优先取 cfg.Secret，否则从 SecretFile 读。
// 部署形态是 0640 root:<服务用户>：进程以服务用户运行必须能组读，other 位为 0。
// secret 为空或短于 MinSecretLen 直接报错：无签名密钥/弱密钥时宁可不启动，也不开无鉴权入口。
// 错误信息只含路径/长度，不回显 secret 内容。
func NewServer(cfg Config, up *Updater, st *StateStore, logger *log.Logger) (*Server, error) {
	if st == nil {
		return nil, fmt.Errorf("状态存储为空")
	}
	secret := cfg.Secret
	if len(secret) == 0 {
		fi, err := os.Stat(cfg.SecretFile)
		if err != nil {
			return nil, fmt.Errorf("读 webhook secret 文件: %w", err)
		}
		// Windows 的 ACL 不映射到 Unix 权限位，权限断言只在 Unix 生效（部署目标是 Linux）。
		if runtime.GOOS != "windows" && fi.Mode().Perm()&0o007 != 0 {
			return nil, fmt.Errorf("webhook secret 文件 %s 权限 %04o 过宽：必须为 0640/0600，other 位不可读写", cfg.SecretFile, fi.Mode().Perm())
		}
		b, err := os.ReadFile(cfg.SecretFile)
		if err != nil {
			return nil, fmt.Errorf("读 webhook secret 文件: %w", err)
		}
		secret = []byte(strings.TrimSpace(string(b)))
	}
	if len(secret) == 0 {
		return nil, fmt.Errorf("webhook secret 为空")
	}
	// 与 JWT/TURN 同口径：短 secret 可被离线暴力撞出 HMAC，拒绝启动。
	if len(secret) < MinSecretLen {
		return nil, fmt.Errorf("webhook secret 长度 %d 字节，不足 %d 字节（用 openssl rand -hex 32 生成）", len(secret), MinSecretLen)
	}
	return &Server{
		cfg: cfg, up: up, st: st, log: logger, secret: secret,
		invalid: make(map[string]invalidCounter),
		now:     time.Now,
	}, nil
}

func (s *Server) logf(format string, args ...any) {
	if s.log != nil {
		s.log.Printf(format, args...)
	}
}

// Handler 路由表。/status 仅环回可达。
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /hooks/github-release", s.handleWebhook)
	mux.HandleFunc("GET /healthz", s.handleHealthz)
	mux.HandleFunc("GET /status", s.handleStatus)
	return mux
}

func (s *Server) handleHealthz(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// handleStatus 状态面（只读）。环回限制：部署元数据不给公网看。
func (s *Server) handleStatus(w http.ResponseWriter, r *http.Request) {
	if !isLoopback(r.RemoteAddr) {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	// 状态可能已被 --once 或另一个进程推进：best-effort 重读，失败沿用内存快照。
	if err := s.st.Reload(); err != nil {
		s.logf("重读状态失败（沿用内存快照）: %v", err)
	}
	s.mu.Lock()
	updating := s.busy
	s.mu.Unlock()
	st := s.st.Get()
	writeJSON(w, http.StatusOK, map[string]any{
		"enabled":          s.cfg.Enabled,
		"allow_prerelease": s.cfg.AllowPrerelease,
		"updating":         updating,
		"state":            st,
	})
}

// handleWebhook GitHub release webhook。
// 校验通过立即 202 并由后台单飞 worker 执行（GitHub 10s 超时不能打断下载）。
// 策略拒绝（关闭更新 / 预发布未放行 / 非 published action）也返回 202 ignored：
// 这是「收到了、选择不动作」，不该在 GitHub 留失败投递。
func (s *Server) handleWebhook(w http.ResponseWriter, r *http.Request) {
	delivery := r.Header.Get("X-GitHub-Delivery")
	r.Body = http.MaxBytesReader(w, r.Body, MaxBodyBytes)
	body, err := io.ReadAll(r.Body)
	if err != nil {
		// 超限或读失败：一律 413/400，不区分细节
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			http.Error(w, "payload too large", http.StatusRequestEntityTooLarge)
		} else {
			http.Error(w, "bad request", http.StatusBadRequest)
		}
		return
	}

	if !validSignature(s.secret, r.Header.Get("X-Hub-Signature-256"), body) {
		ip := clientIP(r.RemoteAddr)
		if !s.noteInvalid(ip) {
			s.logf("webhook 拒绝：签名失败次数过多（限流）delivery=%s ip=%s", delivery, ip)
			http.Error(w, "too many requests", http.StatusTooManyRequests)
			return
		}
		s.logf("webhook 拒绝：签名无效 delivery=%s", delivery)
		http.Error(w, "invalid signature", http.StatusUnauthorized)
		return
	}

	event := r.Header.Get("X-GitHub-Event")
	if event == "ping" {
		s.logf("webhook ping 已忽略 delivery=%s", delivery)
		w.WriteHeader(http.StatusAccepted)
		return
	}
	if event != "release" {
		s.logf("webhook 拒绝：事件类型 %q 不在白名单 delivery=%s", event, delivery)
		http.Error(w, "unsupported event", http.StatusBadRequest)
		return
	}

	var payload releaseEvent
	if err := json.Unmarshal(body, &payload); err != nil {
		s.logf("webhook 拒绝：payload 解析失败 delivery=%s", delivery)
		http.Error(w, "bad payload", http.StatusBadRequest)
		return
	}
	if payload.Action != "published" {
		s.logf("webhook 忽略：action=%q（只处理 published）delivery=%s", payload.Action, delivery)
		w.WriteHeader(http.StatusAccepted)
		return
	}
	if payload.Repository.FullName != s.cfg.Repo {
		s.logf("webhook 拒绝：仓库 %q 不在白名单 delivery=%s", payload.Repository.FullName, delivery)
		http.Error(w, "repository not allowed", http.StatusBadRequest)
		return
	}
	tag := payload.Release.TagName
	if _, ok := parseTag(tag); !ok {
		s.logf("webhook 拒绝：tag %q 不符合白名单形态 delivery=%s", tag, delivery)
		http.Error(w, "tag not allowed", http.StatusBadRequest)
		return
	}
	if (payload.Release.Prerelease || isPrereleaseTag(tag)) && !s.cfg.AllowPrerelease {
		s.logf("webhook 忽略：预发布 %q 未放行（ALLOW_PRERELEASE=0）delivery=%s", tag, delivery)
		w.WriteHeader(http.StatusAccepted)
		return
	}
	if !s.cfg.Enabled {
		s.logf("webhook 忽略：更新器已关闭（ENABLED=0）tag=%s delivery=%s", tag, delivery)
		w.WriteHeader(http.StatusAccepted)
		return
	}

	// 状态可能已被 --once 或另一个进程推进：best-effort 重读，失败沿用内存快照。
	// 去重与防降级都依赖 CurrentVersion，用过期快照会重复更新或误判降级。
	if err := s.st.Reload(); err != nil {
		s.logf("重读状态失败（沿用内存快照）delivery=%s: %v", delivery, err)
	}
	s.mu.Lock()
	st := s.st.Get()
	if st.HasDelivery(delivery) {
		s.mu.Unlock()
		s.logf("webhook 忽略：重复投递 delivery=%s tag=%s", delivery, tag)
		w.WriteHeader(http.StatusOK)
		return
	}
	if !tagNewerThan(tag, st.CurrentVersion) {
		s.mu.Unlock()
		s.logf("webhook 忽略：tag=%s 不新于当前版本 %s（防降级）", tag, st.CurrentVersion)
		w.WriteHeader(http.StatusOK)
		return
	}
	if s.busy {
		s.mu.Unlock()
		s.logf("webhook 拒绝：更新进行中 tag=%s delivery=%s", tag, delivery)
		http.Error(w, "update in progress", http.StatusConflict)
		return
	}
	// 进程内 busy 通过后再取跨进程锁：--once/另一个实例正在更新时同样 409。
	// 非阻塞，可以在 s.mu 内做；失败路径都不设置 busy。
	lock, err := AcquireLock(s.cfg.StateDir)
	if errors.Is(err, ErrLocked) {
		s.mu.Unlock()
		s.logf("webhook 拒绝：外部更新进程持有锁 tag=%s delivery=%s", tag, delivery)
		http.Error(w, "update in progress", http.StatusConflict)
		return
	}
	if err != nil {
		s.mu.Unlock()
		s.logf("webhook 拒绝：获取更新锁失败 delivery=%s: %v", delivery, err)
		http.Error(w, "updater unavailable", http.StatusInternalServerError)
		return
	}
	s.busy = true
	s.mu.Unlock()

	w.WriteHeader(http.StatusAccepted)
	go s.runUpdate(tag, delivery, lock)
}

// runUpdate 后台单飞执行。锁由 handleWebhook 在受理时取得，这里负责释放；
// 失败不记 delivery，允许重投重试。
func (s *Server) runUpdate(tag, delivery string, lock *Lock) {
	defer func() {
		lock.Release()
		s.mu.Lock()
		s.busy = false
		s.mu.Unlock()
	}()
	s.logf("更新开始 tag=%s delivery=%s", tag, delivery)
	// 走 runOnce：锁已在受理时持有，再调 RunOnce 会自己撞 ErrLocked。
	res, err := s.up.runOnce(context.Background(), tag, delivery)
	if err != nil {
		s.logf("更新失败 tag=%s 回滚=%v 耗时=%s err=%v", tag, res.RolledBack, res.Duration.Round(time.Millisecond), err)
		return
	}
	s.logf("更新成功 tag=%s 旧版本=%s 校验和=%s 耗时=%s", tag, res.OldVersion, res.Checksum, res.Duration.Round(time.Millisecond))
}

// validSignature 校验 X-Hub-Signature-256（HMAC-SHA256，常量时间比较）。
func validSignature(secret []byte, header string, body []byte) bool {
	const prefix = "sha256="
	if !strings.HasPrefix(header, prefix) {
		return false
	}
	want, err := hex.DecodeString(header[len(prefix):])
	if err != nil || len(want) != sha256.Size {
		return false
	}
	mac := hmac.New(sha256.New, secret)
	mac.Write(body)
	return hmac.Equal(mac.Sum(nil), want)
}

// noteInvalid 记录一次签名失败，返回该 IP 是否仍在窗口配额内（false → 429）。
// 窗口自然过期后重新计数；每个窗口最多全扫一次过期条目，防公网扫描把 map 撑爆。
// 超限后计数停在阈值 +1，不随攻击量无界增长。
func (s *Server) noteInvalid(ip string) bool {
	now := s.now()
	s.invalidMu.Lock()
	defer s.invalidMu.Unlock()
	if now.Sub(s.lastSweep) >= invalidWindowSize {
		for k, w := range s.invalid {
			if now.Sub(w.start) >= invalidWindowSize {
				delete(s.invalid, k)
			}
		}
		s.lastSweep = now
	}
	w := s.invalid[ip]
	if w.start.IsZero() || now.Sub(w.start) >= invalidWindowSize {
		w = invalidCounter{start: now}
	}
	if w.count <= invalidMaxPerIP {
		w.count++
	}
	s.invalid[ip] = w
	return w.count <= invalidMaxPerIP
}

// clientIP 从 RemoteAddr 取 host（失败时用原值）。
// 不信任 X-Forwarded-For：8088 直接对公网监听，该头可伪造，按它限流等于没有限流。
func clientIP(remoteAddr string) string {
	host, _, err := net.SplitHostPort(remoteAddr)
	if err != nil {
		return remoteAddr
	}
	return host
}

// isLoopback RemoteAddr 是否环回（/status 用）。
func isLoopback(remoteAddr string) bool {
	ip := net.ParseIP(clientIP(remoteAddr))
	return ip != nil && ip.IsLoopback()
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
