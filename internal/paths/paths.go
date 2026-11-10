// Package paths 解析 codex-trace 的本地状态目录与配置文件位置。
package paths

import (
	"os"
	"path/filepath"
	"runtime"
)

const appName = "codex-trace"

// Paths 是一次解析得到的全部位置，不随进程环境变化。
type Paths struct {
	StateDir          string
	UserConfigFile    string
	ManagedConfigFile string
}

func (p Paths) SpoolDir() string      { return filepath.Join(p.StateDir, "spool") }
func (p Paths) OutboxDir() string     { return filepath.Join(p.StateDir, "outbox") }
func (p Paths) FailedDir() string     { return filepath.Join(p.StateDir, "failed") }
func (p Paths) SessionsDir() string   { return filepath.Join(p.StateDir, "sessions") }
func (p Paths) LogsDir() string       { return filepath.Join(p.StateDir, "logs") }
func (p Paths) LockFile() string      { return filepath.Join(p.StateDir, "upload.lock") }
func (p Paths) PausedFile() string    { return filepath.Join(p.StateDir, "paused") }
func (p Paths) LastHookFile() string  { return filepath.Join(p.StateDir, "last-hook") }
func (p Paths) StatusFile() string    { return filepath.Join(p.StateDir, "status.json") }
func (p Paths) LastBatchFile() string { return filepath.Join(p.LogsDir(), "last-batch.json") }

// Resolve 按 显式环境变量 → XDG → 默认 的顺序解析位置。
func Resolve(getenv func(string) string, home, goos string) Paths {
	return Paths{
		StateDir: firstNonEmpty(
			getenv("CODEX_TRACE_STATE_DIR"),
			xdgDir(getenv("XDG_STATE_HOME"), home, ".local/state"),
		),
		UserConfigFile: firstNonEmpty(
			getenv("CODEX_TRACE_CONFIG"),
			filepath.Join(xdgDir(getenv("XDG_CONFIG_HOME"), home, ".config"), "config.toml"),
		),
		ManagedConfigFile: firstNonEmpty(
			getenv("CODEX_TRACE_MANAGED_CONFIG"),
			managedConfigPath(goos),
		),
	}
}

// FromEnv 使用当前进程环境解析位置。
func FromEnv() Paths {
	home, err := os.UserHomeDir()
	if err != nil {
		home = os.TempDir()
	}
	return Resolve(os.Getenv, home, runtime.GOOS)
}

func xdgDir(base, home, fallback string) string {
	if base == "" || !filepath.IsAbs(base) {
		base = filepath.Join(home, fallback)
	}
	return filepath.Join(base, appName)
}

func managedConfigPath(goos string) string {
	if goos == "darwin" {
		return "/Library/Application Support/codex-trace/config.toml"
	}
	return "/etc/codex-trace/config.toml"
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}
