package authority

import (
	"context"
	"errors"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/godbus/dbus/v5"
)

var sentinels = []struct {
	name string
	err  error
}{
	{"ErrAuthOwner", ErrAuthOwner},
	{"ErrKeyUnknown", ErrKeyUnknown},
	{"ErrKeyAmbiguous", ErrKeyAmbiguous},
	{"ErrShape", ErrShape},
	{"ErrBus", ErrBus},
	{"ErrAuthdChanged", ErrAuthdChanged},
}

// wantClass requires err to be an error that matches exactly want among the
// sentinels (none of them when want is nil).
func wantClass(t *testing.T, name string, err, want error) {
	t.Helper()
	if err == nil {
		t.Errorf("%s: no error, want %v", name, want)
		return
	}
	for _, s := range sentinels {
		if got := errors.Is(err, s.err); got != (s.err == want) {
			t.Errorf("%s: errors.Is(%q, %s) = %v", name, err, s.name, got)
		}
	}
}

// Every error path of the authd oracle wraps exactly one sentinel.
func TestOracleErrorClasses(t *testing.T) {
	owner := func(r *rig) error { _, err := r.o.AuthOwnerIsAuthd(ctxT(t)); return err }
	role := func(r *rig) error { _, err := r.o.KeyRole(ctxT(t), phonePK); return err }
	entry := func(r *rig) error { _, err := r.o.KeyEntry(ctxT(t), phonePK); return err }
	both := func(r *rig) error { // the owner check, and the key behind it
		if err := owner(r); err == nil {
			return errors.New("the owner check passed")
		}
		r.clock.advance(time.Second)
		return role(r)
	}
	with := func(f func(*KeyInfoEntry)) *fakeBus {
		e := phoneRow()
		f(&e)
		return authdBus(otherRow(), e)
	}
	cases := []struct {
		name  string
		bus   *fakeBus
		setup func(*rig)
		call  func(*rig) error
		want  error
	}{
		{"GetNameOwner fails", func() *fakeBus { b := authdBus(phoneRow()); delete(b.names, authDest); return b }(), nil, both, ErrBus},
		{"owner is empty", func() *fakeBus { b := authdBus(phoneRow()); b.names[authDest] = ""; return b }(), nil, both, ErrAuthOwner},
		{"owner is a well-known name", func() *fakeBus { b := authdBus(phoneRow()); b.names[authDest] = authDest; return b }(), nil, both, ErrAuthOwner},
		{"GetConnectionUnixUser fails", func() *fakeBus { b := authdBus(phoneRow()); delete(b.uids, ":1.42"); return b }(), nil, both, ErrBus},
		{"owner runs as root", func() *fakeBus { b := authdBus(phoneRow()); b.uids[":1.42"] = 0; return b }(), nil, both, ErrAuthOwner},
		{"owner runs as the gateway", func() *fakeBus { b := authdBus(phoneRow()); b.uids[":1.42"] = 4950; return b }(), nil, both, ErrAuthOwner},
		{"KeyInfo call fails", func() *fakeBus { b := authdBus(phoneRow()); delete(b.keyInfo, ":1.42"); return b }(), nil, role, ErrBus},
		{"KeyInfo has two values", func() *fakeBus {
			b := authdBus()
			b.keyInfo[":1.42"] = append(keyInfoReply(phoneRow()), uint32(0))
			return b
		}(), nil, role, ErrShape},
		{"KeyInfo role is int32", func() *fakeBus {
			b := authdBus()
			b.keyInfo[":1.42"] = []interface{}{[][]interface{}{{phoneID, phonePK, int32(2), uint32(6), uint64(0)}}}
			return b
		}(), nil, role, ErrShape},
		{"key not listed", authdBus(otherRow()), nil, role, ErrKeyUnknown},
		{"key not listed (KeyEntry)", authdBus(otherRow()), nil, entry, ErrKeyUnknown},
		{"empty list", authdBus(), nil, role, ErrKeyUnknown},
		{"key listed twice", authdBus(phoneRow(), phoneRow()), nil, role, ErrKeyAmbiguous},
		{"key listed twice (KeyEntry)", authdBus(phoneRow(), phoneRow()), nil, entry, ErrKeyAmbiguous},
		{"Id is not SHA-1(key)[:4]", with(func(e *KeyInfoEntry) { e.ID = mustHex("cce6a2d3") }), nil, role, ErrShape},
		{"Id 5 bytes", with(func(e *KeyInfoEntry) { e.ID = append(append([]byte{}, phoneID...), 0) }), nil, entry, ErrShape},
		{"SecondsLeft 1", with(func(e *KeyInfoEntry) { e.SecondsLeft = 1 }), nil, role, ErrAuthdChanged},
		{"role 12", with(func(e *KeyInfoEntry) { e.Role = 12 }), nil, entry, ErrAuthdChanged},
		{"64-byte key", authdBus(phoneRow()), nil, func(r *rig) error { _, err := r.o.KeyRole(ctxT(t), phonePK[1:]); return err }, ErrShape},
		{"key prefix 0x05", authdBus(phoneRow()), nil, func(r *rig) error {
			_, err := r.o.KeyEntry(ctxT(t), append([]byte{5}, phonePK[1:]...))
			return err
		}, ErrShape},
		{"authd pin mismatch", authdBus(phoneRow()), func(r *rig) { r.write(t, "authd", "another build") }, both, ErrAuthdChanged},
		{"authd unreadable", authdBus(phoneRow()), func(r *rig) { os.Remove(r.path("authd")) }, both, ErrBus},
		{"passwd missing", authdBus(phoneRow()), func(r *rig) { os.Remove(r.path("passwd")) }, both, ErrBus},
		{"no authd in passwd", authdBus(phoneRow()), func(r *rig) { r.write(t, "passwd", "root:x:0:0::/root:/bin/sh\n") }, both, ErrBus},
		{"dial fails", authdBus(phoneRow()), func(r *rig) { r.setDialErr(errors.New("read: connection reset by peer")) }, both, ErrBus},
		{"handshake fails", authdBus(phoneRow()), func(r *rig) { r.setDialErr(fmt.Errorf("handshake: %w", errors.New("timed out"))) }, both, ErrBus},
	}
	for _, c := range cases {
		r := newRig(t, c.bus)
		if c.setup != nil {
			c.setup(r)
		}
		wantClass(t, c.name, c.call(r), c.want)
	}
}

