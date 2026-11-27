package server

import (
	"context"
	"encoding/json"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"airgapp/gateway/capability"
	"airgapp/gateway/capability/echo"
)

// hookCap is ECHO with overridable PreArm/Open. A nil hook uses ECHO's.
// Handlers it returns count Teardown calls.
type hookCap struct {
	capability.Capability
	preArm    func(ctx context.Context) error
	open      func(ctx context.Context, w capability.Window) (capability.Handler, error)
	calls     atomic.Int32 // Open calls
	teardowns atomic.Int32
}

func newHookCap() *hookCap { return &hookCap{Capability: echo.New()} }

func (c *hookCap) PreArm(ctx context.Context, pk []byte) error {
	if c.preArm != nil {
		return c.preArm(ctx)
	}
	return nil
}

func (c *hookCap) Open(ctx context.Context, w capability.Window, p json.RawMessage) (capability.Handler, error) {
	c.calls.Add(1)
	open := c.open
	if open == nil {
		open = func(ctx context.Context, w capability.Window) (capability.Handler, error) {
			return c.Capability.Open(ctx, w, p)
		}
	}
	h, err := open(ctx, w)
	if h != nil {
		h = &hookHandler{Handler: h, c: c}
	}
	return h, err
}

type hookHandler struct {
	capability.Handler
	c *hookCap
}

func (h *hookHandler) Teardown(r string) { h.c.teardowns.Add(1); h.Handler.Teardown(r) }

// armSilentlyClosed runs a handshake that must end, after the ARM, in a
// close with no message.
func (e *env) armSilentlyClosed() {
	e.t.Helper()
	cl := e.dial()
	if err := cl.sendHello(echo.ID, e.dev); err != nil {
		e.t.Fatal(err)
	}
	cl.readChallenge()
	cl.writeRaw(cl.armFrame(e.dev, cl.ch.N[:], echo.ID, cl.ch.ExpiryMS))
	cl.expectClosedNoBytes()
}

func TestPreArmAndOpenBudgets(t *testing.T) {
	hc := newHookCap()
	var preArmLeft, openLeft atomic.Int64
	hc.preArm = func(ctx context.Context) error {
		if d, ok := ctx.Deadline(); ok {
			preArmLeft.Store(int64(time.Until(d)))
		}
		return nil
	}
	hc.open = func(ctx context.Context, w capability.Window) (capability.Handler, error) {
		if d, ok := ctx.Deadline(); ok {
			openLeft.Store(int64(time.Until(d)))
		}
		return echo.New().Open(ctx, w, nil)
	}
	e := newEnv(t, func(e *env) { e.caps = []capability.Capability{hc} })
	cl := e.open(echo.ID)
	cl.echo([]byte("ok"))
	if d := time.Duration(preArmLeft.Load()); d <= 800*time.Millisecond || d > time.Second {
		t.Fatalf("PreArm ctx budget %v, want 1 s", d)
	}
	if d := time.Duration(openLeft.Load()); d <= 1800*time.Millisecond || d > 2*time.Second {
		t.Fatalf("Open ctx budget %v, want 2 s", d)
	}
}

func TestPreArmPanicClosesSilently(t *testing.T) {
	hc := newHookCap()
	var n atomic.Int32
	hc.preArm = func(context.Context) error {
		if n.Add(1) == 1 {
			panic("prearm boom")
		}
		return nil
	}
	e := newEnv(t, func(e *env) { e.caps = []capability.Capability{hc} })
	e.armSilentlyClosed()
	if e.srv.slotOpen(echo.ID) || hc.calls.Load() != 0 {
		t.Fatal("PreArm panic reached the slot or Open")
	}
	waitFor(t, time.Second, "pre-auth released", func() bool { return e.srv.preauthCount() == 0 })
	e.open(echo.ID).echo([]byte("server survived"))
}

