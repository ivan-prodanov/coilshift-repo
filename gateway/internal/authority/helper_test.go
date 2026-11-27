package authority

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
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"airgapp/gateway/camhelper/authproto"
	"airgapp/gateway/camhelper/proto"
)

// phoneHex is phonePK as the gateway sends it: 130 lowercase hex digits.
var phoneHex = hex.EncodeToString(phonePK)

// shortDir is a temp dir with a short path: t.TempDir() names can exceed
// the 104-byte unix socket path limit on macOS.
func shortDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "ah")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	return dir
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

// fakeHelper is the camera root helper's sign-in socket (camhelper/authproto
// over a unix socket) for the client tests. By default it answers owner ok
// and key from rows (unlisted: key_unknown), and a watch with the state and
// keys_changed "resubscribed", as the real helper does; respond and attach
// replace that per test.
type fakeHelper struct {
	t    *testing.T
	sock string

	mu       sync.Mutex
	ln       net.Listener
	conns    map[*fakeConn]struct{}
	accepted int
	eofs     int // connections the client closed
	reqs     []authproto.Request
	rows     map[string]KeyInfoEntry
	up       bool // the state a new watcher is told
	respond  func(fc *fakeConn, req authproto.Request)
	attach   func(fc *fakeConn, req authproto.Request)
	wg       sync.WaitGroup
}

type fakeConn struct {
	h     *fakeHelper
	c     net.Conn
	wmu   sync.Mutex
	watch bool
}

func newFakeHelper(t *testing.T) *fakeHelper {
	return newFakeHelperAt(t, filepath.Join(shortDir(t), "a.sock"))
}

func newFakeHelperAt(t *testing.T, sock string) *fakeHelper {
	h := &fakeHelper{t: t, sock: sock, conns: map[*fakeConn]struct{}{},
		rows: map[string]KeyInfoEntry{string(phonePK): phoneRow()}}
	h.listen()
	t.Cleanup(h.stop)
	return h
}

func (h *fakeHelper) listen() {
	ln, err := net.Listen("unix", h.sock)
	if err != nil {
		h.t.Fatal(err)
	}
	h.mu.Lock()
	h.ln = ln
	h.mu.Unlock()
	h.wg.Add(1)
	go func() {
		defer h.wg.Done()
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			fc := &fakeConn{h: h, c: c}
			h.mu.Lock()
			h.accepted++
			h.conns[fc] = struct{}{}
			h.mu.Unlock()
			h.wg.Add(1)
			go fc.serve()
		}
	}()
}

// stop closes the listener and every connection, and waits for them.
func (h *fakeHelper) stop() {
	h.mu.Lock()
	ln := h.ln
	h.ln = nil
	var all []*fakeConn
	for fc := range h.conns {
		all = append(all, fc)
	}
	h.mu.Unlock()
	if ln != nil {
		ln.Close()
	}
	for _, fc := range all {
		fc.c.Close()
	}
	h.wg.Wait()
	os.Remove(h.sock)
}

// restart is the helper restarting: every connection dropped, a new listener.
func (h *fakeHelper) restart() {
	h.stop()
	h.listen()
}

func (fc *fakeConn) serve() {
	h := fc.h
	defer h.wg.Done()
	defer func() {
		fc.c.Close()
		h.mu.Lock()
		delete(h.conns, fc)
		h.mu.Unlock()
	}()
	r := bufio.NewReader(fc.c)
	for {
		t, body, err := proto.ReadFrame(r, authproto.MaxClientFrame)
		if err != nil {
			if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
				h.mu.Lock()
				h.eofs++
				h.mu.Unlock()
			}
			return
		}
		if t != authproto.TReq {
			h.t.Errorf("the client sent frame type %#x", t)
			return
		}
		var req authproto.Request
		dec := json.NewDecoder(bytes.NewReader(body))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&req); err != nil || req.V != authproto.V {
			h.t.Errorf("the client sent %s (%v)", body, err)
			return
		}
		if (req.Op == authproto.OpKey) != (req.PK != "") {
			h.t.Errorf("the client sent %s", body)
			return
		}
		if req.Op == authproto.OpKey {
			if _, err := authproto.ParsePK(req.PK); err != nil {
				h.t.Errorf("the client sent pk %q: %v", req.PK, err)
				return
			}
		}
		h.mu.Lock()
		h.reqs = append(h.reqs, req)
		respond, attach := h.respond, h.attach
		h.mu.Unlock()
		switch {
		case req.Op == authproto.OpWatch:
			fc.wmu.Lock()
			fc.watch = true
			fc.wmu.Unlock()
			if attach == nil {
				attach = h.attachOK
			}
			attach(fc, req)
		case respond != nil:
			respond(fc, req)
		default:
			h.answer(fc, req)
		}
	}
}

// answer is the default: owner ok, key from rows.
func (h *fakeHelper) answer(fc *fakeConn, req authproto.Request) {
	r := authproto.Response{ID: req.ID}
	switch req.Op {
	case authproto.OpOwner:
		r.OK = true
	case authproto.OpKey:
		pk, _ := authproto.ParsePK(req.PK)
		h.mu.Lock()
		e, ok := h.rows[string(pk)]
		h.mu.Unlock()
		if !ok {
			r.Err, r.Msg = authproto.ErrKeyUnknown, "authority: key not in authd's KeyInfo list"
		} else {
			r.OK, r.Role, r.FF, r.Secs, r.KID = true, e.Role, e.FormFactor, e.SecondsLeft, hex.EncodeToString(e.ID)
		}
	}
	fc.resp(r)
}

