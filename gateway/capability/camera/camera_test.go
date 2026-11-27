package camera

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"strings"
	"sync"
	"testing"
	"time"

	"airgapp/gateway/camhelper/client"
	"airgapp/gateway/camhelper/proto"
	"airgapp/gateway/capability/testkit"
)

// ---- fake helper ----

type fakeHelper struct {
	mu           sync.Mutex
	calls        []string
	at           []time.Time // when each call was made
	onAU         client.AUFunc
	events       chan proto.Event
	done         chan struct{}
	closed       int
	st           proto.Status
	holdErr      string
	talkErr      string
	selErr       string
	summonErr    string        // summon on refused with this code (summon_unverified: no Summon session on the car)
	summonDelay  time.Duration // summon on takes this long (past CallTimeout: a timeout)
	summonOffErr string        // summon off refused with this code
	capErr       map[int]string
	selDelay     time.Duration
	slowSel      int // when set, selDelay applies only to this value
	pcm          bytes.Buffer
	pcmCalls     int
	block        chan struct{} // when set, every call blocks on it
}

func newFakeHelper() *fakeHelper {
	return &fakeHelper{
		events: make(chan proto.Event, 64), done: make(chan struct{}),
		st: proto.Status{Gear: "P", Sentry: "Armed", Route: "off"}, capErr: map[int]string{},
	}
}

func (f *fakeHelper) rec(ctx context.Context, s string, code string) error {
	f.mu.Lock()
	blk := f.block
	f.calls = append(f.calls, s)
	f.at = append(f.at, time.Now())
	f.mu.Unlock()
	if blk != nil {
		select {
		case <-blk:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	if code != "" {
		return &client.Error{Code: code}
	}
	return nil
}

func (f *fakeHelper) status() proto.Status {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.st
}

func (f *fakeHelper) Hold(ctx context.Context) (proto.Status, error) {
	f.mu.Lock()
	code := f.holdErr
	if code == "" {
		f.st.Held = true
	}
	f.mu.Unlock()
	return f.status(), f.rec(ctx, "hold", code)
}

func (f *fakeHelper) TalkArm(ctx context.Context) (proto.Status, error) {
	f.mu.Lock()
	code := f.talkErr
	if code == "" {
		f.st.Talk = true
		f.st.Route = "down"
	}
	f.mu.Unlock()
	return f.status(), f.rec(ctx, "talk_arm", code)
}

func (f *fakeHelper) TalkDisarm(ctx context.Context, noEdge bool) error {
	f.mu.Lock()
	f.st.Talk = false
	f.mu.Unlock()
	if noEdge {
		return f.rec(ctx, "talk_disarm no_edge", "")
	}
	return f.rec(ctx, "talk_disarm", "")
}
func (f *fakeHelper) Release(ctx context.Context) error {
	f.mu.Lock()
	f.st.Held = false
	f.mu.Unlock()
	return f.rec(ctx, "release", "")
}
func (f *fakeHelper) Gain(ctx context.Context, b bool) error {
	return f.rec(ctx, fmt.Sprintf("gain %v", b), "")
}
func (f *fakeHelper) Sel(ctx context.Context, v int) (proto.Status, error) {
	f.mu.Lock()
	code := f.selErr
	if v == 0 {
		code = ""
	}
	d := f.selDelay
	if f.slowSel != 0 && v != f.slowSel {
		d = 0
	}
	f.mu.Unlock()
	if d > 0 {
		select {
		case <-time.After(d):
		case <-ctx.Done():
			f.rec(ctx, fmt.Sprintf("sel %d (timed out)", v), "")
			return proto.Status{}, ctx.Err()
		}
	}
	return f.status(), f.rec(ctx, fmt.Sprintf("sel %d", v), code)
}
func (f *fakeHelper) Summon(ctx context.Context, on bool) (proto.Status, error) {
	f.mu.Lock()
	d := f.summonDelay
	f.mu.Unlock()
	if on && d > 0 {
		select {
		case <-time.After(d):
		case <-ctx.Done():
			f.rec(ctx, "summon true (timed out)", "")
			return proto.Status{}, ctx.Err()
		}
	}
	f.mu.Lock()
	code := f.summonErr
	if !on {
		code = f.summonOffErr
	}
	if code == "" {
		f.st.Summon = on
	}
	f.mu.Unlock()
	return f.status(), f.rec(ctx, fmt.Sprintf("summon %v", on), code)
}
func (f *fakeHelper) CaptureStart(ctx context.Context, p int) error {
	f.mu.Lock()
	code := f.capErr[p]
	f.mu.Unlock()
	return f.rec(ctx, fmt.Sprintf("cap_start %d", p), code)
}
func (f *fakeHelper) CaptureStop(ctx context.Context, p int) error {
	return f.rec(ctx, fmt.Sprintf("cap_stop %d", p), "")
}
func (f *fakeHelper) Status(ctx context.Context) (proto.Status, error) {
	return f.status(), f.rec(ctx, "status", "")
}
func (f *fakeHelper) WritePCM(b []byte) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.pcm.Write(b)
	f.pcmCalls++
	return nil
}
func (f *fakeHelper) Events() <-chan proto.Event { return f.events }
func (f *fakeHelper) Done() <-chan struct{}      { return f.done }
func (f *fakeHelper) Close() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.closed == 0 {
		close(f.done)
	}
	f.closed++
	return nil
}

