package dialout

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"math/rand/v2"
	"net"
	"os"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// The tests play the Pi with a raw loopback listener and spell the protocol
// bytes out, so they also pin the package's constants to the spec.

// piConn is a connection the raw Pi accepted, with the time it accepted it.
type piConn struct {
	net.Conn
	at time.Time
}

// rawPi is a loopback listener playing the Pi: each connection it accepts
// arrives on conns.
type rawPi struct {
	t     *testing.T
	addr  string
	conns chan piConn

	mu   sync.Mutex
	ln   net.Listener // nil while down
	open []net.Conn
}

func newRawPi(t *testing.T) *rawPi {
	t.Helper()
	p := &rawPi{t: t, conns: make(chan piConn, 1024)}
	p.listen("127.0.0.1:0")
	t.Cleanup(p.down)
	return p
}

func (p *rawPi) listen(addr string) {
	p.t.Helper()
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		p.t.Fatal(err)
	}
	p.mu.Lock()
	p.ln, p.addr = ln, ln.Addr().String()
	p.mu.Unlock()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			at := time.Now()
			p.mu.Lock()
			if p.ln != ln { // taken down meanwhile
				p.mu.Unlock()
				c.Close()
				return
			}
			p.open = append(p.open, c)
			p.mu.Unlock()
			p.conns <- piConn{c, at}
		}
	}()
}

// down closes the listener and every connection it accepted, as a Pi
// restart does.
func (p *rawPi) down() {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.ln != nil {
		p.ln.Close()
		p.ln = nil
	}
	for _, c := range p.open {
		c.Close()
	}
	p.open = nil
}

// up listens again on the same address.
func (p *rawPi) up() {
	p.t.Helper()
	p.listen(p.addr)
}

// next returns the next connection that reaches the Pi within d.
func (p *rawPi) next(d time.Duration) piConn {
	p.t.Helper()
	select {
	case c := <-p.conns:
		return c
	case <-time.After(d):
		p.t.Fatalf("no connection reached the Pi within %v", d)
		return piConn{}
	}
}

// none fails if another connection reaches the Pi within d.
func (p *rawPi) none(d time.Duration) {
	p.t.Helper()
	select {
	case <-p.conns:
		p.t.Fatalf("another connection reached the Pi within %v", d)
	case <-time.After(d):
	}
}

// start runs a Listener with cfg until the test ends; its log is discarded
// unless cfg sets one.
func start(t *testing.T, cfg Config) *Listener {
	t.Helper()
	if cfg.Log == nil {
		cfg.Log = log.New(io.Discard, "", 0)
	}
	l, err := Listen(cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { l.Close() })
	return l
}

func write(t *testing.T, c net.Conn, s string) {
	t.Helper()
	if _, err := c.Write([]byte(s)); err != nil {
		t.Fatal(err)
	}
}

// hello reads the car's 8-byte hello.
func hello(c net.Conn) error {
	c.SetReadDeadline(time.Now().Add(time.Second))
	defer c.SetReadDeadline(time.Time{})
	b := make([]byte, 8)
	if _, err := io.ReadFull(c, b); err != nil {
		return fmt.Errorf("reading the hello: %w", err)
	}
	if string(b) != "AGWDIAL1" {
		return fmt.Errorf("hello %q, want %q", b, "AGWDIAL1")
	}
	return nil
}

func readHello(t *testing.T, c net.Conn) {
	t.Helper()
	if err := hello(c); err != nil {
		t.Fatal(err)
	}
}

// awaitAck reads one byte at a time until A; only K may come before it.
func awaitAck(c net.Conn) error {
	c.SetReadDeadline(time.Now().Add(time.Second))
	defer c.SetReadDeadline(time.Time{})
	var b [1]byte
	for {
		if _, err := io.ReadFull(c, b[:]); err != nil {
			return fmt.Errorf("waiting for A: %w", err)
		}
		switch b[0] {
		case 'K':
		case 'A':
			return nil
		default:
			return fmt.Errorf("got %q before A", b[0])
		}
	}
}

