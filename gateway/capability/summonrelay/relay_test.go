package summonrelay

import (
	"bytes"
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"airgapp/gateway/capability"
	"airgapp/gateway/capability/testkit"
	"airgapp/gateway/internal/guards"
	"airgapp/gateway/internal/summon/relay"
)

// The relay tests play the summon helper on a real unix socket (a fake: each test decides what it sends) and the
// server core through testkit.FakeWindow. HelperUID is this process's uid, so the real peer-credential and Lstat
// checks pass. Socket paths live under os.MkdirTemp("", "sr"): darwin caps them at 104 bytes.

const wait = 2 * time.Second // the bound on anything a test waits for that should happen

func sockPath(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "sr")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	return filepath.Join(dir, "relay.sock")
}

// testConfig is a relay to a fresh socket path served by this uid, with the route gate open. Its log lines are
// discarded; withLog keeps them.
func testConfig(t *testing.T) Config {
	return Config{Socket: sockPath(t), HelperUID: os.Getuid(), RouteCheck: func(context.Context) error { return nil },
		Logf: func(string, ...any) {}}
}

// logCapture keeps the relay's log lines. Logf is safe for the relay's goroutines.
type logCapture struct {
	mu    sync.Mutex
	lines []string
}

func (l *logCapture) Logf(format string, args ...any) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.lines = append(l.lines, fmt.Sprintf(format, args...))
}

func (l *logCapture) get() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]string(nil), l.lines...)
}

// withLog is cfg with its log lines kept.
func withLog(cfg Config) (Config, *logCapture) {
	l := &logCapture{}
	cfg.Logf = l.Logf
	return cfg, l
}

// The secrets a log line must never hold: what tailscaled's prefs could quote (a vehicle-LAN route, the login), and
// what the inner stream carries (the VIN, coordinates, a key). The relay never parses the stream, so a line can hold
// none of it.
var secrets = []string{"192.168.90.0/24", "owner@example.com", "5YJ3E1EA7KF000000", "52.5200001", "13.4050001",
	"04a1b2c3d4e5f60718293a4b5c6d7e8f"}

// secretStream is inner-stream bytes that carry every secret.
func secretStream() []byte {
	return []byte(`{"msg_type":"autopark:device_location","latitude":52.5200001,"longitude":13.4050001,` +
		`"vin":"5YJ3E1EA7KF000000","key":"04a1b2c3d4e5f60718293a4b5c6d7e8f","route":"192.168.90.0/24",` +
		`"login":"owner@example.com"}`)
}

// noSecrets fails the test if a log line holds a secret.
func noSecrets(t *testing.T, lines []string) {
	t.Helper()
	for _, l := range lines {
		for _, s := range secrets {
			if strings.Contains(l, s) {
				t.Fatalf("a log line holds %q: %q", s, l)
			}
		}
	}
}

// oneLine requires exactly one log line, which contains want, and returns it.
func oneLine(t *testing.T, lines []string, want string) string {
	t.Helper()
	if len(lines) != 1 || !strings.Contains(lines[0], want) {
		t.Fatalf("log lines %q, want exactly one containing %q", lines, want)
	}
	return lines[0]
}

// fakeHelper listens on a relay socket and hands each accepted connection to the test, after greet.
type fakeHelper struct {
	conns chan *net.UnixConn
}

// startHelper listens at path. greet runs on each accepted connection before the test gets it; nil sends nothing.
func startHelper(t *testing.T, path string, greet func(*net.UnixConn)) *fakeHelper {
	t.Helper()
	ln, err := net.ListenUnix("unix", &net.UnixAddr{Name: path, Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	h := &fakeHelper{conns: make(chan *net.UnixConn, 16)}
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			c, err := ln.AcceptUnix()
			if err != nil {
				return
			}
			if greet != nil {
				greet(c)
			}
			h.conns <- c
		}
	}()
	t.Cleanup(func() {
		ln.Close()
		<-done
		for {
			select {
			case c := <-h.conns:
				c.Close()
			default:
				return
			}
		}
	})
	return h
}

// owned greets as a ready helper does: OWNED, the connection's first record.
func owned(c *net.UnixConn) { relay.NewWriter(c).Owned() }

// next is the next connection the helper accepted; the test's end closes it.
func (h *fakeHelper) next(t *testing.T) *net.UnixConn {
	t.Helper()
	select {
	case c := <-h.conns:
		t.Cleanup(func() { c.Close() })
		return c
	case <-time.After(wait):
		t.Fatal("no connection reached the helper")
		return nil
	}
}

// helperEnd is the helper's end of an open relay: it writes the helper's records and reads agw's.
type helperEnd struct {
	c *net.UnixConn
	w *relay.Writer
	r *relay.Reader
}

