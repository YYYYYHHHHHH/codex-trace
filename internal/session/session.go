// Package session 维护单个 Codex 会话的关联状态：轮次、工具调用、rollout 扫描进度。
//
// 状态只由上传进程（单实例）读写，按 session_id 持久化为一个 JSON 文件。
package session

import (
	"encoding/json"
	"strings"
	"time"

	"github.com/YYYYYHHHHHH/codex-trace/internal/event"
	"github.com/YYYYYHHHHHH/codex-trace/internal/rollout"
)

// CloseReason 表示轮次如何结束。
type CloseReason string

const (
	CloseNone         CloseReason = ""
	CloseStop         CloseReason = "stop"
	CloseInterrupt    CloseReason = "interrupt"
	CloseSubagentStop CloseReason = "subagent_stop"
	CloseStale        CloseReason = "stale"
)

// Tool 是一次工具调用（PreToolUse / PostToolUse）。
type Tool struct {
	ID            string          `json:"id"`
	Name          string          `json:"name"`
	Input         json.RawMessage `json:"input,omitempty"`
	InputBytes    int             `json:"input_bytes"`
	Response      string          `json:"response,omitempty"`
	ResponseBytes int             `json:"response_bytes"`
	Done          bool            `json:"done"`
	Start         time.Time       `json:"start"`
	End           time.Time       `json:"end"`
	SpawnTask     string          `json:"spawn_task,omitempty"` // spawn_agent 返回的 task_name
}

// Turn 是一个轮次：主会话的一轮对话，或子 agent 的一次执行。
type Turn struct {
	ID                 string      `json:"id"`
	AgentID            string      `json:"agent_id,omitempty"`
	AgentType          string      `json:"agent_type,omitempty"`
	TranscriptPath     string      `json:"transcript_path"`
	Cwd                string      `json:"cwd"`
	Model              string      `json:"model"`
	PermissionMode     string      `json:"permission_mode"`
	Prompt             string      `json:"prompt,omitempty"`
	PromptBytes        int         `json:"prompt_bytes"`
	Output             string      `json:"output,omitempty"`
	OutputBytes        int         `json:"output_bytes"`
	Start              time.Time   `json:"start"`
	LastActivity       time.Time   `json:"last_activity"`
	ClosedAt           time.Time   `json:"closed_at"`
	Close              CloseReason `json:"close"`
	FallbackRootTurnID string      `json:"fallback_root_turn_id,omitempty"`
	PermissionRequests int         `json:"permission_requests"`
	Tools              []Tool      `json:"tools"`
}

// IsSubagent 表示该轮次属于子 agent。
func (t *Turn) IsSubagent() bool { return t.AgentID != "" }

// Closed 表示已收到结束事件（或被判定为过期）。
func (t *Turn) Closed() bool { return t.Close != CloseNone }

// File 是某个 rollout 文件的增量扫描进度与已解析事实。
type File struct {
	Offset  int64                          `json:"offset"`
	Turns   map[string]rollout.TurnFacts   `json:"turns"`
	Metas   map[string]rollout.SessionMeta `json:"metas"`
	Updated time.Time                      `json:"updated"`
}

// SpawnRef 记录一次 spawn_agent 调用，用于把子 agent 挂到它下面。
// 独立于轮次保存：父轮次可能先于子 agent 发出并被释放。
type SpawnRef struct {
	ToolUseID  string    `json:"tool_use_id"`
	RootTurnID string    `json:"root_turn_id"` // 发起 spawn 的轮次所在 trace 的根轮次
	At         time.Time `json:"at"`
}

// State 是一个会话的全部关联状态。
type State struct {
	SessionID    string                `json:"session_id"`
	Turns        map[string]*Turn      `json:"turns"`
	Emitted      map[string]time.Time  `json:"emitted"`
	Files        map[string]*File      `json:"files"`
	Spawns       map[string][]SpawnRef `json:"spawns"` // task_name → spawn 调用（同名可能多次）
	LastActivity time.Time             `json:"last_activity"`
}

// New 创建空状态。
func New(sessionID string) *State {
	s := &State{SessionID: sessionID}
	s.ensureMaps()
	return s
}

func (s *State) ensureMaps() {
	if s.Turns == nil {
		s.Turns = map[string]*Turn{}
	}
	if s.Emitted == nil {
		s.Emitted = map[string]time.Time{}
	}
	if s.Files == nil {
		s.Files = map[string]*File{}
	}
	if s.Spawns == nil {
		s.Spawns = map[string][]SpawnRef{}
	}
}

