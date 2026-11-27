package authsrv

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/godbus/dbus/v5"

	"airgapp/gateway/camhelper/authproto"
)

// bus is a real dbus-daemon on a socket in a temp dir (skipped where there
// is none): the watcher's match rules are the daemon's to apply.
type bus struct {
	t    *testing.T
	dir  string
	addr string
	cmd  *exec.Cmd
}

func startBus(t *testing.T, dir string) *bus {
	t.Helper()
	bin, err := exec.LookPath("dbus-daemon")
	if err != nil {
		t.Skip("no dbus-daemon")
	}
	sock := filepath.Join(dir, "bus")
	conf := filepath.Join(dir, "bus.conf")
	os.WriteFile(conf, []byte(`<!DOCTYPE busconfig PUBLIC "-//freedesktop//DTD D-Bus Bus Configuration 1.0//EN"
 "http://www.freedesktop.org/standards/dbus/1.0/busconfig.dtd">
<busconfig>
  <type>custom</type>
  <listen>unix:path=`+sock+`</listen>
  <auth>EXTERNAL</auth>
  <policy context="default">
    <allow user="*"/>
    <allow own="*"/>
    <allow send_type="signal"/>
    <allow send_destination="*"/>
    <allow receive_type="*"/>
  </policy>
</busconfig>
`), 0o644)
	cmd := exec.Command(bin, "--config-file="+conf, "--nofork", "--print-address")
	out, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	line, err := bufio.NewReader(out).ReadString('\n')
	if err != nil {
		cmd.Process.Kill()
		t.Fatalf("dbus-daemon: %v", err)
	}
	b := &bus{t: t, dir: dir, addr: strings.TrimSpace(line), cmd: cmd}
	t.Cleanup(b.stop)
	return b
}

func (b *bus) stop() {
	if b.cmd.Process != nil {
		b.cmd.Process.Kill()
		b.cmd.Wait()
	}
}

func (b *bus) conn() (*dbus.Conn, error) {
	c, err := dbus.Dial(b.addr)
	if err != nil {
		return nil, err
	}
	return c, handshake(c, 2*time.Second)
}

// events collects what the watcher reports.
type events struct {
	mu  sync.Mutex
	got []authproto.Event
	ch  chan struct{}
}

func (e *events) notify(ev authproto.Event) {
	e.mu.Lock()
	e.got = append(e.got, ev)
	e.mu.Unlock()
	select {
	case e.ch <- struct{}{}:
	default:
	}
}

// expect waits for the events after the first n to equal want, and returns the new n.
func (e *events) expect(t *testing.T, n int, want ...authproto.Event) int {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		e.mu.Lock()
		got := append([]authproto.Event(nil), e.got[n:]...)
		e.mu.Unlock()
		if len(got) >= len(want) {
			if fmt.Sprint(got) != fmt.Sprint(want) {
				t.Fatalf("events %v, want %v", got, want)
			}
			return n + len(want)
		}
		if time.Now().After(deadline) {
			t.Fatalf("events %v, want %v", got, want)
		}
		select {
		case <-e.ch:
		case <-time.After(50 * time.Millisecond):
		}
	}
}

func (e *events) quiet(t *testing.T, n int) {
	t.Helper()
	time.Sleep(300 * time.Millisecond)
	e.mu.Lock()
	defer e.mu.Unlock()
	if len(e.got) != n {
		t.Fatalf("unexpected events %v", e.got[n:])
	}
}

