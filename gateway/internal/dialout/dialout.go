// Package dialout is agw's listener behind the car's Wi-Fi NAT. The car's
// Wi-Fi module drops every inbound packet, so agw dials out to the Pi and
// keeps a few connections idle there; the Pi pairs each one with a phone, and
// Accept returns a connection only once it is paired. The server core sees an
// ordinary net.Listener, so its pre-auth clock starts at the pairing.
//
// Before pairing, a connection carries only this protocol (the car-dials-out
// design, 2026-11-21):
//   - The car sends Hello as soon as it connects.
//   - While idle, each side sends KeepaliveByte every 10 s, and each closes the
//     connection after 25 s with nothing heard.
//   - The Pi pairs the connection with GoByte, and the car answers AckByte.
//     Every byte after GoByte is the phone's; every byte after AckByte is the
//     gateway's.
//   - Any other byte closes the connection.
//
// The car reads one byte at a time until GoByte, so it never takes a phone
// byte, and it sends no KeepaliveByte after AckByte.
package dialout

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/netip"
	"os"
	"slices"
	"sync"
	"time"
)

// Hello is the 8 bytes the car sends first on every connection.
const Hello = "AGWDIAL1"

// The control bytes before pairing.
const (
	KeepaliveByte byte = 'K' // either way, while idle
	GoByte        byte = 'G' // Pi to car: paired; the phone's bytes follow
	AckByte       byte = 'A' // car to Pi: the gateway's bytes follow
)

// Config configures a Listener. Zero values take the defaults.
type Config struct {
	Addr           string // the Pi's car port, literal ip:port
	Idle           int    // connections kept dialling or idle; default 3
	DialsPerSecond int    // at most this many dials start in any 1 s; default 4
	// After a failed dial the next one waits BackoffMin, doubling with each
	// further failure up to BackoffMax; defaults 1 s and 10 s.
	BackoffMin, BackoffMax time.Duration
	// While idle, a K goes out every KeepaliveEvery, and the connection is
	// closed after Silence with nothing heard; defaults 10 s and 25 s.
	KeepaliveEvery, Silence time.Duration
	DialTimeout             time.Duration // default 3 s
	// Dial connects to the Pi; nil means a net.Dialer. It must return soon
	// after ctx is done.
	Dial func(ctx context.Context, network, addr string) (net.Conn, error)
	Log  *log.Logger // nil means log.Default()
}

// Listener keeps Config.Idle connections dialled out to the Pi and returns
// each one from Accept once the Pi has paired it.
type Listener struct {
	cfg  Config
	addr *net.TCPAddr

	ready  chan net.Conn   // paired connections for Accept; capacity cfg.Idle
	done   chan struct{}   // closed by Close
	ctx    context.Context // cancelled by Close: ends a dial in progress
	cancel context.CancelFunc
	wake   chan struct{}  // capacity 1: a slot fell vacant
	once   sync.Once      // Close
	wg     sync.WaitGroup // the supervisor and the connections' goroutines

	mu     sync.Mutex
	closed bool
	conns  map[*conn]struct{} // dialled and not yet paired
	vacant []time.Time        // one per empty slot, earliest first: when it may be dialled
	lost   int                // connections lost before pairing since the Pi last kept one
}

var _ net.Listener = (*Listener)(nil)

