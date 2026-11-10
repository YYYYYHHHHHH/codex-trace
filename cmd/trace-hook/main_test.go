package main

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// buildBinary 编译真实二进制做黑盒测试，覆盖 stdout / 退出码这类只能在进程级验证的约束。
func buildBinary(t *testing.T) string {
	t.Helper()
	bin := filepath.Join(t.TempDir(), "trace-hook")
	out, err := exec.Command("go", "build", "-o", bin, ".").CombinedOutput()
	if err != nil {
		t.Fatalf("go build: %v\n%s", err, out)
	}
	return bin
}

func runBinary(t *testing.T, bin string, env []string, stdin string, args ...string) (string, string, int) {
	t.Helper()
	cmd := exec.Command(bin, args...)
	cmd.Env = env
	cmd.Stdin = strings.NewReader(stdin)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()
	code := 0
	if exitErr, ok := err.(*exec.ExitError); ok {
		code = exitErr.ExitCode()
	} else if err != nil {
		t.Fatal(err)
	}
	return stdout.String(), stderr.String(), code
}

func TestHookBinaryIsSilentAndAlwaysSucceeds(t *testing.T) {
	bin := buildBinary(t)
	state := filepath.Join(t.TempDir(), "state")
	// 新鲜的心跳让 hook 认为上传进程在运行，不去拉起真实的 upload（否则它会消费 spool）
	if err := os.MkdirAll(state, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(state, "upload.heartbeat"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	base := []string{"HOME=" + t.TempDir(), "CODEX_TRACE_STATE_DIR=" + state}
	configured := append(append([]string{}, base...),
		"CODEX_TRACE_LANGFUSE_HOST=http://127.0.0.1:1",
		"CODEX_TRACE_LANGFUSE_PUBLIC_KEY=pk-lf-test",
		"CODEX_TRACE_LANGFUSE_SECRET_KEY=sk-lf-test",
	)

	cases := []struct {
		name  string
		env   []string
		stdin string
	}{
		{"unconfigured", base, `{"session_id":"s"}`},
		{"configured valid", configured, `{"session_id":"s","hook_event_name":"Stop"}`},
		{"configured garbage", configured, "\x00\xffnot json"},
		{"configured empty", configured, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			stdout, stderr, code := runBinary(t, bin, tc.env, tc.stdin, "hook", "Stop")
			if code != 0 || stdout != "" || stderr != "" {
				t.Errorf("code=%d stdout=%q stderr=%q", code, stdout, stderr)
			}
		})
	}

	entries, _ := os.ReadDir(filepath.Join(state, "spool"))
	if len(entries) != 1 {
		t.Errorf("expected exactly one spooled record, got %d", len(entries))
	}
}

func TestUnknownCommandFails(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if code := run([]string{"nope"}, strings.NewReader(""), &stdout, &stderr); code != 2 {
		t.Errorf("code = %d, want 2", code)
	}
	if code := run(nil, strings.NewReader(""), &stdout, &stderr); code != 2 {
		t.Errorf("no args code = %d, want 2", code)
	}
}

func TestVersion(t *testing.T) {
	var stdout bytes.Buffer
	if code := run([]string{"version"}, nil, &stdout, &bytes.Buffer{}); code != 0 || !strings.HasPrefix(stdout.String(), "trace-hook ") {
		t.Errorf("version output %q code %d", stdout.String(), code)
	}
}

func TestUploaderEnvKeepsOnlyAllowlistedVariables(t *testing.T) {
	got := uploaderEnv([]string{
		"HOME=/home/alice",
		"PATH=/usr/bin",
		"HTTPS_PROXY=http://proxy:3128",
		"CODEX_TRACE_STATE_DIR=/tmp/s",
		"ANTHROPIC_AUTH_TOKEN=secret",
		"OPENAI_API_KEY=secret",
		"GITHUB_TOKEN=secret",
	})
	joined := strings.Join(got, "\n")
	for _, want := range []string{"HOME=", "PATH=", "HTTPS_PROXY=", "CODEX_TRACE_STATE_DIR="} {
		if !strings.Contains(joined, want) {
			t.Errorf("missing %s in %v", want, got)
		}
	}
	if strings.Contains(joined, "secret") {
		t.Errorf("unrelated secrets leaked into uploader env: %v", got)
	}
}

func TestUploadWithoutConfigIsNoop(t *testing.T) {
	t.Setenv("CODEX_TRACE_STATE_DIR", t.TempDir())
	t.Setenv("CODEX_TRACE_CONFIG", filepath.Join(t.TempDir(), "none.toml"))
	var stderr bytes.Buffer
	if code := run([]string{"upload", "--once"}, nil, &bytes.Buffer{}, &stderr); code != 0 {
		t.Errorf("code = %d stderr=%q", code, stderr.String())
	}
	if code := run([]string{"upload", "--bogus"}, nil, &bytes.Buffer{}, &stderr); code != 2 {
		t.Errorf("bad flag code = %d", code)
	}
}

func TestResourceAttrsHashInstallationID(t *testing.T) {
	home := t.TempDir()
	_ = os.WriteFile(filepath.Join(home, "installation_id"), []byte("raw-id\n"), 0o600)
	attrs := resourceAttrs(home)
	found := false
	for _, a := range attrs {
		if a.Key == "host.id" {
			found = true
			if a.Value.AsString() == "raw-id" || a.Value.AsString() == "" {
				t.Errorf("host.id must be hashed, got %q", a.Value.AsString())
			}
		}
	}
	if !found {
		t.Error("host.id missing")
	}
}
