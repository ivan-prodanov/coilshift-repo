package server

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"testing"
	"time"

	"airgapp/gateway/capability"
	"airgapp/gateway/capability/echo"
	"airgapp/gateway/internal/authority"
	"airgapp/gateway/internal/keys"
	"airgapp/gateway/internal/proto"
)

// tickCap is a minimal capability with a settable ID and TickHz. Opened
// windows are published on win; teardown, if set, runs in Handler.Teardown.
type tickCap struct {
	id       byte
	name     string
	hz       int
	win      chan capability.Window
	teardown func()
}

func newTickCap(id byte, name string, hz int) *tickCap {
	return &tickCap{id: id, name: name, hz: hz, win: make(chan capability.Window, 4)}
}

func (c *tickCap) Spec() capability.Spec {
	return capability.Spec{
		Name: c.name, ID: c.id, TickHz: c.hz,
		In:  map[byte]int{0x10: 64},
		Out: map[byte]int{0x90: 64},
	}
}

func (c *tickCap) PreArm(context.Context, []byte) error { return nil }

func (c *tickCap) Open(_ context.Context, w capability.Window, _ json.RawMessage) (capability.Handler, error) {
	c.win <- w
	return tickHandler{c}, nil
}

type tickHandler struct{ c *tickCap }

func (tickHandler) Frame(byte, []byte) error { return nil }
func (h tickHandler) Teardown(string) {
	if h.c.teardown != nil {
		h.c.teardown()
	}
}

// recvTick reads the next frame, which must be a TICK, and returns its id
// and issued BOOTTIME ms.
func (cl *client) recvTick() ([8]byte, uint64) {
	cl.t.Helper()
	typ, body := cl.recv()
	if typ != proto.TypeTick {
		cl.t.Fatalf("want TICK, got type 0x%02x %q", typ, body)
	}
	if len(body) != 16 {
		cl.t.Fatalf("TICK body %d bytes, want 16", len(body))
	}
	var id [8]byte
	copy(id[:], body[:8])
	return id, binary.BigEndian.Uint64(body[8:])
}

func TestTicksAtRate(t *testing.T) {
	tc := newTickCap(proto.CapSummon, "SUMMON", 20)
	e := newEnv(t, func(e *env) { e.caps = []capability.Capability{tc} })
	cl := e.open(proto.CapSummon)
	<-tc.win
	seen := map[[8]byte]bool{}
	n := 0
	end := time.Now().Add(time.Second)
	for time.Now().Before(end) {
		id, ms := cl.recvTick()
		if seen[id] {
			t.Fatalf("tick id %x repeated", id)
		}
		seen[id] = true
		if ms != e.clk.BootMS() {
			t.Fatalf("tick issued at %d, Fake clock says %d", ms, e.clk.BootMS())
		}
		n++
	}
	if n < 18 || n > 22 {
		t.Fatalf("%d TICKs in 1 s at TickHz 20", n)
	}
}

func TestNoTicksWhenZero(t *testing.T) {
	tc := newTickCap(proto.CapSummon, "SUMMON", 0)
	e := newEnv(t, func(e *env) { e.caps = []capability.Capability{tc} })
	cl := e.open(proto.CapSummon)
	w := <-tc.win
	time.Sleep(300 * time.Millisecond)
	if err := w.Send(0x90, []byte("status")); err != nil {
		t.Fatal(err)
	}
	if typ, body := cl.recv(); typ != 0x90 || string(body) != "status" {
		t.Fatalf("got type 0x%02x %q, want only the capability's frame", typ, body)
	}
	cl.send(proto.TypeClose, nil)
	cl.expectBye("close")
}

func TestTickAge(t *testing.T) {
	sc := newTickCap(proto.CapSummon, "SUMMON", 20)
	dc := newTickCap(proto.CapDashcam, "OTHER", 20)
	e := newEnv(t, func(e *env) { e.caps = []capability.Capability{sc, dc} })
	cs := e.open(proto.CapSummon)
	ws := <-sc.win
	co := e.open(proto.CapDashcam)
	wo := <-dc.win

	id, _ := cs.recvTick()
	if age, ok := ws.TickAge(id); !ok || age != 0 {
		t.Fatalf("fresh tick: age %v ok %v", age, ok)
	}
	e.clk.Advance(37)
	if age, ok := ws.TickAge(id); !ok || age != 37*time.Millisecond {
		t.Fatalf("after 37 ms: age %v ok %v", age, ok)
	}
	if _, ok := ws.TickAge([8]byte{1, 2, 3, 4, 5, 6, 7, 8}); ok {
		t.Fatal("foreign id accepted")
	}
	if _, ok := wo.TickAge(id); ok {
		t.Fatal("a tick from another window accepted")
	}
	oid, _ := co.recvTick()
	if _, ok := ws.TickAge(oid); ok {
		t.Fatal("the other window's tick accepted by SUMMON")
	}
	if _, ok := wo.TickAge(oid); !ok {
		t.Fatal("the other window's own tick refused")
	}
}

func TestTickRingKeeps64(t *testing.T) {
	var r tickRing
	ids := make([][8]byte, 65)
	for i := range ids {
		ids[i][0], ids[i][1] = byte(i), 0xAA
		r.add(ids[i], uint64(1000+i))
	}
	if _, ok := r.issued(ids[0]); ok {
		t.Fatal("the 65th-newest tick is still retained")
	}
	for i := 1; i < 65; i++ {
		if ms, ok := r.issued(ids[i]); !ok || ms != uint64(1000+i) {
			t.Fatalf("tick %d: ms %d ok %v", i, ms, ok)
		}
	}
}