// A dropped connection is not dialled again within a second: ErrBus.
func TestOracleErrorClassDroppedBus(t *testing.T) {
	bus := authdBus(phoneRow())
	r := newRig(t, bus)
	if _, err := r.o.KeyRole(ctxT(t), phonePK); err != nil {
		t.Fatal(err)
	}
	bus.drop()
	_, err := r.o.KeyRole(ctxT(t), phonePK)
	wantClass(t, "dropped", err, ErrBus)
}

// A check that gives up waiting for the dial: ErrBus, and still the context's
// deadline.
func TestOracleErrorClassDialInFlight(t *testing.T) {
	r := newRig(t, authdBus(phoneRow()))
	gate := make(chan struct{})
	r.gate = gate
	defer close(gate)
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	_, err := r.o.AuthOwnerIsAuthd(ctx)
	wantClass(t, "dial in flight", err, ErrBus)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err=%v, want the context's deadline too", err)
	}
}

// Over the wire: a bus driver reply of the wrong type is ErrShape (never also
// ErrBus); a D-Bus error reply is ErrBus.
func TestOracleErrorClassesOverTheWire(t *testing.T) {
	oracle := func(t *testing.T, answer func(m *dbus.Message) ([]interface{}, string)) *authdOracle {
		c, _ := startDaemon(t, answer)
		r := &rig{dir: t.TempDir()}
		r.write(t, "authd", "abc")
		r.write(t, "passwd", testPasswd)
		o, err := newAuthdOracle(pinABC, r.path("authd"), r.path("passwd"), func() (authBus, error) { return dbusConn{c}, nil })
		if err != nil {
			t.Fatal(err)
		}
		return o
	}
	replace := func(member string, body []interface{}, errName string) func(m *dbus.Message) ([]interface{}, string) {
		return func(m *dbus.Message) ([]interface{}, string) {
			if header(m, dbus.FieldMember) == member {
				return body, errName
			}
			return authdDaemon(m)
		}
	}
	cases := []struct {
		name   string
		answer func(m *dbus.Message) ([]interface{}, string)
		call   func(o *authdOracle) error
		want   error
	}{
		{"uid as int32", replace("GetConnectionUnixUser", []interface{}{int32(403)}, ""),
			func(o *authdOracle) error { _, err := o.AuthOwnerIsAuthd(ctxT(t)); return err }, ErrShape},
		{"owner as an object path", replace("GetNameOwner", []interface{}{dbus.ObjectPath("/1/42")}, ""),
			func(o *authdOracle) error { _, err := o.AuthOwnerIsAuthd(ctxT(t)); return err }, ErrShape},
		{"no owner value", replace("GetNameOwner", nil, ""),
			func(o *authdOracle) error { _, err := o.AuthOwnerIsAuthd(ctxT(t)); return err }, ErrShape},
		{"GetNameOwner NameHasNoOwner", replace("GetNameOwner", nil, "org.freedesktop.DBus.Error.NameHasNoOwner"),
			func(o *authdOracle) error { _, err := o.AuthOwnerIsAuthd(ctxT(t)); return err }, ErrBus},
		{"KeyInfo AccessDenied", replace("KeyInfo", nil, "org.freedesktop.DBus.Error.AccessDenied"),
			func(o *authdOracle) error { _, err := o.KeyRole(ctxT(t), phonePK); return err }, ErrBus},
		{"KeyInfo as a(ayayiut)", replace("KeyInfo", []interface{}{[]struct {
			ID, PublicKey []byte
			Role          int32
			FormFactor    uint32
			SecondsLeft   uint64
		}{{phoneID, phonePK, 2, 6, 0}}}, ""),
			func(o *authdOracle) error { _, err := o.KeyRole(ctxT(t), phonePK); return err }, ErrShape},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			wantClass(t, c.name, c.call(oracle(t, c.answer)), c.want)
		})
	}
}