func (f *fakeHelper) callList() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.calls...)
}

// index is the position of the first call c, or -1.
func (f *fakeHelper) index(c string) int {
	for i, x := range f.callList() {
		if x == c {
			return i
		}
	}
	return -1
}

func (f *fakeHelper) has(call string) bool {
	for _, c := range f.callList() {
		if c == call {
			return true
		}
	}
	return false
}

// callAt returns when the nth (1-based) call was made, or the zero time.
func (f *fakeHelper) callAt(call string, nth int) time.Time {
	f.mu.Lock()
	defer f.mu.Unlock()
	for i, c := range f.calls {
		if c == call {
			if nth--; nth == 0 {
				return f.at[i]
			}
		}
	}
	return time.Time{}
}

func (f *fakeHelper) count(call string) int {
	n := 0
	for _, c := range f.callList() {
		if c == call {
			n++
		}
	}
	return n
}

// au sends one access unit as the helper would.
func (f *fakeHelper) au(port uint16, idr, ps bool, payload string) {
	var flags byte
	if idr {
		flags |= proto.FlagIDR
	}
	if ps {
		flags |= proto.FlagParamSets
	}
	body := make([]byte, proto.AUHeader+len(payload))
	proto.PutAUHeader(body, port, flags, 42)
	copy(body[proto.AUHeader:], payload)
	f.mu.Lock()
	on := f.onAU
	f.mu.Unlock()
	on(port, flags, body)
}

// ---- rig ----

type rig struct {
	t     *testing.T
	w     *testkit.FakeWindow
	h     *handler
	mu    sync.Mutex
	fh    *fakeHelper
	dials int
	fail  bool
	tweak func(*Config) // adjusts testConfig
}

func testConfig(r *rig) Config {
	cfg := Config{
		Dial: func(onAU client.AUFunc) (Helper, error) {
			r.mu.Lock()
			defer r.mu.Unlock()
			r.dials++
			if r.fail {
				return nil, errors.New("no helper")
			}
			r.fh.mu.Lock()
			r.fh.onAU = onAU
			r.fh.mu.Unlock()
			return r.fh, nil
		},
		Tick: 10 * time.Millisecond, StatusEvery: 50 * time.Millisecond, CallTimeout: 300 * time.Millisecond,
		Redial: 20 * time.Millisecond, Retry: 40 * time.Millisecond, NoVideo: 300 * time.Millisecond,
		Stale: 200 * time.Millisecond, TalkTail: 60 * time.Millisecond, Chunk: 5 * time.Millisecond,
		Settle: 30 * time.Millisecond, Log: log.New(io.Discard, "", 0),
	}
	if r.tweak != nil {
		r.tweak(&cfg)
	}
	return cfg
}

func open(t *testing.T, params string, setup ...func(*rig)) *rig {
	t.Helper()
	r := &rig{t: t, w: testkit.NewFakeWindow(), fh: newFakeHelper()}
	for _, s := range setup {
		s(r)
	}
	c := NewWithConfig(testConfig(r))
	var raw json.RawMessage
	if params != "" {
		raw = json.RawMessage(params)
	}
	h, err := c.Open(context.Background(), r.w, raw)
	if err != nil {
		t.Fatal(err)
	}
	r.h = h.(*handler)
	t.Cleanup(func() { r.w.Close("test"); r.h.Teardown("test") })
	return r
}

func (r *rig) newHelper() *fakeHelper {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.fh = newFakeHelper()
	return r.fh
}

func (r *rig) helper() *fakeHelper {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.fh
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(2 * time.Millisecond)
	}
}

func (r *rig) waitCall(call string) {
	r.t.Helper()
	waitFor(r.t, call, func() bool { return r.helper().has(call) })
}

func (r *rig) lastStreams() streamsJSON {
	r.t.Helper()
	var s streamsJSON
	all := r.w.Sent(TStreams)
	if len(all) == 0 {
		r.t.Fatal("no STREAMS")
	}
	if err := json.Unmarshal(all[len(all)-1], &s); err != nil {
		r.t.Fatal(err)
	}
	return s
}

