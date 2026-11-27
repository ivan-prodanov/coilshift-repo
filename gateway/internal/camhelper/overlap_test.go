package camhelper

import (
	"context"
	"encoding/json"
	"io"
	"log"
	"strings"
	"testing"
	"time"

	"airgapp/gateway/capability"
	"airgapp/gateway/capability/camera"
	"airgapp/gateway/capability/testkit"
)

// The real CAMERA capability against the real helper (fake car).

func camCap(r *rig) capability.Capability {
	return camera.NewWithConfig(camera.Config{
		Dial: camera.SocketDialer(r.sock), Tick: 10 * time.Millisecond, Retry: 30 * time.Millisecond,
		Redial: 20 * time.Millisecond, CallTimeout: time.Second, Settle: 20 * time.Millisecond, Log: log.New(io.Discard, "", 0),
	})
}

func openCam(t *testing.T, r *rig, params string) (capability.Handler, *testkit.FakeWindow) {
	t.Helper()
	w := testkit.NewFakeWindow()
	h, err := camCap(r).Open(context.Background(), w, json.RawMessage(params))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { w.Close("test"); h.Teardown("test") })
	return h, w
}

func (r *rig) cond(f func(s *Server) bool) func() bool {
	return func() bool {
		r.srv.mu.Lock()
		defer r.srv.mu.Unlock()
		return f(r.srv)
	}
}

// An Open that overran its budget is torn down late, after the slot was
// freed and a NEW window opened. Its Teardown must release only what its own
// helper session holds: never the new window's sessionActive hold.
func TestLateTeardownOfAbandonedWindowSparesTheNewOne(t *testing.T) {
	r := newRig(t)
	r.car.setLinks(feedersOK)

	// Window A (the abandoned Open): interior with talk, boost on.
	hA, _ := openCam(t, r, `{"view":"interior","talk":true}`)
	waitFor(t, "A holds, owns talk and the selectable", r.cond(func(s *Server) bool {
		return len(s.holders) == 1 && s.talk.owner != nil && s.selOwner != nil
	}))
	hA.Frame(camera.TTalkStart, []byte(`{}`))
	waitFor(t, "A boosted", r.cond(func(s *Server) bool { return s.talk.boost }))
	r.srv.mu.Lock()
	sessA := r.srv.talk.owner
	r.srv.mu.Unlock()

	// Window B opens while A's late Teardown is still pending. The captures
	// are shared per port: B subscribes to A's running children instead of
	// doubling them. B's front needs selectable 3, which A still owns (busy):
	// B retries it.
	waitFor(t, "A's warm captures", func() bool { return len(r.sp.all()) >= 8 }) // 5 direct + 9901 + 9895 + writer
	before := len(r.sp.all())
	hB, wB := openCam(t, r, `{"view":"front"}`)
	waitFor(t, "B holds too", r.cond(func(s *Server) bool { return len(s.holders) == 2 }))
	waitFor(t, "B subscribed to the five direct views and 9901", r.cond(func(s *Server) bool {
		for sess := range s.sessions {
			if sess != sessA && len(sess.caps) == 6 {
				return true
			}
		}
		return false
	}))
	if n := len(r.sp.all()); n != before {
		t.Fatalf("B started %d more children: overlapping windows must share captures", n-before)
	}
	capFront := r.sp.find(t, "port=9901")
	r.car.take()

	// The late Teardown of A ("open overran": A's window was never closed):
	// talk off with an edge (B still holds), then B's retry takes the
	// selectable A held, for its front.
	hA.Teardown("open overran")
	r.waitOps(ra0, sa0, sa1, sel0, "gain=-200", sel3)
	waitFor(t, "A's session released", r.cond(func(s *Server) bool {
		_, alive := s.sessions[sessA]
		return !alive && len(s.holders) == 1 && s.talk.owner == nil && s.selOwner != nil && s.selOwner != sessA
	}))
	if capFront.stopped.Load() {
		t.Fatal("A's teardown stopped B's capture")
	}
	if m := strings.Join(r.markers(), ","); m != "sa,sel" {
		t.Fatalf("markers %q: B's hold and selectable markers must stay", m)
	}
	r.sp.find(t, "port=9895") // B's front, after its settle
	// B keeps working: interior replaces its front hold.
	hB.Frame(camera.TSelect, []byte(`{"view":"interior"}`))
	r.waitOps(sel0, sel1)
	waitFor(t, "B holds interior", r.cond(func(s *Server) bool { return s.selOwner != nil && s.selVal == 1 }))
	wB.Close("close")
	hB.Teardown("close")
	r.waitOps(sa0, sel0)
	if len(r.markers()) != 0 {
		t.Fatalf("markers %v", r.markers())
	}
}

