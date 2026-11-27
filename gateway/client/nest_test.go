package client

import (
	"bytes"
	"context"
	"crypto/ecdh"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"encoding/json"
	"net"
	"runtime"
	"testing"
	"time"

	"airgapp/gateway/capability"
	"airgapp/gateway/internal/authority"
	"airgapp/gateway/internal/proto"
	"airgapp/gateway/internal/server"
)

const (
	nestVIN  = "TESTVIN0000000000"
	outerIn  = 0x10
	outerOut = 0x90
	innerIn  = 0x20 // inner capability inbound type, limit 16384
	innerOut = 0x21 // inner capability outbound type, limit 40000
)

// relayCap is the outer capability: it pipes 0x10 frames into a TCP
// connection to the inner server and sends what comes back as 0x90 frames.
type relayCap struct{ innerAddr string }

func (relayCap) Spec() capability.Spec {
	return capability.Spec{Name: "RELAY", ID: proto.CapSummon,
		In: map[byte]int{outerIn: 16384}, Out: map[byte]int{outerOut: 16384}}
}
func (relayCap) PreArm(context.Context, []byte) error { return nil }
func (r relayCap) Open(ctx context.Context, w capability.Window, _ json.RawMessage) (capability.Handler, error) {
	var d net.Dialer
	conn, err := d.DialContext(ctx, "tcp", r.innerAddr)
	if err != nil {
		return nil, err
	}
	go func() {
		buf := make([]byte, 16384)
		for {
			n, err := conn.Read(buf)
			if n > 0 {
				if w.Send(outerOut, append([]byte(nil), buf[:n]...)) != nil {
					break
				}
			}
			if err != nil {
				break
			}
		}
		w.Close("closed")
	}()
	return &relayH{conn: conn}, nil
}

type relayH struct{ conn net.Conn }

func (h *relayH) Frame(t byte, body []byte) error {
	if t != outerIn {
		return nil
	}
	_, err := h.conn.Write(body)
	return err
}
func (h *relayH) Teardown(string) { h.conn.Close() }

// innerCap is the inner capability (TickHz 20) that records inbound frames
// and exposes its window so a test can send from it.
type innerCap struct {
	got chan []byte
	win chan capability.Window
}

func (innerCap) Spec() capability.Spec {
	return capability.Spec{Name: "INNER", ID: proto.CapSummon, TickHz: 20,
		In: map[byte]int{innerIn: 16384}, Out: map[byte]int{innerOut: 40000}}
}
func (innerCap) PreArm(context.Context, []byte) error { return nil }
func (c innerCap) Open(_ context.Context, w capability.Window, _ json.RawMessage) (capability.Handler, error) {
	c.win <- w
	return innerH{c.got}, nil
}

type innerH struct{ got chan []byte }

func (h innerH) Frame(t byte, body []byte) error {
	if t == innerIn {
		h.got <- append([]byte(nil), body...)
	}
	return nil
}
func (innerH) Teardown(string) {}

type nestRig struct {
	outerGW, innerGW *ecdsa.PrivateKey
	dev              *ecdh.PrivateKey
	outerAddr        string
	inner            innerCap
	outerSrv         *server.Server
}

func newServer(t *testing.T, gw *ecdsa.PrivateKey, c capability.Capability) (*server.Server, string) {
	t.Helper()
	srv := server.New(server.Config{Key: gw, VIN: nestVIN,
		Oracle: authority.Fake{Role: 2, Owner: true}, Caps: []capability.Capability{c}})
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go srv.Serve(ln)
	t.Cleanup(func() { srv.Shutdown("sigterm") })
	return srv, ln.Addr().String()
}

func newNestRig(t *testing.T) *nestRig {
	t.Helper()
	r := &nestRig{}
	r.outerGW, _ = ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	r.innerGW, _ = ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	r.dev, _ = ecdh.P256().GenerateKey(rand.Reader)
	r.inner = innerCap{got: make(chan []byte, 4), win: make(chan capability.Window, 1)}
	_, innerAddr := newServer(t, r.innerGW, r.inner)
	r.outerSrv, r.outerAddr = newServer(t, r.outerGW, relayCap{innerAddr})
	return r
}

