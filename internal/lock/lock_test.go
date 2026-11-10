//go:build unix

package lock

import (
	"path/filepath"
	"testing"
	"time"
)

func TestTryLockIsExclusive(t *testing.T) {
	path := filepath.Join(t.TempDir(), "x.lock")

	release, ok, err := TryLock(path)
	if err != nil || !ok {
		t.Fatalf("first TryLock = %v, %v", ok, err)
	}
	if _, ok2, _ := TryLock(path); ok2 {
		t.Error("second TryLock should fail while held")
	}

	release()
	release2, ok, err := TryLock(path)
	if err != nil || !ok {
		t.Fatalf("TryLock after release = %v, %v", ok, err)
	}
	release2()
}

func TestTryLockWaitAcquiresAfterRelease(t *testing.T) {
	path := filepath.Join(t.TempDir(), "x.lock")
	release, _, _ := TryLock(path)
	go func() {
		time.Sleep(50 * time.Millisecond)
		release()
	}()

	got, ok, err := TryLockWait(path, 2*time.Second, 10*time.Millisecond)
	if err != nil || !ok {
		t.Fatalf("TryLockWait = %v, %v", ok, err)
	}
	got()
}

func TestTryLockWaitTimesOut(t *testing.T) {
	path := filepath.Join(t.TempDir(), "x.lock")
	release, _, _ := TryLock(path)
	defer release()

	start := time.Now()
	_, ok, err := TryLockWait(path, 80*time.Millisecond, 10*time.Millisecond)
	if err != nil || ok {
		t.Fatalf("TryLockWait should time out, got ok=%v err=%v", ok, err)
	}
	if time.Since(start) < 80*time.Millisecond {
		t.Error("returned before timeout")
	}
}

func TestTryLockFailsOnMissingDir(t *testing.T) {
	if _, _, err := TryLock(filepath.Join(t.TempDir(), "missing", "x.lock")); err == nil {
		t.Error("expected error when lock dir is missing")
	}
}
