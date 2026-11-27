package server

import (
	"context"
	"fmt"
	"log"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"airgapp/gateway/capability"
	"airgapp/gateway/capability/echo"
	"airgapp/gateway/internal/authority"
	"airgapp/gateway/internal/proto"
)

// switchOracle is an authority.Fake a test can change while the server runs,
// counting calls.
type switchOracle struct {
	mu    sync.Mutex
	fake  authority.Fake
	calls atomic.Int32
}

func (o *switchOracle) set(f authority.Fake) {
	o.mu.Lock()
	o.fake = f
	o.mu.Unlock()
}

func (o *switchOracle) get() authority.Fake {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.fake
}

func (o *switchOracle) KeyRole(ctx context.Context, pk []byte) (int32, error) {
	o.calls.Add(1)
	return o.get().KeyRole(ctx, pk)
}

func (o *switchOracle) AuthOwnerIsAuthd(ctx context.Context) (bool, error) {
	o.calls.Add(1)
	return o.get().AuthOwnerIsAuthd(ctx)
}

// fakeNow is the re-check loop's clock.
type fakeNow struct {
	mu sync.Mutex
	t  time.Time
}

func (c *fakeNow) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fakeNow) Add(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

var denyKey = authority.Fake{Role: 2, Owner: true, Err: fmt.Errorf("helper says: %w", authority.ErrKeyUnknown)}

// recheckEnv is newEnv with a switchable OWNER oracle, a fake clock and a
// tick channel for the re-check loop. It returns once the loop runs (one
// tick taken, nothing open), so the period starts at the clock's start.
func recheckEnv(t *testing.T, opts ...func(*env)) (*env, *switchOracle, *fakeNow) {
	t.Helper()
	o := &switchOracle{fake: authority.Fake{Role: 2, Owner: true}}
	now := &fakeNow{t: time.Unix(1_800_000_000, 0)}
	all := append([]func(*env){func(e *env) {
		e.oracleImpl = o
		e.recheckTick = make(chan time.Time)
		e.now = now.Now
	}}, opts...)
	e := newEnv(t, all...)
	e.tick()
	return e, o, now
}

// tick hands the re-check loop one tick; it returns once the loop took it,
// so the loop has finished whatever it did before.
func (e *env) tick() {
	e.t.Helper()
	select {
	case e.recheckTick <- time.Time{}:
	case <-time.After(3 * time.Second):
		e.t.Fatal("the re-check loop is not taking ticks")
	}
}

// revokedReason waits for the window's session-log line and returns its reason.
func (e *env) revokedReason() string {
	e.t.Helper()
	l := e.logLines(1)[0]
	r, _ := l["reason"].(string)
	if !strings.HasPrefix(r, "revoked: authority re-check (") {
		e.t.Fatalf("session-log reason %q, want the re-check's", r)
	}
	return r
}

func TestRecheckOnKeysChangedRevokes(t *testing.T) {
	e, o, _ := recheckEnv(t)
	cl := e.open(echo.ID)
	cl.echo([]byte("x"))
	o.set(denyKey)
	e.srv.KeysChanged("keys_updated")
	t0 := time.Now()
	cl.expectBye("revoked")
	if d := time.Since(t0); d > time.Second {
		t.Fatalf("revoked after %v", d)
	}
	r := e.revokedReason()
	if !strings.Contains(r, "keys_changed keys_updated") || !strings.Contains(r, "helper says") {
		t.Fatalf("reason %q must say what triggered the re-check and why it failed", r)
	}
	waitFor(t, time.Second, "slot free", func() bool { return !e.srv.slotOpen(echo.ID) })
}

// A capability sees a re-check revocation exactly as "revoked":
// Teardown("revoked") (SUMMON maps its fixed reasons exactly). Why stays in
// the session log and the stderr log.
func TestRecheckTeardownReasonIsRevoked(t *testing.T) {
	logs := captureLog(t)
	c := newCountCap(echo.New(), 0, "")
	e, o, _ := recheckEnv(t, func(e *env) { e.caps = []capability.Capability{c} })
	cl := e.open(echo.ID)
	o.set(denyKey)
	e.srv.KeysChanged("keys_updated")
	cl.expectBye("revoked")
	select {
	case r := <-c.reasons:
		if r != "revoked" {
			t.Fatalf("Teardown(%q), want exactly \"revoked\"", r)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no Teardown")
	}
	r := e.revokedReason()
	if !strings.Contains(r, "keys_changed keys_updated") || !strings.Contains(r, "helper says") {
		t.Fatalf("session-log reason %q", r)
	}
	waitFor(t, 2*time.Second, "the closed line", func() bool { return strings.Contains(logs.String(), "ECHO closed") })
	if l := logs.String(); !strings.Contains(l, `reason="revoked: authority re-check (keys_changed keys_updated)`) ||
		!strings.Contains(l, "bye=revoked") {
		t.Fatalf("stderr log:\n%s", l)
	}
}

func TestRecheckPassingKeepsTheWindow(t *testing.T) {
	e, o, now := recheckEnv(t)
	cl := e.open(echo.ID)
	before := o.calls.Load()
	e.srv.KeysChanged("resubscribed")
	waitFor(t, 2*time.Second, "the re-check", func() bool { return o.calls.Load() >= before+2 })
	e.tick()
	if n := o.calls.Load(); n != before+2 {
		t.Fatalf("%d oracle calls for one re-check, want 2 (owner, key)", n-before)
	}
	cl.echo([]byte("still open"))
	now.Add(10 * time.Second) // the watch is down: due again
	e.tick()
	e.tick()
	if n := o.calls.Load(); n != before+4 {
		t.Fatalf("%d oracle calls, want 4", n-before)
	}
	cl.echo([]byte("still open"))
}

func TestRecheckEvery30sWhileTheWatchIsUp(t *testing.T) {
	e, o, now := recheckEnv(t)
	e.srv.SetWatchUp(true)
	cl := e.open(echo.ID)
	o.set(denyKey)
	before := o.calls.Load()
	now.Add(29 * time.Second)
	e.tick()
	e.tick()
	cl.echo([]byte("not yet"))
	if n := o.calls.Load(); n != before {
		t.Fatalf("re-checked after 29 s with the watch up (%d calls)", n-before)
	}
	now.Add(time.Second)
	e.tick()
	cl.expectBye("revoked")
	if r := e.revokedReason(); !strings.Contains(r, "every 30 s") {
		t.Fatalf("reason %q", r)
	}
}

func TestRecheckEvery10sWhileTheWatchIsDown(t *testing.T) {
	e, o, now := recheckEnv(t) // the watch is down until the helper says up
	cl := e.open(echo.ID)
	o.set(denyKey)
	now.Add(9 * time.Second)
	e.tick()
	e.tick()
	cl.echo([]byte("not yet"))
	now.Add(time.Second)
	e.tick()
	cl.expectBye("revoked")
	if r := e.revokedReason(); !strings.Contains(r, "watch down") {
		t.Fatalf("reason %q", r)
	}
}

// Re-checks ask Config.RecheckOracle (agw gives it its own helper
// connection, so a re-check never waits behind a sign-in); sign-ins ask
// Config.Oracle.
func TestRecheckAsksTheRecheckOracle(t *testing.T) {
	re := &switchOracle{fake: denyKey}
	e, o, _ := recheckEnv(t, func(e *env) { e.recheck = re })
	cl := e.open(echo.ID) // signs in through o, which passes it
	signIn := o.calls.Load()
	e.srv.KeysChanged("keys_updated")
	cl.expectBye("revoked")
	if re.calls.Load() == 0 {
		t.Fatal("the re-check did not ask RecheckOracle")
	}
	if n := o.calls.Load(); n != signIn {
		t.Fatalf("the re-check asked the sign-in oracle (%d calls)", n-signIn)
	}
}

// The watch going down (or its connection being lost) moves the period to
// 10 s at once.
func TestRecheckWatchGoesDown(t *testing.T) {
	e, o, now := recheckEnv(t)
	e.srv.SetWatchUp(true)
	cl := e.open(echo.ID)
	o.set(denyKey)
	now.Add(10 * time.Second)
	e.tick()
	e.tick()
	cl.echo([]byte("watch up: not yet"))
	e.srv.SetWatchUp(false)
	e.tick()
	cl.expectBye("revoked")
}

// An authority that does not answer in time is a failed check.
func TestRecheckUnreachableRevokes(t *testing.T) {
	e, o, _ := recheckEnv(t)
	cl := e.open(echo.ID)
	o.set(authority.Fake{Role: 2, Owner: true, Delay: 2 * time.Second})
	e.srv.KeysChanged("owner_changed")
	cl.expectBye("revoked")
	if r := e.revokedReason(); !strings.Contains(r, "deadline") {
		t.Fatalf("reason %q", r)
	}
}

// Every open window is re-checked; a key is asked once per pass.
func TestRecheckEveryWindowOneCheckPerKey(t *testing.T) {
	c1 := newCountCap(echo.New(), 0, "")
	c2 := newCountCap(echo.New(), proto.CapCamera, "ECHO2")
	e, o, now := recheckEnv(t, func(e *env) { e.caps = []capability.Capability{c1, c2} })
	a := e.open(echo.ID)
	b := e.open(proto.CapCamera)
	before := o.calls.Load()
	e.srv.KeysChanged("keys_updated")
	waitFor(t, 2*time.Second, "the re-check", func() bool { return o.calls.Load() >= before+2 })
	e.tick()
	if n := o.calls.Load(); n != before+2 {
		t.Fatalf("%d oracle calls for one key in two windows, want 2", n-before)
	}
	o.set(denyKey)
	now.Add(recheckKickGap) // the next keys_changed pass
	e.srv.KeysChanged("keys_updated")
	a.expectBye("revoked")
	b.expectBye("revoked")
}

// kickTaken waits until the loop took the pending kick and finished with it
// (it takes a tick only between passes).
func (e *env) kickTaken() {
	e.t.Helper()
	waitFor(e.t, 2*time.Second, "the loop to take the kick", func() bool { return len(e.srv.kick) == 0 })
	e.tick()
}

// keys_changed passes start at least recheckKickGap apart: a burst of
// notices costs one pass at once and one more after the gap, which covers
// every notice that came before it (the helper's rate limit is shared with
// the sign-ins).
func TestRecheckKeysChangedPassesArePaced(t *testing.T) {
	e, o, now := recheckEnv(t)
	e.srv.SetWatchUp(true)
	cl := e.open(echo.ID)
	before := o.calls.Load()
	e.srv.KeysChanged("keys_updated") // at once
	waitFor(t, 2*time.Second, "the first pass", func() bool { return o.calls.Load() >= before+2 })
	for i := 0; i < 20; i++ {
		e.srv.KeysChanged("keys_updated")
		e.kickTaken()
	}
	now.Add(recheckKickGap - time.Millisecond)
	e.tick()
	if n := o.calls.Load(); n != before+2 {
		t.Fatalf("%d oracle calls within the gap, want one pass (2)", n-before)
	}
	cl.echo([]byte("still open"))
	o.set(denyKey) // the last notice's change
	now.Add(time.Millisecond)
	e.tick()
	cl.expectBye("revoked")
	if r := e.revokedReason(); !strings.Contains(r, "keys_changed keys_updated") {
		t.Fatalf("reason %q", r)
	}
	if n := o.calls.Load(); n != before+3 {
		t.Fatalf("%d oracle calls, want the first pass (2) and one held pass (1)", n-before)
	}
}

// A key change between a sign-in's check and its window taking the slot
// still reaches that window.
func TestRecheckKeysChangedDuringSignIn(t *testing.T) {
	var once sync.Once
	var srv atomic.Pointer[Server]
	var orc atomic.Pointer[switchOracle]
	e, o, _ := recheckEnv(t, func(en *env) {
		en.afterCheck = func() {
			once.Do(func() {
				orc.Load().set(denyKey)
				srv.Load().KeysChanged("keys_updated")
				time.Sleep(50 * time.Millisecond) // the re-check runs before the window is in the registry
			})
		}
	})
	srv.Store(e.srv)
	orc.Store(o)
	cl := e.open(echo.ID)
	cl.expectBye("revoked")
	if r := e.revokedReason(); !strings.Contains(r, "during sign-in") {
		t.Fatalf("reason %q", r)
	}
}

// KeysChanged and SetWatchUp never wait for a re-check in progress.
func TestRecheckTriggersNeverBlock(t *testing.T) {
	e, o, _ := recheckEnv(t)
	cl := e.open(echo.ID)
	o.set(authority.Fake{Role: 2, Owner: true, Delay: 150 * time.Millisecond})
	start := time.Now()
	for i := 0; i < 1000; i++ {
		e.srv.KeysChanged("keys_updated")
		e.srv.SetWatchUp(i%2 == 0)
	}
	if d := time.Since(start); d > 100*time.Millisecond {
		t.Fatalf("1000 triggers took %v", d)
	}
	cl.echo([]byte("passing re-checks keep it"))
}

// stdLog captures the standard logger (the server's stderr log) for one test.
type stdLog struct {
	mu sync.Mutex
	b  strings.Builder
}

func (l *stdLog) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *stdLog) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

func captureLog(t *testing.T) *stdLog {
	l := &stdLog{}
	old := log.Writer()
	log.SetOutput(l)
	t.Cleanup(func() { log.SetOutput(old) })
	return l
}

// Shutdown stops a re-check in flight without revoking: it is not a verdict.
func TestRecheckStopsOnShutdown(t *testing.T) {
	logs := captureLog(t)
	base := runtime.NumGoroutine()
	e, o, _ := recheckEnv(t)
	cl := e.open(echo.ID)
	o.set(authority.Fake{Role: 2, Owner: true, Delay: time.Hour})
	before := o.calls.Load()
	e.srv.KeysChanged("keys_updated")
	waitFor(t, 2*time.Second, "the re-check in flight", func() bool { return o.calls.Load() > before })
	start := time.Now()
	e.close()
	if d := time.Since(start); d > 400*time.Millisecond {
		t.Fatalf("Shutdown took %v with a re-check in flight", d)
	}
	cl.expectBye("error") // Shutdown("test"), not "revoked"
	if r := e.logLines(1)[0]["reason"]; r != "test" {
		t.Fatalf("reason %v", r)
	}
	settleGoroutines(t, base)
	if strings.Contains(logs.String(), "revoking") { // Shutdown waited for the loop
		t.Fatalf("a re-check cut short by Shutdown revoked:\n%s", logs.String())
	}
}

// The period starts when a window opens, not at the last re-check before it.
func TestRecheckPeriodStartsWithTheWindow(t *testing.T) {
	e, o, now := recheckEnv(t)
	e.srv.SetWatchUp(true)
	now.Add(25 * time.Second)
	e.tick() // nothing open
	cl := e.open(echo.ID)
	o.set(denyKey)
	now.Add(10 * time.Second)
	e.tick()
	e.tick()
	cl.echo([]byte("10 s after it opened: not due"))
	now.Add(20 * time.Second)
	e.tick()
	cl.expectBye("revoked")
}
