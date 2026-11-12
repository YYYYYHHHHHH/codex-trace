package trace

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/YYYYYHHHHHH/codex-trace/internal/otlp"
	"github.com/YYYYYHHHHHH/codex-trace/internal/rollout"
	"github.com/YYYYYHHHHHH/codex-trace/internal/session"
)

const secretToken = "sk-proj-abcdefghijklmnopqrstuvwxyz"

func genTurn() (session.Turn, rollout.TurnFacts) {
	turn := session.Turn{ID: "t", Start: t0, ClosedAt: t0.Add(time.Second), Close: session.CloseStop}
	facts := rollout.TurnFacts{Model: "m", Generations: []rollout.Generation{
		{ResponseID: "r1", Start: t0, End: t0.Add(100 * time.Millisecond)},
		{ResponseID: "r2", Start: t0, End: t0.Add(200 * time.Millisecond)},
	}}
	return turn, facts
}

func genContent() rollout.TurnContent {
	user := rollout.Message{Role: "user", Content: "use token " + secretToken}
	call := rollout.Message{Role: "assistant", ToolCalls: []rollout.ToolCall{{
		ID: "c1", Type: "function", Function: rollout.FunctionCall{Name: "exec", Arguments: "ls"},
	}}}
	result := rollout.Message{Role: "tool", ToolCallID: "c1", Content: "a.txt"}
	return rollout.TurnContent{
		Generations: map[string]rollout.GenerationContent{
			"r1": {Input: []rollout.Message{user}, Output: []rollout.Message{call}},
			"r2": {Input: []rollout.Message{result}, Output: []rollout.Message{{Role: "assistant", Content: "Done."}}},
		},
		LastResponseID: "r2",
		System:         "You are Codex.",
		Tools:          []json.RawMessage{json.RawMessage(`{"type":"function","name":"exec"}`)},
		FullInput:      []rollout.Message{user, call, result},
	}
}

// genAttr 返回 response_id 对应 generation span 的某个属性。
func genAttr(t *testing.T, spans []otlp.Span, resp, key string) (string, bool) {
	t.Helper()
	id := otlp.NewSpanID(KindGeneration, resp)
	for _, s := range spans {
		if s.SpanID != id {
			continue
		}
		for _, a := range s.Attrs {
			if a.Key == key {
				return a.Value.AsString(), true
			}
		}
		return "", false
	}
	t.Fatalf("generation %s not found", resp)
	return "", false
}

func buildGen(t *testing.T, tc rollout.TurnContent, capture bool) []otlp.Span {
	t.Helper()
	turn, facts := genTurn()
	return BuildTurn(turn, facts, Context{TraceKey: "t", Content: tc}, Policy{CaptureContent: capture})
}

func TestGenerationContentRequiresCapture(t *testing.T) {
	spans := buildGen(t, genContent(), false)
	for _, resp := range []string{"r1", "r2"} {
		for _, key := range []string{attrObsInput, attrObsOutput} {
			if _, ok := genAttr(t, spans, resp, key); ok {
				t.Errorf("%s carries %s although capture_content=false", resp, key)
			}
		}
	}
}

func TestGenerationCarriesNewInputAndOutput(t *testing.T) {
	spans := buildGen(t, genContent(), true)

	in, _ := genAttr(t, spans, "r1", attrObsInput)
	var msgs []rollout.Message
	if err := json.Unmarshal([]byte(in), &msgs); err != nil {
		t.Fatalf("input is not a message array: %q", in)
	}
	if len(msgs) != 1 || msgs[0].Role != "user" {
		t.Errorf("input = %+v", msgs)
	}
	if strings.Contains(in, secretToken) {
		t.Errorf("input not redacted: %s", in)
	}

	out, _ := genAttr(t, spans, "r1", attrObsOutput)
	var outMsgs []rollout.Message
	if err := json.Unmarshal([]byte(out), &outMsgs); err != nil || len(outMsgs) != 1 || outMsgs[0].ToolCalls[0].Function.Name != "exec" {
		t.Errorf("output = %q (%v)", out, err)
	}
}