func (r *rig) lastStatus() statusJSON {
	var s statusJSON
	all := r.w.Sent(TStatus)
	if len(all) > 0 {
		json.Unmarshal(all[len(all)-1], &s)
	}
	return s
}

func (r *rig) lastTalk() talkStateJSON {
	var s talkStateJSON
	all := r.w.Sent(TTalkState)
	if len(all) > 0 {
		json.Unmarshal(all[len(all)-1], &s)
	}
	return s
}

func (r *rig) waitStatus(what string, cond func(statusJSON) bool) {
	r.t.Helper()
	waitFor(r.t, what, func() bool { return cond(r.lastStatus()) })
}

type vframe struct {
	index, flags byte
	payload      string
}

func (r *rig) videos() []vframe {
	var out []vframe
	for _, b := range r.w.Sent(TVideo) {
		if len(b) < 12 || b[0] != 1 || b[3] != 0 || binary.BigEndian.Uint64(b[4:12]) != 42 {
			r.t.Fatalf("bad VIDEO header % x", b[:12])
		}
		out = append(out, vframe{b[1], b[2], string(b[12:])})
	}
	return out
}

func payloads(v []vframe) string {
	var s []string
	for _, f := range v {
		s = append(s, fmt.Sprintf("%d:%s", f.index, f.payload))
	}
	return strings.Join(s, ",")
}

// ---- tests ----

func TestSpec(t *testing.T) {
	s := New().Spec()
	if s.Name != "CAMERA" || s.ID != 0x02 || s.LeaseMS != 20000 {
		t.Fatalf("%+v", s)
	}
	wantIn := map[byte]int{0x20: 64, 0x21: 32, 0x22: 64, 0x23: 16384, 0x24: 16}
	wantOut := map[byte]int{0xA0: 1 << 20, 0xA1: 1024, 0xA2: 1024, 0xA3: 256}
	if fmt.Sprint(s.In) != fmt.Sprint(wantIn) || fmt.Sprint(s.Out) != fmt.Sprint(wantOut) || len(s.Bulk) != 1 || !s.Bulk[0xA0] {
		t.Fatalf("maps %+v", s)
	}
	if len(`{"mode":"keyframe"}`) > s.In[TMode] || len(`{"view":"grid","focus":"right_repeater"}`) > s.In[TSelect] {
		t.Fatal("limits too small for the protocol's own bodies")
	}
	if New().PreArm(context.Background(), nil) != nil {
		t.Fatal("PreArm")
	}
}

func TestArmParams(t *testing.T) {
	c := NewWithConfig(Config{Dial: func(client.AUFunc) (Helper, error) { return nil, errors.New("x") }, Log: log.New(io.Discard, "", 0)})
	for _, bad := range []string{`{"view":"cabin"}`, `{"mode":"slow"}`, `{"view":"front","x":1}`, `[]`, `{"view":"front"} {}`, `{"talk":"yes"}`} {
		if _, err := c.Open(context.Background(), testkit.NewFakeWindow(), json.RawMessage(bad)); err == nil {
			t.Fatalf("accepted %s", bad)
		}
	}
	for _, good := range []string{``, `{}`, `{"view":"grid","mode":"keyframe","talk":true}`, `{"view":"fascia"}`} {
		w := testkit.NewFakeWindow()
		h, err := c.Open(context.Background(), w, json.RawMessage(good))
		if err != nil {
			t.Fatalf("%s: %v", good, err)
		}
		w.Close("x")
		h.Teardown("x")
	}
}

func TestOpenReturnsAtOnceAndTeardownIsBounded(t *testing.T) {
	r := &rig{t: t, w: testkit.NewFakeWindow(), fh: newFakeHelper()}
	r.fh.block = make(chan struct{}) // every helper call stalls
	cfg := testConfig(r)
	cfg.CallTimeout = 10 * time.Second
	start := time.Now()
	h, err := NewWithConfig(cfg).Open(context.Background(), r.w, json.RawMessage(`{"view":"grid","talk":true}`))
	if err != nil || time.Since(start) > 50*time.Millisecond {
		t.Fatalf("Open took %v: %v", time.Since(start), err)
	}
	waitFor(t, "a stalled helper call", func() bool { return len(r.fh.callList()) > 0 })
	// Overrun case: Teardown before the window is closed (Done still open).
	start = time.Now()
	h.Teardown("open overran")
	if d := time.Since(start); d > 400*time.Millisecond {
		t.Fatalf("Teardown took %v", d)
	}
	if r.fh.closed != 1 {
		t.Fatalf("helper closed %d times", r.fh.closed)
	}
	h.Teardown("again") // exactly-once safe
	if r.fh.closed != 1 {
		t.Fatal("second Teardown closed again")
	}
	close(r.fh.block)
	r.w.Close("x")
}

