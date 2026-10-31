// examplebot 示例机器人（#51 验收脚本）：用 litetalkctl 签发的 bot 令牌连 WS
// 网关（同一条 client.hello 路径，token 即 api_token），加入频道后监听
// chat.message，命中关键字自动回一条。协议类型直接用服务端 internal/signal
// 的生成物——不 import client 模块。
//
// 用法：
//
//	litetalkctl bot create --name 值班机器人   # 拿到一次性 ltk_bot_* 明文
//	LITETALK_BOT_TOKEN=ltk_bot_... LITETALK_CHANNEL=<频道ID> \
//	  go run ./cmd/examplebot -addr 127.0.0.1:8077
//
// 在该频道发一条含关键字（默认 "bot"）的消息，机器人即回帖并打日志。
package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"os"
	ossignal "os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/OpenALC/LiteTalk/server/internal/signal"
	"github.com/coder/websocket"
)

func main() {
	log.SetFlags(log.Ltime)
	cfg := loadConfig(os.Args[1:])
	if cfg.token == "" {
		fmt.Fprintln(os.Stderr, "examplebot: 缺少 bot 令牌（-token 或 LITETALK_BOT_TOKEN）；用 litetalkctl bot create 签发")
		os.Exit(2)
	}

	ctx, stop := ossignal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := run(ctx, cfg); err != nil {
		log.Fatalf("examplebot: %v", err)
	}
}

// config 运行参数：flag > 环境变量 > 默认值。
type config struct {
	addr    string
	token   string
	channel string
	keyword string
	reply   string
	tls     bool
}

func loadConfig(args []string) *config {
	fs := flag.NewFlagSet("examplebot", flag.ContinueOnError)
	cfg := &config{}
	fs.StringVar(&cfg.addr, "addr", envOr("LITETALK_ADDR", "127.0.0.1:8077"), "服务端地址（host[:port]）")
	fs.StringVar(&cfg.token, "token", os.Getenv("LITETALK_BOT_TOKEN"), "bot 令牌明文（ltk_bot_*；缺省读 LITETALK_BOT_TOKEN）")
	fs.StringVar(&cfg.channel, "channel", os.Getenv("LITETALK_CHANNEL"), "要加入的语音频道 ID（可选；文字频道消息无需进房即可收到）")
	fs.StringVar(&cfg.keyword, "keyword", envOr("LITETALK_BOT_KEYWORD", "bot"), "触发回复的关键字（大小写不敏感）")
	fs.StringVar(&cfg.reply, "reply", envOr("LITETALK_BOT_REPLY", "收到，我是 examplebot（自动回复）"), "回复内容")
	fs.BoolVar(&cfg.tls, "tls", false, "使用 wss（域名或前置反代 TLS）")
	if err := fs.Parse(args); err != nil {
		// -h/--help：usage 已打印，按成功退出（验收入口）。
		os.Exit(0)
	}
	return cfg
}

func envOr(key, fallback string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return fallback
}

