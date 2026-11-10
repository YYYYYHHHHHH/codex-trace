// Package trace 把一个已结束的轮次（hook 状态 + rollout 事实）转换成 OTel span。
//
// 结构（见 docs/PLAN.md 第 4 节）：
//
//	轮次 span（agent，trace 根；子 agent 挂在 spawn 工具 span 下）
//	├── 工具 span（tool）
//	└── 模型调用 span（generation）
package trace

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"path/filepath"
	"time"

	"github.com/YYYYYHHHHHH/codex-trace/internal/otlp"
	"github.com/YYYYYHHHHHH/codex-trace/internal/redact"
	"github.com/YYYYYHHHHHH/codex-trace/internal/rollout"
	"github.com/YYYYYHHHHHH/codex-trace/internal/session"
)

// DefaultMaxFieldBytes 是开启内容采集时单个字段的上限。
const DefaultMaxFieldBytes = 10 << 10

// Policy 控制上传哪些内容。
type Policy struct {
	CaptureContent bool
	Environment    string
	Release        string
	MaxFieldBytes  int
}

// Context 是构造轮次 span 所需的、来自轮次之外的信息。
type Context struct {
	SessionID    string
	UserID       string // 已哈希
	CodexVersion string
	TraceKey     string      // root turn id，决定 trace id
	Parent       otlp.SpanID // 子 agent 的父 span；主轮次为零值
	AgentPath    string
	AgentNick    string
	Partial      bool // 等待 rollout 超时，事实可能不完整
}

// HashID 把用户 / 机器标识单向哈希后再上传。
func HashID(kind, value string) string {
	if value == "" {
		return ""
	}
	sum := sha256.Sum256([]byte("codex-trace:" + kind + ":" + value))
	return hex.EncodeToString(sum[:8])
}

// TurnSpanID 返回轮次 span 的 id，供子 agent 计算父 span。
func TurnSpanID(turnID string) otlp.SpanID { return otlp.NewSpanID(KindTurn, turnID) }

// ToolSpanID 返回工具 span 的 id。
func ToolSpanID(toolUseID string) otlp.SpanID { return otlp.NewSpanID(KindTool, toolUseID) }

// BuildTurn 生成轮次及其下所有工具、模型调用的 span。
func BuildTurn(t session.Turn, f rollout.TurnFacts, c Context, p Policy) []otlp.Span {
	if p.MaxFieldBytes <= 0 {
		p.MaxFieldBytes = DefaultMaxFieldBytes
	}
	turnSpan := buildTurnSpan(t, f, c, p)

	spans := make([]otlp.Span, 0, 1+len(t.Tools)+len(f.Generations))
	spans = append(spans, turnSpan)
	for _, tool := range t.Tools {
		spans = append(spans, buildToolSpan(tool, f, turnSpan, p))
	}
	model := firstNonEmpty(f.Model, t.Model)
	for _, g := range f.Generations {
		spans = append(spans, buildGenerationSpan(g, model, f.Effort, turnSpan))
	}
	return spans
}