func TestSingleViewFullRate(t *testing.T) {
	r := open(t, `{"view":"rear"}`)
	r.waitCall("cap_start 9895")
	// Held: the default selectable 3 (front), then the five direct views and
	// fascia's 9901 (warm, in port order), then 9895 after its settle.
	want := "status,hold,sel 3,cap_start 9892,cap_start 9893,cap_start 9896,cap_start 9897,cap_start 9900,cap_start 9901,cap_start 9895"
	if got := strings.Join(r.helper().callList()[:10], ","); got != want {
		t.Fatalf("calls %v", got)
	}
	s := r.lastStreams()
	if s.View != "rear" || len(s.Streams) != 1 || s.Streams[0] != (streamJSON{Index: 0, View: "rear", Full: true}) {
		t.Fatalf("streams %+v", s)
	}
	f := r.helper()
	f.au(9900, false, false, "P0") // before the first IDR: dropped
	f.au(9900, true, false, "I-noPS")
	f.au(9900, true, true, "I1")
	f.au(9900, false, false, "P1")
	f.au(9901, true, true, "other") // not a current stream
	if got := payloads(r.videos()); got != "0:I1,0:P1" {
		t.Fatalf("video %s", got)
	}
	if v := r.videos(); v[0].flags != 3 || v[1].flags != 0 {
		t.Fatalf("flags %+v", v)
	}
	r.waitStatus("streams_ok", func(s statusJSON) bool { return s.StreamsOK && s.View == "rear" && s.Gear == "P" })
	if d := r.w.Detail(); !strings.Contains(d, "view=rear") {
		t.Fatalf("detail %q", d)
	}
}

func TestKeyframeMode(t *testing.T) {
	r := open(t, `{"view":"front","mode":"keyframe"}`)
	r.waitCall("cap_start 9895") // front: the windshield ultrawide, selectable 3
	f := r.helper()
	f.au(9895, true, true, "I1")
	f.au(9895, false, false, "P1")
	f.au(9895, true, true, "I2")
	if got := payloads(r.videos()); got != "0:I1,0:I2" {
		t.Fatalf("video %s", got)
	}
	// MODE full: the stream restarts from the cached GOP (I2 alone here),
	// then P frames follow live.
	r.h.Frame(TMode, []byte(`{"mode":"full"}`))
	waitFor(t, "full stream", func() bool { return r.lastStreams().Streams[0].Full })
	f.au(9895, false, false, "P2")
	f.au(9895, true, true, "I3")
	f.au(9895, false, false, "P3")
	if got := payloads(r.videos()); got != "0:I1,0:I2,0:I2,0:P2,0:I3,0:P3" {
		t.Fatalf("video %s", got)
	}
}

func TestGrid(t *testing.T) {
	r := open(t, `{"view":"front"}`)
	r.waitCall("cap_start 9895")
	if err := r.h.Frame(TSelect, []byte(`{"view":"grid","focus":"rear"}`)); err != nil {
		t.Fatal(err)
	}
	for _, p := range []int{9900, 9897, 9896, 9893, 9892} {
		r.waitCall(fmt.Sprintf("cap_start %d", p))
	}
	waitFor(t, "grid STREAMS", func() bool { return r.lastStreams().View == "grid" })
	s := r.lastStreams()
	if s.View != "grid" || s.Focus != "rear" || len(s.Streams) != 4 { // no pillars
		t.Fatalf("streams %+v", s)
	}
	for i, st := range s.Streams {
		if st.Index != i || st.View != gridViews[i] || st.Full != (st.View == "rear") {
			t.Fatalf("stream %d %+v", i, st)
		}
	}
	f := r.helper()
	f.au(9900, true, true, "rI")
	f.au(9900, false, false, "rP") // focus: full rate
	f.au(9897, true, true, "lI")
	f.au(9897, false, false, "lP") // tile: keyframe only
	f.au(9901, true, true, "xI")   // fascia: not a grid tile
	f.au(9895, true, true, "fI")   // the front tile: the windshield ultrawide
	if got := payloads(r.videos()); got != "1:rI,1:rP,2:lI,0:fI" {
		t.Fatalf("video %s", got)
	}
	if strings.Contains(r.w.Detail(), "grid") == false || !strings.Contains(r.w.Detail(), "seen=front,grid") {
		t.Fatalf("detail %q", r.w.Detail())
	}
	// Back to a single view: the captures stay warm.
	r.h.Frame(TSelect, []byte(`{"view":"left_pillar"}`))
	waitFor(t, "left_pillar STREAMS", func() bool { return r.lastStreams().View == "left_pillar" })
	time.Sleep(30 * time.Millisecond)
	for _, c := range f.callList() {
		if strings.HasPrefix(c, "cap_stop") {
			t.Fatalf("warm capture stopped: %s", c)
		}
	}
}

