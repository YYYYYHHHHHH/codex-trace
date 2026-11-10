package session

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/YYYYYHHHHHH/codex-trace/internal/event"
	"github.com/YYYYYHHHHHH/codex-trace/internal/rollout"
)

var t0 = time.Date(2026, 11, 9, 10, 0, 0, 0, time.UTC)

func apply(s *State, at time.Duration, p event.Payload) {
	if p.SessionID == "" {
		p.SessionID = s.SessionID
	}
	s.Apply(event.Record{ReceivedAt: t0.Add(at), Event: p.HookEventName}, p)
}

func TestTurnLifecycle(t *testing.T) {
	s := New("s")
	apply(s, 0, event.Payload{HookEventName: event.SessionStart})
	apply(s, 1*time.Second, event.Payload{HookEventName: event.UserPromptSubmit, TurnID: "t1", Prompt: "hi", TranscriptPath: "/r.jsonl", Model: "m"})
	apply(s, 2*time.Second, event.Payload{HookEventName: event.PreToolUse, TurnID: "t1", ToolUseID: "c1", ToolName: "Bash", ToolInput: json.RawMessage(`{"command":"ls"}`)})
	apply(s, 3*time.Second, event.Payload{HookEventName: event.PostToolUse, TurnID: "t1", ToolUseID: "c1", ToolName: "Bash", ToolResponse: json.RawMessage(`"out"`)})
	apply(s, 4*time.Second, event.Payload{HookEventName: event.PermissionRequest, TurnID: "t1"})
	apply(s, 5*time.Second, event.Payload{HookEventName: event.Stop, TurnID: "t1", LastAssistantMessage: "done"})

	tr := s.Turns["t1"]
	if tr == nil || tr.Close != CloseStop || !tr.ClosedAt.Equal(t0.Add(5*time.Second)) || tr.Output != "done" {
		t.Fatalf("turn not closed correctly: %+v", tr)
	}
	if tr.Prompt != "hi" || tr.PromptBytes != 2 || tr.TranscriptPath != "/r.jsonl" || tr.PermissionRequests != 1 {
		t.Errorf("turn fields wrong: %+v", tr)
	}
	if len(tr.Tools) != 1 {
		t.Fatalf("tools = %d", len(tr.Tools))
	}
	tool := tr.Tools[0]
	if !tool.Done || tool.Response != "out" || tool.ResponseBytes != 3 || !tool.End.Equal(t0.Add(3*time.Second)) {
		t.Errorf("tool wrong: %+v", tool)
	}
}

func TestContentSizesFromHookAreUsed(t *testing.T) {
	s := New("s")
	apply(s, 0, event.Payload{HookEventName: event.UserPromptSubmit, TurnID: "t", ContentSizes: map[string]int{"prompt": 99}})
	if got := s.Turns["t"].PromptBytes; got != 99 {
		t.Errorf("PromptBytes = %d, want 99 from stripped content sizes", got)
	}
}

func TestFirstCloseWinsAndLateEventsAfterEmitAreIgnored(t *testing.T) {
	s := New("s")
	apply(s, 0, event.Payload{HookEventName: event.UserPromptSubmit, TurnID: "t"})
	apply(s, time.Second, event.Payload{HookEventName: event.Interrupt, TurnID: "t"})
	apply(s, 2*time.Second, event.Payload{HookEventName: event.Stop, TurnID: "t"})
	if s.Turns["t"].Close != CloseInterrupt {
		t.Errorf("first close should win, got %q", s.Turns["t"].Close)
	}

	s.MarkEmitted(s.Turns["t"], t0)
	apply(s, 3*time.Second, event.Payload{HookEventName: event.PostToolUse, TurnID: "t", ToolUseID: "late"})
	if _, ok := s.Turns["t"]; ok {
		t.Error("events for an emitted turn must be ignored")
	}
}

