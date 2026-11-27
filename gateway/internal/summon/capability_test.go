package summon

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"runtime"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"airgapp/gateway/capability"
	"airgapp/gateway/capability/summonrelay"
	"airgapp/gateway/capability/testkit"
	"airgapp/gateway/internal/carlink"
	"airgapp/gateway/internal/clock"
)

// The capability tests run a Capability on the engine tests' recording fakes (engine_test.go): the car, with a hook
// inside Connect where the real car answers H1, and the marker. Like Task 6, each session is seeded with the car's
// location, pushed in answer to H1 as the real car pushes it, and verbs come from 10 m.

// capCar is the engine tests' fakeCar with a hook that runs inside Connect.
type capCar struct {
	*fakeCar

	hookMu    sync.Mutex
	onConnect func()
}

func (c *capCar) Connect() error {
	err := c.fakeCar.Connect()
	c.hookMu.Lock()
	hook := c.onConnect
	c.hookMu.Unlock()
	if hook != nil {
		hook()
	}
	return err
}

func (c *capCar) setOnConnect(f func()) {
	c.hookMu.Lock()
	c.onConnect = f
	c.hookMu.Unlock()
}

func (c *capCar) setOnSend(f func(data []byte)) {
	c.fakeCar.mu.Lock()
	c.fakeCar.onSend = f
	c.fakeCar.mu.Unlock()
}

func (c *capCar) setOnDisconnect(f func()) {
	c.fakeCar.mu.Lock()
	c.fakeCar.onDisconnect = f
	c.fakeCar.mu.Unlock()
}

func (c *capCar) setConnectErr(err error) {
	c.fakeCar.mu.Lock()
	c.fakeCar.connectErr = err
	c.fakeCar.mu.Unlock()
}

// capRig is a Capability on recording fakes, a BOOTTIME clock (a Fake unless the test needs real time) and the fixed
// wall clock wallMS.
type capRig struct {
	c       *Capability
	fake    *clock.Fake // the clock, when it is a Fake
	log     *callLog
	car     *capCar
	marker  *fakeMarker
	healthy atomic.Bool

	mu     sync.Mutex
	logs   []string
	links  []bool
	fatals []error
}

func newCapRig(t *testing.T, clk clock.Clock) *capRig {
	t.Helper()
	r := &capRig{log: &callLog{}}
	r.fake, _ = clk.(*clock.Fake)
	r.car = &capCar{fakeCar: &fakeCar{log: r.log}}
	r.marker = &fakeMarker{log: r.log}
	r.healthy.Store(true)
	r.c = NewCapability(CapConfig{
		Car:    r.car,
		Marker: r.marker,
		Clock:  clk,
		Now:    func() time.Time { return time.UnixMilli(wallMS) },
		Logf: func(format string, args ...any) {
			r.mu.Lock()
			r.logs = append(r.logs, fmt.Sprintf(format, args...))
			r.mu.Unlock()
		},
		Healthy: r.healthy.Load,
		SetLink: func(degraded bool) {
			r.mu.Lock()
			r.links = append(r.links, degraded)
			r.mu.Unlock()
		},
		Fatal: func(err error) {
			r.mu.Lock()
			r.fatals = append(r.fatals, err)
			r.mu.Unlock()
		},
	})
	t.Cleanup(func() {
		r.mu.Lock()
		defer r.mu.Unlock()
		if len(r.fatals) > 0 {
			t.Errorf("Fatal was called: %v", r.fatals)
		}
	})
	return r
}

func (r *capRig) calls() []string { return r.log.get() }

func (r *capRig) lines() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Clone(r.logs)
}

// linesWith is the log lines that contain s.
func (r *capRig) linesWith(s string) []string {
	var out []string
	for _, l := range r.lines() {
		if strings.Contains(l, s) {
			out = append(out, l)
		}
	}
	return out
}

func (r *capRig) linkCalls() []bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Clone(r.links)
}

// step advances the fake BOOTTIME by ms and runs one pass of the D2 loop.
func (r *capRig) step(ms uint64) {
	r.fake.Advance(ms)
	r.c.tick()
}

// waitCall waits until the car, marker or phone recorded call.
func (r *capRig) waitCall(t *testing.T, call string) {
	t.Helper()
	eventually(t, "the call "+call, func() bool { return slices.Contains(r.calls(), call) })
}

// testWindow is what the tests need from a window: testkit.FakeWindow, or a test type that wraps it.
type testWindow interface {
	capability.Window
	CloseReason() string
	Sent(t byte) [][]byte
	IssueTick() [8]byte
	SetTickAge(id [8]byte, age time.Duration, ok bool)
	FailTrySend(v bool)
}

// capSession is an open session as the core holds it (R9): once the window's Done closes, Teardown runs exactly once.
type capSession struct {
	h    capability.Handler
	w    testWindow
	torn chan struct{} // closed when Teardown returned
}

// open opens a session on w with no ARM params, as today's phone does, and runs the core's teardown for it.
func (r *capRig) open(t *testing.T, w testWindow) *capSession {
	t.Helper()
	return r.openWith(t, w, nil)
}

// openWith opens a session on w with the inner ARM params, and runs the core's teardown for it.
func (r *capRig) openWith(t *testing.T, w testWindow, params json.RawMessage) *capSession {
	t.Helper()
	h, err := r.c.Open(context.Background(), w, params)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	s := &capSession{h: h, w: w, torn: make(chan struct{})}
	go func() {
		defer close(s.torn)
		<-w.Done()
		h.Teardown(w.CloseReason())
	}()
	t.Cleanup(func() {
		w.Close("close")
		select {
		case <-s.torn:
		case <-time.After(3 * time.Second):
			t.Errorf("Teardown did not return within 3 s of the window's close")
		}
	})
	return s
}

// openSeeded opens a session whose H1 push brings the car's location (in P, at speed 0), and waits until the location
// reached the window: the engine has the car's position.
func (r *capRig) openSeeded(t *testing.T, w testWindow) *capSession {
	t.Helper()
	return r.openSeededWith(t, w, nil)
}

// openSeededWith is openSeeded with the inner ARM params.
func (r *capRig) openSeededWith(t *testing.T, w testWindow, params json.RawMessage) *capSession {
	t.Helper()
	r.car.setOnConnect(func() { r.c.CarFrame(carFrame(carlink.ChannelTCP, locationJSON(carLat, carLon, "P", 0))) })
	s := r.openWith(t, w, params)
	waitCar(t, w, 1)
	r.car.setOnConnect(nil)
	return s
}

// presenceOn is the inner ARM params of a phone that opted into the presence lease.
var presenceOn = json.RawMessage(`{"presence":1}`)

// eachOptIn runs test on a session opened as today's phone opens it, with no params, and on one that opted into the
// presence lease (R8): D2 behaves the same at the capability in both.
func eachOptIn(t *testing.T, test func(t *testing.T, params json.RawMessage)) {
	t.Helper()
	t.Run("opted out", func(t *testing.T) { test(t, nil) })
	t.Run("presence", func(t *testing.T) { test(t, presenceOn) })
}

// assertOpen fails the test if w has closed.
func assertOpen(t *testing.T, w testWindow, when string) {
	t.Helper()
	if got := w.CloseReason(); got != "" {
		t.Fatalf("%s: the window closed with %q, want it open", when, got)
	}
}

// assertLease fails the test unless w has closed with the presence lease's reason, "lease".
func assertLease(t *testing.T, w testWindow, when string) {
	t.Helper()
	if got := w.CloseReason(); got != "lease" {
		t.Fatalf("%s: the window's close reason is %q, want lease", when, got)
	}
}

// waitTorn waits until the session's Teardown returned.
func (s *capSession) waitTorn(t *testing.T) {
	t.Helper()
	select {
	case <-s.torn:
	case <-time.After(3 * time.Second):
		t.Fatal("the window did not close and tear down within 3 s")
	}
}

// end closes the window as the phone's CLOSE does, and waits for Teardown.
func (s *capSession) end(t *testing.T) {
	t.Helper()
	s.w.Close("close")
	s.waitTorn(t)
}

func (s *capSession) frame(t *testing.T, typ byte, body []byte) {
	t.Helper()
	if err := s.h.Frame(typ, body); err != nil {
		t.Fatalf("Frame(0x%02x, %d bytes): %v", typ, len(body), err)
	}
}

// live sends a LIVE whose tick the window reports with (age, ok).
func (s *capSession) live(t *testing.T, age time.Duration, ok bool) {
	t.Helper()
	id := tickWith(s.w, age, ok)
	s.frame(t, TypeLive, id[:])
}

// verb sends m as a VERB whose tick the window reports with (age, ok).
func (s *capSession) verb(t *testing.T, m Message, age time.Duration, ok bool) {
	t.Helper()
	s.frame(t, TypeVerb, motionBody(tickWith(s.w, age, ok), 7, m.Canonical))
}

// hb sends m as an HB whose tick the window reports with (age, ok).
func (s *capSession) hb(t *testing.T, m Message, age time.Duration, ok bool) {
	t.Helper()
	s.frame(t, TypeHB, motionBody(tickWith(s.w, age, ok), 7, m.Canonical))
}

// msg sends m as a MSG.
func (s *capSession) msg(t *testing.T, m Message) {
	t.Helper()
	s.frame(t, TypeMsg, m.Canonical)
}

// presence sends a PRESENCE whose tick the window reports with (age, ok).
func (s *capSession) presence(t *testing.T, age time.Duration, ok bool) {
	t.Helper()
	id := tickWith(s.w, age, ok)
	s.frame(t, TypePresence, id[:])
}

