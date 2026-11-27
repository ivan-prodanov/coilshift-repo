package client_test

import (
	"context"
	"crypto/ecdh"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"encoding/json"
	"net"
	"testing"
	"time"

	"airgapp/gateway/capability"
	"airgapp/gateway/client"
	"airgapp/gateway/internal/authority"
	"airgapp/gateway/internal/proto"
	"airgapp/gateway/internal/server"
)

// tickProbe is a TickHz-20 test capability with no inbound types: the core
// issues TICKs for it and the phone's Recv has to take them in.
type tickProbe struct{}

func (tickProbe) Spec() capability.Spec {
	return capability.Spec{Name: "TICKPROBE", ID: proto.CapSummon, TickHz: 20}
}
func (tickProbe) PreArm(context.Context, []byte) error { return nil }
func (tickProbe) Open(_ context.Context, w capability.Window, _ json.RawMessage) (capability.Handler, error) {
	return probeH{}, nil
}

type probeH struct{}

func (probeH) Frame(byte, []byte) error { return nil }
func (probeH) Teardown(string)          {}

func dialProbe(t *testing.T) *client.Session {
	t.Helper()
	gw, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	dev, _ := ecdh.P256().GenerateKey(rand.Reader)
	srv := server.New(server.Config{Key: gw, VIN: vin,
		Oracle: authority.Fake{Role: 2, Owner: true}, Caps: []capability.Capability{tickProbe{}}})
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go srv.Serve(ln)
	t.Cleanup(func() { srv.Shutdown("sigterm") })
	s, err := client.Dial(context.Background(), ln.Addr().String(), proto.CapSummon, nil, &gw.PublicKey, vin, dev)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

// Recv takes every TICK in (never returns one) and NewestTick reports a
// fresh one.
func TestTicksConsumed(t *testing.T) {
	s := dialProbe(t)
	if _, _, ok := s.NewestTick(); ok {
		t.Fatal("tick before any Recv")
	}
	types := make(chan byte, 64)
	go func() {
		for {
			typ, _, err := s.Recv()
			if err != nil {
				close(types)
				return
			}
			types <- typ
		}
	}()
	deadline := time.Now().Add(2 * time.Second)
	for {
		if _, age, ok := s.NewestTick(); ok {
			if age >= client.TickMaxAge {
				t.Fatalf("first tick already %v old", age)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("no tick taken in by Recv")
		}
		time.Sleep(5 * time.Millisecond)
	}
	timeout := time.After(300 * time.Millisecond)
	for {
		select {
		case typ, ok := <-types:
			if !ok {
				t.Fatal("session ended")
			}
			if typ == proto.TypeTick {
				t.Fatal("Recv returned a TICK")
			}
		case <-timeout:
			return
		}
	}
}
