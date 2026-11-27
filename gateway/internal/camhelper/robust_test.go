package camhelper

import (
	"errors"
	"io"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"airgapp/gateway/camhelper/proto"
)

func fastBackoff(c *Config) { c.Backoff = func(int) time.Duration { return time.Millisecond } }

// settled waits until no clear of name is queued or running.
func (r *rig) settled(name string) {
	r.t.Helper()
	waitFor(r.t, "clear of "+name+" finished", r.cond(func(s *Server) bool {
		l := s.latch[name]
		return l == nil || !l.queued
	}))
	time.Sleep(2 * time.Millisecond) // past the 1 ms backoff
}

// H1: a 0 write whose readback still shows 1 keeps the marker; the tick
// retries the orphan until the readback confirms it.
func TestLatchKeptUntilReadbackConfirms(t *testing.T) {
	r := newRig(t, fastBackoff)
	c := r.dial()
	c.Hold(ctx())
	r.waitOps(sa1)
	r.car.mu.Lock()
	r.car.stuck[ReadSessionActive] = true
	r.car.mu.Unlock()
	c.Close()
	r.waitOps(sa0)
	r.settled(markSessionAct)
	if !(markers{r.state}).has(markSessionAct) {
		t.Fatal("marker removed although the readback still shows sessionActive 1")
	}
	r.srv.tick() // the orphan is retried
	r.waitOps(sa0)
	r.settled(markSessionAct)
	if !(markers{r.state}).has(markSessionAct) {
		t.Fatal("marker removed on a failed retry")
	}
	r.car.mu.Lock()
	r.car.stuck[ReadSessionActive] = false
	r.car.mu.Unlock()
	r.srv.tick()
	r.waitOps(sa0)
	waitFor(t, "marker cleared", func() bool { return !(markers{r.state}).has(markSessionAct) })
}

func TestFailedZeroWriteIsRetried(t *testing.T) {
	r := newRig(t, fastBackoff)
	c := r.dial()
	c.Sel(ctx(), 3)
	r.waitOps(sel3)
	r.car.mu.Lock()
	r.car.failW[Selectable0] = 2
	r.car.mu.Unlock()
	c.Close()
	waitFor(t, "first attempt", func() bool { r.car.mu.Lock(); defer r.car.mu.Unlock(); return r.car.writeHit[Selectable0] == 1 })
	for i := 0; i < 50 && (markers{r.state}).has(markSelectable); i++ {
		time.Sleep(5 * time.Millisecond)
		r.srv.tick()
	}
	waitFor(t, "cleared on the third attempt", func() bool { return !(markers{r.state}).has(markSelectable) })
	r.car.mu.Lock()
	hits := r.car.writeHit[Selectable0]
	r.car.mu.Unlock()
	if hits != 3 {
		t.Fatalf("%d selectable 0 writes", hits)
	}
}

// An orphan marker left by a previous run (the sweep could not clear it) is
// retried by the running helper's tick even when nothing is held.
func TestOrphanMarkerFromAnEarlierRunIsRetried(t *testing.T) {
	r := newRig(t, fastBackoff)
	(markers{r.state}).set(markRequestAudio)
	r.srv.tick()
	r.waitOps(ra0)
	waitFor(t, "cleared", func() bool { return !(markers{r.state}).has(markRequestAudio) })
}

// M1: no marker, no write: the request is refused.
func TestMarkerFailureRefusesTheOperation(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("root ignores directory permissions")
	}
	r := newRig(t)
	c := r.dial()
	os.Chmod(r.state, 0o500)
	defer os.Chmod(r.state, 0o700)
	_, err := c.Hold(ctx())
	expectCode(t, err, proto.ErrInternal)
	_, err = c.Sel(ctx(), 1)
	expectCode(t, err, proto.ErrInternal)
	r.waitOps() // nothing written
	st, _ := c.Status(ctx())
	if st.Held || st.Sel != 0 {
		t.Fatalf("state changed without a marker: %+v", st)
	}
}

