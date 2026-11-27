package camera

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"airgapp/gateway/camhelper/proto"
	"airgapp/gateway/capability/testkit"
)

// openHost opens CAMERA on a window whose Host the test controls.
func openHost(t *testing.T, params string, setup ...func(*rig)) (*rig, *testkit.FakeHost) {
	t.Helper()
	host := testkit.NewFakeHost()
	r := &rig{t: t, w: testkit.NewFakeWindow(), fh: newFakeHelper()}
	r.w.SetHost(host)
	for _, s := range setup {
		s(r)
	}
	h, err := NewWithConfig(testConfig(r)).Open(context.Background(), r.w, json.RawMessage(params))
	if err != nil {
		t.Fatal(err)
	}
	r.h = h.(*handler)
	t.Cleanup(func() { r.w.Close("test"); r.h.Teardown("test") })
	return r, host
}

// callsAfter returns the helper calls made after the first n.
func (f *fakeHelper) callsAfter(n int) []string {
	c := f.callList()
	if n > len(c) {
		return nil
	}
	return c[n:]
}

// setGear makes the fake helper report gear g (its replies and a STATUS event).
func setGear(f *fakeHelper, g string) {
	f.mu.Lock()
	f.st.Gear, f.st.Driving = g, g == "R" || g == "N" || g == "D"
	st := f.st
	f.mu.Unlock()
	f.events <- proto.Event{Ev: proto.EvStatus, Status: &st}
}

// SUMMON opens (design §6.5, Tesla's summon webcam): talk goes off within one
// poll, the hold stays, SUMMON mode is asked for, and the selectable becomes
// the summon camera (Grid in P) in place of interior's 1.
func TestSummonTalkOffHoldStaysAndGridWithinOnePoll(t *testing.T) {
	r, host := openHost(t, `{"view":"interior","talk":true}`)
	f := r.helper()
	r.waitCall("talk_arm")
	r.waitCall("cap_start 9895")
	n := len(f.callList())
	start := time.Now()
	host.SetSummonOpen(true)
	r.waitCall("sel 11")
	if d := f.callAt("talk_disarm", 1).Sub(start); d > 300*time.Millisecond || d < 0 {
		t.Fatalf("talk off after %v (poll is 150 ms)", d)
	}
	got := strings.Join(f.callsAfter(n), ",")
	if !strings.HasPrefix(got, "talk_disarm,summon true,cap_stop 9895,sel 0,sel 11") {
		t.Fatalf("summon order: %s", got)
	}
	r.waitStatus("summon", func(s statusJSON) bool { return s.Reason == "summon" })
	waitFor(t, "TALK_STATE summon", func() bool { ts := r.lastTalk(); return ts.State == "error" && ts.Reason == "summon" })
	if ev := r.w.Events(); len(ev) != 1 || !strings.HasPrefix(ev[0], "interior hold start=") {
		t.Fatalf("selectable hold not logged: %q", ev)
	}
	if s := r.lastStreams(); len(s.Streams) != 0 {
		t.Fatalf("interior stream during SUMMON: %+v", s)
	}
	// Neither the hold nor talk are touched again while SUMMON is open.
	m := len(f.callList())
	time.Sleep(200 * time.Millisecond)
	for _, c := range f.callsAfter(m) {
		if c == "hold" || c == "release" || c == "talk_arm" || strings.HasPrefix(c, "sel ") {
			t.Fatalf("%s while SUMMON is open", c)
		}
	}
	if r.h.tq.len() != 0 {
		t.Fatal("queued talk audio kept across SUMMON")
	}
}

