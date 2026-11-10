// Package pipeline 把 spool 中的 hook 事件关联成轮次，轮次结束后补全 rollout 事实并生成 span。
package pipeline

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/YYYYYHHHHHH/codex-trace/internal/event"
	"github.com/YYYYYHHHHHH/codex-trace/internal/otlp"
	"github.com/YYYYYHHHHHH/codex-trace/internal/rollout"
	"github.com/YYYYYHHHHHH/codex-trace/internal/session"
	"github.com/YYYYYHHHHHH/codex-trace/internal/trace"
)

// 默认时间参数，含义见 Options。
const (
	DefaultGrace      = time.Second
	DefaultMaxWait    = 15 * time.Second
	DefaultStaleAfter = 30 * time.Minute
	DefaultRetention  = 24 * time.Hour
)

var rolloutName = regexp.MustCompile(`^rollout-[^/]*\.jsonl$`)

// Options 配置 Processor。
type Options struct {
	Store  session.Store
	Policy trace.Policy
	// Scan 默认为 rollout.Scan，测试中可替换。
	Scan func(path string, offset int64) (rollout.Result, error)
	// AllowedRoots 是允许读取的 rollout 目录（如 ~/.codex/sessions）；transcript_path 来自 stdin，必须校验。
	AllowedRoots []string
	// UserFallback 是 rollout 中没有 creator_user_id 时用于生成 user.id 的原始值（会被哈希）。
	UserFallback string
	// Grace：轮次结束后至少等待多久再读 rollout，给 Codex 写完收尾记录的时间。
	Grace time.Duration
	// MaxWait：rollout 一直没有 task_complete / turn_aborted 时，最多等多久就按现有信息发出。
	MaxWait time.Duration
	// StaleAfter：未结束的轮次超过这么久没有任何事件，按"未完成"收尾。
	StaleAfter time.Duration
	// Retention：已发出轮次的去重记录与扫描缓存保留多久。
	Retention time.Duration
	Logf      func(format string, args ...any)
}

// Processor 在内存中缓存会话状态，Commit 时落盘。非并发安全：只在单实例上传进程中使用。
//
// 调用约定：Ingest* → [CloseStale] → Flush → 持久化返回的 span → Commit。
// span 持久化失败时必须调用 Discard，丢弃内存中的变更（spool 保留，下次重放）。
type Processor struct {
	opts     Options
	sessions map[string]*session.State
	dirty    map[string]bool
	deleted  map[string]bool
}

func New(opts Options) *Processor {
	if opts.Scan == nil {
		opts.Scan = rollout.Scan
	}
	if opts.Grace == 0 {
		opts.Grace = DefaultGrace
	}
	if opts.MaxWait == 0 {
		opts.MaxWait = DefaultMaxWait
	}
	if opts.StaleAfter == 0 {
		opts.StaleAfter = DefaultStaleAfter
	}
	if opts.Retention == 0 {
		opts.Retention = DefaultRetention
	}
	if opts.Logf == nil {
		opts.Logf = func(string, ...any) {}
	}
	return &Processor{
		opts:     opts,
		sessions: map[string]*session.State{},
		dirty:    map[string]bool{},
		deleted:  map[string]bool{},
	}
}

// Ingest 应用一条 spool 记录。返回错误表示记录无效，调用方应丢弃它。
func (p *Processor) Ingest(rec event.Record) error {
	pl, err := rec.Decode()
	if err != nil {
		return fmt.Errorf("pipeline: decode record: %w", err)
	}
	st, err := p.load(pl.SessionID)
	if err != nil {
		return err
	}
	st.Apply(rec, pl)
	p.dirty[pl.SessionID] = true
	return nil
}

