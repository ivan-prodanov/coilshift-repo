package helper

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// liveLocked reports whether another open file description holds a lock on the file at path, probed as camhelper's
// reader does (internal/camhelper/summon.go summonLive): a fresh O_RDONLY|O_NOFOLLOW|O_NONBLOCK open, then
// flock(LOCK_SH|LOCK_NB), which fails with EWOULDBLOCK while summonhelper holds its LOCK_EX. A probe that gets the lock
// releases it at once. The file must exist.
func liveLocked(t *testing.T, path string) bool {
	t.Helper()
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		t.Fatalf("the lock probe's open: %v", err)
	}
	defer f.Close()
	fd := int(f.Fd())
	switch err := syscall.Flock(fd, syscall.LOCK_SH|syscall.LOCK_NB); {
	case err == nil:
		syscall.Flock(fd, syscall.LOCK_UN)
		return false
	case errors.Is(err, syscall.EWOULDBLOCK):
		return true
	default:
		t.Fatalf("the lock probe's flock: %v", err)
		return false
	}
}

// noTempFiles fails the test if dir holds a ".live." temp file.
func noTempFiles(t *testing.T, dir string) {
	t.Helper()
	ents, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range ents {
		if strings.HasPrefix(e.Name(), ".live.") {
			t.Fatalf("the temp file %s was left behind", e.Name())
		}
	}
}

func TestLiveFile(t *testing.T) {
	t.Run("held", func(t *testing.T) {
		dir := t.TempDir()
		p := filepath.Join(dir, "live")
		h, err := holdLive(p)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { h.release() })
		if b, err := os.ReadFile(p); err != nil || string(b) != fmt.Sprintf("%d\n", os.Getpid()) {
			t.Fatalf("the live file holds %q (%v), want this process's pid and a newline", b, err)
		}
		fi, err := os.Lstat(p)
		if err != nil {
			t.Fatal(err)
		}
		if !fi.Mode().IsRegular() || fi.Mode().Perm() != 0o600 {
			t.Fatalf("the live file's mode is %v, want a regular file, 0600", fi.Mode())
		}
		if !liveLocked(t, p) {
			t.Fatal("the probe got a lock on a held live file: camhelper would see no session")
		}
		noTempFiles(t, dir)
	})

	t.Run("release removes and unlocks", func(t *testing.T) {
		p := filepath.Join(t.TempDir(), "live")
		h, err := holdLive(p)
		if err != nil {
			t.Fatal(err)
		}
		// The same file, still open after the unlink: it shows whether the lock went with the release.
		f, err := os.Open(p)
		if err != nil {
			t.Fatal(err)
		}
		defer f.Close()
		if err := h.release(); err != nil {
			t.Fatalf("release: %v", err)
		}
		if _, err := os.Lstat(p); !errors.Is(err, fs.ErrNotExist) {
			t.Fatalf("the live file survived its release (Lstat: %v)", err)
		}
		if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
			t.Fatalf("the released file is still locked: %v", err)
		}
	})

	// L4: a late release never removes the file of the hold that replaced it, nor drops that hold's lock.
	t.Run("release keeps another hold's file", func(t *testing.T) {
		p := filepath.Join(t.TempDir(), "live")
		a, err := holdLive(p)
		if err != nil {
			t.Fatal(err)
		}
		b, err := holdLive(p)
		if err != nil {
			t.Fatal(err)
		}
		if err := a.release(); err != nil {
			t.Fatalf("the first hold's release: %v", err)
		}
		if _, err := os.Lstat(p); err != nil {
			t.Fatalf("the first hold's release removed the second hold's file (Lstat: %v)", err)
		}
		if !liveLocked(t, p) {
			t.Fatal("the second hold's file is not locked after the first hold's release")
		}
		if err := b.release(); err != nil {
			t.Fatalf("the second hold's release: %v", err)
		}
		if _, err := os.Lstat(p); !errors.Is(err, fs.ErrNotExist) {
			t.Fatalf("the second hold's file survived its release (Lstat: %v)", err)
		}
	})

	// L3: an unlink that fails still releases the lock, so the file left behind proves nothing.
	t.Run("release when unlink fails", func(t *testing.T) {
		if os.Getuid() == 0 {
			t.Skip("root ignores directory permissions")
		}
		dir := t.TempDir()
		p := filepath.Join(dir, "live")
		h, err := holdLive(p)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(dir, 0o555); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { os.Chmod(dir, 0o755) })
		if err := h.release(); err == nil {
			t.Fatal("a release whose unlink failed reported success")
		}
		f, err := os.Open(p)
		if err != nil {
			t.Fatalf("the live file that could not be removed: %v", err)
		}
		defer f.Close()
		if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
			t.Fatalf("the lock outlived the release: %v", err)
		}
	})

	// L2: release opens nothing, so a FIFO at the path cannot block it; and the FIFO is not ours, so it stays.
	t.Run("a FIFO at the path never blocks release", func(t *testing.T) {
		p := filepath.Join(t.TempDir(), "live")
		h, err := holdLive(p)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.Remove(p); err != nil {
			t.Fatal(err)
		}
		if err := syscall.Mkfifo(p, 0o600); err != nil {
			t.Fatal(err)
		}
		done := make(chan error, 1)
		go func() { done <- h.release() }()
		select {
		case err := <-done:
			if err != nil {
				t.Fatalf("release: %v", err)
			}
		case <-time.After(time.Second):
			t.Fatal("release blocked on a FIFO at the path")
		}
		if fi, err := os.Lstat(p); err != nil || fi.Mode()&fs.ModeNamedPipe == 0 {
			t.Fatalf("the FIFO, which is not the hold's file, did not stay (Lstat: %v)", err)
		}
	})

	// The temp file is created with O_EXCL|O_NOFOLLOW and renamed over the path: a symlink there is replaced, and its
	// target is never created.
	t.Run("a symlink at the path is replaced", func(t *testing.T) {
		dir := t.TempDir()
		p := filepath.Join(dir, "live")
		target := filepath.Join(dir, "elsewhere")
		if err := os.Symlink(target, p); err != nil {
			t.Fatal(err)
		}
		h, err := holdLive(p)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { h.release() })
		if fi, err := os.Lstat(p); err != nil || !fi.Mode().IsRegular() {
			t.Fatalf("the live file is not a regular file (Lstat: %v)", err)
		}
		if _, err := os.Lstat(target); !errors.Is(err, fs.ErrNotExist) {
			t.Fatalf("the symlink's target was created (Lstat: %v)", err)
		}
	})

	t.Run("RemoveLive", func(t *testing.T) {
		p := filepath.Join(t.TempDir(), "live")
		if removed, err := RemoveLive(p); removed || err != nil {
			t.Fatalf("RemoveLive of an absent file = %v, %v; want false, nil", removed, err)
		}
		if err := os.WriteFile(p, []byte("123\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		if removed, err := RemoveLive(p); !removed || err != nil {
			t.Fatalf("RemoveLive of a present file = %v, %v; want true, nil", removed, err)
		}
		if _, err := os.Lstat(p); !errors.Is(err, fs.ErrNotExist) {
			t.Fatalf("RemoveLive left the file (Lstat: %v)", err)
		}
	})
}
