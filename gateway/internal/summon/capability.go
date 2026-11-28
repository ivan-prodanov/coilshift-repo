package summon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"sync"
	"sync/atomic"
	"time"

	"airgapp/gateway/capability"
	"airgapp/gateway/internal/carlink"
	"airgapp/gateway/internal/clock"
)

// InnerSpec is the inner SUMMON capability's Spec: the car-side plan's Global Constraints, plus PRESENCE (0x18, 8
// bytes) for the presence lease (the 2026-11-26 review fixes' Global Constraints, F5r).
func InnerSpec() capability.Spec {
	return capability.Spec{
		Name:    "SUMMON",
		ID:      0x01,
		In:      map[byte]int{TypeLive: 8, TypeVerb: 1036, TypeHB: 524, TypeMsg: 2048, TypePresence: 8},
		Out:     map[byte]int{TypeCar: 16384},
		LeaseMS: 20000,
		TickHz:  20,
	}
}

// CapConfig is the capability's dependencies. Car, Marker, Clock and Fatal are required. A nil Logf drops the lines, a
// nil Healthy refuses every PreArm, and a nil SetLink drops the link flag.
type CapConfig struct {
	Car     Car
	Marker  Marker
	Clock   clock.Clock                      // BOOTTIME: each engine's clock, and the D2 loop's progress stamps
	Logf    func(format string, args ...any) // never blocks (Global Constraints): it runs on godbus's read goroutine
	Healthy func() bool                      // the link is owned and healthy; checked by PreArm
	SetLink func(bool)                       // writes a LINK record (the helper's relay listener); never blocks (R11)
	Fatal   func(err error)                  // passed to each Engine
	// Live, if set, is called once for each session that opted into the presence lease and whose Start succeeded, as
	// its engine comes up (never for a session already uninstalled by then), and returns that session's off, or nil
	// when it holds nothing (R1). A session that did not opt in never calls it (ruling F1): agw could stretch such a
	// session by about 20 s per held frame, so it must not vouch for the camera. The off runs at the session's
	// uninstall, clean or not, before its Stop. Unlike the marker, which stays after an unclean end so the takeover
	// aborts, Live means "a Summon session is live now": camhelper keys the Summon camera on it (design §6.5). Live and
	// the offs are called one at a time, never under the capability lock. A panic in either is logged and goes no
	// further (L2).
	Live func() (off func())
}

// Capability is the inner SUMMON capability, served by the helper's core. Each window gets an Engine of its own,
// bound to that window: the engine's frames go to that window only, and its Phone.End closes that window only.
//
// Locks. The capability lock mu guards only which session is current, and each session's queue. It is held for a
// few instructions and never across a call out: not across Start, Stop, an engine method, a window method or a Logf.
// So the D-Bus handlers (CarFrame, Signaling, Reply) and End never wait for the engine lock, which Stop holds through
// H4's emits and Disconnect's confirmation (R10).
//
// Goroutines. Each session has one forwarder, started once its Start succeeded and ended by Teardown. It hands that
// session's car frames to its engine in the car's order, and may wait for the engine lock. The D2 loop is Loop's.
type Capability struct {
	cfg CapConfig

	mu  sync.Mutex
	cur *session // the session whose engine is installed, or nil

	liveMu sync.Mutex // serializes CapConfig.Live and its offs; never taken under mu

	progress atomic.Uint64 // BOOTTIME ms of the D2 loop's latest stamp
}

var _ capability.Capability = (*Capability)(nil)

// NewCapability returns the capability, with no session up. It seeds the progress stamp with the clock's current
// value, so Progress measures from here until Loop's first pass. It panics if a required part is missing.
func NewCapability(cfg CapConfig) *Capability {
	if cfg.Car == nil || cfg.Marker == nil || cfg.Clock == nil || cfg.Fatal == nil {
		panic("summon: CapConfig needs Car, Marker, Clock and Fatal")
	}
	if cfg.Logf == nil {
		cfg.Logf = func(string, ...any) {}
	}
	c := &Capability{cfg: cfg}
	c.progress.Store(cfg.Clock.BootMS())
	return c
}

// Spec is InnerSpec.
func (c *Capability) Spec() capability.Spec { return InnerSpec() }

// PreArm refuses unless the link is owned and healthy. A refusal is logged, as a failed Start is: the core logs it
// only as "PreArm refused: error". Neither names the phone's key.
func (c *Capability) PreArm(context.Context, []byte) error {
	if c.cfg.Healthy == nil || !c.cfg.Healthy() {
		c.cfg.Logf("summon: PreArm refused: the car link is not owned and healthy")
		return errors.New("summon: the car link is not owned and healthy")
	}
	return nil
}

