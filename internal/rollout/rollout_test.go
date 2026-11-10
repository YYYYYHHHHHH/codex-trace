package rollout

import (
	"os"
	"path/filepath"
	"testing"
)

const (
	fixtureDir = "../../testdata/sessions/subagent-resume"
	mainFile   = "rollout-2026-11-09T19-23-20-01a1c37b-4c83-7592-9323-47ac471857d5.jsonl"
	subFile    = "rollout-2026-11-09T19-23-45-01a1c37b-ae27-70b2-8449-dc997b56528e.jsonl"

	mainThread = "01a1c37b-4c83-7592-9323-47ac471857d5"
	subThread  = "01a1c37b-ae27-70b2-8449-dc997b56528e"
	turn1      = "01a1c37b-500d-70c1-8ad5-32d7a4232992"
	subTurn    = "01a1c37b-ae35-7bc1-ad0d-91545c66b71c"
	spawnCall  = "call_842e54cc2ffa4734a95c2275733c043d"
	failedCat  = "exec-5d47b0d0-c287-4c3c-8458-81d4979df5a7"
)

func scanFixture(t *testing.T, name string) Result {
	t.Helper()
	res, err := Scan(filepath.Join(fixtureDir, name), 0)
	if err != nil {
		t.Fatal(err)
	}
	return res
}

func TestScanMainRolloutTurnFacts(t *testing.T) {
	res := scanFixture(t, mainFile)

	if len(res.Turns) != 2 {
		t.Fatalf("expected 2 turns (initial + resume), got %d", len(res.Turns))
	}
	tf := res.Turns[turn1]
	if !tf.Completed || tf.Aborted {
		t.Errorf("turn1 completion flags wrong: %+v", tf)
	}
	if tf.Model != "gpt-6.1-sol" || tf.Effort != "high" {
		t.Errorf("model/effort = %q/%q", tf.Model, tf.Effort)
	}
	if tf.RootTurnID != turn1 || tf.ParentTurnID != "" {
		t.Errorf("root/parent = %q/%q", tf.RootTurnID, tf.ParentTurnID)
	}
	if tf.DurationMs <= 0 || tf.StartedAt.IsZero() || !tf.CompletedAt.After(tf.StartedAt) {
		t.Errorf("timing wrong: %+v", tf)
	}
	if len(tf.Generations) != 6 {
		t.Errorf("expected 6 generations in turn1, got %d", len(tf.Generations))
	}
	for _, g := range tf.Generations {
		if g.ResponseID == "" || g.Usage.Input <= 0 || g.Usage.Total <= 0 || g.End.Before(g.Start) {
			t.Errorf("bad generation: %+v", g)
		}
	}
}

func TestScanExtractsToolItems(t *testing.T) {
	tf := scanFixture(t, mainFile).Turns[turn1]

	cat, ok := tf.Items[failedCat]
	if !ok {
		t.Fatal("failed cat command item missing")
	}
	if cat.Status != "failed" || cat.ExitCode == nil || *cat.ExitCode != 1 {
		t.Errorf("failed cat item = %+v", cat)
	}
	if cat.CompletedAt.Before(cat.StartedAt) {
		t.Errorf("item timing wrong: %+v", cat)
	}

	spawn, ok := tf.Items[spawnCall]
	if !ok || spawn.Type != "SubAgentActivity" || spawn.Kind != "started" || spawn.AgentThreadID != subThread {
		t.Errorf("spawn activity item = %+v (present %v)", spawn, ok)
	}
}