func buildTurnSpan(t session.Turn, f rollout.TurnFacts, c Context, p Policy) otlp.Span {
	start := earliest(t.Start, f.StartedAt)
	end := latest(f.CompletedAt, t.ClosedAt)
	if end.IsZero() {
		end = t.LastActivity
	}
	end = notBefore(end, start)

	name := "codex turn"
	if t.IsSubagent() {
		name = "subagent:" + firstNonEmpty(t.AgentType, "agent")
	}
	model := firstNonEmpty(f.Model, t.Model)

	attrs := []otlp.Attr{
		otlp.String(attrObsType, obsAgent),
		otlp.String(attrSessionID, c.SessionID),
	}
	attrs = appendNonEmpty(attrs, attrUserID, c.UserID)
	attrs = appendNonEmpty(attrs, attrEnvironment, p.Environment)
	attrs = appendNonEmpty(attrs, attrRelease, p.Release)

	tags := []string{"codex"}
	if t.IsSubagent() {
		tags = append(tags, "subagent")
	}
	if c.Partial {
		tags = append(tags, "partial")
	}
	switch {
	case t.Close == session.CloseInterrupt || f.Aborted:
		tags = append(tags, "interrupted")
		attrs = append(attrs, otlp.String(attrObsLevel, levelWarning), otlp.String(attrObsStatusMsg, "interrupted"))
	case t.Close == session.CloseStale:
		tags = append(tags, "incomplete")
		attrs = append(attrs, otlp.String(attrObsLevel, levelWarning), otlp.String(attrObsStatusMsg, "turn did not finish"))
	}

	if !t.IsSubagent() {
		attrs = append(attrs, otlp.String(attrTraceName, name), otlp.Strings(attrTraceTags, tags))
		attrs = append(attrs, metaAttrs(attrTraceMetaPref, map[string]any{
			"model":         model,
			"codex_version": c.CodexVersion,
		})...)
	}
	attrs = append(attrs, metaAttrs(attrObsMetaPref, turnMeta(t, f, c, p, model))...)

	if p.CaptureContent {
		in, out := content(t.Prompt, p), content(t.Output, p)
		attrs = append(attrs, otlp.String(attrObsInput, in), otlp.String(attrObsOutput, out))
		if !t.IsSubagent() {
			attrs = append(attrs, otlp.String(attrTraceInput, in), otlp.String(attrTraceOutput, out))
		}
	}

	return otlp.Span{
		TraceID: otlp.NewTraceID(c.TraceKey),
		SpanID:  TurnSpanID(t.ID),
		Parent:  c.Parent,
		Name:    name,
		Start:   start,
		End:     end,
		Attrs:   attrs,
	}
}

func turnMeta(t session.Turn, f rollout.TurnFacts, c Context, p Policy, model string) map[string]any {
	meta := map[string]any{
		"turn_id":         t.ID,
		"model":           model,
		"reasoning":       f.Effort,
		"codex_version":   c.CodexVersion,
		"cwd_name":        filepath.Base(t.Cwd),
		"permission_mode": t.PermissionMode,
		"close_reason":    string(t.Close),
		"duration_ms":     f.DurationMs,
		"ttft_ms":         f.TTFTMs,
		"tool_calls":      len(t.Tools),
		"model_calls":     len(f.Generations),
	}
	if t.PermissionRequests > 0 {
		meta["permission_requests"] = t.PermissionRequests
	}
	if t.IsSubagent() {
		meta["agent_id"] = t.AgentID
		meta["agent_type"] = t.AgentType
		meta["agent_path"] = c.AgentPath
		meta["agent_nickname"] = c.AgentNick
	}
	if !p.CaptureContent {
		meta["prompt_bytes"] = t.PromptBytes
		meta["output_bytes"] = t.OutputBytes
	}
	return meta
}

func buildToolSpan(tool session.Tool, f rollout.TurnFacts, parent otlp.Span, p Policy) otlp.Span {
	item, hasItem := f.Items[tool.ID]
	start, end := tool.Start, tool.End
	if hasItem && !item.StartedAt.IsZero() {
		start = item.StartedAt
	}
	if hasItem && !item.CompletedAt.IsZero() {
		end = item.CompletedAt
	}
	if start.IsZero() {
		start = parent.Start
	}
	if end.IsZero() {
		end = parent.End
	}
	end = notBefore(end, start)

	attrs := []otlp.Attr{otlp.String(attrObsType, obsTool)}
	meta := map[string]any{"tool_use_id": tool.ID}
	var status otlp.Status

	switch {
	case hasItem && item.ExitCode != nil && *item.ExitCode != 0:
		msg := fmt.Sprintf("exit code %d", *item.ExitCode)
		status = otlp.Status{Error: true, Message: msg}
		attrs = append(attrs, otlp.String(attrObsLevel, levelError), otlp.String(attrObsStatusMsg, msg))
	case hasItem && item.Status == "failed":
		status = otlp.Status{Error: true, Message: "failed"}
		attrs = append(attrs, otlp.String(attrObsLevel, levelError), otlp.String(attrObsStatusMsg, "failed"))
	case !tool.Done:
		attrs = append(attrs, otlp.String(attrObsLevel, levelWarning), otlp.String(attrObsStatusMsg, "no PostToolUse received"))
	}
	if hasItem && item.ExitCode != nil {
		meta["exit_code"] = *item.ExitCode
	}
	if hasItem {
		meta["status"] = item.Status
	}
	meta["spawn_task"] = tool.SpawnTask

	if p.CaptureContent {
		attrs = append(attrs,
			otlp.String(attrObsInput, content(string(tool.Input), p)),
			otlp.String(attrObsOutput, content(tool.Response, p)))
	} else {
		meta["input_bytes"] = tool.InputBytes
		meta["output_bytes"] = tool.ResponseBytes
	}
	attrs = append(attrs, metaAttrs(attrObsMetaPref, meta)...)

	return otlp.Span{
		TraceID: parent.TraceID,
		SpanID:  ToolSpanID(tool.ID),
		Parent:  parent.SpanID,
		Name:    firstNonEmpty(tool.Name, "tool"),
		Start:   start,
		End:     end,
		Attrs:   attrs,
		Status:  status,
	}
}

