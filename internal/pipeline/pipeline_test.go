package pipeline

import (
	"bufio"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/YYYYYHHHHHH/codex-trace/internal/event"
	"github.com/YYYYYHHHHHH/codex-trace/internal/otlp"
	"github.com/YYYYYHHHHHH/codex-trace/internal/session"
	"github.com/YYYYYHHHHHH/codex-trace/internal/trace"
)

const (
	turn1     = "01a1c37b-500d-70c1-8ad5-32d7a4232992"
	subTurn   = "01a1c37b-ae35-7bc1-ad0d-91545c66b71c"
	spawnCall = "call_842e54cc2ffa4734a95c2275733c043d"
	failedCat = "exec-5d47b0d0-c287-4c3c-8458-81d4979df5a7"
	subBash   = "exec-61b3b3c1-b0c7-4f91-82be-9270de80e159"
)

// loadFixture 读取 testdata/sessions/<name>/hooks.jsonl，把 ROLLOUT_DIR 占位符替换为 fixture 目录。
func loadFixture(t *testing.T, name string) (string, []event.Record) {
	t.Helper()
	dir, err := filepath.Abs(filepath.Join("..", "..", "testdata", "sessions", name))
	if err != nil {
		t.Fatal(err)
	}
	f, err := os.Open(filepath.Join(dir, "hooks.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	var recs []event.Record
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	for sc.Scan() {
		line := strings.ReplaceAll(sc.Text(), "ROLLOUT_DIR", dir)
		rec, err := event.ParseRecord([]byte(line))
		if err != nil {
			t.Fatal(err)
		}
		recs = append(recs, rec)
	}
	return dir, recs
}

func newProcessor(t *testing.T, root string, store session.Store, policy trace.Policy) *Processor {
	t.Helper()
	return New(Options{Store: store, Policy: policy, AllowedRoots: []string{root}, UserFallback: "alice"})
}

func ingestAll(t *testing.T, p *Processor, recs []event.Record) {
	t.Helper()
	for _, r := range recs {
		if err := p.Ingest(r); err != nil {
			t.Fatal(err)
		}
	}
}

type spanIndex map[otlp.SpanID]otlp.Span

func index(spans []otlp.Span) spanIndex {
	idx := spanIndex{}
	for _, s := range spans {
		idx[s.SpanID] = s
	}
	return idx
}

func attr(s otlp.Span, key string) (otlp.Attr, bool) {
	for _, a := range s.Attrs {
		if a.Key == key {
			return a, true
		}
	}
	return otlp.Attr{}, false
}

func countType(spans []otlp.Span, typ string) int {
	n := 0
	for _, s := range spans {
		if a, ok := attr(s, "langfuse.observation.type"); ok && a.Value.AsString() == typ {
			n++
		}
	}
	return n
}

func TestSubagentSessionProducesExpectedTree(t *testing.T) {
	root, recs := loadFixture(t, "subagent-resume")
	p := newProcessor(t, root, session.NewStore(t.TempDir()), trace.Policy{Environment: "test"})
	ingestAll(t, p, recs)

	spans := p.Flush(recs[len(recs)-1].ReceivedAt.Add(time.Minute))
	idx := index(spans)

	if got := countType(spans, "agent"); got != 3 {
		t.Errorf("agent spans = %d, want 3 (turn1, subagent, turn2)", got)
	}
	if got := countType(spans, "tool"); got != 7 {
		t.Errorf("tool spans = %d, want 7", got)
	}
	if got := countType(spans, "generation"); got != 10 {
		t.Errorf("generation spans = %d, want 10 (6 + 2 + 2)", got)
	}

	main1 := idx[trace.TurnSpanID(turn1)]
	sub := idx[trace.TurnSpanID(subTurn)]
	spawn := idx[trace.ToolSpanID(spawnCall)]
	if main1.Name != "codex turn" || !main1.Parent.IsZero() {
		t.Errorf("turn1 should be a root span: %+v", main1)
	}
	if sub.TraceID != main1.TraceID {
		t.Error("subagent must share the parent turn's trace")
	}
	if sub.Parent != spawn.SpanID || spawn.Parent != main1.SpanID {
		t.Error("subagent must hang under the spawn_agent tool span")
	}
	if b := idx[trace.ToolSpanID(subBash)]; b.Parent != sub.SpanID {
		t.Error("tool inside subagent must hang under the subagent span")
	}

	cat := idx[trace.ToolSpanID(failedCat)]
	if !cat.Status.Error || cat.Status.Message != "exit code 1" {
		t.Errorf("failed cat should be ERROR with exit code: %+v", cat.Status)
	}
	if lvl, _ := attr(cat, "langfuse.observation.level"); lvl.Value.AsString() != "ERROR" {
		t.Error("failed tool must carry level ERROR")
	}

	traces := map[otlp.TraceID]bool{}
	for _, s := range spans {
		traces[s.TraceID] = true
		if _, ok := attr(s, "langfuse.observation.input"); ok {
			t.Errorf("span %q carries content although capture_content=false", s.Name)
		}
		if s.End.Before(s.Start) {
			t.Errorf("span %q ends before it starts", s.Name)
		}
	}
	if len(traces) != 2 {
		t.Errorf("expected 2 traces (turn1+subagent, turn2), got %d", len(traces))
	}
	if sid, _ := attr(main1, "session.id"); sid.Value.AsString() != "01a1c37b-4c83-7592-9323-47ac471857d5" {
		t.Error("turn span must carry session.id")
	}
	if uid, ok := attr(main1, "user.id"); !ok || uid.Value.AsString() == "" || uid.Value.AsString() == "user-test" {
		t.Error("user.id must be present and hashed")
	}
}

func TestInterruptedTurnIsTaggedAndWarned(t *testing.T) {
	root, recs := loadFixture(t, "interrupted")
	p := newProcessor(t, root, session.NewStore(t.TempDir()), trace.Policy{})
	ingestAll(t, p, recs)

	spans := p.Flush(recs[len(recs)-1].ReceivedAt.Add(time.Minute))
	if len(spans) != 1 {
		t.Fatalf("expected only the turn span, got %d", len(spans))
	}
	tags, _ := attr(spans[0], "langfuse.trace.tags")
	if strings.Join(tags.Value.AsStrings(), ",") != "codex,interrupted" {
		t.Errorf("tags = %+v", tags)
	}
	if lvl, _ := attr(spans[0], "langfuse.observation.level"); lvl.Value.AsString() != "WARNING" {
		t.Error("interrupted turn must be WARNING")
	}
}

func TestFlushWaitsForGraceAndKeepsPending(t *testing.T) {
	root, recs := loadFixture(t, "interrupted")
	p := newProcessor(t, root, session.NewStore(t.TempDir()), trace.Policy{})
	ingestAll(t, p, recs)
	last := recs[len(recs)-1].ReceivedAt

	if spans := p.Flush(last.Add(100 * time.Millisecond)); len(spans) != 0 {
		t.Errorf("turn must not be emitted within grace period, got %d spans", len(spans))
	}
	if !p.Pending() {
		t.Error("closed-but-unsent turn should be pending")
	}
	if spans := p.Flush(last.Add(2 * time.Second)); len(spans) != 1 {
		t.Errorf("turn should be emitted after grace (rollout has turn_aborted), got %d", len(spans))
	}
	if p.Pending() {
		t.Error("nothing should be pending after emission")
	}
}

func TestOutputIsDeterministic(t *testing.T) {
	root, recs := loadFixture(t, "subagent-resume")
	now := recs[len(recs)-1].ReceivedAt.Add(time.Minute)
	run := func() []otlp.Span {
		p := newProcessor(t, root, session.NewStore(t.TempDir()), trace.Policy{})
		ingestAll(t, p, recs)
		return p.Flush(now)
	}
	a, b := index(run()), index(run())
	if len(a) != len(b) {
		t.Fatalf("span count differs: %d vs %d", len(a), len(b))
	}
	for id, s := range a {
		if other, ok := b[id]; !ok || other.TraceID != s.TraceID || other.Parent != s.Parent {
			t.Errorf("span %s differs between runs", id)
		}
	}
}

func TestReplayAfterCommitDoesNotReemit(t *testing.T) {
	root, recs := loadFixture(t, "subagent-resume")
	store := session.NewStore(t.TempDir())
	now := recs[len(recs)-1].ReceivedAt.Add(time.Minute)

	p := newProcessor(t, root, store, trace.Policy{})
	ingestAll(t, p, recs)
	if spans := p.Flush(now); len(spans) == 0 {
		t.Fatal("first run should emit")
	}
	if err := p.Commit(); err != nil {
		t.Fatal(err)
	}

	// 模拟崩溃后 spool 被重放
	p2 := newProcessor(t, root, store, trace.Policy{})
	ingestAll(t, p2, recs)
	if spans := p2.Flush(now); len(spans) != 0 {
		t.Errorf("replayed records must not re-emit, got %d spans", len(spans))
	}
}

func TestContentIsRedactedWhenCaptureEnabled(t *testing.T) {
	root, recs := loadFixture(t, "subagent-resume")
	p := newProcessor(t, root, session.NewStore(t.TempDir()), trace.Policy{CaptureContent: true, MaxFieldBytes: 64})
	ingestAll(t, p, recs)

	spans := p.Flush(recs[len(recs)-1].ReceivedAt.Add(time.Minute))
	main1 := index(spans)[trace.TurnSpanID(turn1)]
	in, ok := attr(main1, "langfuse.trace.input")
	if !ok {
		t.Fatal("trace input missing with capture_content=true")
	}
	if in.Value.AsString() == "" {
		t.Error("trace input should not be empty")
	}
}

func TestStaleOpenTurnIsClosedAsIncomplete(t *testing.T) {
	_, recs := loadFixture(t, "interrupted")
	p := New(Options{Store: session.NewStore(t.TempDir())}) // 无允许目录：rollout 不可读，仍应收尾
	for _, r := range recs[:2] {                            // 只有 SessionStart + UserPromptSubmit
		if err := p.Ingest(r); err != nil {
			t.Fatal(err)
		}
	}
	now := recs[1].ReceivedAt.Add(DefaultStaleAfter + time.Minute)
	if spans := p.Flush(now); len(spans) != 0 {
		t.Fatal("Flush alone must not close stale turns (spool may still hold the Stop)")
	}
	p.CloseStale(now)
	spans := p.Flush(now)
	if len(spans) != 1 {
		t.Fatalf("stale turn should be emitted, got %d spans", len(spans))
	}
	if tags, _ := attr(spans[0], "langfuse.trace.tags"); strings.Join(tags.Value.AsStrings(), ",") != "codex,incomplete" {
		t.Errorf("tags = %+v", tags)
	}
}

func TestTranscriptOutsideAllowedRootsIsNotRead(t *testing.T) {
	root, recs := loadFixture(t, "interrupted")
	scanned := 0
	p := New(Options{
		Store:        session.NewStore(t.TempDir()),
		AllowedRoots: []string{t.TempDir()}, // fixture 目录不在允许范围内
		Scan: func(string, int64) (_ rolloutResult, _ error) {
			scanned++
			return rolloutResult{}, nil
		},
	})
	_ = root
	ingestAll(t, p, recs)
	p.Flush(recs[len(recs)-1].ReceivedAt.Add(time.Minute))
	if scanned != 0 {
		t.Error("transcript outside allowed roots must not be scanned")
	}
}

func TestResolveTranscriptRejectsBadPaths(t *testing.T) {
	root := t.TempDir()
	good := filepath.Join(root, "rollout-x.jsonl")
	_ = os.WriteFile(good, nil, 0o600)
	outside := filepath.Join(t.TempDir(), "rollout-y.jsonl")
	_ = os.WriteFile(outside, nil, 0o600)
	link := filepath.Join(root, "rollout-link.jsonl")
	_ = os.Symlink(outside, link)
	other := filepath.Join(root, "notes.txt")
	_ = os.WriteFile(other, nil, 0o600)

	p := New(Options{AllowedRoots: []string{root}})
	cases := map[string]bool{
		good:                     true,
		outside:                  false,
		link:                     false,
		other:                    false,
		"relative/rollout.jsonl": false,
		filepath.Join(root, "rollout-missing.jsonl"): false,
	}
	for path, want := range cases {
		if _, ok := p.resolveTranscript(path); ok != want {
			t.Errorf("resolveTranscript(%q) = %v, want %v", path, ok, want)
		}
	}
}

func TestIngestRejectsRecordWithoutSession(t *testing.T) {
	p := New(Options{Store: session.NewStore(t.TempDir())})
	err := p.Ingest(event.Record{Payload: []byte(`{"hook_event_name":"Stop"}`)})
	if !errors.Is(err, ErrInvalidRecord) {
		t.Errorf("expected ErrInvalidRecord, got %v", err)
	}
}

func TestLoadAllDropsCorruptState(t *testing.T) {
	dir := t.TempDir()
	_ = os.WriteFile(filepath.Join(dir, "bad.json"), []byte("{"), 0o600)
	p := New(Options{Store: session.NewStore(dir)})
	if err := p.LoadAll(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "bad.json")); !os.IsNotExist(err) {
		t.Error("corrupt state file should be removed")
	}
}

func TestCloseStaleRespectsRecentTranscriptWrites(t *testing.T) {
	root, recs := loadFixture(t, "interrupted")
	p := newProcessor(t, root, session.NewStore(t.TempDir()), trace.Policy{})
	for _, r := range recs[:2] {
		if err := p.Ingest(r); err != nil {
			t.Fatal(err)
		}
	}
	// rollout 刚被写过：长命令仍在运行
	files, _ := filepath.Glob(filepath.Join(root, "rollout-*.jsonl"))
	now := recs[1].ReceivedAt.Add(DefaultStaleAfter + time.Minute)
	_ = os.Chtimes(files[0], now, now)
	t.Cleanup(func() { _ = os.Chtimes(files[0], recs[1].ReceivedAt, recs[1].ReceivedAt) })

	p.CloseStale(now)
	if spans := p.Flush(now); len(spans) != 0 {
		t.Error("turn with a recently written transcript must not be closed as stale")
	}
}

func TestDiscardDropsUncommittedEmission(t *testing.T) {
	root, recs := loadFixture(t, "interrupted")
	store := session.NewStore(t.TempDir())
	p := newProcessor(t, root, store, trace.Policy{})
	ingestAll(t, p, recs)
	now := recs[len(recs)-1].ReceivedAt.Add(time.Minute)
	if len(p.Flush(now)) == 0 {
		t.Fatal("expected spans")
	}

	p.Discard() // 模拟写 outbox 失败

	ingestAll(t, p, recs) // spool 未删除，下轮重放
	if len(p.Flush(now)) == 0 {
		t.Error("after Discard the turn must be emitted again on replay")
	}
}

func TestMaxWaitEmissionIsTaggedPartial(t *testing.T) {
	root, recs := loadFixture(t, "interrupted")
	p := New(Options{
		Store:        session.NewStore(t.TempDir()),
		AllowedRoots: []string{root},
		Scan:         func(string, int64) (rolloutResult, error) { return rolloutResult{}, nil }, // rollout 迟迟没有收尾记录
	})
	ingestAll(t, p, recs)
	last := recs[len(recs)-1].ReceivedAt

	if spans := p.Flush(last.Add(5 * time.Second)); len(spans) != 0 {
		t.Fatal("must wait up to MaxWait for rollout")
	}
	spans := p.Flush(last.Add(DefaultMaxWait + time.Second))
	if len(spans) != 1 {
		t.Fatalf("spans = %d", len(spans))
	}
	tags, _ := attr(spans[0], "langfuse.trace.tags")
	if !strings.Contains(strings.Join(tags.Value.AsStrings(), ","), "partial") {
		t.Errorf("tags = %v", tags.Value.AsStrings())
	}
}

func TestSubagentWithoutFactsStillJoinsParentTrace(t *testing.T) {
	root, recs := loadFixture(t, "subagent-resume")
	// 只扫描主会话 rollout；子 agent rollout 读不到（例如权限问题）
	p := New(Options{
		Store:        session.NewStore(t.TempDir()),
		AllowedRoots: []string{root},
		Scan: func(path string, off int64) (rolloutResult, error) {
			if strings.Contains(path, "dc997b56528e") {
				return rolloutResult{}, os.ErrPermission
			}
			return scanForTest(path, off)
		},
	})
	ingestAll(t, p, recs)
	spans := p.Flush(recs[len(recs)-1].ReceivedAt.Add(time.Minute))
	idx := index(spans)

	sub, main1 := idx[trace.TurnSpanID(subTurn)], idx[trace.TurnSpanID(turn1)]
	if sub.TraceID != main1.TraceID {
		t.Error("subagent without rollout facts must still join the parent trace")
	}
	if sub.Parent.IsZero() {
		t.Error("subagent must have a parent span")
	}
}
