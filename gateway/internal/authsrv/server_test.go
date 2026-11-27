package authsrv

import (
	"bufio"
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"airgapp/gateway/camhelper/authproto"
	"airgapp/gateway/camhelper/proto"
	"airgapp/gateway/internal/authority"
)

// fakeOracle answers from a fixed list of rows (authority's real matching),
// with an owner verdict, an injected error and a delay.
type fakeOracle struct {
	mu     sync.Mutex
	rows   []authority.KeyInfoEntry
	owner  bool
	err    error
	delay  time.Duration
	ignore bool // answer even after ctx is done
	calls  int
}

func (f *fakeOracle) wait(ctx context.Context) error {
	f.mu.Lock()
	f.calls++
	d, err, ignore := f.delay, f.err, f.ignore
	f.mu.Unlock()
	if d > 0 {
		if ignore {
			time.Sleep(d)
		} else {
			select {
			case <-time.After(d):
			case <-ctx.Done():
				return ctx.Err()
			}
		}
	}
	return err
}

func (f *fakeOracle) AuthOwnerIsAuthd(ctx context.Context) (bool, error) {
	if err := f.wait(ctx); err != nil {
		return false, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.owner, nil
}

func (f *fakeOracle) KeyEntry(ctx context.Context, pk []byte) (authority.KeyInfoEntry, error) {
	if err := f.wait(ctx); err != nil {
		return authority.KeyInfoEntry{}, err
	}
	f.mu.Lock()
	rows := f.rows
	f.mu.Unlock()
	body := []interface{}{[][]interface{}{}}
	for _, r := range rows {
		body[0] = append(body[0].([][]interface{}), []interface{}{r.ID, r.PublicKey, r.Role, r.FormFactor, r.SecondsLeft})
	}
	entries, err := authority.ParseKeyInfoBody(body)
	if err != nil {
		return authority.KeyInfoEntry{}, err
	}
	role, _, err := authority.RoleFor(entries, pk)
	if err != nil {
		return authority.KeyInfoEntry{}, err
	}
	for _, e := range entries {
		if bytes.Equal(e.PublicKey, pk) && int32(e.Role) == role {
			return e, nil
		}
	}
	return authority.KeyInfoEntry{}, errors.New("unreachable")
}

// The phone key of the car tests (public).
const phoneHex = "04e70805d8e2d25f6337d3ba01c18d7247b38afc0330ee85b21bdf490a85eb667ff5acda7cd5af3d21a90a12d3e3c656d512a598da3a8e6494bf66ea8a9c69a0a9"

func phone(t *testing.T) []byte {
	b, err := hex.DecodeString(phoneHex)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func row(pk []byte, role, ff uint32) authority.KeyInfoEntry {
	return authority.KeyInfoEntry{ID: authority.KeyID(pk), PublicKey: pk, Role: role, FormFactor: ff}
}

type rig struct {
	t    *testing.T
	srv  *Server
	sock string
	or   *fakeOracle
	logs *syncBuf
	now  *fakeClock
	done chan error
}

type syncBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuf) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuf) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fakeClock) Add(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

func newRig(t *testing.T, edit func(*Config)) *rig {
	t.Helper()
	dir, err := os.MkdirTemp("", "authsrv") // short: unix socket paths are limited
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	r := &rig{t: t, sock: filepath.Join(dir, "a.sock"), logs: &syncBuf{}, now: &fakeClock{t: time.Unix(1_800_000_000, 0)},
		or: &fakeOracle{owner: true, rows: []authority.KeyInfoEntry{row(phone(t), 2, 6)}}}
	cfg := Config{Oracle: r.or, AllowUID: os.Getuid(), Budget: 200 * time.Millisecond, Now: r.now.Now,
		Log: log.New(r.logs, "", 0)}
	if edit != nil {
		edit(&cfg)
	}
	r.srv = New(cfg)
	ln, err := net.Listen("unix", r.sock)
	if err != nil {
		t.Fatal(err)
	}
	r.done = make(chan error, 1)
	go func() { r.done <- r.srv.Serve(ln) }()
	t.Cleanup(func() {
		r.srv.Shutdown()
		select {
		case <-r.done:
		case <-time.After(5 * time.Second):
			t.Error("Serve did not return after Shutdown")
		}
	})
	return r
}

type client struct {
	t *testing.T
	c net.Conn
	r *bufio.Reader
}

func (r *rig) dial() *client {
	r.t.Helper()
	c, err := net.Dial("unix", r.sock)
	if err != nil {
		r.t.Fatal(err)
	}
	r.t.Cleanup(func() { c.Close() })
	return &client{t: r.t, c: c, r: bufio.NewReader(c)}
}

func (c *client) send(v interface{}) {
	c.t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		c.t.Fatal(err)
	}
	c.raw(authproto.TReq, b)
}

