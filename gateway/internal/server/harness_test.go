package server

import (
	"bufio"
	"context"
	"crypto/ecdh"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"airgapp/gateway/capability"
	"airgapp/gateway/capability/echo"
	"airgapp/gateway/internal/authority"
	"airgapp/gateway/internal/clock"
	"airgapp/gateway/internal/proto"
	"airgapp/gateway/internal/session"
	"airgapp/gateway/internal/wire"
)

const testVIN = "TESTVIN0000000000"

// countOracle is authority.Fake plus a call counter.
type countOracle struct {
	authority.Fake
	calls atomic.Int32
}

func (o *countOracle) KeyRole(ctx context.Context, pk []byte) (int32, error) {
	o.calls.Add(1)
	return o.Fake.KeyRole(ctx, pk)
}

func (o *countOracle) AuthOwnerIsAuthd(ctx context.Context) (bool, error) {
	o.calls.Add(1)
	return o.Fake.AuthOwnerIsAuthd(ctx)
}

// countCap wraps a capability, optionally overriding its ID/Name, and counts
// Teardown calls. Opened windows are published on win.
type countCap struct {
	capability.Capability
	id        byte
	name      string
	teardowns atomic.Int32
	reasons   chan string
	win       chan capability.Window
}

func newCountCap(c capability.Capability, id byte, name string) *countCap {
	return &countCap{Capability: c, id: id, name: name, reasons: make(chan string, 16), win: make(chan capability.Window, 16)}
}

func (c *countCap) Spec() capability.Spec {
	s := c.Capability.Spec()
	if c.id != 0 {
		s.ID, s.Name = c.id, c.name
	}
	return s
}

func (c *countCap) Open(ctx context.Context, w capability.Window, p json.RawMessage) (capability.Handler, error) {
	h, err := c.Capability.Open(ctx, w, p)
	if err != nil {
		return nil, err
	}
	c.win <- w
	return &countHandler{Handler: h, c: c}, nil
}

type countHandler struct {
	capability.Handler
	c *countCap
}

func (h *countHandler) Teardown(r string) {
	h.c.teardowns.Add(1)
	select {
	case h.c.reasons <- r:
	default:
	}
	h.Handler.Teardown(r)
}

type env struct {
	t       *testing.T
	srv     *Server
	addr    string
	gw      *ecdsa.PrivateKey
	dev     *ecdh.PrivateKey
	clk     *clock.Fake
	oracle  *countOracle
	logPath string
	maxLog  int64
	caps    []capability.Capability
	overrun func() // Server.overrunHook

	oracleImpl  authority.Oracle // the server's oracle instead of oracle, when set
	recheck     authority.Oracle // Config.RecheckOracle
	recheckTick chan time.Time   // Server.recheckTick
	now         func() time.Time // Server.now
	afterCheck  func()           // Server.afterCheckHook

	closeOnce sync.Once
	serveDone chan struct{}
	serveErr  error
}

func newECDH(t testing.TB) *ecdh.PrivateKey {
	k, err := ecdh.P256().GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return k
}

func newECDSA(t testing.TB) *ecdsa.PrivateKey {
	k, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return k
}

// newEnv starts a server on 127.0.0.1:0 with ECHO, a Fake clock and an
// OWNER Fake oracle; opts adjust the env before the server is built.
func newEnv(t *testing.T, opts ...func(*env)) *env {
	t.Helper()
	dir := t.TempDir()
	e := &env{
		t:         t,
		gw:        newECDSA(t),
		dev:       newECDH(t),
		clk:       clock.NewFake(),
		oracle:    &countOracle{Fake: authority.Fake{Role: 2, Owner: true}},
		logPath:   filepath.Join(dir, "session.log"),
		caps:      []capability.Capability{echo.New()},
		serveDone: make(chan struct{}),
	}
	for _, o := range opts {
		o(e)
	}
	var oracle authority.Oracle = e.oracle
	if e.oracleImpl != nil {
		oracle = e.oracleImpl
	}
	e.srv = New(Config{
		Key: e.gw, VIN: testVIN, Oracle: oracle, RecheckOracle: e.recheck, Clock: e.clk,
		Caps: e.caps, LogPath: e.logPath, MaxLogBytes: e.maxLog,
	})
	e.srv.overrunHook = e.overrun
	if e.recheckTick != nil {
		e.srv.recheckTick = e.recheckTick
	}
	if e.now != nil {
		e.srv.now = e.now
	}
	e.srv.afterCheckHook = e.afterCheck
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	e.addr = ln.Addr().String()
	go func() {
		e.serveErr = e.srv.Serve(ln)
		close(e.serveDone)
	}()
	t.Cleanup(e.close)
	return e
}