// attachOK is the real helper's attach: ok, the state, keys_changed resubscribed.
func (h *fakeHelper) attachOK(fc *fakeConn, req authproto.Request) {
	fc.resp(authproto.Response{ID: req.ID, OK: true})
	h.mu.Lock()
	ev := authproto.EvWatchDown
	if h.up {
		ev = authproto.EvWatchUp
	}
	h.mu.Unlock()
	fc.event(authproto.Event{Ev: ev})
	fc.event(authproto.Event{Ev: authproto.EvKeysChanged, Reason: authproto.ReasonResubscribed})
}

func (h *fakeHelper) setRespond(f func(fc *fakeConn, req authproto.Request)) {
	h.mu.Lock()
	h.respond = f
	h.mu.Unlock()
}

func (h *fakeHelper) setAttach(f func(fc *fakeConn, req authproto.Request)) {
	h.mu.Lock()
	h.attach = f
	h.mu.Unlock()
}

// frame writes one frame (errors ignored: the client may have closed).
func (fc *fakeConn) frame(t byte, body []byte) {
	fc.wmu.Lock()
	defer fc.wmu.Unlock()
	fc.c.SetWriteDeadline(time.Now().Add(2 * time.Second))
	proto.WriteFrame(fc.c, t, body)
}

func (fc *fakeConn) resp(r authproto.Response) {
	b, err := json.Marshal(r)
	if err != nil {
		fc.h.t.Error(err)
	}
	fc.frame(authproto.TResp, b)
}

func (fc *fakeConn) event(e authproto.Event) {
	b, _ := json.Marshal(e)
	fc.frame(authproto.TEvent, b)
}

// notify sends e to every watcher; watch_up and watch_down also set the
// state a new watcher is told.
func (h *fakeHelper) notify(e authproto.Event) {
	h.watchFrame(authproto.TEvent, func() []byte { b, _ := json.Marshal(e); return b }())
	h.mu.Lock()
	switch e.Ev {
	case authproto.EvWatchUp:
		h.up = true
	case authproto.EvWatchDown:
		h.up = false
	}
	h.mu.Unlock()
}

// watchFrame sends one raw frame to every watcher.
func (h *fakeHelper) watchFrame(t byte, body []byte) {
	h.mu.Lock()
	var ws []*fakeConn
	for fc := range h.conns {
		fc.wmu.Lock()
		if fc.watch {
			ws = append(ws, fc)
		}
		fc.wmu.Unlock()
	}
	h.mu.Unlock()
	for _, fc := range ws {
		fc.frame(t, body)
	}
}

func (h *fakeHelper) counts() (accepted, eofs int) {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.accepted, h.eofs
}

func (h *fakeHelper) requests() []authproto.Request {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]authproto.Request(nil), h.reqs...)
}

func (h *fakeHelper) watchers() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	n := 0
	for fc := range h.conns {
		fc.wmu.Lock()
		if fc.watch {
			n++
		}
		fc.wmu.Unlock()
	}
	return n
}

// waitEOFs waits until the client has closed n connections in all.
func (h *fakeHelper) waitEOFs(n int) {
	h.t.Helper()
	waitUntil(h.t, fmt.Sprintf("%d connection(s) closed by the client", n), func() bool {
		_, e := h.counts()
		return e >= n
	})
}

func waitUntil(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(2 * time.Millisecond)
	}
}

// newTestHelper is a client for a helper running as this test's uid, with a
// fake clock for the re-dial limit and a fast watch backoff.
func newTestHelper(t *testing.T, sock string, edit func(*HelperConfig)) (*Helper, *fakeClock, *syncBuf) {
	t.Helper()
	clk := &fakeClock{t: time.Unix(1_000_000, 0)}
	logs := &syncBuf{}
	cfg := HelperConfig{HelperUID: os.Getuid(), Now: clk.now, Log: log.New(logs, "", 0),
		MinBackoff: 20 * time.Millisecond, MaxBackoff: 50 * time.Millisecond}
	if edit != nil {
		edit(&cfg)
	}
	h := NewHelper(sock, cfg)
	t.Cleanup(func() { h.Close() })
	return h, clk, logs
}

// connectedForTest reports whether the request connection is up.
func (h *Helper) connectedForTest() bool {
	h.lock <- struct{}{}
	defer h.release()
	if h.hc == nil {
		return false
	}
	select {
	case <-h.hc.dead:
		return false
	default:
		return true
	}
}

// classes is the six authority classes and ErrHelper.
var classes = append(append([]struct {
	name string
	err  error
}{}, sentinels...), struct {
	name string
	err  error
}{"ErrHelper", ErrHelper})

// wantHelperClass requires err to match exactly want among the six classes
// and ErrHelper.
func wantHelperClass(t *testing.T, name string, err, want error) {
	t.Helper()
	if err == nil {
		t.Errorf("%s: no error, want %v", name, want)
		return
	}
	for _, c := range classes {
		if got := errors.Is(err, c.err); got != (c.err == want) {
			t.Errorf("%s: errors.Is(%q, %s) = %v", name, err, c.name, got)
		}
	}
}

