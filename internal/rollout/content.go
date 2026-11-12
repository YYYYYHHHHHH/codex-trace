package rollout

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
)

// Message 是模型输入 / 输出中的一条消息，由 Codex response item 归一化而来（OpenAI chat 格式，Langfuse 可直接渲染）。
type Message struct {
	Role       string     `json:"role"`
	Content    string     `json:"content,omitempty"`
	ToolCalls  []ToolCall `json:"tool_calls,omitempty"`
	ToolCallID string     `json:"tool_call_id,omitempty"`
}

// ToolCall 是模型发起的一次工具调用。
type ToolCall struct {
	ID       string       `json:"id"`
	Type     string       `json:"type"`
	Function FunctionCall `json:"function"`
}

// FunctionCall 是工具名与参数原文。
type FunctionCall struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

// GenerationContent 是一次模型调用的内容。推理（reasoning）在 rollout 中是加密的，永不包含。
type GenerationContent struct {
	Input  []Message // 相对上一次调用新增的输入：用户 prompt、上一个工具的结果等
	Output []Message // 本次调用产出的回复与工具调用
}

// TurnContent 是某个轮次内所有模型调用的内容。
type TurnContent struct {
	Generations map[string]GenerationContent // 按 response_id
	// LastResponseID 是本轮最后一次模型调用；System、Tools、FullInput 是它看到的完整输入。
	LastResponseID string
	System         string
	Tools          []json.RawMessage
	FullInput      []Message
}

// entry 是会话历史中的一项：一条消息，或一组工具定义（additional_tools）。
type entry struct {
	msg   *Message
	tools []json.RawMessage
}

type turnState struct {
	content  TurnContent
	pendIn   []Message
	pendOut  []Message
	firstOut int // 本次调用第一条产出在 history 中的下标，-1 表示还没有产出
	lastCut  []entry
}

type contentBuilder struct {
	want    map[string]bool
	system  string
	history []entry
	turns   map[string]*turnState
}

// ReadContent 从头读取 rollout，重建指定轮次中每次模型调用的内容。
// 会话历史按出现顺序累积，遇到 compacted 记录时整体替换为压缩后的历史。
func ReadContent(path string, turnIDs map[string]bool) (map[string]TurnContent, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("rollout: open %s: %w", path, err)
	}
	defer f.Close()

	b := &contentBuilder{want: turnIDs, turns: map[string]*turnState{}}
	r := bufio.NewReaderSize(f, 256<<10)
	for {
		line, err := readLine(r)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("rollout: read %s: %w", path, err)
		}
		if len(line) <= maxLineBytes {
			b.add(line)
		}
	}
	return b.result(), nil
}

func (b *contentBuilder) add(line []byte) {
	var rec rawRecord
	if json.Unmarshal(line, &rec) != nil || len(rec.Payload) == 0 {
		return
	}
	switch rec.Type {
	case "session_meta":
		b.addMeta(rec.Payload)
	case "compacted":
		b.addCompacted(rec.Payload)
	case "response_item":
		b.addItem(rec.Payload)
	case "token_usage_record":
		b.addUsage(rec.Payload)
	}
}

// addMeta 取文件中第一个 session_meta 的系统提示词（子 agent 文件中后续的 meta 来自复制的父会话）。
func (b *contentBuilder) addMeta(raw json.RawMessage) {
	if b.system != "" {
		return
	}
	var p struct {
		BaseInstructions struct {
			Text string `json:"text"`
		} `json:"base_instructions"`
	}
	if json.Unmarshal(raw, &p) == nil {
		b.system = p.BaseInstructions.Text
	}
}

func (b *contentBuilder) addCompacted(raw json.RawMessage) {
	var p struct {
		ReplacementHistory []json.RawMessage `json:"replacement_history"`
	}
	if json.Unmarshal(raw, &p) != nil || p.ReplacementHistory == nil {
		return
	}
	history := make([]entry, 0, len(p.ReplacementHistory))
	for _, item := range p.ReplacementHistory {
		if e, ok := toEntry(item); ok {
			history = append(history, e)
		}
	}
	b.history = history
	for _, st := range b.turns {
		st.firstOut = -1
	}
}

func (b *contentBuilder) addItem(raw json.RawMessage) {
	e, ok := toEntry(raw)
	if !ok {
		return
	}
	b.history = append(b.history, e)
	if e.msg == nil {
		return // 工具定义只进入完整输入，不算作某次调用的新增输入
	}
	var head payloadHead
	_ = json.Unmarshal(raw, &head)
	turnID := firstNonEmpty(head.TurnID, head.Passthrough.TurnID)
	if !b.want[turnID] {
		return
	}
	st := b.turn(turnID)
	if isOutput(*e.msg) {
		if st.firstOut < 0 {
			st.firstOut = len(b.history) - 1
		}
		st.pendOut = append(st.pendOut, *e.msg)
		return
	}
	st.pendIn = append(st.pendIn, *e.msg)
}

