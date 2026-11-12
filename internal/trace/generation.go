package trace

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/YYYYYHHHHHH/codex-trace/internal/otlp"
	"github.com/YYYYYHHHHHH/codex-trace/internal/rollout"
)

// 模型调用内容的大小上限。单条消息先脱敏再截断；整个字段超限时从最早的消息开始丢弃。
const (
	maxMessageBytes   = 64 << 10  // 系统提示词、含 skill 列表的 developer 消息通常在 20～40KB
	maxGenFieldBytes  = 256 << 10 // 新增输入、产出
	maxFullInputBytes = 1 << 20   // 每轮最后一次调用的完整输入
	maxToolsBytes     = 256 << 10 // 工具定义超过此值时只保留名称
)

// fullInput 是每轮最后一次模型调用的完整输入。
type fullInput struct {
	Tools    []json.RawMessage `json:"tools,omitempty"`
	Messages []rollout.Message `json:"messages"`
}

// generationContent 返回模型调用的 input / output 属性：
// 每轮最后一次调用带完整输入（系统提示词 + 工具定义 + 全部历史），其余调用只带新增输入。
func generationContent(responseID string, tc rollout.TurnContent) []otlp.Attr {
	g, ok := tc.Generations[responseID]
	if !ok {
		return nil
	}
	var in string
	if responseID == tc.LastResponseID {
		in = encodeFullInput(tc)
	} else {
		in = encodeMessages(fitMessages(clipMessages(g.Input), maxGenFieldBytes, 0))
	}
	out := encodeMessages(fitMessages(clipMessages(g.Output), maxGenFieldBytes, 0))
	return []otlp.Attr{otlp.String(attrObsInput, in), otlp.String(attrObsOutput, out)}
}

func encodeFullInput(tc rollout.TurnContent) string {
	tools := limitTools(tc.Tools)
	budget := maxFullInputBytes - jsonSize(tools) - 64
	var system []rollout.Message
	if tc.System != "" {
		system = clipMessages([]rollout.Message{{Role: "system", Content: tc.System}})
		budget -= jsonSize(system)
	}
	// 先按原始大小粗筛，只对可能保留的最近消息做脱敏，避免长会话每轮都处理全部历史
	recent := newestWithin(tc.FullInput, budget)
	dropped := len(tc.FullInput) - len(recent)
	msgs := append(system, fitMessages(clipMessages(recent), budget, dropped)...)
	data, err := json.Marshal(fullInput{Tools: tools, Messages: msgs})
	if err != nil {
		return ""
	}
	return string(data)
}

// clipMessages 返回脱敏、截断后的副本，不修改入参。
func clipMessages(msgs []rollout.Message) []rollout.Message {
	out := make([]rollout.Message, len(msgs))
	for i, m := range msgs {
		m.Content = clip(m.Content, maxMessageBytes)
		if len(m.ToolCalls) > 0 {
			calls := make([]rollout.ToolCall, len(m.ToolCalls))
			for j, c := range m.ToolCalls {
				c.Function.Arguments = clip(c.Function.Arguments, maxMessageBytes)
				calls[j] = c
			}
			m.ToolCalls = calls
		}
		out[i] = m
	}
	return out
}

// newestWithin 返回最近的、按截断后大小估算不超过 budget 的消息。
func newestWithin(msgs []rollout.Message, budget int) []rollout.Message {
	total := 0
	start := len(msgs)
	for start > 0 {
		m := msgs[start-1]
		size := min(len(m.Content), maxMessageBytes)
		for _, c := range m.ToolCalls {
			size += min(len(c.Function.Arguments), maxMessageBytes)
		}
		if total+size > budget {
			break
		}
		total += size
		start--
	}
	return msgs[start:]
}

// fitMessages 从最早的消息开始丢弃，直到编码后不超过 budget，并在开头注明省略了多少条（含此前已丢弃的 dropped 条）。
func fitMessages(msgs []rollout.Message, budget, dropped int) []rollout.Message {
	total := 2
	start := len(msgs)
	for start > 0 {
		size := jsonSize(msgs[start-1]) + 1
		if total+size > budget-128 { // 预留省略说明的空间
			break
		}
		total += size
		start--
	}
	if start+dropped == 0 {
		return msgs
	}
	marker := rollout.Message{Role: "system", Content: fmt.Sprintf("[codex-trace] 超出大小上限，省略了 %d 条较早的消息", start+dropped)}
	return append([]rollout.Message{marker}, msgs[start:]...)
}

// limitTools 在工具定义过大时只保留名称。
func limitTools(tools []json.RawMessage) []json.RawMessage {
	if jsonSize(tools) <= maxToolsBytes {
		return tools
	}
	out := make([]json.RawMessage, 0, len(tools))
	for _, t := range tools {
		var head struct {
			Type string `json:"type"`
			Name string `json:"name"`
		}
		_ = json.Unmarshal(t, &head)
		data, _ := json.Marshal(head)
		out = append(out, data)
	}
	return out
}

func encodeMessages(msgs []rollout.Message) string {
	data, err := json.Marshal(msgs)
	if err != nil {
		return ""
	}
	return string(data)
}

func jsonSize(v any) int {
	data, err := json.Marshal(v)
	if err != nil {
		return 0
	}
	return len(data)
}

// addInventoryMeta 把上下文中的工具与 skill 清单写入元数据（逗号分隔，便于在 Langfuse 中按"包含"筛选）。
// withFiles 时附带每个 skill 的 SKILL.md 路径（~/ 形式，不含用户名），只放在轮次 observation 上。
func addInventoryMeta(meta map[string]any, inv rollout.Inventory, withFiles bool) {
	if inv.Empty() {
		return
	}
	names := make([]string, len(inv.Skills))
	files := make([]string, len(inv.Skills))
	for i, s := range inv.Skills {
		names[i] = s.Name
		files[i] = s.Name + ": " + rollout.HomeRelative(s.File)
	}
	meta["context_tools"] = strings.Join(inv.Tools, ", ")
	meta["context_tool_count"] = len(inv.Tools)
	meta["context_mcp_servers"] = strings.Join(inv.MCPServers, ", ")
	meta["context_skills"] = strings.Join(names, ", ")
	meta["context_skill_count"] = len(inv.Skills)
	meta["skills_used"] = strings.Join(inv.SkillsUsed, ", ")
	if withFiles {
		meta["context_skill_files"] = strings.Join(files, "\n")
	}
}

// inventoryTags 把本轮读取过的 skill 与上下文中的 MCP 服务做成 trace 标签。
// Langfuse 列表与筛选读取的元数据值会被截断到 200 字符，标签不会，按标签筛选更可靠。
func inventoryTags(inv rollout.Inventory) []string {
	tags := make([]string, 0, len(inv.SkillsUsed)+len(inv.MCPServers))
	for _, s := range inv.SkillsUsed {
		tags = append(tags, "skill:"+s)
	}
	for _, m := range inv.MCPServers {
		tags = append(tags, "mcp:"+m)
	}
	return tags
}