// tickWith is a tick id that w reports with (age, ok). With ok false, the window does not know it.
func tickWith(w testWindow, age time.Duration, ok bool) [8]byte {
	id := w.IssueTick()
	w.SetTickAge(id, age, ok)
	return id
}

// motionBody is a VERB or HB body: tick id ‖ press id (u32 BE) ‖ Tesla's JSON.
func motionBody(id [8]byte, press uint32, js []byte) []byte {
	b := make([]byte, 12, 12+len(js))
	copy(b, id[:])
	binary.BigEndian.PutUint32(b[8:], press)
	return append(b, js...)
}

// carFrame is one CarserverToPeerCommsData call from QtCarServer's connection.
func carFrame(channel string, data []byte) carlink.CarFrame {
	return carlink.CarFrame{Channel: channel, UID: carlink.PhoneUID, Sender: ":1.7", Data: data}
}

// carBody is the CAR body the phone should get for data on channel byte ch.
func carBody(ch byte, data []byte) []byte { return append([]byte{ch}, data...) }

// waitCar waits until the window holds at least n CAR frames, and returns them.
func waitCar(t *testing.T, w testWindow, n int) [][]byte {
	t.Helper()
	var got [][]byte
	eventually(t, fmt.Sprintf("%d CAR frames at the window", n), func() bool {
		got = w.Sent(TypeCar)
		return len(got) >= n
	})
	return got
}

// eventually polls cond every millisecond for up to 2 s.
func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("no %s within 2 s", what)
		}
		time.Sleep(time.Millisecond)
	}
}

// refusalAt checks that the CAR body b is the helper's refusal of cmdType with reason on dcTcp, and returns its
// timestamp.
func refusalAt(t *testing.T, b []byte, cmdType, reason string) string {
	t.Helper()
	f := phoneFrame{ch: b[0], data: string(b[1:])}
	assertRefusal(t, f, cmdType, reason)
	return refusalTS(t, f)
}

// jsonOfSize is a car JSON object of exactly n bytes with msg_type typ, padded with a field whose value starts with
// pad.
func jsonOfSize(t *testing.T, typ, pad string, n int) []byte {
	t.Helper()
	head := fmt.Sprintf(`{"msg_type":%q,"pad":"%s`, typ, pad)
	fill := n - len(head) - len(`"}`)
	if fill < 0 {
		t.Fatalf("no %d-byte object fits %q", n, head)
	}
	return []byte(head + strings.Repeat("x", fill) + `"}`)
}

func TestInnerSpec(t *testing.T) {
	want := capability.Spec{
		Name:    "SUMMON",
		ID:      0x01,
		In:      map[byte]int{0x14: 8, 0x15: 1036, 0x16: 524, 0x17: 2048, 0x18: 8},
		Out:     map[byte]int{0x91: 16384},
		LeaseMS: 20000,
		TickHz:  20,
	}
	if got := InnerSpec(); !reflect.DeepEqual(got, want) {
		t.Fatalf("InnerSpec() = %+v, want %+v", got, want)
	}
	r := newCapRig(t, clock.NewFake())
	if got := r.c.Spec(); !reflect.DeepEqual(got, want) {
		t.Fatalf("Spec() = %+v, want %+v", got, want)
	}
	if TypeLive != 0x14 || TypeVerb != 0x15 || TypeHB != 0x16 || TypeMsg != 0x17 || TypeCar != 0x91 {
		t.Fatal("the frame types differ from the Global Constraints")
	}
	if TypePresence != 0x18 {
		t.Fatalf("TypePresence = 0x%02x, want 0x18", TypePresence)
	}
	// PRESENCE is a type of its own: not the relay's (0x10, 0x90), so a frame sent to the wrong session is refused as
	// grammar, and not another phone frame's.
	for _, other := range []byte{0x10, 0x90, summonrelay.TypeInner, summonrelay.TypeInnerOut, 0x14, 0x15, 0x16, 0x17,
		TypeLive, TypeVerb, TypeHB, TypeMsg} {
		if TypePresence == other {
			t.Fatalf("TypePresence 0x%02x is also another frame's type", TypePresence)
		}
	}
}

// sentinelKey stands for the phone's key that the core passes PreArm: no log line may hold it, raw or in hex.
var sentinelKey = append([]byte{0x04}, bytes.Repeat([]byte{0x5A, 0xC3}, 32)...)

// noSentinel fails the test if a log line holds the sentinel key, or the sentinel params' coordinates or VIN.
func noSentinel(t *testing.T, lines []string) {
	t.Helper()
	for _, l := range lines {
		for _, s := range []string{string(sentinelKey), hex.EncodeToString(sentinelKey), "5ac35ac3", "52.5200001",
			"13.4050001", "5YJ3E1EA7KF000000"} {
			if strings.Contains(l, s) {
				t.Fatalf("a log line holds %q: %q", s, l)
			}
		}
	}
}

// The core logs a refusal only as "SUMMON PreArm refused: error", so PreArm logs its reason itself (INT-2), once per
// refusal and never the phone's key. A PreArm that passes logs nothing.
func TestPreArmRefusesWhenUnhealthy(t *testing.T) {
	r := newCapRig(t, clock.NewFake())
	r.healthy.Store(false)
	if err := r.c.PreArm(context.Background(), sentinelKey); err == nil {
		t.Fatal("PreArm passed while the link is not owned and healthy")
	}
	if got := r.lines(); len(got) != 1 || got[0] != "summon: PreArm refused: the car link is not owned and healthy" {
		t.Fatalf("log lines %q, want the refusal's reason once", got)
	}
	noSentinel(t, r.lines())
	r.healthy.Store(true)
	if err := r.c.PreArm(context.Background(), sentinelKey); err != nil {
		t.Fatalf("PreArm refused while the link is healthy: %v", err)
	}
	if got := r.lines(); len(got) != 1 {
		t.Fatalf("a passing PreArm logged: %q", got[1:])
	}
	// With no health predicate at all, PreArm fails closed.
	c := NewCapability(CapConfig{Car: r.car, Marker: r.marker, Clock: clock.NewFake(), Fatal: func(error) {}})
	if err := c.PreArm(context.Background(), nil); err == nil {
		t.Fatal("PreArm passed with no Healthy predicate")
	}
}

func TestOpenEmitsH1Once(t *testing.T) {
	r := newCapRig(t, clock.NewFake())
	// The car answers H1 at once, so the engine must be installed (Reply 1) before H1 goes out.
	var replyAtH1 atomic.Int32
	replyAtH1.Store(-1)
	r.car.setOnConnect(func() { replyAtH1.Store(r.c.Reply()) })
	if got := r.c.Reply(); got != 0 {
		t.Fatalf("Reply() = %d with no session, want 0", got)
	}
	s := r.open(t, testkit.NewFakeWindow())
	want := []string{"marker.set", "car.connect"}
	if got := r.calls(); !slices.Equal(got, want) {
		t.Fatalf("Open made the calls %q, want %q", got, want)
	}
	if got := replyAtH1.Load(); got != 1 {
		t.Fatalf("Reply() = %d while H1 was emitted, want 1: the engine was not installed first", got)
	}
	if got := r.c.Reply(); got != 1 {
		t.Fatalf("Reply() = %d with a session up, want 1", got)
	}
	s.live(t, 0, true)
	s.live(t, 0, true) // the second Frame
	if got := r.calls(); !slices.Equal(got, want) {
		t.Fatalf("after two frames the calls are %q, want only %q: H1 goes out once, at Open", got, want)
	}
}

func TestOpenRefusedWhileSessionUp(t *testing.T) {
	// The core holds one slot per capability, so this never happens there. If it did, a second engine must not emit
	// a second H1 while the first session is live. The refusal is logged as a failed Start is (INT-2), and never
	// quotes the ARM params.
	r := newCapRig(t, clock.NewFake())
	r.open(t, testkit.NewFakeWindow())
	r.log.reset()
	before := len(r.lines())
	params := json.RawMessage(`{"lat":52.5200001,"lon":13.4050001,"vin":"5YJ3E1EA7KF000000"}`)
	if _, err := r.c.Open(context.Background(), testkit.NewFakeWindow(), params); err == nil {
		t.Fatal("a second Open succeeded while a session is up")
	}
	if got := r.calls(); len(got) != 0 {
		t.Fatalf("the refused Open made the calls %q", got)
	}
	if got := r.lines()[before:]; len(got) != 1 || got[0] != "summon: the session did not start: a session is already up" {
		t.Fatalf("log lines %q, want the refusal's reason once", got)
	}
	noSentinel(t, r.lines())
}

// liveRig is a capability rig whose Live records "on" and returns an off that records "off" (R1), and the record.
func liveRig(t *testing.T) (*capRig, func() []string) {
	t.Helper()
	r := newCapRig(t, clock.NewFake())
	var mu sync.Mutex
	var lives []string
	add := func(s string) {
		mu.Lock()
		lives = append(lives, s)
		mu.Unlock()
	}
	r.c.cfg.Live = func() func() {
		add("on")
		return func() { add("off") }
	}
	return r, func() []string {
		mu.Lock()
		defer mu.Unlock()
		return slices.Clone(lives)
	}
}