func TestTicksStopAtTeardown(t *testing.T) {
	tc := newTickCap(proto.CapSummon, "SUMMON", 50)
	e := newEnv(t, func(e *env) { e.caps = []capability.Capability{tc} })
	tc.teardown = func() { time.Sleep(150 * time.Millisecond) } // several tick periods
	cl := e.open(proto.CapSummon)
	w := <-tc.win
	cl.recvTick()
	w.Close("close")
	closedAt := e.clk.BootMS()
	e.clk.Advance(10_000) // any tick issued after Close carries a later ms
	for {
		typ, body := cl.recv()
		if typ == proto.TypeBye {
			if string(body) != "close" {
				t.Fatalf("BYE %q", body)
			}
			break
		}
		if typ != proto.TypeTick || len(body) != 16 {
			t.Fatalf("unexpected type 0x%02x", typ)
		}
		if ms := binary.BigEndian.Uint64(body[8:]); ms > closedAt {
			t.Fatalf("TICK issued after Close (ms %d > %d)", ms, closedAt)
		}
	}
	cl.expectEOF()
	if _, ok := w.TickAge([8]byte{}); ok {
		t.Fatal("zero id accepted")
	}
}

func TestHostSummonOpen(t *testing.T) {
	sc := newTickCap(proto.CapSummon, "SUMMON", 0)
	cc := newTickCap(proto.CapCamera, "CAMERA", 0)
	e := newEnv(t, func(e *env) { e.caps = []capability.Capability{sc, cc} })
	ccl := e.open(proto.CapCamera)
	wc := <-cc.win
	if wc.Host().SummonOpen() {
		t.Fatal("SummonOpen with only a CAMERA window")
	}
	scl := e.open(proto.CapSummon)
	ws := <-sc.win
	if !wc.Host().SummonOpen() || !ws.Host().SummonOpen() {
		t.Fatal("SummonOpen false while SUMMON holds its slot")
	}
	scl.send(proto.TypeClose, nil)
	scl.expectBye("close")
	waitFor(t, 2*time.Second, "SUMMON slot freed", func() bool { return !e.srv.slotOpen(proto.CapSummon) })
	if wc.Host().SummonOpen() {
		t.Fatal("SummonOpen after the SUMMON window freed its slot")
	}
	ccl.send(proto.TypeClose, nil)
	ccl.expectBye("close")
}

func TestSetLinkDegradedOnlySummon(t *testing.T) {
	sc := newTickCap(proto.CapSummon, "SUMMON", 0)
	cc := newTickCap(proto.CapCamera, "CAMERA", 0)
	e := newEnv(t, func(e *env) { e.caps = []capability.Capability{sc, cc} })
	e.open(proto.CapCamera)
	wc := <-cc.win

	wc.Host().SetSummonLinkDegraded(true) // no SUMMON window, and not SUMMON
	if wc.Host().SummonLinkDegraded() {
		t.Fatal("CAMERA set the degraded flag")
	}
	scl := e.open(proto.CapSummon)
	ws := <-sc.win
	wc.Host().SetSummonLinkDegraded(true) // SUMMON open, still not SUMMON
	if ws.Host().SummonLinkDegraded() {
		t.Fatal("CAMERA set the degraded flag while SUMMON is open")
	}
	ws.Host().SetSummonLinkDegraded(true)
	if !wc.Host().SummonLinkDegraded() {
		t.Fatal("SUMMON's degraded flag not visible to CAMERA")
	}
	ws.Host().SetSummonLinkDegraded(false)
	if wc.Host().SummonLinkDegraded() {
		t.Fatal("SUMMON could not clear the flag")
	}
	ws.Host().SetSummonLinkDegraded(true)
	scl.send(proto.TypeClose, nil)
	scl.expectBye("close")
	waitFor(t, 2*time.Second, "SUMMON slot freed", func() bool { return !e.srv.slotOpen(proto.CapSummon) })
	if wc.Host().SummonLinkDegraded() {
		t.Fatal("degraded not reset when the SUMMON window freed its slot")
	}
	ws.Host().SetSummonLinkDegraded(true) // a stale SUMMON window
	if wc.Host().SummonLinkDegraded() {
		t.Fatal("a freed SUMMON window set the flag")
	}
	e.open(proto.CapSummon)
	ws2 := <-sc.win
	if ws2.Host().SummonLinkDegraded() {
		t.Fatal("a new SUMMON window starts degraded")
	}
}

func TestWindowPublicKey(t *testing.T) {
	cc := newCountCap(echo.New(), 0, "")
	e := newEnv(t, func(e *env) { e.caps = []capability.Capability{cc} })
	e.open(echo.ID)
	w := <-cc.win
	want := e.dev.PublicKey().Bytes()
	pk := w.PublicKey()
	if len(pk) != 65 || !bytes.Equal(pk, want) {
		t.Fatalf("PublicKey %x", pk)
	}
	pk[0] ^= 0xFF
	if !bytes.Equal(w.PublicKey(), want) {
		t.Fatal("PublicKey returned the window's own slice")
	}
	if keys.Fingerprint(w.PublicKey()) != w.KeyFingerprint() {
		t.Fatal("PublicKey does not match KeyFingerprint")
	}
}

func TestTickHzValidation(t *testing.T) {
	cfg := func(hz int) Config {
		c := newTickCap(proto.CapSummon, "SUMMON", hz)
		return Config{Key: newECDSA(t), Oracle: authority.Fake{}, Caps: []capability.Capability{c}}
	}
	for _, hz := range []int{0, 1, 20, 50} {
		New(cfg(hz))
	}
	for _, hz := range []int{-1, 51, 1000} {
		func() {
			defer func() {
				if recover() == nil {
					t.Fatalf("New accepted TickHz %d", hz)
				}
			}()
			New(cfg(hz))
		}()
	}
}