func TestBadSelectAndModeAreErrors(t *testing.T) {
	r := open(t, `{}`)
	for _, c := range []struct {
		t    byte
		body string
	}{
		{TSelect, `{"view":"cabin"}`}, {TSelect, `{"view":"front","focus":"rear"}`}, {TSelect, `{"view":"grid","focus":"grid"}`},
		{TSelect, `{"view":"front","x":1}`}, {TSelect, `nope`}, {TMode, `{"mode":"fast"}`},
		{TTalkStart, `{}`}, {TTalkPCM, `ab`}, {TTalkStop, `{}`}, {0x30, ``},
	} {
		if err := r.h.Frame(c.t, []byte(c.body)); err == nil {
			t.Fatalf("0x%02x %s accepted", c.t, c.body)
		}
	}
}

func TestDropToNextIDRWatermarks(t *testing.T) {
	r := open(t, `{"view":"rear"}`)
	r.waitCall("cap_start 9900")
	f := r.helper()
	f.au(9900, true, true, "I1")
	r.w.SetQueuedBytes(DropAbove + 1)
	f.au(9900, true, true, "I2") // above the high watermark: everything drops
	f.au(9900, false, false, "P2")
	r.w.SetQueuedBytes(512 << 10) // between the marks: still dropping
	f.au(9900, true, true, "I3")
	r.w.SetQueuedBytes(100 << 10) // below: resume, but only at an IDR
	f.au(9900, false, false, "P3")
	f.au(9900, true, true, "I4")
	f.au(9900, false, false, "P4")
	if got := payloads(r.videos()); got != "0:I1,0:I4,0:P4" {
		t.Fatalf("video %s", got)
	}
	// A full bulk lane (TrySend false) drops to the next IDR too.
	r.w.FailTrySend(true)
	f.au(9900, false, false, "P5")
	r.w.FailTrySend(false)
	f.au(9900, false, false, "P6")
	f.au(9900, true, true, "I7")
	if got := payloads(r.videos()); got != "0:I1,0:I4,0:P4,0:I7" {
		t.Fatalf("video %s", got)
	}
}

func TestDrivingMovesFrontTo9898(t *testing.T) {
	r := open(t, `{"view":"front"}`)
	r.waitCall("cap_start 9895")
	f := r.helper()
	f.au(9895, true, true, "I1")
	n := len(r.w.Sent(TStreams))
	f.events <- proto.Event{Ev: proto.EvStatus, Status: &proto.Status{Gear: "D", Driving: true, Held: true}}
	r.waitCall("cap_start 9898")
	r.waitCall("cap_stop 9895")
	r.waitCall("sel 0") // the default selectable is released while driving
	waitFor(t, "STREAMS resent", func() bool { return len(r.w.Sent(TStreams)) > n })
	if s := r.lastStreams(); len(s.Streams) != 1 || s.Streams[0].Dewarp {
		t.Fatalf("front on 9898 %+v", s)
	}
	f.au(9898, false, false, "P") // the new source starts at an IDR
	f.au(9898, true, true, "I2")
	if got := payloads(r.videos()); got != "0:I1,0:I2" {
		t.Fatalf("video %s", got)
	}
	r.waitStatus("driving", func(s statusJSON) bool { return s.Driving && s.Gear == "D" })
}

func TestInteriorHoldsSelectableOnlyWhileSelected(t *testing.T) {
	r := open(t, `{"view":"interior"}`)
	r.waitCall("sel 1")
	r.waitCall("cap_start 9895")
	if s := r.lastStreams(); len(s.Streams) != 1 || s.Streams[0].View != "interior" || s.Streams[0].Dewarp {
		t.Fatalf("streams %+v", s)
	}
	// Away from interior: released, then the default front hold (3).
	r.h.Frame(TSelect, []byte(`{"view":"fascia"}`))
	r.waitCall("sel 3")
	waitFor(t, "9895 again", func() bool { return r.helper().count("cap_start 9895") == 2 })
	r.h.Frame(TSelect, []byte(`{"view":"rear"}`))
	waitFor(t, "rear STREAMS", func() bool { return r.lastStreams().View == "rear" })
	time.Sleep(30 * time.Millisecond)
	calls := strings.Join(r.helper().callList(), ",")
	if !strings.Contains(calls, "cap_stop 9895,sel 0,sel 3") || strings.Count(calls, "sel ") != 3 {
		t.Fatalf("calls %s", calls)
	}
	if strings.Count(calls, "cap_start 9900") != 1 || strings.Count(calls, "cap_start 9901") != 1 {
		t.Fatalf("rear/fascia were not warm: %s", calls)
	}
	ev := r.w.Events()
	if len(ev) != 1 || !strings.HasPrefix(ev[0], "interior hold start=") {
		t.Fatalf("session-log events %q", ev)
	}
}