// The summon view is the car's summon camera on 9895, which follows the gear
// as Tesla's does: Grid in P and N, the front fisheye in D, Backup in R; no
// dewarp; and no stream outside SUMMON.
func TestSummonViewFollowsTheGear(t *testing.T) {
	r, host := openHost(t, `{"view":"summon"}`)
	f := r.helper()
	r.waitCall("hold")
	r.waitStatus("no summon camera outside SUMMON", func(s statusJSON) bool { return s.Reason == "select_refused" })
	if s := r.lastStreams(); len(s.Streams) != 0 {
		t.Fatalf("summon stream outside SUMMON: %+v", s)
	}
	host.SetSummonOpen(true)
	r.waitCall("sel 11")
	waitFor(t, "the grid on 9895", func() bool {
		s := r.lastStreams()
		return len(s.Streams) == 1 && s.Streams[0].View == "summon" && !s.Streams[0].Dewarp && startedPorts(f)["9895"]
	})
	if raw := r.lastStreamsRaw(); strings.Contains(raw, "dewarp") {
		t.Fatalf("summon stream %s", raw)
	}
	for _, step := range []struct{ gear, sel string }{{"D", "sel 3"}, {"R", "sel 9"}, {"N", "sel 11"}, {"P", "sel 11"}} {
		n := f.count("cap_start 9895")
		setGear(f, step.gear)
		if step.gear != "P" { // N to P keeps Grid: nothing to change
			waitFor(t, step.sel+" in "+step.gear, func() bool { return f.count(step.sel) > 0 && f.count("cap_start 9895") > n })
		}
		r.waitStatus("summon", func(s statusJSON) bool { return s.Reason == "summon" })
	}
	// sel 3: the parked default before SUMMON, then D.
	if f.count("sel 11") != 2 || f.count("sel 3") != 2 || f.count("sel 9") != 1 || f.has("release") {
		t.Fatalf("calls %v", f.callList())
	}
}

// SUMMON closes: the summon camera back to 0, then the mode off, then talk and
// the parked default selectable again; the hold was never dropped.
func TestSummonCloseReleasesTheSummonCameraThenResumes(t *testing.T) {
	r, host := openHost(t, `{"view":"fascia","talk":true}`)
	f := r.helper()
	r.waitCall("talk_arm")
	r.waitCall("sel 3")
	host.SetSummonOpen(true)
	r.waitCall("sel 11")
	n := len(f.callList())
	host.SetSummonOpen(false)
	waitFor(t, "talk re-armed", func() bool { return f.count("talk_arm") == 2 })
	waitFor(t, "selectable again", func() bool { return f.count("sel 3") == 2 })
	waitFor(t, "talk state leaves summon", func() bool { return r.lastTalk().Reason != "summon" })
	if got := strings.Join(f.callsAfter(n), ","); !strings.HasPrefix(got, "sel 0,summon false") {
		t.Fatalf("close order: %s", got)
	}
	if f.count("hold") != 1 || f.has("release") {
		t.Fatalf("the hold was dropped: %v", f.callList())
	}
	events := strings.Join(r.w.Events(), "|")
	if !strings.Contains(events, "front hold start=") || !strings.Contains(events, "grid hold start=") {
		t.Fatalf("session log %q", events)
	}
}

// No Summon session on the car (the helper refuses SUMMON mode): CAMERA keeps
// asking every tick, holds no summon camera, keeps the hold under the normal
// gates, and takes the mode as soon as it is granted.
func TestSummonModeRefusedIsAskedAgain(t *testing.T) {
	r, host := openHost(t, `{"view":"summon"}`, func(r *rig) { r.fh.summonErr = proto.ErrNoSummon })
	f := r.helper()
	r.waitCall("sel 3")
	host.SetSummonOpen(true)
	waitFor(t, "asked again", func() bool { return f.count("summon true") >= 3 })
	if f.has("sel 11") || f.has("release") {
		t.Fatalf("calls %v", f.callList())
	}
	r.waitStatus("summon", func(s statusJSON) bool { return s.Reason == "summon" })
	if s := r.lastStreams(); len(s.Streams) != 0 {
		t.Fatalf("summon stream without the mode: %+v", s)
	}
	f.mu.Lock()
	f.summonErr = ""
	f.mu.Unlock()
	r.waitCall("sel 11")
	waitFor(t, "the summon stream", func() bool { return len(r.lastStreams().Streams) == 1 })
}

// The helper ends SUMMON mode itself (the car's Summon session ended: the
// live file is gone): CAMERA forgets it and asks again.
func TestSummonModeEndedByTheHelper(t *testing.T) {
	r, host := openHost(t, `{"view":"summon"}`)
	f := r.helper()
	host.SetSummonOpen(true)
	r.waitCall("sel 11")
	f.mu.Lock()
	f.summonErr, f.st.Summon = proto.ErrNoSummon, false
	f.mu.Unlock()
	f.events <- proto.Event{Ev: proto.EvSummonEnd, Reason: "live"}
	f.events <- proto.Event{Ev: proto.EvSelEnd, Reason: proto.EvSummonEnd}
	waitFor(t, "asked again", func() bool { return f.count("summon true") >= 2 })
	waitFor(t, "no summon stream", func() bool { return len(r.lastStreams().Streams) == 0 })
	if f.count("sel 11") != 1 {
		t.Fatalf("calls %v", f.callList())
	}
}

