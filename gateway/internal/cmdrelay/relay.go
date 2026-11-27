package cmdrelay

import (
	"bufio"
	"context"
	"crypto/rand"
	"errors"
	"log"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"airgapp/gateway/camhelper/proto"
	"airgapp/gateway/internal/peercred"
)

// DefaultSocket is the relay's socket on the car, in camhelper's socket directory (root:<gw gid> 0750).
const DefaultSocket = "/run/airgapp-camhelper/cmd.sock"

// The relay's frames (camhelper's framing: u32 big-endian length, type byte, body).
//
//	client -> relay: 0x01 MSG   one RoutableMessage from the phone (at most MaxMsg bytes)
//	relay -> client: 0x81 MSG   one reply for this session's routing addresses (the phone's address put back)
//	                 0x82 ERR   body: a reason, then 0x00 and the message's uuid (field 51) when it had one:
//	                            refused | rate | monitor | busy: the message was NOT injected (safe to resend);
//	                            unknown: it was handed to the router but no answer came back (it may have run:
//	                            wait for the router's reply rather than resending)
//	                 0x83 READY the monitor is up: MSGs are accepted (sent once, first)
const (
	TMsg   byte = 0x01
	TReply byte = 0x81
	TErr   byte = 0x82
	TReady byte = 0x83

	MaxMsg = 8 << 10
)

// Limits per session.
const (
	maxAddresses = 8               // distinct phone routing addresses (Tesla's app uses one per domain session)
	queueLen     = 64              // replies waiting for the client; a full queue closes the session
	ratePerSec   = 20              // messages injected per second, on average ...
	rateBurst    = 40              // ... with this burst
	injectBudget = 3 * time.Second // one injection
	readyWait    = 2 * time.Second // how long a new session waits for the monitor before giving up
)

// Injector hands one message to the command-router as the authd source. An error wrapping ErrNotSent means the
// router never got the message; any other error means it may have.
type Injector interface {
	Inject(ctx context.Context, msg []byte) error
}

// ErrNotSent marks an injection that certainly did not reach the router.
var ErrNotSent = errors.New("not sent")

// Config configures a Server.
type Config struct {
	AllowUID int      // the only peer uid served (agw's)
	Inject   Injector // the authd-uid child
	Log      *log.Logger
}

// Server relays phone messages in and router replies out. Run a Monitor whose Deliver is Server.Deliver and whose
// SetUp is Server.SetMonitorUp alongside it.
type Server struct {
	cfg Config

	monitorUp atomic.Bool
	upCh      chan struct{} // closed (and replaced) whenever the monitor comes up
	mu        sync.Mutex    // guards upCh, byCar, sessions, closed
	byCar     map[string]*session
	sessions  map[*session]struct{}
	closed    bool
}

// New returns a Server.
func New(cfg Config) *Server {
	if cfg.Log == nil {
		cfg.Log = log.Default()
	}
	return &Server{cfg: cfg, upCh: make(chan struct{}), byCar: map[string]*session{}, sessions: map[*session]struct{}{}}
}

// SetMonitorUp is the monitor's state: replies can only be seen while it is up.
func (s *Server) SetMonitorUp(up bool) {
	s.monitorUp.Store(up)
	if up {
		s.mu.Lock()
		close(s.upCh)
		s.upCh = make(chan struct{})
		s.mu.Unlock()
	}
}

// Deliver is one com.tesla.Auth.RouteMessageBytes body the monitor saw. A reply to a routing address this relay
// handed out goes to that session with the phone's address put back; everything else is ignored.
func (s *Server) Deliver(body []byte) {
	ra := ReplyAddress(body)
	if ra == nil {
		return
	}
	s.mu.Lock()
	sess := s.byCar[string(ra)]
	s.mu.Unlock()
	if sess == nil {
		return
	}
	phone := sess.phoneFor(ra)
	if phone == nil {
		return
	}
	out, err := Inbound(body, phone)
	if err != nil {
		return
	}
	sess.queue(proto.Encode(TReply, out))
}

// Serve accepts sessions on ln until Shutdown or a listener error.
func (s *Server) Serve(ln net.Listener) error {
	for {
		c, err := ln.Accept()
		if err != nil {
			s.mu.Lock()
			closed := s.closed
			s.mu.Unlock()
			if closed {
				return nil
			}
			return err
		}
		uid, err := peercred.PeerUID(c)
		if err != nil || uid != s.cfg.AllowUID {
			s.cfg.Log.Printf("cmdrelay: refused a connection from uid %d (%v)", uid, err)
			c.Close()
			continue
		}
		sess := &session{srv: s, c: c, out: make(chan []byte, queueLen), done: make(chan struct{}),
			toCar: map[string][]byte{}, toPhone: map[string][]byte{}, tokens: rateBurst, last: time.Now()}
		s.mu.Lock()
		if s.closed {
			s.mu.Unlock()
			c.Close()
			return nil
		}
		s.sessions[sess] = struct{}{}
		s.mu.Unlock()
		go sess.run()
	}
}

// Shutdown closes every session; Serve's listener is the caller's to close.
func (s *Server) Shutdown() {
	s.mu.Lock()
	s.closed = true
	var all []*session
	for sess := range s.sessions {
		all = append(all, sess)
	}
	s.mu.Unlock()
	for _, sess := range all {
		sess.close()
	}
}

func (s *Server) waitUp(d time.Duration) bool {
	if s.monitorUp.Load() {
		return true
	}
	s.mu.Lock()
	ch := s.upCh
	s.mu.Unlock()
	select {
	case <-ch:
	case <-time.After(d):
	}
	return s.monitorUp.Load()
}

