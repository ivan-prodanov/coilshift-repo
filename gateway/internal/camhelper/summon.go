package camhelper

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"syscall"

	"airgapp/gateway/camhelper/proto"
)

// DefaultSummonLive is summonhelper's live file (internal/summon/helper), there only while a Summon session is up
// whose phone opted into the presence lease. summonhelper holds an exclusive flock on it from before it is visible
// until it is unlinked; the kernel drops that lock when the process dies, so a dead summonhelper's file proves
// nothing. (The session marker is no use here: it stays after an unclean end, for the takeover's abort.)
const DefaultSummonLive = proto.SummonLivePath

// DefaultSummonUID is summonhelper's uid on the car (user webrtc): the only owner the live file and its directory
// may have.
const DefaultSummonUID = 1337

// summonLive reports whether the live file at path proves a live Summon session, and the pid written in it (for the
// log only). SUMMON mode waives the gear, Sentry and camera-access gates, so agw's word alone is not enough. Live
// means all of:
//   - the directory is a real directory (not a symlink) owned by summonUID and not group- or other-writable (the
//     installer makes /run/airgapp-summon 1337:4950 2750: agw can read it, never write it);
//   - the file, opened O_NOFOLLOW|O_NONBLOCK, is a regular file owned by summonUID and not group- or
//     other-writable;
//   - another open file description holds a lock on it: flock(LOCK_SH|LOCK_NB) fails with EWOULDBLOCK. Only
//     summonhelper, which keeps its LOCK_EX until it unlinks the file, can hold one: agw cannot open a 0600 file
//     owned by 1337. A probe that gets the lock releases it at once and counts as no session.
//
// Anything that cannot be checked counts as no session.
func summonLive(path string, summonUID int) (bool, string) {
	di, err := os.Lstat(filepath.Dir(path))
	if err != nil || !di.IsDir() || !ownedBy(di, summonUID) {
		return false, ""
	}
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK|syscall.O_CLOEXEC, 0)
	if err != nil {
		return false, ""
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil || !fi.Mode().IsRegular() || !ownedBy(fi, summonUID) {
		return false, ""
	}
	fd := int(f.Fd())
	if err := syscall.Flock(fd, syscall.LOCK_SH|syscall.LOCK_NB); err == nil {
		syscall.Flock(fd, syscall.LOCK_UN) // nobody holds it: a stale file
		return false, ""
	} else if !errors.Is(err, syscall.EWOULDBLOCK) {
		return false, ""
	}
	buf := make([]byte, 24)
	n, _ := f.Read(buf)
	return true, strings.TrimSpace(string(buf[:n]))
}

// ownedBy reports whether fi is owned by uid and not writable by group or others.
func ownedBy(fi os.FileInfo, uid int) bool {
	st, ok := fi.Sys().(*syscall.Stat_t)
	return ok && int(st.Uid) == uid && fi.Mode().Perm()&0o022 == 0
}

// summonSel is Tesla's summon-webcam camera for the gear (WebcamCommandHandler::checkWebcamAllowed for a summon
// session, 2026.38.6), the same mapping as CAMERA's: Backup (9) in R, the front fisheye (3) in D, Grid (11)
// otherwise. ok is false while the gear is unknown: any of the three is accepted then (CAMERA keeps its last known
// gear across an unreadable one).
func summonSel(gear string) (sel int, ok bool) {
	switch gear {
	case "R":
		return 9, true
	case "D":
		return 3, true
	case "":
		return 0, false
	}
	return 11, true
}

// summonOnlySel reports whether v is a selectable only SUMMON mode may hold: Tesla's summon webcam picks 11 (Grid),
// 9 (Backup) or, in D, 3.
func summonOnlySel(v int) bool { return v == 9 || v == 11 }