// Open starts a session on w. First it reads the authenticated inner ARM params (parseParams): {"presence":1} opts
// the session into the presence lease (presenceCheck), and the lease's stamp is seeded with now, before the session
// is installed. Then Open installs the session's engine as current under the capability lock, so Reply is 1 and the
// car's answer to H1 (its style, status and location, pushed at once) finds the engine. Then Start emits H1 with the
// lock released, since the car may answer inside Connect. The engine's Start is bounded by the Link's emit deadline,
// so Open does not watch ctx.
//
// Once Start succeeded, Open logs once whether the session opted into the presence lease, never the params. Only a
// session that opted in turns the live signal on (ruling F1): the presence lease bounds how long agw can keep it up
// by holding the phone's frames back, and the 20 s lease alone does not. A session that did not opt in keeps Summon
// and the 20 s lease, but not the camera's SUMMON mode.
//
// A failed Start uninstalls the engine and returns an error, so the core closes the connection without a message.
// Its frames are never forwarded: the forwarder starts only after a successful Start. Open refuses params it cannot
// read, before anything else, and refuses while another session is up, which the core's one slot per capability never
// asks for. Each is logged, never with the params: the core logs them only as "Open failed: error". A refused or
// failed Open logs nothing else.
func (c *Capability) Open(_ context.Context, w capability.Window, params json.RawMessage) (capability.Handler, error) {
	presence, err := parseParams(params)
	if err != nil {
		c.cfg.Logf("summon: the session did not start: bad params")
		return nil, err
	}
	s := &session{c: c, w: w, q: make(chan carlink.CarFrame, CarQueue), presence: presence}
	s.echoAt.Store(c.cfg.Clock.BootMS()) // the lease runs from Open; the D2 loop never finds a session unseeded
	s.eng = NewEngine(Config{
		Car:    c.cfg.Car,
		Phone:  windowPhone{s},
		Marker: c.cfg.Marker,
		Clock:  c.cfg.Clock,
		Logf:   c.cfg.Logf,
		Fatal:  c.cfg.Fatal,
	})
	c.mu.Lock()
	busy := c.cur != nil
	if !busy {
		c.cur = s
	}
	c.mu.Unlock()
	if busy {
		c.cfg.Logf("summon: the session did not start: a session is already up")
		return nil, errors.New("summon: a session is already up")
	}
	if err := s.eng.Start(); err != nil {
		c.uninstall(s)
		c.cfg.Logf("summon: the session did not start: %v", err)
		return nil, err
	}
	if s.presence {
		c.cfg.Logf(presenceOnLine)
		c.setLive(s, true)
	} else {
		c.cfg.Logf(presenceOffLine)
	}
	go s.forward()
	return s, nil
}

// Open's line for each session that started: whether the phone opted into the presence lease, and so whether the
// session may turn the live signal on (ruling F1). Fixed texts: the params are never logged.
const (
	presenceOnLine  = "summon: presence lease on for this session"
	presenceOffLine = "summon: presence lease off for this session (the phone did not opt in; no Summon camera)"
)

// errBadParams refuses inner ARM params that parseParams cannot read. It never carries them.
var errBadParams = errors.New("summon: bad params")

// parseParams reads the inner ARM params for the presence lease's opt-in. presence is true only for a JSON object
// whose "presence" is the number 1. No params, JSON null, or an object without "presence" leave it off, and other keys
// are ignored. Any other "presence" value, a value that is not an object, or malformed JSON is errBadParams: json's
// own error is dropped, since it can quote the input.
func parseParams(p json.RawMessage) (presence bool, err error) {
	if len(p) == 0 {
		return false, nil
	}
	var obj map[string]json.RawMessage
	if json.Unmarshal(p, &obj) != nil {
		return false, errBadParams
	}
	v, has := obj["presence"]
	if !has {
		return false, nil
	}
	var n float64
	if json.Unmarshal(v, &n) != nil || n != 1 { // a JSON null leaves n at 0
		return false, errBadParams
	}
	return true, nil
}

// uninstall makes s no longer current, so Reply is 0 and new car frames are dropped, and closes its queue, which ends
// its forwarder once the queued frames are handled. Then s's live signal goes, if s had it. A panic in its off stops
// in setLive, so Teardown's Stop still follows.
func (c *Capability) uninstall(s *session) {
	c.mu.Lock()
	if c.cur == s {
		c.cur = nil
	}
	if !s.qClosed {
		s.qClosed = true
		close(s.q)
	}
	c.mu.Unlock()
	c.setLive(s, false)
}