// openWith opens the relay on win, against a fake helper that sends OWNED. The test's end tears the handler down
// and checks that none of its goroutines is left.
func openWith(t *testing.T, cfg Config, win capability.Window) (*handler, *helperEnd) {
	t.Helper()
	fh := startHelper(t, cfg.Socket, owned)
	ctx, cancel := context.WithTimeout(context.Background(), wait)
	defer cancel()
	hd, err := NewWithConfig(cfg).Open(ctx, win, nil)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	h := hd.(*handler)
	t.Cleanup(func() { teardown(t, h) })
	c := fh.next(t)
	return h, &helperEnd{c: c, w: relay.NewWriter(c), r: relay.NewReader(c, false)}
}

// teardown calls Teardown, as the core does once, and requires it to return and leave no relay goroutine.
func teardown(t *testing.T, h *handler) {
	t.Helper()
	done := make(chan struct{})
	go func() {
		h.Teardown("test")
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(wait):
		t.Error("Teardown did not return")
		return
	}
	noRelayGoroutines(t)
}

// noRelayGoroutines waits until no goroutine runs the relay handler's code.
func noRelayGoroutines(t *testing.T) {
	t.Helper()
	deadline := time.Now().Add(wait)
	for {
		buf := make([]byte, 1<<20)
		buf = buf[:runtime.Stack(buf, true)]
		if !bytes.Contains(buf, []byte("summonrelay.(*handler)")) {
			return
		}
		if time.Now().After(deadline) {
			t.Errorf("relay goroutines left:\n%s", buf)
			return
		}
		time.Sleep(time.Millisecond)
	}
}

// waitFor polls cond until it holds, failing the test after wait.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(wait)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("waiting for %s", what)
		}
		time.Sleep(time.Millisecond)
	}
}

// closedWith waits for the window to close and returns its reason.
func closedWith(t *testing.T, w interface {
	Done() <-chan struct{}
	CloseReason() string
}, within time.Duration) string {
	t.Helper()
	select {
	case <-w.Done():
		return w.CloseReason()
	case <-time.After(within):
		t.Fatalf("the window is still open after %v", within)
		return ""
	}
}

// expectEOF requires agw to have closed its end: the helper reads EOF, nothing else.
func expectEOF(t *testing.T, c net.Conn) {
	t.Helper()
	c.SetReadDeadline(time.Now().Add(wait))
	b, err := io.ReadAll(c)
	if err != nil || len(b) != 0 {
		t.Fatalf("the helper read %q, %v; want EOF and nothing else", b, err)
	}
}

func randBytes(n int) []byte {
	b := make([]byte, n)
	rand.Read(b)
	return b
}

// parkWindow is a FakeWindow whose first TrySend parks until released. It holds the relay's reader goroutine, so a
// test can fail a socket write before the reader reaches the helper's EOF. While unparked, TrySend never parks.
type parkWindow struct {
	*testkit.FakeWindow
	parked   chan struct{} // closed once the first TrySend parked
	release  chan struct{}
	first    sync.Once
	released sync.Once
	off      atomic.Bool // unpark: TrySend does not park; park clears it
}

func newParkWindow() *parkWindow {
	return &parkWindow{FakeWindow: testkit.NewFakeWindow(), parked: make(chan struct{}), release: make(chan struct{})}
}

func (w *parkWindow) TrySend(t byte, body []byte) bool {
	if !w.off.Load() {
		w.first.Do(func() {
			close(w.parked)
			<-w.release
		})
	}
	return w.FakeWindow.TrySend(t, body)
}

// unpark and park turn the parking off and on again.
func (w *parkWindow) unpark() { w.off.Store(true) }
func (w *parkWindow) park()   { w.off.Store(false) }

// letGo releases the parked TrySend; idempotent.
func (w *parkWindow) letGo() { w.released.Do(func() { close(w.release) }) }

// parkHost is a FakeHost whose first SetSummonLinkDegraded(true) parks until released: the reader is applying a
// LINK 1 it read just before Teardown.
type parkHost struct {
	*testkit.FakeHost
	parked   chan struct{} // closed once the first LINK 1 parked
	release  chan struct{}
	first    sync.Once
	released sync.Once
}

func newParkHost() *parkHost {
	return &parkHost{FakeHost: testkit.NewFakeHost(), parked: make(chan struct{}), release: make(chan struct{})}
}

func (h *parkHost) SetSummonLinkDegraded(v bool) {
	if v {
		h.first.Do(func() {
			close(h.parked)
			<-h.release
		})
	}
	h.FakeHost.SetSummonLinkDegraded(v)
}

// letGo releases the parked call; idempotent.
func (h *parkHost) letGo() { h.released.Do(func() { close(h.release) }) }

// ---- Spec and defaults ----

