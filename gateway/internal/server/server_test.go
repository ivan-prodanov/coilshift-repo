package server

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"runtime"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"airgapp/gateway/capability"
	"airgapp/gateway/capability/echo"
	"airgapp/gateway/internal/authority"
	"airgapp/gateway/internal/keys"
	"airgapp/gateway/internal/proto"
	"airgapp/gateway/internal/wire"
)

func TestHappyPathEcho(t *testing.T) {
	e := newEnv(t)
	cl := e.dial()
	if err := cl.sendHello(echo.ID, e.dev); err != nil {
		t.Fatal(err)
	}
	cl.readChallenge()
	cl.writeRaw(cl.armFrame(e.dev, cl.ch.N[:], echo.ID, cl.ch.ExpiryMS))
	if lease := cl.expectReady(); lease != 20000 {
		t.Fatalf("lease_ms %d", lease)
	}
	cl.echo([]byte("hello"))
	cl.echo(bytes.Repeat([]byte{0xAB}, 1024)) // exactly at the In limit
	cl.send(proto.TypeHeartbeat, nil)
	cl.echo(nil)
	cl.send(proto.TypeClose, nil)
	cl.expectBye("close")

	lines := e.logLines(1)
	l := lines[0]
	if l["cap"] != "ECHO" || l["reason"] != "close" || l["fp"] != keys.Fingerprint(e.dev.PublicKey().Bytes()) {
		t.Fatalf("log line %v", l)
	}
	for _, k := range []string{"start", "end"} {
		if s, _ := l[k].(string); s == "" {
			t.Fatalf("log %s missing: %v", k, l)
		}
	}
	raw, _ := os.ReadFile(e.logPath)
	if bytes.Contains(raw, []byte(hex.EncodeToString(e.dev.PublicKey().Bytes()))) || bytes.Contains(raw, []byte(testVIN)) {
		t.Fatal("session log leaks the key or VIN")
	}
	waitFor(t, time.Second, "slot free", func() bool { return !e.srv.slotOpen(echo.ID) })
}

func TestUnregisteredCapability(t *testing.T) {
	e := newEnv(t)
	cl := e.dial()
	cl.sendHello(proto.CapSummon, e.dev) // valid id, not registered
	cl.expectClosedNoBytes()
	cl2 := e.dial()
	cl2.writeRaw([]byte("AGW1\x01\x7fshort")) // malformed hello
	cl2.expectClosedNoBytes()
}

// A role other than OWNER/DRIVER is refused at the hello, with no challenge
// (the after-ARM refusal: TestRoleDropAfterHelloRefusedAtArm).
func TestRoleRejected(t *testing.T) {
	e := newEnv(t, func(e *env) { e.oracle.Role = 8 })
	cl := e.dial()
	cl.sendHello(echo.ID, e.dev)
	cl.expectClosedNoBytes()
	if e.oracle.calls.Load() == 0 {
		t.Fatal("oracle was not consulted")
	}
	if e.srv.slotOpen(echo.ID) {
		t.Fatal("slot held after a rejected role")
	}
}

func TestBadSignatureNeverSent(t *testing.T) {
	e := newEnv(t)
	pinned := newECDSA(t) // the phone pinned a different gateway key
	cl := e.dial()
	cl.sendHello(echo.ID, e.dev)
	cl.c.SetReadDeadline(time.Now().Add(5 * time.Second))
	b, err := wire.ReadFrame(cl.c, 1024)
	if err != nil {
		t.Fatal(err)
	}
	ch, err := proto.ParseChallenge(b)
	if err != nil {
		t.Fatal(err)
	}
	if proto.VerifyChallenge(&pinned.PublicKey, cl.hello, ch) {
		t.Fatal("challenge from another gateway key verified with the pinned key")
	}
	if !proto.VerifyChallenge(&e.gw.PublicKey, cl.hello, ch) {
		t.Fatal("control: the real gateway key must verify")
	}
	// The phone refuses: it never sends ARM and hangs up. The oracle is
	// consulted only by the hello's filter, never again.
	atHello := e.oracle.calls.Load()
	cl.c.Close()
	waitFor(t, time.Second, "pre-auth released", func() bool { return e.srv.preauthCount() == 0 })
	if n := e.oracle.calls.Load(); n != atHello {
		t.Fatalf("oracle called %d times after the hello's filter", n-atHello)
	}
}

