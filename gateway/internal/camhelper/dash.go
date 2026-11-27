package camhelper

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"io"
	"io/fs"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"golang.org/x/sys/unix"

	"airgapp/gateway/camhelper/proto"
)

const (
	maxEventJSON  = 64 << 10 // event.json larger than this is treated as absent
	maxListFiles  = 50000    // listing stops after this many files
	listPageBytes = 256 << 10
	maxCutSeconds = 120
)

// within reports whether p is root or below it (both cleaned, absolute).
func within(root, p string) bool {
	return p == root || strings.HasPrefix(p, root+"/")
}

// resolveClip validates a client path and returns its resolved form. The
// string must be clean, absolute and under the TeslaCam root, and its
// realpath (symlinks resolved) must still be under the root's realpath.
func resolveClip(teslacam, p string) (string, string) {
	if p == "" || strings.ContainsRune(p, 0) || !filepath.IsAbs(p) || filepath.Clean(p) != p ||
		!strings.HasPrefix(p, teslacam+"/") {
		return "", proto.ErrForbidden
	}
	rootReal, err := filepath.EvalSymlinks(teslacam)
	if err != nil {
		return "", proto.ErrNotFound
	}
	real, err := filepath.EvalSymlinks(p)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return "", proto.ErrNotFound
		}
		return "", proto.ErrIO
	}
	if !within(rootReal, real) {
		return "", proto.ErrForbidden
	}
	return real, ""
}

// openRegular opens a resolved path read-only without following a final
// symlink and checks it is a regular file.
func openRegular(p string) (*os.File, int64, string) {
	f, err := os.OpenFile(p, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, 0, proto.ErrNotFound
		}
		if errors.Is(err, syscall.ELOOP) {
			return nil, 0, proto.ErrForbidden
		}
		return nil, 0, proto.ErrIO
	}
	st, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, 0, proto.ErrIO
	}
	if !st.Mode().IsRegular() {
		f.Close()
		return nil, 0, proto.ErrNotFound
	}
	return f, st.Size(), ""
}

// readAt reads up to n bytes at off; short at EOF, empty past it.
func readAt(f *os.File, size, off int64, n int) ([]byte, string) {
	if off >= size {
		return []byte{}, ""
	}
	if int64(n) > size-off {
		n = int(size - off)
	}
	b := make([]byte, n)
	k, err := f.ReadAt(b, off)
	if err != nil && !errors.Is(err, io.EOF) {
		return nil, proto.ErrIO
	}
	return b[:k], ""
}

// validCut checks the dash_cut window (as the laptop viewer did: ss >= 0,
// 0 < t <= 120).
func validCut(ss, t float64) bool {
	return !math.IsNaN(ss) && !math.IsInf(ss, 0) && ss >= 0 && ss <= 86400 &&
		!math.IsNaN(t) && t > 0 && t <= maxCutSeconds
}

// lister walks TeslaCam the way the laptop viewer's INVENTORY_CMD did:
// SentryClips/*/ and SavedClips/*/ (sorted, hidden names skipped) with their
// event.json and *.mp4 files, then RecentClips/*.mp4. Entries whose realpath
// leaves the root are skipped.
type lister struct {
	root, rootReal string
	files          int
}

