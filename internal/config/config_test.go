package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/YYYYYHHHHHH/codex-trace/internal/paths"
)

func testPaths(t *testing.T) paths.Paths {
	t.Helper()
	dir := t.TempDir()
	return paths.Paths{
		StateDir:          filepath.Join(dir, "state"),
		UserConfigFile:    filepath.Join(dir, "user.toml"),
		ManagedConfigFile: filepath.Join(dir, "managed.toml"),
	}
}

func writeFile(t *testing.T, path, content string, mode os.FileMode) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), mode); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, mode); err != nil {
		t.Fatal(err)
	}
}

func noEnv(string) string { return "" }

var testBuiltin = Builtin{
	Host:        "https://builtin.example.com",
	PublicKey:   "pk-lf-builtin",
	SecretKey:   "sk-lf-builtin",
	Environment: "builtin-env",
}

func TestLoadUsesBuiltinDefaults(t *testing.T) {
	cfg := Load(testPaths(t), noEnv, testBuiltin)

	if !cfg.Configured() {
		t.Fatal("expected configured from builtin values")
	}
	if cfg.Host != "https://builtin.example.com" || cfg.SecretKey != "sk-lf-builtin" {
		t.Errorf("unexpected builtin config: %+v", cfg)
	}
	if !cfg.Enabled {
		t.Error("enabled should default to true")
	}
	if cfg.CaptureContent {
		t.Error("capture_content should default to false")
	}
	if cfg.Sources["secret_key"] != SourceBuiltin {
		t.Errorf("secret_key source = %q, want builtin", cfg.Sources["secret_key"])
	}
}

func TestLoadWithoutAnyCredentialsIsNotConfigured(t *testing.T) {
	cfg := Load(testPaths(t), noEnv, Builtin{})
	if cfg.Configured() {
		t.Error("expected not configured")
	}
}

func TestUserFileOverridesBuiltin(t *testing.T) {
	p := testPaths(t)
	writeFile(t, p.UserConfigFile, `
langfuse_host = "https://user.example.com/"
capture_content = true
enabled = false
`, 0o600)

	cfg := Load(p, noEnv, testBuiltin)

	if cfg.Host != "https://user.example.com" {
		t.Errorf("Host = %q, want trailing slash trimmed user value", cfg.Host)
	}
	if !cfg.CaptureContent || cfg.Enabled {
		t.Errorf("bool overrides not applied: %+v", cfg)
	}
	if cfg.PublicKey != "pk-lf-builtin" {
		t.Errorf("unset fields should keep builtin value, got %q", cfg.PublicKey)
	}
	if cfg.Sources["langfuse_host"] != SourceUser {
		t.Errorf("host source = %q, want user", cfg.Sources["langfuse_host"])
	}
}

func TestUserFileWithLoosePermissionsIsIgnored(t *testing.T) {
	p := testPaths(t)
	writeFile(t, p.UserConfigFile, `secret_key = "sk-lf-leaky"`, 0o644)

	cfg := Load(p, noEnv, testBuiltin)

	if cfg.SecretKey != "sk-lf-builtin" {
		t.Errorf("loose-permission file must be ignored, got %q", cfg.SecretKey)
	}
	if !containsWarning(cfg.Warnings, "permission") {
		t.Errorf("expected permission warning, got %v", cfg.Warnings)
	}
}

func TestManagedOverridesUser(t *testing.T) {
	p := testPaths(t)
	writeFile(t, p.UserConfigFile, `environment = "user-env"`, 0o600)
	writeFile(t, p.ManagedConfigFile, `environment = "managed-env"`, 0o644)

	cfg := Load(p, noEnv, testBuiltin)

	if cfg.Environment != "managed-env" {
		t.Errorf("Environment = %q, want managed-env", cfg.Environment)
	}
	if cfg.Sources["environment"] != SourceManaged {
		t.Errorf("source = %q, want managed", cfg.Sources["environment"])
	}
}

func TestEnvOverridesEverything(t *testing.T) {
	p := testPaths(t)
	writeFile(t, p.ManagedConfigFile, `public_key = "pk-lf-managed"`, 0o644)
	env := map[string]string{
		"CODEX_TRACE_LANGFUSE_PUBLIC_KEY": "pk-lf-env",
		"CODEX_TRACE_CAPTURE_CONTENT":     "true",
		"CODEX_TRACE_ENABLED":             "false",
	}

	cfg := Load(p, func(k string) string { return env[k] }, testBuiltin)

	if cfg.PublicKey != "pk-lf-env" || !cfg.CaptureContent || cfg.Enabled {
		t.Errorf("env overrides not applied: %+v", cfg)
	}
	if cfg.Sources["public_key"] != SourceEnv {
		t.Errorf("source = %q, want env", cfg.Sources["public_key"])
	}
}

func TestInvalidEnvBoolIsIgnoredWithWarning(t *testing.T) {
	env := map[string]string{"CODEX_TRACE_ENABLED": "maybe"}
	cfg := Load(testPaths(t), func(k string) string { return env[k] }, testBuiltin)

	if !cfg.Enabled {
		t.Error("invalid bool must not change the value")
	}
	if !containsWarning(cfg.Warnings, "CODEX_TRACE_ENABLED") {
		t.Errorf("expected warning about invalid bool, got %v", cfg.Warnings)
	}
}

func TestMalformedFileIsIgnoredWithWarning(t *testing.T) {
	p := testPaths(t)
	writeFile(t, p.UserConfigFile, `this is = = not toml`, 0o600)

	cfg := Load(p, noEnv, testBuiltin)

	if cfg.Host != testBuiltin.Host {
		t.Errorf("malformed file must be ignored, got host %q", cfg.Host)
	}
	if len(cfg.Warnings) == 0 {
		t.Error("expected parse warning")
	}
}

func TestInsecureHostIsRejected(t *testing.T) {
	cases := map[string]bool{
		"https://cloud.langfuse.com":  true,
		"http://127.0.0.1:4318":       true,
		"http://localhost:4318":       true,
		"http://langfuse.example.com": false,
		"ftp://langfuse.example.com":  false,
		"not a url":                   false,
		"https://":                    false,
	}
	for host, want := range cases {
		b := testBuiltin
		b.Host = host
		cfg := Load(testPaths(t), noEnv, b)
		if cfg.Configured() != want {
			t.Errorf("host %q: Configured() = %v, want %v", host, cfg.Configured(), want)
		}
	}
}

func TestMask(t *testing.T) {
	cases := map[string]string{
		"":                       "",
		"short":                  "****",
		"sk-lf-1234567890abcdef": "sk-l…cdef",
	}
	for in, want := range cases {
		if got := Mask(in); got != want {
			t.Errorf("Mask(%q) = %q, want %q", in, got, want)
		}
	}
}

func containsWarning(ws []string, sub string) bool {
	for _, w := range ws {
		if strings.Contains(w, sub) {
			return true
		}
	}
	return false
}
