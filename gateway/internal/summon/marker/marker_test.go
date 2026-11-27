package marker

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"testing"
)

func TestSetCreatesAndIsIdempotent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "session")
	m := New(path)
	if err := m.Set(); err != nil {
		t.Fatalf("Set: %v", err)
	}
	st, err := os.Lstat(path)
	if err != nil {
		t.Fatalf("no marker after Set: %v", err)
	}
	if !st.Mode().IsRegular() || st.Mode().Perm() != 0o600 {
		t.Fatalf("marker mode %v, want a regular file of mode 0600", st.Mode())
	}
	// R7: Set on an existing marker succeeds, and leaves it in place.
	if err := m.Set(); err != nil {
		t.Fatalf("Set on an existing marker: %v", err)
	}
	if !Present(path) {
		t.Fatal("the marker is gone after a second Set")
	}
}

func TestClear(t *testing.T) {
	path := filepath.Join(t.TempDir(), "session")
	m := New(path)
	if err := m.Set(); err != nil {
		t.Fatal(err)
	}
	if err := m.Clear(); err != nil {
		t.Fatalf("Clear: %v", err)
	}
	if _, err := os.Lstat(path); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("the marker survived Clear: %v", err)
	}
	if err := m.Clear(); err != nil {
		t.Fatalf("Clear on a missing marker: %v, want nil", err)
	}
}

// A symlink at the path is refused: Set fails (the session does not start) and creates nothing at the target.
func TestSetRefusesSymlink(t *testing.T) {
	dir := t.TempDir()
	path, target := filepath.Join(dir, "session"), filepath.Join(dir, "target")
	if err := os.Symlink(target, path); err != nil {
		t.Fatal(err)
	}
	if err := New(path).Set(); err == nil {
		t.Fatal("Set followed a symlink")
	}
	if _, err := os.Lstat(target); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("Set created the symlink's target: %v", err)
	}
}

// Present is fail-safe: only ENOENT means absent.
func TestPresent(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "file")
	if err := os.WriteFile(file, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	dangling := filepath.Join(dir, "dangling")
	if err := os.Symlink(filepath.Join(dir, "nowhere"), dangling); err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name, path string
		want       bool
	}{
		{"missing", filepath.Join(dir, "missing"), false},
		{"a regular file", file, true},
		{"a dangling symlink (Lstat succeeds)", dangling, true},
		{"a path under a regular file (ENOTDIR)", filepath.Join(file, "session"), true},
	}
	for _, c := range cases {
		if got := Present(c.path); got != c.want {
			t.Errorf("%s: Present = %v, want %v", c.name, got, c.want)
		}
	}

	// A directory that cannot be searched gives EACCES: present. Root bypasses the permission, so skip it there.
	if os.Geteuid() == 0 {
		t.Log("running as root: the EACCES case is skipped")
		return
	}
	locked := filepath.Join(dir, "locked")
	if err := os.Mkdir(locked, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(locked, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(locked, 0o700) })
	if !Present(filepath.Join(locked, "session")) {
		t.Error("an unreadable marker path counts as absent; it must count as present")
	}
}