// trySend is send for a connection the helper may already have closed.
func (c *client) trySend(v interface{}) {
	b, _ := json.Marshal(v)
	proto.WriteFrame(c.c, authproto.TReq, b)
}

func (c *client) raw(t byte, body []byte) {
	c.t.Helper()
	if err := proto.WriteFrame(c.c, t, body); err != nil {
		c.t.Fatal(err)
	}
}

// next reads one helper frame (or fails on timeout/close).
func (c *client) next() (byte, []byte, error) {
	c.c.SetReadDeadline(time.Now().Add(3 * time.Second))
	return proto.ReadFrame(c.r, authproto.MaxHelperFrame)
}

func (c *client) resp() authproto.Response {
	c.t.Helper()
	t, b, err := c.next()
	if err != nil || t != authproto.TResp {
		c.t.Fatalf("want a RESP: type %#x %v", t, err)
	}
	if len(b) > authproto.MaxResp {
		c.t.Fatalf("RESP body %d bytes > %d", len(b), authproto.MaxResp)
	}
	var r authproto.Response
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&r); err != nil {
		c.t.Fatalf("RESP %s: %v", b, err)
	}
	return r
}

func (c *client) event() authproto.Event {
	c.t.Helper()
	t, b, err := c.next()
	if err != nil || t != authproto.TEvent {
		c.t.Fatalf("want an EVENT: type %#x %s %v", t, b, err)
	}
	if len(b) > authproto.MaxEvent {
		c.t.Fatalf("EVENT body %d bytes > %d", len(b), authproto.MaxEvent)
	}
	var e authproto.Event
	if err := json.Unmarshal(b, &e); err != nil {
		c.t.Fatal(err)
	}
	return e
}

// closed asserts the helper closes the connection (after any frames).
func (c *client) closed() {
	c.t.Helper()
	for i := 0; i < 4; i++ {
		if _, _, err := c.next(); err != nil {
			if ne, ok := err.(net.Error); ok && ne.Timeout() {
				c.t.Fatal("the helper did not close the connection")
			}
			return
		}
	}
	c.t.Fatal("the helper kept sending instead of closing")
}

func req(id uint32, op, pk string) authproto.Request {
	return authproto.Request{V: authproto.V, ID: id, Op: op, PK: pk}
}

func TestOwnerAndKey(t *testing.T) {
	r := newRig(t, nil)
	c := r.dial()
	c.send(req(1, authproto.OpOwner, ""))
	if got := c.resp(); !got.OK || got.ID != 1 || got.Err != "" {
		t.Fatalf("owner: %+v", got)
	}
	c.send(req(2, authproto.OpKey, phoneHex))
	got := c.resp()
	if !got.OK || got.ID != 2 || got.Role != 2 || got.FF != 6 || got.Secs != 0 || got.KID != "cce6a2d2" {
		t.Fatalf("key: %+v", got)
	}
	// Any number of requests on one connection, answered in order.
	for id := uint32(3); id < 8; id++ {
		c.send(req(id, authproto.OpOwner, ""))
		if got := c.resp(); got.ID != id || !got.OK {
			t.Fatalf("in order: %+v", got)
		}
	}
}