func TestOpenPanicFreesSlot(t *testing.T) {
	hc := newHookCap()
	hc.open = func(ctx context.Context, w capability.Window) (capability.Handler, error) {
		if hc.calls.Load() == 1 {
			panic("open boom")
		}
		return echo.New().Open(ctx, w, nil)
	}
	e := newEnv(t, func(e *env) { e.caps = []capability.Capability{hc} })
	e.armSilentlyClosed()
	waitFor(t, time.Second, "slot free after Open panic", func() bool { return !e.srv.slotOpen(echo.ID) })
	e.open(echo.ID).echo([]byte("server survived"))
	if hc.teardowns.Load() != 0 {
		t.Fatal("Teardown without a Handler")
	}
}

// A slow Open must not hold one of the two global pre-auth slots.
func TestSlowOpenLeavesPreAuth(t *testing.T) {
	hc := newHookCap()
	entered, release := make(chan struct{}), make(chan struct{})
	hc.open = func(ctx context.Context, w capability.Window) (capability.Handler, error) {
		close(entered)
		select {
		case <-release:
		case <-ctx.Done():
		}
		return echo.New().Open(ctx, w, nil)
	}
	e := newEnv(t, func(e *env) { e.caps = []capability.Capability{hc} })
	cl := e.dial()
	cl.sendHello(echo.ID, e.dev)
	cl.readChallenge()
	cl.writeRaw(cl.armFrame(e.dev, cl.ch.N[:], echo.ID, cl.ch.ExpiryMS))
	<-entered
	waitFor(t, time.Second, "Open's connection out of pre-auth", func() bool { return e.srv.preauthCount() == 0 })

	// Two more pre-auth connections fit while Open runs.
	s1, s2 := e.dial(), e.dial()
	s1.c.Write([]byte{0, 0})
	s2.c.Write([]byte{0, 0})
	waitFor(t, time.Second, "two pre-auth connections", func() bool { return e.srv.preauthCount() == 2 })
	s2.c.SetReadDeadline(time.Now().Add(100 * time.Millisecond))
	if _, err := s2.c.Read(make([]byte, 1)); !isTimeout(err) {
		t.Fatalf("second concurrent pre-auth connection refused: %v", err)
	}

	close(release)
	cl.expectReady()
	cl.echo([]byte("opened"))
}

// Send from inside Open must not deadlock on a full priority lane.
func TestSendInOpenDoesNotHang(t *testing.T) {
	hc := newHookCap()
	var sent, refused atomic.Int32
	hc.open = func(ctx context.Context, w capability.Window) (capability.Handler, error) {
		for i := 0; i < 100; i++ { // 100 KiB > the 64 KiB priority lane
			body := make([]byte, 1024)
			body[0] = byte(i)
			if err := w.Send(echo.TypeOut, body); err != nil {
				if errors.Is(err, capability.ErrClosed) {
					return nil, err
				}
				refused.Add(1)
				continue
			}
			sent.Add(1)
		}
		return echo.New().Open(ctx, w, nil)
	}
	e := newEnv(t, func(e *env) { e.caps = []capability.Capability{hc} })
	t0 := time.Now()
	cl := e.open(echo.ID) // READY first
	if d := time.Since(t0); d > time.Second {
		t.Fatalf("handshake took %v", d)
	}
	if sent.Load() == 0 || refused.Load() == 0 {
		t.Fatalf("sent %d refused %d: want some of each", sent.Load(), refused.Load())
	}
	for i := 0; i < int(sent.Load()); i++ {
		typ, body := cl.recv()
		if typ != echo.TypeOut || len(body) != 1024 || body[0] != byte(i) {
			t.Fatalf("frame %d: type 0x%02x len %d", i, typ, len(body))
		}
	}
	cl.echo([]byte("after Open's sends"))
}

