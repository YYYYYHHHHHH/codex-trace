// Package ops 实现面向用户的运维命令：status、doctor、preview、pause / resume、config show。
// 所有输出中的密钥一律打码。
package ops

import (
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/YYYYYHHHHHH/codex-trace/internal/config"
	"github.com/YYYYYHHHHHH/codex-trace/internal/fsutil"
	"github.com/YYYYYHHHHHH/codex-trace/internal/paths"
	"github.com/YYYYYHHHHHH/codex-trace/internal/queue"
	"github.com/YYYYYHHHHHH/codex-trace/internal/status"
)

// HeartbeatFresh 与 hook 包保持一致：心跳在此时间内即认为上传进程在运行。
const HeartbeatFresh = 5 * time.Second

// Env 是运维命令的依赖。
type Env struct {
	Paths     paths.Paths
	Getenv    func(string) string
	Builtin   config.Builtin
	Version   string
	CodexHome string
	Now       func() time.Time
	HTTP      *http.Client
	Out       io.Writer
}

func (e Env) config() config.Config { return config.Load(e.Paths, e.Getenv, e.Builtin) }

func (e Env) printf(format string, args ...any) { fmt.Fprintf(e.Out, format, args...) }

// Pause 停止采集（已采集的数据仍会上传）。
func Pause(e Env) error {
	if err := fsutil.EnsurePrivateDir(e.Paths.StateDir); err != nil {
		return err
	}
	if err := fsutil.WriteFileAtomic(e.Paths.PausedFile(), []byte(e.Now().UTC().Format(time.RFC3339)+"\n"), 0o600); err != nil {
		return fmt.Errorf("写入暂停标记: %w", err)
	}
	e.printf("已暂停采集。恢复：trace-hook resume\n")
	return nil
}

// Resume 恢复采集。
func Resume(e Env) error {
	if err := os.Remove(e.Paths.PausedFile()); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("删除暂停标记: %w", err)
	}
	e.printf("已恢复采集。\n")
	return nil
}

// ConfigShow 显示生效配置及每项的来源。
func ConfigShow(e Env) error {
	cfg := e.config()
	row := func(key, value string) {
		e.printf("  %-16s %-40s %s\n", key, value, sourceLabel(cfg.Sources[key]))
	}
	e.printf("生效配置：\n")
	row("langfuse_host", orDash(cfg.Host))
	row("public_key", orDash(config.Mask(cfg.PublicKey)))
	row("secret_key", orDash(config.Mask(cfg.SecretKey)))
	row("environment", orDash(cfg.Environment))
	row("capture_content", fmt.Sprint(cfg.CaptureContent))
	row("enabled", fmt.Sprint(cfg.Enabled))
	e.printf("\n配置文件：\n  用户  %s\n  托管  %s\n", e.Paths.UserConfigFile, e.Paths.ManagedConfigFile)
	printWarnings(e, cfg.Warnings)
	return nil
}

// Status 显示采集与上传的当前状态。
func Status(e Env) error {
	cfg := e.config()
	now := e.Now()
	e.printf("trace-hook %s\n\n", e.Version)

	e.printf("采集：      %s\n", collectState(e, cfg))
	e.printf("上报地址：  %s（environment=%s，capture_content=%v）\n", orDash(cfg.Host), orDash(cfg.Environment), cfg.CaptureContent)
	e.printf("最近 hook： %s\n", lastHook(e, now))
	e.printf("上传进程：  %s\n", uploaderState(e, now))

	st, ok, err := status.Read(e.Paths.StatusFile())
	switch {
	case err != nil:
		e.printf("上传状态：  无法读取（%v）\n", err)
	case !ok:
		e.printf("上传状态：  尚无记录\n")
	default:
		e.printf("最近成功：  %s\n", ago(st.LastSendOK, now))
		e.printf("累计发送：  %d 个 span / %d 批（拒收 %d，失败 %d，丢弃无效事件 %d）\n",
			st.SentSpans, st.SentBatches, st.RejectedSpans, st.FailedBatches, st.DroppedEvents)
		if st.LastError != "" {
			e.printf("最近错误：  %s（%s）\n", st.LastError, ago(st.LastErrorAt, now))
		}
		if !st.NextRetryAt.IsZero() && st.NextRetryAt.After(now) {
			e.printf("下次重试：  %s 后\n", st.NextRetryAt.Sub(now).Round(time.Second))
		}
	}
	e.printf("积压：      spool %d / outbox %d / failed %d\n",
		queueLen(e.Paths.SpoolDir()), queueLen(e.Paths.OutboxDir()), queueLen(e.Paths.FailedDir()))
	e.printf("状态目录：  %s\n", e.Paths.StateDir)
	printWarnings(e, cfg.Warnings)
	return nil
}

func collectState(e Env, cfg config.Config) string {
	switch {
	case !cfg.Configured():
		return "未配置凭证（静默不采集）"
	case !cfg.Enabled:
		return "已禁用（enabled=false）"
	case exists(e.Paths.PausedFile()):
		return "已暂停（trace-hook resume 恢复）"
	default:
		return "开启"
	}
}

func lastHook(e Env, now time.Time) string {
	data, err := os.ReadFile(e.Paths.LastHookFile())
	if err != nil {
		return "从未触发"
	}
	fields := strings.Fields(string(data))
	if len(fields) < 2 {
		return "未知"
	}
	t, err := time.Parse(time.RFC3339Nano, fields[0])
	if err != nil {
		return "未知"
	}
	return fmt.Sprintf("%s（%s）", ago(t, now), fields[1])
}

func uploaderState(e Env, now time.Time) string {
	info, err := os.Stat(e.Paths.HeartbeatFile())
	if err == nil && now.Sub(info.ModTime()) < HeartbeatFresh {
		return "运行中"
	}
	return "未运行（有新事件时自动启动）"
}

func printWarnings(e Env, ws []string) {
	if len(ws) == 0 {
		return
	}
	e.printf("\n警告：\n")
	for _, w := range ws {
		e.printf("  - %s\n", w)
	}
}

func queueLen(dir string) int {
	entries, err := queue.New(dir).List()
	if err != nil {
		return 0
	}
	return len(entries)
}

func ago(t, now time.Time) string {
	if t.IsZero() {
		return "无"
	}
	d := now.Sub(t)
	switch {
	case d < 0:
		return t.Local().Format("2006-01-02 15:04:05")
	case d < time.Minute:
		return fmt.Sprintf("%d 秒前", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%d 分钟前", int(d.Minutes()))
	case d < 48*time.Hour:
		return fmt.Sprintf("%d 小时前", int(d.Hours()))
	default:
		return fmt.Sprintf("%d 天前", int(d.Hours()/24))
	}
}

func sourceLabel(s config.Source) string {
	switch s {
	case config.SourceBuiltin:
		return "内置"
	case config.SourceUser:
		return "用户配置"
	case config.SourceManaged:
		return "托管配置"
	case config.SourceEnv:
		return "环境变量"
	case config.SourceDefault:
		return "默认值"
	default:
		return "-"
	}
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

func exists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}
