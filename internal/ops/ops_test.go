package ops

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/YYYYYHHHHHH/codex-trace/internal/config"
	"github.com/YYYYYHHHHHH/codex-trace/internal/otlp"
	"github.com/YYYYYHHHHHH/codex-trace/internal/paths"
	"github.com/YYYYYHHHHHH/codex-trace/internal/queue"
	"github.com/YYYYYHHHHHH/codex-trace/internal/status"
)

var now = time.Date(2026, 11, 9, 12, 0, 0, 0, time.UTC)

type harness struct {
	env Env
	out *bytes.Buffer
}

func newHarness(t *testing.T, host string) *harness {
	t.Helper()
	dir := t.TempDir()
	out := &bytes.Buffer{}
	codexHome := filepath.Join(dir, "codex")
	_ = os.MkdirAll(codexHome, 0o700)
	return &harness{out: out, env: Env{
		Paths: paths.Paths{
			StateDir:          filepath.Join(dir, "state"),
			UserConfigFile:    filepath.Join(dir, "user.toml"),
			ManagedConfigFile: filepath.Join(dir, "managed.toml"),
		},
		Getenv:    func(string) string { return "" },
		Builtin:   config.Builtin{Host: host, PublicKey: "pk-lf-public", SecretKey: "sk-lf-supersecretvalue"},
		Version:   "test",
		CodexHome: codexHome,
		Now:       func() time.Time { return now },
		HTTP:      http.DefaultClient,
		Out:       out,
	}}
}

func TestPauseResume(t *testing.T) {
	h := newHarness(t, "https://lf.example.com")
	if err := Pause(h.env); err != nil {
		t.Fatal(err)
	}
	if !exists(h.env.Paths.PausedFile()) {
		t.Fatal("paused marker missing")
	}
	_ = Status(h.env)
	if !strings.Contains(h.out.String(), "已暂停") {
		t.Errorf("status should report paused:\n%s", h.out)
	}
	if err := Resume(h.env); err != nil || exists(h.env.Paths.PausedFile()) {
		t.Errorf("resume failed: %v", err)
	}
	if err := Resume(h.env); err != nil {
		t.Errorf("resume twice should be fine: %v", err)
	}
}