func TestSelectableHoldLoggedOnTeardown(t *testing.T) {
	r := open(t, `{"view":"fascia"}`)
	r.waitCall("sel 3")
	r.waitCall("cap_start 9895")
	r.w.Close("close")
	r.h.Teardown("close")
	// Named by its camera, not by the view on screen.
	if ev := r.w.Events(); len(ev) != 1 || !strings.HasPrefix(ev[0], "front hold start=") || !strings.HasSuffix(ev[0], "(dashcam wide slot diverted)") {
		t.Fatalf("events %q", ev)
	}
}

func TestInteriorRefusedWhileDriving(t *testing.T) {
	r := open(t, `{"view":"interior"}`, func(r *rig) {
		r.fh.st = proto.Status{Gear: "D", Driving: true}
		r.fh.holdErr = proto.ErrGateGear
	})
	r.waitStatus("select_refused", func(s statusJSON) bool { return s.Reason == "gate_gear" || s.Reason == "select_refused" })
	if r.helper().has("sel 1") || r.helper().has("sel 3") {
		t.Fatal("selectable requested while driving")
	}
	if s := r.lastStreams(); len(s.Streams) != 0 {
		t.Fatalf("streams %+v", s)
	}
	if len(r.w.Events()) != 0 {
		t.Fatal("hold logged")
	}
}

func TestSelectableRefusedByHelper(t *testing.T) {
	busy := func(r *rig) { r.fh.selErr = proto.ErrBusy } // e.g. another window holds it
	for _, view := range []string{"front", "interior"} {
		r := open(t, `{"view":"`+view+`"}`, busy)
		r.waitStatus(view+" select_refused", func(s statusJSON) bool { return s.Reason == "select_refused" })
		waitFor(t, "retried", func() bool {
			return r.helper().count("sel "+map[string]string{"front": "3", "interior": "1"}[view]) >= 2
		})
		if r.helper().has("cap_start 9895") {
			t.Fatal("captured 9895 without a hold")
		}
		if s := r.lastStreams(); len(s.Streams) != 0 {
			t.Fatalf("%s streams without its selectable: %+v", view, s)
		}
	}
	// A view that does not need the selectable ignores the refused default hold.
	r := open(t, `{"view":"rear"}`, busy)
	waitFor(t, "default hold tried", func() bool { return r.helper().count("sel 3") >= 2 })
	r.helper().au(9900, true, true, "I")
	r.waitStatus("rear fine", func(s statusJSON) bool { return s.StreamsOK && s.Reason == "" })
	// In the grid the front tile is missing, the others stream.
	r.h.Frame(TSelect, []byte(`{"view":"grid"}`))
	waitFor(t, "grid", func() bool { return r.lastStreams().View == "grid" })
	s := r.lastStreams()
	if len(s.Streams) != 3 || s.Streams[0].Index != 1 {
		t.Fatalf("grid without the front tile %+v", s)
	}
	r.waitStatus("grid select_refused", func(s statusJSON) bool { return s.Reason == "select_refused" })
}

func TestGateReportedAndCapturesContinue(t *testing.T) {
	r := open(t, `{"view":"rear"}`, func(r *rig) { r.fh.holdErr = proto.ErrGateSentry })
	r.waitCall("cap_start 9900")
	r.waitStatus("gate_sentry", func(s statusJSON) bool { return s.Reason == "gate_sentry" })
	waitFor(t, "hold retried", func() bool { return r.helper().count("hold") >= 2 })
}

func TestNoVideoRestartsCapture(t *testing.T) {
	r := open(t, `{"view":"left_repeater"}`)
	r.waitCall("cap_start 9897")
	r.waitStatus("no_video", func(s statusJSON) bool { return s.Reason == "no_video" })
	r.waitCall("cap_stop 9897")
	waitFor(t, "restart", func() bool { return r.helper().count("cap_start 9897") >= 2 })
	r.helper().au(9897, true, true, "I")
	r.waitStatus("recovered", func(s statusJSON) bool { return s.Reason == "" && s.StreamsOK })
}

