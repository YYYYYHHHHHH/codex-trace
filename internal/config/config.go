// Package config 按 内置值 < 用户配置 < 环境变量 < 托管配置 的优先级合并出生效配置。
//
// 托管配置（MDM 下发）优先级最高，保证企业策略不能被用户环境覆盖。
// 内置凭证只会与内置 host 搭配使用：任何来源改了 host，内置 key 就失效，
// 防止内置 secret key 被发往他人指定的地址。
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

	"github.com/YYYYYHHHHHH/codex-trace/internal/fsutil"
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

	if fc, warn := readFile(p.UserConfigFile, userFileTrusted); warn != "" {
		cfg.Warnings = append(cfg.Warnings, warn)
	} else if fc != nil {
		cfg = applyFile(cfg, *fc, SourceUser)
	}
	cfg = applyEnv(cfg, getenv)
	if fc, warn := readFile(p.ManagedConfigFile, managedFileTrusted); warn != "" {
		cfg.Warnings = append(cfg.Warnings, warn)
	} else if fc != nil {
		cfg = applyFile(cfg, *fc, SourceManaged)
	}

	cfg.Host = strings.TrimRight(strings.TrimSpace(cfg.Host), "/")
	cfg.PublicKey = strings.TrimSpace(cfg.PublicKey)
	cfg.SecretKey = strings.TrimSpace(cfg.SecretKey)
	cfg = unbindBuiltinKeys(cfg)

	if cfg.Host != "" && !validHost(cfg.Host) {
		cfg.Warnings = append(cfg.Warnings, "langfuse_host 不合法：必须是不带账号信息的 https 地址，或指向本机的 http")
	}
	return cfg
}

// unbindBuiltinKeys 在 host 被覆盖时丢弃内置 key。
func unbindBuiltinKeys(cfg Config) Config {
	if cfg.Sources["langfuse_host"] == SourceBuiltin {
		return cfg
	}
	dropped := false
	if cfg.Sources["public_key"] == SourceBuiltin {
		cfg.PublicKey = ""
		delete(cfg.Sources, "public_key")
		dropped = true
	}
	if cfg.Sources["secret_key"] == SourceBuiltin {
		cfg.SecretKey = ""
		delete(cfg.Sources, "secret_key")
		dropped = true
	}
	if dropped {
		cfg.Warnings = append(cfg.Warnings, "langfuse_host 已被覆盖，内置凭证不会发往非内置地址；请在同一处一并配置 public_key / secret_key")
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

// readFile 返回 (nil, "") 表示文件不存在。先打开再 fstat，校验与读取针对同一个文件。
func readFile(path string, trusted func(os.FileInfo) string) (*fileConfig, string) {
	if path == "" {
		return nil, ""
	}
	f, err := os.Open(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, ""
	}
	if err != nil {
		return nil, fmt.Sprintf("读取配置 %s 失败：%v", path, err)
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return nil, fmt.Sprintf("读取配置 %s 失败：%v", path, err)
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Sprintf("配置 %s 不是普通文件，已忽略", path)
	}
	if reason := trusted(info); reason != "" {
		return nil, fmt.Sprintf("配置 %s %s，已忽略", path, reason)
	}
	var fc fileConfig
	if _, err := toml.NewDecoder(f).Decode(&fc); err != nil {
		return nil, fmt.Sprintf("解析配置 %s 失败：%v", path, err)
	}
	return &fc, ""
}

// managedOwnerUID 是托管配置要求的属主（root），测试中可替换。
var managedOwnerUID uint32 = 0

// userFileTrusted 要求用户配置属于当前用户且只有属主可读写（其中可能有 secret key）。
func userFileTrusted(info os.FileInfo) string {
	if uid, ok := fsutil.OwnerUID(info); ok && uid != uint32(os.Geteuid()) {
		return "不属于当前用户"
	}
	if info.Mode().Perm()&0o077 != 0 {
		return fmt.Sprintf("的 permission 为 %o，过宽；请执行 chmod 600", info.Mode().Perm())
	}
	return ""
}

// managedFileTrusted 要求托管配置属于 root 且组 / 其他用户不可写。
func managedFileTrusted(info os.FileInfo) string {
	if uid, ok := fsutil.OwnerUID(info); ok && uid != managedOwnerUID {
		return "不属于 root"
	}
	if info.Mode().Perm()&0o022 != 0 {
		return "可被其他用户修改"
	}
	return ""
}

func validHost(h string) bool {
	u, err := url.Parse(h)
	if err != nil || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
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

// Mask 用于展示密钥：只保留前缀（如 "sk-lf-"），不暴露任何随机部分。
func Mask(s string) string {
	const keep = 6
	if s == "" {
		return ""
	}
	if len(s) <= keep*2 {
		return "****"
	}
	return s[:keep] + "****"
}
