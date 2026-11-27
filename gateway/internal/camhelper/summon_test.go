package camhelper

import (
	"io/fs"
	"os"
	"path/filepath"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"airgapp/gateway/camhelper/proto"
)

var (
	sel9  = Selectable9.String()
	sel11 = Selectable11.String()
)

// summonRig is a rig whose "live Summon session on the car" the test sets.
func summonRig(t *testing.T, opts ...func(*Config)) (*rig, *atomic.Bool) {
	t.Helper()
	live := &atomic.Bool{}
	r := newRig(t, append([]func(*Config){func(c *Config) { c.SummonOK = func() (bool, string) { return live.Load(), "4242" } }}, opts...)...)
	return r, live
}

// holdLock opens path and takes LOCK_EX on its own open file description, as summonhelper does; the returned func
// releases it.
func holdLock(t *testing.T, path string) func() {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		t.Fatal(err)
	}
	return func() { f.Close() }
}

// summonhelper's half of the contract: the live file counts only while another open file description holds a lock
// on it, and only if it and its directory are the summon uid's and not group/other-writable (review L1, live-7).
func TestSummonLive(t *testing.T) {
	root, _ := filepath.EvalSymlinks(t.TempDir())
	dir := filepath.Join(root, "airgapp-summon")
	os.Mkdir(dir, 0o750)
	live := filepath.Join(dir, "live")
	me := os.Getuid()
	check := func() bool { ok, _ := summonLive(live, me); return ok }
	if check() {
		t.Fatal("no live file yet")
	}
	os.WriteFile(live, []byte("4242\n"), 0o600)
	if check() {
		t.Fatal("a file nobody holds a lock on: stale")
	}
	release := holdLock(t, live)
	if ok, who := summonLive(live, me); !ok || who != "4242" {
		t.Fatalf("a locked live file: %v %q", ok, who)
	}
	if ok, _ := summonLive(live, me+1000); ok {
		t.Fatal("owned by another uid than the summon uid")
	}
	os.Chmod(live, 0o620)
	if check() {
		t.Fatal("a group-writable live file")
	}
	os.Chmod(live, 0o600)
	os.Chmod(dir, 0o770)
	if check() {
		t.Fatal("a group-writable dir")
	}
	os.Chmod(dir, 0o750)
	if !check() {
		t.Fatal("restored")
	}
	release()
	if check() {
		t.Fatal("the lock holder is gone")
	}
	// The probe that found no holder released its own lock: summonhelper can take LOCK_EX right after.
	f, _ := os.Open(live)
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		t.Fatalf("the probe kept a lock: %v", err)
	}
	f.Close()

	// Never through a symlink: the file or the directory.
	os.Remove(live)
	other := filepath.Join(root, "elsewhere")
	os.WriteFile(other, []byte("4242"), 0o600)
	defer holdLock(t, other)()
	os.Symlink(other, live)
	if check() {
		t.Fatal("a symlink live file")
	}
	os.Remove(live)
	os.WriteFile(live, []byte("4242"), 0o600)
	defer holdLock(t, live)()
	os.Symlink(dir, filepath.Join(root, "linked"))
	if ok, _ := summonLive(filepath.Join(root, "linked", "live"), me); ok {
		t.Fatal("a live file under a symlinked dir")
	}
	// A FIFO is not a regular file (and the open never blocks).
	fifo := filepath.Join(dir, "fifo")
	if err := syscall.Mkfifo(fifo, 0o600); err == nil {
		if ok, _ := summonLive(fifo, me); ok {
			t.Fatal("a FIFO")
		}
	}
}

type fakeInfo struct {
	mode fs.FileMode
	uid  uint32
}

func (f fakeInfo) Name() string       { return "live" }
func (f fakeInfo) Size() int64        { return 0 }
func (f fakeInfo) Mode() fs.FileMode  { return f.mode }
func (f fakeInfo) ModTime() time.Time { return time.Time{} }
func (f fakeInfo) IsDir() bool        { return f.mode.IsDir() }
func (f fakeInfo) Sys() any           { return &syscall.Stat_t{Uid: f.uid} }