// The live signal (camhelper's Summon camera keys on it, design §6.5) is on only while the engine of a session that
// opted into the presence lease is up (ruling F1): Live is called once after a successful Start, and the off it
// returned runs at the session's uninstall, clean or not (R1). A failed Start never calls it, though the marker stays
// then. TestLiveNeedsTheOptIn has the sessions that did not opt in.
func TestLiveFollowsTheSession(t *testing.T) {
	t.Run("a clean close", func(t *testing.T) {
		r, got := liveRig(t)
		w := testkit.NewFakeWindow()
		s := r.openWith(t, w, presenceOn)
		if l := got(); !slices.Equal(l, []string{"on"}) {
			t.Fatalf("after Open: %q", l)
		}
		w.Close("close")
		s.waitTorn(t)
		if l := got(); !slices.Equal(l, []string{"on", "off"}) {
			t.Fatalf("after Teardown: %q", l)
		}
	})
	// T2: H4 cannot confirm the disconnect, so the marker stays for the takeover; the live signal goes all the same.
	t.Run("an unclean end", func(t *testing.T) {
		r, got := liveRig(t)
		r.car.setDisconnectErr(errors.New("carlink: the disconnect was not confirmed"))
		w := testkit.NewFakeWindow()
		s := r.openWith(t, w, presenceOn)
		w.Close("error")
		s.waitTorn(t)
		if slices.Contains(r.calls(), "marker.clear") {
			t.Fatalf("the calls %q clear the marker without a confirmed disconnect", r.calls())
		}
		if l := got(); !slices.Equal(l, []string{"on", "off"}) {
			t.Fatalf("after an unclean Teardown: %q", l)
		}
	})
	// A failed Start logs only its own line: not whether the session opted in, since it never started.
	t.Run("a failed Start", func(t *testing.T) {
		r, got := liveRig(t)
		r.car.setConnectErr(errors.New("carlink: emit failed"))
		if _, err := r.c.Open(context.Background(), testkit.NewFakeWindow(), presenceOn); err == nil {
			t.Fatal("Open succeeded although H1 failed")
		}
		if l := got(); len(l) != 0 {
			t.Fatalf("a failed Start called %q", l)
		}
		if l := r.lines(); len(l) != 1 || !strings.HasPrefix(l[0], "summon: the session did not start: ") {
			t.Fatalf("log lines %q, want the failed Start's line alone", l)
		}
	})
	// A session that is no longer current never turns the signal on, and a current one turns it on only once.
	t.Run("only the current session, once", func(t *testing.T) {
		r, got := liveRig(t)
		first := r.openWith(t, testkit.NewFakeWindow(), presenceOn)
		first.end(t)
		second := r.openWith(t, testkit.NewFakeWindow(), presenceOn)
		r.c.setLive(first.h.(*session), true)
		if l := got(); !slices.Equal(l, []string{"on", "off", "on"}) {
			t.Fatalf("the first session's on, while the second is current, made the calls %q", l)
		}
		r.c.setLive(second.h.(*session), true)
		if l := got(); !slices.Equal(l, []string{"on", "off", "on"}) {
			t.Fatalf("a second on for the current session made the calls %q", l)
		}
	})
}

// Ruling F1: a session that did not opt into the presence lease never calls Live, so it never vouches for the
// camera's SUMMON mode: agw could keep it up about 20 s per frame it held back. It keeps Summon, the 20 s lease and
// H4, and its teardown has no off to call.
func TestLiveNeedsTheOptIn(t *testing.T) {
	for _, params := range []json.RawMessage{nil, json.RawMessage(`{}`), json.RawMessage(`null`),
		json.RawMessage(`{"other":1}`)} {
		name := string(params)
		if params == nil {
			name = "nil"
		}
		t.Run(name, func(t *testing.T) {
			r, got := liveRig(t)
			w := testkit.NewFakeWindow()
			s := r.openSeededWith(t, w, params) // the forwarder runs: Open has returned whole
			if n := r.c.Reply(); n != 1 {
				t.Fatalf("Reply() = %d after Open, want 1: the session is up", n)
			}
			if l := got(); len(l) != 0 {
				t.Fatalf("Open of a session that did not opt in made the live calls %q", l)
			}
			r.log.reset()
			s.end(t)
			if l := got(); len(l) != 0 {
				t.Fatalf("the teardown of a session that did not opt in made the live calls %q", l)
			}
			if c, want := r.calls(), []string{"car.disconnect", "marker.clear"}; !slices.Equal(c, want) {
				t.Fatalf("Teardown made the calls %q, want H4's %q", c, want)
			}
		})
	}
}

// L2: a panic in the live signal's off is logged and contained, so Teardown's Stop still runs H4, and Fatal is not
// called.
func TestLiveOffPanicDoesNotSkipStop(t *testing.T) {
	r := newCapRig(t, clock.NewFake())
	r.c.cfg.Live = func() func() {
		return func() { panic("test: the live signal's off panics") }
	}
	s := r.openWith(t, testkit.NewFakeWindow(), presenceOn)
	r.log.reset()
	s.end(t)
	if got, want := r.calls(), []string{"car.disconnect", "marker.clear"}; !slices.Equal(got, want) {
		t.Fatalf("Teardown made the calls %q, want H4's %q", got, want)
	}
	if got := r.linesWith("summon: the live signal failed: test: the live signal's off panics"); len(got) != 1 {
		t.Fatalf("log lines %q: want the off's panic logged once", r.lines())
	}
	r.mu.Lock()
	fatals := slices.Clone(r.fatals)
	r.mu.Unlock()
	if len(fatals) != 0 {
		t.Fatalf("Fatal was called: %v", fatals)
	}
}

// L2: a panic in Live itself is logged and contained. The session runs without the live signal, and its teardown calls
// no off and still runs H4.
func TestLiveOnPanicIsContained(t *testing.T) {
	r := newCapRig(t, clock.NewFake())
	var ons atomic.Int32
	r.c.cfg.Live = func() func() {
		ons.Add(1)
		panic("test: the live signal panics")
	}
	w := testkit.NewFakeWindow()
	s := r.openSeededWith(t, w, presenceOn) // Open returned the session, and its forwarder hands the car's frames on
	if got := r.c.Reply(); got != 1 {
		t.Fatalf("Reply() = %d after Open, want 1: the session is up", got)
	}
	if got := r.linesWith("summon: the live signal failed: test: the live signal panics"); len(got) != 1 {
		t.Fatalf("log lines %q: want Live's panic logged once", r.lines())
	}
	r.log.reset()
	s.end(t)
	if got, want := r.calls(), []string{"car.disconnect", "marker.clear"}; !slices.Equal(got, want) {
		t.Fatalf("Teardown made the calls %q, want H4's %q", got, want)
	}
	if got := r.linesWith("summon: the live signal failed"); len(got) != 1 {
		t.Fatalf("log lines %q: the teardown called an off that Live never returned", got)
	}
	if n := ons.Load(); n != 1 {
		t.Fatalf("Live was called %d times, want once", n)
	}
}

func TestOpenFailedStartUninstalls(t *testing.T) {
	t.Run("H1 fails after the car answered", func(t *testing.T) {
		r := newCapRig(t, clock.NewFake())
		r.car.setConnectErr(errors.New("carlink: LocalToRemoteSignaling: emit failed"))
		// H1 may still have reached the car, which pushes its frames before the emit reports its failure.
		r.car.setOnConnect(func() {
			r.c.CarFrame(carFrame(carlink.ChannelTCP, []byte(`{"msg_type":"autopark:style","x":1}`)))
			r.c.CarFrame(carFrame(carlink.ChannelTCP, locationJSON(carLat, carLon, "P", 0)))
		})
		w := testkit.NewFakeWindow()
		if _, err := r.c.Open(context.Background(), w, nil); err == nil {
			t.Fatal("Open succeeded although H1 failed")
		}
		if got := r.c.Reply(); got != 0 {
			t.Fatalf("Reply() = %d after a failed Start, want 0", got)
		}
		r.c.CarFrame(carFrame(carlink.ChannelUDP, carEcho)) // between sessions: dropped
		// The next session works, and nothing of the failed one ever reached its window.
		r.car.setConnectErr(nil)
		r.openSeeded(t, testkit.NewFakeWindow())
		time.Sleep(20 * time.Millisecond)
		if got := w.Sent(TypeCar); len(got) != 0 {
			t.Fatalf("the failed session's window got %d CAR frames: frames went to an engine whose Start failed",
				len(got))
		}
	})
	t.Run("the marker fails", func(t *testing.T) {
		r := newCapRig(t, clock.NewFake())
		r.marker.mu.Lock()
		r.marker.setErr = errors.New("marker: read-only file system")
		r.marker.mu.Unlock()
		if _, err := r.c.Open(context.Background(), testkit.NewFakeWindow(), nil); err == nil {
			t.Fatal("Open succeeded although the marker could not be set")
		}
		if got := r.calls(); !slices.Equal(got, []string{"marker.set"}) {
			t.Fatalf("calls %q: H1 must not go out without the marker", got)
		}
		if got := r.c.Reply(); got != 0 {
			t.Fatalf("Reply() = %d after a failed Start, want 0", got)
		}
	})
}

