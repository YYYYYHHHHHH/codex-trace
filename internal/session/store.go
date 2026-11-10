package session

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/YYYYYHHHHHH/codex-trace/internal/fsutil"
)

var safeID = regexp.MustCompile(`^[A-Za-z0-9_-]{1,128}$`)

// Store 把会话状态持久化为 <dir>/<session>.json。
type Store struct {
	dir string
}

func NewStore(dir string) Store { return Store{dir: dir} }

// Load 读取会话状态；不存在时返回空状态。
func (s Store) Load(sessionID string) (*State, error) {
	data, err := os.ReadFile(s.path(sessionID))
	if errors.Is(err, fs.ErrNotExist) {
		return New(sessionID), nil
	}
	if err != nil {
		return nil, fmt.Errorf("session: read %s: %w", sessionID, err)
	}
	var st State
	if err := json.Unmarshal(data, &st); err != nil {
		return nil, fmt.Errorf("session: decode %s: %w", sessionID, err)
	}
	st.SessionID = sessionID
	st.ensureMaps()
	return &st, nil
}

func (s Store) Save(st *State) error {
	if err := os.MkdirAll(s.dir, 0o700); err != nil {
		return fmt.Errorf("session: mkdir: %w", err)
	}
	data, err := json.Marshal(st)
	if err != nil {
		return fmt.Errorf("session: encode %s: %w", st.SessionID, err)
	}
	if err := fsutil.WriteFileAtomic(s.path(st.SessionID), data, 0o600); err != nil {
		return fmt.Errorf("session: write %s: %w", st.SessionID, err)
	}
	return nil
}

func (s Store) Delete(sessionID string) error {
	err := os.Remove(s.path(sessionID))
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	return err
}

// List 返回已持久化的会话 id（无法还原的哈希文件名会原样返回，Load 时同样可用）。
func (s Store) List() ([]string, error) {
	entries, err := os.ReadDir(s.dir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("session: list: %w", err)
	}
	var ids []string
	for _, e := range entries {
		name := e.Name()
		if e.Type().IsRegular() && strings.HasSuffix(name, ".json") && !strings.HasPrefix(name, ".") {
			ids = append(ids, strings.TrimSuffix(name, ".json"))
		}
	}
	return ids, nil
}

// path 只把安全字符组成的 id 直接用作文件名，其余一律哈希，杜绝路径穿越。
func (s Store) path(sessionID string) string {
	name := sessionID
	if !safeID.MatchString(sessionID) {
		sum := sha256.Sum256([]byte(sessionID))
		name = "h-" + hex.EncodeToString(sum[:16])
	}
	return filepath.Join(s.dir, name+".json")
}
