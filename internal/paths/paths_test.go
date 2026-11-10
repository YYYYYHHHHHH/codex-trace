package paths

import (
	"path/filepath"
	"testing"
)

func envOf(m map[string]string) func(string) string {
	return func(k string) string { return m[k] }
}

func TestResolveDefaults(t *testing.T) {
	p := Resolve(envOf(nil), "/home/alice", "linux")

	if want := "/home/alice/.local/state/codex-trace"; p.StateDir != want {
		t.Errorf("StateDir = %q, want %q", p.StateDir, want)
	}
	if want := "/home/alice/.config/codex-trace/config.toml"; p.UserConfigFile != want {
		t.Errorf("UserConfigFile = %q, want %q", p.UserConfigFile, want)
	}
	if want := "/etc/codex-trace/config.toml"; p.ManagedConfigFile != want {
		t.Errorf("ManagedConfigFile = %q, want %q", p.ManagedConfigFile, want)
	}
}

func TestResolveDarwinManagedPath(t *testing.T) {
	p := Resolve(envOf(nil), "/Users/alice", "darwin")
	if want := "/Library/Application Support/codex-trace/config.toml"; p.ManagedConfigFile != want {
		t.Errorf("ManagedConfigFile = %q, want %q", p.ManagedConfigFile, want)
	}
}

func TestResolveHonorsXDG(t *testing.T) {
	p := Resolve(envOf(map[string]string{
		"XDG_STATE_HOME":  "/xdg/state",
		"XDG_CONFIG_HOME": "/xdg/config",
	}), "/home/alice", "linux")

	if want := "/xdg/state/codex-trace"; p.StateDir != want {
		t.Errorf("StateDir = %q, want %q", p.StateDir, want)
	}
	if want := "/xdg/config/codex-trace/config.toml"; p.UserConfigFile != want {
		t.Errorf("UserConfigFile = %q, want %q", p.UserConfigFile, want)
	}
}

func TestResolveIgnoresRelativeXDG(t *testing.T) {
	p := Resolve(envOf(map[string]string{"XDG_STATE_HOME": "relative/dir"}), "/home/alice", "linux")
	if want := "/home/alice/.local/state/codex-trace"; p.StateDir != want {
		t.Errorf("StateDir = %q, want %q", p.StateDir, want)
	}
}

func TestResolveExplicitOverrides(t *testing.T) {
	p := Resolve(envOf(map[string]string{
		"CODEX_TRACE_STATE_DIR":      "/tmp/state",
		"CODEX_TRACE_CONFIG":         "/tmp/user.toml",
		"CODEX_TRACE_MANAGED_CONFIG": "/tmp/managed.toml",
	}), "/home/alice", "linux")

	if p.StateDir != "/tmp/state" || p.UserConfigFile != "/tmp/user.toml" || p.ManagedConfigFile != "/tmp/managed.toml" {
		t.Errorf("explicit overrides not applied: %+v", p)
	}
}

func TestDerivedPathsLiveUnderStateDir(t *testing.T) {
	p := Paths{StateDir: "/s"}
	for name, got := range map[string]string{
		"spool":    p.SpoolDir(),
		"outbox":   p.OutboxDir(),
		"failed":   p.FailedDir(),
		"sessions": p.SessionsDir(),
		"logs":     p.LogsDir(),
	} {
		if filepath.Dir(got) != "/s" {
			t.Errorf("%s dir %q not directly under state dir", name, got)
		}
	}
	for _, f := range []string{p.LockFile(), p.PausedFile(), p.LastHookFile(), p.StatusFile()} {
		if filepath.Dir(f) != "/s" {
			t.Errorf("file %q not directly under state dir", f)
		}
	}
	if filepath.Dir(p.LastBatchFile()) != p.LogsDir() {
		t.Errorf("last batch file %q not under logs dir", p.LastBatchFile())
	}
}
