package hook

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/YYYYYHHHHHH/codex-trace/internal/config"
	"github.com/YYYYYHHHHHH/codex-trace/internal/event"
	"github.com/YYYYYHHHHHH/codex-trace/internal/lock"
	"github.com/YYYYYHHHHHH/codex-trace/internal/paths"
	"github.com/YYYYYHHHHHH/codex-trace/internal/queue"
)

var creds = config.Builtin{Host: "https://lf.example.com", PublicKey: "pk-lf-x", SecretKey: "sk-lf-x"}

type harness struct {
	paths    paths.Paths
	started  int
	startErr error
}

func newHarness(t *testing.T) *harness {
	dir := t.TempDir()
	return &harness{paths: paths.Paths{
		StateDir:          filepath.Join(dir, "state"),
		UserConfigFile:    filepath.Join(dir, "user.toml"),
		ManagedConfigFile: filepath.Join(dir, "managed.toml"),
	}}
}

func (h *harness) deps(stdin string, builtin config.Builtin) Deps {
	return Deps{
		Stdin:   strings.NewReader(stdin),
		Args:    []string{"PreToolUse"},
		Getenv:  func(string) string { return "" },
		Now:     func() time.Time { return time.Date(2026, 11, 9, 10, 0, 0, 0, time.UTC) },
		Paths:   h.paths,
		Builtin: builtin,
		StartUploader: func() error {
			h.started++
			return h.startErr
		},
	}
}

func (h *harness) spool(t *testing.T) []queue.Entry {
	t.Helper()
	entries, err := queue.New(h.paths.SpoolDir()).List()
	if err != nil {
		t.Fatal(err)
	}
	return entries
}

const validPayload = `{"session_id":"s1","turn_id":"t1","hook_event_name":"PreToolUse","tool_use_id":"call_1"}`

func TestRunWritesSpoolRecordAndStartsUploader(t *testing.T) {
	h := newHarness(t)

	Run(h.deps(validPayload, creds))

	entries := h.spool(t)
	if len(entries) != 1 {
		t.Fatalf("expected 1 spool entry, got %d", len(entries))
	}
	data, _ := queue.New(h.paths.SpoolDir()).Read(entries[0].Name)
	rec, err := event.ParseRecord(data)
	if err != nil {
		t.Fatal(err)
	}
	if rec.Event != "PreToolUse" || !rec.ReceivedAt.Equal(time.Date(2026, 11, 9, 10, 0, 0, 0, time.UTC)) {
		t.Errorf("unexpected record: %+v", rec)
	}
	var got, want any
	_ = json.Unmarshal(rec.Payload, &got)
	_ = json.Unmarshal([]byte(validPayload), &want)
	if !jsonEqual(got, want) {
		t.Errorf("payload changed: %s", rec.Payload)
	}
	if h.started != 1 {
		t.Errorf("uploader started %d times, want 1", h.started)
	}
	if _, err := os.Stat(h.paths.LastHookFile()); err != nil {
		t.Errorf("last-hook marker missing: %v", err)
	}
}

func TestRunIsSilentWhenNotConfigured(t *testing.T) {
	h := newHarness(t)

	Run(h.deps(validPayload, config.Builtin{}))

	if _, err := os.Stat(h.paths.StateDir); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("state dir must not be created when unconfigured: %v", err)
	}
	if h.started != 0 {
		t.Error("uploader must not start when unconfigured")
	}
}

func TestRunIsSilentWhenDisabled(t *testing.T) {
	h := newHarness(t)
	d := h.deps(validPayload, creds)
	d.Getenv = func(k string) string {
		if k == "CODEX_TRACE_ENABLED" {
			return "false"
		}
		return ""
	}

	Run(d)

	if len(h.spool(t)) != 0 || h.started != 0 {
		t.Error("disabled config must not capture")
	}
}

func TestRunIsSilentWhenPaused(t *testing.T) {
	h := newHarness(t)
	_ = os.MkdirAll(h.paths.StateDir, 0o700)
	_ = os.WriteFile(h.paths.PausedFile(), nil, 0o600)

	Run(h.deps(validPayload, creds))

	if len(h.spool(t)) != 0 || h.started != 0 {
		t.Error("paused must not capture")
	}
}

