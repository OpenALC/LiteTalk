// bot.go bot create 子命令（#51）：创建 bot 账号（users.is_bot=1，password_hash
// 空串无法密码登录，email 用 <prefix>@bots.invalid 保留域）并签发 kind=bot 的
// api_token。scope 复用 internal/token 的白名单校验——创建端拒收未知值与空列表，
// 之后每次请求再由 authz 取交集。
package main

import (
	"context"
	"crypto/rand"
	"errors"
	"flag"
	"fmt"
	"io"
	"math/big"
	"os"
	"strings"

	"github.com/OpenALC/LiteTalk/server/internal/store"
	"github.com/OpenALC/LiteTalk/server/internal/token"
)

// defaultBotScopes bot 的缺省 scope：收事件 + 发消息的最小集。创建端只收窄不放大，
// 需要更多能力的运维必须显式传 --scopes（职责留在执行命令的人手里）。
var defaultBotScopes = []string{token.ScopeMessagesRead, token.ScopeMessagesWrite}

// botCmd 解析 `litetalkctl bot <args>`。
func botCmd(args []string) error {
	if len(args) == 0 || args[0] != "create" {
		return fmt.Errorf("用法: litetalkctl bot create --name <名字> [--scopes <列表>] [--data <目录>]")
	}
	fs := flag.NewFlagSet("bot create", flag.ContinueOnError)
	name := fs.String("name", "", "bot 显示名（2–16 字符，用户在 @提及/消息作者里能看到）")
	scopesFlag := fs.String("scopes", strings.Join(defaultBotScopes, " "),
		"scope 列表（空格或逗号分隔，白名单: messages.read messages.write attachments.write voice.connect guild.manage admin.read）")
	dataDirFlag := fs.String("data", "", "数据目录（SQLite 路径回退；缺省 LITETALK_DATA_DIR 或 ./data）")
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}
	if *dataDirFlag != "" {
		os.Setenv("LITETALK_DATA_DIR", *dataDirFlag)
	}
	scopes, err := parseScopes(*scopesFlag)
	if err != nil {
		return err
	}
	ctx := context.Background()
	st, closeStore, err := openStore(ctx)
	if err != nil {
		return err
	}
	defer closeStore()
	user, plain, row, err := createBot(ctx, st, *name, scopes)
	if err != nil {
		return err
	}
	printBotCreated(os.Stdout, user, plain, row)
	return nil
}

// parseScopes 拆分 --scopes 并复用 T2 白名单校验（非空、去重、全部在白名单内）。
func parseScopes(raw string) ([]string, error) {
	fields := strings.Fields(strings.ReplaceAll(raw, ",", " "))
	if !token.ValidScopes(fields) {
		return nil, fmt.Errorf("--scopes 必须非空、不重复且全部在白名单内（messages.read messages.write attachments.write voice.connect guild.manage admin.read）")
	}
	return fields, nil
}

// createBot 建 bot 用户 + bot 令牌。prefix 同时是 email 前缀（8 位 hex，
// 撞 users.email 唯一索引的概率 2^-32，撞 tag 或 prefix 就整组重摇）。
func createBot(ctx context.Context, st store.Store, name string, scopes []string) (*store.User, *token.Plain, *store.APIToken, error) {
	if n := len([]rune(name)); n < 2 || n > 16 {
		return nil, nil, nil, fmt.Errorf("--name 需为 2–16 个字符")
	}
	if !token.ValidScopes(scopes) {
		return nil, nil, nil, fmt.Errorf("scopes 非法（必须非空、不重复且在白名单内）")
	}
	var lastErr error
	for i := 0; i < 5; i++ {
		plain, err := token.NewPlain(token.KindBot)
		if err != nil {
			return nil, nil, nil, err
		}
		tag, err := randBotTag()
		if err != nil {
			return nil, nil, nil, err
		}
		user := &store.User{
			// bots.invalid 是 RFC 6761 保留域，bot 永远收不到邮件；prefix 唯一
			// 保证 email 唯一。password_hash 空串 = 无法密码登录（等时拒绝）。
			Email:        plain.Prefix + "@bots.invalid",
			Nickname:     name,
			Tag:          tag,
			PasswordHash: "",
			IsBot:        true,
		}
		if err := st.CreateUser(ctx, user); err != nil {
			if !errors.Is(err, store.ErrConflict) {
				return nil, nil, nil, err
			}
			lastErr = err
			continue
		}
		row := &store.APIToken{
			UserID:     user.ID,
			Name:       name,
			Prefix:     plain.Prefix,
			SecretHash: plain.Hash,
			Kind:       token.KindBot,
			Scopes:     strings.Join(scopes, " "),
		}
		if err := st.CreateAPIToken(ctx, row); err != nil {
			if !errors.Is(err, store.ErrConflict) {
				return nil, nil, nil, err
			}
			lastErr = err
			continue
		}
		return user, plain, row, nil
	}
	return nil, nil, nil, fmt.Errorf("创建 bot 连续冲突 5 次（tag/prefix 撞唯一索引）: %w", lastErr)
}

// printBotCreated 输出创建结果。明文只出现在这里，不落日志、不落盘。
func printBotCreated(w io.Writer, user *store.User, plain *token.Plain, row *store.APIToken) {
	fmt.Fprintf(w, "bot 已创建\n")
	fmt.Fprintf(w, "  uid     %d\n", user.ID)
	fmt.Fprintf(w, "  名字    %s#%s\n", user.Nickname, user.Tag)
	fmt.Fprintf(w, "  scope   %s\n", row.Scopes)
	fmt.Fprintf(w, "  prefix  %s\n", plain.Prefix)
	fmt.Fprintf(w, "  令牌    %s\n", plain.Token)
	fmt.Fprintf(w, "明文仅此一次，立即保存；泄漏后在服务器日志/库中按 prefix 定位该行吊销。\n")
}

// randBotTag 4 位定宽数字串（users.tag 唯一索引口径）。
func randBotTag() (string, error) {
	n, err := rand.Int(rand.Reader, big.NewInt(10000))
	if err != nil {
		return "", fmt.Errorf("生成 tag 失败: %w", err)
	}
	return fmt.Sprintf("%04d", n.Int64()), nil
}
