// Package rollout 增量解析 Codex rollout JSONL（~/.codex/sessions/**/rollout-*.jsonl），
// 按 turn_id 提取补全调用链所需的事实：模型、token、耗时、工具退出码、子 agent 关系。
//
// 子 agent 的 rollout 开头复制了父会话历史（含父轮次的 turn_aborted 等），
// 因此所有事实都按 turn_id 归档，调用方只取自己关心的轮次。
package rollout

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"time"
)

// maxLineBytes 是单行上限；超长行（通常是大段消息内容）直接跳过。
const maxLineBytes = 64 << 20

// Result 是一次扫描的结果。
type Result struct {
	Metas  map[string]SessionMeta // 按 thread id（session_meta.id）
	Turns  map[string]TurnFacts   // 按 turn_id
	Offset int64                  // 最后一个完整行之后的字节偏移，下次从这里继续
}

// SessionMeta 是 session_meta 记录中有用的字段。
type SessionMeta struct {
	ID             string
	SessionID      string
	CLIVersion     string
	CreatorUserID  string
	AgentPath      string // 仅子 agent
	AgentNickname  string // 仅子 agent
	ParentThreadID string // 仅子 agent
}

// TurnFacts 是某个轮次在 rollout 中的全部事实。
type TurnFacts struct {
	TurnID       string
	RootTurnID   string
	ParentTurnID string
	Model        string
	Effort       string
	StartedAt    time.Time
	CompletedAt  time.Time
	DurationMs   int64
	TTFTMs       int64
	Completed    bool
	Aborted      bool
	AbortReason  string
	Generations  []Generation
	Items        map[string]Item // 按 item id（= hook 的 tool_use_id）
	// LastRecordAt 是本轮次最后一条记录的时间，用于跨扫描估算下一次 generation 的开始时间。
	LastRecordAt time.Time
}

// Generation 对应一次模型调用（token_usage_record）。
type Generation struct {
	ResponseID string
	Start      time.Time // 同一轮次内上一条记录的时间（近似）
	End        time.Time
	Usage      Usage
}

// Usage 是 token 用量，字段与 Codex 一致：Input 包含 CachedInput，Output 包含 ReasoningOutput。
type Usage struct {
	Input           int64 `json:"input_tokens"`
	CachedInput     int64 `json:"cached_input_tokens"`
	CacheWriteInput int64 `json:"cache_write_input_tokens"`
	Output          int64 `json:"output_tokens"`
	ReasoningOutput int64 `json:"reasoning_output_tokens"`
	Total           int64 `json:"total_tokens"`
}

// Item 是 item_completed 中与工具相关的条目。
type Item struct {
	ID            string
	Type          string // CommandExecution / FileChange / SubAgentActivity / CollabAgentToolCall
	Status        string
	ExitCode      *int
	StartedAt     time.Time
	CompletedAt   time.Time
	AgentThreadID string // SubAgentActivity
	AgentPath     string // SubAgentActivity
	Kind          string // SubAgentActivity: started / completed
}

// Scan 从 offset 开始读到文件末尾。offset 超过文件大小（文件被替换或截断）时从头扫描。
func Scan(path string, offset int64) (Result, error) {
	f, err := os.Open(path)
	if err != nil {
		return Result{}, fmt.Errorf("rollout: open %s: %w", path, err)
	}
	defer f.Close()

	info, err := f.Stat()
	if err != nil {
		return Result{}, fmt.Errorf("rollout: stat %s: %w", path, err)
	}
	if offset < 0 || offset > info.Size() {
		offset = 0
	}
	if _, err := f.Seek(offset, io.SeekStart); err != nil {
		return Result{}, fmt.Errorf("rollout: seek %s: %w", path, err)
	}

	b := newBuilder()
	r := bufio.NewReaderSize(f, 256<<10)
	pos := offset
	for {
		line, err := readLine(r)
		if errors.Is(err, io.EOF) {
			break // 不完整的末行（写入中）留待下次
		}
		if err != nil {
			return Result{}, fmt.Errorf("rollout: read %s: %w", path, err)
		}
		pos += int64(len(line))
		if len(line) <= maxLineBytes {
			b.add(line)
		}
	}
	return b.result(pos), nil
}