// M2: a 3 s car call never stalls PCM: the client's 1 s write deadline holds
// while another session waits on the slow car.
func TestSlowCarDoesNotStallPCM(t *testing.T) {
	r := newRig(t)
	talker, other := r.dial(), r.dial()
	talker.Hold(ctx())
	if _, err := talker.TalkArm(ctx()); err != nil {
		t.Fatal(err)
	}
	w := r.sp.find(t, "pw-cat")
	r.car.mu.Lock()
	r.car.delay = 3 * time.Second
	r.car.mu.Unlock()
	go other.Sel(ctx(), 1) // queues a 3 s car write
	time.Sleep(50 * time.Millisecond)
	start := w.stdinBytes.Load()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if err := talker.WritePCM(make([]byte, 1920)); err != nil {
			t.Fatalf("PCM write failed during a slow car call: %v", err)
		}
		time.Sleep(20 * time.Millisecond)
	}
	if got := w.stdinBytes.Load() - start; got < 30*1920 {
		t.Fatalf("only %d PCM bytes reached the writer during a slow car call", got)
	}
	r.car.mu.Lock()
	r.car.delay = 0
	r.car.mu.Unlock()
}

// M3: a writer that cannot be restarted ends talk after WriterRestarts tries.
func TestWriterRestartFailuresEndTalk(t *testing.T) {
	r := newRig(t, fastBackoff)
	c := r.dial()
	c.Hold(ctx())
	c.TalkArm(ctx())
	w := r.sp.find(t, "pw-cat")
	r.sp.mu.Lock()
	r.sp.fail = true
	r.sp.mu.Unlock()
	w.exit(errors.New("pw-cat: connection refused"))
	r.srv.mu.Lock()
	r.srv.talk.restartAt = time.Time{}
	r.srv.mu.Unlock()
	r.car.take()
	for i := 0; i < WriterRestarts+2; i++ {
		r.srv.tick()
		time.Sleep(3 * time.Millisecond)
	}
	if e := nextEvent(t, c, proto.EvTalkEnd); e.Reason != proto.ErrSpawn {
		t.Fatalf("talk_end %+v", e)
	}
	st, _ := c.Status(ctx())
	if st.Talk || !st.Held {
		t.Fatalf("status %+v", st)
	}
	if !strings.Contains(strings.Join(r.car.snapshot(), ","), ra0) {
		t.Fatal("requestAudio not cleared after the failed restarts")
	}
}

// M4: overlapping sessions share one capture child per port.
func TestCapturesAreSharedBetweenSessions(t *testing.T) {
	r := newRig(t)
	a, b := r.dial(), r.dial()
	for _, c := range []*conn{a, b} {
		for _, p := range []int{9900, 9901} {
			if err := c.CaptureStart(ctx(), p); err != nil {
				t.Fatal(err)
			}
		}
	}
	if n := len(r.sp.all()); n != 2 {
		t.Fatalf("%d children for two ports", n)
	}
	ch := r.sp.find(t, "port=9900")
	go ch.outW.Write(annexB([]byte{0x67, 1}, []byte{0x68, 1}, []byte{0x65, 0x88}, []byte{0x41, 0x9A}, []byte{0x41, 0x9A}))
	for _, c := range []*conn{a, b} {
		select {
		case got := <-c.aus:
			if got.port != 9900 {
				t.Fatalf("port %d", got.port)
			}
		case <-time.After(3 * time.Second):
			t.Fatal("a subscriber got no AU")
		}
	}
	a.CaptureStop(ctx(), 9900)
	time.Sleep(20 * time.Millisecond)
	if ch.stopped.Load() {
		t.Fatal("the shared capture stopped while b still watches it")
	}
	b.Close()
	waitFor(t, "last subscriber gone", ch.stopped.Load)
}

func TestGlobalChildCap(t *testing.T) {
	r := newRig(t)
	c := r.dial()
	r.srv.mu.Lock()
	for i := 0; i < MaxChildren; i++ {
		r.srv.children[90000+i] = []string{"x"}
	}
	r.srv.mu.Unlock()
	expectCode(t, c.CaptureStart(ctx(), 9900), proto.ErrBusy)
	r.srv.mu.Lock()
	for i := 0; i < MaxChildren; i++ {
		delete(r.srv.children, 90000+i)
	}
	r.srv.mu.Unlock()
	if err := c.CaptureStart(ctx(), 9900); err != nil {
		t.Fatal(err)
	}
}

