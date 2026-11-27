package cmdrelay

import (
	"bufio"
	"bytes"
	"context"
	"io"
	"log"
	"sync"
	"testing"
	"time"

	"github.com/godbus/dbus/v5"

	"airgapp/gateway/camhelper/proto"
	"airgapp/gateway/internal/carlink/bustest"
)

// fakeRouter owns com.tesla.CommandRouter; RouteFromAuthd records the bytes and answers like the car's router: a call
// to com.tesla.Auth.RouteMessageBytes carrying the reply.
type fakeRouter struct {
	conn  *dbus.Conn
	reply []byte
	mu    sync.Mutex
	got   [][]byte
}

func (r *fakeRouter) RouteFromAuthd(b []byte) *dbus.Error {
	r.mu.Lock()
	r.got = append(r.got, append([]byte(nil), b...))
	r.mu.Unlock()
	go r.conn.Object(authName, "/Auth").Call(authName+"."+authMember, dbus.FlagNoReplyExpected, r.reply)
	return nil
}

// fakeAuthd owns com.tesla.Auth and drops whatever it is given (as the car's authd does).
type fakeAuthd struct{}

func (fakeAuthd) RouteMessageBytes([]byte) *dbus.Error { return nil }

func own(t *testing.T, c *dbus.Conn, name string) {
	t.Helper()
	r, err := c.RequestName(name, dbus.NameFlagDoNotQueue)
	if err != nil || r != dbus.RequestNameReplyPrimaryOwner {
		t.Fatalf("own %s: %v %v", name, r, err)
	}
}

