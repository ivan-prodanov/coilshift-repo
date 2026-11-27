// Package server is the gateway's connection state machine and window
// manager: accept limits, the pre-auth handshake, one window per capability,
// and the authority re-checks of open windows (recheck.go).
package server

import (
	"context"
	"crypto/ecdsa"
	"errors"
	"fmt"
	"log"
	"net"
	"sync"
	"time"

	"airgapp/gateway/capability"
	"airgapp/gateway/internal/authority"
	"airgapp/gateway/internal/clock"
	"airgapp/gateway/internal/proto"
)

const (
	maxPreAuth      = 2               // concurrent connections before ARM
	preAuthTimeout  = 2 * time.Second // whole pre-auth phase, partial frames included
	helloBurst      = 5               // at most helloBurst accepts ...
	helloWindow     = time.Second     // ... in any helloWindow
	shutdownTimeout = time.Second     // bound on waiting for windows to finish
)

// Config is the gateway's runtime configuration. There is no key list of the
// gateway's own: a phone key signs in when it proves possession (the
// handshake) and Oracle says authd lists it as OWNER or DRIVER, and an open
// window loses its session when a re-check says so no more (recheck.go).
type Config struct {
	Key         *ecdsa.PrivateKey // long-term signing key
	VIN         string
	Oracle      authority.Oracle // sign-ins, and re-checks unless RecheckOracle is set
	Clock       clock.Clock      // BOOTTIME; nil means clock.Boot()
	Caps        []capability.Capability
	LogPath     string // session log; "" disables it
	MaxLogBytes int64  // rotate to LogPath+".1" past this size; 0 = never

	// RecheckOracle answers the re-checks of open windows (recheck.go); nil
	// means Oracle. agw gives it a helper connection of its own, so a
	// re-check never waits behind a sign-in.
	RecheckOracle authority.Oracle
}

type capEntry struct {
	cap   capability.Capability
	spec  capability.Spec
	maxIn int // largest Spec.In value
}

// Server accepts connections and owns the capability windows.
type Server struct {
	cfg  Config
	clk  clock.Clock
	caps map[byte]*capEntry
	slog *sessionLog

	ctx    context.Context // cancelled by Shutdown
	cancel context.CancelFunc

	mu        sync.Mutex
	closed    bool
	listeners map[net.Listener]struct{}
	pre       map[net.Conn]struct{} // connections in pre-auth
	windows   map[byte]*window      // capability slots
	accepts   []time.Time           // recent accept times (rate limit)
	nonces    nonceCache
	degraded  bool // Host: SUMMON's link-degraded flag; reset when SUMMON frees its slot

	wg sync.WaitGroup // connection goroutines and the re-check loop

	overrunHook func() // tests: runs after an Open overrun, before abort

	// Authority re-checks of open windows (recheck.go).
	kick           chan string      // a re-check of every window is due (its trigger); coalesced
	keysGen        uint64           // KeysChanged calls so far; guarded by mu
	watchUp        bool             // the helper's key-change watch is live; guarded by mu
	recheckStarted bool             // guarded by mu
	recheckTick    <-chan time.Time // tests; nil: a 1 s ticker
	now            func() time.Time // tests; nil: time.Now
	afterCheckHook func()           // tests: runs after a sign-in's authority check passed
}

// New builds a server. It panics on a programming error in cfg (missing key
// or oracle, duplicate capability IDs, an invalid Spec: see validateSpec).
func New(cfg Config) *Server {
	if cfg.Key == nil || cfg.Oracle == nil {
		panic("server: Config needs Key and Oracle")
	}
	clk := cfg.Clock
	if clk == nil {
		clk = clock.Boot()
	}
	if cfg.RecheckOracle == nil {
		cfg.RecheckOracle = cfg.Oracle
	}
	s := &Server{
		cfg:       cfg,
		clk:       clk,
		caps:      map[byte]*capEntry{},
		slog:      &sessionLog{path: cfg.LogPath, max: cfg.MaxLogBytes},
		listeners: map[net.Listener]struct{}{},
		pre:       map[net.Conn]struct{}{},
		windows:   map[byte]*window{},
		kick:      make(chan string, 1),
	}
	for _, c := range cfg.Caps {
		sp := c.Spec()
		if err := validateSpec(sp); err != nil {
			panic(fmt.Sprintf("server: capability %q (0x%02x): %v", sp.Name, sp.ID, err))
		}
		if _, dup := s.caps[sp.ID]; dup {
			panic(fmt.Sprintf("server: duplicate capability id 0x%02x", sp.ID))
		}
		e := &capEntry{cap: c, spec: sp}
		for _, n := range sp.In {
			e.maxIn = max(e.maxIn, n)
		}
		s.caps[sp.ID] = e
	}
	s.ctx, s.cancel = context.WithCancel(context.Background())
	return s
}