func listTeslaCam(root string, page func(proto.ListPage) error) error {
	rootReal, err := filepath.EvalSymlinks(root)
	if err != nil {
		return nil // no USB drive: an empty listing, as the viewer gave
	}
	l := &lister{root: root, rootReal: rootReal}
	var cur proto.ListPage
	curBytes := 0
	flush := func() error {
		if len(cur.Dirs) == 0 && len(cur.Recent) == 0 {
			return nil
		}
		err := page(cur)
		cur, curBytes = proto.ListPage{}, 0
		return err
	}
	for _, typ := range []string{"SentryClips", "SavedClips"} {
		for _, name := range l.names(filepath.Join(root, typ)) {
			if l.files >= maxListFiles {
				break
			}
			dir := filepath.Join(root, typ, name)
			if st, ok := l.stat(dir); !ok || !st.IsDir() {
				continue
			}
			d := proto.DashDir{Type: typ, Folder: name, Files: []proto.DashFile{}}
			if b, ok := l.eventJSON(filepath.Join(dir, "event.json")); ok {
				d.HasEvent, d.Event = true, b
			}
			d.Files = l.mp4s(dir)
			sz := 200 + len(d.Folder) + len(d.Event)*4/3
			for _, f := range d.Files {
				sz += 40 + len(f.Name)
			}
			if curBytes+sz > listPageBytes {
				if err := flush(); err != nil {
					return err
				}
			}
			cur.Dirs = append(cur.Dirs, d)
			curBytes += sz
		}
	}
	for _, f := range l.mp4s(filepath.Join(root, "RecentClips")) {
		sz := 40 + len(f.Name)
		if curBytes+sz > listPageBytes {
			if err := flush(); err != nil {
				return err
			}
		}
		cur.Recent = append(cur.Recent, f)
		curBytes += sz
	}
	return flush()
}

// names returns the non-hidden entry names of dir, sorted bytewise.
func (l *lister) names(dir string) []string {
	ents, err := os.ReadDir(dir) // sorted by name
	if err != nil {
		return nil
	}
	var out []string
	for _, e := range ents {
		if !strings.HasPrefix(e.Name(), ".") {
			out = append(out, e.Name())
		}
	}
	return out
}

// stat follows symlinks but refuses anything resolving outside the root.
func (l *lister) stat(p string) (fs.FileInfo, bool) {
	real, err := filepath.EvalSymlinks(p)
	if err != nil || !within(l.rootReal, real) {
		return nil, false
	}
	st, err := os.Stat(real)
	return st, err == nil
}

func (l *lister) eventJSON(p string) ([]byte, bool) {
	st, ok := l.stat(p)
	if !ok || !st.Mode().IsRegular() || st.Size() > maxEventJSON {
		return nil, false
	}
	real, _ := filepath.EvalSymlinks(p)
	f, _, code := openRegular(real)
	if code != "" {
		return nil, false
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, maxEventJSON+1))
	if err != nil || len(b) > maxEventJSON {
		return nil, false
	}
	return b, true
}

func (l *lister) mp4s(dir string) []proto.DashFile {
	out := []proto.DashFile{}
	for _, name := range l.names(dir) {
		if !strings.HasSuffix(name, ".mp4") || l.files >= maxListFiles {
			continue
		}
		st, ok := l.stat(filepath.Join(dir, name))
		if !ok || !st.Mode().IsRegular() {
			continue
		}
		out = append(out, proto.DashFile{Name: name, Size: st.Size()})
		l.files++
	}
	return out
}

// ---- dash_read / dash_cut ----

type cutFile struct {
	key, token, path string
	size             int64
}

// freeBytes is the space available to an unprivileged writer in dir.
func freeBytes(dir string) (int64, error) {
	var st unix.Statfs_t
	if err := unix.Statfs(dir, &st); err != nil {
		return 0, err
	}
	return int64(st.Bavail) * int64(st.Bsize), nil
}

func (s *Server) dashRead(sess *session, req proto.Request) ([]byte, int64, string) {
	if req.Offset < 0 || req.Length <= 0 || req.Length > proto.MaxRead {
		return nil, 0, proto.ErrBadRequest
	}
	var p string
	if req.Cut != "" {
		s.mu.Lock()
		for _, ct := range sess.cuts {
			if ct.token == req.Cut {
				p = ct.path
			}
		}
		s.mu.Unlock()
		if p == "" {
			return nil, 0, proto.ErrNotFound
		}
	} else {
		real, code := resolveClip(s.cfg.TeslaCam, req.Path)
		if code != "" {
			return nil, 0, code
		}
		p = real
	}
	f, size, code := openRegular(p)
	if code != "" {
		return nil, 0, code
	}
	defer f.Close()
	data, code := readAt(f, size, req.Offset, req.Length)
	return data, size, code
}

