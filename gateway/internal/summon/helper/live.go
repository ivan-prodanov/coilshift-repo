package helper

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"syscall"
)

// The live file (Config.LivePath: camhelper/proto.SummonLivePath, /run/airgapp-summon/live) says "a Summon session's
// engine is up now, in process <pid>". camhelper grants the Summon camera only while some process holds a lock on it
// (design §6.5): its probe's flock(LOCK_SH|LOCK_NB) must fail with EWOULDBLOCK. Only a session that opted into the
// presence lease makes one (summon.CapConfig.Live; ruling F1), and it holds its own file, under LOCK_EX, from before
// the file is visible until it is unlinked. The kernel drops the lock when the holder dies, so a file left by a helper
// that died proves nothing; RemoveLive still removes it wherever a helper's end is handled: at Serve's start, in the
// takeover, and in Finish and `summonhelper finish`.

// liveHold is one session's live file: the open file, locked, kept until release.
type liveHold struct {
	f    *os.File
	path string
}

// holdLive creates the live file at path for one session. It creates a temp file ".live.<random>" in path's directory
// (O_CREATE|O_EXCL|O_NOFOLLOW, mode 0600, so it never follows a symlink), takes LOCK_EX on it, writes "<pid>\n" and
// renames it to path, replacing whatever path named: path is never visible empty or unlocked. The hold keeps the file
// open, and so locked, until release. On any failure, a panic included, the temp file is closed and removed.
func holdLive(path string) (*liveHold, error) {
	dir := filepath.Dir(path)
	var (
		f   *os.File
		tmp string
		err error
	)
	for range 10 {
		tmp = filepath.Join(dir, ".live."+randomSuffix())
		f, err = os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL|syscall.O_NOFOLLOW, 0o600)
		if !errors.Is(err, fs.ErrExist) {
			break
		}
	}
	if err != nil {
		return nil, err
	}
	held := false
	defer func() {
		if !held {
			f.Close()
			os.Remove(tmp)
		}
	}()
	for {
		err = syscall.Flock(int(f.Fd()), syscall.LOCK_EX)
		if !errors.Is(err, syscall.EINTR) {
			break
		}
	}
	if err != nil {
		return nil, fmt.Errorf("locking %s: %w", tmp, err)
	}
	if _, err := fmt.Fprintf(f, "%d\n", os.Getpid()); err != nil {
		return nil, err
	}
	if err := os.Rename(tmp, path); err != nil {
		return nil, err
	}
	held = true
	return &liveHold{f: f, path: path}, nil
}

// randomSuffix is 16 random hex digits: a temp file name nobody can guess ahead of it.
func randomSuffix() string {
	var b [8]byte
	rand.Read(b[:]) // never fails (crypto/rand)
	return hex.EncodeToString(b[:])
}

// release ends the hold. It unlinks path only while path still names the hold's own file (os.SameFile: the same device
// and inode), so a late release never removes the file of the hold that replaced it (L4), nor a file that is not one of
// ours. Then it closes the file, which drops the lock whatever the unlink did (L3), a panic in it included: the close
// is deferred, so the setLive that recovers that panic (L2) never leaves the lock held. It opens nothing, so nothing at
// path can block it (L2). Its error is the unlink's, or that of the checks before it; a file already gone is no error.
func (h *liveHold) release() error {
	defer h.f.Close()
	return h.unlinkOwn()
}

// unlinkOwn unlinks path if it names the hold's file.
func (h *liveHold) unlinkOwn() error {
	li, err := os.Lstat(h.path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	fi, err := h.f.Stat()
	if err != nil {
		return err
	}
	if !os.SameFile(fi, li) {
		return nil
	}
	if err := os.Remove(h.path); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return nil
}

// RemoveLive removes a live file left at path by a helper that is gone. removed reports whether one was there; a
// missing file is no error.
func RemoveLive(path string) (removed bool, err error) {
	switch err := os.Remove(path); {
	case err == nil:
		return true, nil
	case errors.Is(err, fs.ErrNotExist):
		return false, nil
	default:
		return false, err
	}
}

// leftLive is what a live file that could not be removed means: camhelper goes by the lock, not by the file.
const leftLive = "unlocked, it proves nothing to camhelper; still locked by a dying process, it counts until that " +
	"process exits"

// LogLiveRemoval logs what RemoveLive returned where a helper's end is handled, who ("takeover" or "finish") having
// called it: the removal, or its error, and nothing when no file was there. It is never called before an abort that
// is still to be sent. The takeover and Finish call it only after they have released the name (R4); Finish without a
// marker has none to release. cmdFinish (`summonhelper finish`) calls it only where it sends nothing: without a
// marker, and when the bus cannot be reached, so Finish never runs; with a marker, Finish removes and logs.
func LogLiveRemoval(logf func(format string, args ...any), who string, removed bool, err error) {
	switch {
	case err != nil:
		logf("summonhelper: %s could not remove the live file: %v (%s)", who, err, leftLive)
	case removed:
		logf("summonhelper: %s removed the live file", who)
	}
}