func TestArmMismatch(t *testing.T) {
	cases := map[string]func(cl *client, dev []byte) []byte{
		"wrong n": func(cl *client, _ []byte) []byte {
			n := cl.ch.N
			n[0] ^= 1
			return cl.armFrame(cl.e.dev, n[:], echo.ID, cl.ch.ExpiryMS)
		},
		"wrong cap": func(cl *client, _ []byte) []byte {
			return cl.armFrame(cl.e.dev, cl.ch.N[:], proto.CapCamera, cl.ch.ExpiryMS)
		},
		"wrong exp": func(cl *client, _ []byte) []byte {
			return cl.armFrame(cl.e.dev, cl.ch.N[:], echo.ID, cl.ch.ExpiryMS+1)
		},
		"not arm": func(cl *client, _ []byte) []byte {
			return cl.firstFrame(cl.e.dev, proto.TypeHeartbeat, armJSON(cl.ch.N[:], echo.ID, cl.ch.ExpiryMS))
		},
		"bad json": func(cl *client, _ []byte) []byte {
			return cl.firstFrame(cl.e.dev, proto.TypeArm, []byte(`{"n":`))
		},
		"short n": func(cl *client, _ []byte) []byte {
			return cl.armFrame(cl.e.dev, cl.ch.N[:8], echo.ID, cl.ch.ExpiryMS)
		},
		"other sender key": func(cl *client, _ []byte) []byte {
			return cl.armFrame(newECDH(cl.t), cl.ch.N[:], echo.ID, cl.ch.ExpiryMS)
		},
		"garbage": func(cl *client, _ []byte) []byte {
			return bytes.Repeat([]byte{7}, 200)
		},
	}
	for name, build := range cases {
		t.Run(name, func(t *testing.T) {
			e := newEnv(t)
			cl := e.dial()
			cl.sendHello(echo.ID, e.dev)
			cl.readChallenge()
			atHello := e.oracle.calls.Load() // the hello's filter
			cl.writeRaw(build(cl, nil))
			cl.expectClosedNoBytes()
			if n := e.oracle.calls.Load(); n != atHello {
				t.Fatalf("oracle called %d times for a bad ARM", n-atHello)
			}
		})
	}
}

func TestArmReplayNewConnection(t *testing.T) {
	e := newEnv(t)
	cl1 := e.dial()
	cl1.sendHello(echo.ID, e.dev)
	cl1.readChallenge()
	arm1 := cl1.armFrame(e.dev, cl1.ch.N[:], echo.ID, cl1.ch.ExpiryMS)
	cl1.writeRaw(arm1)
	cl1.expectReady()
	cl1.send(proto.TypeClose, nil)
	cl1.expectBye("close")
	waitFor(t, time.Second, "slot free", func() bool { return !e.srv.slotOpen(echo.ID) })

	cl2 := e.dial()
	cl2.sendHello(echo.ID, e.dev)
	cl2.readChallenge()
	cl2.writeRaw(arm1) // replay
	cl2.expectClosedNoBytes()
	if e.srv.slotOpen(echo.ID) {
		t.Fatal("replayed ARM opened a window")
	}
}