const (
	maxInLimit  = 16384   // largest inbound body (TALK PCM)
	maxOutLimit = 1 << 20 // largest outbound body (an H264 access unit)
	maxTickHz   = 50
)

// validateSpec rejects limits above the global constraints, core types in
// In/Out, Bulk types missing from Out, and a TickHz outside 0..50.
func validateSpec(sp capability.Spec) error {
	if sp.TickHz < 0 || sp.TickHz > maxTickHz {
		return fmt.Errorf("TickHz %d not in 0..%d", sp.TickHz, maxTickHz)
	}
	for t, n := range sp.In {
		if t >= proto.TypeArm && t <= proto.TypeClose {
			return fmt.Errorf("In has core type 0x%02x", t)
		}
		if n > maxInLimit {
			return fmt.Errorf("In 0x%02x limit %d > %d", t, n, maxInLimit)
		}
	}
	for t, n := range sp.Out {
		if t == proto.TypeReady || t == proto.TypeBye || t == proto.TypeTick {
			return fmt.Errorf("Out has core type 0x%02x", t)
		}
		if n > maxOutLimit {
			return fmt.Errorf("Out 0x%02x limit %d > %d", t, n, maxOutLimit)
		}
	}
	for t := range sp.Bulk {
		if _, ok := sp.Out[t]; !ok {
			return fmt.Errorf("Bulk 0x%02x not in Out", t)
		}
	}
	return nil
}

// Serve accepts connections on ln until Shutdown (returns nil) or a
// permanent listener error.
func (s *Server) Serve(ln net.Listener) error {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		ln.Close()
		return nil
	}
	s.listeners[ln] = struct{}{}
	s.mu.Unlock()
	s.startRecheck()
	defer func() {
		s.mu.Lock()
		delete(s.listeners, ln)
		s.mu.Unlock()
		ln.Close()
	}()

	backoff := 5 * time.Millisecond
	for {
		c, err := ln.Accept()
		if err != nil {
			s.mu.Lock()
			closed := s.closed
			s.mu.Unlock()
			if closed {
				return nil
			}
			if errors.Is(err, net.ErrClosed) {
				return err
			}
			log.Printf("agw: accept: %v", err)
			time.Sleep(backoff)
			backoff = min(2*backoff, time.Second)
			continue
		}
		backoff = 5 * time.Millisecond
		s.admit(c)
	}
}

// admit applies the pre-auth limits and starts the connection goroutine.
// The pre-auth cap is checked first so refused-for-capacity connections do
// not spend rate tokens.
func (s *Server) admit(c net.Conn) {
	now := time.Now()
	s.mu.Lock()
	if s.closed || len(s.pre) >= maxPreAuth || !s.takeHelloToken(now) {
		s.mu.Unlock()
		c.Close()
		return
	}
	s.pre[c] = struct{}{}
	s.wg.Add(1)
	s.mu.Unlock()
	go s.handle(c, now.Add(preAuthTimeout))
}

// takeHelloToken admits at most helloBurst connections in any helloWindow
// (a token bucket whose tokens each return helloWindow after use). It is
// global: the one upstream is the Pi forwarder. Caller holds s.mu.
func (s *Server) takeHelloToken(now time.Time) bool {
	if len(s.accepts) >= helloBurst {
		if now.Sub(s.accepts[0]) < helloWindow {
			return false
		}
		s.accepts = s.accepts[1:]
	}
	s.accepts = append(s.accepts, now)
	return true
}

