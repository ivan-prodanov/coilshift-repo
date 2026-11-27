package authority_test

import (
	"bytes"
	"context"
	"encoding/hex"
	"errors"
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
	"airgapp/gateway/internal/authority"
	"airgapp/gateway/internal/authsrv"
)

// The phone key the car listed (public): Id cce6a2d2, role 2, form factor 6.
const phoneHex = "04e70805d8e2d25f6337d3ba01c18d7247b38afc0330ee85b21bdf490a85eb667ff5acda7cd5af3d21a90a12d3e3c656d512a598da3a8e6494bf66ea8a9c69a0a9"

// authdRows stands in for authd behind the real helper server: a fixed
// KeyInfo list, matched by the gateway core's own RoleFor.
type authdRows struct{ rows []authority.KeyInfoEntry }

func (authdRows) AuthOwnerIsAuthd(context.Context) (bool, error) { return true, nil }

func (a authdRows) KeyEntry(_ context.Context, pk []byte) (authority.KeyInfoEntry, error) {
	if _, _, err := authority.RoleFor(a.rows, pk); err != nil {
		return authority.KeyInfoEntry{}, err
	}
	for _, r := range a.rows {
		if bytes.Equal(r.PublicKey, pk) {
			return r, nil
		}
	}
	return authority.KeyInfoEntry{}, errors.New("unreachable")
}

type watchLog struct {
	mu  sync.Mutex
	got []string
}

func (w *watchLog) add(s string) {
	w.mu.Lock()
	w.got = append(w.got, s)
	w.mu.Unlock()
}

func (w *watchLog) expect(t *testing.T, n int, want ...string) int {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for {
		w.mu.Lock()
		got := append([]string(nil), w.got[n:]...)
		w.mu.Unlock()
		if len(got) >= len(want) {
			if !reflect.DeepEqual(got[:len(want)], want) {
				t.Fatalf("watch reported %q, want %q", got, want)
			}
			return n + len(want)
		}
		if time.Now().After(deadline) {
			t.Fatalf("watch reported %q, want %q", got, want)
		}
		time.Sleep(2 * time.Millisecond)
	}
}