func TestHelperOwnerAndKey(t *testing.T) {
	fh := newFakeHelper(t)
	h, _, _ := newTestHelper(t, fh.sock, nil)
	var _ KeyEntryOracle = h
	if ok, err := h.AuthOwnerIsAuthd(ctxT(t)); err != nil || !ok {
		t.Fatalf("owner: %v %v", ok, err)
	}
	e, err := h.KeyEntry(ctxT(t), phonePK)
	if err != nil {
		t.Fatal(err)
	}
	want := KeyInfoEntry{ID: phoneID, PublicKey: phonePK, Role: 2, FormFactor: 6}
	if !reflect.DeepEqual(e, want) {
		t.Fatalf("KeyEntry %+v, want %+v", e, want)
	}
	if role, err := h.KeyRole(ctxT(t), phonePK); err != nil || role != 2 {
		t.Fatalf("KeyRole %d %v", role, err)
	}
	if err := Check(context.Background(), h, phonePK); err != nil {
		t.Fatalf("Check: %v", err)
	}
	// One connection, ids from a counter, the key as lowercase hex; Check
	// asks key only (see TestCheckAsksTheHelperOnce).
	wantReqs := []authproto.Request{
		{V: 1, ID: 1, Op: "owner"}, {V: 1, ID: 2, Op: "key", PK: phoneHex}, {V: 1, ID: 3, Op: "key", PK: phoneHex},
		{V: 1, ID: 4, Op: "key", PK: phoneHex},
	}
	if got := fh.requests(); !reflect.DeepEqual(got, wantReqs) {
		t.Fatalf("requests %+v\nwant %+v", got, wantReqs)
	}
	if a, e := fh.counts(); a != 1 || e != 0 {
		t.Fatalf("accepted %d, closed %d: want one persistent connection", a, e)
	}
	// Other roles pass through for Check to decide; a role 8 key is denied.
	guest := phoneRow()
	guest.Role, guest.FormFactor = 8, 7
	fh.mu.Lock()
	fh.rows[string(phonePK)] = guest
	fh.mu.Unlock()
	if role, err := h.KeyRole(ctxT(t), phonePK); err != nil || role != 8 {
		t.Fatalf("role 8: %d %v", role, err)
	}
	if Check(context.Background(), h, phonePK) == nil {
		t.Fatal("Check accepted role 8")
	}
}

// Check asks the helper once: op key runs the owner check before it reads
// the key's row, on the unique name it then asks (camhelper/authproto), so a
// separate owner request only doubled the round trips (and the time a
// concurrent check waits). A key answer refusing on the owner check denies.
func TestCheckAsksTheHelperOnce(t *testing.T) {
	fh := newFakeHelper(t)
	h, _, _ := newTestHelper(t, fh.sock, nil)
	if err := Check(context.Background(), h, phonePK); err != nil {
		t.Fatalf("Check: %v", err)
	}
	if got, want := fh.requests(), []authproto.Request{{V: 1, ID: 1, Op: "key", PK: phoneHex}}; !reflect.DeepEqual(got, want) {
		t.Fatalf("requests %+v, want %+v", got, want)
	}
	for _, code := range []string{authproto.ErrAuth, authproto.ErrAuthOwner} {
		fh.setRespond(func(fc *fakeConn, req authproto.Request) {
			fc.resp(authproto.Response{ID: req.ID, Err: code,
				Msg: "authority: com.tesla.Auth is not owned by authd: owned by uid 0, authd's uid is 403"})
		})
		if err := Check(context.Background(), h, phonePK); err == nil || !strings.Contains(err.Error(), "not owned by authd") {
			t.Fatalf("%s: Check %v, want the owner check's denial", code, err)
		}
	}
}

// Each error code maps to its class; the connection stays up for answers
// the helper keeps serving after, and is closed after the others.
func TestHelperErrorCodes(t *testing.T) {
	cases := []struct {
		code  string
		want  error
		keeps bool
	}{
		{authproto.ErrAuthOwner, ErrAuthOwner, true},
		{authproto.ErrKeyUnknown, ErrKeyUnknown, true},
		{authproto.ErrKeyAmbig, ErrKeyAmbiguous, true},
		{authproto.ErrShape, ErrShape, true},
		{authproto.ErrBus, ErrBus, true},
		{authproto.ErrAuthdChange, ErrAuthdChanged, true},
		{authproto.ErrRateLimited, ErrHelper, true},
		{authproto.ErrTimeout, ErrHelper, true},
		{authproto.ErrAuth, ErrHelper, true},
		{authproto.ErrBadRequest, ErrHelper, false},
		{authproto.ErrInternal, ErrHelper, false},
		{"made_up", ErrHelper, false},
	}
	for _, c := range cases {
		t.Run(c.code, func(t *testing.T) {
			fh := newFakeHelper(t)
			h, clk, _ := newTestHelper(t, fh.sock, nil)
			fh.setRespond(func(fc *fakeConn, req authproto.Request) {
				fc.resp(authproto.Response{ID: req.ID, Err: c.code, Msg: "authority: the helper's text"})
			})
			_, err := h.KeyRole(ctxT(t), phonePK)
			wantHelperClass(t, "KeyRole", err, c.want)
			var he *HelperError
			if !errors.As(err, &he) || he.Code != c.code || he.Msg != "authority: the helper's text" || he.Op != authproto.OpKey {
				t.Fatalf("KeyRole: %#v, want a HelperError for %s", err, c.code)
			}
			if _, err := h.KeyEntry(ctxT(t), phonePK); !errors.Is(err, c.want) {
				t.Fatalf("KeyEntry: %v", err)
			}
			clk.advance(time.Second)
			ok, err := h.AuthOwnerIsAuthd(ctxT(t))
			if c.code == authproto.ErrAuthOwner {
				if ok || err != nil {
					t.Fatalf("owner: %v %v, want false and no error", ok, err)
				}
			} else {
				if ok {
					t.Fatal("owner: ok after an error")
				}
				wantHelperClass(t, "owner", err, c.want)
			}
			clk.advance(time.Second)
			wantHelperClass(t, "Check", Check(context.Background(), h, phonePK), c.want)
			if accepted, _ := fh.counts(); c.keeps != (accepted == 1) {
				t.Fatalf("%d connections for 4 answers, want the connection kept=%v", accepted, c.keeps)
			}
		})
	}
}

