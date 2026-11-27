package authority

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

// pinABC is SHA-256("abc"): the pin of the test authd "binary" "abc".
const pinABC = "ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad"

const testPasswd = "root:x:0:0::/root:/bin/sh\nauthd:x:403:403:VCSEC:/:/bin/false\nairgapp-gw:x:4950:4950::/nonexistent:/bin/false\n"

// fakeBus is the system bus with authd on it. names maps well-known names to
// unique names, uids unique names to uids and keyInfo unique names to KeyInfo
// reply bodies. It records every call in order.
type fakeBus struct {
	mu       sync.Mutex
	names    map[string]string
	uids     map[string]uint32
	keyInfo  map[string][]interface{}
	afterUID func(b *fakeBus) // runs (mu held) after each GetConnectionUnixUser
	calls    []string
	dropped  bool
	closed   bool
}

// authdBus is a bus on which :1.42, uid 403, owns com.tesla.Auth and lists rows.
func authdBus(rows ...KeyInfoEntry) *fakeBus {
	return &fakeBus{
		names:   map[string]string{"com.tesla.Auth": ":1.42"},
		uids:    map[string]uint32{":1.42": 403},
		keyInfo: map[string][]interface{}{":1.42": keyInfoReply(rows...)},
	}
}

// keyInfoReply is rows as godbus decodes a(ayayuut).
func keyInfoReply(rows ...KeyInfoEntry) []interface{} {
	out := [][]interface{}{}
	for _, r := range rows {
		out = append(out, []interface{}{r.ID, r.PublicKey, r.Role, r.FormFactor, r.SecondsLeft})
	}
	return []interface{}{out}
}

func (b *fakeBus) NameOwner(ctx context.Context, name string) (string, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.calls = append(b.calls, "GetNameOwner "+name)
	if u, ok := b.names[name]; ok {
		return u, nil
	}
	return "", errors.New("org.freedesktop.DBus.Error.NameHasNoOwner")
}

func (b *fakeBus) UnixUser(ctx context.Context, name string) (uint32, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.calls = append(b.calls, "GetConnectionUnixUser "+name)
	uid, ok := b.uids[name]
	if b.afterUID != nil {
		b.afterUID(b)
	}
	if !ok {
		return 0, errors.New("org.freedesktop.DBus.Error.NameHasNoOwner")
	}
	return uid, nil
}

// KeyInfo reaches a unique name itself and a well-known name's current owner.
func (b *fakeBus) KeyInfo(ctx context.Context, dest string) ([]interface{}, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.calls = append(b.calls, "KeyInfo "+dest)
	if u, ok := b.names[dest]; ok {
		dest = u
	}
	if body, ok := b.keyInfo[dest]; ok {
		return body, nil
	}
	return nil, errors.New("org.freedesktop.DBus.Error.ServiceUnknown")
}

func (b *fakeBus) Connected() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return !b.dropped && !b.closed
}

func (b *fakeBus) Close() error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.closed = true
	return nil
}

func (b *fakeBus) drop() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.dropped = true
}

func (b *fakeBus) isClosed() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.closed
}

func (b *fakeBus) log() []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]string(nil), b.calls...)
}

type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *fakeClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fakeClock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

// rig is an authd oracle over a temp authd ("abc") and passwd, a fake clock
// and a dialler that hands out buses in order (the last one again when they
// run out).
type rig struct {
	dir   string
	o     *authdOracle
	clock *fakeClock

	mu      sync.Mutex
	dials   int
	buses   []*fakeBus
	dialErr error
	gate    chan struct{} // dial waits for it when set
	entered chan struct{} // dial signals it when set
}

func newRig(t *testing.T, buses ...*fakeBus) *rig {
	t.Helper()
	r := &rig{dir: t.TempDir(), clock: &fakeClock{t: time.Unix(1_000_000, 0)}, buses: buses}
	r.write(t, "authd", "abc")
	r.write(t, "passwd", testPasswd)
	o, err := newAuthdOracle(pinABC, r.path("authd"), r.path("passwd"), r.dial)
	if err != nil {
		t.Fatal(err)
	}
	o.now = r.clock.now
	r.o = o
	return r
}

func (r *rig) path(name string) string { return filepath.Join(r.dir, name) }

