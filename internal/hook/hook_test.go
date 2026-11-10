package hook

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/YYYYYHHHHHH/codex-trace/internal/config"
	"github.com/YYYYYHHHHHH/codex-trace/internal/event"
	"github.com/YYYYYHHHHHH/codex-trace/internal/paths"
	"github.com/YYYYYHHHHHH/codex-trace/internal/queue"
)

var creds = config.Builtin{Host: "https://lf.example.com", PublicKey: "pk-lf-x", SecretKey: "sk-lf-x"}

type harness struct {
	mu       sync.Mutex
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
			h.mu.Lock()
			defer h.mu.Unlock()
			h.started++
			return h.startErr
		},
		Rand: func() float64 { return 0.99 },
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

func TestRunDoesNotStartUploaderWhenHeartbeatIsFresh(t *testing.T) {
	h := newHarness(t)
	d := h.deps(validPayload, creds)
	_ = os.MkdirAll(h.paths.StateDir, 0o700)
	_ = os.WriteFile(h.paths.HeartbeatFile(), nil, 0o600)
	now := time.Now()
	_ = os.Chtimes(h.paths.HeartbeatFile(), now, now)
	d.Now = func() time.Time { return now.Add(time.Second) }

	Run(d)

	if h.started != 0 {
		t.Error("uploader must not be started while heartbeat is fresh")
	}
	if len(h.spool(t)) != 1 {
		t.Error("payload should still be spooled")
	}
}

func TestRunStartsUploaderWhenHeartbeatIsStale(t *testing.T) {
	h := newHarness(t)
	d := h.deps(validPayload, creds)
	_ = os.MkdirAll(h.paths.StateDir, 0o700)
	_ = os.WriteFile(h.paths.HeartbeatFile(), nil, 0o600)
	old := time.Now().Add(-time.Minute)
	_ = os.Chtimes(h.paths.HeartbeatFile(), old, old)
	d.Now = time.Now

	Run(d)

	if h.started != 1 {
		t.Errorf("stale heartbeat should trigger a start, started=%d", h.started)
	}
}

func readOnlyRecord(t *testing.T, h *harness) map[string]json.RawMessage {
	t.Helper()
	entries := h.spool(t)
	if len(entries) != 1 {
		t.Fatalf("expected 1 spool entry, got %d", len(entries))
	}
	data, _ := queue.New(h.paths.SpoolDir()).Read(entries[0].Name)
	rec, err := event.ParseRecord(data)
	if err != nil {
		t.Fatal(err)
	}
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(rec.Payload, &obj); err != nil {
		t.Fatal(err)
	}
	return obj
}

func TestRunStripsContentByDefault(t *testing.T) {
	h := newHarness(t)
	payload := `{"session_id":"s","hook_event_name":"PostToolUse","tool_name":"Bash","tool_use_id":"c1",` +
		`"tool_input":{"command":"cat secret.txt"},"tool_response":"top secret","prompt":"你好"}`

	Run(h.deps(payload, creds))

	obj := readOnlyRecord(t, h)
	for _, k := range []string{"tool_input", "tool_response", "prompt"} {
		if _, ok := obj[k]; ok {
			t.Errorf("content field %q must not be spooled when capture_content=false", k)
		}
	}
	var sizes map[string]int
	_ = json.Unmarshal(obj[event.ContentSizesField], &sizes)
	if sizes["tool_response"] != len("top secret") || sizes["prompt"] != len("你好") || sizes["tool_input"] == 0 {
		t.Errorf("content sizes = %v", sizes)
	}
	if string(obj["tool_use_id"]) != `"c1"` {
		t.Error("metadata fields must be kept")
	}
}

func TestRunKeepsSpawnAgentResponseForLinking(t *testing.T) {
	h := newHarness(t)
	payload := `{"session_id":"s","tool_name":"collaborationspawn_agent","tool_input":{"message":"do x"},` +
		`"tool_response":"{\"task_name\":\"/root/x\"}"}`

	Run(h.deps(payload, creds))

	obj := readOnlyRecord(t, h)
	if _, ok := obj["tool_response"]; !ok {
		t.Error("spawn_agent tool_response must be kept")
	}
	if _, ok := obj["tool_input"]; ok {
		t.Error("spawn_agent tool_input (task message) must still be stripped")
	}
}

func TestRunKeepsContentWhenCaptureEnabled(t *testing.T) {
	h := newHarness(t)
	d := h.deps(`{"session_id":"s","prompt":"hello"}`, creds)
	d.Getenv = func(k string) string {
		if k == "CODEX_TRACE_CAPTURE_CONTENT" {
			return "true"
		}
		return ""
	}

	Run(d)

	if obj := readOnlyRecord(t, h); string(obj["prompt"]) != `"hello"` {
		t.Errorf("prompt should be kept, got %s", obj["prompt"])
	}
}

func TestRunRefusesSymlinkedStateDir(t *testing.T) {
	h := newHarness(t)
	target := filepath.Join(t.TempDir(), "elsewhere")
	_ = os.Mkdir(target, 0o700)
	if err := os.Symlink(target, h.paths.StateDir); err != nil {
		t.Fatal(err)
	}

	Run(h.deps(validPayload, creds))

	entries, _ := os.ReadDir(target)
	if len(entries) != 0 || h.started != 0 {
		t.Error("nothing may be written through a symlinked state dir")
	}
}

func TestRunWithUnusablePathsIsNoop(t *testing.T) {
	d := newHarness(t).deps(validPayload, creds)
	d.Paths = paths.Paths{}
	Run(d)
}

func TestRunEnforcesSpoolCapWhenSampled(t *testing.T) {
	h := newHarness(t)
	q := queue.New(h.paths.SpoolDir())
	big := bytes.Repeat([]byte("x"), SpoolMaxBytes/2+1)
	for i := 0; i < 3; i++ {
		if _, err := q.Put(big, time.Unix(int64(i), 0)); err != nil {
			t.Fatal(err)
		}
	}
	d := h.deps(validPayload, creds)
	d.Rand = func() float64 { return 0 }

	Run(d)

	if size, _ := q.TotalSize(); size > SpoolMaxBytes {
		t.Errorf("spool size %d exceeds cap", size)
	}
}

func TestConcurrentRunsKeepEveryRecord(t *testing.T) {
	h := newHarness(t)
	const n = 32
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			d := h.deps(validPayload, creds)
			d.Now = time.Now
			Run(d)
		}()
	}
	wg.Wait()

	if got := len(h.spool(t)); got != n {
		t.Errorf("spooled %d records, want %d", got, n)
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
			Rand:          func() float64 { return 0.99 },
		})
	}
}

func jsonEqual(a, b any) bool {
	ja, _ := json.Marshal(a)
	jb, _ := json.Marshal(b)
	return bytes.Equal(ja, jb)
}
