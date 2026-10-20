package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"time"

	"github.com/OpenALC/LiteTalk/server/internal/signal"
)

// T4（srv-hub）在 server/internal/signal/protocol.go 落库 TypeSessionKicked /
// TypeSessionVoiceTaken 前，本探针按契约 §0.5 以字面值引用；落库后可换常量，
// 字面值必须与服务端一致。
const (
	typeSessionKicked     = "session.kicked"      // §0.5 S→C 同类设备新登录踢线（随后 close 4001）
	typeSessionVoiceTaken = "session.voice_taken" // §0.5 S→C 同账号另一会话抢占语音，本端本地退房
)

// 契约 §0.6 close code：4001 = 同类设备新登录。
const closeCodeLoginElsewhere = 4001

const (
	kickWait      = 8 * time.Second  // 踢线消息 + 4001 关闭的等待上限（发送窗口 300~500ms）
	coexistWindow = 5 * time.Second  // 异 device 共存观察窗
	voiceWait     = 8 * time.Second  // voice_taken 等待上限
	markWait      = 10 * time.Second // hello / channel.joined 等待上限
)

// runMultiDevice 多设备会话全链路（契约 §0.4~§0.6、§0.10）：
//  1. 注册 → 按 device=desktop 两次登录拿不同 sid 的 token；
//  2. 双 WS 同 device：第二个上线后第一个收 session.kicked 并以 4001 关闭；
//  3. 异 device（desktop/mobile）双 WS 共存 5s 不互踢；
//  4. desktop 先入语音房，mobile 进房抢占 → desktop 收 session.voice_taken。
//
// 走真实服务端（REST + WS），退出码 0=PASS / 1=FAIL。
func runMultiDevice(ctx context.Context, cfg *config) int {
	fail := func(format string, a ...any) int {
		fmt.Fprintf(os.Stderr, "结论: FAIL——"+format+"\n", a...)
		return 1
	}
	wait := func(d time.Duration, cond func() bool) bool { return waitUntil(ctx, d, cond) }

	fmt.Printf("多设备探测 %s：同设备踢线 + 异设备共存 + 语音抢占\n", cfg.addr)

	fmt.Println("阶段 1/4：注册 + 同 device 双登录…")
	email, _, _, _, err := register(cfg.addr, "多端探测")
	if err != nil {
		return fail("注册: %v", err)
	}
	tokenA, err := loginDevice(cfg.addr, email, "desktop")
	if err != nil {
		return fail("首次登录: %v", err)
	}
	uA := newUser(cfg, "", "ws-A", tokenA, nil, nil)
	if err := uA.dial(ctx); err != nil {
		return fail("%v", err)
	}
	go uA.readLoop(ctx)
	defer uA.close()
	// hello 到达表示服务端已完成该连接的注册（T4 注册先于/紧随 hello）；
	// 再留 300ms 余量，避免第二个连接先于第一个注册导致互踢方向颠倒。
	if !wait(markWait, uA.gotHello.Load) {
		return fail("ws-A 未收到 hello（%s）", uA.srvErr())
	}
	time.Sleep(300 * time.Millisecond)

	// 第二次登录：同 device 换新 sid（契约 §0.4 覆盖即吊销旧会话），
	// 使两条 WS 属于同 device、不同 sid → 走 4001 踢线而非 4000 同会话替换。
	tokenB, err := loginDevice(cfg.addr, email, "desktop")
	if err != nil {
		return fail("二次登录: %v", err)
	}
	uB := newUser(cfg, "", "ws-B", tokenB, nil, nil)
	if err := uB.dial(ctx); err != nil {
		return fail("%v", err)
	}
	go uB.readLoop(ctx)
	defer uB.close()

	fmt.Println("阶段 2/4：同 device 第二连上线，期望 ws-A 收 session.kicked + close 4001…")
	if !wait(kickWait, uA.gotKicked.Load) {
		return fail("ws-A 未收到 session.kicked（%s）", uA.srvErr())
	}
	if !wait(kickWait, func() bool { return uA.closeCode.Load() == closeCodeLoginElsewhere }) {
		return fail("ws-A 关闭码 = %d，want %d", uA.closeCode.Load(), closeCodeLoginElsewhere)
	}
	if !wait(markWait, uB.gotHello.Load) {
		return fail("ws-B 未收到 hello（%s）", uB.srvErr())
	}
	if !alive(uB) {
		return fail("ws-B 被连带关闭（closeCode=%d）", uB.closeCode.Load())
	}
	fmt.Println("  踢线 OK：ws-A 收 kicked 并以 4001 关闭，ws-B 存活")

	// 结束 ws-B（干净 1000 释放 desktop 槽位；tokenB 的会话仍是当前 desktop 会话）。
	uB.close()
	if !wait(markWait, func() bool {
		select {
		case <-uB.readDone:
			return true
		default:
			return false
		}
	}) {
		return fail("ws-B 主动关闭后 readLoop 未退出")
	}

	fmt.Println("阶段 3/4：异 device（desktop/mobile）双连共存 5s…")
	tokenM, err := loginDevice(cfg.addr, email, "mobile")
	if err != nil {
		return fail("mobile 登录: %v", err)
	}
	// desktop 侧沿用 tokenB：sid 仍是当前 desktop 会话（mobile 登录只覆盖 (uid,mobile) 键）。
	uD := newUser(cfg, "", "ws-desktop", tokenB, nil, nil)
	if err := uD.dial(ctx); err != nil {
		return fail("%v", err)
	}
	go uD.readLoop(ctx)
	defer uD.close()
	uM := newUser(cfg, "", "ws-mobile", tokenM, nil, nil)
	if err := uM.dial(ctx); err != nil {
		return fail("%v", err)
	}
	go uM.readLoop(ctx)
	defer uM.close()
	if !wait(markWait, uD.gotHello.Load) {
		return fail("ws-desktop 未收到 hello（%s）", uD.srvErr())
	}
	if !wait(markWait, uM.gotHello.Load) {
		return fail("ws-mobile 未收到 hello（%s）", uM.srvErr())
	}
	time.Sleep(coexistWindow)
	if !alive(uD) {
		return fail("共存期 ws-desktop 被踢（kicked=%v closeCode=%d）",
			uD.gotKicked.Load(), uD.closeCode.Load())
	}
	if !alive(uM) {
		return fail("共存期 ws-mobile 被踢（kicked=%v closeCode=%d）",
			uM.gotKicked.Load(), uM.closeCode.Load())
	}
	fmt.Println("  共存 OK：desktop/mobile 双连 5s 无互踢")

	fmt.Println("阶段 4/4：desktop 入房 → mobile 抢占 → desktop 收 session.voice_taken…")
	voiceCh, err := createVoiceChannel(cfg.addr, tokenB)
	if err != nil {
		return fail("建服务器/语音频道: %v", err)
	}
	if err := uD.wsSend(signal.TypeChannelJoin, signal.ChannelJoinData{ChannelID: voiceCh}); err != nil {
		return fail("ws-desktop channel.join: %v", err)
	}
	if !wait(markWait, uD.joinedRoom.Load) {
		return fail("ws-desktop 未入房（%s）", uD.srvErr())
	}
	if err := uM.wsSend(signal.TypeChannelJoin, signal.ChannelJoinData{ChannelID: voiceCh}); err != nil {
		return fail("ws-mobile channel.join: %v", err)
	}
	if !wait(markWait, uM.joinedRoom.Load) {
		return fail("ws-mobile 未入房（%s）", uM.srvErr())
	}
	if !wait(voiceWait, uD.gotVoiceTaken.Load) {
		return fail("ws-desktop 未收到 session.voice_taken（%s）", uD.srvErr())
	}
	if !alive(uD) || !alive(uM) {
		return fail("抢占期连接异常（desktop closeCode=%d mobile closeCode=%d）",
			uD.closeCode.Load(), uM.closeCode.Load())
	}

	fmt.Println("\n==== 多设备探测汇总 ====")
	fmt.Println("同 device 踢线: kicked + 4001 ✓")
	fmt.Println("异 device 共存: 5s 无互踢 ✓")
	fmt.Println("语音抢占: voice_taken ✓")
	fmt.Println("结论: PASS——多设备会话全链路正常")
	return 0
}