// awaitEOF reads until EOF, for at most d; only K may come before it.
func awaitEOF(c net.Conn, d time.Duration) error {
	c.SetReadDeadline(time.Now().Add(d))
	defer c.SetReadDeadline(time.Time{})
	var b [1]byte
	for {
		_, err := io.ReadFull(c, b[:])
		switch {
		case errors.Is(err, io.EOF):
			return nil
		case err != nil:
			return fmt.Errorf("waiting for EOF: %w", err)
		case b[0] != 'K':
			return fmt.Errorf("got %q before EOF", b[0])
		}
	}
}

type accepted struct {
	c   net.Conn
	err error
	at  time.Time // when Accept returned
}

// acceptAsync runs one l.Accept in a goroutine.
func acceptAsync(l *Listener) <-chan accepted {
	ch := make(chan accepted, 1)
	go func() {
		c, err := l.Accept()
		ch <- accepted{c, err, time.Now()}
	}()
	return ch
}

// accept returns the next paired connection, which must come within d.
func accept(t *testing.T, l *Listener, d time.Duration) net.Conn {
	t.Helper()
	select {
	case a := <-acceptAsync(l):
		if a.err != nil {
			t.Fatalf("Accept: %v", a.err)
		}
		t.Cleanup(func() { a.c.Close() })
		return a.c
	case <-time.After(d):
		t.Fatalf("Accept returned nothing within %v", d)
		return nil
	}
}

func TestDefaults(t *testing.T) {
	// A nil Log is log.Default(), where the failed dial to port 1 goes: keep
	// it out of the test output. Close (a cleanup registered later, so run
	// first) waits for the dialer before the output is restored.
	prev := log.Writer()
	log.SetOutput(io.Discard)
	t.Cleanup(func() { log.SetOutput(prev) })
	l, err := Listen(Config{Addr: "127.0.0.1:1"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { l.Close() })

	got := l.cfg
	if got.Dial == nil {
		t.Error("Dial is nil, want net.Dialer's")
	}
	if got.Log != log.Default() {
		t.Error("Log is not log.Default()")
	}
	got.Dial, got.Log = nil, nil
	want := Config{
		Addr: "127.0.0.1:1", Idle: 3, DialsPerSecond: 4,
		BackoffMin: time.Second, BackoffMax: 10 * time.Second,
		KeepaliveEvery: 10 * time.Second, Silence: 25 * time.Second,
		DialTimeout: 3 * time.Second,
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("defaults:\n got %+v\nwant %+v", got, want)
	}
	if a := l.Addr().String(); a != "127.0.0.1:1" {
		t.Errorf("Addr() = %s, want 127.0.0.1:1", a)
	}
}

func TestListenNeedsLiteralIPAndPort(t *testing.T) {
	// The hook refuses every dial, so nothing is contacted even if Listen
	// wrongly accepted an address.
	refuse := func(context.Context, string, string) (net.Conn, error) { return nil, errors.New("refused") }
	for _, addr := range []string{"", "localhost:17101", "127.0.0.1", "127.0.0.1:port"} {
		l, err := Listen(Config{Addr: addr, Dial: refuse, Log: log.New(io.Discard, "", 0)})
		if err == nil {
			l.Close()
			t.Errorf("Listen(%q) succeeded, want an error", addr)
		}
	}
}

func TestKeepsIdleConnectionsWithHello(t *testing.T) {
	pi := newRawPi(t)
	start(t, Config{Addr: pi.addr})
	deadline := time.Now().Add(time.Second)
	for range 3 {
		readHello(t, pi.next(time.Until(deadline)))
	}
	pi.none(300 * time.Millisecond)
}

func TestPairingHandsOverExactlyThePhoneBytes(t *testing.T) {
	pi := newRawPi(t)
	// Keepalives every 2 ms, so the car's K's are in flight around the pairing.
	l := start(t, Config{Addr: pi.addr, Idle: 1, KeepaliveEvery: 2 * time.Millisecond, Silence: 100 * time.Millisecond})
	c := pi.next(time.Second)
	readHello(t, c)
	write(t, c, "K")
	write(t, c, "G")
	if err := awaitAck(c); err != nil {
		t.Fatal(err)
	}
	write(t, c, "phone-bytes")

	gw := accept(t, l, time.Second)
	// The paired connection carries none of the handshake's deadlines: use
	// it past Silence, bounded by a watchdog rather than a deadline of ours.
	time.Sleep(150 * time.Millisecond)
	watchdog := time.AfterFunc(time.Second, func() { gw.Close() })
	defer watchdog.Stop()
	got := make([]byte, len("phone-bytes"))
	if _, err := io.ReadFull(gw, got); err != nil || string(got) != "phone-bytes" {
		t.Fatalf("the paired connection's first read: %q, %v; want %q", got, err, "phone-bytes")
	}
	write(t, gw, "gw")
	c.SetReadDeadline(time.Now().Add(time.Second))
	got = make([]byte, 2)
	if _, err := io.ReadFull(c, got); err != nil || string(got) != "gw" {
		t.Fatalf("after A the Pi read %q, %v; want %q", got, err, "gw")
	}
}

// The protocol lets the Pi send the phone's first bytes right behind G, even
// in the same write (today's Pi waits for A), so they can reach the car
// together: the car takes only the G, and the paired connection's first read
// is exactly the phone's bytes. A buffered read in awaitGo would swallow them.
func TestGoAndPhoneBytesInOneWrite(t *testing.T) {
	pi := newRawPi(t)
	l := start(t, Config{Addr: pi.addr, Idle: 1})
	c := pi.next(time.Second)
	readHello(t, c)
	write(t, c, "Gphone-bytes")
	if err := awaitAck(c); err != nil {
		t.Fatal(err)
	}
	gw := accept(t, l, time.Second)
	gw.SetReadDeadline(time.Now().Add(time.Second))
	got := make([]byte, 64)
	n, err := gw.Read(got)
	if err != nil || string(got[:n]) != "phone-bytes" {
		t.Fatalf("the paired connection's first read: %q, %v; want %q", got[:n], err, "phone-bytes")
	}
}

func TestNoKeepaliveAfterAck(t *testing.T) {
	const iterations, workers = 200, 8
	pi := newRawPi(t)
	l := start(t, Config{Addr: pi.addr, Idle: workers, DialsPerSecond: 1000, KeepaliveEvery: time.Millisecond})
	go func() { // the server: holds each paired connection, silent, until the Pi closes it
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			go func() {
				io.Copy(io.Discard, c)
				c.Close()
			}()
		}
	}()
	var left atomic.Int32
	left.Store(iterations)
	var wg sync.WaitGroup
	for range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for left.Add(-1) >= 0 {
				var c piConn
				select {
				case c = <-pi.conns:
				case <-time.After(2 * time.Second):
					t.Error("no connection reached the Pi within 2 s")
					return
				}
				if err := quietAfterAck(c); err != nil {
					t.Error(err)
					return
				}
			}
		}()
	}
	wg.Wait()
}

