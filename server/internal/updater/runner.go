package updater

import (
	"context"
	"fmt"
	"os/exec"
	"strings"
)

// Runner 执行系统命令。生产走 ExecRunner；测试注入假实现，不依赖 root/systemd。
type Runner interface {
	Run(ctx context.Context, name string, args ...string) error
}

// ExecRunner 固定参数直调 exec（绝不经 shell）：tag/URL 等外部输入永远不会进入命令行拼接。
type ExecRunner struct {
	// Stdout/Stderr 为 nil 时交给 journald（systemd 单元的 StandardOutput=journal）。
	Stdout interface{ Write([]byte) (int, error) }
	Stderr interface{ Write([]byte) (int, error) }
}

func (e ExecRunner) Run(ctx context.Context, name string, args ...string) error {
	cmd := exec.CommandContext(ctx, name, args...)
	if e.Stdout != nil {
		cmd.Stdout = e.Stdout
	}
	if e.Stderr != nil {
		cmd.Stderr = e.Stderr
	}
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("%s %s: %w", name, strings.Join(args, " "), err)
	}
	return nil
}

// freeDiskBytes 返回 path 所在文件系统可用字节数（平台实现见 disk_*.go；测试注入覆盖）。
var freeDiskBytes = platformFreeDisk