// The owner and mode rule on its own, for an owner the test cannot create (review T4).
func TestSummonOwnedBy(t *testing.T) {
	for _, c := range []struct {
		fi   fakeInfo
		want bool
	}{
		{fakeInfo{0o600, 1337}, true},
		{fakeInfo{0o640, 1337}, true},
		{fakeInfo{0o600, 4950}, false},
		{fakeInfo{0o600, 0}, false},
		{fakeInfo{0o620, 1337}, false},
		{fakeInfo{0o602, 1337}, false},
		{fakeInfo{fs.ModeDir | 0o750, 1337}, true},
		{fakeInfo{fs.ModeDir | 0o770, 1337}, false},
	} {
		if got := ownedBy(c.fi, 1337); got != c.want {
			t.Errorf("%v owner %d: %v", c.fi.mode, c.fi.uid, got)
		}
	}
}

// The production default (no SummonOK injected) keys on the live file: refused without it, granted while its lock
// is held (review T3).
func TestSummonDefaultCheckUsesTheLiveFile(t *testing.T) {
	dir, _ := filepath.EvalSymlinks(t.TempDir())
	live := filepath.Join(dir, "live")
	r := newRig(t, func(c *Config) { c.SummonOK, c.SummonLive, c.SummonUID = nil, live, os.Getuid() })
	c := r.dial()
	_, err := c.Summon(ctx(), true)
	expectCode(t, err, proto.ErrNoSummon)
	os.WriteFile(live, []byte("4242\n"), 0o600)
	_, err = c.Summon(ctx(), true)
	expectCode(t, err, proto.ErrNoSummon) // nobody holds its lock
	defer holdLock(t, live)()
	if st, err := c.Summon(ctx(), true); err != nil || !st.Summon {
		t.Fatalf("locked live file: %+v %v", st, err)
	}
}

func TestSummonModeNeedsALiveSummonSession(t *testing.T) {
	r, live := summonRig(t)
	a, b := r.dial(), r.dial()
	st, err := a.Summon(ctx(), true)
	expectCode(t, err, proto.ErrNoSummon)
	if st.Summon {
		t.Fatal("refused, yet in SUMMON mode")
	}
	live.Store(true)
	if st, err = a.Summon(ctx(), true); err != nil || !st.Summon {
		t.Fatalf("summon on: %+v %v", st, err)
	}
	if _, err := a.Summon(ctx(), true); err != nil {
		t.Fatalf("summon on twice: %v", err)
	}
	_, err = b.Summon(ctx(), true)
	expectCode(t, err, proto.ErrBusy)
	// Closing the session ends its SUMMON mode: another can take it.
	a.Close()
	waitFor(t, "the mode freed", func() bool {
		_, err := b.Summon(ctx(), true)
		return err == nil
	})
	r.waitOps()
}

// Tesla's webcam handler skips checkWebcamAllowed for a summon session: the
// hold and the summon selectable are granted and kept in any gear, with Sentry
// off and camera access off. Talk never runs during SUMMON.
func TestSummonModeWaivesTheGates(t *testing.T) {
	r, live := summonRig(t)
	r.car.set(ReadGear, "(true, 'D')")
	r.car.set(ReadSentry, "(true, 'Off')")
	r.car.set(ReadCamAccess, "(true, 'false')")
	c := r.dial()
	_, err := c.Hold(ctx())
	expectCode(t, err, proto.ErrGateGear)
	live.Store(true)
	if _, err := c.Summon(ctx(), true); err != nil {
		t.Fatal(err)
	}
	st, err := c.Hold(ctx())
	if err != nil || !st.Held || st.Gate != "" || !st.Summon {
		t.Fatalf("hold in SUMMON mode: %+v %v", st, err)
	}
	if err := selc(c, 3); err != nil { // D: the front fisheye
		t.Fatal(err)
	}
	r.waitOps(sa1, sel3)
	for i := 0; i < 3; i++ { // the gates would end everything at the second tick
		r.srv.tick()
		r.waitOps(sa1, sel3)
	}
	select {
	case e := <-c.Events():
		if e.Ev != proto.EvStatus {
			t.Fatalf("event %+v in SUMMON mode", e)
		}
	default:
	}
	_, err = c.TalkArm(ctx())
	expectCode(t, err, proto.ErrSummon)
}

