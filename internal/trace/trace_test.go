package trace

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/YYYYYHHHHHH/codex-trace/internal/otlp"
	"github.com/YYYYYHHHHHH/codex-trace/internal/rollout"
	"github.com/YYYYYHHHHHH/codex-trace/internal/session"
)

var t0 = time.Date(2026, 11, 9, 10, 0, 0, 0, time.UTC)

func find(spans []otlp.Span, key string) map[string]otlp.Value {
	out := map[string]otlp.Value{}
	for _, s := range spans {
		for _, a := range s.Attrs {
			if a.Key == key {
				out[s.Name] = a.Value
			}
		}
	}
	return out
}

func TestGenerationUsageSeparatesCachedTokens(t *testing.T) {
	turn := session.Turn{ID: "t", Start: t0, ClosedAt: t0.Add(time.Second), Close: session.CloseStop}
	facts := rollout.TurnFacts{Model: "m", Effort: "high", Generations: []rollout.Generation{{
		ResponseID: "r1", Start: t0, End: t0.Add(time.Second),
		Usage: rollout.Usage{Input: 100, CachedInput: 60, Output: 10, ReasoningOutput: 4, Total: 110},
	}}}

	spans := BuildTurn(turn, facts, Context{SessionID: "s", TraceKey: "t"}, Policy{})

	raw := find(spans, attrObsUsage)["m"].AsString()
	var usage map[string]int64
	if err := json.Unmarshal([]byte(raw), &usage); err != nil {
		t.Fatalf("usage not JSON: %q", raw)
	}
	if usage["input"] != 40 || usage["input_cached_tokens"] != 60 || usage["output"] != 10 || usage["total"] != 110 {
		t.Errorf("usage = %v", usage)
	}
	if params := find(spans, attrObsModelParam)["m"].AsString(); !strings.Contains(params, "high") {
		t.Errorf("model params = %q", params)
	}
}

func TestToolWithoutPostIsWarningAndEndsWithTurn(t *testing.T) {
	turn := session.Turn{
		ID: "t", Start: t0, ClosedAt: t0.Add(5 * time.Second), Close: session.CloseInterrupt,
		Tools: []session.Tool{{ID: "c1", Name: "Bash", Start: t0.Add(time.Second)}},
	}

	spans := BuildTurn(turn, rollout.TurnFacts{}, Context{TraceKey: "t"}, Policy{})

	tool := spans[1]
	if !tool.End.Equal(t0.Add(5 * time.Second)) {
		t.Errorf("unfinished tool should end with the turn, got %v", tool.End)
	}
	if lvl := find(spans, attrObsLevel)["Bash"].AsString(); lvl != levelWarning {
		t.Errorf("level = %q", lvl)
	}
	if lvl := find(spans, attrObsLevel)["codex turn"].AsString(); lvl != levelWarning {
		t.Errorf("interrupted turn level = %q", lvl)
	}
}

func TestFailedStatusWithoutExitCode(t *testing.T) {
	turn := session.Turn{ID: "t", Start: t0, Tools: []session.Tool{{ID: "p1", Name: "apply_patch", Start: t0, End: t0, Done: true}}}
	facts := rollout.TurnFacts{Items: map[string]rollout.Item{"p1": {ID: "p1", Type: "FileChange", Status: "failed"}}}

	spans := BuildTurn(turn, facts, Context{TraceKey: "t"}, Policy{})

	if !spans[1].Status.Error || spans[1].Status.Message != "failed" {
		t.Errorf("status = %+v", spans[1].Status)
	}
}

func TestSubagentSpanNamingAndParent(t *testing.T) {
	parent := ToolSpanID("spawn")
	turn := session.Turn{ID: "sub", AgentID: "a", AgentType: "explorer", Start: t0}

	spans := BuildTurn(turn, rollout.TurnFacts{}, Context{TraceKey: "root", Parent: parent, AgentPath: "/root/x"}, Policy{})

	s := spans[0]
	if s.Name != "subagent:explorer" || s.Parent != parent || s.TraceID != otlp.NewTraceID("root") {
		t.Errorf("subagent span = %+v", s)
	}
	if _, ok := find(spans, attrTraceName)[s.Name]; ok {
		t.Error("subagent must not override the trace name")
	}
	if got := find(spans, attrObsMetaPref+"agent_path")[s.Name].AsString(); got != "/root/x" {
		t.Errorf("agent_path meta = %q", got)
	}
}

func TestContentPolicy(t *testing.T) {
	turn := session.Turn{
		ID: "t", Start: t0, Prompt: "token sk-proj-abcdefghijklmnopqrstuvwxyz " + strings.Repeat("x", 100),
		PromptBytes: 140, Tools: []session.Tool{{ID: "c", Name: "Bash", Input: json.RawMessage(`{"command":"ls"}`), InputBytes: 16, Done: true}},
	}

	off := BuildTurn(turn, rollout.TurnFacts{}, Context{TraceKey: "t"}, Policy{})
	if len(find(off, attrObsInput)) != 0 {
		t.Error("no content may be exported when capture is off")
	}
	if got := find(off, attrObsMetaPref+"input_bytes")["Bash"].AsInt(); got != 16 {
		t.Errorf("input_bytes = %d", got)
	}

	on := BuildTurn(turn, rollout.TurnFacts{}, Context{TraceKey: "t"}, Policy{CaptureContent: true, MaxFieldBytes: 40})
	in := find(on, attrTraceInput)["codex turn"].AsString()
	if strings.Contains(in, "abcdefghijklmnop") || !strings.Contains(in, "truncated") {
		t.Errorf("trace input not redacted/truncated: %q", in)
	}
}

func TestHashID(t *testing.T) {
	if HashID("user", "") != "" {
		t.Error("empty value should hash to empty")
	}
	if HashID("user", "a") == HashID("host", "a") || len(HashID("user", "a")) != 16 {
		t.Error("hash must be namespaced and 16 hex chars")
	}
}
