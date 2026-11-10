// Package uploader 是后台上传进程：消费 spool → 关联成 span → 写 outbox → 发送到 Langfuse。
//
// 持久化顺序保证崩溃后可安全重放：先写 outbox，再提交会话状态，最后删除 spool 记录。
// 已发出的轮次在会话状态中有去重记录，重放的 spool 记录不会再次生成 span。
//
// 退出顺序堵住"丢失唤醒"：先删除心跳文件，再做最后一次 spool 检查，最后释放锁。
// 期间写入 spool 的 hook 看不到心跳，会拉起新的上传进程；新进程用 TryLockWait 等待接手。
package uploader

import (
	"context"
	"fmt"
	"math/rand/v2"
	"os"
	"time"

	"github.com/YYYYYHHHHHH/codex-trace/internal/event"
	"github.com/YYYYYHHHHHH/codex-trace/internal/exporter"
	"github.com/YYYYYHHHHHH/codex-trace/internal/fsutil"
	"github.com/YYYYYHHHHHH/codex-trace/internal/lock"
	"github.com/YYYYYHHHHHH/codex-trace/internal/otlp"
	"github.com/YYYYYHHHHHH/codex-trace/internal/paths"
	"github.com/YYYYYHHHHHH/codex-trace/internal/pipeline"
	"github.com/YYYYYHHHHHH/codex-trace/internal/queue"
	"github.com/YYYYYHHHHHH/codex-trace/internal/status"
)

const (
	DefaultTick      = time.Second
	DefaultIdleExit  = 60 * time.Second
	DefaultLockWait  = 2 * time.Second
	DefaultRunFor    = 2 * time.Minute // Once 模式的上限
	SpoolBatch       = 500
	MaxSpansPerBatch = 100
	OutboxMaxBytes   = 16 << 20
	FailedMaxBytes   = 4 << 20
	minBackoff       = time.Second
	maxBackoff       = 5 * time.Minute
	sendTimeout      = 30 * time.Second
	lockPoll         = 50 * time.Millisecond
	scopeName        = "codex-trace"
)

// Sender 发送一个 OTLP 请求体。
type Sender interface {
	Send(ctx context.Context, body []byte) exporter.Result
}

// Deps 是 Run 的依赖。零值字段使用默认值。
type Deps struct {
	Paths     paths.Paths
	Processor *pipeline.Processor
	Sender    Sender
	Resource  []otlp.Attr
	Version   string
	Now       func() time.Time
	// Sleep 等待 d；ctx 结束时提前返回 false。
	Sleep    func(ctx context.Context, d time.Duration) bool
	Tick     time.Duration
	IdleExit time.Duration
	LockWait time.Duration
	// Once 为 true 时处理完现有数据（含等待中的轮次）即退出，最长 RunFor。
	Once   bool
	RunFor time.Duration
	Logf   func(format string, args ...any)
}

type runner struct {
	d         Deps
	spool     queue.Queue
	outbox    queue.Queue
	failed    queue.Queue
	st        status.Status
	backoff   time.Duration
	nextRetry time.Time
	idleSince time.Time
}

// Run 持有单实例锁运行上传循环。已有实例在运行时立即返回 nil。
func Run(ctx context.Context, d Deps) error {
	d = withDefaults(d)
	if err := fsutil.EnsurePrivateDir(d.Paths.StateDir); err != nil {
		return fmt.Errorf("uploader: state dir: %w", err)
	}
	release, ok, err := lock.TryLockWait(d.Paths.LockFile(), d.LockWait, lockPoll)
	if err != nil {
		return fmt.Errorf("uploader: lock: %w", err)
	}
	if !ok {
		return nil
	}
	defer release()

	r := &runner{
		d:      d,
		spool:  queue.New(d.Paths.SpoolDir()),
		outbox: queue.New(d.Paths.OutboxDir()),
		failed: queue.New(d.Paths.FailedDir()),
		st:     status.Status{PID: os.Getpid(), Version: d.Version, StartedAt: d.Now().UTC()},
	}
	if prev, ok, _ := status.Read(d.Paths.StatusFile()); ok {
		r.st.SentSpans, r.st.SentBatches = prev.SentSpans, prev.SentBatches
		r.st.FailedBatches, r.st.DroppedEvents = prev.FailedBatches, prev.DroppedEvents
		r.st.RejectedSpans, r.st.LastSendOK = prev.RejectedSpans, prev.LastSendOK
	}
	if err := d.Processor.LoadAll(); err != nil {
		d.Logf("载入会话状态失败: %v", err)
	}
	deadline := d.Now().Add(d.RunFor)

	for {
		r.heartbeat()
		r.cycle(ctx)
		if ctx.Err() != nil {
			r.stopHeartbeat()
			return nil
		}
		if r.shouldExit(deadline) {
			r.stopHeartbeat()
			if r.spoolEmpty() || (r.d.Once && r.d.Now().After(deadline)) {
				return nil
			}
			r.idleSince = time.Time{} // 退出前有新数据：继续处理
			continue
		}
		if !d.Sleep(ctx, d.Tick) {
			r.stopHeartbeat()
			return nil
		}
	}
}

