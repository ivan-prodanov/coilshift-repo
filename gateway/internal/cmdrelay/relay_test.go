package cmdrelay

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"airgapp/gateway/camhelper/proto"
)

type fakeInjector struct {
	mu   sync.Mutex
	msgs [][]byte
	err  error
	got  chan []byte
}

func (f *fakeInjector) Inject(_ context.Context, msg []byte) error {
	f.mu.Lock()
	f.msgs = append(f.msgs, append([]byte(nil), msg...))
	err := f.err
	f.mu.Unlock()
	if f.got != nil {
		f.got <- append([]byte(nil), msg...)
	}
	return err
}

func startRelay(t *testing.T, inj Injector, up bool, uid int) (*Server, string) {
	t.Helper()
	dir, err := os.MkdirTemp("", "cr")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	sock := filepath.Join(dir, "s")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	srv := New(Config{AllowUID: uid, Inject: inj, Log: log.New(io.Discard, "", 0)})
	srv.SetMonitorUp(up)
	go srv.Serve(ln)
	t.Cleanup(func() { srv.Shutdown(); ln.Close() })
	return srv, sock
}

type client struct {
	c net.Conn
	r *bufio.Reader
}

func dial(t *testing.T, sock string) *client {
	t.Helper()
	c, err := net.Dial("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	return &client{c, bufio.NewReader(c)}
}

func (c *client) read(t *testing.T) (byte, []byte) {
	t.Helper()
	c.c.SetReadDeadline(time.Now().Add(3 * time.Second))
	typ, body, err := proto.ReadFrame(c.r, 1+MaxMsg)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	return typ, body
}

func (c *client) send(t *testing.T, msg []byte) {
	t.Helper()
	if err := proto.WriteFrame(c.c, TMsg, msg); err != nil {
		t.Fatal(err)
	}
}

// fromAddress is the from routing address of an injected message.
func fromAddress(t *testing.T, msg []byte) []byte {
	t.Helper()
	fs, err := fields(msg)
	if err != nil {
		t.Fatal(err)
	}
	d, _, err := dest(msg, fs, fieldFrom)
	if err != nil || !d.HasAddress {
		t.Fatalf("injected message has no from address: %v", err)
	}
	return d.RoutingAddress
}

func TestRoundTrip(t *testing.T) {
	inj := &fakeInjector{got: make(chan []byte, 4)}
	srv, sock := startRelay(t, inj, true, os.Getuid())
	c := dial(t, sock)
	if typ, _ := c.read(t); typ != TReady {
		t.Fatalf("first frame 0x%02x, want READY", typ)
	}
	phone := bytes.Repeat([]byte{0x11}, 16)
	c.send(t, request(DomainInfotainment, phone))
	injected := <-inj.got
	car := fromAddress(t, injected)
	if bytes.Equal(car, phone) || len(car) != 16 {
		t.Fatalf("the phone's address was not replaced: %x", car)
	}
	if !bytes.Equal(injected, request(DomainInfotainment, car)) {
		t.Fatal("anything but the from address changed")
	}
	// Replies to other addresses are ignored; ours comes back with the phone's address.
	srv.Deliver(reply(bytes.Repeat([]byte{0x33}, 16), DomainInfotainment))
	srv.Deliver([]byte{0xff, 0xff})
	srv.Deliver(reply(car, DomainInfotainment))
	typ, body := c.read(t)
	if typ != TReply || !bytes.Equal(body, reply(phone, DomainInfotainment)) {
		t.Fatalf("reply 0x%02x %x", typ, body)
	}
	// The same phone address keeps its car-side address; the VCSEC domain goes through too.
	c.send(t, request(DomainVehicleSecurity, phone))
	if got := fromAddress(t, <-inj.got); !bytes.Equal(got, car) {
		t.Fatal("the car-side address changed within a session")
	}
}

func TestSessionsAreIsolated(t *testing.T) {
	inj := &fakeInjector{got: make(chan []byte, 4)}
	srv, sock := startRelay(t, inj, true, os.Getuid())
	a, b := dial(t, sock), dial(t, sock)
	a.read(t)
	b.read(t)
	phone := bytes.Repeat([]byte{0x11}, 16) // both phones use the same address
	a.send(t, request(DomainInfotainment, phone))
	carA := fromAddress(t, <-inj.got)
	b.send(t, request(DomainInfotainment, phone))
	carB := fromAddress(t, <-inj.got)
	if bytes.Equal(carA, carB) {
		t.Fatal("two sessions share a car-side address")
	}
	srv.Deliver(reply(carB, DomainInfotainment))
	if typ, _ := b.read(t); typ != TReply {
		t.Fatal("b did not get its reply")
	}
	// a must not get b's reply: it gets its own next.
	srv.Deliver(reply(carA, DomainInfotainment))
	if typ, body := a.read(t); typ != TReply || !bytes.Equal(body, reply(phone, DomainInfotainment)) {
		t.Fatal("a got the wrong frame")
	}
	// After b closes, its address is forgotten.
	b.c.Close()
	time.Sleep(50 * time.Millisecond)
	srv.mu.Lock()
	_, still := srv.byCar[string(carB)]
	srv.mu.Unlock()
	if still {
		t.Fatal("a closed session's address is still routed")
	}
}

func TestRefusals(t *testing.T) {
	inj := &fakeInjector{}
	_, sock := startRelay(t, inj, true, os.Getuid())
	c := dial(t, sock)
	c.read(t)
	phone := bytes.Repeat([]byte{1}, 16)
	for _, m := range [][]byte{request(5, phone), request(0, phone)} {
		c.send(t, m)
		if typ, body := c.read(t); typ != TErr || !isErr(body, "refused") {
			t.Fatalf("got 0x%02x %q, want ERR refused with the uuid", typ, body)
		}
	}
	c.send(t, []byte{0xff}) // no uuid to quote
	if typ, body := c.read(t); typ != TErr || string(body) != "refused" {
		t.Fatalf("got 0x%02x %q, want a bare ERR refused", typ, body)
	}
	inj.mu.Lock()
	n := len(inj.msgs)
	inj.mu.Unlock()
	if n != 0 {
		t.Fatalf("injected %d refused messages", n)
	}
}

func TestRateLimit(t *testing.T) {
	inj := &fakeInjector{}
	_, sock := startRelay(t, inj, true, os.Getuid())
	c := dial(t, sock)
	c.read(t)
	phone := bytes.Repeat([]byte{1}, 16)
	for i := 0; i < rateBurst+10; i++ {
		c.send(t, request(3, phone))
	}
	if typ, body := c.read(t); typ != TErr || !isErr(body, "rate") {
		t.Fatalf("0x%02x %q, want ERR rate", typ, body)
	}
}

func TestInjectFailureAndMonitorDown(t *testing.T) {
	inj := &fakeInjector{err: errors.New("bus down")}
	srv, sock := startRelay(t, inj, true, os.Getuid())
	c := dial(t, sock)
	c.read(t)
	c.send(t, request(3, bytes.Repeat([]byte{1}, 16)))
	if typ, body := c.read(t); typ != TErr || !isErr(body, "unknown") {
		t.Fatalf("0x%02x %q, want ERR unknown (it may have run)", typ, body)
	}
	inj.mu.Lock()
	inj.err = fmt.Errorf("child gone: %w", ErrNotSent)
	inj.mu.Unlock()
	c.send(t, request(3, bytes.Repeat([]byte{1}, 16)))
	if typ, body := c.read(t); typ != TErr || !isErr(body, "busy") {
		t.Fatalf("0x%02x %q, want ERR busy (not sent)", typ, body)
	}
	srv.SetMonitorUp(false)
	c.send(t, request(3, bytes.Repeat([]byte{1}, 16)))
	if typ, body := c.read(t); typ != TErr || !isErr(body, "monitor") {
		t.Fatalf("0x%02x %q, want ERR monitor", typ, body)
	}
}

func TestNoMonitorNoSession(t *testing.T) {
	_, sock := startRelay(t, &fakeInjector{}, false, os.Getuid())
	c := dial(t, sock)
	c.c.SetReadDeadline(time.Now().Add(readyWait + 2*time.Second))
	if _, _, err := proto.ReadFrame(c.r, 1+MaxMsg); err == nil {
		t.Fatal("got a frame without a monitor")
	}
}

func TestWrongUIDRefused(t *testing.T) {
	_, sock := startRelay(t, &fakeInjector{}, true, os.Getuid()+1)
	c := dial(t, sock)
	c.c.SetReadDeadline(time.Now().Add(time.Second))
	if _, _, err := proto.ReadFrame(c.r, 1+MaxMsg); err == nil {
		t.Fatal("a wrong uid got a frame")
	}
}

func TestUnknownFrameCloses(t *testing.T) {
	_, sock := startRelay(t, &fakeInjector{}, true, os.Getuid())
	c := dial(t, sock)
	c.read(t)
	proto.WriteFrame(c.c, 0x09, []byte{1})
	c.c.SetReadDeadline(time.Now().Add(time.Second))
	if _, _, err := proto.ReadFrame(c.r, 1+MaxMsg); err == nil {
		t.Fatal("session survived an unknown frame")
	}
}

// isErr: an ERR body is the reason, then 0x00 and the message uuid (request() carries one).
func isErr(body []byte, reason string) bool {
	return bytes.Equal(body, append(append([]byte(reason), 0), bytes.Repeat([]byte{0xAB}, 16)...))
}

func TestAddressCapEvictsTheOldest(t *testing.T) {
	inj := &fakeInjector{got: make(chan []byte, maxAddresses+2)}
	srv, sock := startRelay(t, inj, true, os.Getuid())
	c := dial(t, sock)
	c.read(t)
	var cars [][]byte
	for i := 0; i <= maxAddresses; i++ { // one more than the cap
		c.send(t, request(3, bytes.Repeat([]byte{byte(i + 10)}, 16)))
		cars = append(cars, fromAddress(t, <-inj.got))
	}
	srv.mu.Lock()
	_, oldest := srv.byCar[string(cars[0])]
	_, newest := srv.byCar[string(cars[maxAddresses])]
	srv.mu.Unlock()
	if oldest || !newest {
		t.Fatalf("oldest still routed %v, newest routed %v", oldest, newest)
	}
	// The newest address still gets its reply.
	srv.Deliver(reply(cars[maxAddresses], 3))
	if typ, body := c.read(t); typ != TReply || !bytes.Equal(body, reply(bytes.Repeat([]byte{byte(maxAddresses + 10)}, 16), 3)) {
		t.Fatalf("0x%02x %x", typ, body)
	}
}

func TestMessageUUID(t *testing.T) {
	if u := MessageUUID(request(3, []byte{1})); !bytes.Equal(u, bytes.Repeat([]byte{0xAB}, 16)) {
		t.Fatalf("uuid %x", u)
	}
	if MessageUUID(fBytes(fieldTo, domainDest(3))) != nil || MessageUUID([]byte{0xff}) != nil {
		t.Fatal("a uuid from nothing")
	}
}
