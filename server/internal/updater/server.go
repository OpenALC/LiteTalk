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
	"strings"
	"sync"
	"time"
)

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
}

// NewServer 构造 HTTP 面。secret 优先取 cfg.Secret，否则从 SecretFile 读。
// 部署形态是 0640 root:<服务用户>：进程以服务用户运行必须能组读，other 位为 0。
// secret 为空直接报错：无签名密钥时宁可不启动，也不开无鉴权入口。
func NewServer(cfg Config, up *Updater, st *StateStore, logger *log.Logger) (*Server, error) {
	if st == nil {
		return nil, fmt.Errorf("状态存储为空")
	}
	secret := cfg.Secret
	if len(secret) == 0 {
		b, err := os.ReadFile(cfg.SecretFile)
		if err != nil {
			return nil, fmt.Errorf("读 webhook secret 文件: %w", err)
		}
		secret = []byte(strings.TrimSpace(string(b)))
	}
	if len(secret) == 0 {
		return nil, fmt.Errorf("webhook secret 为空")
	}
	return &Server{cfg: cfg, up: up, st: st, log: logger, secret: secret}, nil
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
	s.busy = true
	s.mu.Unlock()

	w.WriteHeader(http.StatusAccepted)
	go s.runUpdate(tag, delivery)
}

// runUpdate 后台单飞执行。结束才释放锁；失败不记 delivery，允许重投重试。
func (s *Server) runUpdate(tag, delivery string) {
	defer func() {
		s.mu.Lock()
		s.busy = false
		s.mu.Unlock()
	}()
	s.logf("更新开始 tag=%s delivery=%s", tag, delivery)
	res, err := s.up.RunOnce(context.Background(), tag, delivery)
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

// isLoopback RemoteAddr 是否环回（/status 用）。
func isLoopback(remoteAddr string) bool {
	host, _, err := net.SplitHostPort(remoteAddr)
	if err != nil {
		host = remoteAddr
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