// Listen starts keeping cfg.Idle connections to the Pi at cfg.Addr, dialling
// at once. It fails only if cfg.Addr is not a literal ip:port.
func Listen(cfg Config) (*Listener, error) {
	ap, err := netip.ParseAddrPort(cfg.Addr)
	if err != nil {
		return nil, fmt.Errorf("dialout: the Pi's address must be a literal ip:port: %w", err)
	}
	if cfg.Idle <= 0 {
		cfg.Idle = 3
	}
	if cfg.DialsPerSecond <= 0 {
		cfg.DialsPerSecond = 4
	}
	if cfg.BackoffMin <= 0 {
		cfg.BackoffMin = time.Second
	}
	if cfg.BackoffMax <= 0 {
		cfg.BackoffMax = 10 * time.Second
	}
	cfg.BackoffMax = max(cfg.BackoffMax, cfg.BackoffMin)
	if cfg.KeepaliveEvery <= 0 {
		cfg.KeepaliveEvery = 10 * time.Second
	}
	if cfg.Silence <= 0 {
		cfg.Silence = 25 * time.Second
	}
	if cfg.DialTimeout <= 0 {
		cfg.DialTimeout = 3 * time.Second
	}
	if cfg.Dial == nil {
		cfg.Dial = (&net.Dialer{}).DialContext
	}
	if cfg.Log == nil {
		cfg.Log = log.Default()
	}
	ctx, cancel := context.WithCancel(context.Background())
	l := &Listener{
		cfg:    cfg,
		addr:   net.TCPAddrFromAddrPort(ap),
		ready:  make(chan net.Conn, cfg.Idle),
		done:   make(chan struct{}),
		ctx:    ctx,
		cancel: cancel,
		wake:   make(chan struct{}, 1),
		conns:  map[*conn]struct{}{},
		vacant: make([]time.Time, cfg.Idle), // the zero time: dial at once
	}
	l.wg.Add(1)
	go l.supervise()
	return l, nil
}

// Accept returns the next connection the Pi has paired, as Dial returned it:
// its next byte is the phone's. After Close it returns net.ErrClosed.
func (l *Listener) Accept() (net.Conn, error) {
	select {
	case nc := <-l.ready:
		select {
		case <-l.done: // Close has begun, and its drain may have missed nc
			nc.Close()
			return nil, net.ErrClosed
		default:
			return nc, nil
		}
	case <-l.done:
		return nil, net.ErrClosed
	}
}

// Close stops dialling, closes the idle and handshaking connections and any
// paired one not yet accepted, and waits for the Listener's goroutines.
// It is idempotent.
func (l *Listener) Close() error {
	l.once.Do(func() {
		l.mu.Lock()
		l.closed = true
		conns := make([]*conn, 0, len(l.conns))
		for c := range l.conns {
			conns = append(conns, c)
		}
		l.mu.Unlock()
		l.cancel()
		close(l.done)
		for _, c := range conns {
			c.nc.Close()
		}
		l.wg.Wait()
		for {
			select {
			case nc := <-l.ready:
				nc.Close()
			default:
				return
			}
		}
	})
	return nil
}

// Addr is the Pi's address.
func (l *Listener) Addr() net.Addr { return l.addr }

// supervise keeps cfg.Idle connections dialling or idle, dialling one at a
// time. A slot whose connection was handed out is dialled again at once, and
// one whose connection was lost before pairing after BackoffMin. No more than
// DialsPerSecond dials start in any second, and failed dials back off from
// BackoffMin, doubling up to BackoffMax, until one succeeds.
func (l *Listener) supervise() {
	defer l.wg.Done()
	var (
		recent []time.Time   // when the latest dials (at most DialsPerSecond) began
		delay  time.Duration // the wait after the latest failed dial; 0 after a success
		retry  time.Time     // no dial before this
		failed int           // failed dials in a row
	)
	for {
		at, ok := l.nextVacancy()
		if !ok { // every slot is filled
			select {
			case <-l.wake:
				continue
			case <-l.done:
				return
			}
		}
		if at.Before(retry) {
			at = retry
		}
		if len(recent) == l.cfg.DialsPerSecond {
			if free := recent[0].Add(time.Second); at.Before(free) {
				at = free
			}
		}
		if wait := time.Until(at); wait > 0 {
			t := time.NewTimer(wait)
			select {
			case <-t.C:
			case <-l.wake: // a slot fell vacant, perhaps due sooner
			case <-l.done:
				t.Stop()
				return
			}
			t.Stop()
			continue
		}

		recent = append(recent, time.Now())
		if len(recent) > l.cfg.DialsPerSecond {
			recent = recent[1:]
		}
		nc, err := l.dial()
		if l.ctx.Err() != nil { // closed
			if err == nil {
				nc.Close()
			}
			return
		}
		if err != nil {
			delay = min(max(2*delay, l.cfg.BackoffMin), l.cfg.BackoffMax)
			retry = time.Now().Add(delay)
			if failed++; failed == 1 {
				l.cfg.Log.Printf("agw: cannot reach the Pi: %v (retrying with backoff)", err)
			}
			continue
		}
		if failed > 0 {
			l.cfg.Log.Printf("agw: reached the Pi at %s after %d failed dials", l.addr, failed)
		}
		delay, retry, failed = 0, time.Time{}, 0
		l.add(nc)
	}
}

