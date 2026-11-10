// Package queue 实现基于目录的持久化 FIFO：每个条目一个文件，tmp + rename 原子写入。
// 文件名以纳秒时间戳开头，字典序即先后顺序。
package queue

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
)

const (
	dirPerm   = 0o700
	filePerm  = 0o600
	tmpPrefix = ".tmp-"
)

var namePattern = regexp.MustCompile(`^[0-9]{20}-[0-9]+-[0-9a-f]{8}\.json$`)

// Queue 是一个目录队列；零值不可用，请使用 New。
type Queue struct {
	dir string
}

// Entry 是队列中的一个条目。
type Entry struct {
	Name string
	Size int64
}

func New(dir string) Queue { return Queue{dir: dir} }

func (q Queue) Dir() string { return q.dir }

// Put 原子写入一个条目并返回其文件名。
func (q Queue) Put(data []byte, now time.Time) (string, error) {
	if err := os.MkdirAll(q.dir, dirPerm); err != nil {
		return "", err
	}
	suffix, err := randomHex(4)
	if err != nil {
		return "", err
	}
	name := fmt.Sprintf("%020d-%d-%s.json", now.UnixNano(), os.Getpid(), suffix)

	tmp, err := os.CreateTemp(q.dir, tmpPrefix+"*")
	if err != nil {
		return "", err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName) // rename 成功后为无害的 no-op

	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return "", err
	}
	if err := tmp.Chmod(filePerm); err != nil {
		tmp.Close()
		return "", err
	}
	if err := tmp.Close(); err != nil {
		return "", err
	}
	if err := os.Rename(tmpName, filepath.Join(q.dir, name)); err != nil {
		return "", err
	}
	return name, nil
}

// List 按先后顺序返回条目；目录不存在时返回空。
func (q Queue) List() ([]Entry, error) {
	dirEntries, err := os.ReadDir(q.dir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	entries := make([]Entry, 0, len(dirEntries))
	for _, de := range dirEntries {
		if !de.Type().IsRegular() || !namePattern.MatchString(de.Name()) {
			continue
		}
		info, err := de.Info()
		if err != nil {
			continue // 并发删除
		}
		entries = append(entries, Entry{Name: de.Name(), Size: info.Size()})
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Name < entries[j].Name })
	return entries, nil
}

func (q Queue) Read(name string) ([]byte, error) {
	if err := checkName(name); err != nil {
		return nil, err
	}
	return os.ReadFile(filepath.Join(q.dir, name))
}

func (q Queue) Remove(name string) error {
	if err := checkName(name); err != nil {
		return err
	}
	err := os.Remove(filepath.Join(q.dir, name))
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	return err
}

func (q Queue) TotalSize() (int64, error) {
	entries, err := q.List()
	if err != nil {
		return 0, err
	}
	var total int64
	for _, e := range entries {
		total += e.Size
	}
	return total, nil
}

// Enforce 从最旧的条目开始删除，直到总大小不超过 maxBytes；返回删除个数。
func (q Queue) Enforce(maxBytes int64) (int, error) {
	entries, err := q.List()
	if err != nil {
		return 0, err
	}
	var total int64
	for _, e := range entries {
		total += e.Size
	}
	dropped := 0
	for _, e := range entries {
		if total <= maxBytes {
			break
		}
		if err := q.Remove(e.Name); err != nil {
			return dropped, err
		}
		total -= e.Size
		dropped++
	}
	return dropped, nil
}

// SweepTemp 删除崩溃遗留、超过 maxAge 的临时文件。
func (q Queue) SweepTemp(now time.Time, maxAge time.Duration) {
	dirEntries, err := os.ReadDir(q.dir)
	if err != nil {
		return
	}
	for _, de := range dirEntries {
		if !strings.HasPrefix(de.Name(), tmpPrefix) {
			continue
		}
		if info, err := de.Info(); err == nil && now.Sub(info.ModTime()) > maxAge {
			_ = os.Remove(filepath.Join(q.dir, de.Name()))
		}
	}
}

func checkName(name string) error {
	if !namePattern.MatchString(name) {
		return fmt.Errorf("queue: invalid entry name %q", name)
	}
	return nil
}

func randomHex(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}