func TestDenials(t *testing.T) {
	r := newRig(t, nil)
	c := r.dial()
	// A valid point that is not listed.
	other := "04" + strings.Repeat("11", 64)
	c.send(req(1, authproto.OpKey, other))
	if got := c.resp(); got.OK || got.Err != authproto.ErrKeyUnknown || !strings.Contains(got.Msg, "not in authd's KeyInfo list") {
		t.Fatalf("unknown key: %+v", got)
	}
	// The owner is not authd.
	r.or.mu.Lock()
	r.or.owner = false
	r.or.mu.Unlock()
	c.send(req(2, authproto.OpOwner, ""))
	if got := c.resp(); got.OK || got.Err != authproto.ErrAuthOwner {
		t.Fatalf("owner false: %+v", got)
	}
	// Any oracle error denies, with its class.
	r.or.mu.Lock()
	r.or.owner, r.or.err = true, fmt.Errorf("authority: %w: connection refused", authority.ErrBus)
	r.or.mu.Unlock()
	c.send(req(3, authproto.OpKey, phoneHex))
	if got := c.resp(); got.OK || got.Err != authproto.ErrBus || !strings.Contains(got.Msg, "connection refused") {
		t.Fatalf("oracle error: %+v", got)
	}
	// One that wraps no class is auth_failed.
	r.or.mu.Lock()
	r.or.err = errors.New("authority: something else")
	r.or.mu.Unlock()
	c.send(req(4, authproto.OpOwner, ""))
	if got := c.resp(); got.OK || got.Err != authproto.ErrAuth || !strings.Contains(got.Msg, "something else") {
		t.Fatalf("unclassed oracle error: %+v", got)
	}
	// The full key never reaches the log; its kid does.
	if logs := r.logs.String(); strings.Contains(logs, phoneHex[2:40]) || strings.Contains(logs, other[2:40]) || !strings.Contains(logs, "kid=cce6a2d2") {
		t.Fatalf("logs must name keys by kid only:\n%s", logs)
	}
}

func TestBudget(t *testing.T) {
	r := newRig(t, nil)
	c := r.dial()
	r.or.mu.Lock()
	r.or.delay = time.Second
	r.or.mu.Unlock()
	start := time.Now()
	c.send(req(1, authproto.OpKey, phoneHex))
	got := c.resp()
	if got.OK || got.Err != authproto.ErrTimeout || time.Since(start) > 700*time.Millisecond {
		t.Fatalf("over budget: %+v after %v", got, time.Since(start))
	}
	// An oracle that ignores the deadline and then succeeds is still denied.
	r.or.mu.Lock()
	r.or.delay, r.or.ignore = 300*time.Millisecond, true
	r.or.mu.Unlock()
	c.send(req(2, authproto.OpKey, phoneHex))
	if got := c.resp(); got.OK || got.Err != authproto.ErrTimeout {
		t.Fatalf("late success must deny: %+v", got)
	}
}

func TestStrictRequests(t *testing.T) {
	r := newRig(t, nil)
	bad := []struct {
		name string
		body string
	}{
		{"version 2", `{"v":2,"id":1,"op":"owner"}`},
		{"no version", `{"id":1,"op":"owner"}`},
		{"unknown op", `{"v":1,"id":1,"op":"role"}`},
		{"extra field", `{"v":1,"id":1,"op":"owner","x":1}`},
		{"pk with owner", `{"v":1,"id":1,"op":"owner","pk":"` + phoneHex + `"}`},
		{"pk with watch", `{"v":1,"id":1,"op":"watch","pk":"` + phoneHex + `"}`},
		{"key without pk", `{"v":1,"id":1,"op":"key"}`},
		{"uppercase pk", `{"v":1,"id":1,"op":"key","pk":"` + strings.ToUpper(phoneHex) + `"}`},
		{"short pk", `{"v":1,"id":1,"op":"key","pk":"` + phoneHex[:128] + `"}`},
		{"two objects", `{"v":1,"id":1,"op":"owner"}{"v":1,"id":2,"op":"owner"}`},
		{"not json", `owner`},
		{"upper-case keys", `{"V":1,"ID":7,"OP":"owner"}`},
		{"duplicate op", `{"v":1,"id":1,"op":"key","op":"owner"}`},
		{"duplicate v", `{"v":2,"v":1,"id":1,"op":"owner"}`},
		{"missing id", `{"v":1,"op":"owner"}`},
		{"negative id", `{"v":1,"id":-1,"op":"owner"}`},
		{"string v", `{"v":"1","id":1,"op":"owner"}`},
		{"float v", `{"v":1.0,"id":1,"op":"owner"}`},
		{"null op", `{"v":1,"id":1,"op":null}`},
		{"null pk", `{"v":1,"id":1,"op":"owner","pk":null}`},
		{"empty pk", `{"v":1,"id":1,"op":"owner","pk":""}`},
		{"empty pk on key", `{"v":1,"id":1,"op":"key","pk":""}`},
		{"array", `[{"v":1,"id":1,"op":"owner"}]`},
		{"trailing comma", `{"v":1,"id":1,"op":"owner",}`},
	}
	for _, b := range bad {
		c := r.dial()
		c.raw(authproto.TReq, []byte(b.body))
		if got := c.resp(); got.OK || got.Err != authproto.ErrBadRequest {
			t.Fatalf("%s: %+v", b.name, got)
		}
		c.closed()
	}
	// An unknown frame type, or a frame over the limit: closed without a reply.
	c := r.dial()
	c.raw(0x02, []byte("pcm"))
	c.closed()
	c = r.dial()
	c.raw(authproto.TReq, bytes.Repeat([]byte(" "), authproto.MaxReq+1))
	c.closed()
	if n := r.or.calls; n != 0 {
		t.Fatalf("no bad request may reach the oracle (%d calls)", n)
	}
}