func TestChallengeExpiredAfterSuspend(t *testing.T) {
	t.Run("advanced 6000 ms", func(t *testing.T) {
		e := newEnv(t)
		cl := e.dial()
		cl.sendHello(echo.ID, e.dev)
		cl.readChallenge()
		atHello := e.oracle.calls.Load() // the hello's filter
		e.clk.Advance(6000)              // the car dozed between challenge and ARM
		cl.writeRaw(cl.armFrame(e.dev, cl.ch.N[:], echo.ID, cl.ch.ExpiryMS))
		cl.expectClosedNoBytes()
		if n := e.oracle.calls.Load(); n != atHello {
			t.Fatalf("oracle called %d times for an expired challenge", n-atHello)
		}
	})
	t.Run("boundary 5000 ms accepted", func(t *testing.T) {
		e := newEnv(t)
		cl := e.dial()
		cl.sendHello(echo.ID, e.dev)
		cl.readChallenge()
		if cl.ch.ExpiryMS != e.clk.BootMS()+5000 {
			t.Fatalf("expiry %d, now %d", cl.ch.ExpiryMS, e.clk.BootMS())
		}
		e.clk.Advance(5000)
		cl.writeRaw(cl.armFrame(e.dev, cl.ch.N[:], echo.ID, cl.ch.ExpiryMS))
		cl.expectReady()
	})
}

func TestPreAuthSlowloris(t *testing.T) {
	e := newEnv(t)
	start := time.Now()
	s1, s2 := e.dial(), e.dial()
	s1.c.Write([]byte{0, 0, 0}) // 3 bytes of a length prefix, then nothing
	s2.c.Write([]byte{0, 0, 0})
	waitFor(t, time.Second, "two pre-auth connections", func() bool { return e.srv.preauthCount() == 2 })

	t3 := time.Now()
	c3 := e.dial()
	c3.expectClosedNoBytes()
	if d := time.Since(t3); d > 500*time.Millisecond {
		t.Fatalf("third connection closed after %v, want at once", d)
	}

	for _, s := range []*client{s1, s2} {
		s.expectClosedNoBytes()
		if d := time.Since(start); d < 1800*time.Millisecond || d > 2700*time.Millisecond {
			t.Fatalf("stalled connection closed after %v, want ~2 s", d)
		}
	}
	waitFor(t, time.Second, "pre-auth released", func() bool { return e.srv.preauthCount() == 0 })
	// Capacity is back: a normal handshake succeeds.
	e.open(echo.ID)
}

func TestHelloRate(t *testing.T) {
	e := newEnv(t)
	start := time.Now()
	for i := 0; i < 6; i++ {
		waitFor(t, time.Second, "pre-auth released", func() bool { return e.srv.preauthCount() == 0 })
		cl := e.dial()
		cl.sendHello(echo.ID, e.dev)
		if i < 5 {
			cl.readChallenge()
			cl.c.Close()
			continue
		}
		cl.expectClosedNoBytes()
	}
	if d := time.Since(start); d >= time.Second {
		t.Fatalf("test took %v: the 6 hellos were not within 1 s", d)
	}
	// After the window passes, hellos are admitted again.
	time.Sleep(1100 * time.Millisecond)
	e.open(echo.ID)
}

// A second window on a held slot is refused after the authority check (an
// unauthorised key must not learn the slot's state: access_test.go), with no
// message; the first window is untouched.
func TestSecondWindowRefused(t *testing.T) {
	e := newEnv(t)
	cl1 := e.open(echo.ID)
	calls := e.oracle.calls.Load()

	cl2 := e.dial()
	cl2.sendHello(echo.ID, e.dev)
	cl2.readChallenge()
	cl2.writeRaw(cl2.armFrame(e.dev, cl2.ch.N[:], echo.ID, cl2.ch.ExpiryMS))
	cl2.expectClosedNoBytes()
	if e.oracle.calls.Load() == calls {
		t.Fatal("the slot's state was told before the authority check")
	}
	cl1.echo([]byte("still here"))
	cl1.send(proto.TypeClose, nil)
	cl1.expectBye("close")
}

