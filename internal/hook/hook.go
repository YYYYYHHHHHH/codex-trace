// Package hook 实现 Codex hook 入口：读 stdin → 写 spool → 必要时拉起上传进程。
//
// 约束（见 docs/HANDOFF.md 第 6 节）：不发网络请求、不向 stdout 输出、
// 永远正常返回、未配置时静默。所有错误只写本地日志。
package hook

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"math/rand/v2"
	"os"
	"strings"
	"time"

	"github.com/YYYYYHHHHHH/codex-trace/internal/config"
	"github.com/YYYYYHHHHHH/codex-trace/internal/event"
	"github.com/YYYYYHHHHHH/codex-trace/internal/fsutil"
	"github.com/YYYYYHHHHHH/codex-trace/internal/logx"
	"github.com/YYYYYHHHHHH/codex-trace/internal/paths"
	"github.com/YYYYYHHHHHH/codex-trace/internal/queue"
)

const (
	// MaxPayloadBytes 是单条 payload 上限；超出则丢弃（字段级截断在上传进程中完成）。
	MaxPayloadBytes = 16 << 20
	// SpoolMaxBytes 是 spool 容量上限，超出时丢最旧的记录。
	SpoolMaxBytes = 32 << 20
	// HeartbeatFresh 内有心跳即认为上传进程在运行。
	HeartbeatFresh = 5 * time.Second
	// enforceEvery 表示平均每多少次 hook 做一次容量清理，避免每次都扫描目录。
	enforceEvery = 64
	tempMaxAge   = 10 * time.Minute
)

// contentFields 是 capture_content=false 时不落盘的字段。
var contentFields = []string{"prompt", "tool_input", "tool_response", "last_assistant_message"}

// Deps 是 Run 的全部外部依赖，便于测试。
type Deps struct {
	Stdin         io.Reader
	Args          []string // `trace-hook hook` 之后的参数，第一个为事件名
	Getenv        func(string) string
	Now           func() time.Time
	Paths         paths.Paths
	Builtin       config.Builtin
	StartUploader func() error
	// Rand 返回 [0,1) 随机数，用于抽样清理；为 nil 时使用 math/rand。
	Rand func() float64
}

// Run 处理一次 hook 调用，永不 panic、永不返回错误。
func Run(d Deps) {
	defer func() { _ = recover() }() // 未配置时连日志都不写

	data, readErr := readLimited(d.Stdin, MaxPayloadBytes)
	if !d.Paths.Usable() {
		return
	}
	cfg := config.Load(d.Paths, d.Getenv, d.Builtin)
	if !cfg.Enabled || !cfg.Configured() || exists(d.Paths.PausedFile()) {
		return
	}
	if err := fsutil.EnsurePrivateDir(d.Paths.StateDir); err != nil {
		return // 目录不可信时不写任何东西（包括日志）
	}

	log := logx.New(d.Paths.LogsDir(), "hook")
	defer func() {
		if r := recover(); r != nil {
			log.Printf("panic: %v", r)
		}
	}()

	if err := capture(d, cfg, data, readErr); err != nil {
		log.Printf("%v", err)
		return
	}
	maybeEnforce(d)
	if uploaderAlive(d.Paths.HeartbeatFile(), d.Now()) {
		return
	}
	if err := d.StartUploader(); err != nil {
		log.Printf("启动上传进程失败: %v", err)
	}
}