// M5: no cut without room for the whole remaining budget; a cut that hits
// the -fs limit is truncated and refused.
func TestCutBudgetAndFreeSpace(t *testing.T) {
	var mu sync.Mutex
	free := int64(1 << 30)
	r := newRig(t, func(c *Config) {
		c.CutBudget = 4 << 20
		c.FreeBytes = func(string) (int64, error) { mu.Lock(); defer mu.Unlock(); return free, nil }
	})
	r.tree(t)
	p := r.cam + "/SentryClips/2026-11-16_07-56-30/2026-11-16_07-55-46-front.mp4"
	c := r.dial()
	mu.Lock()
	free = 3 << 20
	mu.Unlock()
	_, _, err := c.DashCut(ctx(), p, 1, 2)
	expectCode(t, err, proto.ErrBusy)
	mu.Lock()
	free = 1 << 30
	mu.Unlock()
	r.sp.mu.Lock()
	r.sp.onStart = func(ch *fakeChild) { // writes exactly the -fs limit
		a := ch.spec.Argv
		limit := a[len(a)-4]
		n := 0
		for _, d := range limit {
			n = n*10 + int(d-'0')
		}
		os.WriteFile(strings.TrimPrefix(a[len(a)-1], "file:"), make([]byte, n), 0o600)
		ch.exit(nil)
	}
	r.sp.mu.Unlock()
	_, _, err = c.DashCut(ctx(), p, 1, 2)
	expectCode(t, err, proto.ErrCutFailed)
	if ents, _ := os.ReadDir(filepath.Join(r.state, cutSubdir)); len(ents) != 0 {
		t.Fatal("a truncated cut was kept")
	}
	if ff := r.sp.all()[0].spec.Argv; ff[len(ff)-5] != "-fs" || ff[len(ff)-4] != "4194304" {
		t.Fatalf("ffmpeg argv %q", ff)
	}
}

// L2: three unknown gear readings end the selectable as well as talk.
func TestUnknownGearEndsTheSelectable(t *testing.T) {
	r := newRig(t)
	c := r.dial()
	c.Hold(ctx())
	c.Sel(ctx(), 1)
	r.car.take()
	r.car.set(ReadGear, "(false, '')")
	r.srv.tick()
	r.srv.tick()
	r.waitOps(sa1, sel1, sa1, sel1)
	r.srv.tick()
	r.waitOps(sel0, sa1)
	nextEvent(t, c, proto.EvSelEnd)
}

// H3: the sweep waits before its writes when a marker exists, confirms each
// 0 by readback, and keeps a marker whose readback disagrees.
func TestSweepSettlesAndConfirms(t *testing.T) {
	dir := t.TempDir()
	m := markers{dir}
	m.set(markSessionAct)
	m.set(markSelectable)
	car := newFakeCar()
	car.stuck[ReadSelectable] = true
	quiet := log.New(io.Discard, "", 0)
	start := time.Now()
	clean := Sweep(SweepConfig{Car: car, StateDir: dir, Settle: 150 * time.Millisecond, Log: quiet,
		Kill: func(int, syscall.Signal) error { return nil }})
	if time.Since(start) < 150*time.Millisecond {
		t.Fatal("no settle before the writes")
	}
	if clean || m.has(markSessionAct) || !m.has(markSelectable) {
		t.Fatalf("clean=%v sa=%v sel=%v", clean, m.has(markSessionAct), m.has(markSelectable))
	}
	// Nothing marked: no settle, no write.
	empty := t.TempDir()
	start = time.Now()
	if !Sweep(SweepConfig{Car: car, StateDir: empty, Settle: time.Second, Log: quiet}) || time.Since(start) > 500*time.Millisecond {
		t.Fatal("an empty sweep waited or failed")
	}
}

func TestIsCleared(t *testing.T) {
	for raw, want := range map[string]bool{
		"(true, 'false')": true, "(true, '0')": true, "(true, 0)": true, "(false, '')": true, "(true, None)": true,
		"(true, 'true')": false, "(true, '1')": false, "(true, '12')": false, "garbage": false, "": false,
	} {
		if got := isCleared(raw); got != want {
			t.Errorf("isCleared(%q) = %v", raw, got)
		}
	}
}