// The outer SUMMON window: cap 0x01 (Host().SummonOpen() keys on it), 0x10 in and 0x90 out of at most one INNER
// record each, both on the priority lane, a 20 s lease and no TICKs (the inner session has its own).
func TestRelaySpec(t *testing.T) {
	sp := New().Spec()
	if sp.Name != "SUMMON" || sp.ID != 0x01 || sp.LeaseMS != 20000 || sp.TickHz != 0 {
		t.Fatalf("Spec = %+v", sp)
	}
	if len(sp.In) != 1 || sp.In[0x10] != 16384 {
		t.Fatalf("In = %v, want only 0x10 of up to 16384 bytes", sp.In)
	}
	if len(sp.Out) != 1 || sp.Out[0x90] != 16384 {
		t.Fatalf("Out = %v, want only 0x90 of up to 16384 bytes", sp.Out)
	}
	if len(sp.Bulk) != 0 {
		t.Fatalf("Bulk = %v: every SUMMON type goes on the priority lane", sp.Bulk)
	}
}

// With nothing set, the relay serves the car: the helper's socket and uid 1337, the real peer credentials, the real
// Lstat owner and the real route gate.
func TestDefaults(t *testing.T) {
	for _, c := range []capability.Capability{New(), NewWithConfig(Config{})} {
		cfg := c.(*relayCap).cfg
		if cfg.Socket != "/run/airgapp-summon/relay.sock" || cfg.HelperUID != 1337 {
			t.Fatalf("socket %q, helper uid %d", cfg.Socket, cfg.HelperUID)
		}
		if cfg.PeerUID == nil || cfg.RouteCheck == nil || cfg.SocketUID == nil {
			t.Fatal("a default check is missing")
		}
	}
	if DefaultSocket != "/run/airgapp-summon/relay.sock" || HelperUID != 1337 {
		t.Fatalf("DefaultSocket %q, HelperUID %d", DefaultSocket, HelperUID)
	}
}

// ---- PreArm ----

// preArm runs PreArm with the core's 1 s budget.
func preArm(cfg Config) error {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	return NewWithConfig(cfg).PreArm(ctx, nil)
}

// The positive control for the refusals below: the route gate is open, and the socket is a socket owned by the
// helper's uid (the real Lstat owner).
func TestPreArmPasses(t *testing.T) {
	cfg := testConfig(t)
	startHelper(t, cfg.Socket, nil)
	if err := preArm(cfg); err != nil {
		t.Fatalf("PreArm: %v", err)
	}
}

// R1 fails closed, with an error of its own: nothing of the route check's text (the prefs it read) goes on.
func TestPreArmRouteRefused(t *testing.T) {
	cfg := testConfig(t)
	startHelper(t, cfg.Socket, nil)
	called := false
	cfg.RouteCheck = func(ctx context.Context) error {
		called = true
		if _, ok := ctx.Deadline(); !ok {
			t.Error("the route check got a context without the PreArm budget")
		}
		return errors.New("route gate: advertised 192.168.90.0/24 for owner@example.com")
	}
	err := preArm(cfg)
	if err == nil || !called {
		t.Fatalf("PreArm = %v (route check called: %v), want a refusal", err, called)
	}
	if strings.Contains(err.Error(), "192.168.90") || strings.Contains(err.Error(), "owner@example.com") {
		t.Fatalf("the refusal echoes the route check: %v", err)
	}
}

// The default route check is guards.RouteOff on tailscaled's socket. On a host without tailscaled it must refuse.
func TestPreArmDefaultRouteGate(t *testing.T) {
	if _, err := os.Lstat(guards.DefaultTailscaleSock); err == nil {
		t.Skipf("%s exists on this host", guards.DefaultTailscaleSock)
	}
	cfg := testConfig(t)
	startHelper(t, cfg.Socket, nil)
	cfg.RouteCheck = nil
	if err := preArm(cfg); err == nil {
		t.Fatal("PreArm passed with no tailscaled to ask: the default route gate is open")
	}
}

func TestPreArmSocketMissing(t *testing.T) {
	if err := preArm(testConfig(t)); err == nil {
		t.Fatal("PreArm passed with no helper socket")
	}
}

// The socket must be owned by the helper's uid: through the SocketUID seam, and through the default Lstat owner.
func TestPreArmSocketWrongOwner(t *testing.T) {
	cfg := testConfig(t)
	startHelper(t, cfg.Socket, nil)
	uid := os.Getuid()
	var asked string
	cfg.SocketUID = func(p string) (int, error) {
		asked = p
		return uid + 1, nil
	}
	if err := preArm(cfg); err == nil {
		t.Fatal("PreArm passed a socket owned by another uid")
	}
	if asked != cfg.Socket {
		t.Fatalf("SocketUID asked about %q, want %q", asked, cfg.Socket)
	}
	cfg.SocketUID = func(string) (int, error) { return uid, errors.New("no owner") }
	if err := preArm(cfg); err == nil {
		t.Fatal("PreArm passed when the owner could not be read")
	}
	cfg.SocketUID = nil
	cfg.HelperUID = uid + 1 // the socket's real owner is this uid
	if err := preArm(cfg); err == nil {
		t.Fatal("PreArm passed a socket whose Lstat owner is not the helper's uid")
	}
}