// leavePreAuth forgets c as a pre-auth connection.
func (s *Server) leavePreAuth(c net.Conn) {
	s.mu.Lock()
	delete(s.pre, c)
	s.mu.Unlock()
}

// slotBusy reports whether capability id has a window (pre-check once the
// sign-in's authority check passed).
func (s *Server) slotBusy(id byte) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.windows[id] != nil
}

// reserve atomically claims the capability slot for w. It fails if the slot
// is taken or the server is shutting down. gen is keysGeneration before the
// sign-in's authority check: if keys changed since, the re-check that
// KeysChanged started may have missed w, so another one is asked for.
func (s *Server) reserve(w *window, gen uint64) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed || s.windows[w.spec.ID] != nil {
		return false
	}
	s.windows[w.spec.ID] = w
	if s.keysGen != gen {
		s.kickLocked("keys_changed during sign-in")
	}
	return true
}

// keysGeneration counts KeysChanged calls.
func (s *Server) keysGeneration() uint64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.keysGen
}

// free releases w's slot. Freeing the SUMMON slot clears the Host's
// link-degraded flag.
func (s *Server) free(w *window) {
	s.mu.Lock()
	if s.windows[w.spec.ID] == w {
		delete(s.windows, w.spec.ID)
		if w.spec.ID == proto.CapSummon {
			s.degraded = false
		}
	}
	s.mu.Unlock()
}

// windowHost is the capability.Host a window hands out: shared server
// state, with writes allowed only through the SUMMON window holding its slot.
type windowHost struct{ w *window }

func (h windowHost) SummonOpen() bool {
	s := h.w.s
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.windows[proto.CapSummon] != nil
}

func (h windowHost) SummonLinkDegraded() bool {
	s := h.w.s
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.degraded
}

func (h windowHost) SetSummonLinkDegraded(v bool) {
	s := h.w.s
	s.mu.Lock()
	defer s.mu.Unlock()
	if h.w.spec.ID == proto.CapSummon && s.windows[proto.CapSummon] == h.w {
		s.degraded = v
	}
}

// useNonce marks N used; false if it was seen within the TTL.
func (s *Server) useNonce(n [16]byte) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.nonces.use(n, s.clk.BootMS())
}

// Shutdown stops accepting, closes pre-auth connections and tears down every
// window with reason, waiting (bounded) for teardown to finish. Idempotent.
func (s *Server) Shutdown(reason string) {
	s.mu.Lock()
	s.closed = true
	for ln := range s.listeners {
		ln.Close()
	}
	for c := range s.pre {
		c.Close()
	}
	var ws []*window
	for _, w := range s.windows {
		ws = append(ws, w)
	}
	s.mu.Unlock()
	s.cancel()

	for _, w := range ws {
		w.Close(reason)
	}
	done := make(chan struct{})
	go func() { s.wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(shutdownTimeout):
		log.Printf("agw: shutdown: connections still finishing after %v", shutdownTimeout)
	}
}

// preauthCount and slotOpen are for tests.
func (s *Server) preauthCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.pre)
}

func (s *Server) slotOpen(id byte) bool { return s.slotBusy(id) }

const (
	nonceTTLms = 10_000 // remember a used N this long (BOOTTIME)
	nonceMax   = 1024   // hard bound; at 5 hellos/s only ~50 are live
)

// nonceCache remembers used challenge nonces for nonceTTLms. Not
// goroutine-safe (Server guards it with mu).
type nonceCache struct {
	m map[[16]byte]uint64 // N -> forget-after BOOTTIME ms
}

// use returns false if n was used within the TTL. When the cache is full of
// live entries it fails closed.
func (c *nonceCache) use(n [16]byte, now uint64) bool {
	if c.m == nil {
		c.m = map[[16]byte]uint64{}
	}
	for k, exp := range c.m {
		if now > exp {
			delete(c.m, k)
		}
	}
	if _, seen := c.m[n]; seen {
		return false
	}
	if len(c.m) >= nonceMax {
		return false
	}
	c.m[n] = now + nonceTTLms
	return true
}

// isShutdown is for tests.
func (s *Server) isShutdown() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.closed
}
