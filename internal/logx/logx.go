// Package logx 是尽力而为的本地文件日志：任何错误都被吞掉，绝不影响调用方。
package logx

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const defaultMaxBytes = 1 << 20

// Logger 写入 <dir>/<name>.log，超过上限时轮转为 .log.1。
type Logger struct {
	path     string
	maxBytes int64
	now      func() time.Time
}

func New(dir, name string) Logger {
	return Logger{path: filepath.Join(dir, name+".log"), maxBytes: defaultMaxBytes, now: time.Now}
}

func (l Logger) WithClock(now func() time.Time) Logger { l.now = now; return l }

func (l Logger) WithMaxBytes(n int64) Logger { l.maxBytes = n; return l }

func (l Logger) Path() string { return l.path }

func (l Logger) Printf(format string, args ...any) {
	if err := os.MkdirAll(filepath.Dir(l.path), 0o700); err != nil {
		return
	}
	if info, err := os.Stat(l.path); err == nil && info.Size() >= l.maxBytes {
		_ = os.Rename(l.path, l.path+".1")
	}
	f, err := os.OpenFile(l.path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return
	}
	defer f.Close()
	msg := strings.ReplaceAll(fmt.Sprintf(format, args...), "\n", "\\n")
	_, _ = fmt.Fprintf(f, "%s %s\n", l.now().UTC().Format(time.RFC3339), msg)
}