// A symlink to the helper's socket, or a file that is not a socket, is refused.
func TestPreArmSymlinkRefused(t *testing.T) {
	cfg := testConfig(t)
	startHelper(t, cfg.Socket, nil)
	link := filepath.Join(filepath.Dir(cfg.Socket), "link.sock")
	if err := os.Symlink(cfg.Socket, link); err != nil {
		t.Fatal(err)
	}
	viaLink := cfg
	viaLink.Socket = link
	if err := preArm(viaLink); err == nil {
		t.Fatal("PreArm passed a symlink to the socket")
	}
	file := filepath.Join(filepath.Dir(cfg.Socket), "file.sock")
	if err := os.WriteFile(file, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	plain := cfg
	plain.Socket = file
	if err := preArm(plain); err == nil {
		t.Fatal("PreArm passed a regular file")
	}
	if err := preArm(cfg); err != nil { // the control: the socket itself passes
		t.Fatalf("PreArm on the socket itself: %v", err)
	}
}

// ---- Open ----

// open runs Open with the core's 2 s budget.
func open(cfg Config, w capability.Window) (capability.Handler, error) {
	ctx, cancel := context.WithTimeout(context.Background(), wait)
	defer cancel()
	return NewWithConfig(cfg).Open(ctx, w, nil)
}

// A socket served by any uid but the helper's is closed at once, without waiting for OWNED; so is one whose peer
// credentials cannot be read.
func TestOpenWrongServerUID(t *testing.T) {
	cfg := testConfig(t)
	cfg.HelperUID = os.Getuid() + 1
	fh := startHelper(t, cfg.Socket, owned)
	if h, err := open(cfg, testkit.NewFakeWindow()); err == nil {
		h.Teardown("test")
		t.Fatal("Open accepted a socket served by another uid")
	}
	expectEOF(t, fh.next(t))

	cfg = testConfig(t)
	fh = startHelper(t, cfg.Socket, owned)
	cfg.PeerUID = func(net.Conn) (int, error) { return os.Getuid(), errors.New("no credentials") }
	if h, err := open(cfg, testkit.NewFakeWindow()); err == nil {
		h.Teardown("test")
		t.Fatal("Open accepted a socket whose server uid is unknown")
	}
	expectEOF(t, fh.next(t))
	noRelayGoroutines(t)
}

// A helper that sends nothing (it does not own the name, or is wedged) fails Open after OwnedWait, and agw closes.
func TestOpenNoOwned(t *testing.T) {
	cfg := testConfig(t)
	fh := startHelper(t, cfg.Socket, nil)
	start := time.Now()
	h, err := open(cfg, testkit.NewFakeWindow())
	took := time.Since(start)
	if err == nil {
		h.Teardown("test")
		t.Fatal("Open succeeded without OWNED")
	}
	if took < 450*time.Millisecond || took > 1500*time.Millisecond {
		t.Fatalf("Open failed after %v, want about %v", took, OwnedWait)
	}
	expectEOF(t, fh.next(t))
	noRelayGoroutines(t)
}

// The first record must be OWNED: anything else, or the helper closing, fails Open at once and closes the socket.
func TestOpenGarbageFirstRecord(t *testing.T) {
	cases := []struct {
		name  string
		greet func(*net.UnixConn)
	}{
		{"INNER first", func(c *net.UnixConn) { relay.NewWriter(c).Inner([]byte("x")) }},
		{"LINK first", func(c *net.UnixConn) { relay.NewWriter(c).Link(true) }},
		{"unknown type", func(c *net.UnixConn) { c.Write([]byte{0x7F, 0, 0}) }},
		{"OWNED with a payload", func(c *net.UnixConn) { c.Write([]byte{relay.RecOwned, 0, 1, 0}) }},
		{"closed", func(c *net.UnixConn) { c.Close() }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := testConfig(t)
			fh := startHelper(t, cfg.Socket, tc.greet)
			start := time.Now()
			h, err := open(cfg, testkit.NewFakeWindow())
			if err == nil {
				h.Teardown("test")
				t.Fatal("Open succeeded")
			}
			if took := time.Since(start); took > OwnedWait/2 {
				t.Fatalf("Open failed after %v, want at once", took)
			}
			if c := fh.next(t); tc.name != "closed" {
				expectEOF(t, c)
			}
		})
	}
	noRelayGoroutines(t)
}

// Open never blocks past its context: shutdown cancels the OWNED wait.
func TestOpenHonoursContext(t *testing.T) {
	cfg := testConfig(t)
	fh := startHelper(t, cfg.Socket, nil)
	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(50*time.Millisecond, cancel)
	start := time.Now()
	h, err := NewWithConfig(cfg).Open(ctx, testkit.NewFakeWindow(), nil)
	if err == nil {
		h.Teardown("test")
		t.Fatal("Open succeeded without OWNED")
	}
	if took := time.Since(start); took > 300*time.Millisecond {
		t.Fatalf("Open returned %v after its context ended", took-50*time.Millisecond)
	}
	expectEOF(t, fh.next(t))
}

