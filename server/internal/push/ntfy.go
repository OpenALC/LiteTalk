package push

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"time"
)

// Ntfy 基于 ntfy 的 JSON publish（POST <URL 根> + {"topic": ...}）。
// 开源自托管最友好的通道：设备注册时把 ntfy topic 当 push_token 上报，
// 服务端逐设备投递，无需任何厂商凭据。URL 是 ntfy 服务器根（如 https://ntfy.sh）。
type Ntfy struct {
	URL    string       // ntfy 服务器根 URL
	Log    *log.Logger  // 可空；只记投递失败，绝不打印 token（topic 打码）
	Client *http.Client // nil 时用默认 10s 超时；测试注入 httptest 桩
	base   string       // 测试覆盖用（空 = 用 URL）
}

// ntfyPayload ntfy JSON publish 请求体（仅用官方支持的字段）。
type ntfyPayload struct {
	Topic   string `json:"topic"`   // 设备的 push_token（ntfy 订阅主题）
	Title   string `json:"title"`   // 事实性标题，无消息正文
	Message string `json:"message"` // body，同红线
	Click   string `json:"click"`   // 客户端路由提示
}

// Push 逐设备 POST JSON publish。2xx 视为成功；一机失败不影响其余设备。
func (n *Ntfy) Push(ctx context.Context, devices []Device, notif Notification) error {
	for _, d := range devices {
		if d.Token == "" {
			continue
		}
		if err := n.pushOne(ctx, d, notif); err != nil {
			safeLog(n.Log).Printf("[push/ntfy] 设备 %s 投递失败: %v", MaskToken(d.Token), err)
		}
	}
	return nil
}

func (n *Ntfy) pushOne(ctx context.Context, d Device, notif Notification) error {
	url := n.base
	if url == "" {
		url = n.URL
	}
	payload, err := json.Marshal(ntfyPayload{
		Topic:   d.Token,
		Title:   notif.Title,
		Message: notif.Body,
		Click:   notif.Route,
	})
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(payload))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	cli := n.Client
	if cli == nil {
		cli = &http.Client{Timeout: 10 * time.Second}
	}
	resp, err := cli.Do(req)
	if err != nil {
		return fmt.Errorf("ntfy: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return fmt.Errorf("ntfy: HTTP %d", resp.StatusCode)
	}
	return nil
}