// quietAfterAck sends G after a random 0-5 ms, reads up to the car's A, and
// then expects 20 ms with no byte from the car.
func quietAfterAck(c net.Conn) error {
	defer c.Close()
	if err := hello(c); err != nil {
		return err
	}
	time.Sleep(rand.N(5*time.Millisecond + 1))
	if _, err := c.Write([]byte("G")); err != nil {
		return err
	}
	if err := awaitAck(c); err != nil {
		return err
	}
	c.SetReadDeadline(time.Now().Add(20 * time.Millisecond))
	var b [16]byte
	n, err := c.Read(b[:])
	if n > 0 {
		return fmt.Errorf("the car sent %q after A", b[:n])
	}
	if !errors.Is(err, os.ErrDeadlineExceeded) {
		return fmt.Errorf("reading after A: %v, want 20 ms of silence", err)
	}
	return nil
}

// stagedConn is the car's first connection to the Pi, with its first
// keepalive write staged by the test: k runs in its place, under the car's
// write lock.
type stagedConn struct {
	net.Conn
	k          func(*stagedConn) error
	closeDelay time.Duration // Close takes effect only this long after the call

	kStarted, gotG, closed  chan struct{} // the staged K began; a read returned G; Close took effect
	kOnce, gOnce, closeOnce sync.Once

	mu       sync.Mutex
	deadline time.Time // the latest write deadline
}

