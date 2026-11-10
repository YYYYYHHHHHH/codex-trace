package ops

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/BurntSushi/toml"

	"github.com/YYYYYHHHHHH/codex-trace/internal/exporter"
	"github.com/YYYYYHHHHHH/codex-trace/internal/fsutil"
	"github.com/YYYYYHHHHHH/codex-trace/internal/status"
)

// checkLevel 是一项检查的结论。
type checkLevel int

const (
	levelOK checkLevel = iota
	levelWarn
	levelFail
)

var levelMark = [...]string{"✓", "!", "✗"}

type check struct {
	level  checkLevel
	title  string
	detail string
}

// hookNeverFiredAfter：安装后多久仍未触发 hook 视为异常。
const hookStaleAfter = 7 * 24 * time.Hour

// DoctorOptions 控制 doctor 行为。
type DoctorOptions struct {
	Offline bool // 跳过网络检查
}

// ErrDoctorFailed 表示存在失败项。
var ErrDoctorFailed = errors.New("doctor: 存在失败项")

// Doctor 逐项诊断采集链路，存在失败项时返回 ErrDoctorFailed。
func Doctor(ctx context.Context, e Env, opts DoctorOptions) error {
	checks := []check{
		checkStateDir(e),
		checkConfig(e),
		checkPlugin(e),
		checkHookFiring(e),
		checkUploader(e),
		checkBacklog(e),
	}
	if opts.Offline {
		checks = append(checks, check{levelWarn, "Langfuse 连通性", "已跳过（--offline）"})
	} else {
		checks = append(checks, checkConnectivity(ctx, e))
	}

	failed := false
	for _, c := range checks {
		e.printf("%s %s", levelMark[c.level], c.title)
		if c.detail != "" {
			e.printf("：%s", c.detail)
		}
		e.printf("\n")
		failed = failed || c.level == levelFail
	}
	if failed {
		return ErrDoctorFailed
	}
	return nil
}

// checkStateDir 只检查、不修改（诊断命令不应有副作用）。
func checkStateDir(e Env) check {
	if !e.Paths.Usable() {
		return check{levelFail, "状态目录", "无法确定 HOME"}
	}
	info, err := os.Lstat(e.Paths.StateDir)
	switch {
	case errors.Is(err, os.ErrNotExist):
		return check{levelOK, "状态目录", e.Paths.StateDir + "（尚未创建，首次 hook 触发时创建）"}
	case err != nil:
		return check{levelFail, "状态目录", err.Error()}
	case info.Mode()&os.ModeSymlink != 0 || !info.IsDir():
		return check{levelFail, "状态目录", e.Paths.StateDir + " 不是真实目录"}
	}
	if uid, ok := fsutil.OwnerUID(info); ok && uid != uint32(os.Geteuid()) {
		return check{levelFail, "状态目录", fmt.Sprintf("%s 属于 uid %d，hook 会拒绝使用", e.Paths.StateDir, uid)}
	}
	if info.Mode().Perm()&0o077 != 0 {
		return check{levelWarn, "状态目录", fmt.Sprintf("%s 权限为 %o，下次 hook 时会收紧为 700", e.Paths.StateDir, info.Mode().Perm())}
	}
	return check{levelOK, "状态目录", e.Paths.StateDir}
}

func checkConfig(e Env) check {
	cfg := e.config()
	if len(cfg.Warnings) > 0 && !cfg.Configured() {
		return check{levelFail, "配置", strings.Join(cfg.Warnings, "；")}
	}
	if !cfg.Configured() {
		return check{levelFail, "配置", "没有可用凭证：此版本未内置凭证，也没有配置文件 / 环境变量"}
	}
	detail := fmt.Sprintf("%s（凭证来源：%s）", cfg.Host, sourceLabel(cfg.Sources["secret_key"]))
	switch {
	case !cfg.Enabled:
		return check{levelWarn, "配置", detail + "；enabled=false，采集已禁用"}
	case exists(e.Paths.PausedFile()):
		return check{levelWarn, "配置", detail + "；已暂停（trace-hook resume 恢复）"}
	case len(cfg.Warnings) > 0:
		return check{levelWarn, "配置", detail + "；" + strings.Join(cfg.Warnings, "；")}
	}
	return check{levelOK, "配置", detail}
}

// pluginHookCount 是插件 hooks.json 中声明的 hook 数。
const pluginHookCount = 9

