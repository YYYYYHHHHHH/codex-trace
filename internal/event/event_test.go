package event

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func loadFixture(t *testing.T, name string) []byte {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("..", "..", "testdata", "hook-payloads", name))
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func TestDecodeRealPayloads(t *testing.T) {
	cases := []struct {
		fixture string
		check   func(t *testing.T, p Payload)
	}{
		{"SessionStart-startup.json", func(t *testing.T, p Payload) {
			if p.HookEventName != SessionStart || p.Source != "startup" || p.SessionID == "" || p.TurnID != "" {
				t.Errorf("unexpected: %+v", p)
			}
		}},
		{"UserPromptSubmit.json", func(t *testing.T, p Payload) {
			if p.TurnID == "" || p.Prompt == "" {
				t.Errorf("unexpected: %+v", p)
			}
		}},
		{"PostToolUse-Bash-failed.json", func(t *testing.T, p Payload) {
			if p.ToolName != "Bash" || p.ToolUseID == "" || p.ToolResponseText() == "" {
				t.Errorf("unexpected: %+v", p)
			}
		}},
		{"PreToolUse-subagent-Bash.json", func(t *testing.T, p Payload) {
			if p.AgentID == "" || !p.IsSubagent() {
				t.Errorf("subagent payload not detected: %+v", p)
			}
		}},
		{"SubagentStop.json", func(t *testing.T, p Payload) {
			if p.AgentTranscriptPath == "" || p.LastAssistantMessage == "" {
				t.Errorf("unexpected: %+v", p)
			}
		}},
	}
	for _, tc := range cases {
		t.Run(tc.fixture, func(t *testing.T) {
			rec := Record{Version: RecordVersion, ReceivedAt: time.Now(), Payload: loadFixture(t, tc.fixture)}
			p, err := rec.Decode()
			if err != nil {
				t.Fatal(err)
			}
			tc.check(t, p)
		})
	}
}

func TestToolResponseTextHandlesNonString(t *testing.T) {
	p := Payload{ToolResponse: json.RawMessage(`{"ok":true}`)}
	if got := p.ToolResponseText(); got != `{"ok":true}` {
		t.Errorf("ToolResponseText = %q", got)
	}
	if got := (Payload{}).ToolResponseText(); got != "" {
		t.Errorf("empty response should be empty, got %q", got)
	}
}

func TestDecodeRejectsMissingSession(t *testing.T) {
	rec := Record{Payload: json.RawMessage(`{"hook_event_name":"Stop"}`)}
	if _, err := rec.Decode(); err == nil {
		t.Error("expected error for payload without session_id")
	}
}

func TestRecordRoundTrip(t *testing.T) {
	in := Record{Version: RecordVersion, ReceivedAt: time.Unix(1700000000, 123).UTC(), Event: "Stop", Payload: json.RawMessage(`{"session_id":"s"}`)}
	data, err := json.Marshal(in)
	if err != nil {
		t.Fatal(err)
	}
	out, err := ParseRecord(data)
	if err != nil {
		t.Fatal(err)
	}
	if !out.ReceivedAt.Equal(in.ReceivedAt) || out.Event != "Stop" || string(out.Payload) != `{"session_id":"s"}` {
		t.Errorf("round trip mismatch: %+v", out)
	}
	if _, err := ParseRecord([]byte(`{"v":99}`)); err == nil {
		t.Error("unknown record version should be rejected")
	}
}
