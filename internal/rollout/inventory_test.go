package rollout

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// skillsText 模仿 Codex developer 消息中的 skill 段：根目录表 + 可用 skill 列表。
const skillsText = "<skills_instructions>\n## Skills\n### Skill roots\n" +
	"- `r0` = `/Users/alice/.codex/skills`\n" +
	"- `r1` = `/Users/alice/.codex/plugins/cache/composio`\n" +
	"### Available skills\n" +
	"- imagegen: Generate images (for example (png) files). (file: r0/imagegen/SKILL.md)\n" +
	"- composio:composio: Route Composio tasks. (file: r1/composio/0.2.3/skills/composio/SKILL.md)\n" +
	"- broken entry without file\n" +
	"</skills_instructions>"

func inventoryRollout(t *testing.T, readSkill string) string {
	t.Helper()
	dev, _ := json.Marshal(map[string]any{
		"type": "message", "role": "developer",
		"content": []map[string]string{{"type": "input_text", "text": skillsText}},
		"internal_chat_message_metadata_passthrough": map[string]string{"turn_id": "turn-a"},
	})
	call, _ := json.Marshal(map[string]any{
		"type": "custom_tool_call", "call_id": "c1", "name": "exec", "input": "cat " + readSkill,
		"internal_chat_message_metadata_passthrough": map[string]string{"turn_id": "turn-a"},
	})
	lines := []string{
		`{"type":"session_meta","payload":{"id":"thread","base_instructions":{"text":"sys"}}}`,
		`{"type":"response_item","payload":{"type":"additional_tools","tools":[` +
			`{"type":"namespace","name":"functions","tools":[{"type":"function","name":"exec"},{"type":"function","name":"wait"}]},` +
			`{"type":"namespace","name":"mcp__cua_repl","tools":[{"type":"function","name":"js"}]},` +
			`{"type":"function","name":"web_search"}]}}`,
		`{"type":"response_item","payload":` + string(dev) + `}`,
		`{"type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"go"}],"internal_chat_message_metadata_passthrough":{"turn_id":"turn-a"}}}`,
		`{"type":"response_item","payload":` + string(call) + `}`,
		`{"type":"token_usage_record","payload":{"turn_id":"turn-a","response_id":"r1","usage":{"input_tokens":1}}}`,
		`{"type":"response_item","payload":{"type":"message","role":"assistant","content":[{"type":"output_text","text":"ok"}],"internal_chat_message_metadata_passthrough":{"turn_id":"turn-a"}}}`,
		`{"type":"token_usage_record","payload":{"turn_id":"turn-a","response_id":"r2","usage":{"input_tokens":1}}}`,
	}
	path := filepath.Join(t.TempDir(), "rollout-inv.jsonl")
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func inventoryFor(t *testing.T, readSkill string) Inventory {
	t.Helper()
	got, err := ReadContent(inventoryRollout(t, readSkill), map[string]bool{"turn-a": true})
	if err != nil {
		t.Fatal(err)
	}
	return got["turn-a"].Inventory
}

func TestInventoryListsToolsAndMCPServers(t *testing.T) {
	inv := inventoryFor(t, "/tmp/nothing")

	wantTools := []string{"functions.exec", "functions.wait", "mcp__cua_repl.js", "web_search"}
	if !reflect.DeepEqual(inv.Tools, wantTools) {
		t.Errorf("tools = %v, want %v", inv.Tools, wantTools)
	}
	if !reflect.DeepEqual(inv.MCPServers, []string{"cua_repl"}) {
		t.Errorf("mcp servers = %v", inv.MCPServers)
	}
}

func TestInventoryParsesSkillsWithExpandedPaths(t *testing.T) {
	inv := inventoryFor(t, "/tmp/nothing")

	want := []Skill{
		{Name: "imagegen", File: "/Users/alice/.codex/skills/imagegen/SKILL.md"},
		{Name: "composio:composio", File: "/Users/alice/.codex/plugins/cache/composio/composio/0.2.3/skills/composio/SKILL.md"},
	}
	if !reflect.DeepEqual(inv.Skills, want) {
		t.Errorf("skills = %+v, want %+v", inv.Skills, want)
	}
	if len(inv.SkillsUsed) != 0 {
		t.Errorf("no skill was read, got %v", inv.SkillsUsed)
	}
}

func TestInventoryDetectsSkillsReadInTurn(t *testing.T) {
	cases := map[string]string{
		"absolute path": "/Users/alice/.codex/plugins/cache/composio/composio/0.2.3/skills/composio/SKILL.md",
		"home relative": "~/.codex/plugins/cache/composio/composio/0.2.3/skills/composio/SKILL.md",
	}
	for name, read := range cases {
		t.Run(name, func(t *testing.T) {
			if got := inventoryFor(t, read).SkillsUsed; !reflect.DeepEqual(got, []string{"composio:composio"}) {
				t.Errorf("skills used = %v", got)
			}
		})
	}
}

func TestParseSkillsWithoutRootsKeepsShortPath(t *testing.T) {
	got := parseSkills("### Available skills\n- a: desc (file: r9/a/SKILL.md)\n</skills_instructions>")
	if !reflect.DeepEqual(got, []Skill{{Name: "a", File: "r9/a/SKILL.md"}}) {
		t.Errorf("skills = %+v", got)
	}
}
