// presence 在线状态场景（#11）：注册 A/B → 互加好友 → A 连 WS（上线）→
// B 侧 REST 好友列表可见 online=true → A 断开 → online 翻 false。
//
//	go run ./cmd/e2eprobe -addr 127.0.0.1:8077 -presence
//
// 断开走正常关闭：cleanup 会立即 DEL presence:{uid}（秒级离线）。
// 异常掉线（进程被杀/断网）才走 TTL 过期（90s）路径，探针无法在传输层制造，
// 该路径由 signal 包单测（心跳续期 + TTL）覆盖。
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/coder/websocket"
)

// probePair 一对探测身份：注册响应里的昵称与 tag（加好友要用 昵称#tag）。
type probePair struct {
	id    int64
	nick  string
	tag   string
	token string
}

// registerTagged 注册并返回含 tag 的身份（simple/full 模式都走 register；full 需验证码，
// 探针假定服务端为 simple 或已配免验证码）。邮箱唯一，撞库即报错。
func registerTagged(addr, nickname string) (*probePair, error) {
	email := fmt.Sprintf("probe%d@example.com", time.Now().UnixNano())
	raw, _ := json.Marshal(map[string]string{
		"nickname": nickname, "email": email, "password": "probe1234",
	})
	req, err := http.NewRequest(http.MethodPost, "http://"+addr+"/api/v1/auth/register", bytes.NewReader(raw))
	if err != nil {
		return nil, fmt.Errorf("register 请求: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(clientVersionHeader, probeClientVersion)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("register 请求: %w", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusCreated {
		return nil, fmt.Errorf("register HTTP %d: %s", resp.StatusCode, body)
	}
	var out struct {
		AccessToken string `json:"access_token"`
		User        struct {
			ID       jsonID `json:"id"`
			Nickname string `json:"nickname"`
			Tag      string `json:"tag"`
		} `json:"user"`
	}
	if err := json.Unmarshal(body, &out); err != nil || out.AccessToken == "" {
		return nil, fmt.Errorf("register 响应解析失败: %s", body)
	}
	return &probePair{
		id: int64(out.User.ID), nick: out.User.Nickname, tag: out.User.Tag, token: out.AccessToken,
	}, nil
}

// restJSON 带 Bearer 的请求小工具；返回状态码与解析出的 JSON 顶层对象/数组。
func restJSON(method, addr, path, token string, body any) (int, []byte, error) {
	var rdr io.Reader
	if body != nil {
		raw, _ := json.Marshal(body)
		rdr = bytes.NewReader(raw)
	}
	req, err := http.NewRequest(method, "http://"+addr+path, rdr)
	if err != nil {
		return 0, nil, err
	}
	req.Header.Set(clientVersionHeader, probeClientVersion)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, b, nil
}

// friendOnline 查 B 视角下 target 的 online（GET /friends 找 user.id 匹配项）。
// 找不到返回 (false, false)——好友行缺失也算失败信号。
func friendOnline(addr, token string, target int64) (online, found bool, err error) {
	code, b, err := restJSON("GET", addr, "/api/v1/friends", token, nil)
	if err != nil {
		return false, false, err
	}
	if code != http.StatusOK {
		return false, false, fmt.Errorf("friends HTTP %d: %s", code, b)
	}
	var list []struct {
		ID   jsonID `json:"id"`
		User struct {
			ID     jsonID `json:"id"`
			Online bool   `json:"online"`
		} `json:"user"`
	}
	if err := json.Unmarshal(b, &list); err != nil {
		return false, false, fmt.Errorf("friends 解析: %w (%s)", err, b)
	}
	for _, f := range list {
		if int64(f.User.ID) == target {
			return f.User.Online, true, nil
		}
	}
	return false, false, nil
}

// waitOnline 轮询 target 的 online 直到与 want 一致或超时。
func waitOnline(addr, token string, target int64, want bool, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		online, found, err := friendOnline(addr, token, target)
		if err == nil && found && online == want {
			return nil
		}
		time.Sleep(500 * time.Millisecond)
	}
	return fmt.Errorf("等待 online=%v 超时（%s）", want, timeout)
}

// runPresence -presence 场景主流程。返回进程退出码。
func runPresence(ctx context.Context, cfg *config) int {
	fmt.Printf("E2E 探测 %s：在线状态（presence）场景\n", cfg.addr)
	a, err := registerTagged(cfg.addr, "在场A")
	if err != nil {
		fmt.Println("注册 A 失败:", err)
		return 1
	}
	b, err := registerTagged(cfg.addr, "在场B")
	if err != nil {
		fmt.Println("注册 B 失败:", err)
		return 1
	}

	// B 加 A（昵称#tag）→ A 接受。
	code, body, err := restJSON("POST", cfg.addr, "/api/v1/friends/requests", b.token,
		map[string]string{"nickname": fmt.Sprintf("%s#%s", a.nick, a.tag)})
	if err != nil || code != http.StatusCreated {
		fmt.Printf("好友请求失败: code=%d err=%v body=%s\n", code, err, body)
		return 1
	}
	var fr struct {
		ID jsonID `json:"id"`
	}
	_ = json.Unmarshal(body, &fr)
	code, body, err = restJSON("POST", cfg.addr,
		fmt.Sprintf("/api/v1/friends/requests/%s/accept", fr.ID), a.token, nil)
	if err != nil || code != http.StatusOK {
		fmt.Printf("接受好友失败: code=%d err=%v body=%s\n", code, err, body)
		return 1
	}

	// A 连 WS = 上线。
	conn, _, err := websocket.Dial(ctx,
		fmt.Sprintf("ws://%s/ws?token=%s", cfg.addr, a.token), nil)
	if err != nil {
		fmt.Println("A WS 拨号失败:", err)
		return 1
	}
	if err := waitOnline(cfg.addr, b.token, a.id, true, 5*time.Second); err != nil {
		fmt.Println("上线后 online 未翻 true:", err)
		_ = conn.CloseNow()
		return 1
	}
	fmt.Println("A 上线：B 侧 friends.user.online = true")

	// A 断开 = 下线（cleanup 立即 DEL；异常掉线才走 90s TTL）。
	_ = conn.Close(websocket.StatusNormalClosure, "done")
	if err := waitOnline(cfg.addr, b.token, a.id, false, 5*time.Second); err != nil {
		fmt.Println("断开后 online 未翻 false:", err)
		return 1
	}
	fmt.Println("A 断开：B 侧 friends.user.online = false")
	fmt.Println("presence 场景通过")
	return 0
}