// LoadAll 载入所有持久化的会话，用于启动时检查过期轮次。损坏的状态文件会被丢弃并记录日志。
func (p *Processor) LoadAll() error {
	ids, err := p.opts.Store.List()
	if err != nil {
		return err
	}
	for _, id := range ids {
		if _, err := p.load(id); err != nil {
			p.opts.Logf("丢弃无法读取的会话状态 %s: %v", id, err)
			_ = p.opts.Store.Delete(id)
		}
	}
	return nil
}

func (p *Processor) load(id string) (*session.State, error) {
	if st, ok := p.sessions[id]; ok {
		return st, nil
	}
	st, err := p.opts.Store.Load(id)
	if err != nil {
		return nil, fmt.Errorf("pipeline: load session: %w", err)
	}
	p.sessions[id] = st
	return st, nil
}

// Discard 丢弃未提交的内存状态，下次访问时从存储重新载入。
func (p *Processor) Discard() {
	p.sessions = map[string]*session.State{}
	p.dirty = map[string]bool{}
	p.deleted = map[string]bool{}
}

// CloseStale 把长时间没有活动的未结束轮次标记为"未完成"。
// 只应在 spool 已读空时调用，否则积压中的 Stop 尚未应用，会被误判。
// 活动同时参考 hook 事件与 transcript 文件的修改时间（长命令期间 rollout 仍在写入）。
func (p *Processor) CloseStale(now time.Time) {
	for _, st := range p.sessions {
		for _, t := range st.Turns {
			if t.Closed() {
				continue
			}
			last := t.LastActivity
			if mt := p.transcriptModTime(t.TranscriptPath); mt.After(last) {
				last = mt
			}
			if now.Sub(last) > p.opts.StaleAfter {
				t.Close = session.CloseStale
				t.ClosedAt = last.Add(p.opts.StaleAfter) // 判定为过期的时刻，使其可在本次收尾
				p.dirty[st.SessionID] = true
			}
		}
	}
}

func (p *Processor) transcriptModTime(path string) time.Time {
	resolved, ok := p.resolveTranscript(path)
	if !ok {
		return time.Time{}
	}
	info, err := os.Stat(resolved)
	if err != nil {
		return time.Time{}
	}
	return info.ModTime()
}

// Flush 收尾所有可以发出的轮次并返回 span。
func (p *Processor) Flush(now time.Time) []otlp.Span {
	var spans []otlp.Span
	for _, id := range sortedIDs(p.sessions) {
		spans = append(spans, p.flushSession(p.sessions[id], now)...)
	}
	return spans
}

// Pending 表示有已结束但尚未发出的轮次（上传进程此时不应退出）。
func (p *Processor) Pending() bool {
	for _, st := range p.sessions {
		for _, t := range st.Turns {
			if t.Closed() {
				return true
			}
		}
	}
	return false
}

// Commit 持久化变更过的会话，并从内存中移出已无未发轮次的会话。
func (p *Processor) Commit() error {
	for id := range p.dirty {
		st := p.sessions[id]
		var err error
		if p.deleted[id] {
			err = p.opts.Store.Delete(id)
		} else if st != nil {
			err = p.opts.Store.Save(st)
		}
		if err != nil {
			return fmt.Errorf("pipeline: commit %s: %w", id, err)
		}
	}
	for id, st := range p.sessions {
		if len(st.Turns) == 0 || p.deleted[id] {
			delete(p.sessions, id)
		}
	}
	p.dirty = map[string]bool{}
	p.deleted = map[string]bool{}
	return nil
}

func (p *Processor) flushSession(st *session.State, now time.Time) []otlp.Span {
	cands := p.candidates(st, now)
	if len(cands) > 0 {
		p.scanTranscripts(st, cands, now)
	}

	var spans []otlp.Span
	for _, t := range cands {
		var facts rollout.TurnFacts
		if f, ok := st.Files[t.TranscriptPath]; ok {
			facts = f.Turns[t.ID]
		}
		finished := facts.Completed || facts.Aborted || t.Close == session.CloseStale
		if !finished && now.Sub(t.ClosedAt) < p.opts.MaxWait {
			continue
		}
		c := p.context(st, t, facts)
		if !finished {
			c.Partial = true
			p.opts.Logf("轮次 %s 等待 rollout 超时，按已有信息发出", t.ID)
		}
		spans = append(spans, trace.BuildTurn(*t, facts, c, p.opts.Policy)...)
		st.MarkEmitted(t, now)
		p.dirty[st.SessionID] = true
	}

	if st.Prune(now, p.opts.Retention) {
		p.deleted[st.SessionID] = true
		p.dirty[st.SessionID] = true
	}
	return spans
}