// The core frees the slot before an abandoned Open even dials: the new
// window acquires first, then the old handler's goroutine acquires and is
// torn down. The refcount keeps B's hold.
func TestAbandonedWindowAcquiringAfterTheNewOne(t *testing.T) {
	r := newRig(t)
	hB, _ := openCam(t, r, `{"view":"rear"}`)
	waitFor(t, "B holds (and front's selectable)", r.cond(func(s *Server) bool { return len(s.holders) == 1 && s.selOwner != nil }))
	hA, _ := openCam(t, r, `{"view":"rear"}`)
	waitFor(t, "A holds", r.cond(func(s *Server) bool { return len(s.holders) == 2 }))
	r.car.take()
	hA.Teardown("open overran")
	waitFor(t, "A released", r.cond(func(s *Server) bool { return len(s.holders) == 1 && s.selOwner != nil }))
	r.waitOps() // no sessionActive 0 while B holds; B's selectable stays
	hB.Teardown("close")
	r.waitOps(sa0, sel0)
}

// CAMERA keeps the five direct views and 9901 captured while it holds
// sessionActive (plus 9895 during a selectable hold); Teardown releases every
// one of them.
func TestTeardownStopsEveryWarmCapture(t *testing.T) {
	r := newRig(t)
	h, w := openCam(t, r, `{"view":"interior"}`)
	ports := []string{"9892", "9893", "9895", "9896", "9897", "9900", "9901"}
	var kids []*fakeChild
	for _, p := range ports {
		kids = append(kids, r.sp.find(t, "port="+p))
	}
	waitFor(t, "sel and hold", r.cond(func(s *Server) bool { return s.selOwner != nil && len(s.holders) == 1 }))
	r.srv.mu.Lock()
	for sess := range r.srv.sessions {
		if n := len(sess.caps); n != len(ports) || n > MaxCapturesPerSession {
			t.Errorf("%d captures in the session", n)
		}
	}
	r.srv.mu.Unlock()
	r.car.take()
	w.Close("close")
	h.Teardown("close")
	r.waitOps(sa0, sel0)
	waitFor(t, "every warm capture stopped", func() bool {
		for _, k := range kids {
			if !k.stopped.Load() {
				return false
			}
		}
		return true
	})
}

// SUMMON opens while CAMERA has talk armed (design §6.5, the car's Summon
// session live): the exact car writes. Talk goes with its usual edge (the
// hold stays, so the exterior stream is un-routed; the car is still in P when
// the window opens), then the parked selectable is replaced by the summon
// camera, Grid in P. The watched capture keeps running. SUMMON closes: Grid
// back to 0, then the parked default and talk again.
func TestSummonOpenWriteSequence(t *testing.T) {
	live := func(c *Config) { c.SummonOK = func() (bool, string) { return true, "4242" } }
	open := func(t *testing.T, r *rig, params string) (capability.Handler, *testkit.FakeHost) {
		host := testkit.NewFakeHost()
		w := testkit.NewFakeWindow()
		w.SetHost(host)
		h, err := camCap(r).Open(context.Background(), w, json.RawMessage(params))
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { w.Close("test"); h.Teardown("test") })
		return h, host
	}
	t.Run("interior, talk boosted", func(t *testing.T) {
		r := newRig(t, live)
		r.car.setLinks(feedersOK)
		h, host := open(t, r, `{"view":"interior","talk":true}`)
		waitFor(t, "talk and selectable", r.cond(func(s *Server) bool {
			return s.talk.owner != nil && s.talk.writer != nil && s.selOwner != nil
		}))
		h.Frame(camera.TTalkStart, []byte(`{}`))
		waitFor(t, "boosted", r.cond(func(s *Server) bool { return s.talk.boost }))
		time.Sleep(20 * time.Millisecond)
		r.car.take()
		host.SetSummonOpen(true)
		r.waitOps(ra0, sa0, sa1, "gain=-200", sel0, sel11)
		time.Sleep(50 * time.Millisecond)
		if ops := r.car.snapshot(); len(ops) != 0 {
			t.Fatalf("writes after the switch: %v", ops)
		}
		if m := strings.Join(r.markers(), ","); m != markSessionAct+","+markSelectable {
			t.Fatalf("markers %v", m)
		}
	})

	t.Run("rear, talk", func(t *testing.T) {
		r := newRig(t, live)
		r.car.setLinks(feedersOK)
		_, host := open(t, r, `{"view":"rear","talk":true}`)
		waitFor(t, "talk armed", r.cond(func(s *Server) bool { return s.talk.owner != nil && s.talk.writer != nil }))
		waitFor(t, "front's default selectable", r.cond(func(s *Server) bool { return s.selOwner != nil && s.selVal == 3 }))
		rear := r.sp.find(t, "port=9900")
		r.car.take()
		host.SetSummonOpen(true)
		r.waitOps(ra0, sa0, sa1, sel0, sel11) // the default selectable 3 makes way for Grid
		time.Sleep(50 * time.Millisecond)
		if ops := r.car.snapshot(); len(ops) != 0 {
			t.Fatalf("writes after the switch: %v", ops)
		}
		if rear.stopped.Load() {
			t.Fatal("the watched capture stopped during SUMMON")
		}
		host.SetSummonOpen(false)
		waitFor(t, "back to the parked default", r.cond(func(s *Server) bool {
			return s.summon == nil && s.selVal == 3 && s.talk.owner != nil
		}))
		if ops := strings.Join(r.car.snapshot(), ","); !strings.HasPrefix(ops, sel0+","+sel3) {
			t.Fatalf("close writes %s", ops)
		}
	})
}
