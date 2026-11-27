package server

import (
	"context"
	"crypto/ecdh"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"airgapp/gateway/capability"
	"airgapp/gateway/capability/echo"
)

// Access is the car's own: a key signs in when it proves possession (the
// handshake) and authd lists it as OWNER or DRIVER. There is no list of the
// gateway's own.

// keyOracle answers each key's role from roles (0 for a key authd does not
// list), with com.tesla.Auth owned by authd. It counts KeyRole calls.
type keyOracle struct {
	mu    sync.Mutex
	roles map[string]int32
	asked atomic.Int32
}

func newKeyOracle() *keyOracle { return &keyOracle{roles: map[string]int32{}} }

func (o *keyOracle) set(k *ecdh.PrivateKey, role int32) {
	o.mu.Lock()
	o.roles[string(k.PublicKey().Bytes())] = role
	o.mu.Unlock()
}

func (o *keyOracle) KeyRole(_ context.Context, pk []byte) (int32, error) {
	o.asked.Add(1)
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.roles[string(pk)], nil
}

func (o *keyOracle) AuthOwnerIsAuthd(context.Context) (bool, error) { return true, nil }

// openAs is env.open with key k.
func (e *env) openAs(k *ecdh.PrivateKey) *client {
	e.t.Helper()
	cl := e.dial()
	if err := cl.sendHello(echo.ID, k); err != nil {
		e.t.Fatal(err)
	}
	cl.readChallenge()
	cl.writeRaw(cl.armFrame(k, cl.ch.N[:], echo.ID, cl.ch.ExpiryMS))
	cl.expectReady()
	return cl
}

// refusedAtHello sends a hello as k and expects the connection closed with no
// byte at all: no challenge, so no VIN binding and no gateway key to take away.
func (e *env) refusedAtHello(k *ecdh.PrivateKey) {
	e.t.Helper()
	cl := e.dial()
	cl.sendHello(echo.ID, k) // the close may come first: a write error is fine
	cl.expectClosedNoBytes()
}

// waitLog waits until the std log holds want.
func waitLog(t *testing.T, logs *stdLog, want string) {
	t.Helper()
	waitFor(t, 2*time.Second, "log line "+want, func() bool { return strings.Contains(logs.String(), want) })
}

// A key that was never written anywhere signs in when authd lists it as
// OWNER or DRIVER.
func TestOwnerOrDriverKeySignsInUnlisted(t *testing.T) {
	for _, role := range []int32{2, 3} {
		o := newKeyOracle()
		e := newEnv(t, func(e *env) { e.oracleImpl = o })
		k := newECDH(t) // a phone key the gateway has never seen
		o.set(k, role)
		cl := e.openAs(k)
		cl.echo([]byte("in"))
		e.close()
	}
}

// A key authd does not list as OWNER or DRIVER is refused at its hello, before
// any challenge (which carries the VIN binding), and so before PreArm and Open;
// it never learns whether the window is open: the same silent close whether
// the slot is free or held.
func TestUnknownRoleRefusedBeforeSlotState(t *testing.T) {
	for _, role := range []int32{0, 1, 8} {
		logs := captureLog(t)
		o := newKeyOracle()
		hc := newHookCap()
		var preArms atomic.Int32
		hc.preArm = func(context.Context) error { preArms.Add(1); return nil }
		e := newEnv(t, func(e *env) { e.oracleImpl = o; e.caps = []capability.Capability{hc} })
		stranger := newECDH(t)
		o.set(stranger, role)

		// The slot free.
		e.refusedAtHello(stranger)
		waitLog(t, logs, "role "+strconv.Itoa(int(role))+" not permitted")
		if n := hc.calls.Load(); n != 0 || preArms.Load() != 0 {
			t.Fatalf("role %d: PreArm %d, Open %d calls, want none", role, preArms.Load(), n)
		}

		// The slot held by the owner.
		owner := newECDH(t)
		o.set(owner, 2)
		cl := e.openAs(owner)
		pre := preArms.Load()
		e.refusedAtHello(stranger)
		waitFor(t, 2*time.Second, "the second refusal", func() bool {
			return strings.Count(logs.String(), "role "+strconv.Itoa(int(role))+" not permitted") == 2
		})
		if strings.Contains(logs.String(), "already open") {
			t.Fatalf("role %d: the refusal tells the slot's state:\n%s", role, logs)
		}
		if preArms.Load() != pre || hc.calls.Load() != 1 {
			t.Fatalf("role %d: an unknown key reached PreArm or Open", role)
		}
		cl.echo([]byte("unaffected"))
		e.close()
	}
}

// The hello-time check is a filter; the check after the ARM is the gate: a
// key whose role drops between its hello and its ARM is refused after the ARM,
// before PreArm, and is not told the slot's state.
func TestRoleDropAfterHelloRefusedAtArm(t *testing.T) {
	logs := captureLog(t)
	o := newKeyOracle()
	hc := newHookCap()
	var preArms atomic.Int32
	hc.preArm = func(context.Context) error { preArms.Add(1); return nil }
	e := newEnv(t, func(e *env) { e.oracleImpl = o; e.caps = []capability.Capability{hc} })
	k := newECDH(t)
	o.set(k, 2)
	cl := e.dial()
	if err := cl.sendHello(echo.ID, k); err != nil {
		t.Fatal(err)
	}
	cl.readChallenge()
	if n := o.asked.Load(); n != 1 {
		t.Fatalf("authd asked %d times before the challenge, want once (the hello filter)", n)
	}
	o.set(k, 0)
	cl.writeRaw(cl.armFrame(k, cl.ch.N[:], echo.ID, cl.ch.ExpiryMS))
	cl.expectClosedNoBytes()
	waitLog(t, logs, "role 0 not permitted")
	if o.asked.Load() != 2 || preArms.Load() != 0 || hc.calls.Load() != 0 {
		t.Fatalf("asked %d, PreArm %d, Open %d: want the ARM's check to refuse before PreArm",
			o.asked.Load(), preArms.Load(), hc.calls.Load())
	}
}

// An OWNER key that finds the slot held is refused after the authority check
// and before PreArm; the window it found is untouched.
func TestBusySlotRefusedAfterAuthority(t *testing.T) {
	logs := captureLog(t)
	o := newKeyOracle()
	hc := newHookCap()
	var preArms atomic.Int32
	hc.preArm = func(context.Context) error { preArms.Add(1); return nil }
	e := newEnv(t, func(e *env) { e.oracleImpl = o; e.caps = []capability.Capability{hc} })
	a, b := newECDH(t), newECDH(t)
	o.set(a, 2)
	o.set(b, 3)
	cl := e.openAs(a)
	asked := o.asked.Load()
	cl2 := e.dial()
	if err := cl2.sendHello(echo.ID, b); err != nil {
		t.Fatal(err)
	}
	cl2.readChallenge()
	cl2.writeRaw(cl2.armFrame(b, cl2.ch.N[:], echo.ID, cl2.ch.ExpiryMS))
	cl2.expectClosedNoBytes()
	waitLog(t, logs, "ECHO window already open")
	if o.asked.Load() == asked {
		t.Fatal("the busy slot was told before the authority check")
	}
	if preArms.Load() != 1 {
		t.Fatalf("PreArm ran %d times, want only the first window's", preArms.Load())
	}
	cl.echo([]byte("unaffected"))
}