func TestReconnectAfterDrop(t *testing.T) {
	base := runtime.NumGoroutine()
	e := newEnv(t)
	cl := e.open(echo.ID)
	cl.echo([]byte("before drop"))

	dropped := time.Now()
	cl.c.(interface{ SetLinger(int) error }).SetLinger(0)
	cl.c.Close() // kill: RST, no CLOSE
	waitFor(t, 100*time.Millisecond, "slot freed after drop", func() bool { return !e.srv.slotOpen(echo.ID) })
	t.Logf("slot freed %v after drop", time.Since(dropped))

	cl2 := e.open(echo.ID)
	cl2.echo([]byte("after reconnect"))
	cl2.send(proto.TypeClose, nil)
	cl2.expectBye("close")

	lines := e.logLines(2)
	if lines[0]["reason"] != "drop" || lines[1]["reason"] != "close" {
		t.Fatalf("log reasons %v / %v", lines[0]["reason"], lines[1]["reason"])
	}
	e.close()
	settleGoroutines(t, base)
}

func TestGrammarTeardown(t *testing.T) {
	cases := map[string]func(cl *client){
		"unknown type":    func(cl *client) { cl.send(0x55, nil) },
		"outbound type":   func(cl *client) { cl.send(echo.TypeOut, []byte("x")) },
		"second arm":      func(cl *client) { cl.send(proto.TypeArm, armJSON(cl.ch.N[:], echo.ID, cl.ch.ExpiryMS)) },
		"heartbeat body":  func(cl *client) { cl.send(proto.TypeHeartbeat, []byte{1}) },
		"empty plaintext": func(cl *client) { cl.writeRaw(cl.snd.Seal(nil)) },
		"core ready type": func(cl *client) { cl.send(proto.TypeReady, nil) },
	}
	for name, f := range cases {
		t.Run(name, func(t *testing.T) {
			cc := newCountCap(echo.New(), 0, "")
			e := newEnv(t, func(e *env) { e.caps = []capability.Capability{cc} })
			cl := e.open(echo.ID)
			f(cl)
			cl.expectBye("grammar")
			waitFor(t, time.Second, "teardown", func() bool { return cc.teardowns.Load() == 1 })
		})
	}
}

func TestDecryptFailureTeardown(t *testing.T) {
	e := newEnv(t)
	cl := e.open(echo.ID)
	cl.writeRaw(bytes.Repeat([]byte{1}, 40))
	cl.expectBye("decrypt")
}

func TestOversizeAfterArm(t *testing.T) {
	cc := newCountCap(echo.New(), 0, "")
	e := newEnv(t, func(e *env) { e.caps = []capability.Capability{cc} })
	cl := e.open(echo.ID)
	cl.send(echo.TypeIn, make([]byte, 1025))
	cl.expectBye("oversize")
	waitFor(t, time.Second, "teardown", func() bool { return cc.teardowns.Load() == 1 })
	if r := <-cc.reasons; r != "oversize" {
		t.Fatalf("teardown reason %q", r)
	}
}

func TestLeaseExpiry(t *testing.T) {
	cc := newCountCap(echo.New(), 0, "")
	e := newEnv(t, func(e *env) { e.caps = []capability.Capability{cc} })
	cl := e.open(echo.ID)

	// A HEARTBEAT renews the lease.
	e.clk.Advance(15000)
	cl.send(proto.TypeHeartbeat, nil)
	time.Sleep(200 * time.Millisecond)
	e.clk.Advance(15000)
	time.Sleep(300 * time.Millisecond)
	cl.echo([]byte("alive")) // would be BYE "lease" if the heartbeat had not renewed

	e.clk.Advance(20000)
	t0 := time.Now()
	cl.expectBye("lease")
	if d := time.Since(t0); d > 500*time.Millisecond {
		t.Fatalf("lease BYE after %v", d)
	}
	time.Sleep(200 * time.Millisecond)
	if n := cc.teardowns.Load(); n != 1 {
		t.Fatalf("Teardown called %d times", n)
	}
	if r := <-cc.reasons; r != "lease" {
		t.Fatalf("teardown reason %q", r)
	}
}

