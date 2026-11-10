// Package event 定义 spool 记录格式与 Codex hook payload。
package event

import (
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

// RecordVersion 是 spool 记录格式版本，格式变更时递增。
const RecordVersion = 1

// Codex hook 事件名（payload 中的 hook_event_name）。
const (
	SessionStart      = "SessionStart"
	UserPromptSubmit  = "UserPromptSubmit"
	PreToolUse        = "PreToolUse"
	PostToolUse       = "PostToolUse"
	PermissionRequest = "PermissionRequest"
	SubagentStart     = "SubagentStart"
	SubagentStop      = "SubagentStop"
	Stop              = "Stop"
	Interrupt         = "Interrupt"
)

// ContentSizesField 是 hook 剥离内容字段后记录各字段原始长度的字段名。
const ContentSizesField = "codex_trace_content_sizes"

// Record 是 hook 写入 spool 的一条记录：原始 payload + 接收时间。
type Record struct {
	Version    int             `json:"v"`
	ReceivedAt time.Time       `json:"received_at"`
	Event      string          `json:"event"`
	Payload    json.RawMessage `json:"payload"`
}

// ParseRecord 解析 spool 文件内容。
func ParseRecord(data []byte) (Record, error) {
	var r Record
	if err := json.Unmarshal(data, &r); err != nil {
		return Record{}, err
	}
	if r.Version != RecordVersion {
		return Record{}, fmt.Errorf("event: unsupported record version %d", r.Version)
	}
	return r, nil
}

// Payload 是 Codex hook stdin 的字段并集，见 docs/payload-notes.md。
type Payload struct {
	SessionID      string `json:"session_id"`
	TurnID         string `json:"turn_id"`
	TranscriptPath string `json:"transcript_path"`
	Cwd            string `json:"cwd"`
	HookEventName  string `json:"hook_event_name"`
	Model          string `json:"model"`
	PermissionMode string `json:"permission_mode"`
	Source         string `json:"source"`

	Prompt string `json:"prompt"`

	ToolName     string          `json:"tool_name"`
	ToolInput    json.RawMessage `json:"tool_input"`
	ToolResponse json.RawMessage `json:"tool_response"`
	ToolUseID    string          `json:"tool_use_id"`

	AgentID             string `json:"agent_id"`
	AgentType           string `json:"agent_type"`
	AgentTranscriptPath string `json:"agent_transcript_path"`

	LastAssistantMessage string `json:"last_assistant_message"`

	// ContentSizes 在 capture_content=false 时由 hook 写入：被剥离字段 → 原始字节数。
	ContentSizes map[string]int `json:"codex_trace_content_sizes"`
}

// Decode 解析记录中的 payload；缺少 session_id 的 payload 无法关联，视为无效。
func (r Record) Decode() (Payload, error) {
	var p Payload
	if err := json.Unmarshal(r.Payload, &p); err != nil {
		return Payload{}, err
	}
	if p.SessionID == "" {
		return Payload{}, errors.New("event: payload has no session_id")
	}
	if p.HookEventName == "" {
		p.HookEventName = r.Event
	}
	return p, nil
}

// IsSubagent 表示事件发生在子 agent 内（或是子 agent 生命周期事件）。
func (p Payload) IsSubagent() bool { return p.AgentID != "" }

// ToolResponseText 返回工具输出文本；Codex 目前给的是 JSON 字符串，其他类型原样返回。
func (p Payload) ToolResponseText() string {
	if len(p.ToolResponse) == 0 {
		return ""
	}
	var s string
	if err := json.Unmarshal(p.ToolResponse, &s); err == nil {
		return s
	}
	return string(p.ToolResponse)
}