func TestCapEndRestarts(t *testing.T) {
	r := open(t, `{"view":"right_pillar"}`)
	r.waitCall("cap_start 9892")
	r.helper().events <- proto.Event{Ev: proto.EvCapEnd, Port: 9892, Reason: "exit"}
	waitFor(t, "restart", func() bool { return r.helper().count("cap_start 9892") == 2 })
}

func TestHelperDownThenUp(t *testing.T) {
	r := open(t, `{"view":"rear","talk":true}`, func(r *rig) { r.fail = true })
	r.waitStatus("helper_down", func(s statusJSON) bool { return s.Reason == "helper_down" })
	// emit() sends STATUS, then TALK_STATE.
	waitFor(t, "talk helper_down", func() bool { ts := r.lastTalk(); return ts.State == "arming" && ts.Reason == "helper_down" })
	r.mu.Lock()
	r.fail = false
	r.mu.Unlock()
	r.waitCall("talk_arm")
	r.waitCall("cap_start 9900")
	// The helper dies: everything is re-acquired on a new session.
	old := r.helper()
	nf := r.newHelper()
	old.Close()
	waitFor(t, "re-acquired", func() bool { return nf.has("hold") && nf.has("cap_start 9900") && nf.has("talk_arm") })
}

func TestTeardownClosesTheSessionAndStopsGoroutines(t *testing.T) {
	r := open(t, `{"view":"grid","talk":true}`)
	r.waitCall("talk_arm")
	r.w.Close("close")
	r.h.Teardown("close")
	if r.helper().closed != 1 {
		t.Fatal("helper session not closed")
	}
	done := make(chan struct{})
	go func() { r.h.wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("goroutines still running")
	}
	n := len(r.w.Sent(TStatus))
	time.Sleep(50 * time.Millisecond)
	if len(r.w.Sent(TStatus)) != n {
		t.Fatal("frames after teardown")
	}
}

// ---- talk ----

func routeUp(r *rig) {
	r.t.Helper()
	r.waitCall("talk_arm")
	r.helper().events <- proto.Event{Ev: proto.EvRoute, Reason: "ok"}
	waitFor(r.t, "talk ready", func() bool { return r.lastTalk().State == "ready" })
}

func TestTalkSilenceWhileIdle(t *testing.T) {
	r := open(t, `{"view":"front","talk":true}`)
	r.waitCall("hold")
	// TALK_STATE goes out in reconcile's deferred emit(), after Hold returns.
	waitFor(t, "talk arming", func() bool { return r.lastTalk().State == "arming" })
	routeUp(r)
	f := r.helper()
	start := f.pcmLen()
	time.Sleep(200 * time.Millisecond)
	got := f.pcmLen() - start
	// ~200 ms of silence (48 kHz s16 = 96 bytes/ms), never a burst.
	if got < 100*bytesPerMS || got > 400*bytesPerMS {
		t.Fatalf("%d bytes of silence in 200 ms", got)
	}
	if !f.allZero() {
		t.Fatal("non-silence written while idle")
	}
	r.waitStatus("route ok", func(s statusJSON) bool { return s.Route == "ok" && s.Talk == "ready" })
}

func (f *fakeHelper) pcmLen() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.pcm.Len()
}

func (f *fakeHelper) allZero() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, b := range f.pcm.Bytes() {
		if b != 0 {
			return false
		}
	}
	return true
}

func TestPushToTalkBoostAndTail(t *testing.T) {
	r := open(t, `{"view":"front","talk":true}`)
	routeUp(r)
	f := r.helper()
	r.h.Frame(TTalkStart, []byte(`{}`))
	r.waitCall("gain true")
	msg := bytes.Repeat([]byte{1, 2}, 50*bytesPerMS/2) // 50 ms of audio
	r.h.Frame(TTalkPCM, msg[:1000])
	r.h.Frame(TTalkPCM, msg[1000:])
	waitFor(t, "playing", func() bool { return r.lastTalk().State == "playing" })
	r.h.Frame(TTalkStop, nil)
	r.waitCall("gain false")
	f.mu.Lock()
	pcm := f.pcm.Bytes()
	i := bytes.Index(pcm, msg)
	f.mu.Unlock()
	if i < 0 {
		t.Fatal("the message did not reach the writer intact and in order")
	}
	waitFor(t, "ready again", func() bool { return r.lastTalk().State == "ready" })
	if calls := strings.Join(f.callList(), ","); strings.Count(calls, "gain") != 2 {
		t.Fatalf("calls %s", calls)
	}
}

