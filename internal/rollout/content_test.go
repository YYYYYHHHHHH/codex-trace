package rollout

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const (
	contentTurn  = "turn-a"
	contentTurn2 = "turn-b"
)

// contentRollout 是一份合成的 rollout：两轮对话，第一轮三次模型调用，第二轮前发生一次上下文压缩。
var contentRollout = []string{
	`{"type":"session_meta","payload":{"id":"thread","base_instructions":{"text":"You are Codex."}}}`,
	`{"type":"event_msg","payload":{"type":"task_started","turn_id":"turn-a"}}`,
	`{"type":"response_item","payload":{"type":"additional_tools","tools":[{"type":"function","name":"exec"}]}}`,
	`{"type":"response_item","payload":{"type":"message","role":"developer","content":[{"type":"input_text","text":"AGENTS.md rules"}],"internal_chat_message_metadata_passthrough":{"turn_id":"turn-a"}}}`,
	`{"type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"check mail"},{"type":"input_image","image_url":"data:..."}],"internal_chat_message_metadata_passthrough":{"turn_id":"turn-a"}}}`,
	`{"type":"response_item","payload":{"type":"message","role":"assistant","content":[{"type":"output_text","text":"Looking."}],"internal_chat_message_metadata_passthrough":{"turn_id":"turn-a"}}}`,
	`{"type":"response_item","payload":{"type":"custom_tool_call","call_id":"c1","name":"exec","input":"ls","internal_chat_message_metadata_passthrough":{"turn_id":"turn-a"}}}`,
	`{"type":"token_usage_record","payload":{"turn_id":"turn-a","response_id":"r1","usage":{"input_tokens":10}}}`,
	`{"type":"response_item","payload":{"type":"custom_tool_call_output","call_id":"c1","output":[{"type":"input_text","text":"a.txt"}],"internal_chat_message_metadata_passthrough":{"turn_id":"turn-a"}}}`,
	`{"type":"response_item","payload":{"type":"function_call","call_id":"c2","name":"read","arguments":"{\"path\":\"a.txt\"}","internal_chat_message_metadata_passthrough":{"turn_id":"turn-a"}}}`,
	`{"type":"token_usage_record","payload":{"turn_id":"turn-a","response_id":"r2","usage":{"input_tokens":20}}}`,
	`{"type":"response_item","payload":{"type":"function_call_output","call_id":"c2","output":"hello","internal_chat_message_metadata_passthrough":{"turn_id":"turn-a"}}}`,
	`{"type":"response_item","payload":{"type":"reasoning","encrypted_content":"secret","summary":[],"internal_chat_message_metadata_passthrough":{"turn_id":"turn-a"}}}`,
	`{"type":"response_item","payload":{"type":"message","role":"assistant","content":[{"type":"output_text","text":"Done."}],"internal_chat_message_metadata_passthrough":{"turn_id":"turn-a"}}}`,
	`{"type":"token_usage_record","payload":{"turn_id":"turn-a","response_id":"r3","usage":{"input_tokens":30}}}`,
	`{"type":"event_msg","payload":{"type":"task_complete","turn_id":"turn-a"}}`,
	`{"type":"compacted","payload":{"message":"","replacement_history":[{"type":"message","role":"user","content":[{"type":"input_text","text":"summary of earlier work"}]},{"type":"compaction","encrypted_content":"x"}]}}`,
	`{"type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"next"}],"internal_chat_message_metadata_passthrough":{"turn_id":"turn-b"}}}`,
	`{"type":"response_item","payload":{"type":"local_shell_call","call_id":"c3","action":{"command":["pwd"]},"internal_chat_message_metadata_passthrough":{"turn_id":"turn-b"}}}`,
	`{"type":"token_usage_record","payload":{"turn_id":"turn-b","response_id":"r4","usage":{"input_tokens":5}}}`,
	"not json",
}