func (b *contentBuilder) addUsage(raw json.RawMessage) {
	var p struct {
		TurnID     string `json:"turn_id"`
		ResponseID string `json:"response_id"`
	}
	if json.Unmarshal(raw, &p) != nil || p.ResponseID == "" || !b.want[p.TurnID] {
		return
	}
	st := b.turn(p.TurnID)
	st.content.Generations[p.ResponseID] = GenerationContent{Input: st.pendIn, Output: st.pendOut}
	cut := len(b.history)
	if st.firstOut >= 0 && st.firstOut < cut {
		cut = st.firstOut
	}
	// 只追加、压缩时整体替换，因此截取的前缀之后不会被改写
	st.lastCut = b.history[:cut:cut]
	st.content.LastResponseID = p.ResponseID
	st.pendIn, st.pendOut, st.firstOut = nil, nil, -1
}

func (b *contentBuilder) turn(id string) *turnState {
	st, ok := b.turns[id]
	if !ok {
		st = &turnState{content: TurnContent{Generations: map[string]GenerationContent{}}, firstOut: -1}
		b.turns[id] = st
	}
	return st
}

func (b *contentBuilder) result() map[string]TurnContent {
	out := make(map[string]TurnContent, len(b.turns))
	for id, st := range b.turns {
		tc := st.content
		if tc.LastResponseID != "" {
			tc.System = b.system
			for _, e := range st.lastCut {
				if e.msg != nil {
					tc.FullInput = append(tc.FullInput, *e.msg)
				} else {
					tc.Tools = append(tc.Tools, e.tools...)
				}
			}
		}
		out[id] = tc
	}
	return out
}

func isOutput(m Message) bool {
	return m.Role == "assistant"
}

type rawItem struct {
	Type      string            `json:"type"`
	Role      string            `json:"role"`
	Content   []contentPart     `json:"content"`
	CallID    string            `json:"call_id"`
	Name      string            `json:"name"`
	Arguments string            `json:"arguments"`
	Input     string            `json:"input"`
	Output    json.RawMessage   `json:"output"`
	Tools     []json.RawMessage `json:"tools"`
}

type contentPart struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

// toEntry 把 response item 归一化；推理、压缩摘要等不可读或无关的条目返回 false。
func toEntry(raw json.RawMessage) (entry, bool) {
	var it rawItem
	if json.Unmarshal(raw, &it) != nil {
		return entry{}, false
	}
	switch {
	case it.Type == "additional_tools":
		return entry{tools: it.Tools}, len(it.Tools) > 0
	case it.Type == "message":
		return msgEntry(Message{Role: it.Role, Content: joinParts(it.Content)}), it.Role != ""
	case it.Type == "function_call":
		return callEntry(it.CallID, it.Name, it.Arguments), true
	case it.Type == "custom_tool_call":
		return callEntry(it.CallID, it.Name, it.Input), true
	case strings.HasSuffix(it.Type, "_call_output"):
		return msgEntry(Message{Role: "tool", ToolCallID: it.CallID, Content: outputText(it.Output)}), true
	case strings.HasSuffix(it.Type, "_call"):
		return callEntry(it.CallID, it.Type, stripMeta(raw)), true
	}
	return entry{}, false
}

func msgEntry(m Message) entry { return entry{msg: &m} }

func callEntry(id, name, args string) entry {
	return msgEntry(Message{Role: "assistant", ToolCalls: []ToolCall{{
		ID: id, Type: "function", Function: FunctionCall{Name: name, Arguments: args},
	}}})
}

// joinParts 拼接文本片段；图片等非文本片段用 [类型] 占位。
func joinParts(parts []contentPart) string {
	texts := make([]string, 0, len(parts))
	for _, p := range parts {
		switch {
		case p.Text != "":
			texts = append(texts, p.Text)
		case p.Type == "input_image":
			texts = append(texts, "[image]")
		case p.Type != "":
			texts = append(texts, "["+p.Type+"]")
		}
	}
	return strings.Join(texts, "\n")
}

// outputText 处理工具输出的几种形态：字符串、文本片段数组，其余保留 JSON 原文。
func outputText(raw json.RawMessage) string {
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	var parts []contentPart
	if json.Unmarshal(raw, &parts) == nil {
		return joinParts(parts)
	}
	return string(raw)
}

// stripMeta 去掉 Codex 内部字段，保留工具调用本身的参数。
func stripMeta(raw json.RawMessage) string {
	var m map[string]json.RawMessage
	if json.Unmarshal(raw, &m) != nil {
		return string(raw)
	}
	for _, k := range []string{"type", "id", "call_id", "status", "internal_chat_message_metadata_passthrough"} {
		delete(m, k)
	}
	data, err := json.Marshal(m)
	if err != nil {
		return string(raw)
	}
	return string(data)
}