func TestPeerUID(t *testing.T) {
	r := newRig(t, func(c *Config) { c.AllowUID = os.Getuid() + 1 })
	c := r.dial()
	c.trySend(req(1, authproto.OpOwner, "")) // may already fail: the helper closes at once
	c.closed()
	if !strings.Contains(r.logs.String(), "refused peer uid") {
		t.Fatalf("log: %s", r.logs.String())
	}
}

func TestRateLimit(t *testing.T) {
	r := newRig(t, func(c *Config) { c.Burst, c.PerSec = 3, 1 })
	c := r.dial()
	for id := uint32(1); id <= 3; id++ {
		c.send(req(id, authproto.OpOwner, ""))
		if got := c.resp(); !got.OK {
			t.Fatalf("within the burst: %+v", got)
		}
	}
	c.send(req(4, authproto.OpOwner, ""))
	if got := c.resp(); got.OK || got.Err != authproto.ErrRateLimited || got.ID != 4 {
		t.Fatalf("over the burst: %+v", got)
	}
	calls := r.or.calls
	r.now.Add(time.Second) // one token back
	c.send(req(5, authproto.OpOwner, ""))
	if got := c.resp(); !got.OK {
		t.Fatalf("refilled: %+v", got)
	}
	if r.or.calls != calls+1 {
		t.Fatal("a rate-limited request must not reach the oracle")
	}
}