func TestOpenOverrunAborts(t *testing.T) {
	cases := map[string]func(ctx context.Context){
		"ignores ctx":       func(context.Context) { time.Sleep(2300 * time.Millisecond) },
		"returns at expiry": func(ctx context.Context) { <-ctx.Done() },
	}
	for name, stall := range cases {
		t.Run(name, func(t *testing.T) {
			hc := newHookCap()
			hc.open = func(ctx context.Context, w capability.Window) (capability.Handler, error) {
				if hc.calls.Load() == 1 {
					stall(ctx)
				}
				return echo.New().Open(ctx, w, nil)
			}
			e := newEnv(t, func(e *env) { e.caps = []capability.Capability{hc} })
			t0 := time.Now()
			e.armSilentlyClosed()
			if d := time.Since(t0); d > 2600*time.Millisecond {
				t.Fatalf("closed after %v, want ~2 s", d)
			}
			waitFor(t, time.Second, "slot free after overrun", func() bool { return !e.srv.slotOpen(echo.ID) })
			// The late Handler is torn down once.
			waitFor(t, 2*time.Second, "late Handler torn down", func() bool { return hc.teardowns.Load() == 1 })
			e.open(echo.ID).echo([]byte("slot reusable"))
		})
	}
}

// doneWaiter is a Handler whose Teardown waits for the window's Done, as
// CAPABILITY-API tells capabilities to (stop goroutines on Done()). The 5 s
// cap only keeps a broken server from hanging the test binary.
type doneWaiter struct {
	w        capability.Window
	sawDone  atomic.Bool
	finished chan struct{}
}

func (h *doneWaiter) Frame(byte, []byte) error { return nil }

func (h *doneWaiter) Teardown(string) {
	select {
	case <-h.w.Done():
		h.sawDone.Store(true)
	case <-time.After(5 * time.Second):
	}
	close(h.finished)
}

// A Handler that Open returns after its budget is torn down only after its
// window is closed, so a Teardown that waits on Done() cannot hang the
// connection or hold the slot. Both late paths: inline (Open returns as its
// ctx expires) and the background one (Open returns after the hard timer,
// racing the abort; the hook holds the abort back to force that order).
func TestLateTeardownSeesClosedWindow(t *testing.T) {
	for _, path := range []string{"inline", "timer"} {
		t.Run(path, func(t *testing.T) {
			hc := newHookCap()
			late := make(chan *doneWaiter, 1)
			hc.open = func(ctx context.Context, w capability.Window) (capability.Handler, error) {
				if hc.calls.Load() != 1 {
					return echo.New().Open(ctx, w, nil)
				}
				if path == "inline" {
					<-ctx.Done()
				} else {
					time.Sleep(openBudget + overrunGrace + 150*time.Millisecond)
				}
				h := &doneWaiter{w: w, finished: make(chan struct{})}
				late <- h
				return h, nil
			}
			hookTimedOut := atomic.Bool{}
			e := newEnv(t, func(e *env) {
				e.caps = []capability.Capability{hc}
				if path == "timer" {
					e.overrun = func() { // let the late Handler's Teardown run before abort
						select {
						case h := <-late:
							late <- h
							select {
							case <-h.finished:
							case <-time.After(1500 * time.Millisecond):
								hookTimedOut.Store(true)
							}
						case <-time.After(2 * time.Second):
							hookTimedOut.Store(true)
						}
					}
				}
			})
			e.armSilentlyClosed() // within 3 s of the ARM
			waitFor(t, time.Second, "slot free", func() bool { return !e.srv.slotOpen(echo.ID) })
			h := <-late
			select {
			case <-h.finished:
			case <-time.After(time.Second):
				t.Fatal("late Teardown did not finish")
			}
			if !h.sawDone.Load() || hookTimedOut.Load() {
				t.Fatalf("late Teardown ran on an open window (sawDone=%v, blocked=%v)", h.sawDone.Load(), hookTimedOut.Load())
			}
			if n := hc.teardowns.Load(); n != 1 {
				t.Fatalf("Teardown ran %d times", n)
			}
			e.open(echo.ID).echo([]byte("slot reusable"))
		})
	}
}