func writeContentRollout(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "rollout-test.jsonl")
	if err := os.WriteFile(path, []byte(strings.Join(contentRollout, "\n")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func readContent(t *testing.T, turns ...string) map[string]TurnContent {
	t.Helper()
	want := map[string]bool{}
	for _, id := range turns {
		want[id] = true
	}
	got, err := ReadContent(writeContentRollout(t), want)
	if err != nil {
		t.Fatal(err)
	}
	return got
}

func roles(msgs []Message) string {
	out := make([]string, len(msgs))
	for i, m := range msgs {
		out[i] = m.Role
		if len(m.ToolCalls) > 0 {
			out[i] += "(call " + m.ToolCalls[0].Function.Name + ")"
		}
	}
	return strings.Join(out, ",")
}

func TestReadContentSplitsEachGenerationIntoNewInputAndOutput(t *testing.T) {
	tc := readContent(t, contentTurn)[contentTurn]

	cases := []struct {
		resp, in, out string
	}{
		{"r1", "developer,user", "assistant,assistant(call exec)"},
		{"r2", "tool", "assistant(call read)"},
		{"r3", "tool", "assistant"},
	}
	for _, c := range cases {
		g, ok := tc.Generations[c.resp]
		if !ok {
			t.Fatalf("generation %s missing", c.resp)
		}
		if got := roles(g.Input); got != c.in {
			t.Errorf("%s input = %s, want %s", c.resp, got, c.in)
		}
		if got := roles(g.Output); got != c.out {
			t.Errorf("%s output = %s, want %s", c.resp, got, c.out)
		}
	}

	r1 := tc.Generations["r1"]
	if r1.Input[1].Content != "check mail\n[image]" {
		t.Errorf("user content = %q", r1.Input[1].Content)
	}
	if call := r1.Output[1].ToolCalls[0]; call.ID != "c1" || call.Type != "function" || call.Function.Arguments != "ls" {
		t.Errorf("custom tool call = %+v", call)
	}
	if out := tc.Generations["r2"].Input[0]; out.ToolCallID != "c1" || out.Content != "a.txt" {
		t.Errorf("custom tool output = %+v", out)
	}
	if out := tc.Generations["r3"].Input[0]; out.ToolCallID != "c2" || out.Content != "hello" {
		t.Errorf("function output = %+v", out)
	}
}

func TestReadContentNeverIncludesReasoning(t *testing.T) {
	data, _ := json.Marshal(readContent(t, contentTurn, contentTurn2))
	if strings.Contains(string(data), "secret") {
		t.Fatal("encrypted reasoning must not appear in content")
	}
}

func TestReadContentFullInputOfLastGeneration(t *testing.T) {
	tc := readContent(t, contentTurn)[contentTurn]

	if tc.LastResponseID != "r3" {
		t.Fatalf("last response = %q, want r3", tc.LastResponseID)
	}
	if tc.System != "You are Codex." {
		t.Errorf("system = %q", tc.System)
	}
	if len(tc.Tools) != 1 || !strings.Contains(string(tc.Tools[0]), `"exec"`) {
		t.Errorf("tools = %s", tc.Tools)
	}
	// 最后一次调用看到的历史：不含它自己产出的最终回复
	want := "developer,user,assistant,assistant(call exec),tool,assistant(call read),tool"
	if got := roles(tc.FullInput); got != want {
		t.Errorf("full input = %s, want %s", got, want)
	}
}

func TestReadContentUsesCompactedHistory(t *testing.T) {
	tc := readContent(t, contentTurn2)[contentTurn2]

	if got := roles(tc.FullInput); got != "user,user" {
		t.Fatalf("full input after compaction = %s, want user,user", got)
	}
	if tc.FullInput[0].Content != "summary of earlier work" {
		t.Errorf("replacement history not used: %+v", tc.FullInput[0])
	}
	call := tc.Generations["r4"].Output[0].ToolCalls[0]
	if call.ID != "c3" || call.Function.Name != "local_shell_call" || !strings.Contains(call.Function.Arguments, "pwd") {
		t.Errorf("generic tool call = %+v", call)
	}
	if strings.Contains(call.Function.Arguments, "internal_chat_message_metadata_passthrough") {
		t.Errorf("passthrough metadata should be stripped: %s", call.Function.Arguments)
	}
}

func TestReadContentOnlyReturnsRequestedTurns(t *testing.T) {
	got := readContent(t, contentTurn2)
	if _, ok := got[contentTurn]; ok || len(got) != 1 {
		t.Fatalf("unexpected turns: %v", got)
	}
}

func TestReadContentMissingFile(t *testing.T) {
	if _, err := ReadContent(filepath.Join(t.TempDir(), "missing.jsonl"), map[string]bool{"x": true}); err == nil {
		t.Fatal("expected error for missing file")
	}
}