// setLive turns s's live signal on (Open, after a successful Start, only for a session that opted into the presence
// lease) or off (uninstall), under liveMu. On calls CapConfig.Live and keeps the off it returns, but only while s is
// current, its signal has not ended and it is not on already: a session uninstalled first never turns the signal on.
// Off ends s's signal for good, then takes the off Live returned, if any, and calls it; a session that never turned
// it on has none. A panic in Live or in an off is logged and recovered here, never re-panicked, so Open goes on, and
// Teardown's Stop (H4) still runs after the off (L2).
func (c *Capability) setLive(s *session, on bool) {
	f := c.cfg.Live
	if f == nil {
		return
	}
	c.liveMu.Lock()
	defer c.liveMu.Unlock()
	defer func() {
		if p := recover(); p != nil {
			c.cfg.Logf("summon: the live signal failed: %v", p)
		}
	}()
	if !on {
		s.liveEnded = true
		off := s.liveOff
		s.liveOff = nil
		if off != nil {
			off()
		}
		return
	}
	if c.current() == s && !s.liveEnded && s.liveOff == nil {
		s.liveOff = f()
	}
}

// current is the session whose engine is installed, or nil.
func (c *Capability) current() *session {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.cur
}

// CarFrame is carlink's CarFrame handler, on godbus's read goroutine. It only enqueues: the frame is bound to the
// session current now, read under the capability lock, and queued for that session's forwarder. With no session up
// it is dropped. If the session's queue is full, the frame is dropped and the session's overflow flag set, and the
// forwarder ends the session (plan call 6: a frame is never dropped silently).
func (c *Capability) CarFrame(f carlink.CarFrame) {
	c.mu.Lock()
	s := c.cur
	if s != nil {
		select {
		case s.q <- f:
		default:
			s.overflow.Store(true)
		}
	}
	c.mu.Unlock()
	if s == nil {
		c.cfg.Logf("summon: a car frame between sessions dropped (%d bytes)", len(f.Data))
	}
}

// Signaling is carlink's Signaling handler, on godbus's read goroutine. The car's RemoteToLocalSignaling with an
// integer type 9, which it sends 60 s after a disconnect, closes the current window with reason "close".
func (c *Capability) Signaling(body map[string]any) {
	if !isType9(body["type"]) {
		return
	}
	if s := c.current(); s != nil {
		c.cfg.Logf("summon: the car's signaling type 9 closes the session")
		s.w.Close("close")
	}
}

// isType9 reports whether v is an integer equal to 9, whatever its D-Bus integer type.
func isType9(v any) bool {
	switch n := v.(type) {
	case int:
		return n == 9
	case int8:
		return n == 9
	case int16:
		return n == 9
	case int32:
		return n == 9
	case int64:
		return n == 9
	case uint:
		return n == 9
	case uint8:
		return n == 9
	case uint16:
		return n == 9
	case uint32:
		return n == 9
	case uint64:
		return n == 9
	}
	return false
}

// End closes the current window, if any, with reason: the name was lost, or the link turned unhealthy.
func (c *Capability) End(reason string) {
	if s := c.current(); s != nil {
		s.w.Close(reason)
	}
}

// Reply is CarserverToPeerCommsData_Reply: 1 while a session is up, else 0. It takes only the capability lock.
func (c *Capability) Reply() int32 {
	if c.current() != nil {
		return 1
	}
	return 0
}