// okResp is a well-formed success for req.
func okResp(req authproto.Request) authproto.Response {
	r := authproto.Response{ID: req.ID, OK: true}
	if req.Op == authproto.OpKey {
		r.Role, r.FF, r.KID = 2, 6, "cce6a2d2"
	}
	return r
}

// Anything the helper must not send denies and closes the connection; the
// next request (once the re-dial limit allows) uses a new one.
func TestHelperViolationsCloseTheConnection(t *testing.T) {
	edit := func(f func(*authproto.Response)) func(fc *fakeConn, req authproto.Request) {
		return func(fc *fakeConn, req authproto.Request) {
			r := okResp(req)
			f(&r)
			fc.resp(r)
		}
	}
	raw := func(format string) func(fc *fakeConn, req authproto.Request) {
		return func(fc *fakeConn, req authproto.Request) {
			fc.frame(authproto.TResp, []byte(strings.ReplaceAll(format, "ID", fmt.Sprint(req.ID))))
		}
	}
	big := func(fc *fakeConn, req authproto.Request) {
		r := authproto.Response{ID: req.ID, Err: authproto.ErrInternal}
		b, _ := json.Marshal(r)
		r.Msg = strings.Repeat("x", authproto.MaxResp+1-len(b)-len(`,"msg":""`))
		b, _ = json.Marshal(r)
		if len(b) != authproto.MaxResp+1 {
			t.Errorf("oversized RESP is %d bytes", len(b))
		}
		fc.frame(authproto.TResp, b)
	}
	cases := []struct {
		name    string
		op      string
		respond func(fc *fakeConn, req authproto.Request)
	}{
		{"kid mismatch", "key", edit(func(r *authproto.Response) { r.KID = "cce6a2d3" })},
		{"kid upper case", "key", edit(func(r *authproto.Response) { r.KID = "CCE6A2D2" })},
		{"kid missing", "key", edit(func(r *authproto.Response) { r.KID = "" })},
		{"secs 5", "key", edit(func(r *authproto.Response) { r.Secs = 5 })},
		{"role 12", "key", edit(func(r *authproto.Response) { r.Role = 12 })},
		{"id mismatch", "key", edit(func(r *authproto.Response) { r.ID++ })},
		{"id mismatch (owner)", "owner", edit(func(r *authproto.Response) { r.ID-- })},
		{"oversized RESP", "key", big},
		{"unknown field", "key", raw(`{"id":ID,"ok":true,"role":2,"ff":6,"kid":"cce6a2d2","x":1}`)},
		{"duplicate field", "owner", raw(`{"id":ID,"ok":true,"ok":true}`)},
		{"upper-case field", "owner", raw(`{"ID":ID,"ok":true}`)},
		{"null field", "owner", raw(`{"id":ID,"ok":true,"msg":null}`)},
		{"no ok", "owner", raw(`{"id":ID}`)},
		{"no id", "owner", raw(`{"ok":true}`)},
		{"string id", "owner", raw(`{"id":"ID","ok":true}`)},
		{"negative role", "key", raw(`{"id":ID,"ok":true,"role":-2,"ff":6,"kid":"cce6a2d2"}`)},
		{"float role", "key", raw(`{"id":ID,"ok":true,"role":2.0,"ff":6,"kid":"cce6a2d2"}`)},
		{"two objects", "owner", raw(`{"id":ID,"ok":true}{}`)},
		{"an array", "owner", raw(`[{"id":ID,"ok":true}]`)},
		{"not JSON", "owner", raw(`ok`)},
		{"unknown frame type", "owner", func(fc *fakeConn, req authproto.Request) { fc.frame(0x83, []byte("au")) }},
		{"EVENT on a request connection", "owner", func(fc *fakeConn, req authproto.Request) {
			fc.event(authproto.Event{Ev: authproto.EvKeysChanged, Reason: authproto.ReasonKeysUpdated})
		}},
		{"ok and a code", "owner", edit(func(r *authproto.Response) { r.Err = authproto.ErrInternal })},
		{"no ok and no code", "owner", edit(func(r *authproto.Response) { r.OK = false })},
		{"a code that is not [a-z_]", "key", raw(`{"id":ID,"ok":false,"err":"key unknown\n"}`)},
		{"an error with a role", "key", raw(`{"id":ID,"ok":false,"err":"key_unknown","role":2}`)},
		{"owner ok with a role", "owner", edit(func(r *authproto.Response) { r.Role = 2 })},
		{"owner ok with a kid", "owner", edit(func(r *authproto.Response) { r.KID = "cce6a2d2" })},
		{"ok with a msg", "key", edit(func(r *authproto.Response) { r.Msg = "fine" })},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			fh := newFakeHelper(t)
			h, clk, _ := newTestHelper(t, fh.sock, nil)
			fh.setRespond(c.respond)
			var err error
			if c.op == "key" {
				_, err = h.KeyEntry(ctxT(t), phonePK)
			} else {
				var ok bool
				if ok, err = h.AuthOwnerIsAuthd(ctxT(t)); ok {
					t.Fatal("owner ok")
				}
			}
			wantHelperClass(t, c.name, err, ErrHelper)
			var he *HelperError
			if errors.As(err, &he) {
				t.Fatalf("a violation reported as the helper's refusal %q", he.Code)
			}
			fh.waitEOFs(1)
			fh.setRespond(nil)
			clk.advance(time.Second)
			if _, err := h.KeyEntry(ctxT(t), phonePK); err != nil {
				t.Fatalf("after the violation: %v", err)
			}
			if a, _ := fh.counts(); a != 2 {
				t.Fatalf("%d connections, want a new one after the violation", a)
			}
		})
	}
}