func (l *Listener) dial() (net.Conn, error) {
	ctx, cancel := context.WithTimeout(l.ctx, l.cfg.DialTimeout)
	defer cancel()
	return l.cfg.Dial(ctx, "tcp", l.addr.String())
}

// nextVacancy returns when the earliest empty slot may be dialled; false if
// every slot is filled.
func (l *Listener) nextVacancy() (time.Time, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if len(l.vacant) == 0 {
		return time.Time{}, false
	}
	return l.vacant[0], true
}

// vacateLocked records an empty slot that may be dialled from at on, and
// wakes the supervisor. Caller holds mu.
func (l *Listener) vacateLocked(at time.Time) {
	i := slices.IndexFunc(l.vacant, func(v time.Time) bool { return v.After(at) })
	if i < 0 {
		i = len(l.vacant)
	}
	l.vacant = slices.Insert(l.vacant, i, at)
	select {
	case l.wake <- struct{}{}:
	default:
	}
}

// add puts nc in the earliest empty slot and starts its handshake.
func (l *Listener) add(nc net.Conn) {
	c := &conn{l: l, nc: nc, stop: make(chan struct{})}
	l.mu.Lock()
	if l.closed {
		l.mu.Unlock()
		nc.Close()
		return
	}
	l.conns[c] = struct{}{}
	if len(l.vacant) > 0 {
		l.vacant = l.vacant[1:]
	}
	l.wg.Add(1)
	l.mu.Unlock()
	go c.run()
}

// lose drops c, which ended before pairing; its slot is dialled again after
// BackoffMin. It closes c first: that ends a keepalive write stuck under wmu,
// which stopKeepalives would otherwise wait for. Only the first loss of a
// streak is logged (a Pi that takes each connection and closes it would
// otherwise log one a second per slot); kept logs the streak's count.
func (l *Listener) lose(c *conn, err error) {
	c.nc.Close()
	c.stopKeepalives()
	l.mu.Lock()
	delete(l.conns, c)
	first := false
	if !l.closed {
		l.vacateLocked(time.Now().Add(l.cfg.BackoffMin))
		l.lost++
		first = l.lost == 1
	}
	l.mu.Unlock()
	if first {
		l.cfg.Log.Printf("agw: a connection to the Pi at %s ended before pairing: %v (any more are counted until the Pi keeps one)", l.addr, err)
	}
}

// kept records that the Pi keeps a connection (it sent the connection's first
// K, or G): it ends a streak of losses and logs how many there were.
func (l *Listener) kept() {
	l.mu.Lock()
	n := l.lost
	l.lost = 0
	l.mu.Unlock()
	if n > 0 {
		l.cfg.Log.Printf("agw: the Pi at %s keeps a connection again, after %d ended before pairing", l.addr, n)
	}
}

// handOut gives the paired c to Accept, or closes it after Close; its slot
// is dialled again at once.
func (l *Listener) handOut(c *conn) {
	c.nc.SetDeadline(time.Time{})
	l.mu.Lock()
	delete(l.conns, c)
	if !l.closed {
		l.vacateLocked(time.Now())
	}
	l.mu.Unlock()
	select {
	case l.ready <- c.nc:
	case <-l.done:
		c.nc.Close()
	}
}