// stageFirst returns sc and a Dial hook that wraps the car's first
// connection in sc and leaves the later ones alone.
func stageFirst(k func(*stagedConn) error, closeDelay time.Duration) (*stagedConn, func(context.Context, string, string) (net.Conn, error)) {
	sc := &stagedConn{k: k, closeDelay: closeDelay,
		kStarted: make(chan struct{}), gotG: make(chan struct{}), closed: make(chan struct{})}
	dials := 0 // only the supervisor dials, one at a time
	return sc, func(ctx context.Context, network, addr string) (net.Conn, error) {
		c, err := (&net.Dialer{}).DialContext(ctx, network, addr)
		if err != nil {
			return nil, err
		}
		if dials++; dials > 1 {
			return c, nil
		}
		sc.Conn = c
		return sc, nil
	}
}

func (sc *stagedConn) Read(b []byte) (int, error) {
	n, err := sc.Conn.Read(b)
	if bytes.IndexByte(b[:n], 'G') >= 0 {
		sc.gOnce.Do(func() { close(sc.gotG) })
	}
	return n, err
}

func (sc *stagedConn) Write(b []byte) (int, error) {
	staged := false
	if string(b) == "K" {
		sc.kOnce.Do(func() { staged = true })
	}
	if !staged {
		return sc.Conn.Write(b)
	}
	close(sc.kStarted)
	return 0, sc.k(sc)
}

func (sc *stagedConn) SetWriteDeadline(t time.Time) error {
	sc.mu.Lock()
	sc.deadline = t
	sc.mu.Unlock()
	return sc.Conn.SetWriteDeadline(t)
}

func (sc *stagedConn) Close() error {
	time.Sleep(sc.closeDelay)
	err := sc.Conn.Close()
	sc.closeOnce.Do(func() { close(sc.closed) })
	return err
}

// waitK waits for the staged keepalive write, which holds the car's write
// lock.
func (sc *stagedConn) waitK(t *testing.T) {
	t.Helper()
	select {
	case <-sc.kStarted:
	case <-time.After(time.Second):
		t.Fatal("no keepalive write within 1 s")
	}
}

// A keepalive write that fails closes the connection before the car can
// answer a G it has already read: the Pi gets no A, and Accept nothing.
func TestNoAckAfterFailedKeepalive(t *testing.T) {
	pi := newRawPi(t)
	sc, dial := stageFirst(func(sc *stagedConn) error {
		select {
		case <-sc.gotG: // the car has read the Pi's G, and its A waits for the write lock
			time.Sleep(10 * time.Millisecond)
		case <-sc.closed:
		case <-time.After(2 * time.Second):
		}
		return errors.New("staged: the keepalive write fails")
	}, 100*time.Millisecond) // and the close takes 100 ms to land
	l := start(t, Config{Addr: pi.addr, Idle: 1, KeepaliveEvery: 20 * time.Millisecond, Silence: time.Second, Dial: dial})
	acc := acceptAsync(l)
	c := pi.next(time.Second)
	readHello(t, c)
	sc.waitK(t)
	write(t, c, "G")
	if err := awaitEOF(c, time.Second); err != nil {
		t.Fatal(err)
	}
	select {
	case a := <-acc:
		t.Fatalf("Accept returned (%v, %v): a connection whose keepalive write had failed", a.c, a.err)
	case <-time.After(200 * time.Millisecond):
	}
}

// A connection lost before pairing while a keepalive write is stuck (a full
// send buffer) is closed at once, which ends the write: its slot is redialled
// after BackoffMin, not once the write's deadline passes.
func TestStuckKeepaliveDoesNotDelayRedial(t *testing.T) {
	pi := newRawPi(t)
	sc, dial := stageFirst(func(sc *stagedConn) error {
		sc.mu.Lock()
		deadline := sc.deadline
		sc.mu.Unlock()
		select {
		case <-sc.closed:
			return net.ErrClosed
		case <-time.After(time.Until(deadline)):
			return os.ErrDeadlineExceeded
		}
	}, 0)
	start(t, Config{Addr: pi.addr, Idle: 1, KeepaliveEvery: 20 * time.Millisecond, Silence: time.Second,
		BackoffMin: 50 * time.Millisecond, Dial: dial})
	c := pi.next(time.Second)
	readHello(t, c)
	sc.waitK(t)
	write(t, c, "X") // ends the connection, with the K stuck until Silence (1 s)
	readHello(t, pi.next(500*time.Millisecond))
}

