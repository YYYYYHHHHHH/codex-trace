// trace-hook 是 codex-trace 的单一可执行文件：Codex hook 入口、后台上传进程与运维命令。
package main

import (
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
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
		runHook(args[1:], stdin)
		return 0
	case "version":
		fmt.Fprintf(stdout, "trace-hook %s\n", buildinfo.Version)
		return 0
	default:
		fmt.Fprintf(stderr, "未知命令 %q\n\n%s", args[0], usage)
		return 2
	}
}

// runHook 在任何情况下都不输出、不 panic：hook 的退出码与 stdout 会被 Codex 解读。
func runHook(args []string, stdin io.Reader) {
	defer func() { _ = recover() }()
	hook.Run(hook.Deps{
		Stdin:         stdin,
		Args:          args,
		Getenv:        os.Getenv,
		Now:           time.Now,
		Paths:         paths.FromEnv(),
		Builtin:       builtin(),
		StartUploader: startUploader,
	})
}

func builtin() config.Builtin {
	return config.Builtin{
		Host:        buildinfo.LangfuseHost,
		PublicKey:   buildinfo.LangfusePublicKey,
		SecretKey:   buildinfo.LangfuseSecretKey,
		Environment: buildinfo.Environment,
	}
}

// startUploader 以独立会话拉起后台上传进程：
//   - 标准流全部指向 /dev/null，不继承 Codex 的 hook 管道，否则 Codex 会等待它退出；
//   - 工作目录设为 /，避免长期占用用户项目目录；
//   - 只传必要的环境变量，不把 Codex 环境里无关的密钥带进常驻进程。
func startUploader() error {
	exe, err := os.Executable()
	if err != nil {
		return fmt.Errorf("定位可执行文件: %w", err)
	}
	cmd := exec.Command(exe, "upload")
	cmd.Dir = "/"
	cmd.Env = uploaderEnv(os.Environ())
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("启动 upload: %w", err)
	}
	return cmd.Process.Release()
}

var uploaderEnvKeys = map[string]bool{
	"HOME": true, "USER": true, "LOGNAME": true, "PATH": true, "LANG": true, "TMPDIR": true,
	"CODEX_HOME": true, "XDG_STATE_HOME": true, "XDG_CONFIG_HOME": true,
	"HTTPS_PROXY": true, "https_proxy": true, "HTTP_PROXY": true, "http_proxy": true,
	"NO_PROXY": true, "no_proxy": true, "ALL_PROXY": true, "all_proxy": true,
	"SSL_CERT_FILE": true, "SSL_CERT_DIR": true,
}

// uploaderEnv 从当前环境中筛出上传进程需要的变量。
func uploaderEnv(environ []string) []string {
	var out []string
	for _, kv := range environ {
		key, _, _ := strings.Cut(kv, "=")
		if uploaderEnvKeys[key] || strings.HasPrefix(key, "CODEX_TRACE_") {
			out = append(out, kv)
		}
	}
	return out
}
