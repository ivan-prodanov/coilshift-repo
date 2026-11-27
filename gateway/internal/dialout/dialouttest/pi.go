// Package dialouttest is a fake Pi for tests above dialout. It pools the car
// connections that a dialout.Listener dials and pairs each phone with one, by
// the Pi rules of the car-dials-out design (2026-11-21).
package dialouttest

import (
	"io"
	"net"
	"slices"
	"sync"
	"testing"
	"time"

	"airgapp/gateway/internal/dialout"
)

// Pi listens on loopback for the car (CarAddr) and for phones (PhoneAddr).
// A car connection must send the hello, and then waits idle in the pool, with
// keepalives both ways. Each phone takes one idle car connection: the Pi
// sends G, waits for the car's A, and then pipes bytes both ways.
type Pi struct {
	// The Pi's rules, set to the spec's values by Start. The first call to
	// CarAddr or PhoneAddr starts serving with the values set then; later
	// changes have no effect.
	HelloWait      time.Duration // a car's hello must arrive within this (3 s)
	KeepaliveEvery time.Duration // a K to each idle car this often (10 s)
	Silence        time.Duration // an idle car that sends nothing this long is closed (25 s)
	AckWait        time.Duration // a car must answer G with A within this (2 s)
	CarWait        time.Duration // a phone waits this long for an idle car (3 s)
	PoolMax        int           // idle car connections kept at most (4)

	carLn, phoneLn net.Listener
	started        sync.Once
	rules          rules         // the fields above, as serving started
	done           chan struct{} // closed by Close
	closeOnce      sync.Once
	wg             sync.WaitGroup

	mu     sync.Mutex
	closed bool
	pool   []*car                // idle car connections, oldest first
	joined chan struct{}         // closed and replaced whenever a car joins the pool
	conns  map[net.Conn]struct{} // every open connection, car or phone
}

type rules struct {
	helloWait, keepaliveEvery, silence, ackWait, carWait time.Duration
	poolMax                                              int
}

// Start returns a Pi with the spec's rules, closed when the test ends.
func Start(t testing.TB) *Pi {
	t.Helper()
	carLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	phoneLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		carLn.Close()
		t.Fatal(err)
	}
	p := &Pi{
		HelloWait:      3 * time.Second,
		KeepaliveEvery: 10 * time.Second,
		Silence:        25 * time.Second,
		AckWait:        2 * time.Second,
		CarWait:        3 * time.Second,
		PoolMax:        4,
		carLn:          carLn,
		phoneLn:        phoneLn,
		done:           make(chan struct{}),
		joined:         make(chan struct{}),
		conns:          map[net.Conn]struct{}{},
	}
	t.Cleanup(p.Close)
	return p
}

// CarAddr is the address the car dials.
func (p *Pi) CarAddr() string {
	p.serve()
	return p.carLn.Addr().String()
}

// PhoneAddr is the address a phone connects to; each phone is paired with
// one idle car connection.
func (p *Pi) PhoneAddr() string {
	p.serve()
	return p.phoneLn.Addr().String()
}

// Idle is the number of idle car connections in the pool.
func (p *Pi) Idle() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.pool)
}

// Close closes both listeners and every connection, and waits for the Pi's
// goroutines. It is idempotent.
func (p *Pi) Close() {
	p.closeOnce.Do(func() {
		p.mu.Lock()
		p.closed = true
		p.pool = nil
		conns := make([]net.Conn, 0, len(p.conns))
		for c := range p.conns {
			conns = append(conns, c)
		}
		p.mu.Unlock()
		close(p.done)
		p.carLn.Close()
		p.phoneLn.Close()
		for _, c := range conns {
			c.Close()
		}
		p.wg.Wait()
	})
}

// serve starts the accept loops, once, with the rules as they are now.
func (p *Pi) serve() {
	p.started.Do(func() {
		p.rules = rules{p.HelloWait, p.KeepaliveEvery, p.Silence, p.AckWait, p.CarWait, p.PoolMax}
		p.mu.Lock()
		defer p.mu.Unlock()
		if p.closed {
			return
		}
		p.wg.Add(2)
		go p.acceptLoop(p.carLn, p.serveCar)
		go p.acceptLoop(p.phoneLn, p.servePhone)
	})
}

func (p *Pi) acceptLoop(ln net.Listener, serve func(net.Conn)) {
	defer p.wg.Done()
	for {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		if !p.track(c) {
			c.Close()
			return
		}
		p.wg.Add(1)
		go func() {
			defer p.wg.Done()
			serve(c)
		}()
	}
}

// track records c so that Close closes it; false once the Pi is closed.
func (p *Pi) track(c net.Conn) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		return false
	}
	p.conns[c] = struct{}{}
	return true
}

// drop closes c and forgets it.
func (p *Pi) drop(c net.Conn) {
	p.mu.Lock()
	delete(p.conns, c)
	p.mu.Unlock()
	c.Close()
}

// car is one car connection, idle in the pool or being paired.
type car struct {
	c    net.Conn
	stop chan struct{} // closed, under wmu, when the Pi's keepalives end
	ack  chan struct{} // closed when the car's A arrives
	gone chan struct{} // closed when the car's reader ends without an A

	wmu     sync.Mutex
	quiet   bool // guarded by wmu: no more K
	pairing bool // guarded by wmu: G is sent, and the car's A is awaited
}