func TestConfigShowMasksSecrets(t *testing.T) {
	h := newHarness(t, "https://lf.example.com")
	if err := ConfigShow(h.env); err != nil {
		t.Fatal(err)
	}
	out := h.out.String()
	if strings.Contains(out, "supersecretvalue") {
		t.Error("secret key must be masked")
	}
	for _, want := range []string{"https://lf.example.com", "sk-lf-****", "内置"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
}

func TestStatusShowsUploadState(t *testing.T) {
	h := newHarness(t, "https://lf.example.com")
	_ = os.MkdirAll(h.env.Paths.StateDir, 0o700)
	_ = status.Write(h.env.Paths.StatusFile(), status.Status{
		SentSpans: 42, SentBatches: 3, LastSendOK: now.Add(-time.Minute),
		LastError: "exporter: HTTP 503: busy", LastErrorAt: now.Add(-30 * time.Second),
		NextRetryAt: now.Add(10 * time.Second),
	})
	_ = os.WriteFile(h.env.Paths.LastHookFile(), []byte(now.Add(-5*time.Second).Format(time.RFC3339Nano)+" PostToolUse\n"), 0o600)
	_ = os.WriteFile(h.env.Paths.HeartbeatFile(), nil, 0o600)
	_ = os.Chtimes(h.env.Paths.HeartbeatFile(), now, now)

	if err := Status(h.env); err != nil {
		t.Fatal(err)
	}
	out := h.out.String()
	for _, want := range []string{"开启", "42 个 span", "HTTP 503", "5 秒前（PostToolUse）", "运行中", "下次重试", "1 分钟前"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
}

func TestStatusWhenUnconfigured(t *testing.T) {
	h := newHarness(t, "")
	h.env.Builtin = config.Builtin{}
	_ = Status(h.env)
	if !strings.Contains(h.out.String(), "未配置凭证") || !strings.Contains(h.out.String(), "从未触发") {
		t.Errorf("unexpected:\n%s", h.out)
	}
}

func lfServer(t *testing.T, code int) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(code)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestDoctorHealthy(t *testing.T) {
	srv := lfServer(t, 200)
	h := newHarness(t, srv.URL)
	writeCodexConfig(t, h, true)
	_ = os.MkdirAll(h.env.Paths.StateDir, 0o700)
	_ = os.WriteFile(h.env.Paths.LastHookFile(), []byte(now.Format(time.RFC3339Nano)+" Stop\n"), 0o600)
	_ = status.Write(h.env.Paths.StatusFile(), status.Status{LastSendOK: now})

	err := Doctor(context.Background(), h.env, DoctorOptions{})

	if err != nil {
		t.Errorf("healthy setup should pass, got %v\n%s", err, h.out)
	}
	if strings.Count(h.out.String(), "✓") != 7 {
		t.Errorf("expected 7 passing checks:\n%s", h.out)
	}
}

func TestDoctorDetectsBadCredentials(t *testing.T) {
	srv := lfServer(t, 401)
	h := newHarness(t, srv.URL)

	err := Doctor(context.Background(), h.env, DoctorOptions{})

	if !errors.Is(err, ErrDoctorFailed) || !strings.Contains(h.out.String(), "凭证被拒绝") {
		t.Errorf("err=%v\n%s", err, h.out)
	}
	if strings.Contains(h.out.String(), "supersecretvalue") {
		t.Error("doctor output must not contain the secret")
	}
}

func TestDoctorDetectsNetworkFailure(t *testing.T) {
	srv := lfServer(t, 200)
	url := srv.URL
	srv.Close()
	h := newHarness(t, url)

	err := Doctor(context.Background(), h.env, DoctorOptions{})

	if !errors.Is(err, ErrDoctorFailed) || !strings.Contains(h.out.String(), "网络不通") {
		t.Errorf("err=%v\n%s", err, h.out)
	}
}

func TestDoctorUnconfiguredAndOffline(t *testing.T) {
	h := newHarness(t, "")
	h.env.Builtin = config.Builtin{}

	err := Doctor(context.Background(), h.env, DoctorOptions{Offline: true})

	out := h.out.String()
	if !errors.Is(err, ErrDoctorFailed) || !strings.Contains(out, "没有可用凭证") || !strings.Contains(out, "--offline") {
		t.Errorf("err=%v\n%s", err, out)
	}
}

func TestDoctorReportsUploadFailure(t *testing.T) {
	h := newHarness(t, "https://lf.example.com")
	_ = os.MkdirAll(h.env.Paths.StateDir, 0o700)
	_ = status.Write(h.env.Paths.StatusFile(), status.Status{LastError: "exporter: HTTP 401: invalid", LastErrorAt: now})

	_ = Doctor(context.Background(), h.env, DoctorOptions{Offline: true})

	if !strings.Contains(h.out.String(), "✗ 上传进程") {
		t.Errorf("upload failure should fail the check:\n%s", h.out)
	}
}

func writeCodexConfig(t *testing.T, h *harness, trusted bool) {
	t.Helper()
	content := "[plugins.\"codex-trace@codex-trace\"]\nenabled = true\n"
	// 无关路径中恰好含 codex-trace 的 hook 不应被计入
	content += "\n[hooks.state.\"/Users/alice/Code/codex-trace/.codex/hooks.json:stop:0:0\"]\ntrusted_hash = \"sha256:y\"\n"
	if trusted {
		for _, ev := range []string{"session_start", "user_prompt_submit", "pre_tool_use", "post_tool_use", "permission_request", "subagent_start", "subagent_stop", "stop", "interrupt"} {
			content += "\n[hooks.state.\"plugin:codex-trace@codex-trace:" + ev + ":0:0\"]\ntrusted_hash = \"sha256:x\"\n"
		}
	}
	if err := os.WriteFile(filepath.Join(h.env.CodexHome, "config.toml"), []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestCheckPlugin(t *testing.T) {
	h := newHarness(t, "https://lf.example.com")
	if c := checkPlugin(h.env); c.level != levelWarn {
		t.Errorf("missing config.toml should warn: %+v", c)
	}
	writeCodexConfig(t, h, false)
	if c := checkPlugin(h.env); c.level != levelWarn || !strings.Contains(c.detail, "审批") {
		t.Errorf("installed but untrusted should warn about approval: %+v", c)
	}
	writeCodexConfig(t, h, true)
	if c := checkPlugin(h.env); c.level != levelOK {
		t.Errorf("trusted plugin should pass: %+v", c)
	}
}

func TestPreview(t *testing.T) {
	h := newHarness(t, "https://lf.example.com")
	if err := Preview(h.env, PreviewOptions{}); err == nil {
		t.Error("preview without data should error")
	}

	root := otlp.Span{TraceID: otlp.NewTraceID("t"), SpanID: otlp.NewSpanID("turn", "t"), Name: "codex turn",
		Attrs: []otlp.Attr{otlp.String("langfuse.observation.type", "agent"), otlp.String("langfuse.observation.input", "hello")}}
	child := otlp.Span{TraceID: root.TraceID, SpanID: otlp.NewSpanID("tool", "c"), Parent: root.SpanID, Name: "Bash",
		Attrs: []otlp.Attr{otlp.String("langfuse.observation.type", "tool"), otlp.Int("langfuse.observation.metadata.exit_code", 1)}}
	body, _ := otlp.Encode(nil, "codex-trace", "test", []otlp.Span{root, child})
	if _, err := queue.New(h.env.Paths.OutboxDir()).Put(body, now); err != nil {
		t.Fatal(err)
	}

	if err := Preview(h.env, PreviewOptions{}); err != nil {
		t.Fatal(err)
	}
	out := h.out.String()
	for _, want := range []string{"待上传", "- [agent] codex turn", "  - [tool] Bash", "input(5 字节)", "共 2 个 span"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}

	h.out.Reset()
	if err := Preview(h.env, PreviewOptions{Raw: true}); err != nil || !strings.Contains(h.out.String(), `"resourceSpans"`) {
		t.Errorf("raw preview failed: %v\n%s", err, h.out)
	}
}

func TestPreviewFallsBackToLastBatch(t *testing.T) {
	h := newHarness(t, "https://lf.example.com")
	_ = os.MkdirAll(h.env.Paths.LogsDir(), 0o700)
	body, _ := otlp.Encode(nil, "s", "v", nil)
	_ = os.WriteFile(h.env.Paths.LastBatchFile(), body, 0o600)
	if err := Preview(h.env, PreviewOptions{}); err != nil || !strings.Contains(h.out.String(), "最近一次已上传") {
		t.Errorf("err=%v\n%s", err, h.out)
	}
}

func TestAgo(t *testing.T) {
	cases := map[time.Duration]string{
		10 * time.Second: "10 秒前",
		5 * time.Minute:  "5 分钟前",
		3 * time.Hour:    "3 小时前",
		72 * time.Hour:   "3 天前",
	}
	for d, want := range cases {
		if got := ago(now.Add(-d), now); got != want {
			t.Errorf("ago(%v) = %q, want %q", d, got, want)
		}
	}
	if ago(time.Time{}, now) != "无" {
		t.Error("zero time")
	}
}

func TestCheckPluginPartialApproval(t *testing.T) {
	h := newHarness(t, "https://lf.example.com")
	content := "[plugins.\"codex-trace@codex-trace\"]\nenabled = true\n" +
		"\n[hooks.state.\"plugin:codex-trace@codex-trace:stop:0:0\"]\ntrusted_hash = \"sha256:x\"\n"
	_ = os.WriteFile(filepath.Join(h.env.CodexHome, "config.toml"), []byte(content), 0o600)
	if c := checkPlugin(h.env); c.level != levelWarn || !strings.Contains(c.detail, "1/9") {
		t.Errorf("partial approval should warn: %+v", c)
	}
}

func TestCheckStateDirHasNoSideEffects(t *testing.T) {
	h := newHarness(t, "https://lf.example.com")
	if c := checkStateDir(h.env); c.level != levelOK || !strings.Contains(c.detail, "尚未创建") {
		t.Errorf("missing dir: %+v", c)
	}
	if _, err := os.Stat(h.env.Paths.StateDir); !os.IsNotExist(err) {
		t.Error("doctor must not create the state dir")
	}
	_ = os.Mkdir(h.env.Paths.StateDir, 0o755)
	_ = os.Chmod(h.env.Paths.StateDir, 0o755)
	if c := checkStateDir(h.env); c.level != levelWarn {
		t.Errorf("loose perms should warn: %+v", c)
	}
	if info, _ := os.Stat(h.env.Paths.StateDir); info.Mode().Perm() != 0o755 {
		t.Error("doctor must not change permissions")
	}
}

func TestPreviewShowsContentSizesOnly(t *testing.T) {
	h := newHarness(t, "https://lf.example.com")
	root := otlp.Span{TraceID: otlp.NewTraceID("t"), SpanID: otlp.NewSpanID("turn", "t"), Name: "codex turn",
		Attrs: []otlp.Attr{otlp.String("langfuse.observation.input", "TOP-SECRET-PROMPT")}}
	body, _ := otlp.Encode(nil, "s", "v", []otlp.Span{root})
	_, _ = queue.New(h.env.Paths.OutboxDir()).Put(body, now)
	_ = Preview(h.env, PreviewOptions{})
	if strings.Contains(h.out.String(), "TOP-SECRET-PROMPT") {
		t.Error("tree preview must show content sizes, not content")
	}
}