func TestBusRoundTrip(t *testing.T) {
	bus := bustest.Start(t)
	dial := func() (*dbus.Conn, error) {
		c, err := dbus.Dial(bus.Addr())
		if err != nil {
			return nil, err
		}
		if err := c.Auth(nil); err != nil {
			return nil, err
		}
		return c, c.Hello()
	}
	reply := reply(bytes.Repeat([]byte{0x22}, 16), DomainInfotainment)

	rc := bus.Conn(t)
	router := &fakeRouter{conn: rc, reply: reply}
	if err := rc.Export(router, routerPath, routerName); err != nil {
		t.Fatal(err)
	}
	own(t, rc, routerName)
	ac := bus.Conn(t)
	if err := ac.Export(fakeAuthd{}, "/Auth", authName); err != nil {
		t.Fatal(err)
	}
	own(t, ac, authName)

	// The monitor.
	got := make(chan []byte, 4)
	up := make(chan bool, 4)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	m := &Monitor{Dial: dial, Deliver: func(b []byte) { got <- b }, SetUp: func(v bool) { up <- v },
		Log: log.New(io.Discard, "", 0)}
	go m.Run(ctx)
	select {
	case v := <-up:
		if !v {
			t.Fatal("monitor reported down first")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("monitor never came up")
	}

	// The injector child, over pipes.
	inR, inW := io.Pipe()
	outR, outW := io.Pipe()
	childDone := make(chan error, 1)
	go func() { childDone <- RunChild(inR, outW, dial) }()
	msg := request(DomainInfotainment, bytes.Repeat([]byte{0x22}, 16))
	if err := proto.WriteFrame(inW, childMsg, msg); err != nil {
		t.Fatal(err)
	}
	typ, _, err := proto.ReadFrame(bufio.NewReader(outR), 4096)
	if err != nil || typ != childOK {
		t.Fatalf("child answered 0x%02x %v", typ, err)
	}
	router.mu.Lock()
	if len(router.got) != 1 || !bytes.Equal(router.got[0], msg) {
		t.Fatalf("router got %x", router.got)
	}
	router.mu.Unlock()
	select {
	case b := <-got:
		if !bytes.Equal(b, reply) {
			t.Fatalf("monitor delivered %x", b)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the monitor did not see the reply")
	}
	inW.Close()
	if err := <-childDone; err != nil {
		t.Fatalf("child: %v", err)
	}
}

// A call the router refuses comes back as childErr, and the child keeps serving.
func TestChildReportsCallErrors(t *testing.T) {
	bus := bustest.Start(t)
	dial := func() (*dbus.Conn, error) {
		c, err := dbus.Dial(bus.Addr())
		if err != nil {
			return nil, err
		}
		if err := c.Auth(nil); err != nil {
			return nil, err
		}
		return c, c.Hello()
	}
	inR, inW := io.Pipe()
	outR, outW := io.Pipe()
	go RunChild(inR, outW, dial)
	r := bufio.NewReader(outR)
	for i := 0; i < 2; i++ { // no router on the bus: ServiceUnknown, twice
		proto.WriteFrame(inW, childMsg, []byte{1, 2, 3})
		typ, body, err := proto.ReadFrame(r, 4096)
		if err != nil || typ != childErr || len(body) == 0 {
			t.Fatalf("answer %d: 0x%02x %q %v", i, typ, body, err)
		}
	}
	inW.Close()
}

func TestReplyBodyFilter(t *testing.T) {
	mk := func(typ dbus.Type, dest, iface, member string, body ...interface{}) *dbus.Message {
		return &dbus.Message{Type: typ, Headers: map[dbus.HeaderField]dbus.Variant{
			dbus.FieldDestination: dbus.MakeVariant(dest), dbus.FieldInterface: dbus.MakeVariant(iface),
			dbus.FieldMember: dbus.MakeVariant(member)}, Body: body}
	}
	ok := mk(dbus.TypeMethodCall, authName, authName, authMember, []byte{1, 2})
	if b := replyBody(ok); !bytes.Equal(b, []byte{1, 2}) {
		t.Fatal("the reply was not taken")
	}
	for _, m := range []*dbus.Message{
		nil,
		mk(dbus.TypeSignal, authName, authName, authMember, []byte{1}),
		mk(dbus.TypeMethodCall, "com.tesla.VCSECRemote", "com.tesla.VCSECRemote", authMember, []byte{1}),
		mk(dbus.TypeMethodCall, authName, authName, "Decrypt", []byte{1}),
		mk(dbus.TypeMethodCall, authName, authName, authMember, "text"),
		mk(dbus.TypeMethodCall, authName, authName, authMember, []byte{1}, []byte{2}),
	} {
		if replyBody(m) != nil {
			t.Errorf("took %v", m)
		}
	}
}

func privateDial(bus *bustest.Bus) func() (*dbus.Conn, error) {
	return func() (*dbus.Conn, error) {
		c, err := dbus.Dial(bus.Addr())
		if err != nil {
			return nil, err
		}
		if err := c.Auth(nil); err != nil {
			return nil, err
		}
		return c, c.Hello()
	}
}

// Only the router's current owner is believed: a spoofed RouteMessageBytes from another connection is ignored, and
// after the router restarts (a new unique name) its replies are delivered again.
func TestMonitorBelievesOnlyTheRouter(t *testing.T) {
	bus := bustest.Start(t)
	ac := bus.Conn(t)
	if err := ac.Export(fakeAuthd{}, "/Auth", authName); err != nil {
		t.Fatal(err)
	}
	own(t, ac, authName)
	router := bus.Conn(t)
	own(t, router, routerName)

	got := make(chan []byte, 8)
	up := make(chan bool, 8)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go (&Monitor{Dial: privateDial(bus), Deliver: func(b []byte) { got <- b }, SetUp: func(v bool) { up <- v },
		Log: log.New(io.Discard, "", 0)}).Run(ctx)
	if !<-up {
		t.Fatal("monitor down")
	}
	send := func(c *dbus.Conn, b []byte) {
		c.Object(authName, "/Auth").Call(authName+"."+authMember, dbus.FlagNoReplyExpected, b)
	}
	expect := func(want []byte, what string) {
		t.Helper()
		select {
		case b := <-got:
			if !bytes.Equal(b, want) {
				t.Fatalf("%s: delivered %x", what, b)
			}
		case <-time.After(3 * time.Second):
			t.Fatalf("%s: nothing delivered", what)
		}
	}
	spoofer := bus.Conn(t)
	send(spoofer, []byte{0xBA, 0xD0})
	send(router, []byte{1})
	expect([]byte{1}, "the router's reply") // the spoof (sent first) was not delivered

	// The router restarts as another connection.
	router.Close()
	router2 := bus.Conn(t)
	own(t, router2, routerName)
	time.Sleep(100 * time.Millisecond) // NameOwnerChanged reaches the monitor
	send(router2, []byte{2})
	expect([]byte{2}, "the restarted router's reply")
}

// The injector child redials when its bus connection drops.
func TestChildRedials(t *testing.T) {
	bus := bustest.Start(t)
	rc := bus.Conn(t)
	router := &fakeRouter{conn: rc, reply: []byte{0}}
	if err := rc.Export(router, routerPath, routerName); err != nil {
		t.Fatal(err)
	}
	own(t, rc, routerName)
	var mu sync.Mutex
	var conns []*dbus.Conn
	dial := func() (*dbus.Conn, error) {
		c, err := privateDial(bus)()
		if err == nil {
			mu.Lock()
			conns = append(conns, c)
			mu.Unlock()
		}
		return c, err
	}
	inR, inW := io.Pipe()
	outR, outW := io.Pipe()
	go RunChild(inR, outW, dial)
	r := bufio.NewReader(outR)
	call := func() byte {
		t.Helper()
		proto.WriteFrame(inW, childMsg, request(3, bytes.Repeat([]byte{1}, 16)))
		typ, _, err := proto.ReadFrame(r, 4096)
		if err != nil {
			t.Fatal(err)
		}
		return typ
	}
	if call() != childOK {
		t.Fatal("first call failed")
	}
	mu.Lock()
	conns[0].Close()
	mu.Unlock()
	if typ := call(); typ != childOK {
		t.Fatalf("after the drop: 0x%02x, want a redial and OK", typ)
	}
	mu.Lock()
	n := len(conns)
	mu.Unlock()
	if n != 2 {
		t.Fatalf("%d dials, want 2", n)
	}
	inW.Close()
}
