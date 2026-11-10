package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"time"

	"github.com/YYYYYHHHHHH/codex-trace/internal/buildinfo"
	"github.com/YYYYYHHHHHH/codex-trace/internal/ops"
	"github.com/YYYYYHHHHHH/codex-trace/internal/paths"
)

func runOps(args []string, stdout, stderr io.Writer) int {
	p := paths.FromEnv()
	if !p.Usable() {
		fmt.Fprintln(stderr, "无法确定 HOME 目录")
		return 1
	}
	env := ops.Env{
		Paths:     p,
		Getenv:    os.Getenv,
		Builtin:   builtin(),
		Version:   buildinfo.Version,
		CodexHome: codexHomeDir(),
		Now:       time.Now,
		HTTP:      &http.Client{Timeout: 15 * time.Second},
		Out:       stdout,
	}

	fs := flag.NewFlagSet(args[0], flag.ContinueOnError)
	fs.SetOutput(stderr)
	offline := fs.Bool("offline", false, "doctor：跳过网络检查")
	raw := fs.Bool("raw", false, "preview：输出原始 OTLP JSON")
	if err := fs.Parse(args[1:]); err != nil {
		return 2
	}

	var err error
	switch args[0] {
	case "status":
		err = ops.Status(env)
	case "doctor":
		err = ops.Doctor(context.Background(), env, ops.DoctorOptions{Offline: *offline})
	case "preview":
		err = ops.Preview(env, ops.PreviewOptions{Raw: *raw})
	case "pause":
		err = ops.Pause(env)
	case "resume":
		err = ops.Resume(env)
	case "config":
		if fs.Arg(0) != "show" {
			fmt.Fprintln(stderr, "用法: trace-hook config show")
			return 2
		}
		err = ops.ConfigShow(env)
	}
	switch {
	case errors.Is(err, ops.ErrDoctorFailed):
		return 1
	case err != nil:
		fmt.Fprintln(stderr, err)
		return 1
	}
	return 0
}