// Hold re-checks the live signal itself, not only at the tick (review T3).
func TestSummonHoldRechecksTheLiveSignal(t *testing.T) {
	r, live := summonRig(t)
	live.Store(true)
	c := r.dial()
	c.Summon(ctx(), true)
	r.car.set(ReadGear, "(true, 'D')")
	live.Store(false)
	_, err := c.Hold(ctx())
	expectCode(t, err, proto.ErrGateGear)
	r.waitOps()
}

// In SUMMON mode only Tesla's camera for the gear is accepted: Grid in P, N or
// an unknown gear, Backup in R, the front fisheye in D; any of the three while
// the gear cannot be read (review live-5).
func TestSummonSelectableValues(t *testing.T) {
	r, live := summonRig(t)
	c := r.dial()
	expectCode(t, selc(c, 11), proto.ErrSelRefused) // P, but no SUMMON mode
	expectCode(t, selc(c, 9), proto.ErrSelRefused)
	expectCode(t, selc(c, 7), proto.ErrBadRequest)
	live.Store(true)
	c.Summon(ctx(), true)
	expectCode(t, selc(c, 9), proto.ErrSelRefused) // P: Grid only
	expectCode(t, selc(c, 3), proto.ErrSelRefused)
	if err := selc(c, 11); err != nil {
		t.Fatal(err)
	}
	r.car.set(ReadGear, "(true, 'R')")
	selc(c, 0)
	expectCode(t, selc(c, 11), proto.ErrSelRefused) // R: Backup only
	if err := selc(c, 9); err != nil {
		t.Fatal(err)
	}
	expectCode(t, selc(c, 1), proto.ErrSelRefused) // interior stays parked-only
	r.car.set(ReadGear, "(false, '')")             // unreadable: any summon camera
	selc(c, 0)
	if err := selc(c, 3); err != nil {
		t.Fatal(err)
	}
	r.waitOps(sel11, sel0, sel9, sel0, sel3)
	// The live signal is checked at the request too, not only by the watcher.
	live.Store(false)
	selc(c, 0)
	expectCode(t, selc(c, 11), proto.ErrSelRefused)
	r.waitOps(sel0)
}

// A summon selectable that does not match the gear on two consecutive ticks is
// ended; one that CAMERA switches between them is kept (review live-5).
func TestSummonSelectableFollowsTheGearOrEnds(t *testing.T) {
	r, live := summonRig(t)
	live.Store(true)
	c := r.dial()
	c.Summon(ctx(), true)
	c.Hold(ctx())
	selc(c, 11)
	r.car.set(ReadGear, "(true, 'D')")
	r.srv.tick() // one mismatch
	selc(c, 0)   // CAMERA follows within the status event
	selc(c, 3)
	r.srv.tick()
	r.srv.tick()
	if st, _ := c.Status(ctx()); st.Sel != 3 {
		t.Fatalf("a switched selectable was ended: %+v", st)
	}
	r.car.set(ReadGear, "(true, 'R')") // agw does not follow
	r.srv.tick()
	if st, _ := c.Status(ctx()); st.Sel != 3 {
		t.Fatalf("ended after one mismatch: %+v", st)
	}
	r.srv.tick()
	if e := nextEvent(t, c, proto.EvSelEnd); e.Reason != proto.ErrSelRefused {
		t.Fatalf("sel_end %+v", e)
	}
	if st, _ := c.Status(ctx()); st.Sel != 0 || !st.Held || !st.Summon {
		t.Fatalf("after the end: %+v", st)
	}
}