func TestOpenNoSocket(t *testing.T) {
	if h, err := open(testConfig(t), testkit.NewFakeWindow()); err == nil {
		h.Teardown("test")
		t.Fatal("Open succeeded with no socket")
	}
}

// ---- the relay ----

// The inner stream crosses intact both ways: the phone's 0x10 bodies (at most 16384 bytes) as INNER records, and the
// helper's INNER records as 0x90 bodies of at most 16384 bytes. Frame copies its body, which the core reuses.
func TestRelayBothWays(t *testing.T) {
	w := testkit.NewFakeWindow()
	h, he := openWith(t, testConfig(t), w)
	up, down := randBytes(40000), randBytes(40000)

	for off := 0; off < len(up); off += 16384 {
		body := append([]byte(nil), up[off:min(off+16384, len(up))]...)
		if err := h.Frame(TypeInner, body); err != nil {
			t.Fatalf("Frame: %v", err)
		}
		clear(body) // the core's buffer, reused once Frame returns
	}
	var got []byte
	he.c.SetReadDeadline(time.Now().Add(wait))
	for len(got) < len(up) {
		typ, p, err := he.r.Next()
		if err != nil || typ != relay.RecInner {
			t.Fatalf("the helper read 0x%02x, %v after %d bytes", typ, err, len(got))
		}
		got = append(got, p...)
	}
	if !bytes.Equal(got, up) {
		t.Fatal("the phone's bytes reached the helper changed")
	}

	if err := he.w.Inner(down); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the helper's bytes", func() bool { return len(bytes.Join(w.Sent(TypeInnerOut), nil)) >= len(down) })
	for _, b := range w.Sent(TypeInnerOut) {
		if len(b) > 16384 {
			t.Fatalf("a 0x90 body of %d bytes", len(b))
		}
	}
	if !bytes.Equal(bytes.Join(w.Sent(TypeInnerOut), nil), down) {
		t.Fatal("the helper's bytes reached the phone changed")
	}
	if w.CloseReason() != "" {
		t.Fatalf("the window closed: %q", w.CloseReason())
	}
}

// Frame relays only 0x10: the core never hands it another type, and nothing else becomes an INNER record.
func TestFrameRefusesOtherTypes(t *testing.T) {
	h, _ := openWith(t, testConfig(t), testkit.NewFakeWindow())
	if err := h.Frame(0x11, []byte("x")); err == nil {
		t.Fatal("Frame took type 0x11")
	}
}

// LINK records set the Host's link flag through the window's Host, both ways.
func TestLinkSetsHost(t *testing.T) {
	w := testkit.NewFakeWindow()
	host := testkit.NewFakeHost()
	w.SetHost(host)
	_, he := openWith(t, testConfig(t), w)
	for _, v := range []bool{true, false, true} {
		if err := he.w.Link(v); err != nil {
			t.Fatal(err)
		}
		waitFor(t, "the link flag", func() bool { return host.SummonLinkDegraded() == v })
	}
	if w.CloseReason() != "" {
		t.Fatalf("the window closed: %q", w.CloseReason())
	}
}

// Teardown closes the socket (the helper reads EOF) and clears the link flag, even when the window never closed.
func TestTeardownClosesSocketAndClearsLink(t *testing.T) {
	w := testkit.NewFakeWindow()
	host := testkit.NewFakeHost()
	w.SetHost(host)
	h, he := openWith(t, testConfig(t), w)
	he.w.Link(true)
	waitFor(t, "the link flag", host.SummonLinkDegraded)
	teardown(t, h)
	if host.SummonLinkDegraded() {
		t.Fatal("the link flag is still set after Teardown")
	}
	expectEOF(t, he.c)
}

// Teardown clears the link flag only once the reader is gone: a LINK 1 the reader was applying when Teardown began
// cannot set the flag again after Teardown returns.
func TestTeardownClearsLinkLast(t *testing.T) {
	w := testkit.NewFakeWindow()
	host := newParkHost()
	w.SetHost(host)
	h, he := openWith(t, testConfig(t), w)
	t.Cleanup(host.letGo)
	he.w.Link(true)
	select {
	case <-host.parked:
	case <-time.After(wait):
		t.Fatal("the reader never applied the LINK")
	}
	done := make(chan struct{})
	go func() {
		h.Teardown("test")
		close(done)
	}()
	time.Sleep(50 * time.Millisecond) // a Teardown that does not wait for the reader has cleared the flag by now
	host.letGo()
	select {
	case <-done:
	case <-time.After(wait):
		t.Fatal("Teardown did not return")
	}
	noRelayGoroutines(t) // the reader has applied its LINK 1, whenever it did
	if host.SummonLinkDegraded() {
		t.Fatal("the link flag is set after Teardown")
	}
}