func TestShutdown(t *testing.T) {
	base := runtime.NumGoroutine()
	c1 := newCountCap(echo.New(), 0, "")
	c2 := newCountCap(echo.New(), proto.CapCamera, "ECHO2")
	e := newEnv(t, func(e *env) { e.caps = []capability.Capability{c1, c2} })
	a := e.open(echo.ID)
	b := e.open(proto.CapCamera)
	b.send(echo.TypeIn, []byte("b"))
	if typ, _ := b.recv(); typ != echo.TypeOut {
		t.Fatal("echo on second window")
	}
	pre := e.dial()
	pre.c.Write([]byte{0, 0})
	waitFor(t, time.Second, "pre-auth conn", func() bool { return e.srv.preauthCount() == 1 })

	done := make(chan struct{})
	go func() { e.srv.Shutdown("sigterm"); close(done) }()
	a.expectBye("sigterm")
	b.expectBye("sigterm")
	pre.expectClosedNoBytes()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Shutdown did not return")
	}
	select {
	case <-e.serveDone:
	case <-time.After(time.Second):
		t.Fatal("Serve did not return")
	}
	for _, c := range []*countCap{c1, c2} {
		if n := c.teardowns.Load(); n != 1 {
			t.Fatalf("%s Teardown %d times", c.Spec().Name, n)
		}
		if r := <-c.reasons; r != "sigterm" {
			t.Fatalf("teardown reason %q", r)
		}
	}
	lines := e.logLines(2)
	for _, l := range lines {
		if l["reason"] != "sigterm" {
			t.Fatalf("log %v", l)
		}
	}
	e.srv.Shutdown("again") // idempotent
	e.close()
	settleGoroutines(t, base)
}

func TestWindowSendRules(t *testing.T) {
	cc := newCountCap(echo.New(), 0, "")
	e := newEnv(t, func(e *env) { e.caps = []capability.Capability{cc} })
	cl := e.open(echo.ID)
	w := <-cc.win

	if w.KeyFingerprint() != keys.Fingerprint(e.dev.PublicKey().Bytes()) {
		t.Fatal("fingerprint")
	}
	if err := w.Send(echo.TypeOut, make([]byte, 1025)); err == nil {
		t.Fatal("oversize outbound accepted")
	}
	if err := w.Send(0x99, nil); err == nil {
		t.Fatal("outbound type outside Spec.Out accepted")
	}
	if err := w.Send(proto.TypeBye, []byte("x")); err == nil {
		t.Fatal("capability sent a core type")
	}
	if w.TrySend(echo.TypeOut, make([]byte, 1025)) || w.TrySend(0x99, nil) {
		t.Fatal("TrySend accepted a bad frame")
	}
	if !w.TrySend(echo.TypeOut, []byte("pushed")) {
		t.Fatal("TrySend refused a valid frame")
	}
	if typ, body := cl.recv(); typ != echo.TypeOut || string(body) != "pushed" {
		t.Fatalf("got 0x%02x %q", typ, body)
	}
	cl.echo([]byte("window survived bad sends"))
	if w.QueuedBytes() != 0 {
		t.Fatal("QueuedBytes on an idle window")
	}
	w.SetDetail("front,rear")
	w.Close("close")
	w.Close("again") // idempotent
	select {
	case <-w.Done():
	case <-time.After(time.Second):
		t.Fatal("Done not closed")
	}
	if err := w.Send(echo.TypeOut, []byte("late")); !errors.Is(err, capability.ErrClosed) {
		t.Fatalf("Send after Close: %v", err)
	}
	if w.TrySend(echo.TypeOut, []byte("late")) {
		t.Fatal("TrySend after Close")
	}
	cl.expectBye("close")
	l := e.logLines(1)[0]
	if l["detail"] != "front,rear" || l["reason"] != "close" {
		t.Fatalf("log %v", l)
	}
	if cc.teardowns.Load() != 1 {
		t.Fatal("teardown count")
	}
}

// bulkCap queues 48 x 64 KiB of bulk then one priority frame on 0x40.
type bulkCap struct{ remaining chan int }