func TestSummonOffReleasesTheSelectable(t *testing.T) {
	r, live := summonRig(t)
	live.Store(true)
	c := r.dial()
	c.Summon(ctx(), true)
	c.Hold(ctx())
	selc(c, 11)
	r.waitOps(sa1, sel11)
	st, err := c.Summon(ctx(), false)
	if err != nil || st.Summon || st.Sel != 0 || !st.Held {
		t.Fatalf("summon off: %+v %v", st, err)
	}
	r.waitOps(sel0)
	expectCode(t, selc(c, 11), proto.ErrSelRefused)
	if len(r.markers()) != 1 || r.markers()[0] != markSessionAct {
		t.Fatalf("markers %v", r.markers())
	}
}

// summon on ends the session's talk: talk never runs during SUMMON (review
// live-4). The hold stays, so the exterior stream is un-routed by an edge.
func TestSummonOnEndsTheSessionsTalk(t *testing.T) {
	r, live := summonRig(t)
	r.car.setLinks(feedersOK)
	c := r.dial()
	c.Hold(ctx())
	if _, err := c.TalkArm(ctx()); err != nil {
		t.Fatal(err)
	}
	r.car.take()
	live.Store(true)
	if st, err := c.Summon(ctx(), true); err != nil || st.Talk || !st.Held {
		t.Fatalf("summon on: %+v %v", st, err)
	}
	if e := nextEvent(t, c, proto.EvTalkEnd); e.Reason != proto.ErrSummon {
		t.Fatalf("talk_end %+v", e)
	}
	r.waitOps(ra0, sa0, sa1)
}

// The car's Summon session ends (summonhelper released the live file): the
// mode ends at the next tick, the summon selectable is released, and the
// normal gates apply again in the same tick (two non-P readings end the hold).
func TestSummonModeEndsWithTheLiveFile(t *testing.T) {
	r, live := summonRig(t)
	live.Store(true)
	c := r.dial()
	c.Summon(ctx(), true)
	c.Hold(ctx())
	r.car.set(ReadGear, "(true, 'D')")
	selc(c, 3)
	r.waitOps(sa1, sel3)
	r.srv.tick() // first non-P reading, waived
	r.waitOps(sa1, sel3)
	live.Store(false)
	r.srv.tick()
	if e := nextEvent(t, c, proto.EvSummonEnd); e.Reason != "live" {
		t.Fatalf("summon_end %+v", e)
	}
	nextEvent(t, c, proto.EvSelEnd)
	if e := nextEvent(t, c, proto.EvHoldEnd); e.Reason != proto.ErrGateGear {
		t.Fatalf("hold_end %+v", e)
	}
	r.waitOps(sel0, sa0)
	if len(r.markers()) != 0 {
		t.Fatalf("markers %v", r.markers())
	}
	st, _ := c.Status(ctx())
	if st.Summon || st.Held || st.Sel != 0 {
		t.Fatalf("status %+v", st)
	}
}

// The watcher ends the mode within SummonPoll of the live file going, with no
// tick at all, whose car reads can take seconds (review live-3).
func TestSummonWatcherEndsTheModeWithoutTheTick(t *testing.T) {
	r, live := summonRig(t, func(c *Config) { c.SummonPoll = 10 * time.Millisecond })
	live.Store(true)
	c := r.dial()
	c.Summon(ctx(), true)
	c.Hold(ctx())
	selc(c, 11)
	r.waitOps(sa1, sel11)
	live.Store(false)
	start := time.Now()
	if e := nextEvent(t, c, proto.EvSummonEnd); e.Reason != "live" {
		t.Fatalf("summon_end %+v", e)
	}
	if d := time.Since(start); d > time.Second {
		t.Fatalf("ended after %v", d)
	}
	nextEvent(t, c, proto.EvSelEnd)
	r.waitOps(sel0)
	if st, _ := c.Status(ctx()); st.Summon || st.Sel != 0 || !st.Held {
		t.Fatalf("status %+v", st)
	}
}