func (r *nestRig) dialOuter(t *testing.T) *Session {
	t.Helper()
	s, err := Dial(context.Background(), r.outerAddr, proto.CapSummon, nil, &r.outerGW.PublicKey, nestVIN, r.dev)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestHandshakeOverConn(t *testing.T) {
	r := newNestRig(t)
	conn, err := net.Dial("tcp", r.outerAddr)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	s, err := Handshake(context.Background(), conn, proto.CapSummon, nil, &r.outerGW.PublicKey, nestVIN, r.dev)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err := s.Send(outerIn, []byte("x")); err != nil {
		t.Fatal(err)
	}
}

func TestInnerConnNestedSession(t *testing.T) {
	r := newNestRig(t)
	outer := r.dialOuter(t)
	conn := InnerConn(outer, outerIn, outerOut)
	defer conn.Close()
	inner, err := Handshake(context.Background(), conn, proto.CapSummon, nil, &r.innerGW.PublicKey, nestVIN, r.dev)
	if err != nil {
		t.Fatal(err)
	}
	defer inner.Close()

	type msg struct {
		t    byte
		body []byte
	}
	recvd := make(chan msg, 4)
	go func() {
		for {
			typ, body, err := inner.Recv()
			if err != nil {
				return
			}
			recvd <- msg{typ, body}
		}
	}()

	deadline := time.Now().Add(500 * time.Millisecond)
	for {
		if _, age, ok := inner.NewestTick(); ok {
			if age >= TickMaxAge {
				t.Fatalf("tick already %v old", age)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("no inner tick within 500 ms")
		}
		time.Sleep(5 * time.Millisecond)
	}

	// phone -> inner: a body that seals to ~16405 bytes spans two 0x10 frames.
	want := make([]byte, 16384)
	rand.Read(want)
	if err := inner.Send(innerIn, want); err != nil {
		t.Fatal(err)
	}
	select {
	case got := <-r.inner.got:
		if !bytes.Equal(got, want) {
			t.Fatal("16384-byte body arrived altered")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("16384-byte body never arrived")
	}

	// inner -> phone: 40000 bytes, bigger than one outer frame.
	w := <-r.inner.win
	big := make([]byte, 40000)
	rand.Read(big)
	if err := w.Send(innerOut, append([]byte(nil), big...)); err != nil {
		t.Fatal(err)
	}
	select {
	case m := <-recvd:
		if m.t != innerOut || !bytes.Equal(m.body, big) {
			t.Fatalf("inner->phone: type 0x%02x, %d bytes, want whole 40000", m.t, len(m.body))
		}
	case <-time.After(2 * time.Second):
		t.Fatal("40000-byte body never arrived")
	}
}

func TestInnerConnCloseClosesOuter(t *testing.T) {
	r := newNestRig(t)
	outer := r.dialOuter(t)
	conn := InnerConn(outer, outerIn, outerOut)
	if err := conn.Close(); err != nil {
		t.Fatal(err)
	}
	if err := outer.Send(outerIn, []byte("x")); err == nil {
		t.Fatal("outer still open after conn.Close")
	}
	if _, _, err := outer.Recv(); err == nil {
		t.Fatal("outer Recv succeeded after close")
	}
	if _, err := conn.Write([]byte("x")); err == nil {
		t.Fatal("write on closed conn succeeded")
	}
	// both pump goroutines (client/nest.go) must be gone
	deadline := time.Now().Add(2 * time.Second)
	for {
		buf := make([]byte, 1<<20)
		buf = buf[:runtime.Stack(buf, true)]
		if !bytes.Contains(buf, []byte("client.InnerConn.func")) {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("InnerConn pump goroutines leaked:\n%s", buf)
		}
		time.Sleep(10 * time.Millisecond)
	}
}