func buildGenerationSpan(g rollout.Generation, model, effort string, parent otlp.Span) otlp.Span {
	u := g.Usage
	// Codex 的 input_tokens 含缓存命中部分；Langfuse 的 input 按非缓存部分计费。
	usage, _ := json.Marshal(map[string]int64{
		"input":                    max(u.Input-u.CachedInput, 0),
		"input_cached_tokens":      u.CachedInput,
		"input_cache_write_tokens": u.CacheWriteInput,
		"output":                   u.Output,
		"output_reasoning_tokens":  u.ReasoningOutput,
		"total":                    u.Total,
	})
	attrs := []otlp.Attr{
		otlp.String(attrObsType, obsGeneration),
		otlp.String(attrObsUsage, string(usage)),
		otlp.Int(attrGenAIInputTokens, u.Input),
		otlp.Int(attrGenAIOutputTokens, u.Output),
		otlp.String(attrObsMetaPref+"response_id", g.ResponseID),
	}
	if model != "" {
		attrs = append(attrs, otlp.String(attrObsModel, model), otlp.String(attrGenAIModel, model))
	}
	if effort != "" {
		params, _ := json.Marshal(map[string]string{"reasoning_effort": effort})
		attrs = append(attrs, otlp.String(attrObsModelParam, string(params)))
	}
	start, end := g.Start, g.End
	if end.IsZero() { // rollout 时间戳无法解析时退回到父 span 的时间范围
		start, end = parent.Start, parent.End
	}
	return otlp.Span{
		TraceID: parent.TraceID,
		SpanID:  otlp.NewSpanID(KindGeneration, g.ResponseID),
		Parent:  parent.SpanID,
		Name:    firstNonEmpty(model, "model"),
		Start:   start,
		End:     notBefore(end, start),
		Attrs:   attrs,
	}
}

func content(s string, p Policy) string {
	return redact.Truncate(redact.Text(s), p.MaxFieldBytes)
}

func appendNonEmpty(attrs []otlp.Attr, key, value string) []otlp.Attr {
	if value == "" {
		return attrs
	}
	return append(attrs, otlp.String(key, value))
}

// metaAttrs 把元数据展开为 <prefix><key> 属性；按 key 排序保证输出稳定，跳过空字符串与零值 int64。
func metaAttrs(prefix string, meta map[string]any) []otlp.Attr {
	keys := sortedKeys(meta)
	out := make([]otlp.Attr, 0, len(keys))
	for _, k := range keys {
		switch v := meta[k].(type) {
		case string:
			if v != "" && v != "." {
				out = append(out, otlp.String(prefix+k, v))
			}
		case int:
			out = append(out, otlp.Int(prefix+k, int64(v)))
		case int64:
			if v != 0 {
				out = append(out, otlp.Int(prefix+k, v))
			}
		}
	}
	return out
}

func earliest(a, b time.Time) time.Time {
	switch {
	case a.IsZero():
		return b
	case b.IsZero() || a.Before(b):
		return a
	default:
		return b
	}
}

func latest(a, b time.Time) time.Time {
	if a.After(b) {
		return a
	}
	return b
}

func notBefore(t, min time.Time) time.Time {
	if t.Before(min) {
		return min
	}
	return t
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}