func TestLastGenerationCarriesFullInput(t *testing.T) {
	spans := buildGen(t, genContent(), true)

	in, _ := genAttr(t, spans, "r2", attrObsInput)
	var full struct {
		Tools    []json.RawMessage `json:"tools"`
		Messages []rollout.Message `json:"messages"`
	}
	if err := json.Unmarshal([]byte(in), &full); err != nil {
		t.Fatalf("full input not an object: %q", in)
	}
	if len(full.Tools) != 1 {
		t.Errorf("tools = %s", full.Tools)
	}
	roles := make([]string, len(full.Messages))
	for i, m := range full.Messages {
		roles[i] = m.Role
	}
	if got := strings.Join(roles, ","); got != "system,user,assistant,tool" {
		t.Errorf("full input roles = %s", got)
	}
	if full.Messages[0].Content != "You are Codex." {
		t.Errorf("system message = %+v", full.Messages[0])
	}
	if strings.Contains(in, secretToken) {
		t.Error("full input not redacted")
	}
	if out, _ := genAttr(t, spans, "r2", attrObsOutput); !strings.Contains(out, "Done.") {
		t.Errorf("last output = %q", out)
	}
}

func TestFullInputDropsOldestMessagesBeyondLimit(t *testing.T) {
	tc := genContent()
	tc.FullInput = nil
	for i := 0; i < 3000; i++ {
		tc.FullInput = append(tc.FullInput, rollout.Message{Role: "user", Content: fmt.Sprintf("msg-%04d %s", i, strings.Repeat("x", 1000))})
	}

	in, _ := genAttr(t, buildGen(t, tc, true), "r2", attrObsInput)

	if len(in) > maxFullInputBytes {
		t.Errorf("full input %d bytes exceeds %d", len(in), maxFullInputBytes)
	}
	if !strings.Contains(in, "msg-2999") || strings.Contains(in, "msg-0000") {
		t.Error("should keep newest messages and drop oldest")
	}
	var full fullInput
	if err := json.Unmarshal([]byte(in), &full); err != nil {
		t.Fatal(err)
	}
	var kept int
	for _, m := range full.Messages {
		if strings.HasPrefix(m.Content, "msg-") {
			kept++
		}
	}
	if marker := fmt.Sprintf("省略了 %d 条", 3000-kept); !strings.Contains(in, marker) {
		t.Errorf("omission marker should count all dropped messages (%s)", marker)
	}
	if !strings.Contains(in, "You are Codex.") {
		t.Error("system prompt and omission marker should be kept")
	}
}

func TestGenerationMessagesAreClipped(t *testing.T) {
	tc := genContent()
	huge := strings.Repeat("y", 3*maxMessageBytes)
	tc.Generations["r1"] = rollout.GenerationContent{
		Input: []rollout.Message{{Role: "tool", Content: huge}},
		Output: []rollout.Message{{Role: "assistant", ToolCalls: []rollout.ToolCall{{
			ID: "c", Type: "function", Function: rollout.FunctionCall{Name: "exec", Arguments: huge},
		}}}},
	}

	spans := buildGen(t, tc, true)

	for _, key := range []string{attrObsInput, attrObsOutput} {
		v, _ := genAttr(t, spans, "r1", key)
		if len(v) > 2*maxMessageBytes || !strings.Contains(v, "truncated") {
			t.Errorf("%s not clipped: %d bytes", key, len(v))
		}
	}
}

func TestOversizedToolDefinitionsKeepOnlyNames(t *testing.T) {
	tc := genContent()
	desc := strings.Repeat("d", maxToolsBytes)
	tc.Tools = []json.RawMessage{json.RawMessage(fmt.Sprintf(`{"type":"function","name":"exec","description":%q}`, desc))}

	in, _ := genAttr(t, buildGen(t, tc, true), "r2", attrObsInput)

	if strings.Contains(in, desc[:100]) || !strings.Contains(in, `"exec"`) {
		t.Errorf("oversized tools should be reduced to names: %d bytes", len(in))
	}
}

func TestGenerationWithoutContentHasNoInput(t *testing.T) {
	spans := buildGen(t, rollout.TurnContent{}, true)
	if _, ok := genAttr(t, spans, "r1", attrObsInput); ok {
		t.Error("generation without rollout content should not carry input")
	}
}