func (r *rig) write(t *testing.T, name, content string) {
	t.Helper()
	if err := os.WriteFile(r.path(name), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func (r *rig) dial() (authBus, error) {
	r.mu.Lock()
	r.dials++
	gate, entered, err := r.gate, r.entered, r.dialErr
	b := r.buses[0]
	if len(r.buses) > 1 {
		r.buses = r.buses[1:]
	}
	r.mu.Unlock()
	if entered != nil {
		select {
		case entered <- struct{}{}:
		default:
		}
	}
	if gate != nil {
		<-gate
	}
	if err != nil {
		return nil, err
	}
	return b, nil
}

func (r *rig) dialCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.dials
}

func (r *rig) setDialErr(err error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.dialErr = err
}

// ctxT is a generous budget for one check.
func ctxT(t *testing.T) context.Context {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	t.Cleanup(cancel)
	return ctx
}

func TestOracleAsksTheVerifiedUniqueName(t *testing.T) {
	bus := authdBus(otherRow(), phoneRow())
	r := newRig(t, bus)
	role, err := r.o.KeyRole(ctxT(t), phonePK)
	if err != nil || role != 2 {
		t.Fatalf("role=%d err=%v, want 2", role, err)
	}
	want := []string{"GetNameOwner com.tesla.Auth", "GetConnectionUnixUser :1.42", "KeyInfo :1.42"}
	if got := bus.log(); !reflect.DeepEqual(got, want) {
		t.Fatalf("calls %q\nwant  %q", got, want)
	}
	if err := Check(ctxT(t), r.o, phonePK); err != nil {
		t.Fatalf("Check: %v", err)
	}
}

func TestOracleNeverAsksANameHandedOverAfterTheCheck(t *testing.T) {
	guest := phoneRow()
	guest.Role = 8
	bus := authdBus(guest)
	// After the uid check authd exits, and a uid-4950 connection takes the
	// name and lists the phone as OWNER.
	bus.afterUID = func(b *fakeBus) {
		delete(b.uids, ":1.42")
		delete(b.keyInfo, ":1.42")
		b.names["com.tesla.Auth"] = ":1.99"
		b.uids[":1.99"] = 4950
		b.keyInfo[":1.99"] = keyInfoReply(phoneRow())
	}
	r := newRig(t, bus)
	if role, err := r.o.KeyRole(ctxT(t), phonePK); err == nil {
		t.Fatalf("role %d accepted from the name's new owner", role)
	}
	if got := bus.log(); got[len(got)-1] != "KeyInfo :1.42" {
		t.Fatalf("calls %q: KeyInfo must go to the unique name that passed the uid check", got)
	}
}

func TestOracleRefusesAnOwnerThatIsNotAuthd(t *testing.T) {
	cases := []struct {
		name   string
		uid    uint32 // owner's uid on the bus
		passwd string
	}{
		{"root owns the name", 0, testPasswd},
		{"the gateway owns the name", 4950, testPasswd},
		{"passwd names another authd uid", 403, strings.Replace(testPasswd, "authd:x:403:403", "authd:x:404:404", 1)},
	}
	for _, c := range cases {
		bus := authdBus(phoneRow())
		bus.uids[":1.42"] = c.uid
		r := newRig(t, bus)
		r.write(t, "passwd", c.passwd)
		if ok, err := r.o.AuthOwnerIsAuthd(ctxT(t)); ok || err == nil {
			t.Errorf("%s: AuthOwnerIsAuthd = %v, %v", c.name, ok, err)
		}
		if role, err := r.o.KeyRole(ctxT(t), phonePK); err == nil {
			t.Errorf("%s: KeyRole = %d", c.name, role)
		}
		for _, call := range bus.log() {
			if strings.HasPrefix(call, "KeyInfo") {
				t.Errorf("%s: %s sent to an owner that is not authd", c.name, call)
			}
		}
	}
}

func TestOracleRefusesAnOwnerThatIsNotAUniqueName(t *testing.T) {
	for _, owner := range []string{"", "com.tesla.Auth"} {
		bus := authdBus()
		bus.names["com.tesla.Auth"] = owner
		bus.uids[owner] = 403
		bus.keyInfo[owner] = keyInfoReply(phoneRow())
		r := newRig(t, bus)
		if role, err := r.o.KeyRole(ctxT(t), phonePK); err == nil {
			t.Errorf("owner %q: KeyRole = %d", owner, role)
		}
	}
}

func TestOracleDeniesAKeyInfoReplyOfTheWrongShape(t *testing.T) {
	bus := authdBus()
	bus.keyInfo[":1.42"] = []interface{}{[][]interface{}{{phoneID, phonePK, int32(2), uint32(6), uint64(0)}}}
	r := newRig(t, bus)
	if role, err := r.o.KeyRole(ctxT(t), phonePK); err == nil {
		t.Fatalf("int32 role accepted as %d", role)
	}
}

func TestOracleDeniesUnlistedAndAmbiguousKeys(t *testing.T) {
	for name, rows := range map[string][]KeyInfoEntry{
		"not listed":   {otherRow()},
		"listed twice": {phoneRow(), phoneRow()},
		"Id only":      {{ID: phoneID, PublicKey: otherPK, Role: 2, FormFactor: 6}},
	} {
		r := newRig(t, authdBus(rows...))
		if role, err := r.o.KeyRole(ctxT(t), phonePK); err == nil {
			t.Errorf("%s: KeyRole = %d", name, role)
		}
		if e, err := r.o.KeyEntry(ctxT(t), phonePK); err == nil {
			t.Errorf("%s: KeyEntry = %+v", name, e)
		}
		if Check(ctxT(t), r.o, phonePK) == nil {
			t.Errorf("%s: Check accepted", name)
		}
	}
}

func TestOracleKeyEntryIsTheMatchedRow(t *testing.T) {
	r := newRig(t, authdBus(otherRow(), phoneRow()))
	e, err := r.o.KeyEntry(ctxT(t), phonePK)
	if err != nil {
		t.Fatal(err)
	}
	want := KeyInfoEntry{ID: mustHex("cce6a2d2"), PublicKey: phonePK, Role: 2, FormFactor: 6, SecondsLeft: 0}
	if !reflect.DeepEqual(e, want) {
		t.Fatalf("got %+v\nwant %+v", e, want)
	}
}

func TestOraclePinMismatchFailsEveryCheckClosed(t *testing.T) {
	r := newRig(t, authdBus(phoneRow()))
	r.write(t, "authd", "another build")
	for i := 0; i < 3; i++ {
		_, err := r.o.KeyRole(ctxT(t), phonePK)
		if err == nil || !strings.Contains(err.Error(), "authd changed") || !strings.Contains(err.Error(), "re-verif") {
			t.Fatalf("check %d: err=%v, want one saying authd changed and KeyInfo must be re-verified", i, err)
		}
		if ok, err := r.o.AuthOwnerIsAuthd(ctxT(t)); ok || err == nil {
			t.Fatalf("check %d: AuthOwnerIsAuthd = %v, %v", i, ok, err)
		}
		r.write(t, "authd", "abc") // the mismatch stands for the life of the process
		r.clock.advance(time.Second)
	}
	if n := r.dialCount(); n != 0 {
		t.Fatalf("dialled the bus %d times with a changed authd", n)
	}
}

func TestOraclePinIsReadLazilyAndOnce(t *testing.T) {
	// Lazily: nothing is read or dialled when the oracle is built.
	dir := t.TempDir()
	dialled := false
	_, err := newAuthdOracle(pinABC, filepath.Join(dir, "no-authd"), filepath.Join(dir, "no-passwd"),
		func() (authBus, error) { dialled = true; return nil, errors.New("no bus") })
	if err != nil || dialled {
		t.Fatalf("building the oracle: err=%v dialled=%v", err, dialled)
	}

	// Once: after a match the file is not hashed again, even to reconnect.
	bus1, bus2 := authdBus(phoneRow()), authdBus(phoneRow())
	r := newRig(t, bus1, bus2)
	if _, err := r.o.KeyRole(ctxT(t), phonePK); err != nil {
		t.Fatal(err)
	}
	r.write(t, "authd", "another build")
	bus1.drop()
	r.clock.advance(time.Second)
	if role, err := r.o.KeyRole(ctxT(t), phonePK); err != nil || role != 2 {
		t.Fatalf("after reconnect: role=%d err=%v", role, err)
	}
	if n := r.dialCount(); n != 2 {
		t.Fatalf("dials %d, want 2", n)
	}
}

func TestOraclePinReadErrorIsRetried(t *testing.T) {
	r := newRig(t, authdBus(phoneRow()))
	os.Remove(r.path("authd"))
	if _, err := r.o.KeyRole(ctxT(t), phonePK); err == nil {
		t.Fatal("unreadable authd accepted")
	}
	r.write(t, "authd", "abc")
	r.clock.advance(time.Second)
	if role, err := r.o.KeyRole(ctxT(t), phonePK); err != nil || role != 2 {
		t.Fatalf("role=%d err=%v", role, err)
	}
}

func TestNewAuthdOraclePinFormat(t *testing.T) {
	dial := func() (authBus, error) { return nil, errors.New("unused") }
	for _, bad := range []string{"", "abc", pinABC[:63], pinABC + "0", strings.Repeat("g", 64), " " + pinABC[1:]} {
		if _, err := newAuthdOracle(bad, "authd", "passwd", dial); err == nil {
			t.Errorf("pin %q accepted", bad)
		}
	}
	for _, good := range []string{pinABC, strings.ToUpper(pinABC), DefaultAuthdSHA256} {
		if _, err := newAuthdOracle(good, "authd", "passwd", dial); err != nil {
			t.Errorf("pin %q: %v", good, err)
		}
	}
}

func TestOracleReadsPasswdWhenItConnects(t *testing.T) {
	r := newRig(t, authdBus(phoneRow()))
	os.Remove(r.path("passwd"))
	for i := 0; i < 2; i++ {
		if _, err := r.o.KeyRole(ctxT(t), phonePK); err == nil {
			t.Fatal("missing passwd accepted")
		}
		r.clock.advance(time.Second)
	}
	if n := r.dialCount(); n != 0 {
		t.Fatalf("dialled %d times without authd's uid", n)
	}
	r.write(t, "passwd", testPasswd)
	if role, err := r.o.KeyRole(ctxT(t), phonePK); err != nil || role != 2 {
		t.Fatalf("role=%d err=%v", role, err)
	}
}

func TestOracleRetriesTheBusAtMostOncePerSecond(t *testing.T) {
	r := newRig(t, authdBus(phoneRow()))
	r.setDialErr(errors.New("read: connection reset by peer"))
	check := func(wantDials int) {
		t.Helper()
		_, err := r.o.KeyRole(ctxT(t), phonePK)
		if err == nil || !strings.Contains(err.Error(), "connection reset by peer") {
			t.Fatalf("err=%v, want the dial's cause", err)
		}
		if n := r.dialCount(); n != wantDials {
			t.Fatalf("dials %d, want %d", n, wantDials)
		}
	}
	check(1)
	check(1)
	r.clock.advance(999 * time.Millisecond)
	check(1)
	r.clock.advance(time.Millisecond)
	check(2)
	r.setDialErr(nil)
	r.clock.advance(time.Second)
	if role, err := r.o.KeyRole(ctxT(t), phonePK); err != nil || role != 2 {
		t.Fatalf("role=%d err=%v", role, err)
	}
	if n := r.dialCount(); n != 3 {
		t.Fatalf("dials %d, want 3", n)
	}
}

func TestOracleReconnectsWhenTheBusDrops(t *testing.T) {
	bus1, bus2, bus3 := authdBus(phoneRow()), authdBus(phoneRow()), authdBus(phoneRow())
	r := newRig(t, bus1, bus2, bus3)
	if _, err := r.o.KeyRole(ctxT(t), phonePK); err != nil {
		t.Fatal(err)
	}
	// Long after connecting: the next check re-dials at once.
	r.clock.advance(time.Hour)
	bus1.drop()
	if role, err := r.o.KeyRole(ctxT(t), phonePK); err != nil || role != 2 {
		t.Fatalf("after drop: role=%d err=%v", role, err)
	}
	if !bus1.isClosed() || r.dialCount() != 2 || len(bus2.log()) == 0 {
		t.Fatalf("closed=%v dials=%d bus2 calls=%q", bus1.isClosed(), r.dialCount(), bus2.log())
	}
	// Right after connecting: fail closed until a second has passed.
	bus2.drop()
	if _, err := r.o.KeyRole(ctxT(t), phonePK); err == nil {
		t.Fatal("accepted with the bus down")
	}
	if n := r.dialCount(); n != 2 {
		t.Fatalf("dials %d, want 2 (rate limited)", n)
	}
	r.clock.advance(time.Second)
	if role, err := r.o.KeyRole(ctxT(t), phonePK); err != nil || role != 2 {
		t.Fatalf("role=%d err=%v", role, err)
	}
}

func TestOracleWaitsForTheDialOnlyWithinTheBudget(t *testing.T) {
	r := newRig(t, authdBus(phoneRow()))
	gate := make(chan struct{})
	r.gate = gate
	start := time.Now()
	if err := Check(context.Background(), r.o, phonePK); err == nil {
		t.Fatal("accepted while the bus handshake hangs")
	}
	if d := time.Since(start); d > 700*time.Millisecond {
		t.Fatalf("Check took %v, budget 500 ms", d)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if _, err := r.o.KeyRole(ctx, phonePK); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err=%v, want the context's deadline", err)
	}
	close(gate) // the dial in flight completes; the next check uses it
	if role, err := r.o.KeyRole(ctxT(t), phonePK); err != nil || role != 2 {
		t.Fatalf("role=%d err=%v", role, err)
	}
	if n := r.dialCount(); n != 1 {
		t.Fatalf("dials %d, want 1", n)
	}
}

func TestOracleDialsOnceForConcurrentChecks(t *testing.T) {
	r := newRig(t, authdBus(phoneRow()))
	gate, entered := make(chan struct{}), make(chan struct{}, 1)
	r.gate, r.entered = gate, entered
	var wg sync.WaitGroup
	errs := make(chan error, 8)
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := r.o.KeyRole(ctxT(t), phonePK)
			errs <- err
		}()
	}
	select {
	case <-entered:
	case <-time.After(2 * time.Second):
		close(gate)
		t.Fatal("no check dialled the bus")
	}
	close(gate)
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Error(err)
		}
	}
	if n := r.dialCount(); n != 1 {
		t.Fatalf("dials %d, want 1", n)
	}
}
