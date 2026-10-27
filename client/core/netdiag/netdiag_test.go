package netdiag

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/coder/websocket"
	"github.com/pion/stun/v4"
	"github.com/pion/turn/v5"
)

// fakeSTUNServer 本地 STUN binding 服务：回显 XOR-MAPPED-ADDRESS，避免测试
// 依赖公网 STUN 的稳定性（#49 C）。
func fakeSTUNServer(t *testing.T) string {
	t.Helper()
	conn, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatalf("listen udp: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	go func() {
		buf := make([]byte, 1500)
		for {
			n, from, err := conn.ReadFromUDP(buf)
			if err != nil {
				return
			}
			msg := &stun.Message{Raw: append([]byte(nil), buf[:n]...)}
			if msg.Decode() != nil || msg.Type != stun.BindingRequest {
				continue
			}
			resp := stun.MustBuild(
				stun.NewTransactionIDSetter(msg.TransactionID),
				stun.BindingSuccess,
				&stun.XORMappedAddress{IP: from.IP, Port: from.Port},
			)
			_, _ = conn.WriteToUDP(resp.Raw, from)
		}
	}()
	return conn.LocalAddr().String()
}

// blackholeUDP 收包但永不回复，用于超时路径。
func blackholeUDP(t *testing.T) string {
	t.Helper()
	conn, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatalf("listen udp: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	go func() {
		buf := make([]byte, 64)
		for {
			if _, _, err := conn.ReadFromUDP(buf); err != nil {
				return
			}
		}
	}()
	return conn.LocalAddr().String()
}

func wsURLOf(srv *httptest.Server) string {
	return "ws" + strings.TrimPrefix(srv.URL, "http") + "/ws"
}

// TestRunAllProbes REST/WS/STUN 全通、TURN 未配置：整体 OK，TURN 标 skipped。
func TestRunAllProbes(t *testing.T) {
	stunAddr := fakeSTUNServer(t)
	var dialed int
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/healthz", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"ok","version":"v9.9.9-test"}`))
	})
	mux.HandleFunc("/ws", func(w http.ResponseWriter, r *http.Request) {
		dialed++
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		_ = conn.Close(websocket.StatusNormalClosure, "probe")
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	res := Run(context.Background(), Config{
		BaseURL: srv.URL,
		WSURL:   wsURLOf(srv),
		Token:   "tok",
		ICEServers: []ICEServer{
			{URLs: []string{"stun:" + stunAddr}},
		},
		Timeout: 2 * time.Second,
	})
	if !res.OK {
		t.Fatalf("整体诊断应通过: %+v", res)
	}
	if !res.REST.OK || res.REST.Version != "v9.9.9-test" {
		t.Fatalf("REST 探测错误: %+v", res.REST)
	}
	if !res.WS.OK || res.WS.Skipped {
		t.Fatalf("WS 探测错误: %+v", res.WS)
	}
	if dialed == 0 {
		t.Fatal("WS 未实际拨号")
	}
	if !res.STUN.OK || res.STUN.Address == "" {
		t.Fatalf("STUN 探测错误: %+v", res.STUN)
	}
	if !res.TURN.Skipped {
		t.Fatalf("未配置 TURN 应标 skipped: %+v", res.TURN)
	}
}

// TestWSConnectedSkipsDial 已有活跃信令时不得再拨号（同 token 会被服务端按
// 同 sid 顶替，踢掉正在使用的连接）；现有连接即证明可达，按成功上报。
func TestWSConnectedSkipsDial(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("不应发生 WS 拨号: %s", r.URL.Path)
	}))
	defer srv.Close()

	res := probeSignal(context.Background(), Config{
		WSURL:       wsURLOf(srv),
		Token:       "tok",
		WSConnected: true,
	}, time.Second)
	if !res.OK || res.Skipped || res.Note == "" {
		t.Fatalf("已连接时应报成功并注明未拨号: %+v", res)
	}
}