// Apply 把一条 hook 事件合并进状态。已发出的轮次的迟到 / 重复事件会被忽略。
func (s *State) Apply(rec event.Record, p event.Payload) {
	at := rec.ReceivedAt
	if at.After(s.LastActivity) {
		s.LastActivity = at
	}
	if p.TurnID == "" {
		return // SessionStart：没有轮次信息
	}
	if _, done := s.Emitted[p.TurnID]; done {
		return
	}
	t := s.turnFor(p, at)
	if at.After(t.LastActivity) {
		t.LastActivity = at
	}

	switch p.HookEventName {
	case event.UserPromptSubmit:
		t.Prompt = p.Prompt
		t.PromptBytes = contentSize(p, "prompt", len(p.Prompt))
		if at.Before(t.Start) {
			t.Start = at
		}
	case event.PreToolUse:
		if tool := t.tool(p.ToolUseID, at); tool != nil {
			tool.Name = firstNonEmpty(tool.Name, p.ToolName)
			tool.Input = p.ToolInput
			tool.InputBytes = contentSize(p, "tool_input", len(p.ToolInput))
		}
	case event.PostToolUse:
		if tool := t.tool(p.ToolUseID, at); tool != nil {
			tool.Name = firstNonEmpty(tool.Name, p.ToolName)
			if tool.Input == nil {
				tool.Input = p.ToolInput
				tool.InputBytes = contentSize(p, "tool_input", len(p.ToolInput))
			}
			tool.Response = p.ToolResponseText()
			tool.ResponseBytes = contentSize(p, "tool_response", len(tool.Response))
			tool.Done = true
			tool.End = at
			if strings.HasSuffix(tool.Name, "spawn_agent") {
				tool.SpawnTask = spawnTaskName(tool.Response)
				if tool.SpawnTask != "" {
					s.addSpawn(tool.SpawnTask, SpawnRef{ToolUseID: tool.ID, RootTurnID: t.rootID(), At: at})
				}
			}
		}
	case event.PermissionRequest:
		t.PermissionRequests++
	case event.SubagentStop:
		t.close(CloseSubagentStop, at, p)
	case event.Stop:
		t.close(CloseStop, at, p)
	case event.Interrupt:
		t.close(CloseInterrupt, at, p)
	}
}

func (s *State) turnFor(p event.Payload, at time.Time) *Turn {
	transcript := p.TranscriptPath
	if p.HookEventName == event.SubagentStop && p.AgentTranscriptPath != "" {
		transcript = p.AgentTranscriptPath // SubagentStop 的 transcript_path 指向父会话
	}
	t, ok := s.Turns[p.TurnID]
	if !ok {
		t = &Turn{ID: p.TurnID, AgentID: p.AgentID, AgentType: p.AgentType, Start: at, LastActivity: at}
		if p.AgentID != "" {
			if main := s.OpenMainTurn(); main != nil {
				t.FallbackRootTurnID = main.ID
			}
		}
		s.Turns[p.TurnID] = t
	}
	t.TranscriptPath = firstNonEmpty(t.TranscriptPath, transcript)
	t.Cwd = firstNonEmpty(t.Cwd, p.Cwd)
	t.Model = firstNonEmpty(t.Model, p.Model)
	t.PermissionMode = firstNonEmpty(t.PermissionMode, p.PermissionMode)
	t.AgentID = firstNonEmpty(t.AgentID, p.AgentID)
	t.AgentType = firstNonEmpty(t.AgentType, p.AgentType)
	return t
}

// OpenMainTurn 返回最近开始、尚未结束的主会话轮次。
func (s *State) OpenMainTurn() *Turn {
	var best *Turn
	for _, t := range s.Turns {
		if t.IsSubagent() || t.Closed() {
			continue
		}
		if best == nil || t.Start.After(best.Start) {
			best = t
		}
	}
	return best
}

// SpawnFor 查找某个子 agent 对应的 spawn 调用：task_name 相同、且在子 agent 开始之前最近的一次。
func (s *State) SpawnFor(task string, agentStart time.Time) (SpawnRef, bool) {
	var best SpawnRef
	found := false
	for _, ref := range s.Spawns[task] {
		if ref.At.After(agentStart) {
			continue
		}
		if !found || ref.At.After(best.At) {
			best, found = ref, true
		}
	}
	return best, found
}

func (s *State) addSpawn(task string, ref SpawnRef) {
	for _, r := range s.Spawns[task] {
		if r.ToolUseID == ref.ToolUseID {
			return // 重放
		}
	}
	s.Spawns[task] = append(s.Spawns[task], ref)
}

// rootID 是该轮次所在 trace 的根轮次（尽力而为，rollout 事实优先）。
func (t *Turn) rootID() string {
	if t.IsSubagent() {
		return t.FallbackRootTurnID
	}
	return t.ID
}

