// rejoin 重入房场景（#23）：两用户进房互通 RTP → A 离房再进房 → B 必须恢复收听
// （新 OnTrack + 该路包计数继续增长）。
//
//	go run ./cmd/e2eprobe -addr 127.0.0.1:8077 -rejoin -loopback
//
// B 侧在 channel.peer_left 时拆掉 A 的读取循环（真实客户端 remote.RemovePeer
// 语义）——SFU 若惰性保留旧下行轨道（#23 根因），A 重入的 RTP 进旧轨道、
// B 拿不到新 OnTrack，计数冻结，本场景判 FAIL。
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strconv"
	"time"

	"github.com/OpenALC/LiteTalk/server/internal/signal"
	"github.com/pion/webrtc/v4"
)

// recvCount 某 src 的下行包计数（重入房前后对账用）。
func (u *probeUser) recvCount(src string) int64 {
	u.recvMu.Lock()
	defer u.recvMu.Unlock()
	if c, ok := u.recv[src]; ok {
		return c.Load()
	}
	return 0
}

// setupVoicePair 注册 A/B → A 建服务器与语音频道并发邀请 → B 凭码入服。
// 返回频道 ID 与双方探测端（已拨号 WS、未入房）。
func setupVoicePair(ctx context.Context, cfg *config) (room string, uA, uB *probeUser, err error) {
	a, err := registerTagged(cfg.addr, "重入A")
	if err != nil {
		return "", nil, nil, fmt.Errorf("注册 A: %w", err)
	}
	b, err := registerTagged(cfg.addr, "重入B")
	if err != nil {
		return "", nil, nil, fmt.Errorf("注册 B: %w", err)
	}
	code, body, err := restPost(cfg.addr, "/servers", a.token, map[string]string{"name": "重入探测"})
	if err != nil || (code != 200 && code != 201) {
		return "", nil, nil, fmt.Errorf("建服务器: code=%d err=%v body=%s", code, err, body)
	}
	srv := struct {
		ID jsonID `json:"id"`
	}{}
	if err = json.Unmarshal(body, &srv); err != nil || srv.ID == 0 {
		return "", nil, nil, fmt.Errorf("server id 解析: %s", body)
	}
	code, body, err = restPost(cfg.addr, fmt.Sprintf("/servers/%d/channels", srv.ID), a.token,
		map[string]string{"name": "语音", "type": "voice"})
	if err != nil || (code != 200 && code != 201) {
		return "", nil, nil, fmt.Errorf("建语音频道: code=%d err=%v body=%s", code, err, body)
	}
	ch := struct {
		ID jsonID `json:"id"`
	}{}
	if err = json.Unmarshal(body, &ch); err != nil || ch.ID == 0 {
		return "", nil, nil, fmt.Errorf("channel id 解析: %s", body)
	}
	room = ch.ID.String()

	// A 发邀请 → B 凭码入服（进房权限要求服务器成员）。
	code, body, err = restPost(cfg.addr, fmt.Sprintf("/servers/%d/invites", srv.ID), a.token,
		map[string]any{"max_uses": 5})
	if err != nil || (code != 200 && code != 201) {
		return "", nil, nil, fmt.Errorf("建邀请: code=%d err=%v body=%s", code, err, body)
	}
	inv := struct {
		Code string `json:"code"`
	}{}
	if err = json.Unmarshal(body, &inv); err != nil || inv.Code == "" {
		return "", nil, nil, fmt.Errorf("邀请码解析: %s", body)
	}
	if code, body, err = restPost(cfg.addr, "/servers/join", b.token, map[string]string{"code": inv.Code}); err != nil || code != 200 {
		return "", nil, nil, fmt.Errorf("B 入服: code=%d err=%v body=%s", code, err, body)
	}

	uidA, uidB := strconv.FormatInt(a.id, 10), strconv.FormatInt(b.id, 10)
	uA = newUser(cfg, room, "rejoin-A", a.token, nil, []string{uidB})
	uB = newUser(cfg, room, "rejoin-B", b.token, nil, []string{uidA})
	if err = uA.dial(ctx); err != nil {
		return "", nil, nil, err
	}
	go uA.readLoop(ctx)
	if err = uB.dial(ctx); err != nil {
		return "", nil, nil, err
	}
	go uB.readLoop(ctx)
	return room, uA, uB, nil
}