const (
	tTrigger byte = 0x40
	tBulk    byte = 0xC0
	tPrio    byte = 0xC1
)

func (c *bulkCap) Spec() capability.Spec {
	return capability.Spec{
		Name: "BULK", ID: proto.CapDashcam,
		In:   map[byte]int{tTrigger: 16},
		Out:  map[byte]int{tBulk: 1 << 20, tPrio: 64},
		Bulk: map[byte]bool{tBulk: true},
	}
}
func (c *bulkCap) PreArm(context.Context, []byte) error { return nil }
func (c *bulkCap) Open(_ context.Context, w capability.Window, _ json.RawMessage) (capability.Handler, error) {
	return &bulkHandler{w: w, c: c}, nil
}

type bulkHandler struct {
	w capability.Window
	c *bulkCap
}

func (h *bulkHandler) Frame(t byte, _ []byte) error {
	for i := 0; i < 48; i++ {
		if err := h.w.Send(tBulk, bytes.Repeat([]byte{byte(i)}, 64<<10)); err != nil {
			return err
		}
	}
	rem := h.w.QueuedBytes()
	err := h.w.Send(tPrio, []byte("prio"))
	h.c.remaining <- rem
	return err
}
func (h *bulkHandler) Teardown(string) {}

func TestPriorityLaneOvertakesBulk(t *testing.T) {
	bc := &bulkCap{remaining: make(chan int, 1)}
	e := newEnv(t, func(e *env) { e.caps = []capability.Capability{bc} })
	cl := e.open(proto.CapDashcam)
	cl.send(tTrigger, nil)
	time.Sleep(100 * time.Millisecond) // let the writer fill the socket buffers
	rem := <-bc.remaining
	if rem < 1<<20 {
		t.Fatalf("only %d bulk bytes queued when the priority frame was sent", rem)
	}
	seenPrio, bulkAfter, bulkTotal := false, 0, 0
	for bulkTotal < 48 || !seenPrio {
		typ, body := cl.recv()
		switch typ {
		case tBulk:
			if len(body) != 64<<10 || body[0] != byte(bulkTotal) {
				t.Fatalf("bulk frame %d out of order", bulkTotal)
			}
			bulkTotal++
			if seenPrio {
				bulkAfter += len(body)
			}
		case tPrio:
			seenPrio = true
		default:
			t.Fatalf("unexpected type 0x%02x", typ)
		}
	}
	if bulkAfter < rem-(64<<10) {
		t.Fatalf("priority frame arrived with only %d bulk bytes after it; %d were queued", bulkAfter, rem)
	}
	t.Logf("queued at prio send: %d; bulk bytes after prio: %d", rem, bulkAfter)
}

func TestNonceSingleUse(t *testing.T) {
	var c nonceCache
	var n1, n2 [16]byte
	n2[0] = 1
	if !c.use(n1, 1000) || c.use(n1, 1001) {
		t.Fatal("N reused")
	}
	if !c.use(n2, 5000) {
		t.Fatal("fresh N refused")
	}
	if c.use(n1, 1000+nonceTTLms-1) {
		t.Fatal("N forgotten before 10 s")
	}
	// After the TTL n1 is pruned (memory is bounded).
	if !c.use(n1, 1000+nonceTTLms+1) {
		t.Fatal("N not pruned after the TTL")
	}
	for i := 0; i < nonceMax+10; i++ {
		var n [16]byte
		n[0], n[1], n[2] = 9, byte(i), byte(i>>8)
		c.use(n, 20000)
	}
	if len(c.m) > nonceMax {
		t.Fatalf("cache grew to %d", len(c.m))
	}
}