// The window's end closes the socket at once, before Teardown: the helper reads EOF and runs H4.
func TestWindowDoneClosesSocket(t *testing.T) {
	w := testkit.NewFakeWindow()
	_, he := openWith(t, testConfig(t), w)
	w.Close("lease")
	expectEOF(t, he.c)
}

// The helper's end of the socket closes the window with "close".
func TestHelperEOFClosesWindow(t *testing.T) {
	w := testkit.NewFakeWindow()
	_, he := openWith(t, testConfig(t), w)
	he.c.Close()
	if r := closedWith(t, w, wait); r != "close" {
		t.Fatalf("closed with %q, want close", r)
	}
}

// Any read error closes the window, not only EOF (Linux can report the helper's close as ECONNRESET): here, records
// that break the grammar.
func TestHelperBadRecordClosesWindow(t *testing.T) {
	for _, tc := range []struct {
		name string
		rec  []byte
	}{
		{"unknown type", []byte{0x7F, 0, 0}},
		{"LINK 2", []byte{relay.RecLink, 0, 1, 2}},
		{"OWNED again", []byte{relay.RecOwned, 0, 0}},
		{"empty INNER", []byte{relay.RecInner, 0, 0}},
		{"oversize INNER", []byte{relay.RecInner, 0x40, 0x01}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := testkit.NewFakeWindow()
			_, he := openWith(t, testConfig(t), w)
			he.c.Write(tc.rec)
			if r := closedWith(t, w, wait); r != "close" {
				t.Fatalf("closed with %q, want close", r)
			}
		})
	}
}

// A 0x90 the window cannot take (its priority lane is full) ends the window with "drop": a byte of the inner stream
// is never lost silently.
func TestTrySendFullDropsWindow(t *testing.T) {
	w := testkit.NewFakeWindow()
	_, he := openWith(t, testConfig(t), w)
	w.FailTrySend(true)
	he.w.Inner([]byte("car frame"))
	if r := closedWith(t, w, wait); r != "drop" {
		t.Fatalf("closed with %q, want drop", r)
	}
}

// The helper stops reading: Frame never blocks, the queue takes QueueRecords bodies and then refuses, and the write
// that cannot finish within WriteTimeout (1 s) closes the window with "drop" (R13): not before 1 s, and within
// WriteTimeout + 1 s.
func TestWriteQueueFullClosesWindow(t *testing.T) {
	w := testkit.NewFakeWindow()
	h, _ := openWith(t, testConfig(t), w)
	start := time.Now()
	body := bytes.Repeat([]byte{0xA5}, 16384)
	took := 0 // bodies Frame took; the socket buffers hold a few beyond the queue
	var ferr error
	for i := 0; i < QueueRecords+64 && ferr == nil; i++ {
		t0 := time.Now()
		if ferr = h.Frame(TypeInner, body); ferr == nil {
			took++
		}
		if d := time.Since(t0); d > 100*time.Millisecond {
			t.Fatalf("Frame blocked for %v", d)
		}
	}
	if ferr == nil {
		t.Fatalf("Frame took %d bodies of 16384 bytes with the helper not reading: the queue is unbounded", took)
	}
	if took < QueueRecords {
		t.Fatalf("Frame refused after %d bodies, want the queue to hold %d", took, QueueRecords)
	}
	if r := closedWith(t, w, 2*time.Second-time.Since(start)); r != "drop" {
		t.Fatalf("closed with %q, want drop", r)
	}
	if took := time.Since(start); took < time.Second {
		t.Fatalf("the window closed %v after the helper stopped reading: a write gets 1 s", took)
	}
}

// R13: a write that fails (the helper is gone) closes the window with "drop". The reader is parked short of the
// helper's EOF, so only the failed write can close the window.
func TestWriteErrorClosesWindow(t *testing.T) {
	w := newParkWindow()
	h, he := openWith(t, testConfig(t), w)
	t.Cleanup(w.letGo)
	he.w.Inner([]byte("first"))
	select {
	case <-w.parked:
	case <-time.After(wait):
		t.Fatal("the reader never reached TrySend")
	}
	he.c.Close() // agw's next write fails (EPIPE)
	if err := h.Frame(TypeInner, []byte("live")); err != nil {
		t.Fatal(err)
	}
	if r := closedWith(t, w, wait); r != "drop" {
		t.Fatalf("closed with %q, want drop", r)
	}
}