func TestWatch(t *testing.T) {
	r := newRig(t, nil)
	w := r.dial()
	w.send(req(1, authproto.OpWatch, ""))
	if got := w.resp(); !got.OK || got.ID != 1 {
		t.Fatalf("watch: %+v", got)
	}
	// The current state at once (no subscription yet), then keys_changed: anything
	// may have changed before the watcher attached.
	if e := w.event(); e.Ev != authproto.EvWatchDown {
		t.Fatalf("initial: %+v", e)
	}
	if e := w.event(); e.Ev != authproto.EvKeysChanged || e.Reason != authproto.ReasonResubscribed {
		t.Fatalf("attach: %+v", e)
	}
	r.srv.Notify(authproto.Event{Ev: authproto.EvWatchUp})
	r.srv.Notify(authproto.Event{Ev: authproto.EvKeysChanged, Reason: authproto.ReasonResubscribed})
	r.srv.Notify(authproto.Event{Ev: authproto.EvKeysChanged, Reason: authproto.ReasonKeysUpdated})
	for _, want := range []authproto.Event{{Ev: authproto.EvWatchUp}, {Ev: authproto.EvKeysChanged, Reason: authproto.ReasonResubscribed},
		{Ev: authproto.EvKeysChanged, Reason: authproto.ReasonKeysUpdated}} {
		if e := w.event(); e != want {
			t.Fatalf("got %+v, want %+v", e, want)
		}
	}
	// A second watcher gets the state as it is now (up), and both get events.
	w2 := r.dial()
	w2.send(req(7, authproto.OpWatch, ""))
	if got := w2.resp(); !got.OK {
		t.Fatalf("watch 2: %+v", got)
	}
	if e := w2.event(); e.Ev != authproto.EvWatchUp {
		t.Fatalf("initial 2: %+v", e)
	}
	if e := w2.event(); e.Ev != authproto.EvKeysChanged {
		t.Fatalf("attach 2: %+v", e)
	}
	r.srv.Notify(authproto.Event{Ev: authproto.EvKeysChanged, Reason: authproto.ReasonOwnerChanged})
	for _, c := range []*client{w, w2} {
		if e := c.event(); e.Ev != authproto.EvKeysChanged || e.Reason != authproto.ReasonOwnerChanged {
			t.Fatalf("fan-out: %+v", e)
		}
	}
	// Anything sent on a watch connection ends it.
	w.send(req(2, authproto.OpOwner, ""))
	w.closed()
	// watch after a request on a request connection: refused.
	c := r.dial()
	c.send(req(1, authproto.OpOwner, ""))
	c.resp()
	c.send(req(2, authproto.OpWatch, ""))
	if got := c.resp(); got.OK || got.Err != authproto.ErrBadRequest {
		t.Fatalf("watch on a request connection: %+v", got)
	}
	c.closed()
}

// A watcher that does not read is cut off rather than holding events back.
func TestSlowWatcherIsCut(t *testing.T) {
	r := newRig(t, func(c *Config) { c.EventBuf = 2 })
	w := r.dial()
	w.send(req(1, authproto.OpWatch, ""))
	w.resp()
	for i := 0; i < 20000; i++ {
		r.srv.Notify(authproto.Event{Ev: authproto.EvKeysChanged, Reason: authproto.ReasonKeysUpdated})
	}
	deadline := time.Now().Add(3 * time.Second)
	for !strings.Contains(r.logs.String(), "watcher too slow") {
		if time.Now().After(deadline) {
			t.Fatalf("a stuck watcher must be closed: %s", r.logs.String())
		}
		time.Sleep(10 * time.Millisecond)
	}
	// Draining now ends in EOF, not in an endless stream.
	w.c.SetReadDeadline(time.Now().Add(3 * time.Second))
	_, err := io.Copy(io.Discard, w.c)
	if ne, ok := err.(net.Error); ok && ne.Timeout() {
		t.Fatal("the slow watcher's connection was not closed")
	}
}