// close shuts the server down and waits for Serve to return (idempotent).
func (e *env) close() {
	e.closeOnce.Do(func() {
		e.srv.Shutdown("test")
		select {
		case <-e.serveDone:
		case <-time.After(3 * time.Second):
			e.t.Error("Serve did not return after Shutdown")
		}
	})
}

func waitFor(t *testing.T, d time.Duration, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(d)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out after %v waiting for %s", d, what)
		}
		time.Sleep(2 * time.Millisecond)
	}
}

// settleGoroutines waits until the goroutine count is back to base.
func settleGoroutines(t *testing.T, base int) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for runtime.NumGoroutine() > base {
		if time.Now().After(deadline) {
			buf := make([]byte, 1<<16)
			n := runtime.Stack(buf, true)
			t.Fatalf("goroutine leak: %d > %d\n%s", runtime.NumGoroutine(), base, buf[:n])
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// client is the phone side of one connection.
type client struct {
	t     *testing.T
	e     *env
	c     net.Conn
	cap   byte
	hello []byte
	chB   []byte
	ch    proto.Challenge
	enc   []byte
	snd   *session.TestSealer
}

func (e *env) dial() *client {
	e.t.Helper()
	c, err := net.Dial("tcp", e.addr)
	if err != nil {
		e.t.Fatal(err)
	}
	e.t.Cleanup(func() { c.Close() })
	return &client{t: e.t, e: e, c: c}
}

// sendHello writes the hello frame; write errors are returned, not fatal,
// because a refused connection may already be closed.
func (cl *client) sendHello(cap byte, dev *ecdh.PrivateKey) error {
	var h proto.Hello
	h.Cap = cap
	copy(h.PkS[:], dev.PublicKey().Bytes())
	cl.cap = cap
	cl.hello = h.Bytes()
	return wire.WriteFrame(cl.c, cl.hello)
}

// readChallenge reads and verifies the challenge against the env's gateway key.
func (cl *client) readChallenge() {
	cl.t.Helper()
	cl.c.SetReadDeadline(time.Now().Add(5 * time.Second))
	b, err := wire.ReadFrame(cl.c, 1024)
	if err != nil {
		cl.t.Fatalf("read challenge: %v", err)
	}
	ch, err := proto.ParseChallenge(b)
	if err != nil {
		cl.t.Fatal(err)
	}
	if !proto.VerifyChallenge(&cl.e.gw.PublicKey, cl.hello, ch) {
		cl.t.Fatal("challenge signature does not verify with the pinned key")
	}
	if ch.Binding != proto.Binding(testVIN) {
		cl.t.Fatal("challenge binding mismatch")
	}
	cl.chB, cl.ch = b, ch
}

// armFrame builds enc || Seal(ARM) with the given fields and sets up the
// client's sealer.
func (cl *client) armFrame(dev *ecdh.PrivateKey, n []byte, cap byte, exp uint64) []byte {
	cl.t.Helper()
	return cl.firstFrame(dev, proto.TypeArm, armJSON(n, cap, exp))
}

func armJSON(n []byte, cap byte, exp uint64) []byte {
	b, _ := json.Marshal(map[string]any{"n": hex.EncodeToString(n), "cap": cap, "exp": exp, "params": map[string]any{}})
	return b
}

func (cl *client) firstFrame(dev *ecdh.PrivateKey, t byte, body []byte) []byte {
	cl.t.Helper()
	enc, snd, err := session.TestSender(dev, cl.ch.PkE[:], proto.Info(cl.hello, cl.chB))
	if err != nil {
		cl.t.Fatal(err)
	}
	cl.enc, cl.snd = enc, snd
	return append(append([]byte(nil), enc...), snd.Seal(append([]byte{t}, body...))...)
}

func (cl *client) writeRaw(b []byte) {
	cl.t.Helper()
	if err := wire.WriteFrame(cl.c, b); err != nil {
		cl.t.Fatalf("write: %v", err)
	}
}

func (cl *client) send(t byte, body []byte) {
	cl.t.Helper()
	cl.writeRaw(cl.snd.Seal(append([]byte{t}, body...)))
}

// recv reads and opens one car->phone frame; any failure is fatal.
func (cl *client) recv() (byte, []byte) {
	cl.t.Helper()
	cl.c.SetReadDeadline(time.Now().Add(5 * time.Second))
	ct, err := wire.ReadFrame(cl.c, 2<<20)
	if err != nil {
		cl.t.Fatalf("recv: %v", err)
	}
	pt, err := cl.snd.OpenS2C(ct)
	if err != nil {
		cl.t.Fatalf("recv: %v", err)
	}
	if len(pt) == 0 {
		cl.t.Fatal("recv: empty plaintext")
	}
	return pt[0], pt[1:]
}

func (cl *client) expectReady() int {
	cl.t.Helper()
	typ, body := cl.recv()
	if typ != proto.TypeReady {
		cl.t.Fatalf("want READY, got type 0x%02x %q", typ, body)
	}
	var r struct {
		LeaseMS *int `json:"lease_ms"`
	}
	if err := json.Unmarshal(body, &r); err != nil || r.LeaseMS == nil {
		cl.t.Fatalf("READY body %q: %v", body, err)
	}
	return *r.LeaseMS
}

// expectBye requires the next frame to be BYE(reason) followed by EOF.
func (cl *client) expectBye(reason string) {
	cl.t.Helper()
	typ, body := cl.recv()
	if typ != proto.TypeBye || string(body) != reason {
		cl.t.Fatalf("want BYE %q, got type 0x%02x %q", reason, typ, body)
	}
	cl.expectEOF()
}

func (cl *client) expectEOF() {
	cl.t.Helper()
	cl.c.SetReadDeadline(time.Now().Add(3 * time.Second))
	n, err := io.Copy(io.Discard, cl.c)
	if isTimeout(err) {
		cl.t.Fatal("connection not closed")
	}
	if n != 0 {
		cl.t.Fatalf("%d unexpected bytes before close", n)
	}
}

// expectClosedNoBytes requires the server to close without sending anything
// further.
func (cl *client) expectClosedNoBytes() {
	cl.t.Helper()
	cl.expectEOF()
}

func isTimeout(err error) bool {
	var ne net.Error
	return errors.As(err, &ne) && ne.Timeout()
}

func (cl *client) echo(body []byte) {
	cl.t.Helper()
	cl.send(echo.TypeIn, body)
	typ, got := cl.recv()
	if typ != echo.TypeOut || string(got) != string(body) {
		cl.t.Fatalf("echo: type 0x%02x len %d", typ, len(got))
	}
}

// open runs a full handshake for cap and returns the client after READY.
func (e *env) open(cap byte) *client {
	e.t.Helper()
	cl := e.dial()
	if err := cl.sendHello(cap, e.dev); err != nil {
		e.t.Fatal(err)
	}
	cl.readChallenge()
	cl.writeRaw(cl.armFrame(e.dev, cl.ch.N[:], cap, cl.ch.ExpiryMS))
	cl.expectReady()
	return cl
}

// logLines waits for at least n session-log lines and returns them.
func (e *env) logLines(n int) []map[string]any {
	e.t.Helper()
	var lines []map[string]any
	waitFor(e.t, 2*time.Second, "session log lines", func() bool {
		f, err := os.Open(e.logPath)
		if err != nil {
			return false
		}
		defer f.Close()
		lines = lines[:0]
		sc := bufio.NewScanner(f)
		for sc.Scan() {
			var m map[string]any
			if err := json.Unmarshal(sc.Bytes(), &m); err != nil {
				e.t.Fatalf("bad log line %q: %v", sc.Text(), err)
			}
			lines = append(lines, m)
		}
		return len(lines) >= n
	})
	return lines
}