// The gateway's client against the real sign-in server (internal/authsrv, as
// cmd/camhelper builds it), with a fake authd and this test's uid as the
// gateway uid: owner, key (role 2 passes Check; an unlisted key is denied)
// and a keys_changed notice through a watch.
func TestHelperAgainstTheRealSignInServer(t *testing.T) {
	dir, err := os.MkdirTemp("", "ae") // short: unix socket paths are limited
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	sock := filepath.Join(dir, "auth.sock")
	phone, _ := hex.DecodeString(phoneHex)
	other := append([]byte{4}, bytes.Repeat([]byte{0x11}, 64)...)
	var logs bytes.Buffer
	var logMu sync.Mutex
	srv := authsrv.New(authsrv.Config{
		Oracle:   authdRows{rows: []authority.KeyInfoEntry{{ID: authority.KeyID(phone), PublicKey: phone, Role: 2, FormFactor: 6}}},
		AllowUID: os.Getuid(),
		Log:      log.New(writerFunc(func(p []byte) (int, error) { logMu.Lock(); defer logMu.Unlock(); return logs.Write(p) }), "", 0),
	})
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	served := make(chan error, 1)
	go func() { served <- srv.Serve(ln) }()
	t.Cleanup(func() {
		srv.Shutdown()
		if err := <-served; err != nil {
			t.Errorf("Serve: %v", err)
		}
	})

	h := authority.NewHelper(sock, authority.HelperConfig{HelperUID: os.Getuid()})
	defer h.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if ok, err := h.AuthOwnerIsAuthd(ctx); err != nil || !ok {
		t.Fatalf("owner: %v %v", ok, err)
	}
	if err := authority.Check(ctx, h, phone); err != nil {
		t.Fatalf("Check (role 2): %v", err)
	}
	e, err := h.KeyEntry(ctx, phone)
	want := authority.KeyInfoEntry{ID: authority.KeyID(phone), PublicKey: phone, Role: 2, FormFactor: 6}
	if err != nil || !reflect.DeepEqual(e, want) {
		t.Fatalf("KeyEntry %+v %v", e, err)
	}
	// An unlisted key: denied, with the helper's reason, key_unknown.
	_, err = h.KeyRole(ctx, other)
	var he *authority.HelperError
	if !errors.As(err, &he) || he.Code != authproto.ErrKeyUnknown || !errors.Is(err, authority.ErrKeyUnknown) ||
		!strings.Contains(err.Error(), "not in authd's KeyInfo list") {
		t.Fatalf("unlisted key: %v", err)
	}
	if authority.Check(ctx, h, other) == nil {
		t.Fatal("Check accepted an unlisted key")
	}
	// The listed key still passes on the same connection.
	if err := authority.Check(ctx, h, phone); err != nil {
		t.Fatalf("Check after a denial: %v", err)
	}

	wl := &watchLog{}
	wctx, stop := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		h.Watch(wctx, authority.WatchHandlers{
			KeysChanged: func(r string) { wl.add("keys:" + r) },
			State: func(up bool) {
				if up {
					wl.add("up")
				} else {
					wl.add("down")
				}
			},
		})
		close(done)
	}()
	// The helper's own subscription is not up yet: its state, then
	// keys_changed resubscribed on attach.
	n := wl.expect(t, 0, "down", "keys:"+authproto.ReasonResubscribed)
	srv.Notify(authproto.Event{Ev: authproto.EvWatchUp})
	srv.Notify(authproto.Event{Ev: authproto.EvKeysChanged, Reason: authproto.ReasonKeysUpdated})
	wl.expect(t, n, "up", "keys:"+authproto.ReasonKeysUpdated)
	stop()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Watch did not return")
	}

	logMu.Lock()
	defer logMu.Unlock()
	if strings.Contains(logs.String(), phoneHex[2:40]) || !strings.Contains(logs.String(), "kid=cce6a2d2") {
		t.Fatalf("helper log must name keys by kid only:\n%s", logs.String())
	}
}

// slowRows is authdRows taking d per call: a busy bus or authd.
type slowRows struct {
	authdRows
	d time.Duration
}

func (s slowRows) AuthOwnerIsAuthd(ctx context.Context) (bool, error) {
	time.Sleep(s.d)
	return true, nil
}

func (s slowRows) KeyEntry(ctx context.Context, pk []byte) (authority.KeyInfoEntry, error) {
	time.Sleep(s.d)
	return s.authdRows.KeyEntry(ctx, pk)
}

// Two Checks at once (two sign-ins, or a sign-in and a re-check sharing the
// request connection) with each helper answer taking 150 ms: both pass (2
// answers, one at a time). With an owner request before each key request
// the later one missed Check's 500 ms (4 answers).
func TestHelperConcurrentChecksAgainstASlowHelper(t *testing.T) {
	dir, err := os.MkdirTemp("", "ae")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	sock := filepath.Join(dir, "auth.sock")
	phone, _ := hex.DecodeString(phoneHex)
	rows := authdRows{rows: []authority.KeyInfoEntry{{ID: authority.KeyID(phone), PublicKey: phone, Role: 2, FormFactor: 6}}}
	srv := authsrv.New(authsrv.Config{Oracle: slowRows{rows, 150 * time.Millisecond}, AllowUID: os.Getuid()})
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	go srv.Serve(ln)
	t.Cleanup(srv.Shutdown)
	h := authority.NewHelper(sock, authority.HelperConfig{HelperUID: os.Getuid()})
	defer h.Close()
	if err := authority.Check(context.Background(), h, phone); err != nil { // connects
		t.Fatalf("alone: %v", err)
	}
	errs := make(chan error, 2)
	for i := 0; i < 2; i++ {
		go func() { errs <- authority.Check(context.Background(), h, phone) }()
	}
	for i := 0; i < 2; i++ {
		if err := <-errs; err != nil {
			t.Errorf("concurrent Check of a listed key: %v", err)
		}
	}
}

type writerFunc func([]byte) (int, error)

func (f writerFunc) Write(p []byte) (int, error) { return f(p) }
