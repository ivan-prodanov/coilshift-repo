package client_test

import (
	"context"
	"crypto/ecdh"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"net"
	"strings"
	"testing"
	"time"

	"airgapp/gateway/capability"
	"airgapp/gateway/capability/echo"
	"airgapp/gateway/client"
	"airgapp/gateway/internal/authority"
	"airgapp/gateway/internal/keys"
	"airgapp/gateway/internal/proto"
	"airgapp/gateway/internal/server"
)

const vin = "TESTVIN0000000000"

type rig struct {
	srv  *server.Server
	addr string
	gw   *ecdsa.PrivateKey
	dev  *ecdh.PrivateKey
}

func newRig(t *testing.T) *rig {
	t.Helper()
	gw, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	dev, _ := ecdh.P256().GenerateKey(rand.Reader)
	srv := server.New(server.Config{Key: gw, VIN: vin,
		Oracle: authority.Fake{Role: 2, Owner: true}, Caps: []capability.Capability{echo.New()}})
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go srv.Serve(ln)
	t.Cleanup(func() { srv.Shutdown("sigterm") })
	return &rig{srv, ln.Addr().String(), gw, dev}
}

func TestDialEchoClose(t *testing.T) {
	r := newRig(t)
	s, err := client.Dial(context.Background(), r.addr, proto.CapEcho, map[string]any{}, &r.gw.PublicKey, vin, r.dev)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Send(echo.TypeIn, []byte("hi")); err != nil {
		t.Fatal(err)
	}
	typ, body, err := s.Recv()
	if err != nil || typ != echo.TypeOut || string(body) != "hi" {
		t.Fatalf("recv %x %q %v", typ, body, err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.Recv(); err == nil {
		t.Fatal("Recv after Close succeeded")
	}
}

func TestDialRejectsWrongPinAndVIN(t *testing.T) {
	r := newRig(t)
	other, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if _, err := client.Dial(context.Background(), r.addr, proto.CapEcho, nil, &other.PublicKey, vin, r.dev); err == nil {
		t.Fatal("wrong pinned key accepted")
	}
	if _, err := client.Dial(context.Background(), r.addr, proto.CapEcho, nil, &r.gw.PublicKey, "OTHERVIN000000000", r.dev); err == nil {
		t.Fatal("wrong VIN accepted")
	}
}

func TestRecvReportsByeReason(t *testing.T) {
	r := newRig(t)
	s, err := client.Dial(context.Background(), r.addr, proto.CapEcho, nil, &r.gw.PublicKey, vin, r.dev)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	go r.srv.Shutdown("sigterm")
	done := make(chan error, 1)
	go func() { _, _, err := s.Recv(); done <- err }()
	select {
	case err := <-done:
		if err == nil || !strings.Contains(err.Error(), "sigterm") {
			t.Fatalf("want BYE sigterm, got %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("no BYE")
	}
}

func TestParsePin(t *testing.T) {
	r := newRig(t)
	pin := keys.PinString(&r.gw.PublicKey, vin)
	pub, v, err := client.ParsePin(pin)
	if err != nil || v != vin || !pub.Equal(&r.gw.PublicKey) {
		t.Fatalf("ParsePin: %v", err)
	}
	if _, _, err := client.ParsePin("garbage"); err == nil {
		t.Fatal("garbage pin accepted")
	}
}

func TestHeartbeatsKeepSessionHealthy(t *testing.T) {
	defer client.SetHeartbeatEvery(20 * time.Millisecond)()
	r := newRig(t)
	s, err := client.Dial(context.Background(), r.addr, proto.CapEcho, nil, &r.gw.PublicKey, vin, r.dev)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	time.Sleep(300 * time.Millisecond) // ~15 beats interleaved with sends
	for i := 0; i < 3; i++ {
		s.Send(echo.TypeIn, []byte("x"))
		if _, _, err := s.Recv(); err != nil {
			t.Fatal(err)
		}
	}
}