func TestSessionLogRotate(t *testing.T) {
	dir := t.TempDir()
	p := dir + "/s.log"
	l := &sessionLog{path: p, max: 300}
	for i := 0; i < 10; i++ {
		l.write(logEntry{Start: "s", End: "e", FP: "0011223344556677", Cap: "ECHO", Detail: strings.Repeat("d", 20), Reason: "close"})
	}
	st, err := os.Stat(p)
	if err != nil || st.Size() > 300 {
		t.Fatalf("log size %v %v", st, err)
	}
	if _, err := os.Stat(p + ".1"); err != nil {
		t.Fatal("no rotated .1")
	}
	// A write failure must not panic.
	bad := &sessionLog{path: dir + "/missing/dir/s.log", max: 300}
	bad.write(logEntry{Reason: "close"})
}

// shutdownInOpen triggers Shutdown from inside Open and returns only after
// Shutdown has closed the (still pre-auth) connection.
type shutdownInOpen struct {
	*countCap
	srv *Server
}

func (c *shutdownInOpen) Open(ctx context.Context, w capability.Window, p json.RawMessage) (capability.Handler, error) {
	go c.srv.Shutdown("sigterm")
	for !c.srv.isShutdown() {
		time.Sleep(time.Millisecond)
	}
	return c.countCap.Open(ctx, w, p)
}

func TestShutdownDuringOpen(t *testing.T) {
	sc := &shutdownInOpen{countCap: newCountCap(echo.New(), 0, "")}
	e := newEnv(t, func(e *env) { e.caps = []capability.Capability{sc} })
	sc.srv = e.srv
	cl := e.dial()
	cl.sendHello(echo.ID, e.dev)
	cl.readChallenge()
	cl.writeRaw(cl.armFrame(e.dev, cl.ch.N[:], echo.ID, cl.ch.ExpiryMS))
	waitFor(t, 2*time.Second, "Teardown", func() bool { return sc.teardowns.Load() == 1 })
	if r := <-sc.reasons; r != "sigterm" {
		t.Fatalf("teardown reason %q", r)
	}
	l := e.logLines(1)[0]
	if l["reason"] != "sigterm" || l["cap"] != "ECHO" {
		t.Fatalf("log %v", l)
	}
	time.Sleep(100 * time.Millisecond)
	if n := sc.teardowns.Load(); n != 1 {
		t.Fatalf("Teardown %d times", n)
	}
}

func TestCustomCloseReasonNotOnWire(t *testing.T) {
	cc := newCountCap(echo.New(), 0, "")
	e := newEnv(t, func(e *env) { e.caps = []capability.Capability{cc} })
	cl := e.open(echo.ID)
	w := <-cc.win
	w.Close("camera helper: /dev/video3 busy")
	cl.expectBye("error")
	l := e.logLines(1)[0]
	if l["reason"] != "camera helper: /dev/video3 busy" {
		t.Fatalf("log keeps the original reason locally: %v", l)
	}
	if r := <-cc.reasons; r != "camera helper: /dev/video3 busy" {
		t.Fatalf("Teardown reason %q", r)
	}
}

func TestFixedCloseReasonOnWire(t *testing.T) {
	for _, r := range []string{"close", "lease", "grammar", "oversize", "decrypt", "revoked", "sigterm", "error", "drop"} {
		if wireReason(r) != r {
			t.Fatalf("%q not sent verbatim", r)
		}
	}
	if wireReason("x") != "error" || wireReason("") != "error" {
		t.Fatal("unknown reasons must go on the wire as error")
	}
	e := newEnv(t)
	cl := e.open(echo.ID)
	e.srv.Shutdown("test") // not a fixed reason
	cl.expectBye("error")
}