// After a failed write, the reader still relays what the helper sent before it closed (the inner session's BYE),
// and only then does the window close with "drop". Meanwhile the phone's frames are discarded, never refused: a
// refusal would close the window with "error" and cut the drain.
func TestWriteErrorRelaysTheHelpersLastRecords(t *testing.T) {
	w := newParkWindow()
	h, he := openWith(t, testConfig(t), w)
	t.Cleanup(w.letGo)
	he.w.Inner([]byte("first"))
	select {
	case <-w.parked:
	case <-time.After(wait):
		t.Fatal("the reader never reached TrySend")
	}
	he.w.Inner([]byte("bye")) // the inner session's last bytes
	he.c.Close()
	if err := h.Frame(TypeInner, []byte("live")); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the failed write", h.failed.Load)
	for i := 0; i < 2*QueueRecords; i++ {
		if err := h.Frame(TypeInner, []byte("live")); err != nil {
			t.Fatalf("Frame %d after the failed write: %v", i, err)
		}
	}
	w.letGo()
	if r := closedWith(t, w, wait); r != "drop" {
		t.Fatalf("closed with %q, want drop", r)
	}
	got := w.Sent(TypeInnerOut)
	if len(got) != 2 || string(got[0]) != "first" || string(got[1]) != "bye" {
		t.Fatalf("the phone got %q, want the helper's last records [first bye]", got)
	}
}

// ---- the relay's own log (INT-2, OPS-1) ----

// The core logs a capability's refusal only as "SUMMON PreArm refused: error", so PreArm logs its reason itself,
// once per refusal: never the route check's text, and nothing for a PreArm that passes.
func TestPreArmLogsItsReason(t *testing.T) {
	socketFile := func(cfg Config) Config {
		plain := filepath.Join(filepath.Dir(cfg.Socket), "file.sock")
		if err := os.WriteFile(plain, nil, 0o600); err != nil {
			t.Fatal(err)
		}
		cfg.Socket = plain
		return cfg
	}
	for _, c := range []struct {
		name, want string
		helper     bool // a socket is served at cfg.Socket
		change     func(Config) Config
	}{
		{"the route gate", "summonrelay: PreArm refused: the route gate refused", true, func(cfg Config) Config {
			cfg.RouteCheck = func(context.Context) error {
				return errors.New("route gate: advertised 192.168.90.0/24 for owner@example.com")
			}
			return cfg
		}},
		{"no socket", "summonrelay: PreArm refused: no relay socket: ", false, func(cfg Config) Config { return cfg }},
		{"not a socket", "summonrelay: PreArm refused: the relay path is not a socket", false, socketFile},
		{"another owner", fmt.Sprintf("summonrelay: PreArm refused: the relay socket is owned by uid %d, not %d",
			os.Getuid(), os.Getuid()+1), true, func(cfg Config) Config { cfg.HelperUID = os.Getuid() + 1; return cfg }},
		{"an owner that cannot be read", "summonrelay: PreArm refused: the relay socket's owner cannot be read", true,
			func(cfg Config) Config {
				cfg.SocketUID = func(string) (int, error) { return -1, errors.New("no owner") }
				return cfg
			}},
	} {
		t.Run(c.name, func(t *testing.T) {
			cfg, logs := withLog(testConfig(t))
			if c.helper {
				startHelper(t, cfg.Socket, nil)
			}
			if err := preArm(c.change(cfg)); err == nil {
				t.Fatal("PreArm passed")
			}
			oneLine(t, logs.get(), c.want)
			noSecrets(t, logs.get())
		})
	}
	cfg, logs := withLog(testConfig(t))
	startHelper(t, cfg.Socket, nil)
	if err := preArm(cfg); err != nil || len(logs.get()) != 0 {
		t.Fatalf("a passing PreArm: %v, log %q; want no error and no line", err, logs.get())
	}
}

// Open logs why it failed, once: "SUMMON Open failed: error" is all the core logs.
func TestOpenLogsItsReason(t *testing.T) {
	for _, c := range []struct {
		name, want string
		greet      func(*net.UnixConn)
		change     func(Config) Config
	}{
		{"served by another uid", fmt.Sprintf("summonrelay: Open failed: the relay socket is served by uid %d, not %d",
			os.Getuid(), os.Getuid()+1), owned, func(cfg Config) Config { cfg.HelperUID = os.Getuid() + 1; return cfg }},
		{"no OWNED", "summonrelay: Open failed: no OWNED within 500ms", nil, func(cfg Config) Config { return cfg }},
		{"closed before OWNED", "summonrelay: Open failed: closed before OWNED: the helper refused this uid, does not " +
			"own the bus name, or is unhealthy", func(c *net.UnixConn) { c.Close() }, func(cfg Config) Config { return cfg }},
	} {
		t.Run(c.name, func(t *testing.T) {
			cfg, logs := withLog(testConfig(t))
			startHelper(t, cfg.Socket, c.greet)
			if h, err := open(c.change(cfg), testkit.NewFakeWindow()); err == nil {
				h.Teardown("test")
				t.Fatal("Open succeeded")
			}
			oneLine(t, logs.get(), c.want)
		})
	}
	cfg, logs := withLog(testConfig(t))
	if h, err := open(cfg, testkit.NewFakeWindow()); err == nil {
		h.Teardown("test")
		t.Fatal("Open succeeded with no socket")
	}
	oneLine(t, logs.get(), "summonrelay: Open failed: ")
	noRelayGoroutines(t)
}