// summon on times out (the helper may still have granted it) and the window
// closes before an answer: summon off is still sent (review of ccd4428,
// finding 4).
func TestSummonOffAfterATimedOutGrant(t *testing.T) {
	r, host := openHost(t, `{"view":"summon"}`, func(r *rig) { r.fh.summonDelay = time.Second })
	f := r.helper()
	host.SetSummonOpen(true)
	r.waitCall("summon true (timed out)")
	host.SetSummonOpen(false)
	r.waitCall("summon false")
	f.mu.Lock()
	f.summonDelay = 0
	f.mu.Unlock()
	time.Sleep(100 * time.Millisecond)
	if f.count("summon false") != 1 || f.has("sel 11") {
		t.Fatalf("calls %v", f.callList())
	}
}

// An unknown gear reading during SUMMON keeps the summon camera of the last
// known gear: no 3 -> 0 -> 11 -> 0 -> 3 flap (review of ccd4428, finding 5).
func TestSummonCameraKeepsTheLastKnownGear(t *testing.T) {
	r, host := openHost(t, `{"view":"summon"}`)
	f := r.helper()
	host.SetSummonOpen(true)
	r.waitCall("sel 11")
	n3 := f.count("sel 3") // the parked default, if it came before SUMMON
	setGear(f, "D")
	waitFor(t, "front fisheye in D", func() bool { return f.count("sel 3") == n3+1 })
	n := len(f.callList())
	setGear(f, "")
	time.Sleep(200 * time.Millisecond)
	for _, c := range f.callsAfter(n) {
		if strings.HasPrefix(c, "sel ") || c == "cap_stop 9895" {
			t.Fatalf("an unknown gear touched the camera: %v", f.callsAfter(n))
		}
	}
	if s := r.lastStreams(); len(s.Streams) != 1 || s.Streams[0].View != "summon" {
		t.Fatalf("the summon stream went: %+v", s)
	}
}

// The last known gear does not carry into a new SUMMON window: one whose first
// reading is unknown shows Grid, as Tesla's does (review T7).
func TestSummonWindowStartsWithoutAnOldGear(t *testing.T) {
	r, host := openHost(t, `{"view":"summon"}`)
	f := r.helper()
	r.waitCall("hold")
	setGear(f, "D")
	time.Sleep(50 * time.Millisecond)
	setGear(f, "")
	time.Sleep(50 * time.Millisecond)
	n := len(f.callList())
	host.SetSummonOpen(true)
	waitFor(t, "the summon camera", func() bool {
		for _, c := range f.callsAfter(n) {
			if strings.HasPrefix(c, "sel ") && c != "sel 0" {
				return true
			}
		}
		return false
	})
	for _, c := range f.callsAfter(n) {
		if strings.HasPrefix(c, "sel ") && c != "sel 0" {
			if c != "sel 11" {
				t.Fatalf("an old gear chose the camera: %v", f.callsAfter(n))
			}
			break
		}
	}
}

// Talk comes back only after summon off is confirmed, and summon on is asked
// only once talk is off (review live-4).
func TestSummonTalkOrder(t *testing.T) {
	r, host := openHost(t, `{"view":"rear","talk":true}`)
	f := r.helper()
	r.waitCall("talk_arm")
	host.SetSummonOpen(true)
	r.waitCall("summon true")
	if i, j := f.index("talk_disarm"), f.index("summon true"); i < 0 || j < i {
		t.Fatalf("summon on before talk was off: %v", f.callList())
	}
	r.waitCall("sel 11")
	f.mu.Lock()
	f.summonOffErr = proto.ErrBusy // summon off not confirmed
	f.mu.Unlock()
	host.SetSummonOpen(false)
	waitFor(t, "summon off tried twice", func() bool { return f.count("summon false") >= 2 })
	if f.count("talk_arm") != 1 {
		t.Fatalf("talk re-armed before summon off was confirmed: %v", f.callList())
	}
	f.mu.Lock()
	f.summonOffErr = ""
	f.mu.Unlock()
	waitFor(t, "talk again", func() bool { return f.count("talk_arm") == 2 })
	if i, j := lastIndex(f.callList(), "summon false"), lastIndex(f.callList(), "talk_arm"); j < i {
		t.Fatalf("talk before the confirmed summon off: %v", f.callList())
	}
}