// syncBuf is a Listener's log, read by the test while the Listener writes it.
type syncBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuf) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuf) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

// waitLog waits up to 1 s for cond to hold on the log.
func waitLog(t *testing.T, logs *syncBuf, what string, cond func(string) bool) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for !cond(logs.String()) {
		if time.Now().After(deadline) {
			t.Fatalf("waiting for %s; the log:\n%s", what, logs)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// A Pi that takes each connection and closes it at once makes the car lose
// them as fast as it redials. The car logs the first loss of the streak and
// counts the rest, and logs the count once the Pi keeps a connection again
// (a K from it); the next loss starts a new streak.
func TestLossesLoggedOncePerStreak(t *testing.T) {
	const closed = 9
	pi := newRawPi(t)
	logs := &syncBuf{}
	start(t, Config{Addr: pi.addr, DialsPerSecond: 100, BackoffMin: 10 * time.Millisecond, Log: log.New(logs, "", 0)})
	for range closed {
		c := pi.next(time.Second)
		readHello(t, c)
		c.Close()
	}
	// The next 3 are kept. Each of the 3 slots is dialled again only once
	// its loss is counted, so all 9 losses are counted by now.
	var kept []piConn
	for range 3 {
		c := pi.next(time.Second)
		readHello(t, c)
		kept = append(kept, c)
	}
	lossLines := func(s string) int { return strings.Count(s, "ended before pairing:") }
	// The first loss is logged just after it is counted.
	waitLog(t, logs, "the first loss", func(s string) bool { return lossLines(s) >= 1 })

	write(t, kept[0], "K")
	waitLog(t, logs, "the count of the streak", func(s string) bool {
		return strings.Contains(s, fmt.Sprintf("keeps a connection again, after %d ended before pairing", closed))
	})
	if n := lossLines(logs.String()); n != 1 {
		t.Fatalf("%d loss lines for a streak of %d losses, want 1:\n%s", n, closed, logs)
	}

	kept[1].Close()
	waitLog(t, logs, "the first loss of a new streak", func(s string) bool { return lossLines(s) == 2 })
}

func TestSilenceClosesAndRedials(t *testing.T) {
	pi := newRawPi(t)
	start(t, Config{Addr: pi.addr, Idle: 1, Silence: 300 * time.Millisecond, BackoffMin: 150 * time.Millisecond})
	c := pi.next(3 * time.Second)
	readHello(t, c)
	if err := awaitEOF(c, 3*time.Second); err != nil {
		t.Fatal(err)
	}
	// Timed from the accept: the car sends its hello as soon as it connects.
	if d := time.Since(c.at); d < 240*time.Millisecond || d > 900*time.Millisecond {
		t.Fatalf("the car closed %v after its hello, want 240-900 ms", d)
	}
	readHello(t, pi.next(3*time.Second))
}

func TestPeerKeepalivesHoldTheConnection(t *testing.T) {
	pi := newRawPi(t)
	start(t, Config{Addr: pi.addr, Idle: 1, Silence: 300 * time.Millisecond})
	c := pi.next(3 * time.Second)
	readHello(t, c)
	stop := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() { // the Pi's keepalives
		defer wg.Done()
		tick := time.NewTicker(90 * time.Millisecond)
		defer tick.Stop()
		for {
			select {
			case <-stop:
				return
			case <-tick.C:
				c.Write([]byte("K"))
			}
		}
	}()
	err := awaitEOF(c, 1500*time.Millisecond)
	close(stop)
	wg.Wait()
	if !errors.Is(err, os.ErrDeadlineExceeded) {
		t.Fatalf("within 1.5 s: %v; want the connection still open", err)
	}
}

func TestSendsKeepalivesWhileIdle(t *testing.T) {
	pi := newRawPi(t)
	start(t, Config{Addr: pi.addr, Idle: 1, KeepaliveEvery: 20 * time.Millisecond, Silence: time.Second})
	c := pi.next(time.Second)
	readHello(t, c)
	// The Pi only listens, for 200 ms: the car sends K every 20 ms, and
	// nothing else.
	c.SetReadDeadline(time.Now().Add(200 * time.Millisecond))
	n := 0
	b := make([]byte, 64)
	for {
		m, err := c.Read(b)
		for _, x := range b[:m] {
			if x != 'K' {
				t.Fatalf("the car sent %q while idle, want only K", x)
			}
		}
		n += m
		if errors.Is(err, os.ErrDeadlineExceeded) {
			break
		}
		if err != nil {
			t.Fatalf("after %d K: %v", n, err)
		}
	}
	// About 10; at least 5 leaves room for a loaded machine.
	if n < 5 {
		t.Fatalf("%d K in 200 ms, want about 10 (at least 5)", n)
	}
	t.Logf("%d K in 200 ms", n)
}

func TestUnexpectedByteCloses(t *testing.T) {
	pi := newRawPi(t)
	l := start(t, Config{Addr: pi.addr, Idle: 1, BackoffMin: 50 * time.Millisecond})
	c := pi.next(time.Second)
	readHello(t, c)
	acc := acceptAsync(l)
	sent := time.Now()
	write(t, c, "X")
	if err := awaitEOF(c, time.Second); err != nil {
		t.Fatal(err)
	}
	select {
	case a := <-acc:
		t.Fatalf("Accept returned (%v, %v) after X", a.c, a.err)
	case <-time.After(200 * time.Millisecond):
	}
	r := pi.next(time.Second)
	readHello(t, r)
	// The car closed only after reading the X, and replaces a connection
	// lost before pairing only BackoffMin later.
	if d := r.at.Sub(sent); d < 50*time.Millisecond {
		t.Fatalf("the replacement came %v after the X, want at least BackoffMin (50 ms)", d)
	}
}

func TestDialRateCapAndBackoff(t *testing.T) {
	t.Run("backoff", func(t *testing.T) {
		dials := make(chan time.Time, 64)
		refuse := func(context.Context, string, string) (net.Conn, error) {
			dials <- time.Now()
			return nil, errors.New("refused")
		}
		// The rate cap is lifted, so only the backoff spaces the dials.
		start(t, Config{Addr: "127.0.0.1:9", Dial: refuse, DialsPerSecond: 100,
			BackoffMin: 50 * time.Millisecond, BackoffMax: 200 * time.Millisecond})
		var at []time.Time
		for len(at) < 7 {
			select {
			case d := <-dials:
				at = append(at, d)
			case <-time.After(time.Second):
				t.Fatalf("only %d dials", len(at))
			}
		}
		const slack = 150 * time.Millisecond
		for i, ms := range []time.Duration{50, 100, 200, 200, 200, 200} {
			want := ms * time.Millisecond
			if gap := at[i+1].Sub(at[i]); gap < want || gap >= want+slack {
				t.Errorf("gap %d: %v, want %v (under %v)", i+1, gap, want, want+slack)
			}
		}
	})
	t.Run("a success resets the backoff", func(t *testing.T) {
		pi := newRawPi(t)
		dials := make(chan time.Time, 64)
		n := 0 // only the supervisor dials, one at a time
		hook := func(ctx context.Context, network, addr string) (net.Conn, error) {
			dials <- time.Now()
			if n++; n == 4 { // dials 1-3 fail, dial 4 connects, every later one fails
				return (&net.Dialer{}).DialContext(ctx, network, addr)
			}
			return nil, errors.New("refused")
		}
		start(t, Config{Addr: pi.addr, Dial: hook, DialsPerSecond: 100,
			BackoffMin: 50 * time.Millisecond, BackoffMax: 400 * time.Millisecond})
		var at []time.Time
		for len(at) < 6 {
			select {
			case d := <-dials:
				at = append(at, d)
			case <-time.After(time.Second):
				t.Fatalf("only %d dials", len(at))
			}
		}
		// Dial 5 fails right after the success: dial 6 waits BackoffMin
		// again, not the 400 ms that the doubling had reached.
		const want, slack = 50 * time.Millisecond, 150 * time.Millisecond
		if gap := at[5].Sub(at[4]); gap < want || gap >= want+slack {
			t.Errorf("after the success the backoff was %v, want %v (under %v)", gap, want, want+slack)
		}
	})
	t.Run("rate cap", func(t *testing.T) {
		pi := newRawPi(t)
		began := time.Now()
		l := start(t, Config{Addr: pi.addr})
		go func() { // the server: takes each paired connection and drops it
			for {
				c, err := l.Accept()
				if err != nil {
					return
				}
				c.Close()
			}
		}()
		// Count the dials by when the Pi accepted them, not by when this loop
		// got to them (CR-3): a loop descheduled past the window's end could
		// otherwise count a later dial. It keeps draining for a while past the
		// end, so an in-window dial whose hand-over to this loop ran late still
		// counts.
		n, late := 0, 0
		window := began.Add(2 * time.Second)
		drained := time.After(time.Until(window.Add(500 * time.Millisecond)))
	count:
		for {
			select {
			case c := <-pi.conns:
				if c.at.Before(window) {
					n++
				} else {
					late++
				}
				go func() { // the Pi pairs every connection at once
					if hello(c) == nil {
						c.Write([]byte("G"))
					}
				}()
			case <-drained:
				break count
			}
		}
		if n < 8 || n > 9 {
			t.Fatalf("%d dials in 2 s, want 8 or 9 (at most 4 in any second); %d more came after", n, late)
		}
	})
}

func TestRefillAfterHandout(t *testing.T) {
	pi := newRawPi(t)
	start(t, Config{Addr: pi.addr})
	var idle []piConn
	for range 3 {
		c := pi.next(time.Second)
		readHello(t, c)
		idle = append(idle, c)
	}
	write(t, idle[0], "G")
	if err := awaitAck(idle[0]); err != nil {
		t.Fatal(err)
	}
	readHello(t, pi.next(100*time.Millisecond))
}

func TestCloseUnblocksAcceptAndClosesConnections(t *testing.T) {
	pi := newRawPi(t)
	l := start(t, Config{Addr: pi.addr})
	var idle []piConn
	for range 3 {
		c := pi.next(time.Second)
		readHello(t, c)
		idle = append(idle, c)
	}
	acc := acceptAsync(l)
	time.Sleep(20 * time.Millisecond) // Accept is blocked by now
	closing := time.Now()
	if err := l.Close(); err != nil {
		t.Fatal(err)
	}
	if d := time.Since(closing); d > time.Second {
		t.Fatalf("Close took %v", d)
	}
	select {
	case a := <-acc:
		if !errors.Is(a.err, net.ErrClosed) {
			t.Fatalf("the blocked Accept returned (%v, %v), want net.ErrClosed", a.c, a.err)
		}
		if d := a.at.Sub(closing); d > 100*time.Millisecond {
			t.Fatalf("the blocked Accept returned %v after Close, want within 100 ms", d)
		}
	case <-time.After(time.Second):
		t.Fatal("Accept still blocked 1 s after Close")
	}
	for i, c := range idle {
		if err := awaitEOF(c, time.Until(closing.Add(time.Second))); err != nil {
			t.Errorf("idle connection %d, within 1 s of Close: %v", i, err)
		}
	}
	if _, err := l.Accept(); !errors.Is(err, net.ErrClosed) {
		t.Errorf("Accept after Close: %v, want net.ErrClosed", err)
	}
	if err := l.Close(); err != nil {
		t.Errorf("second Close: %v", err)
	}
}

func TestPiRestart(t *testing.T) {
	for _, tc := range []struct {
		name           string
		dialsPerSecond int           // 0: the default cap, 4
		within         time.Duration // for the 3 idle connections, from the Pi's return
	}{
		// The first dials are only milliseconds old when the Pi restarts here
		// (on the car they are long past), so the rate cap is lifted and the
		// backoff alone paces the redials.
		{"backoff alone", 100, time.Second},
		// The shipped cap: the first 3 dials and the failed redial fill the
		// 1 s window, which holds the redials back until about 850 ms after
		// the Pi's return. 1.5 s leaves room for a loaded machine.
		{"default rate cap", 0, 1500 * time.Millisecond},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pi := newRawPi(t)
			start(t, Config{Addr: pi.addr, DialsPerSecond: tc.dialsPerSecond,
				BackoffMin: 50 * time.Millisecond, BackoffMax: 200 * time.Millisecond})
			for range 3 {
				readHello(t, pi.next(time.Second))
			}
			pi.down()
			time.Sleep(150 * time.Millisecond)
			pi.up()
			deadline := time.Now().Add(tc.within)
			for range 3 {
				readHello(t, pi.next(time.Until(deadline)))
			}
		})
	}
}