// TestFailuresAreIndependent 各项独立失败，不互相拖累；整体 OK=false。
func TestFailuresAreIndependent(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/healthz", func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	})
	mux.HandleFunc("/ws", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	res := Run(context.Background(), Config{
		BaseURL: srv.URL,
		WSURL:   wsURLOf(srv),
		Timeout: time.Second,
	})
	if res.OK {
		t.Fatalf("REST/WS 均失败时整体不应通过: %+v", res)
	}
	if res.REST.OK || res.REST.Error == "" {
		t.Fatalf("REST 应失败并带错误: %+v", res.REST)
	}
	if res.WS.OK || res.WS.Error == "" {
		t.Fatalf("WS 应失败并带错误: %+v", res.WS)
	}
	if !res.STUN.Skipped || !res.TURN.Skipped {
		t.Fatalf("无 ICE 配置应为 skipped: stun=%+v turn=%+v", res.STUN, res.TURN)
	}
}

// TestSTUNTimeout 对端不回包时按超时失败，不挂死。
func TestSTUNTimeout(t *testing.T) {
	addr := blackholeUDP(t)
	start := time.Now()
	res := probeSTUNConfig(context.Background(), []ICEServer{{URLs: []string{"stun:" + addr}}}, 300*time.Millisecond)
	if res.OK || res.Error == "" {
		t.Fatalf("无响应应失败: %+v", res)
	}
	if !strings.Contains(res.Error, "超时") {
		t.Fatalf("错误应为超时: %q", res.Error)
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("STUN 超时未受控: %v", elapsed)
	}
}

func TestSTUNUnconfigured(t *testing.T) {
	if res := probeSTUNConfig(context.Background(), nil, time.Second); !res.Skipped {
		t.Fatalf("无 STUN 配置应 skipped: %+v", res)
	}
	// 只有 stuns:：明确报不支持，而不是当作未配置。
	res := probeSTUNConfig(context.Background(), []ICEServer{{URLs: []string{"stuns:stun.example.com:5349"}}}, time.Second)
	if res.Skipped || res.OK || !strings.Contains(res.Error, "stuns") {
		t.Fatalf("stuns: 应报不支持: %+v", res)
	}
}

func TestTURNUnconfiguredAndUnsupported(t *testing.T) {
	if res := probeTURNConfig(context.Background(), nil, time.Second); !res.Skipped {
		t.Fatalf("无 TURN 配置应 skipped: %+v", res)
	}
	noCred := []ICEServer{{URLs: []string{"turn:turn.example.com:3478"}}}
	if res := probeTURNConfig(context.Background(), noCred, time.Second); !res.Skipped || res.Note == "" {
		t.Fatalf("无凭证应 skipped 并注明: %+v", res)
	}
	withCred := []ICEServer{{
		URLs:       []string{"turn:turn.example.com:3478?transport=tcp"},
		Username:   "user",
		Credential: "pass",
	}}
	res := probeTURNConfig(context.Background(), withCred, time.Second)
	if res.Skipped || res.OK || !strings.Contains(res.Error, "tcp") {
		t.Fatalf("TCP TURN 应明确报不支持: %+v", res)
	}
	tlsCred := []ICEServer{{
		URLs:       []string{"turns:turn.example.com:5349"},
		Username:   "user",
		Credential: "pass",
	}}
	res = probeTURNConfig(context.Background(), tlsCred, time.Second)
	if res.Skipped || res.OK || !strings.Contains(res.Error, "turns") {
		t.Fatalf("turns: 应明确报不支持: %+v", res)
	}
}

// TestTURNAllocateSuccess 用本地 pion TURN 服务验证 allocate 成功路径，
// 不依赖公网 TURN。
func TestTURNAllocateSuccess(t *testing.T) {
	pc, err := net.ListenPacket("udp4", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen packet: %v", err)
	}
	const realm = "litetalk-test"
	srv, err := turn.NewServer(turn.ServerConfig{
		Realm: realm,
		AuthHandler: func(ra *turn.RequestAttributes) (string, []byte, bool) {
			if ra.Username != "agent" {
				return "", nil, false
			}
			return ra.Username, turn.GenerateAuthKey(ra.Username, ra.Realm, "pass-1"), true
		},
		PacketConnConfigs: []turn.PacketConnConfig{{
			PacketConn: pc,
			RelayAddressGenerator: &turn.RelayAddressGeneratorStatic{
				RelayAddress: net.ParseIP("127.0.0.1"),
				Address:      "127.0.0.1",
			},
		}},
		LoggerFactory: quietLoggerFactory(),
	})
	if err != nil {
		_ = pc.Close()
		t.Fatalf("turn server: %v", err)
	}
	t.Cleanup(func() {
		_ = srv.Close()
		_ = pc.Close()
	})

	res := probeTURNConfig(context.Background(), []ICEServer{{
		URLs:       []string{"turn:" + pc.LocalAddr().String()},
		Username:   "agent",
		Credential: "pass-1",
	}}, 3*time.Second)
	if !res.OK || res.Address == "" {
		t.Fatalf("TURN allocate 应成功: %+v", res)
	}
}

// TestTURNDialFailure UDP 端口无人监听：快速失败且不泄漏凭证。
func TestTURNDialFailure(t *testing.T) {
	conn, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatalf("listen udp: %v", err)
	}
	addr := conn.LocalAddr().String()
	_ = conn.Close() // 端口关闭：allocate 收不到响应

	servers := []ICEServer{{
		URLs:       []string{"turn:" + addr},
		Username:   "agent-user",
		Credential: "super-secret-credential",
	}}
	start := time.Now()
	res := probeTURNConfig(context.Background(), servers, 400*time.Millisecond)
	if res.OK || res.Error == "" {
		t.Fatalf("不可达 TURN 应失败: %+v", res)
	}
	if strings.Contains(res.Error, "super-secret-credential") ||
		strings.Contains(res.Error, "agent-user") {
		t.Fatalf("错误信息不得泄露凭证: %q", res.Error)
	}
	if elapsed := time.Since(start); elapsed > 3*time.Second {
		t.Fatalf("TURN 探测超时未受控: %v", elapsed)
	}
}

