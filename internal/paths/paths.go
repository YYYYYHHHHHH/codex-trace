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
func (p Paths) HeartbeatFile() string { return filepath.Join(p.StateDir, "upload.heartbeat") }
func (p Paths) StatusFile() string    { return filepath.Join(p.StateDir, "status.json") }
func (p Paths) LastBatchFile() string { return filepath.Join(p.LogsDir(), "last-batch.json") }

// Resolve 按 显式环境变量 → XDG → 默认 的顺序解析位置；只接受绝对路径。
// 托管配置路径固定，不允许通过环境变量替换，否则托管策略形同虚设。
// home 为空时返回零值，调用方应视为不可用。
func Resolve(getenv func(string) string, home, goos string) Paths {
	if home == "" || !filepath.IsAbs(home) {
		return Paths{}
	}
	return Paths{
		StateDir: firstNonEmpty(
			absOrEmpty(getenv("CODEX_TRACE_STATE_DIR")),
			xdgDir(getenv("XDG_STATE_HOME"), home, ".local/state"),
		),
		UserConfigFile: firstNonEmpty(
			absOrEmpty(getenv("CODEX_TRACE_CONFIG")),
			filepath.Join(xdgDir(getenv("XDG_CONFIG_HOME"), home, ".config"), "config.toml"),
		),
		ManagedConfigFile: managedConfigPath(goos),
	}
}

// FromEnv 使用当前进程环境解析位置；无法确定 HOME 时返回零值（不回退到共享的 /tmp）。
func FromEnv() Paths {
	home, err := os.UserHomeDir()
	if err != nil {
		return Paths{}
	}
	return Resolve(os.Getenv, home, runtime.GOOS)
}

// Usable 表示路径解析成功。
func (p Paths) Usable() bool { return p.StateDir != "" }

func absOrEmpty(p string) string {
	if filepath.IsAbs(p) {
		return filepath.Clean(p)
	}
	return ""
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