// Each window logs once why the relay ended it: the helper's close or a record that breaks the grammar, a full
// priority lane, or a write that failed. The inner stream crossed both ways first, and none of it reaches the log.
func TestWindowEndIsLoggedOnce(t *testing.T) {
	for _, c := range []struct {
		name, want string
		end        func(t *testing.T, w *parkWindow, h *handler, he *helperEnd)
	}{
		{"the helper closes", "summonrelay: the window ends: reading the helper's records: EOF",
			func(t *testing.T, w *parkWindow, h *handler, he *helperEnd) { he.c.Close() }},
		{"a record that breaks the grammar", "summonrelay: the window ends: reading the helper's records: relay: bad " +
			"record: type 0x7f", func(t *testing.T, w *parkWindow, h *handler, he *helperEnd) {
			he.c.Write([]byte{0x7F, 0, 0})
		}},
		{"the priority lane is full", "summonrelay: the window ends: the phone's priority lane refused a 0x90 of 9 " +
			"bytes (full, or the window was closing)", func(t *testing.T, w *parkWindow, h *handler, he *helperEnd) {
			w.FailTrySend(true)
			he.w.Inner([]byte("car frame"))
		}},
		{"a write fails", "summonrelay: the window ends: writing to the helper (1s deadline): ",
			func(t *testing.T, w *parkWindow, h *handler, he *helperEnd) {
				// The reader is parked short of the helper's EOF, so the failed write is the first cause.
				w.park()
				he.w.Inner([]byte("first"))
				select {
				case <-w.parked:
				case <-time.After(wait):
					t.Fatal("the reader never reached TrySend")
				}
				he.c.Close()
				if err := h.Frame(TypeInner, secretStream()); err != nil {
					t.Fatal(err)
				}
				waitFor(t, "the failed write", h.failed.Load)
				w.letGo()
			}},
	} {
		t.Run(c.name, func(t *testing.T) {
			cfg, logs := withLog(testConfig(t))
			w := newParkWindow()
			w.unpark() // TrySend parks only when a case asks for it
			t.Cleanup(w.letGo)
			h, he := openWith(t, cfg, w)
			// The inner stream, both ways, before the end.
			if err := h.Frame(TypeInner, secretStream()); err != nil {
				t.Fatal(err)
			}
			he.c.SetReadDeadline(time.Now().Add(wait))
			if typ, p, err := he.r.Next(); err != nil || typ != relay.RecInner || !bytes.Equal(p, secretStream()) {
				t.Fatalf("the helper read 0x%02x %q, %v", typ, p, err)
			}
			if err := he.w.Inner(secretStream()); err != nil {
				t.Fatal(err)
			}
			waitFor(t, "the helper's bytes", func() bool { return len(w.Sent(TypeInnerOut)) == 1 })
			c.end(t, w, h, he)
			closedWith(t, w, wait)
			teardown(t, h)
			oneLine(t, logs.get(), c.want)
			noSecrets(t, logs.get())
		})
	}
}

// A window the core closes (the lease, the phone's CLOSE, a revocation, Teardown) logs nothing from the relay: the
// core logs that end itself.
func TestCoreCloseLogsNothing(t *testing.T) {
	for _, end := range []string{"close by the core", "Teardown"} {
		t.Run(end, func(t *testing.T) {
			cfg, logs := withLog(testConfig(t))
			w := testkit.NewFakeWindow()
			h, he := openWith(t, cfg, w)
			if err := h.Frame(TypeInner, secretStream()); err != nil {
				t.Fatal(err)
			}
			he.c.SetReadDeadline(time.Now().Add(wait))
			if typ, _, err := he.r.Next(); err != nil || typ != relay.RecInner {
				t.Fatalf("the helper read 0x%02x, %v", typ, err)
			}
			if end != "Teardown" {
				w.Close("lease")
			}
			teardown(t, h)
			expectEOF(t, he.c)
			if l := logs.get(); len(l) != 0 {
				t.Fatalf("the relay logged %q for a window the core ended", l)
			}
		})
	}
}

// OPS-1: a helper that refuses agw's uid closes the connection with no record, and probe-summon, Open's replay, must
// name that cause too, not only the bus name or the helper's health.
func TestProbeNamesARefusedUID(t *testing.T) {
	path := sockPath(t)
	ln, err := relay.Listen(relay.ListenConfig{Path: path, AllowUID: os.Getuid() + 1, Ready: func() bool { return true }})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	ctx, cancel := context.WithTimeout(context.Background(), wait)
	defer cancel()
	err = Probe(ctx, path, os.Getuid(), OwnedWait)
	if err == nil || !strings.Contains(err.Error(), "closed before OWNED: the helper refused this uid, does not own "+
		"the bus name, or is unhealthy") {
		t.Fatalf("Probe = %v, want it to name a refused uid among the causes", err)
	}
}