func TestHealthzFailures(t *testing.T) {
	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "down", http.StatusServiceUnavailable)
	}))
	defer bad.Close()
	if res := Healthz(context.Background(), bad.URL, time.Second); res.OK || !strings.Contains(res.Error, "503") {
		t.Fatalf("503 应失败并带状态码: %+v", res)
	}

	// 可达但响应不是 JSON：仍算连通（与设置页「测试连接」行为一致）。
	weird := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("not json"))
	}))
	defer weird.Close()
	if res := Healthz(context.Background(), weird.URL, time.Second); !res.OK {
		t.Fatalf("响应格式异常仍应算连通: %+v", res)
	}

	// 非法地址立即失败。
	if res := Healthz(context.Background(), "not-a-url", time.Second); res.OK || res.Error == "" {
		t.Fatalf("非法地址应快速失败: %+v", res)
	}
}

func TestSanitize(t *testing.T) {
	srv := ICEServer{Username: "user-1", Credential: "pass-1"}
	got := sanitize("auth failed for user-1 with pass-1", srv)
	if strings.Contains(got, "user-1") || strings.Contains(got, "pass-1") {
		t.Fatalf("脱敏失败: %q", got)
	}
	long := sanitize(strings.Repeat("x", 400), srv)
	if utf8.RuneCountInString(long) > 161 {
		t.Fatalf("错误摘要应截断: %d", utf8.RuneCountInString(long))
	}
	// 中文截断不能切出半个 UTF-8 序列。
	cn := sanitize(strings.Repeat("网络诊断", 100), srv)
	if !utf8.ValidString(cn) || utf8.RuneCountInString(cn) > 161 {
		t.Fatalf("中文摘要截断损坏: %q", cn)
	}
}