func capture(d Deps, cfg config.Config, data []byte, readErr error) error {
	if readErr != nil {
		return fmt.Errorf("读取 stdin 失败: %w", readErr)
	}
	if data == nil {
		return fmt.Errorf("payload 超过 %d 字节，已丢弃", MaxPayloadBytes)
	}
	payload, err := preparePayload(data, cfg.CaptureContent)
	if err != nil {
		return fmt.Errorf("payload 不是 JSON 对象（%d 字节），已丢弃", len(data))
	}

	now := d.Now()
	name := eventName(d.Args, payload)
	rec, err := json.Marshal(event.Record{
		Version:    event.RecordVersion,
		ReceivedAt: now.UTC(),
		Event:      name,
		Payload:    payload,
	})
	if err != nil {
		return err
	}
	if _, err := queue.New(d.Paths.SpoolDir()).Put(rec, now); err != nil {
		return fmt.Errorf("写 spool 失败: %w", err)
	}
	marker := fmt.Sprintf("%s %s\n", now.UTC().Format(time.RFC3339Nano), name)
	if err := fsutil.WriteFileAtomic(d.Paths.LastHookFile(), []byte(marker), 0o600); err != nil {
		return fmt.Errorf("写 last-hook 失败: %w", err)
	}
	return nil
}

// preparePayload 校验 payload 为 JSON 对象；不采集内容时在落盘前剥离内容字段，只保留长度。
// spawn_agent 的 tool_response 只有 {"task_name": ...}，是关联子 agent 的依据，予以保留。
func preparePayload(data []byte, captureContent bool) ([]byte, error) {
	trimmed := bytes.TrimSpace(data)
	if len(trimmed) == 0 || trimmed[0] != '{' {
		return nil, fmt.Errorf("not an object")
	}
	if captureContent {
		if !json.Valid(trimmed) {
			return nil, fmt.Errorf("invalid json")
		}
		return trimmed, nil
	}

	var obj map[string]json.RawMessage
	if err := json.Unmarshal(trimmed, &obj); err != nil {
		return nil, err
	}
	keepResponse := isSpawnTool(obj["tool_name"])
	sizes := map[string]int{}
	for _, k := range contentFields {
		v, ok := obj[k]
		if !ok || (k == "tool_response" && keepResponse) {
			continue
		}
		sizes[k] = contentLen(v)
		delete(obj, k)
	}
	if len(sizes) > 0 {
		raw, err := json.Marshal(sizes)
		if err != nil {
			return nil, err
		}
		obj[event.ContentSizesField] = raw
	}
	return json.Marshal(obj)
}

func isSpawnTool(raw json.RawMessage) bool {
	var name string
	_ = json.Unmarshal(raw, &name)
	return strings.HasSuffix(name, "spawn_agent")
}

// contentLen 返回字符串内容的字节长度；非字符串取原始 JSON 长度。
func contentLen(raw json.RawMessage) int {
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return len(s)
	}
	return len(raw)
}

func maybeEnforce(d Deps) {
	r := d.Rand
	if r == nil {
		r = rand.Float64
	}
	if r()*enforceEvery >= 1 {
		return
	}
	q := queue.New(d.Paths.SpoolDir())
	q.SweepTemp(d.Now(), tempMaxAge)
	if dropped, err := q.Enforce(SpoolMaxBytes); err == nil && dropped > 0 {
		logx.New(d.Paths.LogsDir(), "hook").Printf("spool 超过 %d 字节，丢弃最旧 %d 条", SpoolMaxBytes, dropped)
	}
}

// uploaderAlive 依据心跳文件判断上传进程是否在运行。
// 不用 flock 探测：探测本身会短暂持锁，可能让正在启动的上传进程抢锁失败。
func uploaderAlive(heartbeat string, now time.Time) bool {
	info, err := os.Stat(heartbeat)
	return err == nil && now.Sub(info.ModTime()) < HeartbeatFresh
}

// readLimited 读取至多 max 字节；超出时读完剩余内容（避免 Codex 写端阻塞）并返回 nil。
func readLimited(r io.Reader, max int64) ([]byte, error) {
	data, err := io.ReadAll(io.LimitReader(r, max+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > max {
		_, _ = io.Copy(io.Discard, r)
		return nil, nil
	}
	if data == nil {
		data = []byte{}
	}
	return data, nil
}

func eventName(args []string, data []byte) string {
	if len(args) > 0 && args[0] != "" {
		return args[0]
	}
	var p struct {
		HookEventName string `json:"hook_event_name"`
	}
	_ = json.Unmarshal(data, &p)
	return p.HookEventName
}

func exists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}
