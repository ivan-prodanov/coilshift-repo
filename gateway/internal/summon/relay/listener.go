package relay

import (
	"errors"
	"fmt"
	"io/fs"
	"net"
	"os"
	"sync"
	"time"

	"airgapp/gateway/internal/peercred"
)

var (
	_ net.Listener = (*Listener)(nil)
	_ net.Conn     = (*conn)(nil)
)

// ListenConfig configures the helper's relay listener.
type ListenConfig struct {
	Path      string                           // the socket; whatever is at Path is removed first
	AllowUID  int                              // the only peer uid served: agw's, 4950
	PeerUID   func(net.Conn) (int, error)      // default peercred.PeerUID
	Ready     func() bool                      // owned and healthy: required before OWNED
	FirstWait time.Duration                    // default FirstWait
	Logf      func(format string, args ...any) // default: discard
}

// Listener is the helper's end of relay.sock: a net.Listener for the server core.
//
// Its accept goroutine serves one relay connection at a time, through these steps:
//
//  1. accept, then refuse (close with no record) a peer whose uid is not AllowUID, or when Ready() is false;
//  2. write OWNED, then read the first record, which must be INNER and arrive within FirstWait;
//  3. hand the connection to Accept: from here it is active, and SetLink writes to it;
//  4. wait until the core closes it, then go back to 1.
//
// A failure in steps 1 and 2 closes the connection and goes back to 1. In step 4 the slot is freed only by the core's
// Close: agw going away, or breaking the record grammar, fails the core's Read but frees nothing. The core closes only
// after its Teardown, so the next relay connection, waiting in the kernel's backlog meanwhile, gets OWNED only after
// the previous session's H4 went out.
//
// PeerUID, Ready and Logf must not block: Close waits for the accept goroutine.
type Listener struct {
	cfg   ListenConfig
	ln    *net.UnixListener
	conns chan *conn    // unbuffered: the hand-off of step 3
	done  chan struct{} // closed by Close
	wg    sync.WaitGroup

	mu      sync.Mutex
	closed  bool
	pending *net.UnixConn // in steps 1 and 2: Close closes it
	active  *conn         // handed off, until the core closes it
}

// Listen creates the socket at Path, mode 0660, as the current uid, and starts the accept goroutine. On the car the
// directory's setgid bit gives the socket group 4950.
func Listen(cfg ListenConfig) (*Listener, error) {
	if cfg.Path == "" {
		return nil, errors.New("relay: no socket path")
	}
	if cfg.Ready == nil {
		return nil, errors.New("relay: no Ready predicate")
	}
	if cfg.PeerUID == nil {
		cfg.PeerUID = peercred.PeerUID
	}
	if cfg.FirstWait <= 0 {
		cfg.FirstWait = FirstWait
	}
	if cfg.Logf == nil {
		cfg.Logf = func(string, ...any) {}
	}
	if err := os.Remove(cfg.Path); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return nil, err
	}
	ln, err := net.ListenUnix("unix", &net.UnixAddr{Name: cfg.Path, Net: "unix"})
	if err != nil {
		return nil, err
	}
	if err := os.Chmod(cfg.Path, 0o660); err != nil {
		ln.Close()
		return nil, err
	}
	l := &Listener{cfg: cfg, ln: ln, conns: make(chan *conn), done: make(chan struct{})}
	l.wg.Add(1)
	go l.run()
	return l, nil
}

// Accept returns the next connection handed off, or net.ErrClosed once the listener is closed. It returns no other
// error: the core's Serve backs off on any other.
func (l *Listener) Accept() (net.Conn, error) {
	select {
	case c := <-l.conns:
		return c, nil
	case <-l.done:
		return nil, net.ErrClosed
	}
}

// Close stops accepting and closes a connection not yet handed off. The connection the core holds stays open until
// the core closes it. Close waits for the accept goroutine.
func (l *Listener) Close() error {
	err := l.stop()
	l.wg.Wait()
	return err
}