func (p *Processor) candidates(st *session.State, now time.Time) []*session.Turn {
	var out []*session.Turn
	for _, t := range st.Turns {
		if t.Closed() && now.Sub(t.ClosedAt) >= p.opts.Grace {
			out = append(out, t)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Start.Before(out[j].Start) })
	return out
}

func (p *Processor) scanTranscripts(st *session.State, turns []*session.Turn, now time.Time) {
	seen := map[string]bool{}
	for _, t := range turns {
		path := t.TranscriptPath
		if path == "" || seen[path] {
			continue
		}
		seen[path] = true
		resolved, ok := p.resolveTranscript(path)
		if !ok {
			p.opts.Logf("忽略不在允许目录内的 transcript: %s", path)
			continue
		}
		res, err := p.opts.Scan(resolved, st.File(path).Offset)
		if err != nil {
			p.opts.Logf("读取 rollout 失败: %v", err)
			continue
		}
		st.MergeScan(path, res, now)
		p.dirty[st.SessionID] = true
	}
}

// resolveTranscript 校验 transcript_path：绝对路径、rollout-*.jsonl、解析符号链接后位于允许目录内。
func (p *Processor) resolveTranscript(path string) (string, bool) {
	if !filepath.IsAbs(path) || !rolloutName.MatchString(filepath.Base(path)) {
		return "", false
	}
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		return "", false
	}
	for _, root := range p.opts.AllowedRoots {
		rootResolved, err := filepath.EvalSymlinks(root)
		if err != nil {
			continue
		}
		rel, err := filepath.Rel(rootResolved, resolved)
		if err == nil && rel != "." && !strings.HasPrefix(rel, "..") {
			return resolved, true
		}
	}
	return "", false
}

func (p *Processor) context(st *session.State, t *session.Turn, f rollout.TurnFacts) trace.Context {
	c := trace.Context{SessionID: st.SessionID}
	user := p.opts.UserFallback
	if m, ok := st.Meta(st.SessionID); ok {
		c.CodexVersion = m.CLIVersion
		if m.CreatorUserID != "" {
			user = m.CreatorUserID
		}
	}
	c.UserID = trace.HashID("user", user)

	if !t.IsSubagent() {
		c.TraceKey = firstNonEmpty(f.RootTurnID, t.ID)
		return c
	}
	var spawn session.SpawnRef
	var hasSpawn bool
	if m, ok := st.Meta(t.AgentID); ok {
		c.AgentPath, c.AgentNick = m.AgentPath, m.AgentNickname
		if c.CodexVersion == "" {
			c.CodexVersion = m.CLIVersion
		}
		spawn, hasSpawn = st.SpawnFor(m.AgentPath, t.Start)
	}
	// 根轮次：rollout 记录 > spawn 所在 trace > SubagentStart 时正在进行的主轮次 > 自身
	c.TraceKey = firstNonEmpty(f.RootTurnID, spawn.RootTurnID, t.FallbackRootTurnID, t.ID)
	switch {
	case hasSpawn:
		c.Parent = trace.ToolSpanID(spawn.ToolUseID)
	case c.TraceKey != t.ID:
		c.Parent = trace.TurnSpanID(c.TraceKey)
	}
	return c
}

func sortedIDs(m map[string]*session.State) []string {
	ids := make([]string, 0, len(m))
	for id := range m {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}