func TestH1PushReachesWindow(t *testing.T) {
	style := []byte(`{"msg_type":"autopark:style","summon_style":"smart"}`)
	status := []byte(`{"msg_type":"autopark:status","autopark_state":"ready"}`)
	loc := locationJSON(carLat, carLon, "P", 0)
	want := [][]byte{carBody(ChanTCP, style), carBody(ChanTCP, status), carBody(ChanTCP, loc)}
	for i := range 50 {
		r := newCapRig(t, clock.NewFake())
		// The car answers H1 synchronously, inside Connect, while Start holds the engine lock.
		r.car.setOnConnect(func() {
			r.c.CarFrame(carFrame(carlink.ChannelTCP, style))
			r.c.CarFrame(carFrame(carlink.ChannelTCP, status))
			r.c.CarFrame(carFrame(carlink.ChannelTCP, loc))
		})
		w := testkit.NewFakeWindow()
		s := r.open(t, w)
		if got := waitCar(t, w, 3); !reflect.DeepEqual(got, want) {
			t.Fatalf("run %d: the window got %q, want the style, status and location in order", i, got)
		}
		s.end(t)
	}
}

func TestEndClosesCurrentWindow(t *testing.T) {
	r := newCapRig(t, clock.NewFake())
	r.c.End("error") // no session: nothing to close
	w := testkit.NewFakeWindow()
	s := r.open(t, w)
	r.c.End("error")
	s.waitTorn(t)
	if got := w.CloseReason(); got != "error" {
		t.Fatalf("the window closed with %q, want error", got)
	}
	r.waitCall(t, "car.disconnect")
	if got := r.c.Reply(); got != 0 {
		t.Fatalf("Reply() = %d after the session ended, want 0", got)
	}
	// End closes with the reason it was given.
	w2 := testkit.NewFakeWindow()
	s2 := r.open(t, w2)
	r.c.End("sigterm")
	s2.waitTorn(t)
	if got := w2.CloseReason(); got != "sigterm" {
		t.Fatalf("End(\"sigterm\") closed the window with %q", got)
	}
}

func TestPhoneEndClosesItsOwnWindow(t *testing.T) {
	// The engine calls Phone.End after it unlocks, so the call can arrive after its session ended and the next one
	// opened. It closes the window its engine belongs to, never the current one.
	r := newCapRig(t, clock.NewFake())
	w1 := testkit.NewFakeWindow()
	s1 := r.open(t, w1)
	p1 := s1.h.(*session).eng.cfg.Phone
	s1.end(t)
	w2 := testkit.NewFakeWindow()
	r.open(t, w2)
	p1.End("error")
	select {
	case <-w2.Done():
		t.Fatalf("the ended session's Phone.End closed the next session's window (reason %q)", w2.CloseReason())
	case <-time.After(20 * time.Millisecond):
	}
}

func TestRealClockTrip(t *testing.T) { eachOptIn(t, testRealClockTrip) }

