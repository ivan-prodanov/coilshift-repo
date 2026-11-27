package server

import (
	"bytes"
	"crypto/ecdh"
	"testing"
	"time"

	"airgapp/gateway/capability/echo"
	"airgapp/gateway/internal/proto"
	"airgapp/gateway/internal/wire"
)

// sendHelloV2 writes a version 2 hello.
func (cl *client) sendHelloV2(cap byte, dev *ecdh.PrivateKey) {
	cl.t.Helper()
	h := proto.Hello{Version: proto.HelloV2, Cap: cap}
	copy(h.PkS[:], dev.PublicKey().Bytes())
	cl.cap, cl.hello = cap, h.Bytes()
	if err := wire.WriteFrame(cl.c, cl.hello); err != nil {
		cl.t.Fatal(err)
	}
}

// readChallengeV2 reads a v2 challenge, verifies it with the key it carries,
// checks that key is the gateway's, and keeps the exact bytes for the ARM.
func (cl *client) readChallengeV2() {
	cl.t.Helper()
	cl.c.SetReadDeadline(time.Now().Add(5 * time.Second))
	b, err := wire.ReadFrame(cl.c, 1024)
	if err != nil {
		cl.t.Fatalf("read challenge: %v", err)
	}
	if len(b) != proto.ChallengeV2Len {
		cl.t.Fatalf("a v2 hello got a %d-byte challenge, want %d", len(b), proto.ChallengeV2Len)
	}
	ch, err := proto.ParseChallengeV2(b)
	if err != nil {
		cl.t.Fatal(err)
	}
	gk := proto.VerifyChallengeV2(cl.hello, ch)
	if gk == nil || !gk.Equal(&cl.e.gw.PublicKey) {
		cl.t.Fatal("the v2 challenge does not carry, or is not signed by, the gateway's long-term key")
	}
	if ch.Binding != proto.Binding(testVIN) {
		cl.t.Fatal("challenge binding mismatch")
	}
	cl.chB = b
	cl.ch = proto.Challenge{N: ch.N, PkE: ch.PkE, ExpiryMS: ch.ExpiryMS, Binding: ch.Binding}
}

// A v2 hello gets a v2 challenge carrying the gateway's key, and the sign-in
// completes with the HPKE info over the exact v2 bytes sent.
func TestHelloV2SignsIn(t *testing.T) {
	e := newEnv(t)
	cl := e.dial()
	cl.sendHelloV2(echo.ID, e.dev)
	cl.readChallengeV2()
	cl.writeRaw(cl.armFrame(e.dev, cl.ch.N[:], echo.ID, cl.ch.ExpiryMS))
	cl.expectReady()
	cl.echo([]byte("v2"))
}

// The ARM binds the v2 bytes: an info computed over the v1 encoding of the
// same fields does not open, and the connection closes with no message.
func TestHelloV2ArmBindsV2Bytes(t *testing.T) {
	e := newEnv(t)
	cl := e.dial()
	cl.sendHelloV2(echo.ID, e.dev)
	cl.readChallengeV2()
	v1 := cl.ch.Bytes() // the v1 encoding, with a zero signature
	if bytes.Equal(v1, cl.chB) {
		t.Fatal("test setup: the encodings are equal")
	}
	cl.chB = v1
	cl.writeRaw(cl.armFrame(e.dev, cl.ch.N[:], echo.ID, cl.ch.ExpiryMS))
	cl.expectClosedNoBytes()
}

// A v1 hello still gets the v1 challenge, byte for byte (185 bytes).
func TestHelloV1Unchanged(t *testing.T) {
	e := newEnv(t)
	cl := e.dial()
	if err := cl.sendHello(echo.ID, e.dev); err != nil {
		t.Fatal(err)
	}
	cl.readChallenge()
	if len(cl.chB) != proto.ChallengeLen {
		t.Fatalf("v1 challenge %d bytes", len(cl.chB))
	}
}

// An unapproved key's v2 hello gets no byte: neither the VIN binding nor the
// gateway's key leaves the car.
func TestHelloV2UnapprovedGetsNothing(t *testing.T) {
	e := newEnv(t, func(e *env) { e.oracle.Role = 0 })
	cl := e.dial()
	h := proto.Hello{Version: proto.HelloV2, Cap: echo.ID}
	copy(h.PkS[:], e.dev.PublicKey().Bytes())
	wire.WriteFrame(cl.c, h.Bytes()) // the close may come first
	cl.expectClosedNoBytes()
}
