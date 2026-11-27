package camhelper

import (
	"bytes"
	"encoding/json"
	"errors"
	"log"
	"os"
	"path/filepath"
	"strconv"
	"syscall"
	"time"
)

// Marker files in the root-only state directory. Each is created BEFORE the
// car change it records and removed AFTER the change is undone, so a helper
// that dies in between leaves enough behind for Sweep (startup, runit finish
// hook) to clear exactly what it had set.
const (
	markRequestAudio = "ra"            // WEBCAM_webcamSessionRequestAudio was set to 1
	markSessionAct   = "sa"            // WEBCAM_webcamSessionActive was set to 1
	markSelectable   = "sel"           // GUI_selectableCameraRequest was set non-zero
	markGain         = "gain"          // pws-media-gain was changed by us
	childrenFile     = "children.json" // our running children (pid + argv)
	cutSubdir        = "cut"           // ffmpeg cut output (owned by the child uid)
	lockFile         = "lock"          // one helper (or sweep) at a time
)

type markers struct{ dir string }

func (m markers) path(name string) string { return filepath.Join(m.dir, name) }

func (m markers) set(name string) error {
	f, err := os.OpenFile(m.path(name), os.O_CREATE|os.O_WRONLY|syscall.O_NOFOLLOW, 0o600)
	if err != nil {
		return err
	}
	return f.Close()
}

func (m markers) has(name string) bool {
	_, err := os.Lstat(m.path(name))
	return err == nil
}

func (m markers) clear(name string) { os.Remove(m.path(name)) }

type childRec struct {
	Pid  int      `json:"pid"`
	Argv []string `json:"argv"`
}

func (m markers) saveChildren(recs []childRec) error {
	b, err := json.Marshal(recs)
	if err != nil {
		return err
	}
	tmp := m.path(childrenFile + ".tmp")
	os.Remove(tmp)
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_EXCL|os.O_WRONLY|syscall.O_NOFOLLOW, 0o600)
	if err != nil {
		return err
	}
	if _, err := f.Write(b); err != nil {
		f.Close()
		os.Remove(tmp)
		return err
	}
	f.Close()
	return os.Rename(tmp, m.path(childrenFile))
}

func (m markers) loadChildren() []childRec {
	b, err := os.ReadFile(m.path(childrenFile))
	if err != nil {
		return nil
	}
	var recs []childRec
	json.Unmarshal(b, &recs)
	return recs
}

// SweepConfig is what Sweep needs; ProcDir, Kill and Settle are test hooks.
type SweepConfig struct {
	Car      Car
	StateDir string
	ProcDir  string                                  // default /proc
	Kill     func(pid int, sig syscall.Signal) error // default syscall.Kill
	Settle   time.Duration                           // wait before the writes when a marker exists; default 1 s
	Log      *log.Logger
}

// Sweep clears whatever a previous helper run recorded, in the PLAN §4.9
// order: requestAudio 0, sessionActive 0, selectable 0, gain -200 (only when
// marked), our recorded children (only if /proc still shows exactly the
// recorded argv), then the cut files. It never touches anything it did not
// record. When a marker exists it first waits Settle (1 s), so a write still
// in flight from the dead helper lands before ours. A DataValue marker goes
// only after its 0 write AND a get-local-dv readback succeeded; otherwise it
// stays (the next helper retries it) and Sweep returns false. The caller
// holds the state lock.
func Sweep(c SweepConfig) bool {
	lg := c.Log
	if lg == nil {
		lg = log.New(os.Stderr, "camhelper: ", log.LstdFlags)
	}
	proc := c.ProcDir
	if proc == "" {
		proc = "/proc"
	}
	kill := c.Kill
	if kill == nil {
		kill = syscall.Kill
	}
	settle := c.Settle
	if settle == 0 {
		settle = time.Second
	}
	m := markers{c.StateDir}
	any := false
	for _, name := range latchMarkers {
		any = any || m.has(name)
	}
	clean := true
	if any {
		time.Sleep(settle)
	}
	for _, name := range latchMarkers {
		if !m.has(name) {
			continue
		}
		if clearCar(c.Car, name, lg) {
			lg.Printf("sweep: %s cleared", markerWhat(name))
			m.clear(name)
		} else {
			lg.Printf("sweep: LATCH NOT CLEARED: %s; the marker stays for the next helper", markerWhat(name))
			clean = false
		}
	}
	for _, r := range m.loadChildren() {
		if r.Pid <= 1 || len(r.Argv) == 0 {
			continue
		}
		cl, err := os.ReadFile(filepath.Join(proc, strconv.Itoa(r.Pid), "cmdline"))
		if err != nil {
			continue // gone
		}
		want := []byte{}
		for _, a := range r.Argv {
			want = append(append(want, a...), 0)
		}
		if !bytes.Equal(cl, want) {
			continue // a recycled pid: not ours
		}
		if err := kill(-r.Pid, syscall.SIGKILL); err != nil && !errors.Is(err, syscall.ESRCH) {
			lg.Printf("sweep: kill group %d: %v", r.Pid, err)
		} else {
			lg.Printf("sweep: killed stale child group %d (%s)", r.Pid, r.Argv[0])
		}
	}
	os.Remove(m.path(childrenFile))
	if ents, err := os.ReadDir(m.path(cutSubdir)); err == nil {
		for _, e := range ents {
			os.Remove(filepath.Join(m.path(cutSubdir), e.Name()))
		}
	}
	return clean
}