func TestNewMessageDuringTailKeepsBoost(t *testing.T) {
	r := open(t, `{"view":"front","talk":true}`)
	routeUp(r)
	r.h.Frame(TTalkStart, []byte(`{}`))
	r.waitCall("gain true")
	r.h.Frame(TTalkStop, []byte(`{}`))
	r.h.Frame(TTalkStart, []byte(`{}`)) // within the 60 ms tail
	time.Sleep(150 * time.Millisecond)
	if r.helper().has("gain false") {
		t.Fatal("boost dropped during an active message")
	}
}

func TestPCMHeldUntilRouteUp(t *testing.T) {
	r := open(t, `{"view":"front","talk":true}`)
	r.waitCall("talk_arm")
	msg := bytes.Repeat([]byte{7, 7}, 960)
	r.h.Frame(TTalkPCM, msg)
	time.Sleep(60 * time.Millisecond)
	if !r.helper().allZero() {
		t.Fatal("audio played before the route was up")
	}
	waitFor(t, "talk arming", func() bool { return r.lastTalk().State == "arming" })
	r.helper().events <- proto.Event{Ev: proto.EvRoute, Reason: "ok"}
	waitFor(t, "played", func() bool {
		f := r.helper()
		f.mu.Lock()
		defer f.mu.Unlock()
		return bytes.Contains(f.pcm.Bytes(), msg)
	})
}

func TestTalkQueueLimit(t *testing.T) {
	q := &talkQueue{max: talkQueueMax}
	sec := make([]byte, 1000*bytesPerMS)
	for i := 0; i < 44; i++ {
		q.push(sec)
	}
	if q.overflowed() {
		t.Fatal("overflow at 44 s")
	}
	q.push(sec)
	q.push(sec[:1001]) // odd tail of an over-limit push is trimmed to whole samples
	if q.len() != talkQueueMax || !q.overflowed() || q.overflowed() {
		t.Fatalf("len %d", q.len())
	}
	b, _ := q.pop(1920)
	if len(b) != 1920 || q.len() != talkQueueMax-1920 {
		t.Fatal("pop")
	}
	q2 := &talkQueue{max: 100}
	q2.push([]byte{1, 2, 3, 4})
	b, drained := q2.pop(8)
	if !bytes.Equal(b, []byte{1, 2, 3, 4, 0, 0, 0, 0}) || !drained {
		t.Fatalf("padded pop %v %v", b, drained)
	}
	if b, _ := q2.pop(8); b != nil {
		t.Fatal("pop from empty")
	}
}

func TestTalkOverflowReported(t *testing.T) {
	r := open(t, `{"view":"front","talk":true}`)
	r.waitCall("talk_arm") // route stays down: nothing plays
	chunk := make([]byte, 16384)
	for i := 0; i < talkQueueMax/len(chunk)+2; i++ {
		if err := r.h.Frame(TTalkPCM, chunk); err != nil {
			t.Fatal(err)
		}
	}
	waitFor(t, "talk_overflow", func() bool { return r.lastTalk().Reason == "talk_overflow" })
	r.waitStatus("talk_overflow", func(s statusJSON) bool { return s.Reason == "talk_overflow" })
	if r.h.tq.len() != talkQueueMax {
		t.Fatalf("queue %d", r.h.tq.len())
	}
	waitFor(t, "queued_ms 45000", func() bool { return r.lastTalk().QueuedMS == 45000 })
}

func TestTalkArmRefused(t *testing.T) {
	r := open(t, `{"view":"front","talk":true}`, func(r *rig) { r.fh.talkErr = proto.ErrExterior })
	waitFor(t, "talk error", func() bool { ts := r.lastTalk(); return ts.State == "error" && ts.Reason == "exterior_busy" })
	r.waitStatus("reason", func(s statusJSON) bool { return s.Reason == "exterior_busy" && s.Talk == "error" })
	waitFor(t, "retry", func() bool { return r.helper().count("talk_arm") >= 2 })
}

func TestTalkEndedByHelperReArms(t *testing.T) {
	r := open(t, `{"view":"front","talk":true}`)
	routeUp(r)
	r.helper().events <- proto.Event{Ev: proto.EvTalkEnd, Reason: proto.ErrGateGear}
	waitFor(t, "error state", func() bool { return r.lastTalk().State == "error" })
	waitFor(t, "re-arm", func() bool { return r.helper().count("talk_arm") >= 2 })
}

func TestNoTalkWithoutArm(t *testing.T) {
	r := open(t, `{"view":"front"}`)
	r.waitCall("hold")
	time.Sleep(30 * time.Millisecond)
	if r.helper().has("talk_arm") || len(r.w.Sent(TTalkState)) != 0 {
		t.Fatal("talk without talk:true")
	}
	if s := r.lastStatus(); s.Talk != "off" || s.Route != "off" || s.GainDB != nil {
		t.Fatalf("status %+v", s)
	}
}
