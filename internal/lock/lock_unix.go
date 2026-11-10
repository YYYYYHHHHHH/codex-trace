//go:build unix

// Package lock 提供基于 flock 的进程间互斥，用于保证上传进程单实例。
package lock

import (
	"errors"
	"os"
	"syscall"
	"time"
)

// TryLock 非阻塞地获取排他锁。ok=false 表示已被其他进程持有。
func TryLock(path string) (release func(), ok bool, err error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, false, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, false, nil
		}
		return nil, false, err
	}
	return func() {
		_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		f.Close()
	}, true, nil
}

// TryLockWait 在 timeout 内反复尝试获取锁，用于接替正在退出的上一个持锁进程。
func TryLockWait(path string, timeout, poll time.Duration) (release func(), ok bool, err error) {
	deadline := time.Now().Add(timeout)
	for {
		release, ok, err = TryLock(path)
		if err != nil || ok || time.Now().After(deadline) {
			return release, ok, err
		}
		time.Sleep(poll)
	}
}
