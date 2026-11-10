// Package hook 实现 Codex hook 入口：读 stdin → 写 spool → 必要时拉起上传进程。
//
// 约束（见 docs/HANDOFF.md 第 6 节）：不发网络请求、不向 stdout 输出、
// 永远正常返回、未配置时静默。所有错误只写本地日志。
package hook

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/YYYYYHHHHHH/codex-trace/internal/config"
	"github.com/YYYYYHHHHHH/codex-trace/internal/event"
	"github.com/YYYYYHHHHHH/codex-trace/internal/lock"
	"github.com/YYYYYHHHHHH/codex-trace/internal/logx"
	"github.com/YYYYYHHHHHH/codex-trace/internal/paths"
	"github.com/YYYYYHHHHHH/codex-trace/internal/queue"
)

// MaxPayloadBytes 是单条 payload 上限；超出则丢弃（字段级截断在上传进程中完成）。
const MaxPayloadBytes = 16 << 20

// Deps 是 Run 的全部外部依赖，便于测试。
type Deps struct {
	Stdin         io.Reader
	Args          []string // `trace-hook hook` 之后的参数，第一个为事件名
	Getenv        func(string) string
	Now           func() time.Time
	Paths         paths.Paths
	Builtin       config.Builtin
	StartUploader func() error
}

// Run 处理一次 hook 调用，永不 panic、永不返回错误。
func Run(d Deps) {
	data, readErr := readLimited(d.Stdin, MaxPayloadBytes)

	cfg := config.Load(d.Paths, d.Getenv, d.Builtin)
	if !cfg.Enabled || !cfg.Configured() || exists(d.Paths.PausedFile()) {
		return
	}

	log := logx.New(d.Paths.LogsDir(), "hook")
	defer func() {
		if r := recover(); r != nil {
			log.Printf("panic: %v", r)
		}
	}()

	if err := capture(d, data, readErr); err != nil {
		log.Printf("%v", err)
		return
	}
	if lock.Held(d.Paths.LockFile()) {
		return
	}
	if err := d.StartUploader(); err != nil {
		log.Printf("启动上传进程失败: %v", err)
	}
}

func capture(d Deps, data []byte, readErr error) error {
	if readErr != nil {
		return fmt.Errorf("读取 stdin 失败: %w", readErr)
	}
	if data == nil {
		return fmt.Errorf("payload 超过 %d 字节，已丢弃", MaxPayloadBytes)
	}
	if !isJSONObject(data) {
		return fmt.Errorf("payload 不是 JSON 对象（%d 字节），已丢弃", len(data))
	}

	now := d.Now()
	name := eventName(d.Args, data)
	rec, err := json.Marshal(event.Record{
		Version:    event.RecordVersion,
		ReceivedAt: now.UTC(),
		Event:      name,
		Payload:    data,
	})
	if err != nil {
		return err
	}
	if _, err := queue.New(d.Paths.SpoolDir()).Put(rec, now); err != nil {
		return fmt.Errorf("写 spool 失败: %w", err)
	}
	marker := fmt.Sprintf("%s %s\n", now.UTC().Format(time.RFC3339Nano), name)
	_ = os.WriteFile(d.Paths.LastHookFile(), []byte(marker), 0o600)
	return nil
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

func isJSONObject(data []byte) bool {
	var obj map[string]json.RawMessage
	return json.Unmarshal(data, &obj) == nil && obj != nil
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