func TestWatchAgainstARealBus(t *testing.T) {
	dir, err := os.MkdirTemp("", "authwatch")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	b := startBus(t, dir)

	// authd: owns com.tesla.Auth.
	authd, err := b.conn()
	if err != nil {
		t.Fatal(err)
	}
	if r, err := authd.RequestName(authName, dbus.NameFlagDoNotQueue); err != nil || r != dbus.RequestNameReplyPrimaryOwner {
		t.Fatalf("RequestName: %v %v", r, err)
	}

	ev := &events{ch: make(chan struct{}, 1)}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	cur := b
	var curMu sync.Mutex
	var watcher *dbus.Conn // the watch's own connection, for a signal sent straight to it
	w := &Watch{Notify: ev.notify, MinBackoff: 50 * time.Millisecond, MaxBackoff: 100 * time.Millisecond,
		Liveness: 50 * time.Millisecond, Dial: func() (*dbus.Conn, error) {
			curMu.Lock()
			defer curMu.Unlock()
			c, err := cur.conn()
			watcher = c
			return c, err
		}}
	go func() { w.Run(ctx); close(done) }()

	up := []authproto.Event{{Ev: authproto.EvWatchUp}, {Ev: authproto.EvKeysChanged, Reason: authproto.ReasonResubscribed}}
	n := ev.expect(t, 0, up...)

	// authd's KeysUpdated.
	if err := authd.Emit(authPath, authName+".KeysUpdated"); err != nil {
		t.Fatal(err)
	}
	n = ev.expect(t, n, authproto.Event{Ev: authproto.EvKeysChanged, Reason: authproto.ReasonKeysUpdated})

	// The same signal from another client does not match the pinned rule.
	spoof, err := b.conn()
	if err != nil {
		t.Fatal(err)
	}
	spoof.Emit(authPath, authName+".KeysUpdated")
	spoof.Emit("/Other", authName+".KeysUpdated")
	// Nor does one sent straight to the watch's connection, which no match rule filters.
	curMu.Lock()
	target := watcher.Names()[0]
	curMu.Unlock()
	direct := &dbus.Message{Type: dbus.TypeSignal, Headers: map[dbus.HeaderField]dbus.Variant{
		dbus.FieldPath:        dbus.MakeVariant(authPath),
		dbus.FieldInterface:   dbus.MakeVariant(authName),
		dbus.FieldMember:      dbus.MakeVariant("KeysUpdated"),
		dbus.FieldDestination: dbus.MakeVariant(target),
	}}
	if call := spoof.Send(direct, nil); call.Err != nil {
		t.Fatal(call.Err)
	}
	// A NameOwnerChanged sent straight to it by a client does not move the owner either.
	fake := &dbus.Message{Type: dbus.TypeSignal, Headers: map[dbus.HeaderField]dbus.Variant{
		dbus.FieldPath:        dbus.MakeVariant(dbus.ObjectPath("/org/freedesktop/DBus")),
		dbus.FieldInterface:   dbus.MakeVariant(busDriver),
		dbus.FieldMember:      dbus.MakeVariant("NameOwnerChanged"),
		dbus.FieldDestination: dbus.MakeVariant(target),
		dbus.FieldSignature:   dbus.MakeVariant(dbus.SignatureOf("", "", "")),
	}, Body: []interface{}{authName, authd.Names()[0], spoof.Names()[0]}}
	spoof.Send(fake, nil)
	spoof.Send(direct, nil)
	ev.quiet(t, n)
	// The real owner still counts.
	authd.Emit(authPath, authName+".KeysUpdated")
	n = ev.expect(t, n, authproto.Event{Ev: authproto.EvKeysChanged, Reason: authproto.ReasonKeysUpdated})

	// authd goes away (its name changes owner) and comes back.
	authd.Close()
	n = ev.expect(t, n, authproto.Event{Ev: authproto.EvKeysChanged, Reason: authproto.ReasonOwnerChanged})
	authd2, err := b.conn()
	if err != nil {
		t.Fatal(err)
	}
	authd2.RequestName(authName, dbus.NameFlagDoNotQueue)
	n = ev.expect(t, n, authproto.Event{Ev: authproto.EvKeysChanged, Reason: authproto.ReasonOwnerChanged})
	// Now the spoofer cannot fake it either, nor the old owner's name, while authd2 can.
	spoof.Emit(authPath, authName+".KeysUpdated")
	direct.Headers[dbus.FieldDestination] = dbus.MakeVariant(watcher.Names()[0])
	spoof.Send(direct, nil)
	authd2.Emit(authPath, authName+".KeysUpdated")
	n = ev.expect(t, n, authproto.Event{Ev: authproto.EvKeysChanged, Reason: authproto.ReasonKeysUpdated})
	ev.quiet(t, n)

	// The bus dies: watch_down; a new bus: up and resubscribed again.
	b.stop()
	n = ev.expect(t, n, authproto.Event{Ev: authproto.EvWatchDown})
	dir2, _ := os.MkdirTemp("", "authwatch2")
	t.Cleanup(func() { os.RemoveAll(dir2) })
	b2 := startBus(t, dir2)
	curMu.Lock()
	cur = b2
	curMu.Unlock()
	deadline := time.Now().Add(5 * time.Second)
	for {
		ev.mu.Lock()
		var tail []authproto.Event
		for _, e := range ev.got[n:] {
			if e.Ev != authproto.EvWatchDown { // failed retries against the dead bus
				tail = append(tail, e)
			}
		}
		ev.mu.Unlock()
		if len(tail) >= 2 {
			if fmt.Sprint(tail[:2]) != fmt.Sprint(up) {
				t.Fatalf("after the restart: %v", tail)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("no resubscription after the restart: %v", tail)
		}
		time.Sleep(20 * time.Millisecond)
	}

	cancel()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("Run did not return")
	}
	ev.mu.Lock()
	last := ev.got[len(ev.got)-1]
	ev.mu.Unlock()
	if last.Ev != authproto.EvWatchDown {
		t.Fatalf("Run must end with watch_down, got %v", last)
	}
}

func TestDispatchIgnoresOtherSignals(t *testing.T) {
	tr := &tracker{owner: ":1.64"}
	for _, s := range []*dbus.Signal{
		{Name: busDriver + ".NameAcquired", Sender: busDriver, Body: []interface{}{":1.5"}},
		{Name: busDriver + ".NameOwnerChanged", Sender: busDriver, Body: []interface{}{"com.tesla.Other", "", ":1.9"}},
		{Name: busDriver + ".NameOwnerChanged", Sender: ":1.9", Body: []interface{}{authName, "", ":1.9"}},
		{Name: authName + ".KeysUpdated", Path: "/Other", Sender: ":1.64"},
		{Name: authName + ".KeysUpdated", Path: authPath, Sender: ":1.9"},
		{Name: authName + ".Other", Path: authPath, Sender: ":1.64"},
	} {
		if e, ok := tr.dispatch(s); ok {
			t.Errorf("%+v dispatched as %+v", s, e)
		}
	}
	if tr.owner != ":1.64" {
		t.Fatalf("the owner moved: %q", tr.owner)
	}
	// The owner's KeysUpdated counts; the bus driver's NameOwnerChanged moves the owner.
	if _, ok := tr.dispatch(&dbus.Signal{Name: authName + ".KeysUpdated", Path: authPath, Sender: ":1.64"}); !ok {
		t.Fatal("the owner's KeysUpdated")
	}
	if _, ok := tr.dispatch(&dbus.Signal{Name: busDriver + ".NameOwnerChanged", Sender: busDriver, Body: []interface{}{authName, ":1.64", ""}}); !ok || tr.owner != "" {
		t.Fatalf("owner gone: %q", tr.owner)
	}
	if _, ok := tr.dispatch(&dbus.Signal{Name: authName + ".KeysUpdated", Path: authPath, Sender: ":1.64"}); ok {
		t.Fatal("no owner: no KeysUpdated counts")
	}
}