// checkPlugin 读取 Codex 配置，确认插件已启用、hook 有审批记录。
// 注意：只能确认"审批过"，无法确认审批对当前版本仍然有效（插件升级后需重新审批），
// 是否生效以 checkHookFiring 的最近触发时间为准。
func checkPlugin(e Env) check {
	var cfg struct {
		Plugins map[string]struct {
			Enabled *bool `toml:"enabled"`
		} `toml:"plugins"`
		Hooks struct {
			State map[string]struct {
				TrustedHash string `toml:"trusted_hash"`
				Enabled     *bool  `toml:"enabled"`
			} `toml:"state"`
		} `toml:"hooks"`
	}
	path := filepath.Join(e.CodexHome, "config.toml")
	if _, err := toml.DecodeFile(path, &cfg); err != nil {
		return check{levelWarn, "Codex 插件", fmt.Sprintf("无法读取 %s：%v", path, err)}
	}
	var pluginIDs []string
	for name, p := range cfg.Plugins {
		if strings.HasPrefix(name, "codex-trace@") && (p.Enabled == nil || *p.Enabled) {
			pluginIDs = append(pluginIDs, name)
		}
	}
	installed := len(pluginIDs) > 0
	trusted, seen := 0, 0
	for key, st := range cfg.Hooks.State {
		if !hookKeyBelongsTo(key, pluginIDs) {
			continue
		}
		seen++
		if st.TrustedHash != "" && (st.Enabled == nil || *st.Enabled) {
			trusted++
		}
	}
	switch {
	case !installed:
		return check{levelWarn, "Codex 插件", "未在 config.toml 中找到已启用的 codex-trace 插件（MDM 托管安装时可忽略）"}
	case seen == 0:
		return check{levelWarn, "Codex 插件", "已安装，但未发现 hook 审批记录：请在 Codex 中审批 codex-trace 的 hooks"}
	case trusted < pluginHookCount:
		return check{levelWarn, "Codex 插件", fmt.Sprintf("%d/%d 个 hook 有审批记录；请在 Codex 中审批全部 hooks", trusted, pluginHookCount)}
	}
	return check{levelOK, "Codex 插件", fmt.Sprintf("已安装，%d 个 hook 有审批记录（升级后需重新审批，是否生效见下一项）", trusted)}
}

// hookKeyBelongsTo 判断 hooks.state 的 key 是否属于这些插件（key 中包含插件 id 作为独立片段）。
func hookKeyBelongsTo(key string, pluginIDs []string) bool {
	for _, id := range pluginIDs {
		for _, part := range strings.FieldsFunc(key, func(r rune) bool { return r == ':' || r == '/' }) {
			if part == id {
				return true
			}
		}
	}
	return false
}

func checkHookFiring(e Env) check {
	data, err := os.ReadFile(e.Paths.LastHookFile())
	if err != nil {
		return check{levelWarn, "hook 触发", "从未触发：插件未安装、hook 未审批，或尚未使用 Codex"}
	}
	fields := strings.Fields(string(data))
	if len(fields) == 0 {
		return check{levelWarn, "hook 触发", "记录为空"}
	}
	t, err := time.Parse(time.RFC3339Nano, fields[0])
	if err != nil {
		return check{levelWarn, "hook 触发", "记录无法解析"}
	}
	if e.Now().Sub(t) > hookStaleAfter {
		return check{levelWarn, "hook 触发", "最近一次在 " + ago(t, e.Now()) + "：若一直在用 Codex，检查 hook 是否需要重新审批"}
	}
	return check{levelOK, "hook 触发", "最近一次 " + ago(t, e.Now())}
}

func checkUploader(e Env) check {
	st, ok, err := status.Read(e.Paths.StatusFile())
	switch {
	case err != nil:
		return check{levelWarn, "上传进程", err.Error()}
	case !ok:
		return check{levelWarn, "上传进程", "尚未运行过"}
	case st.LastError != "" && st.LastErrorAt.After(st.LastSendOK):
		return check{levelFail, "上传进程", fmt.Sprintf("最近一次失败：%s（%s）", st.LastError, ago(st.LastErrorAt, e.Now()))}
	}
	return check{levelOK, "上传进程", fmt.Sprintf("%s；最近成功 %s", uploaderState(e, e.Now()), ago(st.LastSendOK, e.Now()))}
}

func checkBacklog(e Env) check {
	spool, outbox, failed := queueLen(e.Paths.SpoolDir()), queueLen(e.Paths.OutboxDir()), queueLen(e.Paths.FailedDir())
	detail := fmt.Sprintf("spool %d / outbox %d / failed %d", spool, outbox, failed)
	if failed > 0 || outbox > 20 || spool > 1000 {
		return check{levelWarn, "积压", detail + "（failed 中为被服务端拒绝的批次，见 logs/upload.log）"}
	}
	return check{levelOK, "积压", detail}
}

// checkConnectivity 发送一个空的 OTLP 请求，同时验证网络与凭证。
func checkConnectivity(ctx context.Context, e Env) check {
	cfg := e.config()
	if !cfg.Configured() {
		return check{levelWarn, "Langfuse 连通性", "跳过：未配置凭证"}
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	res := exporter.New(cfg.Host, cfg.PublicKey, cfg.SecretKey, e.HTTP).Send(ctx, []byte(`{"resourceSpans":[]}`))
	proxy := proxyHint(e)
	switch {
	case res.Outcome == exporter.OK:
		return check{levelOK, "Langfuse 连通性", "凭证有效" + proxy}
	case res.Status == 401 || res.Status == 403:
		return check{levelFail, "Langfuse 连通性", fmt.Sprintf("凭证被拒绝（HTTP %d）", res.Status)}
	case res.Status != 0:
		return check{levelFail, "Langfuse 连通性", fmt.Sprintf("HTTP %d：%s", res.Status, res.Message)}
	default:
		return check{levelFail, "Langfuse 连通性", fmt.Sprintf("网络不通：%v%s", res.Err, proxy)}
	}
}

func proxyHint(e Env) string {
	for _, k := range []string{"HTTPS_PROXY", "https_proxy", "ALL_PROXY", "all_proxy"} {
		if v := e.Getenv(k); v != "" {
			return "（经代理 " + k + "）"
		}
	}
	return ""
}
