// Package status 读写上传进程的运行状态文件（status.json），供 status / doctor 命令展示。
package status

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"time"

	"github.com/YYYYYHHHHHH/codex-trace/internal/fsutil"
)

// Status 是上传进程最近一次循环后的状态快照。错误信息中不含凭证。
type Status struct {
	PID            int       `json:"pid"`
	Version        string    `json:"version"`
	StartedAt      time.Time `json:"started_at"`
	UpdatedAt      time.Time `json:"updated_at"`
	LastSendOK     time.Time `json:"last_send_ok,omitempty"`
	LastError      string    `json:"last_error,omitempty"`
	LastErrorAt    time.Time `json:"last_error_at,omitempty"`
	SentSpans      int64     `json:"sent_spans"`
	SentBatches    int64     `json:"sent_batches"`
	RejectedSpans  int64     `json:"rejected_spans"`
	FailedBatches  int64     `json:"failed_batches"`
	DroppedEvents  int64     `json:"dropped_events"`
	DroppedBatches int64     `json:"dropped_batches"` // outbox 超出上限被淘汰的批次
	BackoffSecs    int64     `json:"backoff_secs,omitempty"`
	SpoolPending   int       `json:"spool_pending"`
	OutboxPending  int       `json:"outbox_pending"`
	NextRetryAt    time.Time `json:"next_retry_at,omitempty"`
}

// Write 原子写入状态文件。
func Write(path string, s Status) error {
	data, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return fmt.Errorf("status: encode: %w", err)
	}
	if err := fsutil.WriteFileAtomic(path, data, 0o600); err != nil {
		return fmt.Errorf("status: write: %w", err)
	}
	return nil
}

// Read 读取状态文件；不存在时 ok=false。
func Read(path string) (Status, bool, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return Status{}, false, nil
	}
	if err != nil {
		return Status{}, false, fmt.Errorf("status: read: %w", err)
	}
	var s Status
	if err := json.Unmarshal(data, &s); err != nil {
		return Status{}, false, fmt.Errorf("status: decode: %w", err)
	}
	return s, true, nil
}
