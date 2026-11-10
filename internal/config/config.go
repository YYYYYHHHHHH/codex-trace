// Package config 合并 内置值 → 用户配置 → 托管配置 → 环境变量，得到生效配置。
package config

import (
	"errors"
	"fmt"
	"io/fs"
	"net/url"
	"os"
	"strconv"
	"strings"

	"github.com/BurntSushi/toml"

	"github.com/YYYYYHHHHHH/codex-trace/internal/paths"
)

// Source 标识某个配置项的来源。
type Source string

const (
	SourceDefault Source = "default"
	SourceBuiltin Source = "builtin"
	SourceUser    Source = "user"
	SourceManaged Source = "managed"
	SourceEnv     Source = "env"
)

// Builtin 是构建时注入的默认值。
type Builtin struct {
	Host        string
	PublicKey   string
	SecretKey   string
	Environment string
}

// Config 是生效配置；Load 每次返回新值，调用方不应修改。
type Config struct {
	Host           string
	PublicKey      string
	SecretKey      string
	Environment    string
	CaptureContent bool
	Enabled        bool
	Sources        map[string]Source
	Warnings       []string
}

// Configured 表示凭证齐全且上报地址合法。
func (c Config) Configured() bool {
	return validHost(c.Host) && c.PublicKey != "" && c.SecretKey != ""
}

type fileConfig struct {
	LangfuseHost   *string `toml:"langfuse_host"`
	PublicKey      *string `toml:"public_key"`
	SecretKey      *string `toml:"secret_key"`
	Environment    *string `toml:"environment"`
	CaptureContent *bool   `toml:"capture_content"`
	Enabled        *bool   `toml:"enabled"`
}

// Load 读取全部来源并合并。任何来源出错都只产生 warning，不会失败。
func Load(p paths.Paths, getenv func(string) string, builtin Builtin) Config {
	cfg := Config{
		Enabled: true,
		Sources: map[string]Source{
			"capture_content": SourceDefault,
			"enabled":         SourceDefault,
		},
	}
	cfg = applyBuiltin(cfg, builtin)

	if fc, warn := readFile(p.UserConfigFile, true); warn != "" {
		cfg.Warnings = append(cfg.Warnings, warn)
	} else if fc != nil {
		cfg = applyFile(cfg, *fc, SourceUser)
	}
	if fc, warn := readFile(p.ManagedConfigFile, false); warn != "" {
		cfg.Warnings = append(cfg.Warnings, warn)
	} else if fc != nil {
		cfg = applyFile(cfg, *fc, SourceManaged)
	}
	cfg = applyEnv(cfg, getenv)
	cfg.Host = strings.TrimRight(cfg.Host, "/")

	if cfg.Host != "" && !validHost(cfg.Host) {
		cfg.Warnings = append(cfg.Warnings, fmt.Sprintf("langfuse_host %q 不合法：必须是 https，或指向本机的 http", cfg.Host))
	}
	return cfg
}

func applyBuiltin(cfg Config, b Builtin) Config {
	set := func(key string, dst *string, v string) {
		if v != "" {
			*dst = v
			cfg.Sources[key] = SourceBuiltin
		}
	}
	set("langfuse_host", &cfg.Host, b.Host)
	set("public_key", &cfg.PublicKey, b.PublicKey)
	set("secret_key", &cfg.SecretKey, b.SecretKey)
	set("environment", &cfg.Environment, b.Environment)
	return cfg
}

func applyFile(cfg Config, fc fileConfig, src Source) Config {
	setStr := func(key string, dst *string, v *string) {
		if v != nil {
			*dst = *v
			cfg.Sources[key] = src
		}
	}
	setBool := func(key string, dst *bool, v *bool) {
		if v != nil {
			*dst = *v
			cfg.Sources[key] = src
		}
	}
	setStr("langfuse_host", &cfg.Host, fc.LangfuseHost)
	setStr("public_key", &cfg.PublicKey, fc.PublicKey)
	setStr("secret_key", &cfg.SecretKey, fc.SecretKey)
	setStr("environment", &cfg.Environment, fc.Environment)
	setBool("capture_content", &cfg.CaptureContent, fc.CaptureContent)
	setBool("enabled", &cfg.Enabled, fc.Enabled)
	return cfg
}

func applyEnv(cfg Config, getenv func(string) string) Config {
	strs := []struct {
		env, key string
		dst      *string
	}{
		{"CODEX_TRACE_LANGFUSE_HOST", "langfuse_host", &cfg.Host},
		{"CODEX_TRACE_LANGFUSE_PUBLIC_KEY", "public_key", &cfg.PublicKey},
		{"CODEX_TRACE_LANGFUSE_SECRET_KEY", "secret_key", &cfg.SecretKey},
		{"CODEX_TRACE_ENVIRONMENT", "environment", &cfg.Environment},
	}
	for _, s := range strs {
		if v := getenv(s.env); v != "" {
			*s.dst = v
			cfg.Sources[s.key] = SourceEnv
		}
	}
	bools := []struct {
		env, key string
		dst      *bool
	}{
		{"CODEX_TRACE_CAPTURE_CONTENT", "capture_content", &cfg.CaptureContent},
		{"CODEX_TRACE_ENABLED", "enabled", &cfg.Enabled},
	}
	for _, b := range bools {
		raw := getenv(b.env)
		if raw == "" {
			continue
		}
		v, err := strconv.ParseBool(raw)
		if err != nil {
			cfg.Warnings = append(cfg.Warnings, fmt.Sprintf("%s=%q 不是合法布尔值，已忽略", b.env, raw))
			continue
		}
		*b.dst = v
		cfg.Sources[b.key] = SourceEnv
	}
	return cfg
}

// readFile 返回 (nil, "") 表示文件不存在。requirePrivate 时拒绝组/其他用户可访问的文件。
func readFile(path string, requirePrivate bool) (*fileConfig, string) {
	if path == "" {
		return nil, ""
	}
	info, err := os.Stat(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, ""
	}
	if err != nil {
		return nil, fmt.Sprintf("读取配置 %s 失败：%v", path, err)
	}
	if requirePrivate && info.Mode().Perm()&0o077 != 0 {
		return nil, fmt.Sprintf("配置 %s 的 permission 为 %o，过宽已忽略；请执行 chmod 600", path, info.Mode().Perm())
	}
	var fc fileConfig
	if _, err := toml.DecodeFile(path, &fc); err != nil {
		return nil, fmt.Sprintf("解析配置 %s 失败：%v", path, err)
	}
	return &fc, ""
}

func validHost(h string) bool {
	u, err := url.Parse(h)
	if err != nil || u.Host == "" {
		return false
	}
	switch u.Scheme {
	case "https":
		return true
	case "http":
		host := u.Hostname()
		return host == "127.0.0.1" || host == "localhost" || host == "::1"
	default:
		return false
	}
}

// Mask 用于展示密钥：只保留首尾各 4 个字符。
func Mask(s string) string {
	const keep = 4
	if s == "" {
		return ""
	}
	if len(s) <= keep*3 {
		return "****"
	}
	return s[:keep] + "…" + s[len(s)-keep:]
}
