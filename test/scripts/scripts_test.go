// Package scripts_test 对插件启动脚本与发布构建脚本做黑盒测试。
package scripts_test

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"testing"
)

func repoRoot(t *testing.T) string {
	t.Helper()
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	return root
}

func run(t *testing.T, dir, stdin string, env []string, name string, args ...string) (string, string, int) {
	t.Helper()
	cmd := exec.Command(name, args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), env...)
	cmd.Stdin = strings.NewReader(stdin)
	var out, errOut bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errOut
	err := cmd.Run()
	code := 0
	if ee, ok := err.(*exec.ExitError); ok {
		code = ee.ExitCode()
	} else if err != nil {
		t.Fatal(err)
	}
	return out.String(), errOut.String(), code
}

// pluginCopy 把插件启动脚本复制到带空格的临时路径，并按需放入一个假的平台二进制。
func pluginCopy(t *testing.T, binary string) string {
	t.Helper()
	dst := filepath.Join(t.TempDir(), "path with space", "codex-trace")
	if err := os.MkdirAll(filepath.Join(dst, "bin"), 0o755); err != nil {
		t.Fatal(err)
	}
	src, err := os.ReadFile(filepath.Join(repoRoot(t), "plugins", "codex-trace", "bin", "trace-hook"))
	if err != nil {
		t.Fatal(err)
	}
	launcher := filepath.Join(dst, "bin", "trace-hook")
	if err := os.WriteFile(launcher, src, 0o755); err != nil {
		t.Fatal(err)
	}
	if binary != "" {
		platform := filepath.Join(dst, "bin", runtime.GOOS+"-"+runtime.GOARCH)
		_ = os.MkdirAll(platform, 0o755)
		if err := os.WriteFile(filepath.Join(platform, "trace-hook"), []byte(binary), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	return launcher
}

func TestLauncherExecsPlatformBinary(t *testing.T) {
	launcher := pluginCopy(t, "#!/bin/sh\necho \"real:$*\"\n")
	out, _, code := run(t, t.TempDir(), "", nil, launcher, "status", "--x")
	if code != 0 || strings.TrimSpace(out) != "real:status --x" {
		t.Errorf("code=%d out=%q", code, out)
	}
}

func TestLauncherHookIsSilentWithoutBinary(t *testing.T) {
	launcher := pluginCopy(t, "")
	out, errOut, code := run(t, t.TempDir(), `{"session_id":"s"}`, nil, launcher, "hook", "Stop")
	if code != 0 || out != "" || errOut != "" {
		t.Errorf("hook without binary must be silent: code=%d out=%q err=%q", code, out, errOut)
	}
	_, errOut, code = run(t, t.TempDir(), "", nil, launcher, "status")
	if code != 1 || !strings.Contains(errOut, "没有适用于") {
		t.Errorf("non-hook command should report missing binary: code=%d err=%q", code, errOut)
	}
}

func TestLauncherHookStaysSilentWhenBinaryIsBroken(t *testing.T) {
	launcher := pluginCopy(t, "not an executable format")
	_, errOut, _ := run(t, t.TempDir(), "{}", nil, launcher, "hook", "Stop")
	if errOut != "" {
		t.Errorf("broken binary must not produce stderr in hook mode: %q", errOut)
	}
}

func TestLauncherFallsBackToUname(t *testing.T) {
	launcher := pluginCopy(t, "#!/bin/sh\necho ok\n")
	out, _, code := run(t, t.TempDir(), "", []string{"OSTYPE=bogus", "HOSTTYPE=bogus"}, "/bin/sh", launcher)
	if code != 0 || strings.TrimSpace(out) != "ok" {
		t.Errorf("uname fallback failed: code=%d out=%q", code, out)
	}
}

// bash 在 UTF-8 环境下会把紧跟在 $var 后面的中文字符当作变量名的一部分（set -u 时直接报错），
// 所有脚本中紧挨非 ASCII 字符的变量必须写成 ${var}。
func TestShellScriptsBraceVariablesBeforeNonASCII(t *testing.T) {
	bad := regexp.MustCompile(`\$[A-Za-z_][A-Za-z0-9_]*[^\x00-\x7f]`)
	scripts := []string{"scripts/e2e.sh", "scripts/build-dist.sh", "scripts/capture-hook.sh", "plugins/codex-trace/bin/trace-hook"}
	for _, rel := range scripts {
		data, err := os.ReadFile(filepath.Join(repoRoot(t), rel))
		if err != nil {
			t.Fatal(err)
		}
		for i, line := range strings.Split(string(data), "\n") {
			if bad.MatchString(line) {
				t.Errorf("%s:%d: 变量紧挨非 ASCII 字符，请改为 ${var}: %s", rel, i+1, strings.TrimSpace(line))
			}
		}
	}
}

func buildEnv(t *testing.T, content string, mode os.FileMode) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "build.env")
	if err := os.WriteFile(path, []byte(content), mode); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, mode); err != nil {
		t.Fatal(err)
	}
	return path
}