// loginDevice 按设备登录，返回 access token（每次登录生成新 sid，契约 §0.4）。
func loginDevice(addr, email, device string) (string, error) {
	code, b, err := restPost(addr, "/auth/login", "", map[string]string{
		"email": email, "password": "probe1234", "device": device,
	})
	if err != nil {
		return "", fmt.Errorf("login(%s) 请求: %w", device, err)
	}
	if code != http.StatusOK {
		return "", fmt.Errorf("login(%s) HTTP %d: %s", device, code, b)
	}
	var pair struct {
		AccessToken string `json:"access_token"`
	}
	if err := json.Unmarshal(b, &pair); err != nil || pair.AccessToken == "" {
		return "", fmt.Errorf("login(%s) token 解析失败: %s", device, b)
	}
	return pair.AccessToken, nil
}

// createVoiceChannel 建服务器 + 语音频道，返回数字频道 ID（字符串）。
// 生产模式（Dev=false）只放行真实存在的语音频道，故不能用内置房间名（契约 §0 权限校验）。
func createVoiceChannel(addr, token string) (string, error) {
	code, b, err := restPost(addr, "/servers", token, map[string]string{"name": "多端探测"})
	if err != nil {
		return "", err
	}
	if code != http.StatusOK && code != http.StatusCreated {
		return "", fmt.Errorf("create server HTTP %d: %s", code, b)
	}
	var srv struct {
		ID jsonID `json:"id"`
	}
	if err := json.Unmarshal(b, &srv); err != nil || srv.ID == 0 {
		return "", fmt.Errorf("server id 解析失败: %s", b)
	}
	code, b, err = restPost(addr, fmt.Sprintf("/servers/%d/channels", srv.ID), token,
		map[string]string{"name": "语音", "type": "voice"})
	if err != nil {
		return "", err
	}
	if code != http.StatusOK && code != http.StatusCreated {
		return "", fmt.Errorf("create channel HTTP %d: %s", code, b)
	}
	var ch struct {
		ID jsonID `json:"id"`
	}
	if err := json.Unmarshal(b, &ch); err != nil || ch.ID == 0 {
		return "", fmt.Errorf("channel id 解析失败: %s", b)
	}
	return ch.ID.String(), nil
}

// restPost JSON POST（可带 Bearer），返回状态码与响应体。
func restPost(addr, path, token string, body any) (int, []byte, error) {
	raw, err := json.Marshal(body)
	if err != nil {
		return 0, nil, err
	}
	req, err := http.NewRequest(http.MethodPost, "http://"+addr+"/api/v1"+path, bytes.NewReader(raw))
	if err != nil {
		return 0, nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(clientVersionHeader, probeClientVersion)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := (&http.Client{Timeout: 15 * time.Second}).Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, b, nil
}

// waitUntil 轮询等待条件成立（ctx 取消时提前返回当前结果）。
func waitUntil(ctx context.Context, d time.Duration, cond func() bool) bool {
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if cond() {
			return true
		}
		select {
		case <-ctx.Done():
			return cond()
		case <-time.After(100 * time.Millisecond):
		}
	}
	return cond()
}

// alive 连接仍在读循环中、未收到踢线、无服务端关闭帧。
func alive(u *probeUser) bool {
	select {
	case <-u.readDone:
		return false
	default:
	}
	return !u.gotKicked.Load() && u.closeCode.Load() == 0
}