func TestLogEventInSessionLog(t *testing.T) {
	cc := newCountCap(echo.New(), 0, "")
	e := newEnv(t, func(e *env) { e.caps = []capability.Capability{cc} })
	cl := e.open(echo.ID)
	w := <-cc.win
	w.LogEvent("hold start view=front")
	w.LogEvent(strings.Repeat("x", 200)) // truncated to 128 bytes
	for i := 0; i < 70; i++ {            // capped at 64 events per window
		w.LogEvent("e")
	}
	cl.send(proto.TypeClose, nil)
	cl.expectBye("close")
	l := e.logLines(1)[0]
	evs, ok := l["events"].([]any)
	if !ok || len(evs) != 64 {
		t.Fatalf("events %v", l["events"])
	}
	first := evs[0].(map[string]any)
	if first["e"] != "hold start view=front" {
		t.Fatalf("first event %v", first)
	}
	if ms, ok := first["t"].(float64); !ok || ms < 0 || ms > 5000 {
		t.Fatalf("event t %v", first["t"])
	}
	if s := evs[1].(map[string]any)["e"].(string); len(s) != 128 {
		t.Fatalf("long event kept %d bytes", len(s))
	}
	if len(first) != 2 {
		t.Fatalf("event has extra fields: %v", first)
	}
}

func TestNoEventsFieldWhenUnused(t *testing.T) {
	e := newEnv(t)
	cl := e.open(echo.ID)
	cl.send(proto.TypeClose, nil)
	cl.expectBye("close")
	if _, ok := e.logLines(1)[0]["events"]; ok {
		t.Fatal("empty events array logged")
	}
}

func TestTruncUTF8(t *testing.T) {
	s := truncUTF8(strings.Repeat("é", 100), maxEventLen) // 2-byte runes
	if len(s) != 128 || !utf8.ValidString(s) {
		t.Fatalf("len %d valid %v", len(s), utf8.ValidString(s))
	}
	s = truncUTF8("a"+strings.Repeat("€", 50), maxEventLen) // cut mid-rune
	if len(s) > 128 || !utf8.ValidString(s) {
		t.Fatalf("len %d valid %v", len(s), utf8.ValidString(s))
	}
}

// specCap is a capability with a fixed Spec, for New's validation.
type specCap struct {
	capability.Capability
	spec capability.Spec
}

func (c specCap) Spec() capability.Spec { return c.spec }

func TestNewValidatesSpecs(t *testing.T) {
	ok := func() capability.Spec {
		return capability.Spec{
			Name: "OK", ID: 0x02,
			In:   map[byte]int{0x20: 16384},
			Out:  map[byte]int{0xA0: 1 << 20, 0xA1: 64},
			Bulk: map[byte]bool{0xA0: true},
		}
	}
	cfg := func(specs ...capability.Spec) Config {
		var caps []capability.Capability
		for _, sp := range specs {
			caps = append(caps, specCap{echo.New(), sp})
		}
		return Config{Key: newECDSA(t), Oracle: authority.Fake{}, Caps: caps}
	}
	New(cfg(ok())) // valid: limits at their maximum
	bad := map[string]func(*capability.Spec){
		"In limit > 16384":  func(s *capability.Spec) { s.In[0x21] = 16385 },
		"Out limit > 1 MiB": func(s *capability.Spec) { s.Out[0xA2] = 1<<20 + 1 },
		"In core ARM":       func(s *capability.Spec) { s.In[0x01] = 16 },
		"In core HEARTBEAT": func(s *capability.Spec) { s.In[0x02] = 16 },
		"In core CLOSE":     func(s *capability.Spec) { s.In[0x03] = 16 },
		"Out READY":         func(s *capability.Spec) { s.Out[0x80] = 16 },
		"Out BYE":           func(s *capability.Spec) { s.Out[0x81] = 16 },
		"Out TICK":          func(s *capability.Spec) { s.Out[0x82] = 16 },
		"Bulk not in Out":   func(s *capability.Spec) { s.Bulk[0xA5] = true },
	}
	for name, mutate := range bad {
		t.Run(name, func(t *testing.T) {
			sp := ok()
			mutate(&sp)
			defer func() {
				if recover() == nil {
					t.Fatal("New accepted the spec")
				}
			}()
			New(cfg(sp))
		})
	}
	t.Run("duplicate ID", func(t *testing.T) {
		defer func() {
			if recover() == nil {
				t.Fatal("New accepted duplicate IDs")
			}
		}()
		other := ok()
		other.Name = "OTHER"
		New(cfg(ok(), other))
	})
}
