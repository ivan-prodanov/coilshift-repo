package camera

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"airgapp/gateway/camhelper/proto"
	"airgapp/gateway/capability/testkit"
)

// seqWindow records the global order of everything sent (FakeWindow keeps
// one list per type).
type seqWindow struct {
	*testkit.FakeWindow
	mu  sync.Mutex
	seq []sent
}

type sent struct {
	t    byte
	body []byte
}

func (w *seqWindow) Send(t byte, b []byte) error {
	err := w.FakeWindow.Send(t, b)
	if err == nil {
		w.mu.Lock()
		w.seq = append(w.seq, sent{t, append([]byte(nil), b...)})
		w.mu.Unlock()
	}
	return err
}

func (w *seqWindow) TrySend(t byte, b []byte) bool {
	ok := w.FakeWindow.TrySend(t, b)
	if ok {
		w.mu.Lock()
		w.seq = append(w.seq, sent{t, append([]byte(nil), b...)})
		w.mu.Unlock()
	}
	return ok
}

func (w *seqWindow) since(n int) []sent {
	w.mu.Lock()
	defer w.mu.Unlock()
	return append([]sent(nil), w.seq[n:]...)
}

func (w *seqWindow) len() int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return len(w.seq)
}

// openSeq opens CAMERA with a seqWindow and a 1 h tick, so anything that
// happens at once after a SELECT cannot have waited for a tick.
func openSeq(t *testing.T, params string, setup ...func(*rig)) (*rig, *seqWindow) {
	t.Helper()
	sw := &seqWindow{FakeWindow: testkit.NewFakeWindow()}
	r := &rig{t: t, w: sw.FakeWindow, fh: newFakeHelper()}
	for _, s := range setup {
		s(r)
	}
	cfg := testConfig(r)
	cfg.Tick = time.Hour
	h, err := NewWithConfig(cfg).Open(context.Background(), sw, json.RawMessage(params))
	if err != nil {
		t.Fatal(err)
	}
	r.h = h.(*handler)
	t.Cleanup(func() { sw.Close("test"); r.h.Teardown("test") })
	return r, sw
}

// warm waits for the parked warm set: the five direct views, fascia's 9901
// and 9895 (the default selectable 3, or interior's 1) after its settle.
func warm(t *testing.T, r *rig) {
	t.Helper()
	for _, p := range []int{9892, 9893, 9895, 9896, 9897, 9900, 9901} {
		r.waitCall(fmt.Sprintf("cap_start %d", p))
	}
}

type vf struct {
	index, flags byte
	payload      string
}