// A RESP with no request in flight (here a second one for one REQ) closes
// the connection at once, so it can never answer the next request.
func TestHelperUnsolicitedFrameClosesTheConnection(t *testing.T) {
	fh := newFakeHelper(t)
	h, clk, logs := newTestHelper(t, fh.sock, nil)
	fh.setRespond(func(fc *fakeConn, req authproto.Request) {
		fc.resp(okResp(req))
		fc.resp(okResp(req))
	})
	if ok, err := h.AuthOwnerIsAuthd(ctxT(t)); err != nil || !ok {
		t.Fatalf("first answer: %v %v", ok, err)
	}
	fh.waitEOFs(1)
	if !strings.Contains(logs.String(), "no request in flight") {
		t.Fatalf("log: %q", logs.String())
	}
	fh.setRespond(nil)
	clk.advance(time.Second)
	if ok, err := h.AuthOwnerIsAuthd(ctxT(t)); err != nil || !ok {
		t.Fatalf("on a new connection: %v %v", ok, err)
	}
}

// The helper's end must be HelperUID (root on the car): anything else is
// closed before a request is sent.
func TestHelperPeerUID(t *testing.T) {
	fh := newFakeHelper(t)
	h, _, logs := newTestHelper(t, fh.sock, func(c *HelperConfig) { c.HelperUID = os.Getuid() + 1 })
	ok, err := h.AuthOwnerIsAuthd(ctxT(t))
	if ok {
		t.Fatal("owner ok from a helper of the wrong uid")
	}
	wantHelperClass(t, "wrong uid", err, ErrHelper)
	if !strings.Contains(err.Error(), fmt.Sprintf("uid %d", os.Getuid())) {
		t.Fatalf("err %q does not name the helper's uid", err)
	}
	fh.waitEOFs(1)
	if n := len(fh.requests()); n != 0 {
		t.Fatalf("%d requests sent to a helper of the wrong uid", n)
	}
	if !strings.Contains(logs.String(), "REFUSING") {
		t.Fatalf("log: %q", logs.String())
	}
	// No peer credentials: the same.
	fh2 := newFakeHelper(t)
	h2, _, _ := newTestHelper(t, fh2.sock, func(c *HelperConfig) {
		c.PeerUID = func(net.Conn) (int, error) { return -1, errors.New("no credentials") }
	})
	_, err = h2.KeyRole(ctxT(t), phonePK)
	wantHelperClass(t, "no credentials", err, ErrHelper)
	fh2.waitEOFs(1)
	// The real peer credentials of the test's own helper match its uid.
	fh3 := newFakeHelper(t)
	h3, _, _ := newTestHelper(t, fh3.sock, nil)
	if ok, err := h3.AuthOwnerIsAuthd(ctxT(t)); err != nil || !ok {
		t.Fatalf("the test's own uid: %v %v", ok, err)
	}
}

// An answer later than 400 ms denies within the budget and closes the
// connection; a shorter caller deadline wins.
func TestHelperDeadline(t *testing.T) {
	fh := newFakeHelper(t)
	h, _, _ := newTestHelper(t, fh.sock, nil)
	fh.setRespond(func(fc *fakeConn, req authproto.Request) {
		time.Sleep(600 * time.Millisecond)
		fc.resp(okResp(req))
	})
	start := time.Now()
	_, err := h.KeyRole(context.Background(), phonePK)
	d := time.Since(start)
	wantHelperClass(t, "slow helper", err, ErrHelper)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err %v, want a deadline", err)
	}
	if d < 350*time.Millisecond || d > 550*time.Millisecond {
		t.Fatalf("denied after %v, want about 400 ms", d)
	}
	fh.waitEOFs(1)

	fh2 := newFakeHelper(t)
	h2, _, _ := newTestHelper(t, fh2.sock, nil)
	fh2.setRespond(fh.respond)
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	start = time.Now()
	if _, err := h2.AuthOwnerIsAuthd(ctx); err == nil {
		t.Fatal("slow owner accepted")
	}
	if d := time.Since(start); d > 300*time.Millisecond {
		t.Fatalf("denied after %v, want the caller's 100 ms", d)
	}
}

