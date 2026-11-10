package status

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestWriteReadRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "status.json")
	in := Status{PID: 42, SentSpans: 7, LastError: "HTTP 401", UpdatedAt: time.Unix(1700000000, 0).UTC()}
	if err := Write(path, in); err != nil {
		t.Fatal(err)
	}
	out, ok, err := Read(path)
	if err != nil || !ok || out.PID != 42 || out.SentSpans != 7 || !out.UpdatedAt.Equal(in.UpdatedAt) {
		t.Errorf("Read = %+v, %v, %v", out, ok, err)
	}
	info, _ := os.Stat(path)
	if info.Mode().Perm() != 0o600 {
		t.Errorf("perm = %o", info.Mode().Perm())
	}
}

func TestReadMissingAndCorrupt(t *testing.T) {
	dir := t.TempDir()
	if _, ok, err := Read(filepath.Join(dir, "missing.json")); ok || err != nil {
		t.Errorf("missing: ok=%v err=%v", ok, err)
	}
	bad := filepath.Join(dir, "bad.json")
	_ = os.WriteFile(bad, []byte("{"), 0o600)
	if _, _, err := Read(bad); err == nil {
		t.Error("corrupt status should error")
	}
}