func lastIndex(calls []string, c string) int {
	for i := len(calls) - 1; i >= 0; i-- {
		if calls[i] == c {
			return i
		}
	}
	return -1
}

func TestSummonMovesFrontTo9898AndStopsUnwatchedCaptures(t *testing.T) {
	r, host := openHost(t, `{"view":"front"}`)
	warm(t, r)
	f := r.helper()
	host.SetSummonOpen(true)
	r.waitCall("cap_start 9898")
	waitFor(t, "only the watched view", func() bool { return portSet(startedPorts(f)) == "9898" })
	r.waitStatus("summon", func(s statusJSON) bool { return s.Reason == "summon" })
	// The grid during SUMMON: its six tiles are watched, front on 9898.
	r.h.Frame(TSelect, []byte(`{"view":"grid"}`))
	waitFor(t, "grid tiles", func() bool { return portSet(startedPorts(f)) == "9896,9897,9898,9900" })
	// SUMMON closes: selectable 3 again (front back on 9895), warm set again;
	// the hold was kept throughout.
	host.SetSummonOpen(false)
	waitFor(t, "front's selectable again", func() bool { return f.count("sel 3") == 2 })
	waitFor(t, "warm again", func() bool { return portSet(startedPorts(f)) == "9892,9893,9895,9896,9897,9900,9901" })
	r.waitStatus("no reason", func(s statusJSON) bool { return s.Reason == "" })
	if f.count("hold") != 1 || f.has("release") {
		t.Fatalf("the hold was dropped: %v", f.callList())
	}
}

func TestDegradedLinkForcesKeyframeOnly(t *testing.T) {
	r, host := openHost(t, `{"view":"grid"}`)
	warm(t, r)
	f := r.helper()
	r.h.Frame(TSelect, []byte(`{"view":"grid","focus":"rear"}`))
	waitFor(t, "grid", func() bool { s := r.lastStreams(); return s.View == "grid" && s.Focus == "rear" })
	host.SetSummonLinkDegraded(true)
	waitFor(t, "all keyframe", func() bool {
		for _, s := range r.lastStreams().Streams {
			if s.Full {
				return false
			}
		}
		return true
	})
	f.au(9900, true, true, "I1")
	f.au(9900, false, false, "P1") // the focus is keyframe-only now
	n := len(r.videos())
	if v := r.videos(); v[n-1].payload != "I1" {
		t.Fatalf("video %+v", v[n-1])
	}
	host.SetSummonLinkDegraded(false)
	waitFor(t, "focus full again", func() bool { return r.lastStreams().Streams[1].Full })
	// The focus restarts from the cache (I1 P1), the newest displayed.
	waitFor(t, "catch-up", func() bool { return len(r.videos()) >= n+2 })
	v := r.videos()[n:]
	if v[0].payload != "I1" || v[0].flags&FlagCatchup == 0 || v[1].payload != "P1" || v[1].flags != 0 {
		t.Fatalf("restore %+v", v)
	}
	if f.has("release") {
		t.Fatal("a degraded link alone must not drop the hold")
	}
}

func TestSummonAndDegradedOverlap(t *testing.T) {
	r, host := openHost(t, `{"view":"front"}`)
	warm(t, r)
	f := r.helper()
	host.SetSummonLinkDegraded(true)
	host.SetSummonOpen(true)
	r.waitCall("summon true")
	waitFor(t, "keyframe front on 9898", func() bool {
		s := r.lastStreams()
		return len(s.Streams) == 1 && !s.Streams[0].Full && portSet(startedPorts(f)) == "9898"
	})
	host.SetSummonLinkDegraded(false) // SUMMON still open: full rate, still 9898
	waitFor(t, "full", func() bool { return r.lastStreams().Streams[0].Full })
	host.SetSummonOpen(false)
	waitFor(t, "resumed", func() bool {
		return portSet(startedPorts(f)) == "9892,9893,9895,9896,9897,9900,9901"
	})
	if f.count("hold") != 1 || f.has("release") {
		t.Fatalf("the hold was dropped: %v", f.callList())
	}
}