// selectAndCollect sends SELECT and returns, in order, the frames sent
// within 200 ms (the tick is 1 h): it requires STREAMS before any VIDEO.
func selectAndCollect(t *testing.T, r *rig, sw *seqWindow, body string, wantVideo int) []vf {
	t.Helper()
	n := sw.len()
	start := time.Now()
	if err := r.h.Frame(TSelect, []byte(body)); err != nil {
		t.Fatal(err)
	}
	var out []vf
	for time.Since(start) < 200*time.Millisecond {
		out = out[:0]
		streams := false
		for _, s := range sw.since(n) {
			switch s.t {
			case TStreams:
				streams = true
			case TVideo:
				if !streams {
					t.Fatal("VIDEO before STREAMS")
				}
				out = append(out, vf{s.body[1], s.body[2], string(s.body[12:])})
			}
		}
		if streams && len(out) >= wantVideo {
			return out
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("got %d VIDEO frames within 200 ms, want %d: %+v", len(out), wantVideo, out)
	return nil
}

func fmtVF(v []vf) string {
	var s []string
	for _, f := range v {
		s = append(s, fmt.Sprintf("%d:%s:%d", f.index, f.payload, f.flags))
	}
	return strings.Join(s, ",")
}

const (
	fIDR   = proto.FlagIDR | proto.FlagParamSets // 3
	fCatch = FlagCatchup                         // 4
)

func TestSelectWarmPortSendsTheCachedGOPAtOnce(t *testing.T) {
	r, sw := openSeq(t, `{"view":"front"}`)
	warm(t, r)
	f := r.helper()
	f.au(9900, false, false, "P0") // before any IDR: not cached
	f.au(9900, true, true, "I1")
	f.au(9900, false, false, "P1")
	f.au(9900, false, false, "P2")
	if len(r.w.Sent(TVideo)) != 0 {
		t.Fatal("an unselected port was shown")
	}
	got := selectAndCollect(t, r, sw, `{"view":"rear"}`, 3)
	// In order; every AU but the newest is decode-only.
	if s := fmtVF(got); s != fmt.Sprintf("0:I1:%d,0:P1:%d,0:P2:0", fIDR|fCatch, fCatch) {
		t.Fatalf("catch-up %s", s)
	}
	f.au(9900, false, false, "P3") // live continues
	if v := r.videos(); v[len(v)-1].payload != "P3" || v[len(v)-1].flags != 0 {
		t.Fatalf("live after catch-up %+v", v[len(v)-1])
	}
}

func TestCatchUpOfASingleIDRIsDisplayed(t *testing.T) {
	r, sw := openSeq(t, `{"view":"front"}`)
	warm(t, r)
	r.helper().au(9897, true, true, "I1")
	got := selectAndCollect(t, r, sw, `{"view":"left_repeater"}`, 1)
	if s := fmtVF(got); s != fmt.Sprintf("0:I1:%d", fIDR) {
		t.Fatalf("catch-up %s", s)
	}
}

func TestGOPCacheResetsAtEveryIDR(t *testing.T) {
	r, sw := openSeq(t, `{"view":"front"}`)
	warm(t, r)
	f := r.helper()
	for _, a := range []struct {
		idr bool
		p   string
	}{{true, "I1"}, {false, "P1"}, {true, "I2"}, {false, "P2"}} {
		f.au(9893, a.idr, a.idr, a.p)
	}
	got := selectAndCollect(t, r, sw, `{"view":"left_pillar"}`, 2)
	if s := fmtVF(got); s != fmt.Sprintf("0:I2:%d,0:P2:0", fIDR|fCatch) {
		t.Fatalf("catch-up %s", s)
	}
}

func TestGOPCacheOverflowWaitsForTheNextIDR(t *testing.T) {
	r, sw := openSeq(t, `{"view":"front"}`)
	warm(t, r)
	f := r.helper()
	f.au(9892, true, true, "I1"+strings.Repeat("x", 1<<20-100))
	f.au(9892, false, false, "P1"+strings.Repeat("y", 600<<10)) // GOP > 1.5 MiB: the cache is dropped
	f.au(9892, false, false, "P2")                              // still dropped until the next IDR
	n := sw.len()
	r.h.Frame(TSelect, []byte(`{"view":"right_pillar"}`))
	waitFor(t, "STREAMS", func() bool { return r.lastStreams().View == "right_pillar" })
	time.Sleep(20 * time.Millisecond)
	for _, s := range sw.since(n) {
		if s.t == TVideo {
			t.Fatal("catch-up from an overflowed cache")
		}
	}
	f.au(9892, false, false, "P3") // waits for an IDR, as before catch-up existed
	f.au(9892, true, true, "I2")
	if got := payloads(r.videos()); got != "0:I2" {
		t.Fatalf("video %s", got)
	}
	// The cache works again from that IDR on.
	r.h.vid.mu.Lock()
	g := r.h.vid.gops[9892]
	ok := g != nil && g.ok && len(g.aus) == 1
	r.h.vid.mu.Unlock()
	if !ok {
		t.Fatal("cache not restarted at the IDR")
	}
}

func TestCatchUpSkippedAboveTheHighWatermark(t *testing.T) {
	r, sw := openSeq(t, `{"view":"front"}`)
	warm(t, r)
	f := r.helper()
	f.au(9896, true, true, "I1")
	f.au(9896, false, false, "P1")
	r.w.SetQueuedBytes(DropAbove + 1)
	n := sw.len()
	r.h.Frame(TSelect, []byte(`{"view":"right_repeater"}`))
	waitFor(t, "STREAMS", func() bool { return r.lastStreams().View == "right_repeater" })
	time.Sleep(20 * time.Millisecond)
	for _, s := range sw.since(n) {
		if s.t == TVideo {
			t.Fatal("catch-up sent above the high watermark")
		}
	}
	r.w.SetQueuedBytes(0)
	f.au(9896, false, false, "P2") // falls back to the next IDR
	f.au(9896, true, true, "I2")
	if got := payloads(r.videos()); got != "0:I2" {
		t.Fatalf("video %s", got)
	}
}

func TestCatchUpStopsAtTheWatermarkMidBurst(t *testing.T) {
	r, sw := openSeq(t, `{"view":"front"}`)
	warm(t, r)
	f := r.helper()
	f.au(9900, true, true, "I1")
	f.au(9900, false, false, "P1")
	// The lane fills after the first AU of the burst.
	r.h.vid.mu.Lock()
	r.h.vid.w = &fillAfterFirst{seqWindow: sw}
	r.h.vid.mu.Unlock()
	r.h.Frame(TSelect, []byte(`{"view":"rear"}`))
	waitFor(t, "STREAMS", func() bool { return r.lastStreams().View == "rear" })
	time.Sleep(20 * time.Millisecond)
	v := r.videos()
	if len(v) != 1 || v[0].flags != fIDR|fCatch {
		t.Fatalf("burst %+v", v)
	}
	r.h.vid.mu.Lock()
	need := r.h.vid.byPort[9900].needIDR
	r.h.vid.mu.Unlock()
	if !need {
		t.Fatal("an interrupted catch-up must resync at the next IDR")
	}
}

// fillAfterFirst reports a full bulk lane once one VIDEO frame went out.
type fillAfterFirst struct{ *seqWindow }

func (w *fillAfterFirst) QueuedBytes() int {
	if len(w.Sent(TVideo)) > 0 {
		return DropAbove + 1
	}
	return 0
}

func TestCapEndClearsThePortsCache(t *testing.T) {
	r, sw := openSeq(t, `{"view":"front"}`)
	warm(t, r)
	f := r.helper()
	f.au(9900, true, true, "I1")
	f.au(9900, false, false, "P1")
	f.events <- proto.Event{Ev: proto.EvCapEnd, Port: 9900, Reason: "exit"}
	waitFor(t, "cache cleared", func() bool {
		r.h.vid.mu.Lock()
		defer r.h.vid.mu.Unlock()
		return r.h.vid.gops[9900] == nil
	})
	n := sw.len()
	r.h.Frame(TSelect, []byte(`{"view":"rear"}`))
	waitFor(t, "STREAMS", func() bool { return r.lastStreams().View == "rear" })
	time.Sleep(20 * time.Millisecond)
	for _, s := range sw.since(n) {
		if s.t == TVideo {
			t.Fatal("catch-up from a restarted capture's old GOP")
		}
	}
}

func TestGridFocusUsesTheCache(t *testing.T) {
	r, sw := openSeq(t, `{"view":"front"}`)
	warm(t, r)
	f := r.helper()
	for _, p := range []uint16{9895, 9900, 9897, 9896, 9893, 9892} {
		f.au(p, true, true, fmt.Sprintf("I%d", p))
	}
	f.au(9900, false, false, "P1")
	// Into the grid: the focus gets its GOP, every other new tile its IDR at
	// once (front, index 0 before and after, already shows its picture).
	got := selectAndCollect(t, r, sw, `{"view":"grid","focus":"rear"}`, 4)
	want := fmt.Sprintf("1:I9900:%d,1:P1:0,2:I9897:3,3:I9896:3", fIDR|fCatch)
	if s := fmtVF(got); s != want {
		t.Fatalf("grid entry\n got %s\nwant %s", s, want)
	}
	// Focus moves to left_repeater (keyframe tile -> full): its GOP again.
	f.au(9897, false, false, "Q1")
	got = selectAndCollect(t, r, sw, `{"view":"grid","focus":"left_repeater"}`, 2)
	if s := fmtVF(got); s != fmt.Sprintf("2:I9897:%d,2:Q1:0", fIDR|fCatch) {
		t.Fatalf("focus change %s", s)
	}
}

func TestKeyframeSelectSendsOnlyTheCachedIDR(t *testing.T) {
	r, sw := openSeq(t, `{"view":"front","mode":"keyframe"}`)
	warm(t, r)
	f := r.helper()
	f.au(9900, true, true, "I1")
	f.au(9900, false, false, "P1")
	got := selectAndCollect(t, r, sw, `{"view":"rear"}`, 1)
	time.Sleep(10 * time.Millisecond)
	if s := fmtVF(got); s != "0:I1:3" || len(r.videos()) != 1 {
		t.Fatalf("keyframe catch-up %s", s)
	}
}

func startedPorts(f *fakeHelper) map[string]bool {
	running := map[string]bool{}
	for _, c := range f.callList() {
		if p, ok := strings.CutPrefix(c, "cap_start "); ok {
			running[p] = true
		}
		if p, ok := strings.CutPrefix(c, "cap_stop "); ok {
			delete(running, p)
		}
	}
	return running
}

func portSet(m map[string]bool) string {
	var out []string
	for p := range m {
		out = append(out, p)
	}
	sort.Strings(out)
	return strings.Join(out, ",")
}

func TestWarmCaptureSets(t *testing.T) {
	// Held while parked: the five direct views and fascia's 9901, whatever is
	// shown, plus 9895 (interior's 1, or front's 3 for every other view).
	for _, view := range []string{"interior", "rear", "fascia", "grid"} {
		r := open(t, `{"view":"`+view+`"}`)
		warm(t, r)
		if got := portSet(startedPorts(r.helper())); got != "9892,9893,9895,9896,9897,9900,9901" || len(startedPorts(r.helper())) > MaxCaptures {
			t.Fatalf("%s: parked captures %s", view, got)
		}
	}
	// Parked but refused (Sentry off): only the selected view, and no
	// default selectable (it would divert the dashcam slot for nothing).
	r3 := open(t, `{"view":"rear"}`, func(r *rig) { r.fh.holdErr = proto.ErrGateSentry })
	r3.waitCall("cap_start 9900")
	waitFor(t, "hold retried", func() bool { return r3.helper().count("hold") >= 2 })
	if got := portSet(startedPorts(r3.helper())); got != "9900" || r3.helper().has("sel 3") {
		t.Fatalf("captures without a hold: %s %v", got, r3.helper().callList())
	}
}

func TestDrivingCapturesOnlyTheViewedStreams(t *testing.T) {
	driving := func(r *rig) {
		r.fh.st = proto.Status{Gear: "D", Driving: true}
		r.fh.holdErr = proto.ErrGateGear
	}
	r := open(t, `{"view":"front"}`, driving)
	r.waitCall("cap_start 9898")
	waitFor(t, "hold retried", func() bool { return r.helper().count("hold") >= 2 })
	if got := portSet(startedPorts(r.helper())); got != "9898" {
		t.Fatalf("driving, front: %s", got)
	}
	r.h.Frame(TSelect, []byte(`{"view":"rear"}`))
	r.waitCall("cap_start 9900")
	r.waitCall("cap_stop 9898")
	if got := portSet(startedPorts(r.helper())); got != "9900" {
		t.Fatalf("driving, rear: %s", got)
	}
	// The grid while driving: its four tiles are the viewed streams (no pillars).
	r.h.Frame(TSelect, []byte(`{"view":"grid"}`))
	waitFor(t, "grid captures", func() bool { return len(startedPorts(r.helper())) == 4 })
	if got := portSet(startedPorts(r.helper())); got != "9896,9897,9898,9900" {
		t.Fatalf("driving, grid: %s", got)
	}
}

func TestParkedToDrivingStopsTheWarmSetAtOnce(t *testing.T) {
	r, _ := openSeq(t, `{"view":"rear"}`) // 1 h tick: only the status event can trigger it
	warm(t, r)
	f := r.helper()
	f.au(9893, true, true, "I")
	f.events <- proto.Event{Ev: proto.EvStatus, Status: &proto.Status{Gear: "D", Driving: true, Held: true}}
	waitFor(t, "unwatched captures stopped", func() bool { return portSet(startedPorts(f)) == "9900" })
	r.h.vid.mu.Lock()
	cached := r.h.vid.gops[9893] != nil
	r.h.vid.mu.Unlock()
	if cached {
		t.Fatal("an unwatched port keeps its cache while driving")
	}
}

func TestDrivingToParkedWarmsAgain(t *testing.T) {
	r := open(t, `{"view":"front"}`, func(r *rig) {
		r.fh.st = proto.Status{Gear: "D", Driving: true}
		r.fh.holdErr = proto.ErrGateGear
	})
	f := r.helper()
	r.waitCall("cap_start 9898")
	// Back in P: the next hold succeeds, front's selectable is held again
	// and the warm set (with 9895) comes up; front moves to 9895.
	f.mu.Lock()
	f.st = proto.Status{Gear: "P", Sentry: "Armed"}
	f.holdErr = ""
	f.mu.Unlock()
	waitFor(t, "warm again", func() bool { return portSet(startedPorts(f)) == "9892,9893,9895,9896,9897,9900,9901" })
	if !f.has("sel 3") || !r.lastStreams().Streams[0].Dewarp {
		t.Fatalf("front not back on the windshield ultrawide: %v", f.callList())
	}
}
