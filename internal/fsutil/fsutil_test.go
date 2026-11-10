//go:build unix

package fsutil

import (
	"os"
	"path/filepath"
	"testing"
)

func TestEnsurePrivateDirCreatesWith0700(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "a", "b")
	if err := EnsurePrivateDir(dir); err != nil {
		t.Fatal(err)
	}
	info, _ := os.Stat(dir)
	if info.Mode().Perm() != 0o700 {
		t.Errorf("perm = %o", info.Mode().Perm())
	}
}

func TestEnsurePrivateDirTightensLoosePermissions(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "loose")
	_ = os.Mkdir(dir, 0o755)
	_ = os.Chmod(dir, 0o755)
	if err := EnsurePrivateDir(dir); err != nil {
		t.Fatal(err)
	}
	info, _ := os.Stat(dir)
	if info.Mode().Perm() != 0o700 {
		t.Errorf("perm = %o, want tightened to 700", info.Mode().Perm())
	}
}

func TestEnsurePrivateDirRejectsSymlinkAndRelative(t *testing.T) {
	base := t.TempDir()
	target := filepath.Join(base, "target")
	_ = os.Mkdir(target, 0o700)
	link := filepath.Join(base, "link")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	if err := EnsurePrivateDir(link); err == nil {
		t.Error("symlinked state dir must be rejected")
	}
	if err := EnsurePrivateDir("relative/dir"); err == nil {
		t.Error("relative state dir must be rejected")
	}
	if err := EnsurePrivateDir(""); err == nil {
		t.Error("empty state dir must be rejected")
	}
}

func TestWriteFileAtomic(t *testing.T) {
	path := filepath.Join(t.TempDir(), "f")
	if err := WriteFileAtomic(path, []byte("one"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := WriteFileAtomic(path, []byte("two"), 0o600); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(path)
	info, _ := os.Stat(path)
	if string(data) != "two" || info.Mode().Perm() != 0o600 {
		t.Errorf("content %q perm %o", data, info.Mode().Perm())
	}
	entries, _ := os.ReadDir(filepath.Dir(path))
	if len(entries) != 1 {
		t.Errorf("temp files left behind: %d entries", len(entries))
	}
	if err := WriteFileAtomic(filepath.Join(t.TempDir(), "missing", "f"), nil, 0o600); err == nil {
		t.Error("expected error for missing parent dir")
	}
}

func TestEnsurePrivateDirRejectsRegularFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "file")
	_ = os.WriteFile(path, nil, 0o600)
	if err := EnsurePrivateDir(path); err == nil {
		t.Error("a regular file must not be accepted as state dir")
	}
}

func TestOwnerUIDOfOwnFile(t *testing.T) {
	info, err := os.Stat(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	uid, ok := OwnerUID(info)
	if !ok || uid != uint32(os.Geteuid()) {
		t.Errorf("OwnerUID = %d, %v", uid, ok)
	}
}

func TestEnsurePrivateDirFailsWhenParentIsFile(t *testing.T) {
	file := filepath.Join(t.TempDir(), "file")
	_ = os.WriteFile(file, nil, 0o600)
	if err := EnsurePrivateDir(filepath.Join(file, "sub")); err == nil {
		t.Error("expected mkdir error under a regular file")
	}
}