func TestCaptureDeadDuringSummonIsLogged(t *testing.T) {
	r, host := openHost(t, `{"view":"rear"}`)
	warm(t, r)
	host.SetSummonOpen(true)
	r.waitCall("summon true")
	// 9900 never sends anything: after NoVideo (300 ms) it is restarted and logged.
	waitFor(t, "logged", func() bool {
		for _, e := range r.w.Events() {
			if e == "capture_dead_during_summon port=9900 view=rear" {
				return true
			}
		}
		return false
	})
	waitFor(t, "restarted", func() bool { return r.helper().count("cap_start 9900") >= 2 })
}

// H2: a slow selectable write (3.5 s) neither drops the helper session nor
// the captures or the hold, and the window's reader is never blocked.
func TestSlowSelDoesNotDropTheSession(t *testing.T) {
	r, _ := openHost(t, `{"view":"front"}`)
	warm(t, r)
	f := r.helper()
	f.mu.Lock()
	f.selDelay, f.slowSel = 3500*time.Millisecond, 1 // front's 3 is released at once, the write of 1 is slow
	f.mu.Unlock()
	start := time.Now()
	if err := r.h.Frame(TSelect, []byte(`{"view":"interior"}`)); err != nil {
		t.Fatal(err)
	}
	time.Sleep(100 * time.Millisecond)
	for i := 0; i < 20; i++ { // the reader keeps going while the controller waits
		if err := r.h.Frame(TMode, []byte(`{"mode":"full"}`)); err != nil {
			t.Fatal(err)
		}
	}
	f.au(9900, true, true, "I") // AUs are still processed while the controller waits
	r.h.vid.mu.Lock()
	cached := r.h.vid.gops[9900] != nil
	r.h.vid.mu.Unlock()
	if !cached || f.has("sel 1") {
		t.Fatal("AUs were not processed during the slow call")
	}
	deadline := time.Now().Add(6 * time.Second)
	for !f.has("sel 1") {
		if time.Now().After(deadline) {
			t.Fatal("sel 1 never finished")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if time.Since(start) < 3400*time.Millisecond {
		t.Fatal("sel was not slow")
	}
	waitFor(t, "9895 for interior", func() bool { return f.count("cap_start 9895") == 2 })
	f.mu.Lock()
	closed := f.closed
	f.mu.Unlock()
	if closed != 0 || f.count("hold") != 1 || f.count("cap_start 9901") != 1 || f.count("cap_start 9900") != 1 {
		t.Fatalf("session or captures restarted: closed=%d calls=%v", closed, f.callList())
	}
}

// A sel past SlowTimeout is retried later, never fatal.
func TestTimedOutSelIsRetried(t *testing.T) {
	r := &rig{t: t, w: testkit.NewFakeWindow(), fh: newFakeHelper()}
	r.fh.selDelay = 300 * time.Millisecond
	cfg := testConfig(r)
	cfg.SlowTimeout = 50 * time.Millisecond
	h, err := NewWithConfig(cfg).Open(context.Background(), r.w, json.RawMessage(`{"view":"interior"}`))
	if err != nil {
		t.Fatal(err)
	}
	r.h = h.(*handler)
	t.Cleanup(func() { r.w.Close("test"); r.h.Teardown("test") })
	f := r.helper()
	r.waitCall("sel 1 (timed out)")
	f.mu.Lock()
	f.selDelay = 0
	f.mu.Unlock()
	r.waitCall("sel 1")
	r.waitCall("cap_start 9895")
	f.mu.Lock()
	closed := f.closed
	f.mu.Unlock()
	if closed != 0 || f.count("hold") != 1 {
		t.Fatalf("a timeout dropped the session: %v", f.callList())
	}
	// The timed-out sel was released before the retry (it may have succeeded late).
	calls := strings.Join(f.callList(), ",")
	if i, j := strings.Index(calls, "sel 1 (timed out)"), strings.LastIndex(calls, "sel 0"); i < 0 || j < i {
		t.Fatalf("no release after the timed-out sel: %s", calls)
	}
	// The possibly-held period of the timed-out sel is logged conservatively
	// when it is released; the current hold is logged at its end.
	if ev := r.w.Events(); len(ev) != 1 || !strings.HasPrefix(ev[0], "interior hold start=") {
		t.Fatalf("session-log events %q", ev)
	}
	_ = proto.ErrBusy
}