// joinAndWait 入房 + 建轨 + 等 connected（A/B 两侧共用）。
func joinAndWait(ctx context.Context, u *probeUser, timeout time.Duration) error {
	if err := u.wsSend(signal.TypeChannelJoin, signal.ChannelJoinData{ChannelID: u.room}); err != nil {
		return fmt.Errorf("%s channel.join: %w", u.id, err)
	}
	if err := u.startPeer(); err != nil {
		return fmt.Errorf("%s 建轨: %w", u.id, err)
	}
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if u.pc != nil && u.pc.ConnectionState() == webrtc.PeerConnectionStateConnected {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(50 * time.Millisecond):
		}
	}
	return fmt.Errorf("%s 等 connected 超时（%s）", u.id, u.srvErr())
}

// runRejoin -rejoin 场景主流程。返回进程退出码。
func runRejoin(ctx context.Context, cfg *config) int {
	fail := func(format string, a ...any) int {
		fmt.Fprintf(os.Stderr, "结论: FAIL——"+format+"\n", a...)
		return 1
	}
	wait := func(d time.Duration, cond func() bool) bool { return waitUntil(ctx, d, cond) }

	fmt.Printf("重入房探测 %s：离房再入房，B 须恢复收听 A\n", cfg.addr)
	room, uA, uB, err := setupVoicePair(ctx, cfg)
	if err != nil {
		return fail("%v", err)
	}
	defer uA.close()
	defer uB.close()
	// B 侧 A 的计数键 = A 的服务端 uid（下行轨 TrackID=audio-<uid>），唯一 other。
	srcA := ""
	for k := range uB.recv {
		srcA = k
	}

	fmt.Println("阶段 1/3：双方入房互通…")
	if err := joinAndWait(ctx, uA, 15*time.Second); err != nil {
		return fail("%v", err)
	}
	if err := joinAndWait(ctx, uB, 15*time.Second); err != nil {
		return fail("%v", err)
	}
	stopA := make(chan struct{})
	go uA.sendLoop(stopA)
	if !wait(15*time.Second, func() bool { return uB.recvCount(srcA) > 0 }) {
		return fail("首轮 B 未收到 A 的 RTP（baseline src=%s）", srcA)
	}
	fmt.Println("  首轮互通 OK：B 收到 A 的 RTP")

	fmt.Println("阶段 2/3：A 离房…")
	close(stopA)
	if err := uA.wsSend(signal.TypeChannelLeave, signal.ChannelJoinData{ChannelID: room}); err != nil {
		return fail("A channel.leave: %v", err)
	}
	uA.closePC() // 真实客户端离房顺序：先 leave 再拆 PC
	if !wait(10*time.Second, func() bool { return uB.peerLeftN.Load() >= 1 }) {
		return fail("B 未收到 channel.peer_left（%s）", uB.srvErr())
	}
	time.Sleep(300 * time.Millisecond) // 读取循环拆除生效窗口
	base := uB.recvCount(srcA)

	fmt.Println("阶段 3/3：A 重入房并发包，B 必须恢复收听…")
	if err := joinAndWait(ctx, uA, 15*time.Second); err != nil {
		return fail("A 重入: %v", err)
	}
	stopA2 := make(chan struct{})
	defer close(stopA2)
	go uA.sendLoop(stopA2)
	if !wait(15*time.Second, func() bool { return uB.recvCount(srcA) > base }) {
		return fail("A 重入后 B 未恢复收听：B 侧计数冻结在 %d（旧下行轨道惰性保留 / 无新 OnTrack，#23），B tracksUp=%d",
			base, uB.tracksUp.Load())
	}
	if got := uB.tracksUp.Load(); got != 2 {
		return fail("A 重入后 B 应有 2 次 OnTrack，实际 %d（#23）", got)
	}

	fmt.Println("\n==== 重入房探测汇总 ====")
	fmt.Printf("首轮互通: B recv=%d ✓\n", base)
	fmt.Printf("重入恢复: B recv=%d tracksUp=%d ✓\n", uB.recvCount(srcA), uB.tracksUp.Load())
	fmt.Println("结论: PASS——离房摘轨 + 重入建轨正常")
	return 0
}
