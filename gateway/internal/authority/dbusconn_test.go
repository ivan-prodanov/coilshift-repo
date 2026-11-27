package authority

import (
	"bufio"
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"path/filepath"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/godbus/dbus/v5"
)

// daemon is a bus daemon for one godbus client over a pipe: it accepts the
// EXTERNAL handshake, answers Hello with :1.7, then answers every other method
// call with answer's reply body, or with a D-Bus error when answer names one.
// It records those calls as "destination path interface.member body".
type daemon struct {
	answer func(m *dbus.Message) (body []interface{}, errName string)
	srv    net.Conn

	mu    sync.Mutex
	calls []string
}

func startDaemon(t *testing.T, answer func(m *dbus.Message) ([]interface{}, string)) (*dbus.Conn, *daemon) {
	t.Helper()
	cli, srv := net.Pipe()
	d := &daemon{answer: answer, srv: srv}
	go d.serve()
	c, err := dbus.NewConn(cli)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	if err := handshake(c, 2*time.Second); err != nil {
		t.Fatal(err)
	}
	return c, d
}

func header(m *dbus.Message, f dbus.HeaderField) string {
	v, ok := m.Headers[f]
	if !ok {
		return ""
	}
	return fmt.Sprint(v.Value())
}

func (d *daemon) serve() {
	defer d.srv.Close()
	r := bufio.NewReader(d.srv)
	if !busAuth(d.srv, r) {
		return
	}
	for {
		m, err := dbus.DecodeMessage(r)
		if err != nil {
			return
		}
		var body []interface{}
		errName := "org.freedesktop.DBus.Error.UnknownMethod"
		if header(m, dbus.FieldMember) == "Hello" {
			body, errName = []interface{}{":1.7"}, ""
		} else {
			d.mu.Lock()
			d.calls = append(d.calls, fmt.Sprintf("%s %s %s.%s %v", header(m, dbus.FieldDestination),
				header(m, dbus.FieldPath), header(m, dbus.FieldInterface), header(m, dbus.FieldMember), m.Body))
			d.mu.Unlock()
			if d.answer != nil {
				body, errName = d.answer(m)
			}
		}
		reply := &dbus.Message{Type: dbus.TypeMethodReply, Headers: map[dbus.HeaderField]dbus.Variant{
			dbus.FieldReplySerial: dbus.MakeVariant(m.Serial()),
		}}
		if errName != "" {
			reply.Type = dbus.TypeError
			reply.Headers[dbus.FieldErrorName] = dbus.MakeVariant(errName)
			body = []interface{}{"refused by the test daemon"}
		}
		reply.Body = body
		if len(body) > 0 {
			reply.Headers[dbus.FieldSignature] = dbus.MakeVariant(dbus.SignatureOf(body...))
		}
		if err := reply.EncodeTo(d.srv, binary.LittleEndian); err != nil {
			return
		}
	}
}

func (d *daemon) log() []string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]string(nil), d.calls...)
}

// authdDaemon answers as the car's bus did: :1.42, uid 403, owns
// com.tesla.Auth and lists the phone.
func authdDaemon(m *dbus.Message) ([]interface{}, string) {
	switch header(m, dbus.FieldMember) {
	case "GetNameOwner":
		return []interface{}{":1.42"}, ""
	case "GetConnectionUnixUser":
		return []interface{}{uint32(403)}, ""
	case "KeyInfo":
		return []interface{}{[]wireRow{
			{ID: sha1ID(otherPK), PublicKey: otherPK, Role: 1},
			{ID: phoneID, PublicKey: phonePK, Role: 2, FormFactor: 6},
		}}, ""
	}
	return nil, "org.freedesktop.DBus.Error.UnknownMethod"
}

func TestDBusConnCallsOnTheWire(t *testing.T) {
	c, d := startDaemon(t, authdDaemon)
	b := dbusConn{c}
	ctx := ctxT(t)
	owner, err := b.NameOwner(ctx, "com.tesla.Auth")
	if err != nil || owner != ":1.42" {
		t.Fatalf("NameOwner = %q, %v", owner, err)
	}
	uid, err := b.UnixUser(ctx, ":1.42")
	if err != nil || uid != 403 {
		t.Fatalf("UnixUser = %d, %v", uid, err)
	}
	body, err := b.KeyInfo(ctx, ":1.42")
	if err != nil {
		t.Fatal(err)
	}
	role, ff, err := RoleFor(mustParse(t, body), phonePK)
	if err != nil || role != 2 || ff != 6 {
		t.Fatalf("phone: role=%d ff=%d err=%v", role, ff, err)
	}
	want := []string{
		"org.freedesktop.DBus /org/freedesktop/DBus org.freedesktop.DBus.GetNameOwner [com.tesla.Auth]",
		"org.freedesktop.DBus /org/freedesktop/DBus org.freedesktop.DBus.GetConnectionUnixUser [:1.42]",
		":1.42 /Auth com.tesla.Auth.KeyInfo []",
	}
	if got := d.log(); !reflect.DeepEqual(got, want) {
		t.Fatalf("on the wire:\n%q\nwant\n%q", got, want)
	}
}