func TestRunDropsInvalidInput(t *testing.T) {
	for name, stdin := range map[string]string{
		"empty":     "",
		"not json":  "hello",
		"array":     `[1,2,3]`,
		"truncated": `{"session_id":`,
	} {
		t.Run(name, func(t *testing.T) {
			h := newHarness(t)
			Run(h.deps(stdin, creds))
			if len(h.spool(t)) != 0 {
				t.Errorf("invalid input %q must not be spooled", stdin)
			}
		})
	}
}

func TestRunDrainsAndDropsOversizedInput(t *testing.T) {
	h := newHarness(t)
	big := `{"session_id":"s","x":"` + strings.Repeat("a", MaxPayloadBytes) + `"}`
	r := strings.NewReader(big)
	d := h.deps("", creds)
	d.Stdin = r

	Run(d)

	if r.Len() != 0 {
		t.Errorf("stdin not fully drained, %d bytes left", r.Len())
	}
	if len(h.spool(t)) != 0 {
		t.Error("oversized payload must not be spooled")
	}
}

func TestRunDoesNotStartUploaderWhenAlreadyRunning(t *testing.T) {
	h := newHarness(t)
	_ = os.MkdirAll(h.paths.StateDir, 0o700)
	release, ok, err := lock.TryLock(h.paths.LockFile())
	if err != nil || !ok {
		t.Fatalf("TryLock: %v %v", ok, err)
	}
	defer release()

	Run(h.deps(validPayload, creds))

	if h.started != 0 {
		t.Error("uploader must not be started while lock is held")
	}
	if len(h.spool(t)) != 1 {
		t.Error("payload should still be spooled")
	}
}

func TestRunLogsUploaderStartFailure(t *testing.T) {
	h := newHarness(t)
	h.startErr = errors.New("boom")

	Run(h.deps(validPayload, creds))

	data, _ := os.ReadFile(filepath.Join(h.paths.LogsDir(), "hook.log"))
	if !bytes.Contains(data, []byte("boom")) {
		t.Errorf("expected failure to be logged, got %q", data)
	}
}

func TestRunRecoversFromPanics(t *testing.T) {
	h := newHarness(t)
	d := h.deps(validPayload, creds)
	d.StartUploader = func() error { panic("unexpected") }

	Run(d) // must not panic
}

func TestEventNameFallsBackToPayload(t *testing.T) {
	h := newHarness(t)
	d := h.deps(validPayload, creds)
	d.Args = nil

	Run(d)

	data, _ := queue.New(h.paths.SpoolDir()).Read(h.spool(t)[0].Name)
	rec, _ := event.ParseRecord(data)
	if rec.Event != "PreToolUse" {
		t.Errorf("Event = %q, want PreToolUse from payload", rec.Event)
	}
}

func FuzzRun(f *testing.F) {
	f.Add([]byte(validPayload))
	f.Add([]byte(""))
	f.Add([]byte("{"))
	f.Add([]byte(`{"session_id":null}`))
	f.Fuzz(func(t *testing.T, data []byte) {
		h := newHarness(t)
		d := h.deps("", creds)
		d.Stdin = bytes.NewReader(data)
		Run(d)
	})
}

func BenchmarkRun(b *testing.B) {
	dir := b.TempDir()
	p := paths.Paths{StateDir: filepath.Join(dir, "state"), UserConfigFile: filepath.Join(dir, "u.toml"), ManagedConfigFile: filepath.Join(dir, "m.toml")}
	payload, _ := os.ReadFile(filepath.Join("..", "..", "testdata", "hook-payloads", "PostToolUse-Bash.json"))
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		Run(Deps{
			Stdin:         bytes.NewReader(payload),
			Args:          []string{"PostToolUse"},
			Getenv:        func(string) string { return "" },
			Now:           time.Now,
			Paths:         p,
			Builtin:       creds,
			StartUploader: func() error { return nil },
		})
	}
}

func jsonEqual(a, b any) bool {
	ja, _ := json.Marshal(a)
	jb, _ := json.Marshal(b)
	return bytes.Equal(ja, jb)
}