func withDefaults(d Deps) Deps {
	if d.Now == nil {
		d.Now = time.Now
	}
	if d.Sleep == nil {
		d.Sleep = sleepCtx
	}
	if d.Tick == 0 {
		d.Tick = DefaultTick
	}
	if d.IdleExit == 0 {
		d.IdleExit = DefaultIdleExit
	}
	if d.LockWait == 0 {
		d.LockWait = DefaultLockWait
	}
	if d.RunFor == 0 {
		d.RunFor = DefaultRunFor
	}
	if d.Logf == nil {
		d.Logf = func(string, ...any) {}
	}
	return d
}

func (r *runner) cycle(ctx context.Context) {
	names, drained := r.ingestSpool()
	if drained {
		r.d.Processor.CloseStale(r.d.Now())
	}
	spans := r.d.Processor.Flush(r.d.Now())
	if len(spans) > 0 {
		if err := r.enqueue(spans); err != nil {
			r.d.Processor.Discard()
			r.fail(fmt.Errorf("写 outbox 失败: %w", err))
			return // 不提交、不删 spool：下次重放
		}
	}
	if err := r.d.Processor.Commit(); err != nil {
		r.d.Processor.Discard()
		r.fail(fmt.Errorf("保存会话状态失败: %w", err))
		return
	}
	for _, name := range names {
		if err := r.spool.Remove(name); err != nil {
			r.d.Logf("删除 spool 记录失败: %v", err)
		}
	}
	r.send(ctx)
	if n, _ := r.outbox.Enforce(OutboxMaxBytes); n > 0 {
		r.d.Logf("outbox 超过上限，丢弃最旧 %d 批", n)
	}
	_, _ = r.failed.Enforce(FailedMaxBytes)
	r.writeStatus()
}

// ingestSpool 处理一批 spool 记录；drained 表示本次已读到队尾。
func (r *runner) ingestSpool() (names []string, drained bool) {
	entries, err := r.spool.List()
	if err != nil {
		r.fail(fmt.Errorf("读取 spool 失败: %w", err))
		return nil, false
	}
	drained = len(entries) <= SpoolBatch
	if !drained {
		entries = entries[:SpoolBatch]
	}
	names = make([]string, 0, len(entries))
	for _, e := range entries {
		names = append(names, e.Name)
		data, err := r.spool.Read(e.Name)
		if err != nil {
			r.dropped(e.Name, err)
			continue
		}
		rec, err := event.ParseRecord(data)
		if err == nil {
			err = r.d.Processor.Ingest(rec)
		}
		if err != nil {
			r.dropped(e.Name, err)
		}
	}
	return names, drained
}

func (r *runner) dropped(name string, err error) {
	r.st.DroppedEvents++
	r.d.Logf("丢弃无效 spool 记录 %s: %v", name, err)
}

func (r *runner) enqueue(spans []otlp.Span) error {
	for start := 0; start < len(spans); start += MaxSpansPerBatch {
		end := min(start+MaxSpansPerBatch, len(spans))
		body, err := otlp.Encode(r.d.Resource, scopeName, r.d.Version, spans[start:end])
		if err != nil {
			return err
		}
		if _, err := r.outbox.Put(body, r.d.Now()); err != nil {
			return err
		}
	}
	return nil
}