// conn is one dialled connection until it is paired or lost.
type conn struct {
	l    *Listener
	nc   net.Conn
	stop chan struct{} // closed, under wmu, when the keepalives end for good

	wmu     sync.Mutex // every write before pairing
	stopped bool       // guarded by wmu: no K may be written
	cause   error      // guarded by wmu: why a keepalive write failed
}

// run sends the hello, keeps the connection idle until the Pi's G, answers
// A and hands the connection out; on any failure it drops the connection.
func (c *conn) run() {
	defer c.l.wg.Done()
	c.wmu.Lock()
	err := c.writeLocked([]byte(Hello))
	c.wmu.Unlock()
	if err == nil {
		c.l.wg.Add(1)
		go c.keepalive()
		err = c.awaitGo()
	}
	if err == nil {
		err = c.ack()
	}
	if err != nil {
		c.l.lose(c, err)
		return
	}
	c.l.handOut(c)
}

// awaitGo reads the Pi's bytes one at a time until G. A K refreshes the
// silence deadline; any other byte, Silence with nothing heard, or a failed
// read is an error. The first K or G shows that the Pi keeps the connection.
func (c *conn) awaitGo() error {
	var b [1]byte
	heard := time.Now()
	piKeeps := false // the Pi has sent a K or G on this connection
	for {
		if err := c.nc.SetReadDeadline(heard.Add(c.l.cfg.Silence)); err != nil {
			return err
		}
		if _, err := io.ReadFull(c.nc, b[:]); err != nil {
			return c.readError(err)
		}
		if b[0] != KeepaliveByte && b[0] != GoByte {
			return fmt.Errorf("unexpected byte 0x%02x", b[0])
		}
		if !piKeeps {
			piKeeps = true
			c.l.kept()
		}
		if b[0] == GoByte {
			return nil
		}
		heard = time.Now()
	}
}

// readError says why a read before pairing failed.
func (c *conn) readError(err error) error {
	c.wmu.Lock()
	cause := c.cause
	c.wmu.Unlock()
	switch {
	case cause != nil:
		return cause
	case errors.Is(err, os.ErrDeadlineExceeded):
		return fmt.Errorf("nothing heard for %v", c.l.cfg.Silence)
	case errors.Is(err, io.EOF):
		return errors.New("closed by the Pi")
	}
	return err
}

// ack answers G. Under wmu it stops the keepalives for good and then writes
// A, so no K can follow the A.
func (c *conn) ack() error {
	c.wmu.Lock()
	defer c.wmu.Unlock()
	c.stopLocked()
	return c.writeLocked([]byte{AckByte})
}

// keepalive writes a K every KeepaliveEvery until the keepalives stop. A
// failed write closes the connection, under wmu so that no A can follow on
// it, which ends awaitGo.
func (c *conn) keepalive() {
	defer c.l.wg.Done()
	t := time.NewTicker(c.l.cfg.KeepaliveEvery)
	defer t.Stop()
	for {
		select {
		case <-c.stop:
			return
		case <-t.C:
		}
		c.wmu.Lock()
		if c.stopped {
			c.wmu.Unlock()
			return
		}
		err := c.writeLocked([]byte{KeepaliveByte})
		if err != nil {
			c.cause = fmt.Errorf("writing a keepalive: %w", err)
			c.nc.Close()
		}
		c.wmu.Unlock()
		if err != nil {
			return
		}
	}
}

func (c *conn) stopKeepalives() {
	c.wmu.Lock()
	defer c.wmu.Unlock()
	c.stopLocked()
}

// stopLocked ends the keepalives for good. Caller holds wmu.
func (c *conn) stopLocked() {
	if !c.stopped {
		c.stopped = true
		close(c.stop)
	}
}

// writeLocked writes b with a deadline of Silence. Caller holds wmu.
func (c *conn) writeLocked(b []byte) error {
	if err := c.nc.SetWriteDeadline(time.Now().Add(c.l.cfg.Silence)); err != nil {
		return err
	}
	_, err := c.nc.Write(b)
	return err
}