// RoleFor and ParseKeyInfoBody, the matching the sign-in helper's fakes use too.
func TestRoleForErrorClasses(t *testing.T) {
	with := func(f func(*KeyInfoEntry)) []KeyInfoEntry {
		e := phoneRow()
		f(&e)
		return []KeyInfoEntry{otherRow(), e}
	}
	cases := []struct {
		name string
		rows []KeyInfoEntry
		pk   []byte
		want error
	}{
		{"not listed", []KeyInfoEntry{otherRow()}, phonePK, ErrKeyUnknown},
		{"only the Id matches", []KeyInfoEntry{{ID: phoneID, PublicKey: otherPK, Role: 2}}, phonePK, ErrKeyUnknown},
		{"listed three times", []KeyInfoEntry{phoneRow(), phoneRow(), phoneRow()}, phonePK, ErrKeyAmbiguous},
		{"Id empty", with(func(e *KeyInfoEntry) { e.ID = nil }), phonePK, ErrShape},
		{"SecondsLeft 7", with(func(e *KeyInfoEntry) { e.SecondsLeft = 7 }), phonePK, ErrAuthdChanged},
		{"role 2^31", with(func(e *KeyInfoEntry) { e.Role = 1 << 31 }), phonePK, ErrAuthdChanged},
		{"33-byte key", nil, append([]byte{2}, phonePK[1:33]...), ErrShape},
		{"no key", nil, nil, ErrShape},
	}
	for _, c := range cases {
		_, _, err := RoleFor(c.rows, c.pk)
		wantClass(t, c.name, err, c.want)
	}
	for name, body := range map[string][]interface{}{
		"nil body":    nil,
		"two values":  {[][]interface{}{}, uint32(0)},
		"a string":    {"a(ayayuut)"},
		"4 fields":    {[][]interface{}{{phoneID, phonePK, uint32(2), uint32(6)}}},
		"uint32 secs": {[][]interface{}{{phoneID, phonePK, uint32(2), uint32(6), uint32(0)}}},
	} {
		_, err := ParseKeyInfoBody(body)
		wantClass(t, "ParseKeyInfoBody "+name, err, ErrShape)
	}
}

// Check passes the oracle's class through; its own refusals are classed only
// where they are one of the six.
func TestCheckErrorClasses(t *testing.T) {
	wantClass(t, "owner not authd", Check(context.Background(), Fake{Role: 2, Owner: false}, pk), ErrAuthOwner)
	for _, s := range sentinels {
		err := Check(context.Background(), Fake{Role: 2, Owner: true, Err: fmt.Errorf("oracle: %w", s.err)}, pk)
		wantClass(t, "passes "+s.name, err, s.err)
	}
	wantClass(t, "role 8", Check(context.Background(), Fake{Role: 8, Owner: true}, pk), nil)
	err := Check(context.Background(), Fake{Role: 2, Owner: true, Delay: 600 * time.Millisecond}, pk)
	wantClass(t, "deadline", err, nil)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("deadline: %v", err)
	}
}