// stop closes the listener once; later calls return net.ErrClosed.
func (l *Listener) stop() error {
	l.mu.Lock()
	if l.closed {
		l.mu.Unlock()
		return net.ErrClosed
	}
	l.closed = true
	pending := l.pending
	l.mu.Unlock()
	close(l.done)
	err := l.ln.Close()
	if pending != nil {
		pending.Close()
	}
	return err
}

// Addr is the socket's address.
func (l *Listener) Addr() net.Addr { return l.ln.Addr() }

// SetLink writes the link flag in a LINK record on the active connection, if any. It never blocks: the value waits
// in the connection's one-slot mailbox for its LINK writer, and a value superseded before it is written is dropped.
func (l *Listener) SetLink(degraded bool) {
	l.mu.Lock()
	c := l.active
	l.mu.Unlock()
	if c != nil {
		c.setLink(degraded)
	}
}

// run is the accept goroutine.
func (l *Listener) run() {
	defer l.wg.Done()
	defer l.stop() // if the socket failed for good, Accept returns net.ErrClosed
	backoff := 5 * time.Millisecond
	for {
		uc, err := l.ln.AcceptUnix()
		if err != nil {
			if errors.Is(err, net.ErrClosed) {
				return
			}
			l.cfg.Logf("relay: accept: %v", err)
			select {
			case <-time.After(backoff):
			case <-l.done:
				return
			}
			backoff = min(2*backoff, time.Second)
			continue
		}
		backoff = 5 * time.Millisecond
		c := l.admit(uc)
		if c == nil {
			continue
		}
		if !l.handOff(c) {
			return
		}
		select {
		case <-c.closed: // by the core, after its Teardown
			l.mu.Lock()
			l.active = nil
			l.mu.Unlock()
		case <-l.done:
			return
		}
	}
}

// admit runs steps 1 and 2 on uc. It returns the connection to hand off, or nil once it closed uc.
func (l *Listener) admit(uc *net.UnixConn) *conn {
	l.mu.Lock()
	if l.closed {
		l.mu.Unlock()
		uc.Close()
		return nil
	}
	l.pending = uc
	l.mu.Unlock()

	c, why := l.prepare(uc)

	l.mu.Lock()
	l.pending = nil
	closed := l.closed
	l.mu.Unlock()
	if c == nil {
		uc.Close()
		if !closed {
			l.cfg.Logf("relay: closed a connection: %s", why)
		}
	}
	return c
}

// prepare checks the peer, writes OWNED and reads the first INNER record. It returns the connection, or nil and why
// not.
func (l *Listener) prepare(uc *net.UnixConn) (*conn, string) {
	uid, err := l.cfg.PeerUID(uc)
	if err != nil {
		return nil, fmt.Sprintf("peer uid: %v", err)
	}
	if uid != l.cfg.AllowUID {
		return nil, fmt.Sprintf("peer uid %d refused", uid)
	}
	if !l.cfg.Ready() {
		return nil, "not ready (the name is not owned, or the helper is unhealthy)"
	}
	if err := uc.SetDeadline(time.Now().Add(l.cfg.FirstWait)); err != nil {
		return nil, fmt.Sprintf("deadline: %v", err)
	}
	w := NewWriter(uc)
	if err := w.Owned(); err != nil {
		return nil, fmt.Sprintf("OWNED: %v", err)
	}
	r := NewReader(uc, false)
	typ, first, err := r.Next()
	switch {
	case errors.Is(err, os.ErrDeadlineExceeded):
		return nil, fmt.Sprintf("no INNER within %v", l.cfg.FirstWait)
	case err != nil:
		return nil, fmt.Sprintf("first record: %v", err)
	case typ != RecInner:
		return nil, fmt.Sprintf("first record 0x%02x is not INNER", typ)
	}
	if err := uc.SetDeadline(time.Time{}); err != nil {
		return nil, fmt.Sprintf("deadline: %v", err)
	}
	return &conn{uc: uc, r: r, w: w, in: first, closed: make(chan struct{}), kick: make(chan struct{}, 1)}, ""
}