type session struct {
	srv  *Server
	c    net.Conn
	out  chan []byte
	done chan struct{}
	once sync.Once

	mu      sync.Mutex
	toCar   map[string][]byte // phone routing address -> car-side address
	toPhone map[string][]byte // car-side address -> phone routing address
	order   []string          // phone addresses, oldest first (the cap evicts the oldest)
	tokens  float64
	last    time.Time
}

func (sess *session) close() {
	sess.once.Do(func() {
		close(sess.done)
		sess.c.Close()
		// Lock order is session then server (carFor); never hold both here.
		sess.mu.Lock()
		cars := make([]string, 0, len(sess.toPhone))
		for car := range sess.toPhone {
			cars = append(cars, car)
		}
		sess.mu.Unlock()
		s := sess.srv
		s.mu.Lock()
		delete(s.sessions, sess)
		for _, car := range cars {
			if s.byCar[car] == sess {
				delete(s.byCar, car)
			}
		}
		s.mu.Unlock()
	})
}

func (sess *session) queue(f []byte) {
	select {
	case <-sess.done:
	case sess.out <- f:
	default:
		sess.srv.cfg.Log.Print("cmdrelay: a session's reply queue is full; closing it")
		sess.close()
	}
}

func (sess *session) phoneFor(car []byte) []byte {
	sess.mu.Lock()
	defer sess.mu.Unlock()
	return sess.toPhone[string(car)]
}

// carFor returns the car-side address for a phone address, making one (registered with the server) on first use.
func (sess *session) carFor(phone []byte) ([]byte, error) {
	sess.mu.Lock()
	defer sess.mu.Unlock()
	if car, ok := sess.toCar[string(phone)]; ok {
		return car, nil
	}
	select {
	case <-sess.done:
		return nil, errors.New("session closed")
	default:
	}
	if len(sess.toCar) >= maxAddresses {
		// Tesla's app mints a new routing address on every re-handshake: forget the oldest one (its late replies
		// are dropped) rather than refusing for the rest of the session.
		old := sess.order[0]
		sess.order = sess.order[1:]
		oldCar := sess.toCar[old]
		delete(sess.toCar, old)
		delete(sess.toPhone, string(oldCar))
		s := sess.srv
		s.mu.Lock()
		if s.byCar[string(oldCar)] == sess {
			delete(s.byCar, string(oldCar))
		}
		s.mu.Unlock()
	}
	car := make([]byte, 16)
	if _, err := rand.Read(car); err != nil {
		return nil, err
	}
	s := sess.srv
	s.mu.Lock()
	s.byCar[string(car)] = sess
	s.mu.Unlock()
	sess.toCar[string(phone)] = car
	sess.toPhone[string(car)] = append([]byte(nil), phone...)
	sess.order = append(sess.order, string(phone))
	return car, nil
}

// allow takes a rate token.
func (sess *session) allow() bool {
	sess.mu.Lock()
	defer sess.mu.Unlock()
	now := time.Now()
	sess.tokens += now.Sub(sess.last).Seconds() * ratePerSec
	if sess.tokens > rateBurst {
		sess.tokens = rateBurst
	}
	sess.last = now
	if sess.tokens < 1 {
		return false
	}
	sess.tokens--
	return true
}

func (sess *session) run() {
	defer sess.close()
	go sess.writer()
	if !sess.srv.waitUp(readyWait) {
		sess.srv.cfg.Log.Print("cmdrelay: monitor not up; closing a new session")
		return
	}
	sess.queue(proto.Encode(TReady, nil))
	r := bufio.NewReader(sess.c)
	for {
		t, body, err := proto.ReadFrame(r, 1+MaxMsg)
		if err != nil {
			return
		}
		if t != TMsg {
			sess.srv.cfg.Log.Printf("cmdrelay: unknown frame 0x%02x; closing the session", t)
			return
		}
		sess.handle(body)
	}
}

// fail tells the client a MSG failed: reason, then 0x00 and the message's uuid if it had one.
func (sess *session) fail(reason string, msg []byte) {
	body := []byte(reason)
	if u := MessageUUID(msg); u != nil {
		body = append(append(body, 0), u...)
	}
	sess.queue(proto.Encode(TErr, body))
}

func (sess *session) handle(msg []byte) {
	if !sess.allow() {
		sess.fail("rate", msg)
		return
	}
	if !sess.srv.monitorUp.Load() {
		sess.fail("monitor", msg)
		return
	}
	// The phone's address decides the car-side one, so parse it first (Outbound validates everything again).
	_, phone, err := Outbound(msg, make([]byte, 16))
	if err != nil {
		sess.fail("refused", msg)
		return
	}
	car, err := sess.carFor(phone)
	if err != nil {
		sess.fail("busy", msg)
		return
	}
	out, _, err := Outbound(msg, car)
	if err != nil {
		sess.fail("refused", msg)
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), injectBudget)
	defer cancel()
	if err := sess.srv.cfg.Inject.Inject(ctx, out); err != nil {
		sess.srv.cfg.Log.Printf("cmdrelay: inject: %v", err)
		if errors.Is(err, ErrNotSent) {
			sess.fail("busy", msg)
		} else {
			sess.fail("unknown", msg)
		}
	}
}

func (sess *session) writer() {
	for {
		select {
		case <-sess.done:
			return
		case f := <-sess.out:
			sess.c.SetWriteDeadline(time.Now().Add(2 * time.Second))
			if _, err := sess.c.Write(f); err != nil {
				sess.close()
				return
			}
		}
	}
}
