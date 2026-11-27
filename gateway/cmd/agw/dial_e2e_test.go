package main

import (
	"context"
	"fmt"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"airgapp/gateway/capability/echo"
	"airgapp/gateway/client"
	"airgapp/gateway/internal/proto"
)

// TestDialThroughRealPi signs a phone in through the real Pi forwarder (the
// rpi repo's services.GatewayForward, which its cmd/gwforward-lab runs on this
// machine), with serve -dial on the car's side. It is skipped unless
// AGW_E2E_PI_PHONE and AGW_E2E_PI_CAR name the lab's phone and car ports:
//
//	cd rpi && go run ./cmd/gwforward-lab -phone 127.0.0.1:27100 -car 127.0.0.1:27101 -car-addr 127.0.0.1
//	AGW_E2E_PI_PHONE=127.0.0.1:27100 AGW_E2E_PI_CAR=127.0.0.1:27101 go test -race -run TestDialThroughRealPi -v ./cmd/agw/
//
// AGW_E2E_PI_IDLE, off by default, first leaves the car's connections idle
// for that long. At 30s, say, that is past the 25 s silence limit, so they
// last only if each end's K reaches the other.
func TestDialThroughRealPi(t *testing.T) {
	phone, car := os.Getenv("AGW_E2E_PI_PHONE"), os.Getenv("AGW_E2E_PI_CAR")
	if phone == "" && car == "" {
		if s := os.Getenv("AGW_E2E_PI_IDLE"); s != "" {
			t.Fatalf("AGW_E2E_PI_IDLE=%q needs AGW_E2E_PI_PHONE and AGW_E2E_PI_CAR too", s)
		}
		t.Skip("needs a running gwforward-lab (rpi repo): set AGW_E2E_PI_PHONE and AGW_E2E_PI_CAR to its phone and car addresses")
	}
	if phone == "" || car == "" {
		t.Fatalf("AGW_E2E_PI_PHONE=%q, AGW_E2E_PI_CAR=%q: set both, or neither to skip", phone, car)
	}
	var idle time.Duration
	if s := os.Getenv("AGW_E2E_PI_IDLE"); s != "" {
		var err error
		if idle, err = time.ParseDuration(s); err != nil || idle <= 0 {
			t.Fatalf("AGW_E2E_PI_IDLE=%q: want a duration such as 30s", s)
		}
	}
	// serve -bench dials only loopback, so the lab runs on this machine: the
	// phone dials it on loopback too, never another host.
	if ap, err := netip.ParseAddrPort(phone); err != nil || !ap.Addr().Unmap().IsLoopback() {
		t.Fatalf("AGW_E2E_PI_PHONE=%q: want a loopback ip:port (the lab runs on this machine)", phone)
	}
	ph := setupPhone(t)

	_, _, errb, _ := startServe(t, "-bench", "-fake-role", "2", "-dial", car,
		"-key", ph.keyPath, "-vin-file", ph.vinPath, "-log", filepath.Join(ph.dir, "log"))
	waitLogCount(t, errb, "agw: dialing the Pi at ", 1)

	if idle > 0 {
		// Only K crosses an idle connection. Either end closes it after 25 s
		// with nothing heard, and agw logs the loss.
		t.Logf("leaving the car's connections idle for %v", idle)
		time.Sleep(idle)
		if strings.Contains(errb.String(), "ended before pairing") {
			t.Fatalf("a connection to the Pi ended while idle; agw log:\n%s", errb.String())
		}
	}

	// More sessions than the car's 3 idle connections, so at least two ride
	// connections it redialled. 250 ms apart: the server admits at most 5
	// hellos in any second (helloBurst in internal/server).
	const sessions = 5
	for i := 1; i <= sessions; i++ {
		if i > 1 {
			time.Sleep(250 * time.Millisecond)
		}
		start := time.Now()
		ctx, cancel := context.WithDeadline(context.Background(), start.Add(2*time.Second))
		s, err := client.Dial(ctx, phone, proto.CapEcho, nil, ph.pub, ph.vin, ph.dev)
		if err != nil {
			cancel()
			t.Fatalf("session %d: %v\nagw log:\n%s", i, err, errb.String())
		}
		err = echoPing(ctx, s)
		cancel()
		s.Close()
		if err != nil {
			t.Fatalf("session %d: %v\nagw log:\n%s", i, err, errb.String())
		}
		t.Logf("session %d: %q echoed through the Pi in %v", i, "ping", time.Since(start).Round(time.Millisecond))
		// One window per capability: the next session starts once this one's
		// CLOSE has crossed the Pi and the gateway has freed the slot.
		waitLogCount(t, errb, `agw: ECHO closed`, i)
	}
	if n := strings.Count(errb.String(), `reason="close"`); n != sessions {
		t.Fatalf("%d windows closed by the phone's CLOSE, want %d:\n%s", n, sessions, errb.String())
	}
	if strings.Contains(errb.String(), testVIN) {
		t.Fatal("VIN leaked to the log")
	}
}

// echoPing sends "ping" on s and waits, until ctx ends, for its echo.
func echoPing(ctx context.Context, s *client.Session) error {
	got := make(chan error, 1)
	go func() {
		typ, body, err := s.Recv()
		if err == nil && (typ != echo.TypeOut || string(body) != "ping") {
			err = fmt.Errorf("got type 0x%02x %q, want the echo of \"ping\"", typ, body)
		}
		got <- err
	}()
	if err := s.Send(echo.TypeIn, []byte("ping")); err != nil {
		return err
	}
	select {
	case err := <-got:
		return err
	case <-ctx.Done():
		s.Close() // ends the Recv
		<-got
		return fmt.Errorf("no echo: %w", ctx.Err())
	}
}

// waitLogCount waits up to 2 s for want to appear n times in the log.
func waitLogCount(t *testing.T, logs *syncBuf, want string, n int) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for strings.Count(logs.String(), want) < n {
		if time.Now().After(deadline) {
			t.Fatalf("waiting for %q (%d times); agw log:\n%s", want, n, logs.String())
		}
		time.Sleep(5 * time.Millisecond)
	}
}
