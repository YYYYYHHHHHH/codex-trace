//go:build unix

package lock

import (
	"path/filepath"
	"testing"
)

func TestTryLockIsExclusive(t *testing.T) {
	path := filepath.Join(t.TempDir(), "x.lock")

	release, ok, err := TryLock(path)
	if err != nil || !ok {
		t.Fatalf("first TryLock = %v, %v", ok, err)
	}
	if !Held(path) {
		t.Error("Held should report true while locked")
	}
	if _, ok2, _ := TryLock(path); ok2 {
		t.Error("second TryLock should fail while held")
	}

	release()
	if Held(path) {
		t.Error("Held should report false after release")
	}
	release2, ok, err := TryLock(path)
	if err != nil || !ok {
		t.Fatalf("TryLock after release = %v, %v", ok, err)
	}
	release2()
}

func TestHeldOnMissingFileIsFalse(t *testing.T) {
	if Held(filepath.Join(t.TempDir(), "missing", "x.lock")) {
		t.Error("missing lock file should not be held")
	}
}