const minCutRoom = 1 << 20

// dashCut cuts [ss, ss+t] of a clip into the cut dir (tmpfs). The output is
// bounded by what is left of the cut budget (ffmpeg -fs); with less room than
// that budget on the filesystem, or less than 1 MiB of budget, it is busy.
func (s *Server) dashCut(sess *session, req proto.Request) (string, int64, string) {
	if !validCut(req.SS, req.T) {
		return "", 0, proto.ErrBadRequest
	}
	real, code := resolveClip(s.cfg.TeslaCam, req.Path)
	if code != "" {
		return "", 0, code
	}
	if f, _, code := openRegular(real); code != "" {
		return "", 0, code
	} else {
		f.Close()
	}
	key := real + "|" + strconv.FormatFloat(req.SS, 'f', 2, 64) + "|" + strconv.FormatFloat(req.T, 'f', 2, 64)
	dir := filepath.Join(s.cfg.StateDir, cutSubdir)
	s.mu.Lock()
	for i, ct := range sess.cuts {
		if ct.key == key { // reuse; move to the back (most recent)
			sess.cuts = append(append(sess.cuts[:i:i], sess.cuts[i+1:]...), ct)
			s.mu.Unlock()
			return ct.token, ct.size, ""
		}
	}
	for len(sess.cuts) >= s.cfg.MaxCuts { // make room by count first
		old := sess.cuts[0]
		sess.cuts = sess.cuts[1:]
		s.cutBytes -= old.size
		os.Remove(old.path)
	}
	limit := s.cfg.CutBudget - s.cutBytes
	free, ferr := s.cfg.FreeBytes(dir)
	if limit < minCutRoom || ferr != nil || free < limit {
		s.mu.Unlock()
		s.log.Printf("cut refused: budget left %d, free %d (%v)", limit, free, ferr)
		return "", 0, proto.ErrBusy
	}
	var rnd [12]byte
	rand.Read(rnd[:])
	token := hex.EncodeToString(rnd[:])
	out := filepath.Join(dir, token+".mp4")
	c, err := s.startChildLocked(ChildSpec{Argv: cutArgv(real, out, req.SS, req.T, limit), Env: cutEnv})
	if err != nil {
		s.mu.Unlock()
		s.log.Printf("cut: %v", err)
		if err == errTooManyChildren {
			return "", 0, proto.ErrBusy
		}
		return "", 0, proto.ErrCutFailed
	}
	sess.cutChildren = append(sess.cutChildren, c)
	s.cutBytes += limit // reserved until the size is known
	s.mu.Unlock()

	ok := false
	select {
	case <-c.Done():
		ok = c.ExitErr() == nil
		if !ok {
			s.log.Printf("cut %s: %v", filepath.Base(real), c.ExitErr())
		}
	case <-time.After(s.cfg.CutTimeout):
		s.log.Printf("cut %s: timed out", filepath.Base(real))
		c.Stop()
	case <-sess.quit:
		c.Stop()
	}
	var size int64
	if ok {
		f, sz, code := openRegular(out)
		switch {
		case code != "" || sz == 0:
			ok = false
		case sz >= limit:
			s.log.Printf("cut %s: hit the %d-byte budget (truncated)", filepath.Base(real), limit)
			ok = false
		default:
			size = sz
		}
		if f != nil {
			f.Close()
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.cutBytes -= limit
	for i, x := range sess.cutChildren {
		if x == c {
			sess.cutChildren = append(sess.cutChildren[:i:i], sess.cutChildren[i+1:]...)
			break
		}
	}
	if !ok {
		os.Remove(out)
		return "", 0, proto.ErrCutFailed
	}
	sess.cuts = append(sess.cuts, &cutFile{key: key, token: token, path: out, size: size})
	s.cutBytes += size
	return token, size, ""
}