// readLine 读取以 \n 结尾的完整一行；遇到 EOF 时丢弃不完整内容并返回 io.EOF。
func readLine(r *bufio.Reader) ([]byte, error) {
	line, err := r.ReadBytes('\n')
	if err != nil {
		return nil, err
	}
	return line, nil
}

type rawRecord struct {
	Timestamp string          `json:"timestamp"`
	Type      string          `json:"type"`
	Payload   json.RawMessage `json:"payload"`
}

type payloadHead struct {
	Type        string `json:"type"`
	TurnID      string `json:"turn_id"`
	Passthrough struct {
		TurnID string `json:"turn_id"`
	} `json:"internal_chat_message_metadata_passthrough"`
}

type builder struct {
	metas  map[string]SessionMeta
	turns  map[string]*TurnFacts
	lastTs map[string]time.Time // 每个轮次最近一条记录的时间，用于估算 generation 开始时间
}

func newBuilder() *builder {
	return &builder{
		metas:  map[string]SessionMeta{},
		turns:  map[string]*TurnFacts{},
		lastTs: map[string]time.Time{},
	}
}

func (b *builder) turn(id string) *TurnFacts {
	tf, ok := b.turns[id]
	if !ok {
		tf = &TurnFacts{TurnID: id, Items: map[string]Item{}}
		b.turns[id] = tf
	}
	return tf
}

func (b *builder) add(line []byte) {
	var rec rawRecord
	if json.Unmarshal(line, &rec) != nil || len(rec.Payload) == 0 {
		return
	}
	ts, _ := time.Parse(time.RFC3339Nano, rec.Timestamp)

	if rec.Type == "session_meta" {
		b.addMeta(rec.Payload)
		return
	}
	var head payloadHead
	if json.Unmarshal(rec.Payload, &head) != nil {
		return
	}
	turnID := head.TurnID
	if turnID == "" {
		turnID = head.Passthrough.TurnID
	}
	if turnID == "" {
		return
	}

	switch {
	case rec.Type == "turn_context":
		b.addTurnContext(turnID, rec.Payload)
	case rec.Type == "token_usage_record":
		b.addUsage(turnID, ts, rec.Payload)
	case rec.Type == "event_msg" && head.Type == "task_started":
		b.addTaskStarted(turnID, ts, rec.Payload)
	case rec.Type == "event_msg" && head.Type == "task_complete":
		b.addTaskComplete(turnID, ts, rec.Payload)
	case rec.Type == "event_msg" && head.Type == "turn_aborted":
		b.addTurnAborted(turnID, ts, rec.Payload)
	case rec.Type == "event_msg" && head.Type == "item_completed":
		b.addItem(turnID, rec.Payload)
	}
	if !ts.IsZero() {
		b.lastTs[turnID] = ts
	}
}

func (b *builder) addMeta(raw json.RawMessage) {
	var p struct {
		ID            string          `json:"id"`
		SessionID     string          `json:"session_id"`
		CLIVersion    string          `json:"cli_version"`
		CreatorUserID string          `json:"creator_user_id"`
		Source        json.RawMessage `json:"source"`
	}
	if json.Unmarshal(raw, &p) != nil || p.ID == "" {
		return
	}
	meta := SessionMeta{ID: p.ID, SessionID: p.SessionID, CLIVersion: p.CLIVersion, CreatorUserID: p.CreatorUserID}
	var src struct {
		Subagent struct {
			ThreadSpawn struct {
				ParentThreadID string `json:"parent_thread_id"`
				AgentPath      string `json:"agent_path"`
				AgentNickname  string `json:"agent_nickname"`
			} `json:"thread_spawn"`
		} `json:"subagent"`
	}
	if json.Unmarshal(p.Source, &src) == nil { // source 为字符串时解析失败，忽略即可
		spawn := src.Subagent.ThreadSpawn
		meta.ParentThreadID = spawn.ParentThreadID
		meta.AgentPath = spawn.AgentPath
		meta.AgentNickname = spawn.AgentNickname
	}
	b.metas[p.ID] = meta
}

func (b *builder) addTurnContext(turnID string, raw json.RawMessage) {
	var p struct {
		RootTurnID string `json:"root_turn_id"`
		Model      string `json:"model"`
		Effort     string `json:"effort"`
	}
	if json.Unmarshal(raw, &p) != nil {
		return
	}
	tf := b.turn(turnID)
	tf.Model, tf.Effort = p.Model, p.Effort
	if tf.RootTurnID == "" {
		tf.RootTurnID = p.RootTurnID
	}
}

