package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"os/user"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/YYYYYHHHHHH/codex-trace/internal/buildinfo"
	"github.com/YYYYYHHHHHH/codex-trace/internal/config"
	"github.com/YYYYYHHHHHH/codex-trace/internal/exporter"
	"github.com/YYYYYHHHHHH/codex-trace/internal/logx"
	"github.com/YYYYYHHHHHH/codex-trace/internal/otlp"
	"github.com/YYYYYHHHHHH/codex-trace/internal/paths"
	"github.com/YYYYYHHHHHH/codex-trace/internal/pipeline"
	"github.com/YYYYYHHHHHH/codex-trace/internal/session"
	"github.com/YYYYYHHHHHH/codex-trace/internal/trace"
	"github.com/YYYYYHHHHHH/codex-trace/internal/uploader"
)

func runUpload(args []string, stderr io.Writer) (code int) {
	fs := flag.NewFlagSet("upload", flag.ContinueOnError)
	fs.SetOutput(stderr)
	once := fs.Bool("once", false, "处理完现有数据即退出")
	if err := fs.Parse(args); err != nil {
		return 2
	}

	p := paths.FromEnv()
	if !p.Usable() {
		fmt.Fprintln(stderr, "无法确定 HOME 目录")
		return 1
	}
	cfg := config.Load(p, os.Getenv, builtin())
	if !cfg.Enabled || !cfg.Configured() {
		return 0
	}
	log := logx.New(p.LogsDir(), "upload")
	defer func() {
		if r := recover(); r != nil {
			log.Printf("panic: %v", r)
			code = 1
		}
	}()

	codexHome := codexHomeDir()
	proc := pipeline.New(pipeline.Options{
		Store: session.NewStore(p.SessionsDir()),
		Policy: trace.Policy{
			CaptureContent: cfg.CaptureContent,
			Environment:    cfg.Environment,
			Release:        buildinfo.Version,
		},
		AllowedRoots: []string{filepath.Join(codexHome, "sessions"), filepath.Join(codexHome, "archived_sessions")},
		UserFallback: currentUser(),
		Logf:         log.Printf,
	})

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	err := uploader.Run(ctx, uploader.Deps{
		Paths:     p,
		Processor: proc,
		Sender:    exporter.New(cfg.Host, cfg.PublicKey, cfg.SecretKey, nil),
		Resource:  resourceAttrs(codexHome),
		Version:   buildinfo.Version,
		Once:      *once,
		Logf:      log.Printf,
	})
	if err != nil {
		log.Printf("%v", err)
		fmt.Fprintln(stderr, err)
		return 1
	}
	return 0
}

func codexHomeDir() string {
	if h := os.Getenv("CODEX_HOME"); filepath.IsAbs(h) {
		return h
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".codex")
}

func currentUser() string {
	if u, err := user.Current(); err == nil {
		return u.Username
	}
	return os.Getenv("USER")
}

// resourceAttrs 描述上报来源；机器标识只上传哈希。
func resourceAttrs(codexHome string) []otlp.Attr {
	attrs := []otlp.Attr{
		otlp.String("service.name", "codex"),
		otlp.String("telemetry.sdk.name", "codex-trace"),
		otlp.String("telemetry.sdk.version", buildinfo.Version),
	}
	if data, err := os.ReadFile(filepath.Join(codexHome, "installation_id")); err == nil {
		if id := strings.TrimSpace(string(data)); id != "" {
			attrs = append(attrs, otlp.String("host.id", trace.HashID("host", id)))
		}
	}
	return attrs
}