func buildDist(t *testing.T, env string) (string, int, string) {
	t.Helper()
	out := t.TempDir()
	stdout, stderr, code := run(t, repoRoot(t), "", []string{
		"BUILD_ENV=" + env, "OUT_DIR=" + out, "PLATFORMS=" + runtime.GOOS + "/" + runtime.GOARCH, "VERSION=9.9.9-test",
	}, "sh", "scripts/build-dist.sh")
	return stdout + stderr, code, filepath.Join(out, runtime.GOOS+"-"+runtime.GOARCH, "trace-hook")
}

func TestBuildDistInjectsCredentialsWithoutEchoingSecret(t *testing.T) {
	if testing.Short() {
		t.Skip("builds a binary")
	}
	env := buildEnv(t, "# comment\nCODEX_TRACE_LANGFUSE_HOST=https://lf.example.com\nCODEX_TRACE_LANGFUSE_PUBLIC_KEY=pk-lf-test\n"+
		"CODEX_TRACE_LANGFUSE_SECRET_KEY=sk-lf-very-secret-value\nCODEX_TRACE_ENVIRONMENT=test\n", 0o600)

	output, code, bin := buildDist(t, env)

	if code != 0 {
		t.Fatalf("build failed (%d):\n%s", code, output)
	}
	if strings.Contains(output, "very-secret") {
		t.Error("build output must not echo the secret key")
	}
	cfg, _, _ := run(t, t.TempDir(), "", []string{"CODEX_TRACE_CONFIG=/nonexistent", "CODEX_TRACE_STATE_DIR=" + t.TempDir()}, bin, "config", "show")
	if !strings.Contains(cfg, "https://lf.example.com") || !strings.Contains(cfg, "sk-lf-****") || strings.Contains(cfg, "very-secret") {
		t.Errorf("builtin credentials not injected or not masked:\n%s", cfg)
	}
}

func TestBuildDistInjectsCaptureContentDefault(t *testing.T) {
	if testing.Short() {
		t.Skip("builds a binary")
	}
	cases := map[string]string{
		"enabled":  "CODEX_TRACE_CAPTURE_CONTENT=true\n",
		"disabled": "CODEX_TRACE_CAPTURE_CONTENT=false\n",
		"omitted":  "CODEX_TRACE_ENVIRONMENT=test\n",
	}
	for name, content := range cases {
		t.Run(name, func(t *testing.T) {
			output, code, bin := buildDist(t, buildEnv(t, content, 0o600))
			if code != 0 {
				t.Fatalf("build failed (%d):\n%s", code, output)
			}
			cfg, _, _ := run(t, t.TempDir(), "", []string{"CODEX_TRACE_CONFIG=/nonexistent", "CODEX_TRACE_STATE_DIR=" + t.TempDir()}, bin, "config", "show")
			var line string
			for _, l := range strings.Split(cfg, "\n") {
				if strings.Contains(l, "capture_content") {
					line = l
				}
			}
			want := name == "enabled"
			if got := strings.Contains(line, "true"); got != want {
				t.Errorf("capture_content line = %q, want enabled=%v", line, want)
			}
		})
	}
}

func TestBuildDistRejectsUnsafeEnvFiles(t *testing.T) {
	cases := map[string]struct {
		content string
		mode    os.FileMode
		want    string
	}{
		"group readable":        {"CODEX_TRACE_ENVIRONMENT=x\n", 0o640, "chmod 600"},
		"whitespace":            {"CODEX_TRACE_ENVIRONMENT=a b\n", 0o600, "空白"},
		"unknown key":           {"EVIL=x\n", 0o600, "未知配置项"},
		"invalid capture value": {"CODEX_TRACE_CAPTURE_CONTENT=yes\n", 0o600, "true 或 false"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			output, code, _ := buildDist(t, buildEnv(t, tc.content, tc.mode))
			if code == 0 || !strings.Contains(output, tc.want) {
				t.Errorf("code=%d output:\n%s", code, output)
			}
		})
	}
}

func TestBuildDistNeverExecutesEnvFileContent(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "pwned")
	env := buildEnv(t, "CODEX_TRACE_ENVIRONMENT=$(touch${IFS}"+marker+")\n", 0o600)
	_, _, _ = buildDist(t, env)
	if _, err := os.Stat(marker); err == nil {
		t.Fatal("build.env content must never be executed")
	}
}