func (b *builder) addTaskStarted(turnID string, ts time.Time, raw json.RawMessage) {
	var p struct {
		RootTurnID  string `json:"root_turn_id"`
		Attribution struct {
			RootTurnID   string `json:"root_turn_id"`
			ParentTurnID string `json:"parent_turn_id"`
		} `json:"turn_attribution"`
	}
	if json.Unmarshal(raw, &p) != nil {
		return
	}
	tf := b.turn(turnID)
	tf.StartedAt = ts
	tf.RootTurnID = firstNonEmpty(p.Attribution.RootTurnID, p.RootTurnID, tf.RootTurnID)
	tf.ParentTurnID = p.Attribution.ParentTurnID
}

func (b *builder) addTaskComplete(turnID string, ts time.Time, raw json.RawMessage) {
	var p struct {
		DurationMs int64 `json:"duration_ms"`
		TTFTMs     int64 `json:"time_to_first_token_ms"`
	}
	if json.Unmarshal(raw, &p) != nil {
		return
	}
	tf := b.turn(turnID)
	tf.Completed = true
	tf.CompletedAt = ts
	tf.DurationMs, tf.TTFTMs = p.DurationMs, p.TTFTMs
}

func (b *builder) addTurnAborted(turnID string, ts time.Time, raw json.RawMessage) {
	var p struct {
		Reason     string `json:"reason"`
		DurationMs int64  `json:"duration_ms"`
	}
	if json.Unmarshal(raw, &p) != nil {
		return
	}
	tf := b.turn(turnID)
	tf.Aborted = true
	tf.AbortReason = p.Reason
	tf.CompletedAt = ts
	tf.DurationMs = p.DurationMs
}

func (b *builder) addUsage(turnID string, ts time.Time, raw json.RawMessage) {
	var p struct {
		ResponseID string `json:"response_id"`
		RootTurnID string `json:"root_turn_id"`
		Usage      Usage  `json:"usage"`
	}
	if json.Unmarshal(raw, &p) != nil || p.ResponseID == "" {
		return
	}
	tf := b.turn(turnID)
	if tf.RootTurnID == "" {
		tf.RootTurnID = p.RootTurnID
	}
	start := b.lastTs[turnID]
	if start.IsZero() || start.After(ts) {
		start = ts
	}
	tf.Generations = append(tf.Generations, Generation{ResponseID: p.ResponseID, Start: start, End: ts, Usage: p.Usage})
}

func (b *builder) addItem(turnID string, raw json.RawMessage) {
	var p struct {
		StartedAtMs   int64 `json:"started_at_ms"`
		CompletedAtMs int64 `json:"completed_at_ms"`
		Item          struct {
			ID            string `json:"id"`
			Type          string `json:"type"`
			Status        string `json:"status"`
			ExitCode      *int   `json:"exit_code"`
			AgentThreadID string `json:"agent_thread_id"`
			AgentPath     string `json:"agent_path"`
			Kind          string `json:"kind"`
		} `json:"item"`
	}
	if json.Unmarshal(raw, &p) != nil || p.Item.ID == "" {
		return
	}
	switch p.Item.Type {
	case "CommandExecution", "FileChange", "SubAgentActivity", "CollabAgentToolCall":
	default:
		return
	}
	it := p.Item
	b.turn(turnID).Items[it.ID] = Item{
		ID:            it.ID,
		Type:          it.Type,
		Status:        it.Status,
		ExitCode:      it.ExitCode,
		StartedAt:     msTime(p.StartedAtMs),
		CompletedAt:   msTime(p.CompletedAtMs),
		AgentThreadID: it.AgentThreadID,
		AgentPath:     it.AgentPath,
		Kind:          it.Kind,
	}
}

func (b *builder) result(offset int64) Result {
	turns := make(map[string]TurnFacts, len(b.turns))
	for id, tf := range b.turns {
		tf.LastRecordAt = b.lastTs[id]
		turns[id] = *tf
	}
	return Result{Metas: b.metas, Turns: turns, Offset: offset}
}

func msTime(ms int64) time.Time {
	if ms <= 0 {
		return time.Time{}
	}
	return time.UnixMilli(ms).UTC()
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}
