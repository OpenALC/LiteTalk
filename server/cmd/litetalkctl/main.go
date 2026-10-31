// litetalkctl LiteTalk 服务器管理 CLI。
//
// bot create（#51）：创建 bot 账号（users.is_bot=1）并签发一次性 bot 令牌
// （ltk_bot_*）。创建入口只有 CLI 不开 REST——能连上数据库本身就是要管理员，
// 这比再加一个 REST 权限面诚实。明文令牌只打印到 stdout 一次，不落盘；
// 丢了只能吊销重建。
//
// 连库方式与服务端同一套环境变量：
//
//	LITETALK_STORE=mysql|sqlite（缺省按 LITETALK_MYSQL_DSN 是否非空推断，否则 sqlite）
//	LITETALK_MYSQL_DSN=...        MySQL 连接串
//	LITETALK_SQLITE_PATH=...      SQLite 库文件（缺省 <-data 目录>/litetalk.db）
//	LITETALK_DATA_DIR=...         数据目录（sqlite 路径回退用）
package main

import (
	"context"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/OpenALC/LiteTalk/server/internal/store"
	"github.com/OpenALC/LiteTalk/server/migrations"
)

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	switch os.Args[1] {
	case "bot":
		if err := botCmd(os.Args[2:]); err != nil {
			fmt.Fprintln(os.Stderr, "litetalkctl:", err)
			os.Exit(1)
		}
	case "-h", "--help", "help":
		usage()
	default:
		fmt.Fprintf(os.Stderr, "litetalkctl: 未知子命令 %q\n\n", os.Args[1])
		usage()
		os.Exit(2)
	}
}

func usage() {
	fmt.Print(`litetalkctl — LiteTalk 服务器管理 CLI（在服务器所在机器、以能连数据库的管理员身份执行）

子命令:
  bot create --name <名字> [--scopes <列表>] [--data <目录>]
      创建 bot 账号并签发一次性 bot 令牌（ltk_bot_* 明文，仅此一次输出）。
      --scopes 空格或逗号分隔，缺省 "messages.read messages.write"。

环境变量（与服务端同口径）: LITETALK_STORE / LITETALK_MYSQL_DSN /
LITETALK_SQLITE_PATH / LITETALK_DATA_DIR / LITETALK_MIGRATIONS_DIR
`)
}

// openStore 按服务端同口径选型并连库，幂等执行迁移（CLI 先于服务端首启也能用）。
func openStore(ctx context.Context) (store.Store, func(), error) {
	switch kind := pickStoreKind(); kind {
	case "mysql":
		dsn := os.Getenv("LITETALK_MYSQL_DSN")
		if dsn == "" {
			return nil, nil, fmt.Errorf("LITETALK_STORE=mysql 但未配置 LITETALK_MYSQL_DSN")
		}
		my, err := store.OpenMySQL(dsn)
		if err != nil {
			return nil, nil, fmt.Errorf("MySQL 连接失败: %w", err)
		}
		if err := my.Migrate(ctx, migrations.FS); err != nil {
			_ = my.Close()
			return nil, nil, fmt.Errorf("数据库迁移失败: %w", err)
		}
		return my, func() { _ = my.Close() }, nil
	case "sqlite":
		path := os.Getenv("LITETALK_SQLITE_PATH")
		if path == "" {
			path = filepath.Join(dataDir(), "litetalk.db")
		}
		sq, err := store.OpenSQLite(path)
		if err != nil {
			return nil, nil, fmt.Errorf("SQLite 打开失败: %w", err)
		}
		if err := sq.Migrate(ctx, sqliteMigrations()); err != nil {
			_ = sq.Close()
			return nil, nil, fmt.Errorf("数据库迁移失败: %w", err)
		}
		return sq, func() { _ = sq.Close() }, nil
	default:
		return nil, nil, fmt.Errorf("LITETALK_STORE=%q 不支持：litetalkctl 只连 mysql / sqlite（mem 重启即失，不能挂 bot）", kind)
	}
}

// pickStoreKind 与服务端同一优先级：LITETALK_STORE 显式 > DSN 非空 → mysql > sqlite。
func pickStoreKind() string {
	if k := strings.ToLower(strings.TrimSpace(os.Getenv("LITETALK_STORE"))); k != "" {
		return k
	}
	if os.Getenv("LITETALK_MYSQL_DSN") != "" {
		return "mysql"
	}
	return "sqlite"
}

// dataDir 与服务端 -data/LITETALK_DATA_DIR 同口径，缺省 ./data。
func dataDir() string {
	if d := strings.TrimSpace(os.Getenv("LITETALK_DATA_DIR")); d != "" {
		return d
	}
	return "./data"
}

// sqliteMigrations SQLite 方言迁移 SQL（migrations/sqlite 子目录，与服务端同布局）。
func sqliteMigrations() fs.FS {
	sub, err := fs.Sub(migrations.FS, "sqlite")
	if err != nil {
		// embed 布局由 //go:embed 编译期锁定，走到这里即打包坏了。
		panic("migrations: 缺少 sqlite 子目录: " + err.Error())
	}
	return sub
}