func testRealClockTrip(t *testing.T, params json.RawMessage) {
	r := newCapRig(t, clock.Boot())
	abortAt := make(chan time.Time, 1)
	r.car.setOnSend(func(data []byte) {
		if string(data) == AbortJSON {
			select {
			case abortAt <- time.Now():
			default:
			}
		}
	})
	s := r.openSeededWith(t, testkit.NewFakeWindow(), params)
	ctx, cancel := context.WithCancel(context.Background())
	loopDone := make(chan struct{})
	go func() {
		defer close(loopDone)
		r.c.Loop(ctx)
	}()
	t.Cleanup(func() {
		cancel()
		<-loopDone
	})

	s.verb(t, verb10m(t, forwardType), 0, true)
	var last time.Time
	for range 5 {
		time.Sleep(20 * time.Millisecond)
		last = time.Now() // before the frame, so the measured gap is never shorter than the engine's
		s.live(t, 0, true)
	}
	select {
	case at := <-abortAt:
		if d := at.Sub(last); d < 300*time.Millisecond || d > 350*time.Millisecond {
			t.Fatalf("the injected abort reached the car %v after the last fresh LIVE, want 300-350 ms", d)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no injected abort within 2 s of the last fresh LIVE")
	}
}

func TestTickAgePassedThrough(t *testing.T) { eachOptIn(t, testTickAgePassedThrough) }

func testTickAgePassedThrough(t *testing.T, params json.RawMessage) {
	fwd := func(t *testing.T) Message { return verb10m(t, forwardType) }
	t.Run("VERB", func(t *testing.T) {
		r := newCapRig(t, clock.NewFake())
		w := testkit.NewFakeWindow()
		s := r.openSeededWith(t, w, params)
		m := fwd(t)
		// Each case ends with the phone's abort, so a refusal is for staleness, never for an active verb (R12).
		s.verb(t, m, 0, false) // unknown tick
		s.msg(t, abortMsg(t))
		s.verb(t, m, Fresh, true) // 150 ms
		s.msg(t, abortMsg(t))
		s.verb(t, m, Fresh+time.Millisecond, true) // 151 ms
		s.msg(t, abortMsg(t))
		want := []string{AbortJSON, string(m.Canonical), AbortJSON, AbortJSON}
		if got := r.car.sends(); !slices.Equal(got, want) {
			t.Fatalf("the car got %q, want %q: only the 150 ms verb goes through", got, want)
		}
		cars := w.Sent(TypeCar)
		if len(cars) != 3 {
			t.Fatalf("the window has %d CAR frames, want the location and two refusals", len(cars))
		}
		refusalAt(t, cars[1], forwardType, "action_invalid")
		refusalAt(t, cars[2], forwardType, "action_invalid")
		// The press id reaches the engine, which logs it for the forwarded verb and both refusals.
		if got := r.linesWith("(press 7"); len(got) != 3 {
			t.Fatalf("log lines %q: want the press id 7 for each of the three verbs", got)
		}
	})
	t.Run("stale LIVE and HB stamp no liveness", func(t *testing.T) {
		r := newCapRig(t, clock.NewFake())
		s := r.openSeededWith(t, testkit.NewFakeWindow(), params)
		m, hbm := fwd(t), heartbeatMsg(t)
		s.verb(t, m, 0, true) // the last fresh frame, a liveness instant
		r.step(100)
		s.live(t, 0, false)
		r.step(50)
		s.hb(t, hbm, Fresh+time.Millisecond, true)
		r.step(50)
		s.live(t, Fresh+time.Millisecond, true)
		r.step(50)
		s.hb(t, hbm, 0, false)
		r.step(50) // 300 ms
		if n := r.car.count(AbortJSON); n != 0 {
			t.Fatalf("%d aborts 300 ms after the last fresh frame, want none yet", n)
		}
		r.step(1) // 301 ms
		want := []string{string(m.Canonical), AbortJSON}
		if got := r.car.sends(); !slices.Equal(got, want) {
			t.Fatalf("the car got %q, want %q: the trip 301 ms after the verb, and no stale HB", got, want)
		}
	})
	t.Run("fresh LIVE and HB stamp liveness", func(t *testing.T) {
		r := newCapRig(t, clock.NewFake())
		s := r.openSeededWith(t, testkit.NewFakeWindow(), params)
		m, hbm := fwd(t), heartbeatMsg(t)
		s.verb(t, m, 0, true)
		r.step(200)
		s.live(t, Fresh, true)
		r.step(101) // 101 ms after the fresh LIVE
		r.step(99)
		s.hb(t, hbm, Fresh, true)
		r.step(300) // 300 ms after the fresh HB
		if n := r.car.count(AbortJSON); n != 0 {
			t.Fatalf("%d aborts while fresh LIVE and HB frames came, want none", n)
		}
		r.step(1)
		want := []string{string(m.Canonical), string(hbm.Canonical), AbortJSON}
		if got := r.car.sends(); !slices.Equal(got, want) {
			t.Fatalf("the car got %q, want %q", got, want)
		}
	})
}

func TestFrameDecodeErrors(t *testing.T) {
	r := newCapRig(t, clock.NewFake())
	w := testkit.NewFakeWindow()
	s := r.openSeeded(t, w)
	r.log.reset()
	hbm := heartbeatMsg(t)
	for _, c := range []struct {
		name string
		typ  byte
		body []byte
	}{
		{"7-byte LIVE", TypeLive, make([]byte, 7)},
		{"9-byte LIVE", TypeLive, make([]byte, 9)},
		{"11-byte VERB", TypeVerb, make([]byte, 11)},
		{"11-byte HB", TypeHB, motionBody(tickWith(w, 0, true), 1, hbm.Canonical)[:11]},
		{"7-byte PRESENCE", TypePresence, make([]byte, 7)},
		{"9-byte PRESENCE", TypePresence, make([]byte, 9)},
		{"CAR frame", TypeCar, []byte{0}},
	} {
		err := s.h.Frame(c.typ, c.body)
		if err == nil {
			t.Errorf("Frame with a %s returned nil, want an error", c.name)
			continue
		}
		if !strings.Contains(err.Error(), frameName(c.typ)) {
			t.Errorf("the error %q for a %s does not name the frame", err, c.name)
		}
	}
	// Invalid JSON in a MSG is dropped: nil, and nothing is sent anywhere.
	if err := s.h.Frame(TypeMsg, []byte(`{"msg_type":"autopark:device_location",`)); err != nil {
		t.Fatalf("Frame with an invalid-JSON MSG returned %v, want nil", err)
	}
	if got := r.calls(); len(got) != 0 {
		t.Fatalf("the decode errors made the calls %q, want none", got)
	}
	if got := w.Sent(TypeCar); len(got) != 1 {
		t.Fatalf("the window has %d CAR frames, want only the location", len(got))
	}
	// A verb that does not conform ends the session; the error names its type and size, never a phone-chosen key.
	bad := `{"msg_type":"autopark:cmd_forward","latitude":52.52009,"longitude":13.405,"client_version":2,"created:timestamp":1,"at 52.123456":1}`
	err := s.h.Frame(TypeVerb, motionBody(tickWith(w, 0, true), 1, []byte(bad)))
	if err == nil {
		t.Fatal("a non-conforming verb returned nil, want an error")
	}
	if strings.Contains(err.Error(), "52.123456") {
		t.Fatalf("the error %q quotes the phone's key", err)
	}
	if !strings.Contains(err.Error(), forwardType) {
		t.Fatalf("the error %q does not name the msg_type", err)
	}
	// A heartbeat that does not conform is dropped and logged by its type and size, never the key.
	odd := `{"msg_type":"autopark:heartbeat_app_webrtc","timestamp":1,"created:timestamp":2,"at 52.654321":1}`
	if err := s.h.Frame(TypeHB, motionBody(tickWith(w, 0, true), 1, []byte(odd))); err != nil {
		t.Fatalf("a non-conforming heartbeat returned %v, want nil (dropped)", err)
	}
	if got := r.car.sends(); len(got) != 0 {
		t.Fatalf("the car got %q from non-conforming frames", got)
	}
	lines := r.linesWith("autopark:heartbeat_app_webrtc")
	if len(lines) != 1 || !strings.Contains(lines[0], fmt.Sprint(len(odd))) {
		t.Fatalf("log lines %q: want one naming the dropped heartbeat and its size %d", lines, len(odd))
	}
	if got := r.linesWith("52.654321"); len(got) != 0 {
		t.Fatalf("the log quotes the phone's key: %q", got)
	}
}

func TestTeardownRunsH4(t *testing.T) { eachOptIn(t, testTeardownRunsH4) }

func testTeardownRunsH4(t *testing.T, params json.RawMessage) {
	r := newCapRig(t, clock.NewFake())
	w := testkit.NewFakeWindow()
	s := r.openSeededWith(t, w, params)
	s.verb(t, verb10m(t, forwardType), 0, true)
	// During H4's emits the capability lock is free: Reply answers 0, and a car frame is dropped at once (R10).
	var replyInH4 atomic.Int32
	var frameInH4 atomic.Int64
	replyInH4.Store(-1)
	r.car.setOnDisconnect(func() {
		got := make(chan int32, 1)
		go func() {
			start := time.Now()
			r.c.CarFrame(carFrame(carlink.ChannelUDP, carEcho))
			frameInH4.Store(int64(time.Since(start)))
			got <- r.c.Reply()
		}()
		select {
		case v := <-got:
			replyInH4.Store(v)
		case <-time.After(time.Second):
			replyInH4.Store(-2) // blocked: the capability lock is held across H4
		}
	})
	r.log.reset()
	s.end(t)
	want := []string{"car.send " + AbortJSON, "car.disconnect", "marker.clear"}
	if got := r.calls(); !slices.Equal(got, want) {
		t.Fatalf("Teardown made the calls %q, want H4 %q", got, want)
	}
	if got := r.linesWith("H4 (close)"); len(got) != 1 {
		t.Fatalf("log lines %q: want H4's line with the teardown reason", got)
	}
	if got := replyInH4.Load(); got != 0 {
		t.Fatalf("Reply() during H4 = %d, want 0 (-2: it blocked)", got)
	}
	if d := time.Duration(frameInH4.Load()); d > 100*time.Millisecond {
		t.Fatalf("a car frame during H4 took %v", d)
	}
	if got := w.Sent(TypeCar); len(got) != 1 {
		t.Fatalf("the window has %d CAR frames, want only the location: a frame during H4 is dropped", len(got))
	}
}

// The presence lease (F5r). A session that opted in with {"presence":1} ends with "lease" once more than 10000 ms
// passed since the issue time of the newest tick echo (PRESENCE, LIVE, HB or VERB) that was at most 3000 ms old; Open
// seeds that stamp. Each r.step is one pass of the D2 loop, which runs the check.

// The opt-in: only an object whose "presence" is the number 1 enforces the lease. No params, null, or an object
// without "presence" keep today's 20 s lease alone. Anything else refuses the session with a fixed log line, which
// never quotes the params or json's error. A session that started logs, once, a fixed line saying which it is (the
// car install's check, docs/SUMMON.md); a refused one logs its refusal alone.
func TestPresenceOptIn(t *testing.T) {
	const refusal = "summon: the session did not start: bad params"
	const off, on, refused = "not enforced", "enforced", "refused"
	const (
		leaseLines = "summon: presence lease "
		onLine     = "summon: presence lease on for this session"
		offLine    = "summon: presence lease off for this session (the phone did not opt in; no Summon camera)"
	)
	for _, c := range []struct {
		params json.RawMessage
		want   string
	}{
		{nil, off},
		{json.RawMessage(`{}`), off},
		{json.RawMessage(`null`), off},
		{json.RawMessage(`{"other":1}`), off},
		{json.RawMessage(`{"presence":1}`), on},
		{json.RawMessage(`{"presence":0}`), refused},
		{json.RawMessage(`{"presence":true}`), refused},
		{json.RawMessage(`{"presence":"1"}`), refused},
		{json.RawMessage(`{"presence":2}`), refused},
		{json.RawMessage(`[]`), refused},
		{json.RawMessage(`"x"`), refused},
		{json.RawMessage(`{"presence":1`), refused},
		// The sentinel: json's error for this number quotes it, and the params carry the VIN.
		{json.RawMessage(`{"presence":52.5200001e400,"vin":"5YJ3E1EA7KF000000"}`), refused},
		{json.RawMessage(`{"lat":13.4050001,"presence":`), refused},
	} {
		name := string(c.params)
		if c.params == nil {
			name = "nil"
		}
		t.Run(c.want+" "+name, func(t *testing.T) {
			r := newCapRig(t, clock.NewFake())
			w := testkit.NewFakeWindow()
			switch c.want {
			case refused:
				if _, err := r.c.Open(context.Background(), w, c.params); err == nil {
					t.Fatal("Open accepted the params")
				}
				if got := r.lines(); !slices.Equal(got, []string{refusal}) {
					t.Fatalf("log lines %q, want %q once", got, refusal)
				}
				if got := r.calls(); len(got) != 0 {
					t.Fatalf("the refused Open made the calls %q", got)
				}
				if got := r.c.Reply(); got != 0 {
					t.Fatalf("Reply() = %d after the refusal, want 0", got)
				}
			case on:
				s := r.openWith(t, w, c.params)
				if got := r.linesWith(leaseLines); !slices.Equal(got, []string{onLine}) {
					t.Fatalf("log lines %q after Open, want %q once", got, onLine)
				}
				r.step(10001) // no echo since Open
				assertLease(t, w, "10001 ms after Open")
				s.waitTorn(t)
			case off:
				r.openWith(t, w, c.params)
				if got := r.linesWith(leaseLines); !slices.Equal(got, []string{offLine}) {
					t.Fatalf("log lines %q after Open, want %q once", got, offLine)
				}
				r.step(60000)
				assertOpen(t, w, "60000 ms after Open")
			}
			noSentinel(t, r.lines())
		})
	}
}

// Review focus 1: a slow but present phone, whose PRESENCE echoes arrive 2900 ms old once a second, stays up for as
// long as it sends them. PRESENCE goes neither to the car nor to the link flag: the car gets exactly the MSGs.
func TestPresenceKeepsIdlePhone(t *testing.T) {
	r := newCapRig(t, clock.NewFake())
	w := testkit.NewFakeWindow()
	s := r.openWith(t, w, presenceOn)
	links := r.linkCalls() // Open's; PRESENCE adds none (ruling P1)
	ka := keepaliveMsg(t)
	var want []string
	for sec := 1; sec <= 600; sec++ {
		r.step(1000)
		s.presence(t, 2900*time.Millisecond, true)
		if sec%5 == 0 {
			s.msg(t, ka)
			want = append(want, string(ka.Canonical))
		}
	}
	assertOpen(t, w, "600 s of echoes 2900 ms old")
	if got := r.car.sends(); !slices.Equal(got, want) {
		t.Fatalf("the car got %d sends, want exactly the %d keepalives", len(got), len(want))
	}
	if got := r.linkCalls(); !slices.Equal(got, links) {
		t.Fatalf("SetLink calls %v, want only Open's %v: PRESENCE touched the link flag", got, links)
	}
	if got := r.linesWith("no fresh presence"); len(got) != 0 {
		t.Fatalf("log lines %q, want no lease end", got)
	}
}

// lateWindow is a FakeWindow whose core is slow to tear down: Done, which starts capRig's teardown, closes only once
// the window has closed and the test called release. Until then the D2 loop still finds the session current, as it
// does in the core while Teardown waits for the window's reader.
type lateWindow struct {
	*testkit.FakeWindow
	done    chan struct{}
	gate    chan struct{}
	release func() // lets the teardown start; idempotent
}

func newLateWindow() *lateWindow {
	w := &lateWindow{FakeWindow: testkit.NewFakeWindow(), done: make(chan struct{}), gate: make(chan struct{})}
	var once sync.Once
	w.release = func() { once.Do(func() { close(w.gate) }) }
	go func() {
		<-w.FakeWindow.Done()
		<-w.gate
		close(w.done)
	}()
	return w
}

func (w *lateWindow) Done() <-chan struct{} { return w.done }

// The lease's end: 10000 ms after the newest fresh echo the session stays, and at 10001 ms it closes with "lease". That
// is logged once, though the D2 loop keeps finding the session current until the core tears it down. The normal
// teardown follows: the live signal's off, then H4.
func TestPresenceExpiryTearsDown(t *testing.T) {
	t.Run("no verb", func(t *testing.T) {
		r, lives := liveRig(t)
		w := newLateWindow()
		s := r.openWith(t, w, presenceOn)
		t.Cleanup(w.release) // runs before openWith's cleanup, which waits for the teardown
		r.step(4000)
		s.presence(t, 0, true) // t0
		r.step(10000)
		assertOpen(t, w, "10000 ms after the echo")
		r.step(1)
		assertLease(t, w, "10001 ms after the echo")
		for range 50 {
			r.step(10)
		}
		if got := r.c.Reply(); got != 1 {
			t.Fatalf("Reply() = %d: the session was no longer current while the loop ran", got)
		}
		w.release()
		s.waitTorn(t)
		const line = "summon: no fresh presence for 10001 ms: ending the session (lease)"
		if got := r.linesWith("summon: no fresh presence for"); !slices.Equal(got, []string{line}) {
			t.Fatalf("log lines %q, want %q once", got, line)
		}
		if got := lives(); !slices.Equal(got, []string{"on", "off"}) {
			t.Fatalf("the live signal's calls %q, want on, then the off at the teardown", got)
		}
		want := []string{"marker.set", "car.connect", "car.disconnect", "marker.clear"}
		if got := r.calls(); !slices.Equal(got, want) {
			t.Fatalf("the calls %q, want Open's, then H4 with no abort: %q", got, want)
		}
		if got := r.linesWith("H4 (lease)"); len(got) != 1 {
			t.Fatalf("log lines %q: want H4's line with the reason lease", r.lines())
		}
	})
	// R9: a verb was held with fresh LIVEs, which then stopped. D2 injected its abort 300 ms later and ended the hold,
	// so the lease's H4 sends no second abort.
	t.Run("a verb was held", func(t *testing.T) {
		r := newCapRig(t, clock.NewFake())
		w := testkit.NewFakeWindow()
		s := r.openSeededWith(t, w, presenceOn)
		r.log.reset()
		m := verb10m(t, forwardType)
		s.verb(t, m, 0, true)
		for range 5 {
			r.step(100)
			s.live(t, 0, true)
		}
		r.step(300)
		if n := r.car.count(AbortJSON); n != 0 {
			t.Fatalf("%d aborts 300 ms after the last LIVE, want none yet", n)
		}
		r.step(1)
		if n := r.car.count(AbortJSON); n != 1 {
			t.Fatalf("%d aborts 301 ms after the last LIVE, want D2's one", n)
		}
		r.step(10000 - 301)
		assertOpen(t, w, "10000 ms after the last LIVE")
		r.step(1)
		assertLease(t, w, "10001 ms after the last LIVE")
		s.waitTorn(t)
		want := []string{"car.send " + string(m.Canonical), "car.send " + AbortJSON, "car.disconnect", "marker.clear"}
		if got := r.calls(); !slices.Equal(got, want) {
			t.Fatalf("the calls %q, want %q: D2's one abort, then H4's disconnect and no second abort", got, want)
		}
	})
}

// Only a known tick at most 3000 ms old stamps presence, and it stamps the tick's issue time, not the frame's arrival:
// holding an echo back gains agw nothing.
func TestPresenceNeedsFreshTick(t *testing.T) {
	t.Run("unknown and late ticks stamp nothing", func(t *testing.T) {
		r := newCapRig(t, clock.NewFake())
		w := testkit.NewFakeWindow()
		s := r.openWith(t, w, presenceOn)
		ka := keepaliveMsg(t)
		echoes := func() {
			s.presence(t, 0, false)
			s.presence(t, 3001*time.Millisecond, true)
			s.msg(t, ka)
		}
		echoes()
		for sec := 1; sec <= 10; sec++ {
			r.step(1000)
			assertOpen(t, w, fmt.Sprintf("%d ms after Open", sec*1000))
			echoes()
		}
		r.step(1)
		assertLease(t, w, "10001 ms after Open")
		s.waitTorn(t)
	})
	t.Run("a tick 3000 ms old stamps its issue time", func(t *testing.T) {
		r := newCapRig(t, clock.NewFake())
		w := testkit.NewFakeWindow()
		s := r.openWith(t, w, presenceOn)
		r.step(5000)
		s.presence(t, 3000*time.Millisecond, true) // issued 2000 ms after Open
		r.step(7000)
		assertOpen(t, w, "12000 ms after Open")
		r.step(1)
		assertLease(t, w, "12001 ms after Open")
		s.waitTorn(t)
	})
	// The age rounds up to the ms, so the stamp is never later than the tick's issue.
	t.Run("a part of a ms counts as a whole one", func(t *testing.T) {
		r := newCapRig(t, clock.NewFake())
		w := testkit.NewFakeWindow()
		s := r.openWith(t, w, presenceOn)
		r.step(5000)
		s.presence(t, 2000*time.Millisecond+time.Microsecond, true) // stamps 2999 ms after Open
		r.step(7999)
		assertOpen(t, w, "12999 ms after Open")
		r.step(1)
		assertLease(t, w, "13000 ms after Open")
		s.waitTorn(t)
	})
}

// A hold's echoes stamp presence as PRESENCE does: LIVE, HB and VERB. VERB and HB stamp before Parse (R6), so an HB
// whose JSON Parse drops still counts.
func TestPresenceFromHold(t *testing.T) {
	odd := []byte(`{"msg_type":"autopark:heartbeat_app_webrtc","timestamp":1,"x":1}`)
	for _, c := range []struct {
		name string
		send func(t *testing.T, r *capRig, s *capSession)
	}{
		{"LIVE", func(t *testing.T, _ *capRig, s *capSession) { s.live(t, 0, true) }},
		{"HB", func(t *testing.T, _ *capRig, s *capSession) { s.hb(t, heartbeatMsg(t), 0, true) }},
		{"VERB", func(t *testing.T, _ *capRig, s *capSession) { s.verb(t, verb10m(t, forwardType), 0, true) }},
		{"HB whose JSON is dropped", func(t *testing.T, r *capRig, s *capSession) {
			s.frame(t, TypeHB, motionBody(tickWith(s.w, 0, true), 7, odd))
			if got := r.linesWith("non-conforming HB"); len(got) != 1 {
				t.Fatalf("log lines %q: want the HB's JSON dropped", r.lines())
			}
		}},
	} {
		t.Run(c.name, func(t *testing.T) {
			r := newCapRig(t, clock.NewFake())
			w := testkit.NewFakeWindow()
			s := r.openSeededWith(t, w, presenceOn)
			r.step(9000)
			c.send(t, r, s)
			r.step(10000)
			assertOpen(t, w, "10000 ms after the echo")
			r.step(1)
			assertLease(t, w, "10001 ms after the echo")
			s.waitTorn(t)
		})
	}
}

// R7: a clock that ran backwards below the stamp counts as expired: fail closed. One that ran backwards but not below
// the stamp measures from it as usual.
func TestPresenceClockBackwards(t *testing.T) {
	clk := &stepClock{}
	clk.ms.Store(50_000)
	r := newCapRig(t, clk)
	w := testkit.NewFakeWindow()
	s := r.openWith(t, w, presenceOn)
	clk.ms.Store(55_000)
	s.presence(t, 0, true) // stamps 55000
	clk.ms.Store(58_000)
	r.c.tick()
	assertOpen(t, w, "3000 ms after the stamp")
	clk.ms.Store(56_000)
	r.c.tick()
	assertOpen(t, w, "the clock back to 1000 ms after the stamp")
	clk.ms.Store(54_999)
	r.c.tick()
	assertLease(t, w, "the clock back below the stamp")
	s.waitTorn(t)
	if got := r.linesWith("summon: no fresh presence for"); len(got) != 1 {
		t.Fatalf("log lines %q, want the lease's end once", got)
	}
}

// Review focus 3: a phone that sends PRESENCE without opting in (a build ahead of its opt-in) is accepted and stamped,
// and never enforced: no error, no close, and nothing to the car or the link flag.
func TestPresenceNotEnforcedAcceptsPresence(t *testing.T) {
	r := newCapRig(t, clock.NewFake())
	w := testkit.NewFakeWindow()
	s := r.openWith(t, w, json.RawMessage(`{}`))
	r.log.reset()
	links := r.linkCalls()
	r.step(1000)
	s.presence(t, 0, true)
	if got, want := s.h.(*session).echoAt.Load(), r.fake.BootMS(); got != want {
		t.Fatalf("the stamp is %d after a fresh PRESENCE at %d: an opted-out session stamps too", got, want)
	}
	s.presence(t, 5*time.Second, true)
	s.presence(t, 0, false)
	r.step(60000)
	s.presence(t, 3001*time.Millisecond, true)
	r.step(60000)
	assertOpen(t, w, "120 s after the last fresh PRESENCE")
	if got := r.calls(); len(got) != 0 {
		t.Fatalf("the PRESENCE frames made the calls %q, want none", got)
	}
	if got := r.linkCalls(); !slices.Equal(got, links) {
		t.Fatalf("SetLink calls %v, want only Open's %v", got, links)
	}
	if got := r.linesWith("no fresh presence"); len(got) != 0 {
		t.Fatalf("log lines %q: the lease ended a session that did not opt in", got)
	}
}

// Review focus 2: after a lease end, the next opted-in session's lease runs from its own Open, and it has a live
// signal of its own.
func TestPresenceReconnectSeedsFresh(t *testing.T) {
	r, lives := liveRig(t)
	w1 := testkit.NewFakeWindow()
	s1 := r.openWith(t, w1, presenceOn)
	r.step(10001)
	assertLease(t, w1, "the first session, 10001 ms after its Open")
	s1.waitTorn(t)
	r.step(3000)
	w2 := testkit.NewFakeWindow()
	s2 := r.openWith(t, w2, presenceOn) // t
	r.step(10000)
	assertOpen(t, w2, "the next session, 10000 ms after its Open")
	r.step(1)
	assertLease(t, w2, "the next session, 10001 ms after its Open")
	s2.waitTorn(t)
	if got := lives(); !slices.Equal(got, []string{"on", "off", "on", "off"}) {
		t.Fatalf("the live signal's calls %q, want on and off for each session", got)
	}
	if got := r.linesWith("summon: no fresh presence for"); len(got) != 2 {
		t.Fatalf("log lines %q, want the lease's end once per session", got)
	}
}

func TestTeardownEndsForwarder(t *testing.T) {
	// Each session's forwarder holds a queue of CarQueue frames. Teardown ends it, so sessions leave no goroutines.
	r := newCapRig(t, clock.NewFake())
	before := runtime.NumGoroutine()
	for range 100 {
		w := testkit.NewFakeWindow()
		s := r.open(t, w)
		r.c.CarFrame(carFrame(carlink.ChannelUDP, carEcho))
		waitCar(t, w, 1)
		s.end(t)
	}
	eventually(t, "end of the sessions' goroutines", func() bool { return runtime.NumGoroutine() <= before+10 })
}

func TestCarChannelEncoding(t *testing.T) {
	r := newCapRig(t, clock.NewFake())
	w := testkit.NewFakeWindow()
	r.open(t, w)
	tcp := []byte(`{"msg_type":"autopark:status","n":1}`)
	other := []byte(`{"msg_type":"autopark:status","n":2}`)
	last := []byte(`{"msg_type":"autopark:status","n":3}`)
	r.c.CarFrame(carFrame(carlink.ChannelTCP, tcp))
	r.c.CarFrame(carFrame(carlink.ChannelUDP, carEcho))
	r.c.CarFrame(carFrame("x", other))
	r.c.CarFrame(carFrame(carlink.ChannelTCP, last)) // queued after "x": once it arrives, "x" was handled
	want := [][]byte{carBody(0, tcp), carBody(1, carEcho), carBody(0, last)}
	waitCar(t, w, 3)
	if got := w.Sent(TypeCar); !reflect.DeepEqual(got, want) {
		t.Fatalf("the window got %q, want %q: dcTcp as 0, dcUdp as 1, and \"x\" dropped", got, want)
	}
}

func TestOversizeCarFrameDropped(t *testing.T) {
	r := newCapRig(t, clock.NewFake())
	w := testkit.NewFakeWindow()
	r.open(t, w)
	const pad = "52.987654"
	big := jsonOfSize(t, "autopark:viz", pad, MaxCarJSON+1)
	edge := jsonOfSize(t, "autopark:viz", pad, MaxCarJSON)
	r.c.CarFrame(carFrame(carlink.ChannelUDP, big))
	r.c.CarFrame(carFrame(carlink.ChannelUDP, edge)) // queued after the big one
	waitCar(t, w, 1)
	if got := w.Sent(TypeCar); !reflect.DeepEqual(got, [][]byte{carBody(1, edge)}) {
		t.Fatalf("the window got %d CAR frames, want only the %d-byte one", len(got), MaxCarJSON)
	}
	lines := r.linesWith("autopark:viz")
	if len(lines) != 1 || !strings.Contains(lines[0], fmt.Sprint(MaxCarJSON+1)) {
		t.Fatalf("log lines %q: want one with the msg_type and the size %d", lines, MaxCarJSON+1)
	}
	if got := r.linesWith(pad); len(got) != 0 {
		t.Fatalf("the log carries a field value: %q", got)
	}
	select {
	case <-w.Done():
		t.Fatalf("the oversize frame ended the session (%q)", w.CloseReason())
	default:
	}
}

func TestCarFramesWhilePhoneStalled(t *testing.T) {
	r := newCapRig(t, clock.NewFake())
	w := testkit.NewFakeWindow()
	s := r.openSeeded(t, w)
	w.FailTrySend(true)
	for i := range 100 {
		f := carFrame(carlink.ChannelUDP, carEcho)
		start := time.Now()
		r.c.CarFrame(f)
		if d := time.Since(start); d > time.Millisecond {
			t.Errorf("CarFrame %d took %v, want at most 1 ms", i, d)
		}
	}
	s.waitTorn(t)
	if got := w.CloseReason(); got != "error" {
		t.Fatalf("the window closed with %q, want error", got)
	}
	r.waitCall(t, "car.disconnect")
	// Once the window closed, the later frames never reached the engine: it asked for the end once.
	if got := r.linesWith("ending the session"); len(got) != 1 {
		t.Fatalf("log lines %q: want the engine to ask for the end once", got)
	}
}

func TestFramesBetweenSessionsDropped(t *testing.T) {
	r := newCapRig(t, clock.NewFake())
	w1 := testkit.NewFakeWindow()
	s1 := r.openSeeded(t, w1)
	const high = wallMS + 1_000_000
	r.c.CarFrame(carFrame(carlink.ChannelTCP, resultJSON(forwardType, true, "", high)))
	waitCar(t, w1, 2) // session 1's engine saw the high timestamp
	s1.end(t)

	// A late cmd_result between the sessions, then the next session at once.
	late := resultJSON(forwardType, true, "", high+1)
	r.c.CarFrame(carFrame(carlink.ChannelTCP, late))
	if got := r.linesWith("between sessions"); len(got) != 1 || !strings.Contains(got[0], fmt.Sprint(len(late))) {
		t.Fatalf("log lines %q: want one for the dropped frame, with its size %d", got, len(late))
	}
	style := []byte(`{"msg_type":"autopark:style","n":2}`)
	r.car.setOnConnect(func() { r.c.CarFrame(carFrame(carlink.ChannelTCP, style)) })
	w2 := testkit.NewFakeWindow()
	s2 := r.open(t, w2)
	waitCar(t, w2, 1)
	if got := w2.Sent(TypeCar); !reflect.DeepEqual(got, [][]byte{carBody(0, style)}) {
		t.Fatalf("the second window got %q, want only its own H1 push", got)
	}
	// The second session starts clean: a stale verb's refusal is stamped with Now, not an old highest timestamp, and
	// it has no car position from the first session.
	m := verb10m(t, forwardType)
	s2.verb(t, m, 0, false)
	s2.msg(t, abortMsg(t))
	s2.verb(t, m, 0, true)
	s2.msg(t, abortMsg(t))
	cars := w2.Sent(TypeCar)
	if len(cars) != 3 {
		t.Fatalf("the second window has %d CAR frames, want its push and two refusals", len(cars))
	}
	if ts := refusalAt(t, cars[1], forwardType, "action_invalid"); ts != fmt.Sprint(wallMS) {
		t.Fatalf("the stale verb's refusal has timestamp %s, want Now (%d)", ts, wallMS)
	}
	refusalAt(t, cars[2], forwardType, "invalid_vehicle_location")
}

// gatedWindow is a FakeWindow whose TrySend waits until gate is closed: a phone link that stops taking frames.
type gatedWindow struct {
	*testkit.FakeWindow
	gate    chan struct{}
	entered chan struct{} // closed when the first TrySend arrived
	once    sync.Once
}

func (g *gatedWindow) TrySend(t byte, body []byte) bool {
	g.once.Do(func() { close(g.entered) })
	<-g.gate
	return g.FakeWindow.TrySend(t, body)
}

func TestQueueOverflowEndsSession(t *testing.T) {
	r := newCapRig(t, clock.NewFake())
	g := &gatedWindow{FakeWindow: testkit.NewFakeWindow(), gate: make(chan struct{}), entered: make(chan struct{})}
	s := r.open(t, g)
	var openGate sync.Once
	t.Cleanup(func() { openGate.Do(func() { close(g.gate) }) })
	push := func(i int) {
		start := time.Now()
		r.c.CarFrame(carFrame(carlink.ChannelUDP, carEcho))
		if d := time.Since(start); d > time.Millisecond {
			t.Errorf("CarFrame %d took %v, want at most 1 ms", i, d)
		}
	}
	push(0)
	select {
	case <-g.entered: // the forwarder holds frame 0, stuck in TrySend
	case <-time.After(2 * time.Second):
		t.Fatal("the first car frame never reached the window")
	}
	for i := 1; i < CarQueue+2; i++ {
		push(i)
	}
	select {
	case <-g.Done():
		t.Fatalf("the window closed (%q) before the forwarder could act", g.CloseReason())
	default:
	}
	openGate.Do(func() { close(g.gate) })
	s.waitTorn(t)
	if got := g.CloseReason(); got != "error" {
		t.Fatalf("the window closed with %q, want error", got)
	}
	r.waitCall(t, "car.disconnect")
	if got := r.linesWith("overflow"); len(got) != 1 {
		t.Fatalf("log lines %q: want one about the overflow", got)
	}
}

func TestType9ClosesSession(t *testing.T) {
	r := newCapRig(t, clock.NewFake())
	type9 := func(v any) map[string]any {
		return map[string]any{"type": v, "uid": carlink.PhoneUID, "session_id": ""}
	}
	r.c.Signaling(type9(int32(9))) // no session: nothing to close
	w := testkit.NewFakeWindow()
	s := r.open(t, w)
	for _, v := range []any{int32(8), float64(9), "9", nil, uint8(10)} {
		r.c.Signaling(type9(v))
	}
	r.c.Signaling(map[string]any{"uid": carlink.PhoneUID})
	select {
	case <-w.Done():
		t.Fatalf("a signaling without an integer type 9 closed the window (%q)", w.CloseReason())
	default:
	}
	r.c.Signaling(type9(int32(9))) // what QtCarServer sends 60 s after a disconnect
	s.waitTorn(t)
	if got := w.CloseReason(); got != "close" {
		t.Fatalf("type 9 closed the window with %q, want close", got)
	}
	r.waitCall(t, "car.disconnect")
	for _, v := range []any{uint32(9), int64(9), int(9), uint16(9)} {
		w := testkit.NewFakeWindow()
		s := r.open(t, w)
		r.c.Signaling(type9(v))
		s.waitTorn(t)
		if got := w.CloseReason(); got != "close" {
			t.Fatalf("type %T(9) closed the window with %q, want close", v, got)
		}
	}
}

func TestLinkWired(t *testing.T) { eachOptIn(t, testLinkWired) }

func testLinkWired(t *testing.T, params json.RawMessage) {
	r := newCapRig(t, clock.NewFake())
	s := r.openSeededWith(t, testkit.NewFakeWindow(), params)
	for range 3 {
		s.live(t, 110*time.Millisecond, true)
	}
	if got := r.linkCalls(); !slices.Equal(got, []bool{true}) {
		t.Fatalf("SetLink calls %v after three LIVEs 110 ms late, want [true]", got)
	}
	s.end(t)
	if got := r.linkCalls(); !slices.Equal(got, []bool{true, false}) {
		t.Fatalf("SetLink calls %v after the session ended, want [true false]", got)
	}
}

func TestLogsCarFrameCaller(t *testing.T) {
	r := newCapRig(t, clock.NewFake())
	w := testkit.NewFakeWindow()
	s := r.open(t, w)
	const secret = "caller-data-52.111111"
	data := []byte(`{"msg_type":"autopark:status","note":"` + secret + `"}`)
	a := carlink.CarFrame{Channel: carlink.ChannelTCP, UID: "test_phone", Sender: ":1.5", Data: data}
	b := carlink.CarFrame{Channel: carlink.ChannelTCP, UID: "other_uid", Sender: ":1.9", Data: data}
	for _, f := range []carlink.CarFrame{a, a, a} {
		r.c.CarFrame(f)
	}
	waitCar(t, w, 3)
	if got := r.linesWith(`":1.5"`); len(got) != 1 || !strings.Contains(got[0], "test_phone") {
		t.Fatalf("log lines %q: want the first caller and its uid logged once", got)
	}
	r.c.CarFrame(b)
	waitCar(t, w, 4)
	if got := r.linesWith(`":1.9"`); len(got) != 1 || !strings.Contains(got[0], "other_uid") {
		t.Fatalf("log lines %q: want the second caller and its uid logged once", got)
	}
	r.c.CarFrame(a) // the caller changes back
	r.c.CarFrame(a)
	waitCar(t, w, 6)
	if got := r.linesWith(`":1.5"`); len(got) != 2 {
		t.Fatalf("log lines %q: want the first caller logged again when it comes back", got)
	}
	// The next session logs its first caller afresh.
	s.end(t)
	w2 := testkit.NewFakeWindow()
	r.open(t, w2)
	r.c.CarFrame(b)
	waitCar(t, w2, 1)
	if got := r.linesWith(`":1.9"`); len(got) != 2 {
		t.Fatalf("log lines %q: want the next session's first caller logged", got)
	}
	if got := r.linesWith(secret); len(got) != 0 {
		t.Fatalf("the log carries a car frame's data: %q", got)
	}
}

func TestLoopStampsBeforeLock(t *testing.T) {
	t.Run("real time", func(t *testing.T) {
		r := newCapRig(t, clock.Boot())
		stuck := make(chan struct{}) // closed when the trip's Send has blocked, holding the engine lock
		release := make(chan struct{})
		var once sync.Once
		r.car.setOnSend(func(data []byte) {
			if string(data) == AbortJSON {
				once.Do(func() { close(stuck) })
				<-release
			}
		})
		s := r.openSeeded(t, testkit.NewFakeWindow())
		ctx, cancel := context.WithCancel(context.Background())
		loopDone := make(chan struct{})
		go func() {
			defer close(loopDone)
			r.c.Loop(ctx)
		}()
		t.Cleanup(func() {
			close(release)
			cancel()
			select {
			case <-loopDone:
			case <-time.After(time.Second):
				t.Error("Loop did not return within 1 s of its context's end")
			}
		})

		s.verb(t, verb10m(t, forwardType), 0, true) // no LIVE follows: the loop trips 300 ms later
		select {
		case <-stuck:
		case <-time.After(2 * time.Second):
			t.Fatal("no trip within 2 s of the verb")
		}
		at := time.Now()
		progress := func() time.Duration {
			var p time.Duration
			within(t, 100*time.Millisecond, "Progress", func() { p = r.c.Progress() })
			return p
		}
		if p := progress(); p > MaxProgress {
			t.Fatalf("Progress() = %v when the engine locked up: the loop was not stamping", p)
		}
		for p := progress(); p <= MaxProgress; p = progress() {
			if time.Since(at) > 400*time.Millisecond {
				t.Fatalf("Progress() = %v 400 ms after the engine locked up, want over %v", p, MaxProgress)
			}
			time.Sleep(time.Millisecond)
		}
		// The engine still holds its lock. The bus handlers and the predicates never wait for it.
		within(t, 100*time.Millisecond, "Reply", func() {
			if got := r.c.Reply(); got != 1 {
				t.Errorf("Reply() = %d, want 1", got)
			}
		})
		within(t, 100*time.Millisecond, "CarFrame", func() { r.c.CarFrame(carFrame(carlink.ChannelUDP, carEcho)) })
		within(t, 100*time.Millisecond, "End", func() { r.c.End("error") })
	})
	t.Run("the stamp comes first", func(t *testing.T) {
		// One pass of the loop on the fake clock, whose Tick trips into a Send that blocks. The pass stamped before it
		// took any lock, so Progress is 0 while the engine is stuck, and then grows with the clock.
		r := newCapRig(t, clock.NewFake())
		stuck := make(chan struct{})
		release := make(chan struct{})
		var once sync.Once
		r.car.setOnSend(func(data []byte) {
			if string(data) == AbortJSON {
				once.Do(func() { close(stuck) })
				<-release
			}
		})
		s := r.openSeeded(t, testkit.NewFakeWindow())
		s.verb(t, verb10m(t, forwardType), 0, true)
		r.fake.Advance(301) // the next pass trips
		passed := make(chan struct{})
		go func() {
			defer close(passed)
			r.c.tick()
		}()
		t.Cleanup(func() {
			close(release)
			<-passed
		})
		select {
		case <-stuck:
		case <-time.After(2 * time.Second):
			t.Fatal("the pass did not trip")
		}
		progress := func() time.Duration {
			var p time.Duration
			within(t, 100*time.Millisecond, "Progress", func() { p = r.c.Progress() })
			return p
		}
		if got := progress(); got != 0 {
			t.Fatalf("Progress() = %v while the pass is stuck in Tick, want 0: the stamp comes before the lock", got)
		}
		r.fake.Advance(uint64(MaxProgress/time.Millisecond) + 1)
		if got := progress(); got <= MaxProgress {
			t.Fatalf("Progress() = %v with the engine stuck for longer than %v", got, MaxProgress)
		}
	})
}

// stepClock is a BOOTTIME clock a test sets, backwards too.
type stepClock struct{ ms atomic.Uint64 }

func (c *stepClock) BootMS() uint64 { return c.ms.Load() }

func TestProgress(t *testing.T) {
	clk := &stepClock{}
	clk.ms.Store(5000)
	r := newCapRig(t, clk)
	if got := r.c.Progress(); got != 0 {
		t.Fatalf("Progress() = %v at construction, want 0: NewCapability seeds the stamp", got)
	}
	clk.ms.Store(5120)
	if got := r.c.Progress(); got != 120*time.Millisecond {
		t.Fatalf("Progress() = %v, want 120ms", got)
	}
	r.c.tick()
	if got := r.c.Progress(); got != 0 {
		t.Fatalf("Progress() = %v right after a pass of the loop, want 0", got)
	}
	clk.ms.Store(5000) // a clock that ran backwards: fail closed
	if got := r.c.Progress(); got <= MaxProgress {
		t.Fatalf("Progress() = %v with a clock that ran backwards, want over %v", got, MaxProgress)
	}
}

func TestNewCapabilityNeedsItsParts(t *testing.T) {
	car, marker := &fakeCar{log: &callLog{}}, &fakeMarker{log: &callLog{}}
	full := CapConfig{Car: car, Marker: marker, Clock: clock.NewFake(), Fatal: func(error) {}}
	for name, cut := range map[string]func(*CapConfig){
		"Car":    func(c *CapConfig) { c.Car = nil },
		"Marker": func(c *CapConfig) { c.Marker = nil },
		"Clock":  func(c *CapConfig) { c.Clock = nil },
		"Fatal":  func(c *CapConfig) { c.Fatal = nil },
	} {
		cfg := full
		cut(&cfg)
		func() {
			defer func() {
				if recover() == nil {
					t.Errorf("NewCapability without %s did not panic", name)
				}
			}()
			NewCapability(cfg)
		}()
	}
}