// run 连接网关 → 协商协议 → 进房 → 事件循环。
func run(ctx context.Context, cfg *config) error {
	scheme := "ws"
	if cfg.tls {
		scheme = "wss"
	}
	url := fmt.Sprintf("%s://%s/ws?token=%s", scheme, cfg.addr, cfg.token)
	conn, _, err := websocket.Dial(ctx, url, nil)
	if err != nil {
		return fmt.Errorf("连网关失败（令牌无效或网络不通）: %w", err)
	}
	defer conn.CloseNow()
	log.Printf("已连接 %s", cfg.addr)

	// 协议协商：与服务端同一份生成物常量。
	hello := signal.ClientHelloData{Min: signal.ProtocolVersionMin, Max: signal.ProtocolVersionMax}
	if err := send(ctx, conn, signal.TypeClientHello, hello); err != nil {
		return err
	}

	var selfID string
	// 下行顺序（服务端 ServeWS）：先遗留 hello（带自己身份，v1 兼容口径恒发），
	// 后 client.hello 协商出的 server.hello——顺序不能反，expect 是严格排队读。
	if err := expect(ctx, conn, signal.TypeHello, func(data json.RawMessage) {
		var h signal.HelloData
		if json.Unmarshal(data, &h) == nil {
			selfID = h.User.ID
			log.Printf("身份 uid=%s（%s）", selfID, h.User.Nickname)
		}
	}); err != nil {
		return err
	}
	if err := expect(ctx, conn, signal.TypeServerHello, func(data json.RawMessage) {
		var sh signal.ServerHelloData
		if json.Unmarshal(data, &sh) == nil {
			log.Printf("协议版本 v%d（session=%s）", sh.Version, sh.SessionID)
		}
	}); err != nil {
		return err
	}

	if cfg.channel != "" {
		// channel.join 只进语音房；文字频道消息按服务器成员身份直接广播到
		// 连接（ViewChannel 口径），无需进房。进房失败不退出：监听照常。
		if err := send(ctx, conn, signal.TypeChannelJoin, signal.ChannelJoinData{ChannelID: cfg.channel}); err != nil {
			return err
		}
		if err := expect(ctx, conn, signal.TypeChannelJoined, func(data json.RawMessage) {
			var j signal.ChannelJoinedData
			if json.Unmarshal(data, &j) == nil {
				log.Printf("已加入语音房 %s（%d 人）", j.ChannelID, len(j.Members))
			}
		}); err != nil {
			log.Printf("未进房（-channel 应为语音频道 ID；文字频道无需进房）: %v", err)
		}
	}
	log.Printf("监听关键字 %q，命中即回复", cfg.keyword)

	// 事件循环：chat.message 命中关键字 → 回帖；error 信封原样打日志。
	for {
		_, raw, err := conn.Read(ctx)
		if err != nil {
			// ctx 取消（Ctrl-C）走这里：正常退出。
			if ctx.Err() != nil {
				log.Print("退出")
				return nil
			}
			return fmt.Errorf("连接断开: %w", err)
		}
		var env signal.Envelope
		if err := json.Unmarshal(raw, &env); err != nil {
			log.Printf("忽略不可解信封: %v", err)
			continue
		}
		switch env.Type {
		case signal.TypeChatMessage:
			var m signal.ChatMessageData
			if json.Unmarshal(env.Data, &m) != nil {
				continue
			}
			if m.User.ID == selfID || m.User.ID == "" {
				continue // 自己的回帖不回（防自激）
			}
			if !strings.Contains(strings.ToLower(m.Content), strings.ToLower(cfg.keyword)) {
				continue
			}
			log.Printf("命中：uid=%s 频道=%s 内容=%q", m.User.ID, m.ChannelID, m.Content)
			nonce, err := newNonce()
			if err != nil {
				log.Printf("生成 nonce 失败: %v", err)
				continue
			}
			payload := signal.ChatSendData{
				ChannelID: m.ChannelID, Type: "text",
				Content: cfg.reply, Nonce: nonce,
			}
			if err := send(ctx, conn, signal.TypeChatSend, payload); err != nil {
				log.Printf("回复失败: %v", err)
				continue
			}
			log.Printf("已回复频道 %s（nonce=%s）", m.ChannelID, nonce)
		case signal.TypeError:
			var e signal.ErrorData
			_ = json.Unmarshal(env.Data, &e)
			log.Printf("服务端错误：code=%s message=%s", e.Code, e.Message)
		default:
			log.Printf("收到 %s（忽略）", env.Type)
		}
	}
}

// send 打包并写一条 C→S 信封。
func send(ctx context.Context, conn *websocket.Conn, msgType string, data any) error {
	raw, err := signal.MarshalEnvelope(msgType, data)
	if err != nil {
		return err
	}
	wctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	return conn.Write(wctx, websocket.MessageText, raw)
}

// expect 读到指定类型为止并交给 on（错误信封立即返回，避免吞掉失败原因）。
func expect(ctx context.Context, conn *websocket.Conn, want string, on func(json.RawMessage)) error {
	for {
		_, raw, err := conn.Read(ctx)
		if err != nil {
			return err
		}
		var env signal.Envelope
		if err := json.Unmarshal(raw, &env); err != nil {
			continue
		}
		switch {
		case env.Type == want:
			on(env.Data)
			return nil
		case env.Type == signal.TypeError:
			var e signal.ErrorData
			_ = json.Unmarshal(env.Data, &e)
			return fmt.Errorf("code=%s message=%s", e.Code, e.Message)
		default:
			// hello 之前可能有零星事件，跳过。
		}
	}
}

// newNonce 幂等键：16 字节随机 hex。
func newNonce() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}