// handOff runs step 3: c becomes active, its LINK writer starts, and Accept gets it. It returns false once the
// listener is closed, after closing c.
func (l *Listener) handOff(c *conn) bool {
	l.mu.Lock()
	if l.closed {
		l.mu.Unlock()
		c.Close()
		return false
	}
	l.active = c
	l.mu.Unlock()
	go c.linkWriter()
	select {
	case l.conns <- c:
		l.cfg.Logf("relay: connection handed to the core")
		return true
	case <-l.done:
		l.mu.Lock()
		l.active = nil
		l.mu.Unlock()
		c.Close()
		return false
	}
}

// conn is a relay connection as the core sees it: the inner session's byte stream.
type conn struct {
	uc *net.UnixConn
	r  *Reader
	w  *Writer

	rmu  sync.Mutex
	in   []byte // the rest of the latest INNER payload
	rerr error

	once   sync.Once
	cerr   error
	closed chan struct{} // closed by Close: the core is done with the connection

	lmu     sync.Mutex
	link    bool
	linkSet bool          // link is waiting for the LINK writer
	kick    chan struct{} // capacity 1: wakes the LINK writer
}

// Read yields the INNER payloads as one byte stream, starting with the first, which the listener read. Any other
// record from agw, or a bad one, closes the socket. After an error every later Read returns it.
func (c *conn) Read(p []byte) (int, error) {
	c.rmu.Lock()
	defer c.rmu.Unlock()
	for len(c.in) == 0 {
		if c.rerr != nil {
			return 0, c.rerr
		}
		if len(p) == 0 {
			return 0, nil
		}
		typ, payload, err := c.r.Next()
		if err == nil && typ != RecInner {
			err = fmt.Errorf("%w: 0x%02x from agw", errBadRecord, typ)
		}
		if err != nil {
			c.rerr = err
			if errors.Is(err, errBadRecord) {
				c.uc.Close()
			}
			return 0, err
		}
		c.in = payload
	}
	n := copy(p, c.in)
	c.in = c.in[n:]
	return n, nil
}

// Write sends p as INNER records.
func (c *conn) Write(p []byte) (int, error) { return c.w.inner(p) }

// Close closes the socket and frees the listener's slot: only then is the next relay connection accepted.
func (c *conn) Close() error {
	c.once.Do(func() {
		c.cerr = c.uc.Close()
		close(c.closed)
	})
	return c.cerr
}

func (c *conn) LocalAddr() net.Addr                { return c.uc.LocalAddr() }
func (c *conn) RemoteAddr() net.Addr               { return c.uc.RemoteAddr() }
func (c *conn) SetDeadline(t time.Time) error      { return c.uc.SetDeadline(t) }
func (c *conn) SetReadDeadline(t time.Time) error  { return c.uc.SetReadDeadline(t) }
func (c *conn) SetWriteDeadline(t time.Time) error { return c.uc.SetWriteDeadline(t) }

// setLink puts degraded in the mailbox and wakes the LINK writer. It never blocks.
func (c *conn) setLink(degraded bool) {
	c.lmu.Lock()
	c.link, c.linkSet = degraded, true
	c.lmu.Unlock()
	select {
	case c.kick <- struct{}{}:
	default: // a wake-up is pending: the writer will take the latest value
	}
}

// linkWriter writes the mailbox's latest value as a LINK record whenever woken, until the core closes c. A write
// that fails leaves c to the core, whose own writes fail too.
func (c *conn) linkWriter() {
	for {
		select {
		case <-c.closed:
			return
		case <-c.kick:
		}
		c.lmu.Lock()
		v, ok := c.link, c.linkSet
		c.linkSet = false
		c.lmu.Unlock()
		if ok {
			c.w.Link(v)
		}
	}
}