// Nothing is dialled until a check; then at most once per RedialEvery.
func TestHelperLazyDial(t *testing.T) {
	sock := filepath.Join(shortDir(t), "a.sock")
	h, clk, _ := newTestHelper(t, sock, nil)
	_, err := h.KeyRole(ctxT(t), phonePK)
	wantHelperClass(t, "no socket", err, ErrHelper)
	fh := newFakeHelperAt(t, sock)
	_, err = h.KeyRole(ctxT(t), phonePK)
	wantHelperClass(t, "within a second", err, ErrHelper)
	if a, _ := fh.counts(); a != 0 {
		t.Fatalf("re-dialled within a second (%d connections)", a)
	}
	clk.advance(999 * time.Millisecond)
	if _, err := h.KeyRole(ctxT(t), phonePK); err == nil {
		t.Fatal("re-dialled after 999 ms")
	}
	clk.advance(time.Millisecond)
	if role, err := h.KeyRole(ctxT(t), phonePK); err != nil || role != 2 {
		t.Fatalf("after a second: %d %v", role, err)
	}
}

// A caller out of time (a sign-in past its pre-auth deadline) touches
// nothing: it spends no dial and sends nothing on, or closes, a live
// connection, so the next caller is served at once. Repeated, as acquire's
// select picks at random between the free lock and the done context.
func TestHelperExpiredCallerTouchesNothing(t *testing.T) {
	for i := 0; i < 20; i++ {
		fh := newFakeHelper(t)
		h, _, _ := newTestHelper(t, fh.sock, nil) // the fake clock never advances
		dead, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Millisecond))
		_, err := h.KeyRole(dead, phonePK)
		wantHelperClass(t, "expired, not connected", err, ErrHelper)
		if a, _ := fh.counts(); a != 0 {
			t.Fatalf("run %d: an expired caller dialled", i)
		}
		if role, err := h.KeyRole(ctxT(t), phonePK); err != nil || role != 2 {
			t.Fatalf("run %d: the next caller after an expired one: %d %v", i, role, err)
		}
		_, err = h.KeyRole(dead, phonePK)
		wantHelperClass(t, "expired, connected", err, ErrHelper)
		cancel()
		if n := len(fh.requests()); n != 1 {
			t.Fatalf("run %d: %d requests, want only the live caller's", i, n)
		}
		if role, err := h.KeyRole(ctxT(t), phonePK); err != nil || role != 2 {
			t.Fatalf("run %d: the connection after an expired caller: %d %v", i, role, err)
		}
		if a, e := fh.counts(); a != 1 || e != 0 {
			t.Fatalf("run %d: accepted %d, closed %d: want the one connection kept", i, a, e)
		}
	}
}

// A caller with too little time left to get an answer does not dial: the one
// dial per RedialEvery is kept for a caller that can use it.
func TestHelperShortCallerDoesNotDial(t *testing.T) {
	fh := newFakeHelper(t)
	h, _, _ := newTestHelper(t, fh.sock, nil) // the fake clock never advances
	ctx, cancel := context.WithTimeout(context.Background(), minDialTime/2)
	defer cancel()
	_, err := h.KeyRole(ctx, phonePK)
	wantHelperClass(t, "too little time to dial", err, ErrHelper)
	if a, _ := fh.counts(); a != 0 {
		t.Fatal("dialled with too little time left")
	}
	if role, err := h.KeyRole(ctxT(t), phonePK); err != nil || role != 2 {
		t.Fatalf("the next caller: %d %v", role, err)
	}
}

// When the helper restarts, the client notices the closed connection without
// sending on it, and the next check dials a new one.
func TestHelperReconnectsAfterRestart(t *testing.T) {
	fh := newFakeHelper(t)
	h, clk, _ := newTestHelper(t, fh.sock, nil)
	if _, err := h.KeyRole(ctxT(t), phonePK); err != nil {
		t.Fatal(err)
	}
	fh.restart()
	waitUntil(t, "the client to notice the restart", func() bool { return !h.connectedForTest() })
	clk.advance(time.Second)
	if role, err := h.KeyRole(ctxT(t), phonePK); err != nil || role != 2 {
		t.Fatalf("after the restart: %d %v", role, err)
	}
	if a, _ := fh.counts(); a != 2 {
		t.Fatalf("%d connections, want 2", a)
	}
}

// Concurrent checks share the one connection, one request at a time.
func TestHelperSerializesRequests(t *testing.T) {
	fh := newFakeHelper(t)
	h, _, _ := newTestHelper(t, fh.sock, nil)
	var wg sync.WaitGroup
	errs := make(chan error, 64)
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 4; j++ {
				if j%2 == 0 {
					_, err := h.KeyRole(context.Background(), phonePK)
					errs <- err
				} else {
					_, err := h.AuthOwnerIsAuthd(context.Background())
					errs <- err
				}
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Error(err)
		}
	}
	reqs := fh.requests()
	for i, r := range reqs {
		if r.ID != uint32(i+1) {
			t.Fatalf("request %d has id %d", i, r.ID)
		}
	}
	if a, _ := fh.counts(); a != 1 || len(reqs) != 64 {
		t.Fatalf("%d connections, %d requests", a, len(reqs))
	}
}