// Loop is the D2 loop: every LoopEvery it stamps its progress, then runs the current engine's Tick and the current
// session's presence lease, until ctx ends.
func (c *Capability) Loop(ctx context.Context) {
	t := time.NewTicker(LoopEvery)
	defer t.Stop()
	for {
		c.tick()
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// tick is one pass of the D2 loop. The stamp comes before any lock: an engine stuck while holding its lock stops the
// stamps, and Progress shows it. The presence lease is checked after the engine's Tick (R7), with no lock held: D2
// runs first, and the check never delays it.
func (c *Capability) tick() {
	c.progress.Store(c.cfg.Clock.BootMS())
	if s := c.current(); s != nil {
		s.eng.Tick()
		s.presenceCheck()
	}
}

// Progress is the time since the D2 loop's latest stamp, read without any lock. A clock that ran backwards gives the
// largest Duration: fail closed.
func (c *Capability) Progress() time.Duration {
	last := c.progress.Load() // before the clock: a stamp is never later than a clock read that follows it
	now := c.cfg.Clock.BootMS()
	if now < last || now-last > uint64(math.MaxInt64/int64(time.Millisecond)) {
		return math.MaxInt64
	}
	return time.Duration(now-last) * time.Millisecond
}

// session is one inner window: its engine, the queue of the car frames bound to it, and its forwarder. It is the
// Handler the core drives.
type session struct {
	c   *Capability
	w   capability.Window
	eng *Engine

	q         chan carlink.CarFrame // the car frames bound to this session, in the car's order
	qClosed   bool                  // guarded by c.mu: uninstall closed q
	overflow  atomic.Bool           // a car frame found q full
	liveOff   func()                // guarded by c.liveMu: the off CapConfig.Live returned, until it is called
	liveEnded bool                  // guarded by c.liveMu: the live signal went off; Live is never called for s again

	// The presence lease (F5r). presence is set by Open before the install and only read after it. echoAt is the issue
	// time, in BOOTTIME ms, of the newest tick echo at most PresenceFresh old: Open seeds it, Frame's stamps raise it,
	// and the D2 loop reads it. presenceGone is set once, by the check that closed the window.
	presence     bool
	echoAt       atomic.Uint64
	presenceGone atomic.Bool

	// The forwarder's own state.
	ended  bool   // the forwarder ended the session for an overflow
	caller string // the latest car-frame caller logged
	called bool   // a caller was logged
}

var _ capability.Handler = (*session)(nil)

// forward hands the session's car frames to its engine in order, until Teardown closes the queue. After an overflow
// it ends the session.
func (s *session) forward() {
	for f := range s.q {
		if !s.ended && s.overflow.Load() {
			s.ended = true
			s.c.cfg.Logf("summon: the car-frame queue overflowed (%d frames): the session ends", CarQueue)
			s.w.Close("error")
		}
		s.deliver(f)
	}
}

// deliver logs the frame's caller, then hands the frame to the engine while the window is open. Once the window has
// closed, the phone can take nothing more and Teardown's H4 follows, so the frame is dropped there.
func (s *session) deliver(f carlink.CarFrame) {
	s.logCaller(f)
	select {
	case <-s.w.Done():
		return
	default:
	}
	s.eng.CarFrame(f.Channel, f.Data)
}

// logCaller logs the caller of the session's first car frame with its uid, then each change of caller (design §5.5).
// Frames from every caller are accepted until check 4 identifies QtCarServer's connection.
func (s *session) logCaller(f carlink.CarFrame) {
	if s.called && f.Sender == s.caller {
		return
	}
	if s.called {
		s.c.cfg.Logf("summon: the session's car frames now come from %.64q (uid %.64q)", f.Sender, f.UID)
	} else {
		s.c.cfg.Logf("summon: the session's car frames come from %.64q (uid %.64q)", f.Sender, f.UID)
	}
	s.called, s.caller = true, f.Sender
}

// Frame decodes one phone frame. LIVE, VERB and HB go to the engine with the window's TickAge for their tick id; the
// engine decides D2's freshness. Each tick echo, theirs and PRESENCE's, also goes to the presence lease (stamp); VERB
// and HB go before Parse (R6), and PRESENCE goes no further. Tesla's JSON goes through Parse with the frame's Kind:
// a dropped message is logged by its msg_type and size and skipped, and any other refusal ends the session (BYE
// "error"). Neither the log nor the error carries Parse's text, which can quote a phone-chosen key.
func (s *session) Frame(t byte, body []byte) error {
	switch t {
	case TypeLive:
		tick, err := decodeTick(t, body)
		if err != nil {
			return err
		}
		age, ok := s.w.TickAge(tick)
		s.stamp(age, ok)
		s.eng.Live(age, ok)
	case TypeVerb, TypeHB:
		tick, press, js, err := decodeMotion(t, body)
		if err != nil {
			return err
		}
		age, ok := s.w.TickAge(tick)
		s.stamp(age, ok) // before Parse: a fresh echo proves presence even when its JSON is dropped (R6)
		k := KindVerb
		if t == TypeHB {
			k = KindHeartbeat
		}
		m, keep, err := s.parse(t, k, js)
		if !keep {
			return err
		}
		if t == TypeVerb {
			s.eng.Verb(m, age, ok, press)
		} else {
			s.eng.Heartbeat(m, age, ok, press)
		}
	case TypeMsg:
		m, keep, err := s.parse(t, KindMsg, body)
		if !keep {
			return err
		}
		s.eng.Msg(m)
	case TypePresence: // the presence lease only: never the engine, the car or the link flag
		tick, err := decodeTick(t, body)
		if err != nil {
			return err
		}
		age, ok := s.w.TickAge(tick)
		s.stamp(age, ok)
	default:
		return fmt.Errorf("summon: %s is not a phone frame", frameName(t))
	}
	return nil
}

// stamp takes one tick echo for the presence lease. When the window knows the tick and it is at most PresenceFresh
// old, the tick's issue time, now less its age rounded up to the ms, raises echoAt: an echo agw held back stamps no
// later than the tick it carries. It stamps whether presence is enforced or not; only the close depends on the opt-in.
func (s *session) stamp(age time.Duration, ok bool) {
	if !ok || age < 0 || age > PresenceFresh {
		return
	}
	ms := uint64((age + time.Millisecond - 1) / time.Millisecond)
	now := s.c.cfg.Clock.BootMS()
	if ms > now {
		return // issued before the clock's zero: no issue time to stamp
	}
	issued := now - ms
	for {
		last := s.echoAt.Load()
		if issued <= last || s.echoAt.CompareAndSwap(last, issued) {
			return
		}
	}
}

// presenceCheck is the presence lease, run by the D2 loop after the engine's Tick (R7). In a session that opted in,
// once more than PresenceAfter has passed since echoAt, it logs and closes the window with "lease", once: the core's
// teardown follows (uninstall, which turns the live signal off, then Stop's H4, then BYE lease). A clock that ran
// backwards, below echoAt, counts as expired: fail closed.
func (s *session) presenceCheck() {
	if !s.presence {
		return
	}
	// The stamp before the clock: a stamp is never later than a clock read that follows it. A clock behind the stamp
	// wraps the difference around, which expires.
	last := s.echoAt.Load()
	idle := s.c.cfg.Clock.BootMS() - last
	if idle <= presenceAfterMS || !s.presenceGone.CompareAndSwap(false, true) {
		return
	}
	s.c.cfg.Logf("summon: no fresh presence for %d ms: ending the session (lease)", idle)
	s.w.Close("lease")
}

// parse runs Parse on a frame's JSON. keep is false when the message goes no further: a dropped one, with a nil
// error, or one that ends the session, with an error naming its frame, msg_type and size.
func (s *session) parse(t byte, k Kind, js []byte) (m Message, keep bool, err error) {
	m, err = Parse(k, js)
	switch {
	case err == nil:
		return m, true, nil
	case errors.Is(err, ErrDrop):
		s.c.cfg.Logf("summon: a non-conforming %s %.64q (%d bytes) dropped", frameName(t), msgTypeOf(js), len(js))
		return Message{}, false, nil
	}
	return Message{}, false, fmt.Errorf("summon: a non-conforming %s %.64q (%d bytes): %w",
		frameName(t), msgTypeOf(js), len(js), ErrFatal)
}

// Teardown is H4 (R10): it uninstalls the engine under the capability lock, so Reply is 0 and new car frames are
// dropped, then runs Stop with that lock released. The core calls it once, after reads stop and before BYE.
func (s *session) Teardown(reason string) {
	s.c.uninstall(s)
	s.eng.Stop(reason)
}

// windowPhone is an engine's Phone: the window of the engine's own session, whichever session is current by then.
type windowPhone struct{ s *session }

var _ Phone = windowPhone{}

// Car hands a CAR frame to the window without blocking: false ends the session. Car JSON over MaxCarJSON is dropped
// and logged by its msg_type and size, and the session continues.
func (p windowPhone) Car(ch byte, data []byte) bool {
	if len(data) > MaxCarJSON {
		p.s.c.cfg.Logf("summon: a car frame %.64q of %d bytes dropped: over %d", msgTypeOf(data), len(data), MaxCarJSON)
		return true
	}
	return p.s.w.TrySend(TypeCar, encodeCar(ch, data))
}

// SetLink writes the link flag through CapConfig.SetLink, which never blocks.
func (p windowPhone) SetLink(degraded bool) {
	if f := p.s.c.cfg.SetLink; f != nil {
		f(degraded)
	}
}

// End closes the engine's own window. The engine calls it after it unlocked, so it may come after the session ended
// and the next one opened.
func (p windowPhone) End(reason string) { p.s.w.Close(reason) }