// File 返回某个 rollout 文件的扫描状态（不存在则创建）。
func (s *State) File(path string) *File {
	f, ok := s.Files[path]
	if !ok {
		f = &File{Turns: map[string]rollout.TurnFacts{}, Metas: map[string]rollout.SessionMeta{}}
		s.Files[path] = f
	}
	return f
}

// MergeScan 合并一次增量扫描的结果并推进偏移。
func (s *State) MergeScan(path string, res rollout.Result, now time.Time) {
	f := s.File(path)
	for id, tf := range res.Turns {
		if _, done := s.Emitted[id]; done {
			continue // 已发出轮次的迟到记录
		}
		f.Turns[id] = rollout.Merge(f.Turns[id], tf)
	}
	for id, m := range res.Metas {
		f.Metas[id] = m
	}
	f.Offset = res.Offset
	f.Updated = now
}

// Meta 在所有已扫描文件中查找某个 thread 的 session_meta。
func (s *State) Meta(threadID string) (rollout.SessionMeta, bool) {
	for _, f := range s.Files {
		if m, ok := f.Metas[threadID]; ok {
			return m, true
		}
	}
	return rollout.SessionMeta{}, false
}

// MarkEmitted 记录轮次已发出，并释放其状态。
func (s *State) MarkEmitted(t *Turn, now time.Time) {
	s.Emitted[t.ID] = now
	if f, ok := s.Files[t.TranscriptPath]; ok {
		delete(f.Turns, t.ID)
	}
	delete(s.Turns, t.ID)
}

// Prune 清理超过 retention 的已发出记录与扫描缓存。返回 true 表示会话已无任何状态，可删除。
func (s *State) Prune(now time.Time, retention time.Duration) bool {
	for id, at := range s.Emitted {
		if now.Sub(at) > retention {
			delete(s.Emitted, id)
		}
	}
	for path, f := range s.Files {
		if now.Sub(f.Updated) > retention && !s.fileInUse(path) {
			delete(s.Files, path)
		}
	}
	for task, refs := range s.Spawns {
		kept := refs[:0]
		for _, ref := range refs {
			if now.Sub(ref.At) <= retention {
				kept = append(kept, ref)
			}
		}
		if len(kept) == 0 {
			delete(s.Spawns, task)
		} else {
			s.Spawns[task] = kept
		}
	}
	s.pruneOrphanFacts(now, orphanFactsMaxAge)
	return len(s.Turns) == 0 && len(s.Emitted) == 0 && now.Sub(s.LastActivity) > retention
}

// orphanFactsMaxAge：已结束、但没有对应 hook 轮次的 rollout 事实保留多久
// （来自安装前的历史，或子 agent rollout 中复制的父历史）。
const orphanFactsMaxAge = 10 * time.Minute

func (s *State) pruneOrphanFacts(now time.Time, maxAge time.Duration) {
	for _, f := range s.Files {
		for id, tf := range f.Turns {
			if _, live := s.Turns[id]; live {
				continue
			}
			if (tf.Completed || tf.Aborted) && !tf.CompletedAt.IsZero() && now.Sub(tf.CompletedAt) > maxAge {
				delete(f.Turns, id)
			}
		}
	}
}

func (s *State) fileInUse(path string) bool {
	for _, t := range s.Turns {
		if t.TranscriptPath == path {
			return true
		}
	}
	return false
}

func (t *Turn) close(reason CloseReason, at time.Time, p event.Payload) {
	if t.Closed() {
		return
	}
	t.Close = reason
	t.ClosedAt = at
	t.Output = p.LastAssistantMessage
	t.OutputBytes = contentSize(p, "last_assistant_message", len(p.LastAssistantMessage))
}

func (t *Turn) tool(id string, at time.Time) *Tool {
	if id == "" {
		return nil
	}
	for i := range t.Tools {
		if t.Tools[i].ID == id {
			if at.Before(t.Tools[i].Start) {
				t.Tools[i].Start = at
			}
			return &t.Tools[i]
		}
	}
	t.Tools = append(t.Tools, Tool{ID: id, Start: at})
	return &t.Tools[len(t.Tools)-1]
}

// contentSize 优先使用 hook 剥离内容时记录的原始长度。
func contentSize(p event.Payload, field string, fallback int) int {
	if n, ok := p.ContentSizes[field]; ok {
		return n
	}
	return fallback
}

func spawnTaskName(response string) string {
	var r struct {
		TaskName string `json:"task_name"`
	}
	_ = json.Unmarshal([]byte(response), &r)
	return r.TaskName
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}