// A malformed key is refused before anything is sent.
func TestHelperBadKeyNotSent(t *testing.T) {
	fh := newFakeHelper(t)
	h, _, _ := newTestHelper(t, fh.sock, nil)
	for _, k := range [][]byte{nil, phonePK[1:], append([]byte{5}, phonePK[1:]...), append(append([]byte{}, phonePK...), 0)} {
		_, err := h.KeyEntry(ctxT(t), k)
		wantHelperClass(t, fmt.Sprintf("%d-byte key", len(k)), err, ErrShape)
	}
	if a, _ := fh.counts(); a != 0 {
		t.Fatalf("dialled for a malformed key (%d connections)", a)
	}
}

func TestHelperClose(t *testing.T) {
	fh := newFakeHelper(t)
	h, clk, _ := newTestHelper(t, fh.sock, nil)
	if _, err := h.KeyRole(ctxT(t), phonePK); err != nil {
		t.Fatal(err)
	}
	h.Close()
	fh.waitEOFs(1)
	clk.advance(time.Hour)
	_, err := h.KeyRole(ctxT(t), phonePK)
	wantHelperClass(t, "closed", err, ErrHelper)
	if a, _ := fh.counts(); a != 1 {
		t.Fatalf("dialled after Close (%d connections)", a)
	}
}

// Neither the log nor an error ever holds the full key.
func TestHelperNeverLogsTheKey(t *testing.T) {
	fh := newFakeHelper(t)
	h, clk, logs := newTestHelper(t, fh.sock, nil)
	fh.setRespond(func(fc *fakeConn, req authproto.Request) {
		r := okResp(req)
		r.KID = "00000000"
		fc.resp(r)
	})
	_, err := h.KeyEntry(ctxT(t), phonePK)
	if err == nil {
		t.Fatal("kid mismatch accepted")
	}
	fh.setRespond(func(fc *fakeConn, req authproto.Request) {
		fc.resp(authproto.Response{ID: req.ID, Err: authproto.ErrKeyUnknown})
	})
	clk.advance(time.Second)
	_, err2 := h.KeyEntry(ctxT(t), phonePK)
	out := logs.String() + err.Error() + err2.Error()
	for i := 0; i+16 <= len(phoneHex); i += 8 {
		if strings.Contains(out, phoneHex[i:i+16]) {
			t.Fatalf("key bytes %s in the log or an error:\n%s", phoneHex[i:i+16], out)
		}
	}
	if !strings.Contains(logs.String(), "kid=cce6a2d2") {
		t.Fatalf("the violation's log line should name the kid:\n%s", logs.String())
	}
}

// watchLog records what Watch reports, as "up", "down" or "keys:<reason>".
type watchLog struct {
	mu  sync.Mutex
	got []string
}

func (w *watchLog) handlers() WatchHandlers {
	return WatchHandlers{
		KeysChanged: func(reason string) { w.add("keys:" + reason) },
		State: func(up bool) {
			if up {
				w.add("up")
			} else {
				w.add("down")
			}
		},
	}
}

func (w *watchLog) add(s string) {
	w.mu.Lock()
	w.got = append(w.got, s)
	w.mu.Unlock()
}

func (w *watchLog) all() []string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return append([]string(nil), w.got...)
}

// expect waits for the entries after the first n to start with want, and
// returns the new n.
func (w *watchLog) expect(t *testing.T, n int, want ...string) int {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for {
		got := w.all()[n:]
		if len(got) >= len(want) {
			if !reflect.DeepEqual(got[:len(want)], want) {
				t.Fatalf("watch reported %q, want %q first", got, want)
			}
			return n + len(want)
		}
		if time.Now().After(deadline) {
			t.Fatalf("watch reported %q, want %q", got, want)
		}
		time.Sleep(2 * time.Millisecond)
	}
}

// skipDowns returns n moved past any "down" entries (failed reconnects).
func (w *watchLog) skipDowns(n int) int {
	got := w.all()
	for n < len(got) && got[n] == "down" {
		n++
	}
	return n
}

// startWatch runs h.Watch until the test ends (or stop is called).
func startWatch(t *testing.T, h *Helper) (*watchLog, func()) {
	wl := &watchLog{}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { h.Watch(ctx, wl.handlers()); close(done) }()
	var once sync.Once
	stop := func() {
		once.Do(func() {
			cancel()
			select {
			case <-done:
			case <-time.After(2 * time.Second):
				t.Error("Watch did not return after its context ended")
			}
		})
	}
	t.Cleanup(stop)
	return wl, stop
}

func TestHelperWatch(t *testing.T) {
	fh := newFakeHelper(t)
	h, _, _ := newTestHelper(t, fh.sock, nil)
	wl, stop := startWatch(t, h)
	n := wl.expect(t, 0, "down", "keys:resubscribed") // the helper's subscription is not up yet
	fh.notify(authproto.Event{Ev: authproto.EvWatchUp})
	fh.notify(authproto.Event{Ev: authproto.EvKeysChanged, Reason: authproto.ReasonKeysUpdated})
	fh.notify(authproto.Event{Ev: authproto.EvKeysChanged, Reason: authproto.ReasonOwnerChanged})
	fh.notify(authproto.Event{Ev: authproto.EvWatchDown})
	n = wl.expect(t, n, "up", "keys:keys_updated", "keys:owner_changed", "down")
	fh.notify(authproto.Event{Ev: authproto.EvWatchUp})
	n = wl.expect(t, n, "up")
	// The watch is on its own connection, never the request one.
	if _, err := h.KeyRole(ctxT(t), phonePK); err != nil {
		t.Fatal(err)
	}
	if a, _ := fh.counts(); a != 2 || fh.watchers() != 1 {
		t.Fatalf("%d connections, %d watchers", a, fh.watchers())
	}
	// The helper restarts: the lost connection is "down", then the watch
	// comes back by itself: the state, and keys_changed (changes may have
	// been missed).
	fh.restart()
	n = wl.expect(t, n, "down")
	n = wl.skipDowns(n)
	n = wl.expect(t, n, "up", "keys:resubscribed")
	stop()
	after := len(wl.all())
	fh.notify(authproto.Event{Ev: authproto.EvKeysChanged, Reason: authproto.ReasonKeysUpdated})
	time.Sleep(50 * time.Millisecond)
	if got := wl.all(); len(got) != after {
		t.Fatalf("reported after Watch returned: %q", got[after:])
	}
	fh.waitEOFs(1)
	_ = n
}

