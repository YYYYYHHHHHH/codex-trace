package logx

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestPrintfAppendsTimestampedLine(t *testing.T) {
	dir := t.TempDir()
	l := New(dir, "hook").WithClock(func() time.Time { return time.Date(2026, 11, 9, 10, 0, 0, 0, time.UTC) })

	l.Printf("hello %s", "world")
	l.Printf("second")

	data, err := os.ReadFile(filepath.Join(dir, "hook.log"))
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	if len(lines) != 2 || lines[0] != "2026-11-09T10:00:00Z hello world" {
		t.Errorf("unexpected log content: %q", data)
	}
}

func TestRotatesWhenTooLarge(t *testing.T) {
	dir := t.TempDir()
	l := New(dir, "up").WithMaxBytes(50)

	for i := 0; i < 10; i++ {
		l.Printf("0123456789")
	}

	if _, err := os.Stat(filepath.Join(dir, "up.log.1")); err != nil {
		t.Errorf("expected rotated file: %v", err)
	}
	info, _ := os.Stat(filepath.Join(dir, "up.log"))
	if info.Size() > 50+64 {
		t.Errorf("current log not rotated, size %d", info.Size())
	}
}

func TestPrintfNeverFailsOnUnwritableDir(t *testing.T) {
	l := New("/dev/null/not-a-dir", "x")
	l.Printf("must not panic")
}
