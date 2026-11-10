// trace-hook 是 codex-trace 的单一可执行文件：Codex hook 入口、后台上传进程与运维命令。
package main

import (
	"fmt"
	"io"
	"os"
	"os/exec"
	"syscall"
	"time"

	"github.com/YYYYYHHHHHH/codex-trace/internal/buildinfo"
	"github.com/YYYYYHHHHHH/codex-trace/internal/config"
	"github.com/YYYYYHHHHHH/codex-trace/internal/hook"
	"github.com/YYYYYHHHHHH/codex-trace/internal/paths"
)

const usage = `用法: trace-hook <命令>

  hook <事件名>   Codex hook 入口（由 hooks.json 调用）
  version         显示版本
`

func main() {
	os.Exit(run(os.Args[1:], os.Stdin, os.Stdout, os.Stderr))
}

func run(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprint(stderr, usage)
		return 2
	}
	switch args[0] {
	case "hook":
		hook.Run(hook.Deps{
			Stdin:         stdin,
			Args:          args[1:],
			Getenv:        os.Getenv,
			Now:           time.Now,
			Paths:         paths.FromEnv(),
			Builtin:       builtin(),
			StartUploader: startUploader,
		})
		return 0
	case "version":
		fmt.Fprintf(stdout, "trace-hook %s\n", buildinfo.Version)
		return 0
	default:
		fmt.Fprintf(stderr, "未知命令 %q\n\n%s", args[0], usage)
		return 2
	}
}

func builtin() config.Builtin {
	return config.Builtin{
		Host:        buildinfo.LangfuseHost,
		PublicKey:   buildinfo.LangfusePublicKey,
		SecretKey:   buildinfo.LangfuseSecretKey,
		Environment: buildinfo.Environment,
	}
}

// startUploader 以独立会话拉起后台上传进程。标准流全部指向 /dev/null，
// 不继承 Codex 的 hook 管道，否则 Codex 会等待它退出。
func startUploader() error {
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	cmd := exec.Command(exe, "upload")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		return err
	}
	return cmd.Process.Release()
}
