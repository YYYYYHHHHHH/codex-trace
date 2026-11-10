package uploader

import (
	"bufio"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/YYYYYHHHHHH/codex-trace/internal/event"
	"github.com/YYYYYHHHHHH/codex-trace/internal/exporter"
	"github.com/YYYYYHHHHHH/codex-trace/internal/lock"
	"github.com/YYYYYHHHHHH/codex-trace/internal/paths"
	"github.com/YYYYYHHHHHH/codex-trace/internal/pipeline"
	"github.com/YYYYYHHHHHH/codex-trace/internal/queue"
	"github.com/YYYYYHHHHHH/codex-trace/internal/session"
	"github.com/YYYYYHHHHHH/codex-trace/internal/status"
	"github.com/YYYYYHHHHHH/codex-trace/internal/trace"
)

type fakeClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *fakeClock) Now() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.now }

func (c *fakeClock) Sleep(ctx context.Context, d time.Duration) bool {
	c.mu.Lock()
	c.now = c.now.Add(d)
	c.mu.Unlock()
	return ctx.Err() == nil
}

type fakeSender struct {
	mu      sync.Mutex
	results []exporter.Result // 依次返回；用完后一直返回 OK
	bodies  [][]byte
}

func (s *fakeSender) Send(_ context.Context, body []byte) exporter.Result {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.bodies = append(s.bodies, append([]byte(nil), body...))
	if len(s.results) == 0 {
		return exporter.Result{Outcome: exporter.OK, Status: 200}
	}
	r := s.results[0]
	s.results = s.results[1:]
	return r
}

type env struct {
	paths  paths.Paths
	clock  *fakeClock
	sender *fakeSender
	root   string
}