// Anything unexpected on the watch connection drops it ("down") and the
// watch attaches again.
func TestHelperWatchStrict(t *testing.T) {
	bigEvent := `{"ev":"keys_changed","reason":"` + strings.Repeat("x", authproto.MaxEvent) + `"}`
	cases := []struct {
		name string
		t    byte
		body string
	}{
		{"unknown event", authproto.TEvent, `{"ev":"keys_gone"}`},
		{"unknown field", authproto.TEvent, `{"ev":"keys_changed","reason":"keys_updated","x":1}`},
		{"unknown reason", authproto.TEvent, `{"ev":"keys_changed","reason":"maybe"}`},
		{"keys_changed without a reason", authproto.TEvent, `{"ev":"keys_changed"}`},
		{"watch_up with a reason", authproto.TEvent, `{"ev":"watch_up","reason":"keys_updated"}`},
		{"null event", authproto.TEvent, `{"ev":null}`},
		{"duplicate field", authproto.TEvent, `{"ev":"watch_up","ev":"watch_down"}`},
		{"not JSON", authproto.TEvent, `watch_up`},
		{"oversized event", authproto.TEvent, bigEvent},
		{"a RESP after the attach", authproto.TResp, `{"id":1,"ok":true}`},
		{"unknown frame type", 0x83, `au`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			fh := newFakeHelper(t)
			h, _, logs := newTestHelper(t, fh.sock, nil)
			wl, _ := startWatch(t, h)
			n := wl.expect(t, 0, "down", "keys:resubscribed")
			fh.watchFrame(c.t, []byte(c.body))
			n = wl.expect(t, n, "down")
			fh.waitEOFs(1)
			n = wl.skipDowns(n)
			wl.expect(t, n, "down", "keys:resubscribed")
			if a, _ := fh.counts(); a < 2 {
				t.Fatalf("%d connections: the watch did not attach again", a)
			}
			if !strings.Contains(logs.String(), "watch") {
				t.Fatalf("log: %q", logs.String())
			}
		})
	}
}

// A refused or malformed attach, or a helper of the wrong uid, is "down" and
// retried with backoff; the watch never reports up.
func TestHelperWatchAttachFailures(t *testing.T) {
	cases := []struct {
		name   string
		attach func(fc *fakeConn, req authproto.Request)
		uid    int
	}{
		{"refused", func(fc *fakeConn, req authproto.Request) {
			fc.resp(authproto.Response{ID: req.ID, Err: authproto.ErrInternal})
		}, 0},
		{"wrong id", func(fc *fakeConn, req authproto.Request) {
			fc.resp(authproto.Response{ID: req.ID + 1, OK: true})
		}, 0},
		{"ok with a role", func(fc *fakeConn, req authproto.Request) {
			fc.resp(authproto.Response{ID: req.ID, OK: true, Role: 2})
		}, 0},
		{"an EVENT first", func(fc *fakeConn, req authproto.Request) {
			fc.event(authproto.Event{Ev: authproto.EvWatchUp})
		}, 0},
		{"no answer", func(fc *fakeConn, req authproto.Request) {}, 0},
		{"wrong uid", nil, 1},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			fh := newFakeHelper(t)
			fh.setAttach(c.attach)
			fh.mu.Lock()
			fh.up = true
			fh.mu.Unlock()
			h, _, _ := newTestHelper(t, fh.sock, func(cfg *HelperConfig) {
				cfg.HelperUID += c.uid
				cfg.Budget = 100 * time.Millisecond
			})
			wl, stop := startWatch(t, h)
			waitUntil(t, "two attempts", func() bool { a, _ := fh.counts(); return a >= 2 })
			stop()
			for _, s := range wl.all() {
				if s != "down" {
					t.Fatalf("reported %q", wl.all())
				}
			}
			if len(wl.all()) == 0 {
				t.Fatal("a failed attach must report down")
			}
		})
	}
}

// Watch stops at once when its context ends, also while it backs off.
func TestHelperWatchStopsDuringBackoff(t *testing.T) {
	sock := filepath.Join(shortDir(t), "none.sock")
	h, _, _ := newTestHelper(t, sock, func(c *HelperConfig) { c.MinBackoff, c.MaxBackoff = time.Hour, time.Hour })
	wl, stop := startWatch(t, h)
	wl.expect(t, 0, "down")
	start := time.Now()
	stop()
	if d := time.Since(start); d > time.Second {
		t.Fatalf("Watch took %v to stop", d)
	}
}
