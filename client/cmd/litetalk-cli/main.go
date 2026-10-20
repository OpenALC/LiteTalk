package main

import (
	"context"
	"fmt"
	"os"
	"strings"

	"github.com/OpenALC/LiteTalk/client/core/api"
	"github.com/OpenALC/LiteTalk/client/core/config"
)

const usageText = `litetalk-cli — 轻音无界面客户端（复用 core）

用法：
  litetalk-cli register  --email E --password P --nickname N [--code C] [--bootstrap B]
  litetalk-cli login     (--email E --password P | --guest)
  litetalk-cli logout
  litetalk-cli whoami
  litetalk-cli server    create --name N | list | invite --server ID | join --code C
  litetalk-cli channel   create --server ID --name N [--type voice|text] | list --server ID
  litetalk-cli join      --channel ID [--duration 3s]
  litetalk-cli send      --channel ID --text T
  litetalk-cli call      --channel ID [--duration 10s]
  litetalk-cli listen    --channel ID [--duration 10s]

公共 flag：--addr 服务端基址（缺省 LITETALK_API > 已保存地址 > http://127.0.0.1:8077）
环境变量：LITETALK_CLI_STATE 指定登录态文件（同机多实例隔离）；LITETALK_WS 覆盖信令地址
`

func usage() {
	fmt.Fprint(os.Stderr, usageText)
}

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	var err error
	switch os.Args[1] {
	case "register":
		err = cmdRegister(os.Args[2:])
	case "login":
		err = cmdLogin(os.Args[2:])
	case "logout":
		err = cmdLogout(os.Args[2:])
	case "whoami":
		err = cmdWhoami(os.Args[2:])
	case "server":
		err = cmdServer(os.Args[2:])
	case "channel":
		err = cmdChannel(os.Args[2:])
	case "join":
		err = cmdJoin(os.Args[2:])
	case "send":
		err = cmdSend(os.Args[2:])
	case "call":
		err = cmdCall(os.Args[2:])
	case "listen":
		err = cmdListen(os.Args[2:])
	case "help", "-h", "--help":
		usage()
		return
	default:
		fmt.Fprintf(os.Stderr, "未知命令 %q\n\n", os.Args[1])
		usage()
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
}

// resolveBase 解析 REST 基址：flag 显式 > LITETALK_API > 状态文件 > 默认本机。
func resolveBase(flagVal string, st *State) string {
	if strings.TrimSpace(flagVal) != "" {
		return strings.TrimRight(strings.TrimSpace(flagVal), "/")
	}
	if v := strings.TrimRight(strings.TrimSpace(os.Getenv("LITETALK_API")), "/"); v != "" {
		return v
	}
	if st != nil && st.BaseURL != "" {
		return strings.TrimRight(st.BaseURL, "/")
	}
	return config.DefaultBaseURL
}

// newAPI 构造 REST 客户端并接上令牌刷新回写（401 自动 refresh 一次）。
// X-LiteTalk-Client 保持 NewAPIClient 的默认 dev（服务端对探测值放行，#31）；
// CLI 没有构建注入的版本号。
func newAPI(base string, st *State) *api.APIClient {
	c := api.NewAPIClient(base)
	if st == nil {
		return c
	}
	c.SetToken(st.AccessToken)
	c.SetRefreshSource(func(context.Context) (string, error) { return st.RefreshToken, nil })
	c.SetOnRefreshed(func(t api.AuthTokens) {
		st.AccessToken = t.AccessToken
		st.RefreshToken = t.RefreshToken
		_ = saveState(st) // 令牌旋转丢不起：落盘失败只丢持久化，不打断在飞请求
	})
	return c
}