func TestScanSubagentRolloutIgnoresCopiedParentHistory(t *testing.T) {
	res := scanFixture(t, subFile)

	sub, ok := res.Turns[subTurn]
	if !ok {
		t.Fatal("subagent turn missing")
	}
	if sub.RootTurnID != turn1 || sub.ParentTurnID != turn1 {
		t.Errorf("subagent root/parent = %q/%q, want %q", sub.RootTurnID, sub.ParentTurnID, turn1)
	}
	if !sub.Completed || sub.Aborted {
		t.Errorf("subagent completion flags wrong: %+v", sub)
	}
	if len(sub.Generations) != 2 {
		t.Errorf("subagent generations = %d, want 2", len(sub.Generations))
	}

	// 子 rollout 中复制的父历史包含父轮次的 turn_aborted；它只能出现在父轮次名下
	if parent, ok := res.Turns[turn1]; ok && len(parent.Generations) != 0 {
		t.Errorf("copied parent history must not produce generations, got %d", len(parent.Generations))
	}

	meta, ok := res.Metas[subThread]
	if !ok || meta.AgentPath != "/root/count_files" || meta.ParentThreadID != mainThread || meta.AgentNickname == "" {
		t.Errorf("subagent meta = %+v (present %v)", meta, ok)
	}
	if _, ok := res.Metas[mainThread]; !ok {
		t.Error("copied parent session_meta should be keyed by its own id")
	}
}

func TestScanSessionMeta(t *testing.T) {
	meta := scanFixture(t, mainFile).Metas[mainThread]
	if meta.SessionID != mainThread || meta.CLIVersion == "" || meta.CreatorUserID != "user-test" || meta.AgentPath != "" {
		t.Errorf("main meta = %+v", meta)
	}
}

func TestScanInterruptedTurn(t *testing.T) {
	files, _ := filepath.Glob("../../testdata/sessions/interrupted/rollout-*.jsonl")
	if len(files) != 1 {
		t.Fatalf("expected one interrupted rollout fixture, got %v", files)
	}
	res, err := Scan(files[0], 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Turns) != 1 {
		t.Fatalf("turns = %d", len(res.Turns))
	}
	for _, tf := range res.Turns {
		if !tf.Aborted || tf.AbortReason != "interrupted" || tf.Completed {
			t.Errorf("interrupted turn flags wrong: %+v", tf)
		}
	}
}

func TestScanIsIncrementalAndSkipsPartialLastLine(t *testing.T) {
	src, err := os.ReadFile(filepath.Join(fixtureDir, mainFile))
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "rollout-x.jsonl")
	half := len(src) / 2
	// 写入前半段，最后一行不完整
	if err := os.WriteFile(path, src[:half], 0o600); err != nil {
		t.Fatal(err)
	}
	first, err := Scan(path, 0)
	if err != nil {
		t.Fatal(err)
	}
	if first.Offset <= 0 || first.Offset > int64(half) || src[first.Offset-1] != '\n' {
		t.Fatalf("offset %d must stop after the last complete line", first.Offset)
	}

	if err := os.WriteFile(path, src, 0o600); err != nil {
		t.Fatal(err)
	}
	second, err := Scan(path, first.Offset)
	if err != nil {
		t.Fatal(err)
	}
	if second.Offset != int64(len(src)) {
		t.Errorf("second offset = %d, want %d", second.Offset, len(src))
	}

	full := scanFixture(t, mainFile)
	got := len(first.Turns[turn1].Generations) + len(second.Turns[turn1].Generations)
	if got != len(full.Turns[turn1].Generations) {
		t.Errorf("incremental generations %d != full %d", got, len(full.Turns[turn1].Generations))
	}
}

func TestScanToleratesGarbageLines(t *testing.T) {
	path := filepath.Join(t.TempDir(), "rollout-bad.jsonl")
	content := "not json\n{\"type\":\"event_msg\",\"timestamp\":\"bad\",\"payload\":{\"type\":\"task_started\",\"turn_id\":\"t\"}}\n{}\n"
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	res, err := Scan(path, 0)
	if err != nil {
		t.Fatal(err)
	}
	if res.Offset != int64(len(content)) {
		t.Errorf("garbage lines must still be consumed, offset %d", res.Offset)
	}
}

func TestScanMissingFile(t *testing.T) {
	if _, err := Scan(filepath.Join(t.TempDir(), "missing.jsonl"), 0); err == nil {
		t.Error("expected error for missing file")
	}
}

func TestScanOffsetBeyondFileRestartsFromZero(t *testing.T) {
	res, err := Scan(filepath.Join(fixtureDir, mainFile), 1<<40)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Turns) != 2 {
		t.Errorf("a truncated/rotated file should be rescanned from start, got %d turns", len(res.Turns))
	}
}