// quietLocked ends the keepalives. Caller holds wmu.
func (k *car) quietLocked() {
	if !k.quiet {
		k.quiet = true
		close(k.stop)
	}
}

// serveCar reads a car connection's hello, pools it and reads it until the
// A that answers a pairing, or until it fails.
func (p *Pi) serveCar(c net.Conn) {
	c.SetReadDeadline(time.Now().Add(p.rules.helloWait))
	hello := make([]byte, len(dialout.Hello))
	if _, err := io.ReadFull(c, hello); err != nil || string(hello) != dialout.Hello {
		p.drop(c)
		return
	}
	k := &car{c: c, stop: make(chan struct{}), ack: make(chan struct{}), gone: make(chan struct{})}
	p.mu.Lock()
	if p.closed || len(p.pool) >= p.rules.poolMax {
		p.mu.Unlock()
		p.drop(c)
		return
	}
	p.pool = append(p.pool, k)
	close(p.joined)
	p.joined = make(chan struct{})
	p.wg.Add(1)
	p.mu.Unlock()
	go p.keepalive(k)

	if p.readCar(k) {
		close(k.ack) // the pipe reads from here on
		return
	}
	p.mu.Lock()
	if i := slices.Index(p.pool, k); i >= 0 {
		p.pool = slices.Delete(p.pool, i, i+1)
	}
	p.mu.Unlock()
	k.wmu.Lock()
	k.quietLocked()
	k.wmu.Unlock()
	close(k.gone)
	p.drop(c)
}

// readCar reads the car's bytes one at a time, each within Silence of the
// last. A K is dropped; an A while pairing returns true. Any other byte,
// silence or a failed read returns false.
func (p *Pi) readCar(k *car) bool {
	var b [1]byte
	for {
		k.c.SetReadDeadline(time.Now().Add(p.rules.silence))
		if _, err := io.ReadFull(k.c, b[:]); err != nil {
			return false
		}
		k.wmu.Lock()
		pairing := k.pairing
		k.wmu.Unlock()
		switch {
		case b[0] == dialout.KeepaliveByte:
		case b[0] == dialout.AckByte && pairing:
			return true
		default:
			return false
		}
	}
}

// keepalive sends the car a K every KeepaliveEvery until it is paired or gone.
func (p *Pi) keepalive(k *car) {
	defer p.wg.Done()
	t := time.NewTicker(p.rules.keepaliveEvery)
	defer t.Stop()
	for {
		select {
		case <-k.stop:
			return
		case <-t.C:
		}
		k.wmu.Lock()
		if k.quiet {
			k.wmu.Unlock()
			return
		}
		k.c.SetWriteDeadline(time.Now().Add(p.rules.silence))
		_, err := k.c.Write([]byte{dialout.KeepaliveByte})
		k.wmu.Unlock()
		if err != nil {
			k.c.Close()
			return
		}
	}
}

// servePhone pairs a phone with an idle car connection, trying the next one
// when a car does not answer, and pipes the two. With no idle car within
// CarWait it closes the phone.
func (p *Pi) servePhone(phone net.Conn) {
	deadline := time.Now().Add(p.rules.carWait)
	for {
		k := p.take(deadline)
		if k == nil {
			p.drop(phone)
			return
		}
		if p.pair(k) {
			p.pipe(phone, k.c)
			return
		}
		p.drop(k.c)
	}
}

// take removes the oldest idle car from the pool, waiting until deadline
// for one to join; nil if none does or the Pi closes.
func (p *Pi) take(deadline time.Time) *car {
	t := time.NewTimer(time.Until(deadline))
	defer t.Stop()
	for {
		p.mu.Lock()
		if len(p.pool) > 0 {
			k := p.pool[0]
			p.pool = p.pool[1:]
			p.mu.Unlock()
			return k
		}
		joined := p.joined
		p.mu.Unlock()
		select {
		case <-joined:
		case <-t.C:
			return nil
		case <-p.done:
			return nil
		}
	}
}

// pair stops the car's keepalives, sends it G and waits up to AckWait for its
// A.
func (p *Pi) pair(k *car) bool {
	k.wmu.Lock()
	k.quietLocked()
	k.pairing = true
	k.c.SetWriteDeadline(time.Now().Add(p.rules.ackWait))
	_, err := k.c.Write([]byte{dialout.GoByte})
	k.wmu.Unlock()
	if err != nil {
		return false
	}
	t := time.NewTimer(p.rules.ackWait)
	defer t.Stop()
	select {
	case <-k.ack:
		return true
	case <-k.gone:
	case <-t.C:
	case <-p.done:
	}
	return false
}

// pipe copies bytes both ways, passing each half-close on, and closes both
// connections once both directions have ended.
func (p *Pi) pipe(phone, car net.Conn) {
	phone.SetDeadline(time.Time{})
	car.SetDeadline(time.Time{})
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		relay(car, phone)
	}()
	go func() {
		defer wg.Done()
		relay(phone, car)
	}()
	wg.Wait()
	p.drop(phone)
	p.drop(car)
}

// relay copies src to dst. At src's EOF it half-closes dst; on an error it
// closes both, which ends the other direction too.
func relay(dst, src net.Conn) {
	if _, err := io.Copy(dst, src); err != nil {
		dst.Close()
		src.Close()
		return
	}
	if cw, ok := dst.(interface{ CloseWrite() error }); ok {
		cw.CloseWrite()
	} else {
		dst.Close()
	}
}