func TestSubagentTurnLinking(t *testing.T) {
	s := New("s")
	apply(s, 0, event.Payload{HookEventName: event.UserPromptSubmit, TurnID: "main", TranscriptPath: "/main.jsonl"})
	apply(s, time.Second, event.Payload{HookEventName: event.PostToolUse, TurnID: "main", ToolUseID: "spawn1",
		ToolName: "collaborationspawn_agent", ToolResponse: json.RawMessage(`"{\"task_name\":\"/root/x\"}"`)})
	apply(s, 2*time.Second, event.Payload{HookEventName: event.SubagentStart, TurnID: "sub", AgentID: "a1", AgentType: "default", TranscriptPath: "/sub.jsonl"})
	apply(s, 3*time.Second, event.Payload{HookEventName: event.SubagentStop, TurnID: "sub", AgentID: "a1",
		TranscriptPath: "/main.jsonl", AgentTranscriptPath: "/sub.jsonl", LastAssistantMessage: "1"})

	sub := s.Turns["sub"]
	if !sub.IsSubagent() || sub.FallbackRootTurnID != "main" || sub.TranscriptPath != "/sub.jsonl" || sub.Close != CloseSubagentStop {
		t.Errorf("subagent turn wrong: %+v", sub)
	}
	if ref, ok := s.SpawnFor("/root/x", sub.Start); !ok || ref.ToolUseID != "spawn1" || ref.RootTurnID != "main" {
		t.Errorf("SpawnFor = %+v, %v", ref, ok)
	}
	s.MarkEmitted(s.Turns["main"], t0)
	if ref, ok := s.SpawnFor("/root/x", sub.Start); !ok || ref.ToolUseID != "spawn1" {
		t.Error("spawn link must survive the parent turn being emitted")
	}
	if _, ok := s.SpawnFor("", sub.Start); ok {
		t.Error("unknown task must not match")
	}
}

func TestSubagentStopWithoutStartUsesAgentTranscript(t *testing.T) {
	s := New("s")
	apply(s, 0, event.Payload{HookEventName: event.SubagentStop, TurnID: "sub", AgentID: "a",
		TranscriptPath: "/parent.jsonl", AgentTranscriptPath: "/child.jsonl"})
	if got := s.Turns["sub"].TranscriptPath; got != "/child.jsonl" {
		t.Errorf("TranscriptPath = %q, want agent transcript", got)
	}
}

func TestToolWithoutIDIsIgnoredAndDuplicatesMerge(t *testing.T) {
	s := New("s")
	apply(s, 2*time.Second, event.Payload{HookEventName: event.PreToolUse, TurnID: "t", ToolName: "Bash"})
	apply(s, 3*time.Second, event.Payload{HookEventName: event.PreToolUse, TurnID: "t", ToolUseID: "c", ToolName: "Bash"})
	apply(s, 1*time.Second, event.Payload{HookEventName: event.PreToolUse, TurnID: "t", ToolUseID: "c", ToolName: "Bash"})
	tools := s.Turns["t"].Tools
	if len(tools) != 1 || !tools[0].Start.Equal(t0.Add(time.Second)) {
		t.Errorf("tools = %+v", tools)
	}
}

func TestMergeScanAndMeta(t *testing.T) {
	s := New("s")
	s.MergeScan("/r.jsonl", rollout.Result{
		Turns:  map[string]rollout.TurnFacts{"t": {TurnID: "t", Model: "m"}},
		Metas:  map[string]rollout.SessionMeta{"s": {ID: "s", CLIVersion: "1.0"}},
		Offset: 10,
	}, t0)
	s.MergeScan("/r.jsonl", rollout.Result{
		Turns:  map[string]rollout.TurnFacts{"t": {TurnID: "t", Completed: true}},
		Offset: 20,
	}, t0)

	f := s.Files["/r.jsonl"]
	if f.Offset != 20 || !f.Turns["t"].Completed || f.Turns["t"].Model != "m" {
		t.Errorf("merge scan wrong: %+v", f)
	}
	if m, ok := s.Meta("s"); !ok || m.CLIVersion != "1.0" {
		t.Errorf("Meta = %+v, %v", m, ok)
	}
	if _, ok := s.Meta("missing"); ok {
		t.Error("missing meta should not be found")
	}
}

func TestPrune(t *testing.T) {
	s := New("s")
	apply(s, 0, event.Payload{HookEventName: event.UserPromptSubmit, TurnID: "t", TranscriptPath: "/r.jsonl"})
	s.MergeScan("/r.jsonl", rollout.Result{}, t0)
	s.MergeScan("/other.jsonl", rollout.Result{}, t0)
	s.MarkEmitted(s.Turns["t"], t0)

	if s.Prune(t0.Add(time.Hour), 24*time.Hour) {
		t.Error("recent state must not be pruned")
	}
	if !s.Prune(t0.Add(48*time.Hour), 24*time.Hour) {
		t.Error("old empty state should be removable")
	}
	if len(s.Files) != 0 || len(s.Emitted) != 0 {
		t.Errorf("old caches not pruned: files=%d emitted=%d", len(s.Files), len(s.Emitted))
	}
}

