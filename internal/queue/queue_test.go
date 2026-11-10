package queue

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestPutListReadRemove(t *testing.T) {
	q := New(t.TempDir())
	base := time.Unix(1700000000, 0)

	first, err := q.Put([]byte("one"), base)
	if err != nil {
		t.Fatal(err)
	}
	second, err := q.Put([]byte("two"), base.Add(time.Nanosecond))
	if err != nil {
		t.Fatal(err)
	}

	entries, err := q.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2 || entries[0].Name != first || entries[1].Name != second {
		t.Fatalf("entries not in FIFO order: %+v", entries)
	}

	data, err := q.Read(first)
	if err != nil || !bytes.Equal(data, []byte("one")) {
		t.Fatalf("Read = %q, %v", data, err)
	}

	if err := q.Remove(first); err != nil {
		t.Fatal(err)
	}
	entries, _ = q.List()
	if len(entries) != 1 || entries[0].Name != second {
		t.Fatalf("after remove: %+v", entries)
	}
}

func TestListOnMissingDirIsEmpty(t *testing.T) {
	q := New(filepath.Join(t.TempDir(), "missing"))
	entries, err := q.List()
	if err != nil || len(entries) != 0 {
		t.Fatalf("List = %v, %v", entries, err)
	}
}

func TestListSkipsTempAndForeignFiles(t *testing.T) {
	dir := t.TempDir()
	q := New(dir)
	if _, err := q.Put([]byte("ok"), time.Now()); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{".tmp-123", "notes.txt", "../escape.json"} {
		_ = os.WriteFile(filepath.Join(dir, filepath.Base(name)), []byte("x"), 0o600)
	}

	entries, _ := q.List()
	if len(entries) != 1 {
		t.Fatalf("expected only the queued entry, got %+v", entries)
	}
}

func TestFilesArePrivate(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "q")
	q := New(dir)
	name, err := q.Put([]byte("secret-ish"), time.Now())
	if err != nil {
		t.Fatal(err)
	}

	dirInfo, _ := os.Stat(dir)
	if perm := dirInfo.Mode().Perm(); perm != 0o700 {
		t.Errorf("dir perm = %o, want 700", perm)
	}
	fileInfo, _ := os.Stat(filepath.Join(dir, name))
	if perm := fileInfo.Mode().Perm(); perm != 0o600 {
		t.Errorf("file perm = %o, want 600", perm)
	}
}

func TestReadAndRemoveRejectInvalidNames(t *testing.T) {
	q := New(t.TempDir())
	for _, name := range []string{"../etc/passwd", "x.json", ""} {
		if _, err := q.Read(name); err == nil {
			t.Errorf("Read(%q) should fail", name)
		}
		if err := q.Remove(name); err == nil {
			t.Errorf("Remove(%q) should fail", name)
		}
	}
}

func TestEnforceDropsOldestUntilUnderLimit(t *testing.T) {
	q := New(t.TempDir())
	base := time.Unix(1700000000, 0)
	var names []string
	for i := 0; i < 5; i++ {
		name, err := q.Put(bytes.Repeat([]byte("x"), 100), base.Add(time.Duration(i)))
		if err != nil {
			t.Fatal(err)
		}
		names = append(names, name)
	}

	dropped, err := q.Enforce(250)
	if err != nil {
		t.Fatal(err)
	}
	if dropped != 3 {
		t.Errorf("dropped = %d, want 3", dropped)
	}
	entries, _ := q.List()
	if len(entries) != 2 || entries[0].Name != names[3] || entries[1].Name != names[4] {
		t.Errorf("expected newest two to survive, got %+v", entries)
	}
	size, _ := q.TotalSize()
	if size != 200 {
		t.Errorf("TotalSize = %d, want 200", size)
	}
}

func TestSweepTempRemovesOnlyStaleTempFiles(t *testing.T) {
	dir := t.TempDir()
	q := New(dir)
	now := time.Now()
	stale := filepath.Join(dir, ".tmp-stale")
	fresh := filepath.Join(dir, ".tmp-fresh")
	for _, p := range []string{stale, fresh} {
		if err := os.WriteFile(p, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	_ = os.Chtimes(stale, now.Add(-time.Hour), now.Add(-time.Hour))
	name, _ := q.Put([]byte("keep"), now)

	q.SweepTemp(now, 10*time.Minute)

	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Error("stale temp file should be removed")
	}
	if _, err := os.Stat(fresh); err != nil {
		t.Error("fresh temp file should be kept")
	}
	if _, err := q.Read(name); err != nil {
		t.Error("queued entry must be kept")
	}
}

func TestSweepTempOnMissingDirIsNoop(t *testing.T) {
	New(filepath.Join(t.TempDir(), "missing")).SweepTemp(time.Now(), time.Minute)
}

func TestDirAndRemoveMissingEntry(t *testing.T) {
	dir := t.TempDir()
	q := New(dir)
	if q.Dir() != dir {
		t.Errorf("Dir = %q", q.Dir())
	}
	if err := q.Remove("00000000000000000001-1-deadbeef.json"); err != nil {
		t.Errorf("removing a missing entry should be a no-op, got %v", err)
	}
}