func (r *runner) send(ctx context.Context) {
	if r.d.Now().Before(r.nextRetry) {
		return
	}
	entries, err := r.outbox.List()
	if err != nil {
		r.fail(fmt.Errorf("读取 outbox 失败: %w", err))
		return
	}
	for _, e := range entries {
		if ctx.Err() != nil {
			return
		}
		body, err := r.outbox.Read(e.Name)
		if err != nil {
			_ = r.outbox.Remove(e.Name)
			continue
		}
		sendCtx, cancel := context.WithTimeout(ctx, sendTimeout)
		res := r.d.Sender.Send(sendCtx, body)
		cancel()

		switch res.Outcome {
		case exporter.OK:
			r.onSent(e.Name, body, res)
		case exporter.Retry:
			r.scheduleRetry(res)
			return
		default:
			r.onPermanent(e.Name, body, res)
		}
	}
}

func (r *runner) onSent(name string, body []byte, res exporter.Result) {
	_ = r.outbox.Remove(name)
	r.backoff, r.nextRetry = 0, time.Time{}
	r.st.SentBatches++
	r.st.SentSpans += countSpans(body)
	r.st.LastSendOK = r.d.Now().UTC()
	r.st.NextRetryAt = time.Time{}
	if res.Rejected > 0 {
		r.st.RejectedSpans += res.Rejected
		r.d.Logf("服务端拒收 %d 个 span: %s", res.Rejected, res.Message)
	}
	_ = os.MkdirAll(r.d.Paths.LogsDir(), 0o700)
	_ = fsutil.WriteFileAtomic(r.d.Paths.LastBatchFile(), body, 0o600)
}

func (r *runner) scheduleRetry(res exporter.Result) {
	r.backoff = min(max(r.backoff*2, minBackoff), maxBackoff)
	wait := r.backoff + time.Duration(rand.Int64N(int64(r.backoff)/4+1)) // 抖动，避免多台机器同时重试
	if res.RetryAfter > wait {
		wait = res.RetryAfter
	}
	r.nextRetry = r.d.Now().Add(wait)
	r.st.NextRetryAt = r.nextRetry.UTC()
	r.fail(res.Err)
}

func (r *runner) onPermanent(name string, body []byte, res exporter.Result) {
	if _, err := r.failed.Put(body, r.d.Now()); err != nil {
		r.d.Logf("保存失败批次出错: %v", err)
	}
	_ = r.outbox.Remove(name)
	r.st.FailedBatches++
	r.fail(res.Err)
}

func (r *runner) fail(err error) {
	if err == nil {
		return
	}
	r.st.LastError = err.Error()
	r.st.LastErrorAt = r.d.Now().UTC()
	r.d.Logf("%v", err)
}

// shouldExit：spool 与 outbox 为空（或正处于长退避中）、没有等待收尾的轮次，且空闲足够久。
func (r *runner) shouldExit(deadline time.Time) bool {
	now := r.d.Now()
	if r.d.Once && now.After(deadline) {
		return true
	}
	// 常驻模式下长退避期间可以退出（下一次 hook 会重新拉起）；Once 模式要等到发完或超时
	outboxIdle := r.outboxEmpty() || (!r.d.Once && now.Before(r.nextRetry))
	if !r.spoolEmpty() || !outboxIdle || r.d.Processor.Pending() {
		r.idleSince = time.Time{}
		return false
	}
	if r.d.Once {
		return true
	}
	if r.idleSince.IsZero() {
		r.idleSince = now
	}
	return now.Sub(r.idleSince) >= r.d.IdleExit
}

func (r *runner) spoolEmpty() bool {
	entries, err := r.spool.List()
	return err == nil && len(entries) == 0
}

func (r *runner) outboxEmpty() bool {
	entries, err := r.outbox.List()
	return err == nil && len(entries) == 0
}

func (r *runner) heartbeat() {
	now := r.d.Now()
	if err := os.Chtimes(r.d.Paths.HeartbeatFile(), now, now); err != nil {
		_ = fsutil.WriteFileAtomic(r.d.Paths.HeartbeatFile(), nil, 0o600)
		_ = os.Chtimes(r.d.Paths.HeartbeatFile(), now, now)
	}
}

func (r *runner) stopHeartbeat() { _ = os.Remove(r.d.Paths.HeartbeatFile()) }

func (r *runner) writeStatus() {
	r.st.UpdatedAt = r.d.Now().UTC()
	if entries, err := r.spool.List(); err == nil {
		r.st.SpoolPending = len(entries)
	}
	if entries, err := r.outbox.List(); err == nil {
		r.st.OutboxPending = len(entries)
	}
	if err := status.Write(r.d.Paths.StatusFile(), r.st); err != nil {
		r.d.Logf("%v", err)
	}
}

func sleepCtx(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}