func TestStoreRoundTripAndSafeNames(t *testing.T) {
	dir := t.TempDir()
	store := NewStore(dir)

	s := New("01a1c37b-4c83")
	apply(s, 0, event.Payload{HookEventName: event.UserPromptSubmit, TurnID: "t"})
	if err := store.Save(s); err != nil {
		t.Fatal(err)
	}
	got, err := store.Load("01a1c37b-4c83")
	if err != nil || got.Turns["t"] == nil {
		t.Fatalf("Load = %+v, %v", got, err)
	}

	evil := New("../../etc/passwd")
	if err := store.Save(evil); err != nil {
		t.Fatal(err)
	}
	ids, _ := store.List()
	for _, id := range ids {
		if strings.Contains(id, "/") || strings.Contains(id, "..") {
			t.Errorf("unsafe id leaked into file name: %q", id)
		}
	}
	matches, _ := filepath.Glob(filepath.Join(dir, "*.json"))
	if len(matches) != 2 {
		t.Errorf("expected 2 files inside store dir, got %v", matches)
	}

	if fresh, err := store.Load("never-saved"); err != nil || len(fresh.Turns) != 0 {
		t.Errorf("missing session should load empty: %+v, %v", fresh, err)
	}
	if err := store.Delete("01a1c37b-4c83"); err != nil {
		t.Fatal(err)
	}
	if err := store.Delete("01a1c37b-4c83"); err != nil {
		t.Errorf("deleting twice should be a no-op: %v", err)
	}
}

func TestStoreLoadCorruptFile(t *testing.T) {
	dir := t.TempDir()
	store := NewStore(dir)
	if err := writeRaw(filepath.Join(dir, "bad.json"), "{not json"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Load("bad"); err == nil {
		t.Error("corrupt state should return an error")
	}
	if ids, _ := NewStore(filepath.Join(dir, "missing")).List(); len(ids) != 0 {
		t.Error("missing dir should list nothing")
	}
}

func TestSpawnForPicksLatestSpawnBeforeAgentStart(t *testing.T) {
	s := New("s")
	spawn := func(at time.Duration, id string) {
		apply(s, at, event.Payload{HookEventName: event.PostToolUse, TurnID: "main", ToolUseID: id,
			ToolName: "collaborationspawn_agent", ToolResponse: json.RawMessage(`"{\"task_name\":\"/root/w\"}"`)})
	}
	spawn(1*time.Second, "first")
	spawn(5*time.Second, "second")
	spawn(5*time.Second, "second") // 重放

	if ref, _ := s.SpawnFor("/root/w", t0.Add(3*time.Second)); ref.ToolUseID != "first" {
		t.Errorf("agent started at 3s should link to first spawn, got %q", ref.ToolUseID)
	}
	if ref, _ := s.SpawnFor("/root/w", t0.Add(6*time.Second)); ref.ToolUseID != "second" {
		t.Errorf("agent started at 6s should link to second spawn, got %q", ref.ToolUseID)
	}
	if len(s.Spawns["/root/w"]) != 2 {
		t.Errorf("replayed spawn must not duplicate: %+v", s.Spawns["/root/w"])
	}
}

func TestMergeScanSkipsEmittedAndPrunesOrphans(t *testing.T) {
	s := New("s")
	s.Emitted["done"] = t0
	s.MergeScan("/r.jsonl", rollout.Result{Turns: map[string]rollout.TurnFacts{
		"done":    {TurnID: "done", Completed: true},
		"history": {TurnID: "history", Completed: true, CompletedAt: t0},
		"future":  {TurnID: "future"},
	}}, t0)
	f := s.Files["/r.jsonl"]
	if _, ok := f.Turns["done"]; ok {
		t.Error("facts for emitted turns must be skipped")
	}

	s.Prune(t0.Add(time.Hour), 24*time.Hour)

	if _, ok := f.Turns["history"]; ok {
		t.Error("finished orphan facts should be pruned")
	}
	if _, ok := f.Turns["future"]; !ok {
		t.Error("unfinished facts (turn may not have hooked yet) must be kept")
	}
}