// newEnv 把 fixture 的 hook 记录写入 spool，时钟设在最后一条记录之后。
func newEnv(t *testing.T, fixture string) *env {
	t.Helper()
	dir := t.TempDir()
	p := paths.Paths{StateDir: filepath.Join(dir, "state")}
	root, err := filepath.Abs(filepath.Join("..", "..", "testdata", "sessions", fixture))
	if err != nil {
		t.Fatal(err)
	}
	f, err := os.Open(filepath.Join(root, "hooks.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	q := queue.New(p.SpoolDir())
	var last time.Time
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	for sc.Scan() {
		line := strings.ReplaceAll(sc.Text(), "ROLLOUT_DIR", root)
		rec, err := event.ParseRecord([]byte(line))
		if err != nil {
			t.Fatal(err)
		}
		last = rec.ReceivedAt
		if _, err := q.Put([]byte(line), rec.ReceivedAt); err != nil {
			t.Fatal(err)
		}
	}
	return &env{paths: p, clock: &fakeClock{now: last.Add(time.Second)}, sender: &fakeSender{}, root: root}
}

func (e *env) deps(once bool) Deps {
	return Deps{
		Paths: e.paths,
		Processor: pipeline.New(pipeline.Options{
			Store:        session.NewStore(e.paths.SessionsDir()),
			Policy:       trace.Policy{},
			AllowedRoots: []string{e.root},
		}),
		Sender:   e.sender,
		Version:  "test",
		Now:      e.clock.Now,
		Sleep:    e.clock.Sleep,
		Once:     once,
		LockWait: 10 * time.Millisecond,
	}
}

func count(t *testing.T, dir string) int {
	t.Helper()
	entries, err := queue.New(dir).List()
	if err != nil {
		t.Fatal(err)
	}
	return len(entries)
}

func TestRunOnceDeliversWholeSession(t *testing.T) {
	e := newEnv(t, "subagent-resume")

	if err := Run(context.Background(), e.deps(true)); err != nil {
		t.Fatal(err)
	}

	if count(t, e.paths.SpoolDir()) != 0 || count(t, e.paths.OutboxDir()) != 0 {
		t.Error("spool and outbox should be drained")
	}
	st, ok, _ := status.Read(e.paths.StatusFile())
	if !ok || st.SentSpans != 20 || st.LastSendOK.IsZero() || st.DroppedEvents != 0 {
		t.Errorf("status = %+v (want 20 spans: 3 turns + 7 tools + 10 generations)", st)
	}
	if _, err := os.Stat(e.paths.HeartbeatFile()); !os.IsNotExist(err) {
		t.Error("heartbeat must be removed on exit")
	}
	if _, err := os.Stat(e.paths.LastBatchFile()); err != nil {
		t.Error("last batch should be kept for preview")
	}
}

func TestRetryableFailureBacksOffThenDelivers(t *testing.T) {
	e := newEnv(t, "interrupted")
	e.sender.results = []exporter.Result{
		{Outcome: exporter.Retry, Status: 503, Err: errors.New("HTTP 503")},
		{Outcome: exporter.Retry, Status: 429, RetryAfter: 30 * time.Second, Err: errors.New("HTTP 429")},
	}

	if err := Run(context.Background(), e.deps(true)); err != nil {
		t.Fatal(err)
	}

	if len(e.sender.bodies) != 3 {
		t.Errorf("expected 2 failures + 1 success, got %d sends", len(e.sender.bodies))
	}
	if count(t, e.paths.OutboxDir()) != 0 {
		t.Error("batch should eventually be delivered")
	}
	st, _, _ := status.Read(e.paths.StatusFile())
	if st.SentBatches != 1 || !strings.Contains(st.LastError, "429") {
		t.Errorf("status = %+v", st)
	}
}

func TestPermanentFailureMovesBatchToFailed(t *testing.T) {
	e := newEnv(t, "interrupted")
	e.sender.results = []exporter.Result{{Outcome: exporter.Permanent, Status: 401, Err: errors.New("HTTP 401")}}

	if err := Run(context.Background(), e.deps(true)); err != nil {
		t.Fatal(err)
	}

	if count(t, e.paths.OutboxDir()) != 0 || count(t, e.paths.FailedDir()) != 1 {
		t.Error("permanently failed batch should move to failed/")
	}
	st, _, _ := status.Read(e.paths.StatusFile())
	if st.FailedBatches != 1 || st.SentBatches != 0 {
		t.Errorf("status = %+v", st)
	}
}

func TestSecondInstanceExitsWhenLockHeld(t *testing.T) {
	e := newEnv(t, "interrupted")
	_ = os.MkdirAll(e.paths.StateDir, 0o700)
	release, ok, err := lock.TryLock(e.paths.LockFile())
	if err != nil || !ok {
		t.Fatal(err)
	}
	defer release()

	if err := Run(context.Background(), e.deps(true)); err != nil {
		t.Fatal(err)
	}
	if len(e.sender.bodies) != 0 || count(t, e.paths.SpoolDir()) == 0 {
		t.Error("second instance must not process anything")
	}
}

func TestDaemonExitsAfterIdlePeriod(t *testing.T) {
	e := newEnv(t, "interrupted")
	d := e.deps(false)
	d.IdleExit = 5 * time.Second
	start := e.clock.Now()

	if err := Run(context.Background(), d); err != nil {
		t.Fatal(err)
	}

	if elapsed := e.clock.Now().Sub(start); elapsed < 5*time.Second || elapsed > 30*time.Second {
		t.Errorf("daemon ran for %v of fake time", elapsed)
	}
	if len(e.sender.bodies) != 1 {
		t.Errorf("sends = %d", len(e.sender.bodies))
	}
}

func TestInvalidSpoolRecordsAreDroppedAndCounted(t *testing.T) {
	e := newEnv(t, "interrupted")
	q := queue.New(e.paths.SpoolDir())
	_, _ = q.Put([]byte("not json"), e.clock.Now())
	_, _ = q.Put([]byte(`{"v":1,"received_at":"2026-11-09T10:00:00Z","payload":{"hook_event_name":"Stop"}}`), e.clock.Now())

	if err := Run(context.Background(), e.deps(true)); err != nil {
		t.Fatal(err)
	}

	st, _, _ := status.Read(e.paths.StatusFile())
	if st.DroppedEvents != 2 || count(t, e.paths.SpoolDir()) != 0 {
		t.Errorf("status = %+v, spool = %d", st, count(t, e.paths.SpoolDir()))
	}
}

func TestContextCancelStopsLoop(t *testing.T) {
	e := newEnv(t, "interrupted")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := Run(ctx, e.deps(false)); err != nil {
		t.Fatal(err)
	}
}

func TestCountersSurviveRestart(t *testing.T) {
	e := newEnv(t, "interrupted")
	if err := Run(context.Background(), e.deps(true)); err != nil {
		t.Fatal(err)
	}
	if err := Run(context.Background(), e.deps(true)); err != nil {
		t.Fatal(err)
	}
	st, _, _ := status.Read(e.paths.StatusFile())
	if st.SentBatches != 1 {
		t.Errorf("counters should persist across runs, got %+v", st)
	}
}

func TestCountSpans(t *testing.T) {
	if countSpans([]byte("garbage")) != 0 {
		t.Error("garbage should count 0")
	}
	body := `{"resourceSpans":[{"scopeSpans":[{"spans":[{},{}]},{"spans":[{}]}]}]}`
	if n := countSpans([]byte(body)); n != 3 {
		t.Errorf("countSpans = %d", n)
	}
}

func TestOutboxWriteFailureKeepsSpoolForReplay(t *testing.T) {
	e := newEnv(t, "interrupted")
	_ = os.MkdirAll(e.paths.StateDir, 0o700)
	if err := os.WriteFile(e.paths.OutboxDir(), []byte("not a dir"), 0o600); err != nil {
		t.Fatal(err)
	}
	d := e.deps(true)
	d.RunFor = 3 * time.Second

	if err := Run(context.Background(), d); err != nil {
		t.Fatal(err)
	}
	if count(t, e.paths.SpoolDir()) == 0 {
		t.Fatal("spool must be kept when outbox cannot be written")
	}

	_ = os.Remove(e.paths.OutboxDir())
	if err := Run(context.Background(), e.deps(true)); err != nil {
		t.Fatal(err)
	}
	if len(e.sender.bodies) != 1 || count(t, e.paths.SpoolDir()) != 0 {
		t.Errorf("after recovery the turn must be delivered: sends=%d spool=%d", len(e.sender.bodies), count(t, e.paths.SpoolDir()))
	}
}

func TestOnceModeGivesUpAtDeadlineWhenSpoolIsStuck(t *testing.T) {
	e := newEnv(t, "interrupted")
	_ = os.MkdirAll(e.paths.StateDir, 0o700)
	_ = os.WriteFile(e.paths.OutboxDir(), []byte("x"), 0o600) // outbox 一直写不进去
	d := e.deps(true)
	d.RunFor = 5 * time.Second
	start := e.clock.Now()

	if err := Run(context.Background(), d); err != nil {
		t.Fatal(err)
	}
	if e.clock.Now().Sub(start) > time.Minute {
		t.Error("once mode must stop at its deadline")
	}
}

func TestPoisonRecordDoesNotBlockSpool(t *testing.T) {
	e := newEnv(t, "interrupted")
	// 合法外壳但 payload 不是对象：Decode 失败；再放一条完全损坏的
	q := queue.New(e.paths.SpoolDir())
	_, _ = q.Put([]byte(`{"v":1,"received_at":"2026-11-09T10:00:00Z","payload":[1,2]}`), e.clock.Now())

	if err := Run(context.Background(), e.deps(true)); err != nil {
		t.Fatal(err)
	}
	if count(t, e.paths.SpoolDir()) != 0 || len(e.sender.bodies) != 1 {
		t.Errorf("poison record must be dropped while the rest is delivered: spool=%d sends=%d",
			count(t, e.paths.SpoolDir()), len(e.sender.bodies))
	}
}

func TestCommitFailureRollsBackOutbox(t *testing.T) {
	e := newEnv(t, "interrupted")
	// sessions 目录可读不可写：载入成功、保存失败
	if err := os.MkdirAll(e.paths.SessionsDir(), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(e.paths.SessionsDir(), 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(e.paths.SessionsDir(), 0o700) })
	d := e.deps(true)
	d.RunFor = 5 * time.Second

	if err := Run(context.Background(), d); err != nil {
		t.Fatal(err)
	}

	if n := count(t, e.paths.OutboxDir()); n != 0 {
		t.Errorf("outbox must be rolled back on commit failure, has %d batches", n)
	}
	if len(e.sender.bodies) != 0 || count(t, e.paths.SpoolDir()) == 0 {
		t.Error("nothing may be sent and spool must be kept")
	}
	st, ok, _ := status.Read(e.paths.StatusFile())
	if !ok || !strings.Contains(st.LastError, "持久化失败") {
		t.Errorf("failure must be visible in status: %+v", st)
	}
}

func TestStorageErrorKeepsRecordsInsteadOfDropping(t *testing.T) {
	e := newEnv(t, "interrupted")
	_ = os.MkdirAll(e.paths.StateDir, 0o700)
	if err := os.WriteFile(e.paths.SessionsDir(), []byte("x"), 0o600); err != nil { // 状态无法读取
		t.Fatal(err)
	}
	d := e.deps(true)
	d.RunFor = 3 * time.Second

	if err := Run(context.Background(), d); err != nil {
		t.Fatal(err)
	}

	st, _, _ := status.Read(e.paths.StatusFile())
	if st.DroppedEvents != 0 || count(t, e.paths.SpoolDir()) != 3 || st.LastError == "" {
		t.Errorf("storage errors must keep records for retry: dropped=%d spool=%d err=%q",
			st.DroppedEvents, count(t, e.paths.SpoolDir()), st.LastError)
	}
}

func TestBackoffSurvivesRestart(t *testing.T) {
	e := newEnv(t, "interrupted")
	e.sender.results = []exporter.Result{{Outcome: exporter.Retry, Status: 503, RetryAfter: 2 * time.Minute, Err: errors.New("HTTP 503")}}
	d := e.deps(false)
	d.IdleExit = time.Second // 退避期间空闲退出

	if err := Run(context.Background(), d); err != nil {
		t.Fatal(err)
	}
	if len(e.sender.bodies) != 1 {
		t.Fatalf("first run sends = %d", len(e.sender.bodies))
	}

	// 新进程在退避窗口内启动：不应立即重发
	d2 := e.deps(false)
	d2.IdleExit = time.Second
	if err := Run(context.Background(), d2); err != nil {
		t.Fatal(err)
	}
	if len(e.sender.bodies) != 1 {
		t.Errorf("restarted uploader must honor persisted backoff, sends = %d", len(e.sender.bodies))
	}
}