func TestMaxConns(t *testing.T) {
	r := newRig(t, func(c *Config) { c.MaxConns = 2 })
	a, b := r.dial(), r.dial()
	a.send(req(1, authproto.OpOwner, ""))
	a.resp()
	b.send(req(1, authproto.OpOwner, ""))
	b.resp()
	c := r.dial()
	c.trySend(req(1, authproto.OpOwner, "")) // may already fail: the helper closes at once
	c.closed()
	a.c.Close()
	// The slot frees once the helper has seen a's close.
	deadline := time.Now().Add(3 * time.Second)
	for {
		d := r.dial()
		d.trySend(req(1, authproto.OpOwner, ""))
		if typ, body, err := d.next(); err == nil && typ == authproto.TResp && strings.Contains(string(body), `"ok":true`) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("a freed slot was not reused")
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestShutdownClosesEverything(t *testing.T) {
	r := newRig(t, nil)
	w := r.dial()
	w.send(req(1, authproto.OpWatch, ""))
	w.resp()
	w.event()
	c := r.dial()
	c.send(req(1, authproto.OpOwner, ""))
	c.resp()
	r.srv.Shutdown()
	select {
	case err := <-r.done:
		if err != nil {
			t.Fatalf("Serve: %v", err)
		}
		r.done <- nil // for the rig's cleanup
	case <-time.After(3 * time.Second):
		t.Fatal("Serve did not return")
	}
	w.closed()
	c.closed()
	r.srv.Notify(authproto.Event{Ev: authproto.EvKeysChanged, Reason: authproto.ReasonKeysUpdated}) // no panic after shutdown
}

// A long authority error still fits a RESP.
func TestLongErrorFits(t *testing.T) {
	r := newRig(t, nil)
	r.or.mu.Lock()
	r.or.err = errors.New(strings.Repeat("x", 4000))
	r.or.mu.Unlock()
	c := r.dial()
	c.send(req(1, authproto.OpKey, phoneHex))
	if got := c.resp(); got.OK || got.Err != authproto.ErrAuth {
		t.Fatalf("%+v", got)
	}
}

// An oracle row for another key is a denial, never that key's role.
type wrongKeyOracle struct{ fakeOracle }

func (w *wrongKeyOracle) KeyEntry(ctx context.Context, pk []byte) (authority.KeyInfoEntry, error) {
	other, _ := hex.DecodeString("04" + strings.Repeat("22", 64))
	return row(other, 2, 6), nil
}

func TestOracleAnsweringForAnotherKeyDenies(t *testing.T) {
	wk := &wrongKeyOracle{fakeOracle{owner: true}}
	r := newRig(t, func(c *Config) { c.Oracle = wk })
	c := r.dial()
	c.send(req(1, authproto.OpKey, phoneHex))
	if got := c.resp(); got.OK || got.Role != 0 || got.KID != "" || got.Err != authproto.ErrAuth {
		t.Fatalf("%+v", got)
	}
}

// A watcher attached while events flow never misses one: every Notify after
// its RESP reaches it, after its two attach events.
func TestWatchAttachUnderLoad(t *testing.T) {
	r := newRig(t, func(c *Config) { c.EventBuf = 1 << 12 })
	stop := make(chan struct{})
	var sent sync.WaitGroup
	sent.Add(1)
	go func() {
		defer sent.Done()
		for {
			select {
			case <-stop:
				return
			default:
				r.srv.Notify(authproto.Event{Ev: authproto.EvKeysChanged, Reason: authproto.ReasonKeysUpdated})
				time.Sleep(50 * time.Microsecond)
			}
		}
	}()
	for i := 0; i < 20; i++ {
		w := r.dial()
		w.send(req(1, authproto.OpWatch, ""))
		if got := w.resp(); !got.OK {
			t.Fatalf("watch: %+v", got)
		}
		if e := w.event(); e.Ev != authproto.EvWatchDown {
			t.Fatalf("first: %+v", e)
		}
		if e := w.event(); e.Ev != authproto.EvKeysChanged || e.Reason != authproto.ReasonResubscribed {
			t.Fatalf("attach: %+v", e)
		}
		w.c.Close()
	}
	close(stop)
	sent.Wait()
}

// Every authority error class maps to its code; the budget wins over a class
// it also wraps, and an error that wraps none is auth_failed.
func TestClassify(t *testing.T) {
	for _, tc := range []struct {
		err  error
		want string
	}{
		{fmt.Errorf("authority: %w: owned by uid 0", authority.ErrAuthOwner), authproto.ErrAuthOwner},
		{fmt.Errorf("authority: %w", authority.ErrKeyUnknown), authproto.ErrKeyUnknown},
		{fmt.Errorf("authority: %w", authority.ErrKeyAmbiguous), authproto.ErrKeyAmbig},
		{fmt.Errorf("authority: KeyInfo: %w: Id", authority.ErrShape), authproto.ErrShape},
		{fmt.Errorf("authority: %w: dial", authority.ErrBus), authproto.ErrBus},
		{fmt.Errorf("authority: %w: sha256", authority.ErrAuthdChanged), authproto.ErrAuthdChange},
		{context.DeadlineExceeded, authproto.ErrTimeout},
		{context.Canceled, authproto.ErrTimeout},
		{fmt.Errorf("authority: %w: %w", authority.ErrBus, context.DeadlineExceeded), authproto.ErrTimeout},
		{errors.New("authority: the oracle answered for another key"), authproto.ErrAuth},
	} {
		if got := classify(tc.err); got != tc.want {
			t.Errorf("classify(%v) = %q, want %q", tc.err, got, tc.want)
		}
	}
}