func mustParse(t *testing.T, body []interface{}) []KeyInfoEntry {
	t.Helper()
	e, err := ParseKeyInfoBody(body)
	if err != nil {
		t.Fatal(err)
	}
	return e
}

func TestDBusConnTakesOnlyExactReplyTypes(t *testing.T) {
	cases := []struct {
		name  string
		reply []interface{}
		call  func(b dbusConn) error
	}{
		{"uid as int32", []interface{}{int32(403)}, func(b dbusConn) error { _, err := b.UnixUser(ctxT(t), ":1.42"); return err }},
		{"uid and more", []interface{}{uint32(403), uint32(0)}, func(b dbusConn) error { _, err := b.UnixUser(ctxT(t), ":1.42"); return err }},
		{"owner as an object path", []interface{}{dbus.ObjectPath("/1/42")}, func(b dbusConn) error { _, err := b.NameOwner(ctxT(t), "com.tesla.Auth"); return err }},
		{"no owner value", nil, func(b dbusConn) error { _, err := b.NameOwner(ctxT(t), "com.tesla.Auth"); return err }},
	}
	for _, c := range cases {
		conn, _ := startDaemon(t, func(*dbus.Message) ([]interface{}, string) { return c.reply, "" })
		if err := c.call(dbusConn{conn}); err == nil {
			t.Errorf("%s: accepted", c.name)
		}
	}
}

func TestDBusConnReturnsDBusErrors(t *testing.T) {
	c, _ := startDaemon(t, func(*dbus.Message) ([]interface{}, string) {
		return nil, "org.freedesktop.DBus.Error.AccessDenied"
	})
	if body, err := (dbusConn{c}).KeyInfo(ctxT(t), ":1.42"); err == nil {
		t.Fatalf("AccessDenied returned as %v", body)
	}
}

func TestDBusConnNoticesTheDaemonHangingUp(t *testing.T) {
	c, d := startDaemon(t, authdDaemon)
	b := dbusConn{c}
	if !b.Connected() {
		t.Fatal("not connected after the handshake")
	}
	d.srv.Close()
	for deadline := time.Now().Add(2 * time.Second); b.Connected(); time.Sleep(5 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("still connected after the daemon hung up")
		}
	}
}

func TestHandshakeGivesUpOnASilentDaemon(t *testing.T) {
	cli, srv := net.Pipe()
	defer srv.Close()
	go io.Copy(io.Discard, srv) // reads the client's lines, never answers
	c, err := dbus.NewConn(cli)
	if err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	if err := handshake(c, 100*time.Millisecond); err == nil {
		t.Fatal("handshake succeeded with a silent daemon")
	}
	if d := time.Since(start); d > time.Second {
		t.Fatalf("handshake took %v, limit 100 ms", d)
	}
	if c.Connected() {
		t.Fatal("connection left open")
	}
}

func TestOracleOverTheWire(t *testing.T) {
	c, d := startDaemon(t, authdDaemon)
	dir := t.TempDir()
	r := &rig{dir: dir}
	r.write(t, "authd", "abc")
	r.write(t, "passwd", testPasswd)
	o, err := newAuthdOracle(pinABC, filepath.Join(dir, "authd"), filepath.Join(dir, "passwd"),
		func() (authBus, error) { return dbusConn{c}, nil })
	if err != nil {
		t.Fatal(err)
	}
	if err := Check(context.Background(), o, phonePK); err != nil {
		t.Fatalf("Check: %v", err)
	}
	if got := d.log(); got[len(got)-1] != ":1.42 /Auth com.tesla.Auth.KeyInfo []" {
		t.Fatalf("calls %q", got)
	}
	if err := Check(context.Background(), o, otherPK); err == nil {
		t.Fatal("SERVICE key accepted")
	}
}