// SUMMON mode waives only the SUMMON session's own hold and selectable: another
// session's talk keeps its strict gate, and its hold and selectable end as
// usual (review of ccd4428 finding 2; review T6).
func TestSummonModeKeepsOtherSessionsGates(t *testing.T) {
	t.Run("sentry off", func(t *testing.T) {
		r, live := summonRig(t)
		live.Store(true)
		r.car.setLinks(feedersOK)
		a, b := r.dial(), r.dial()
		a.Summon(ctx(), true)
		a.Hold(ctx())
		b.Hold(ctx())
		if _, err := b.TalkArm(ctx()); err != nil { // parked, every gate passes
			t.Fatal(err)
		}
		r.car.set(ReadSentry, "(true, 'Off')")
		r.car.take()
		r.srv.tick()
		r.srv.tick()
		if e := nextEvent(t, b, proto.EvTalkEnd); e.Reason != proto.ErrGateSentry {
			t.Fatalf("b's talk_end %+v", e)
		}
		if e := nextEvent(t, b, proto.EvHoldEnd); e.Reason != proto.ErrGateSentry {
			t.Fatalf("b's hold_end %+v", e)
		}
		if st, _ := a.Status(ctx()); !st.Held || !st.Summon {
			t.Fatalf("a: %+v", st)
		}
		if st, _ := b.Status(ctx()); st.Held || st.Talk {
			t.Fatalf("b: %+v", st)
		}
	})
	t.Run("sentry off, another session's selectable", func(t *testing.T) {
		r, live := summonRig(t)
		live.Store(true)
		a, b := r.dial(), r.dial()
		a.Summon(ctx(), true)
		a.Hold(ctx())
		b.Hold(ctx())
		selc(b, 3)
		r.car.set(ReadSentry, "(true, 'Off')")
		r.car.take()
		r.srv.tick()
		r.srv.tick()
		nextEvent(t, b, proto.EvSelEnd)
		nextEvent(t, b, proto.EvHoldEnd)
		if st, _ := a.Status(ctx()); !st.Held {
			t.Fatalf("a lost its hold: %+v", st)
		}
		for _, o := range r.car.snapshot() {
			if o == sa0 {
				t.Fatalf("sessionActive dropped while a holds: %v", r.car.snapshot())
			}
		}
	})
	t.Run("D, the SUMMON session without a hold", func(t *testing.T) {
		r, live := summonRig(t)
		live.Store(true)
		a, b := r.dial(), r.dial()
		a.Summon(ctx(), true)
		b.Hold(ctx())
		selc(b, 3)
		r.car.set(ReadGear, "(true, 'D')")
		r.car.take()
		r.srv.tick()
		nextEvent(t, b, proto.EvSelEnd)
		for _, o := range r.car.snapshot() {
			if o == sa1 {
				t.Fatalf("sessionActive re-asserted in D for a non-SUMMON holder: %v", r.car.snapshot())
			}
		}
		r.srv.tick()
		if e := nextEvent(t, b, proto.EvHoldEnd); e.Reason != proto.ErrGateGear {
			t.Fatalf("b's hold_end %+v", e)
		}
	})
}

// A SUMMON grant that lands while the tick reads the car is not ended by that
// tick (it was checked at its grant): no summon_end, and the summon selectable
// taken right after it is kept (review of ccd4428 finding 3; review T10).
func TestSummonGrantDuringTheTicksReadIsKept(t *testing.T) {
	r, live := summonRig(t)
	live.Store(true)
	a := r.dial()
	a.Hold(ctx())         // busy: the tick reads the car
	var fired atomic.Bool // not sync.Once: sel's own car read re-enters the hook
	r.car.mu.Lock()
	r.car.onRead = func() {
		if !fired.CompareAndSwap(false, true) {
			return
		}
		if _, err := a.Summon(ctx(), true); err != nil {
			t.Errorf("summon on: %v", err)
		}
		if err := selc(a, 11); err != nil {
			t.Errorf("sel 11: %v", err)
		}
	}
	r.car.mu.Unlock()
	r.srv.tick()
	st, _ := a.Status(ctx())
	if !st.Summon || st.Sel != 11 {
		t.Fatalf("the grant was ended by the tick: %+v", st)
	}
	for {
		select {
		case e := <-a.Events():
			if e.Ev == proto.EvSummonEnd || e.Ev == proto.EvSelEnd {
				t.Fatalf("%+v", e)
			}
			continue
		default:
		}
		break
	}
}
