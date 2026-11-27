// Package marker is the Summon session marker, /run/airgapp-summon/session. The engine sets it before H1 and clears
// it only after a clean H4 (internal/summon). While it exists, the supervisor's takeover and the runit finish hook send
// the abort and the disconnect themselves.
//
// It is a leaf package (R15): it imports nothing from internal/summon, so the no-motion probe can use the marker
// without linking the helper's abort code.
package marker

import (
	"errors"
	"io/fs"
	"os"
	"syscall"
)

// File is the marker at one path. It satisfies summon.Marker.
type File struct{ path string }

// New returns the marker at path. It touches nothing.
func New(path string) *File { return &File{path: path} }

// Set creates the marker, an empty file of mode 0600. It succeeds when the marker exists already (R7). It refuses to
// follow a symlink at the path: Set then fails, and the session does not start.
func (f *File) Set() error {
	fd, err := os.OpenFile(f.path, os.O_RDONLY|os.O_CREATE|syscall.O_NOFOLLOW, 0o600)
	if err != nil {
		return err
	}
	return fd.Close()
}

// Clear removes the marker. A missing marker is already clear.
func (f *File) Clear() error {
	if err := os.Remove(f.path); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return nil
}

// Present is the fail-safe marker check: true when Lstat succeeds, or fails with anything other than ENOENT. A marker
// that cannot be checked counts as present, so the abort is sent.
func Present(path string) bool {
	_, err := os.Lstat(path)
	return err == nil || !errors.Is(err, syscall.ENOENT)
}
