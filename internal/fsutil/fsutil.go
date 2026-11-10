//go:build unix

// Package fsutil 提供私有目录校验与原子写文件。
package fsutil

import (
	"fmt"
	"os"
	"path/filepath"
	"syscall"
)

// EnsurePrivateDir 创建（如需要）并校验目录：必须是真实目录（非符号链接）、属于当前用户。
// 权限过宽时收紧为 0700；属主不对时拒绝使用（fail closed）。
func EnsurePrivateDir(dir string) error {
	if dir == "" || !filepath.IsAbs(dir) {
		return fmt.Errorf("fsutil: state dir %q must be an absolute path", dir)
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	info, err := os.Lstat(dir)
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return fmt.Errorf("fsutil: %s is not a real directory", dir)
	}
	if uid, ok := OwnerUID(info); ok && uid != uint32(os.Geteuid()) {
		return fmt.Errorf("fsutil: %s is owned by uid %d, not the current user", dir, uid)
	}
	if info.Mode().Perm()&0o077 != 0 {
		return os.Chmod(dir, 0o700)
	}
	return nil
}

// WriteFileAtomic 通过 tmp + rename 写入文件，读者不会看到半截内容。
// 不做 fsync：hook 的时间预算放不下 macOS 的 F_FULLFSYNC，断电时可能丢失最近写入。
func WriteFileAtomic(path string, data []byte, perm os.FileMode) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), ".tmp-"+filepath.Base(path)+"-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Chmod(perm); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpName, path)
}

// OwnerUID 返回文件属主；平台不支持时 ok=false。
func OwnerUID(info os.FileInfo) (uint32, bool) {
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, false
	}
	return st.Uid, true
}
