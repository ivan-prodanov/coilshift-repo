package summon

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"sync"
	"time"

	"airgapp/gateway/internal/carlink"
	"airgapp/gateway/internal/clock"
)

// D2's timing values (design §5.2, §6.2) and the CAR frame's channel bytes.
const (
	Fresh        = 500 * time.Millisecond // F: a tick age at or under this is fresh (owner 2026-11-27: was 150 ms; the car's LTE round trip is 109-442 ms)
	TripAfter    = 800 * time.Millisecond // verbActive and no liveness instant for longer than this: the trip (owner 2026-11-27: was 300 ms)
	ChanTCP      = byte(0)                // CAR channel byte for dcTcp
	ChanUDP      = byte(1)                // CAR channel byte for dcUdp
	LinkSetAge   = 100 * time.Millisecond // LinkSetRun LIVE ages in a row over this set the link flag
	LinkSetRun   = 3
	LinkClearAge = 75 * time.Millisecond // LinkClearFor of LIVE ages at or under this clear it
	LinkClearFor = 2 * time.Second
)

// The helper's gates 6-9 are gone (owner ruling 2026-11-28): the car's APE handles obstacles and the goal itself, and
// on the LTE link the gates misfired. The engine no longer refuses a verb while another is active (gate 6, "one verb
// at a time") or for a stale tick alone (gate 7, "fresh press"), and D3 is gone: no stale-car-position refusal (gate 8,
// invalid_vehicle_location) and no 3 m floor (gate 9, location_params_invalid). One refusal stays, to keep D2 sound: a
// stale verb that would start a hold with no fresh liveness instant in the last TripAfter gets action_invalid
// (refuseLocked), so an all-stale link cannot drive the car in trip-length pulses.

const (
	tripMS     = uint64(TripAfter / time.Millisecond)
	clearForMS = uint64(LinkClearFor / time.Millisecond)

	cmdResultType = "autopark:cmd_result"
	statusType    = "autopark:status" // the car's Summon status: logged as it changes, never acted on
	endReason     = "error"           // Phone.End's reason: the window closes with BYE "error"

	reasonActionInvalid = "action_invalid" // a stale verb that would start a hold with no fresh liveness in TripAfter

	// arrivalKeep is how many LIVE and HB arrivals the engine keeps for the trip's log line: at the phone's 20 Hz LIVE
	// and 10 Hz HB, TripAfter holds about 24.
	arrivalKeep = 128

	// maxCarTS is the largest cmd_result timestamp the engine takes (2^53, JavaScript's largest exact integer). A
	// larger one is ignored, so a refusal's timestamp is always a well-defined int64.
	maxCarTS = 1 << 53
)

// Car is com.tesla.Webrtc toward QtCarServer. The Link bounds each call with its 100 ms deadlines.
type Car interface { // carlink.Link implements it
	Connect() error         // H1: data_channels_ready
	Send(data []byte) error // PeerCommsToCarserverData
	Disconnect() error      // H4's disconnect map; nil only after the Link's confirmation (Task 4)
}

// Phone is the session's phone side. Car and SetLink are called under the engine lock and must never block.
type Phone interface { // the capability adapter implements it
	Car(channel byte, data []byte) bool // false: the phone link cannot take it
	SetLink(degraded bool)              // called under the engine lock: it must never block (R11)
	End(reason string)                  // called by the engine only after it releases its lock
}

// Marker is the session marker: set before H1, cleared only by a clean H4 in a session where no Car.Send failed.
// While it exists, the finish hook and the supervisor's takeover send the abort and the disconnect themselves.
type Marker interface {
	Set() error
	Clear() error
}

// Config is an Engine's dependencies. Car, Phone, Marker and Clock are required. A nil Now is time.Now, a nil Logf
// drops the lines, and a nil Fatal raises the error as a panic again.
type Config struct {
	Car    Car
	Phone  Phone
	Marker Marker
	Clock  clock.Clock                      // BOOTTIME ms: liveness, the trip
	Now    func() time.Time                 // wall clock: refusal timestamps
	Logf   func(format string, args ...any) // never blocks (Global Constraints)
	Fatal  func(err error)                  // a panic in Stop or Tick; the helper logs it and exits
}

// Engine is one Summon session between the phone and the car: the deadman D2 with its link flag (design §5.2,
// §6.2), the log of the car's Summon status, and the session end H4 (§6.1). D3 (the 3 m floor and the car position's
// staleness) and most verb refusals were dropped by owner ruling 2026-11-28. It is pure: it starts no goroutines and
// reads time only from Config.Clock and Config.Now.
//
// One mutex guards it. Car.Send, Phone.Car and Phone.SetLink are called under it; the Link's emit deadline bounds
// a Send. Phone.End is called only after it is released. Logs carry message types, sizes and the car's status words,
// never positions, the VIN or keys.
type Engine struct {
	cfg Config

	mu         sync.Mutex
	started    bool      // Start ran
	stopped    bool      // Stop ran: every input is ignored
	verbActive bool      // a forwarded verb may be executing on the car
	tripped    bool      // D2 tripped: no HB is forwarded until the next forwarded verb
	lastLive   uint64    // BOOTTIME ms of the latest liveness instant
	liveSet    bool      // a liveness instant came in the session: lastLive means something
	holdAt     uint64    // D2's trip floor: a fresh hold-starting verb's forwarding, or a stale one's last liveness instant
	verbAt     uint64    // BOOTTIME ms when the latest verb was forwarded
	verbType   string    // the latest forwarded verb's msg_type
	resends    int       // verbs of the active verb's type that replaced it in this hold, logged at the hold's end
	staleSends int       // how many of those were stale
	hold       holdStats // this hold's HB and LIVE arrivals, logged at its end
	swallow    int       // injected aborts whose cmd_result is not forwarded
	highestTS  int64     // the highest cmd_result timestamp seen in the session, Unix ms
	sendFailed bool      // a Car.Send failed in the session: H4 never clears the marker

	link      bool   // the link flag
	lateRun   int    // LIVE ages over LinkSetAge in a row, in this hold
	goodRun   bool   // every LIVE age since goodSince was at or under LinkClearAge
	goodSince uint64 // BOOTTIME ms of the first LIVE of that run

	// Diagnostics for the trip's log line: why no liveness instant came. Never acted on.
	liveSeen  bool                 // a LIVE arrived in the session, fresh or not
	liveAt    uint64               // BOOTTIME ms of the latest LIVE's arrival
	liveAge   time.Duration        // its tick age
	liveAgeOK bool                 // its tick was known
	arrivals  [arrivalKeep]arrival // the latest LIVE and HB arrivals, a ring
	arrNext   int                  // the ring's next slot
	arrCount  int                  // arrivals recorded, up to arrivalKeep

	status    carStatus // the car's latest autopark:status, as logged
	hasStatus bool      // an autopark:status arrived in the session

	endAsked bool   // the engine asked for the session's end
	endNow   string // the Phone.End that unlock delivers
}

// NewEngine returns an engine for one session.
func NewEngine(cfg Config) *Engine {
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.Logf == nil {
		cfg.Logf = func(string, ...any) {}
	}
	if cfg.Fatal == nil {
		cfg.Fatal = func(err error) { panic(err) }
	}
	return &Engine{cfg: cfg}
}

// Start sets the session marker, then emits H1. An error refuses the session. After a failed Connect the marker
// stays, because H1 may have reached the car: the finish hook disconnects it. An engine starts once, and never after
// Stop. A car frame the car pushes in answer to H1 waits for Start to return.
func (e *Engine) Start() error {
	e.mu.Lock()
	defer e.unlock()
	if e.started || e.stopped {
		return errors.New("summon: an engine starts once, and never after Stop")
	}
	e.started = true
	if err := e.cfg.Marker.Set(); err != nil {
		return fmt.Errorf("summon: session marker: %w", err)
	}
	if err := e.cfg.Car.Connect(); err != nil {
		return fmt.Errorf("summon: H1: %w", err)
	}
	return nil
}

// Stop is H4, exactly once: cmd_abort if a verb is active, then the disconnect map, then the marker's removal. The
// marker is removed only if no Car.Send failed in the session, and the abort (when one was sent) and the disconnect
// both returned nil. Otherwise it stays, and the supervisor's takeover sends its own abort and disconnect, whatever
// the Car implementation latched. Stop clears the link flag. Later calls do nothing, and every later phone or car
// input is ignored. A panic goes to Config.Fatal: the core would otherwise recover it in Teardown, and a half-done H4
// would go unnoticed.
//
// Stop holds the engine lock across H4. The Link's D-Bus handlers never take it (the capability queues car frames),
// so Disconnect's confirmation round trip is not blocked (R10).
func (e *Engine) Stop(reason string) {
	defer e.recoverTo("Stop")
	e.mu.Lock()
	defer e.unlock()
	if e.stopped {
		return
	}
	e.stopped = true
	aborted := e.verbActive
	var abortErr error
	if e.verbActive {
		abortErr = e.cfg.Car.Send([]byte(AbortJSON))
		e.verbActive = false
		e.holdCountLocked()
	}
	discErr := e.cfg.Car.Disconnect()
	cleared := false
	if !e.sendFailed && abortErr == nil && discErr == nil {
		if err := e.cfg.Marker.Clear(); err != nil {
			e.cfg.Logf("summon: H4: the session marker stays: %v", err)
		} else {
			cleared = true
		}
	}
	e.setLinkLocked(false)
	e.cfg.Logf("summon: H4 (%s): abort %v (err %v), disconnect err %v, an earlier send failed %v, marker cleared %v",
		reason, aborted, abortErr, discErr, e.sendFailed, cleared)
}

// Live is a LIVE frame. age and ok are Window.TickAge's answer for its tick id. A fresh LIVE is a liveness instant;
// every LIVE feeds the link flag.
func (e *Engine) Live(age time.Duration, ok bool) {
	e.mu.Lock()
	defer e.unlock()
	if e.stopped {
		return
	}
	now := e.cfg.Clock.BootMS()
	isFresh := fresh(age, ok)
	if isFresh {
		e.liveLocked(now)
	}
	e.liveSeen, e.liveAt, e.liveAge, e.liveAgeOK = true, now, age, ok
	if e.verbActive {
		e.hold.liveIn++
		if isFresh {
			e.hold.liveFresh.mark(now)
		}
	}
	e.arrivalLocked(now, false, !isFresh)
	e.linkLocked(age, ok, now)
}

// Verb is a VERB frame: cmd_find_me, cmd_forward or cmd_reverse. Owner ruling 2026-11-28 dropped gates 6-9 (the
// car's APE handles obstacles and the goal, and the gates misfired on LTE):
//   - a verb while verbActive replaces the active one (gate 6, "one verb at a time", is gone): the phone re-sends
//     cmd_forward or cmd_reverse about every 250 ms while held, and switches between them quickly;
//   - a stale verb (a late or unknown tick) is forwarded too (gate 7, "fresh press", is gone), with one exception:
//     a stale verb that would start a hold (no verb active, a trip included) is refused with action_invalid unless a
//     liveness instant came at most TripAfter before it. Without it an all-stale link re-sending every 250 ms would
//     restart the car after each trip, moving it in trip-length pulses with no liveness at all;
//   - D3 is gone: no car-position check, no 3 m floor.
//
// A forwarded verb sets verbActive, stamps verbAt and clears tripped. It is a liveness instant only if it is fresh, so
// D2 stays sound: a stale verb never feeds the deadman. The trip clock of a hold a fresh verb starts begins at its
// forwarding (holdAt); a stale verb that starts a hold sets holdAt to the last fresh liveness instant, so with no fresh
// LIVE or HB after it the hold trips TripAfter after that instant: at most TripAfter of motion with no fresh liveness.
// A verb that replaces an active one leaves holdAt alone, so stale re-sends never keep a hold alive.
//
// Logs: a verb that starts a hold, or replaces one of another type, gets a line. A re-send of the active type is only
// counted, and the count is logged once when the hold ends (holdCountLocked). pressID is logged; no rule applies to it.
func (e *Engine) Verb(m Message, age time.Duration, ok bool, pressID uint32) {
	e.mu.Lock()
	defer e.unlock()
	if e.stopped || !e.kindLocked(m, KindVerb) {
		return
	}
	now := e.cfg.Clock.BootMS()
	isFresh := fresh(age, ok)
	// A clock that ran backwards wraps the difference around, which refuses: fail closed.
	if !e.verbActive && !isFresh && (!e.liveSet || now-e.lastLive > tripMS) {
		e.cfg.Logf("summon: %s (press %d) refused: no fresh liveness in %d ms", m.Type, pressID, tripMS)
		e.refuseLocked(m, reasonActionInvalid)
		return
	}
	// verbActive is set even if Send fails: the verb may have reached the car, so H4 aborts it.
	err := e.cfg.Car.Send(m.Canonical)
	stale := ""
	if !isFresh {
		stale = ", stale tick: not a liveness instant"
	}
	switch {
	case !e.verbActive:
		e.holdAt, e.resends, e.staleSends, e.hold = now, 0, 0, holdStats{}
		if !isFresh {
			// A stale verb starts no trip clock of its own: the hold trips TripAfter after the last fresh liveness
			// instant (at most TripAfter ago, as checked above), never later.
			e.holdAt = e.lastLive
		}
		e.cfg.Logf("summon: %s (press %d, %d bytes) forwarded%s", m.Type, pressID, len(m.Canonical), stale)
	case m.Type != e.verbType:
		e.cfg.Logf("summon: %s (press %d, %d bytes) forwarded%s, replacing the active %s (forwarded %d ms before)",
			m.Type, pressID, len(m.Canonical), stale, e.verbType, now-e.verbAt)
	default:
		e.resends++
		if !isFresh {
			e.staleSends++
		}
	}
	if isFresh {
		e.liveLocked(now)
	}
	e.verbActive = true
	e.verbAt, e.verbType = now, m.Type
	e.tripped = false
	if err != nil {
		e.failLocked(m.Type, err)
	}
}

// Heartbeat is an HB frame (heartbeat_app_webrtc). A fresh one is a liveness instant, and it is forwarded unless D2
// tripped since the last forwarded verb. A stale one is neither forwarded nor a liveness instant.
func (e *Engine) Heartbeat(m Message, age time.Duration, ok bool, pressID uint32) {
	e.mu.Lock()
	defer e.unlock()
	if e.stopped || !e.kindLocked(m, KindHeartbeat) {
		return
	}
	now := e.cfg.Clock.BootMS()
	e.arrivalLocked(now, true, !fresh(age, ok))
	if e.verbActive {
		e.hold.hbIn++
	}
	if !fresh(age, ok) {
		if e.verbActive {
			e.hold.hbStale++
		}
		return
	}
	e.liveLocked(now)
	if e.tripped {
		if e.verbActive { // never: a forwarded verb clears tripped; kept so the counts always add up
			e.hold.hbTripped++
		}
		return
	}
	if e.verbActive {
		e.hold.hbFwd.mark(now)
	}
	if err := e.cfg.Car.Send(m.Canonical); err != nil {
		e.failLocked(m.Type, err)
	}
}

// Msg is a MSG frame. cmd_abort always goes to the car as AbortJSON, and ends the hold: verbActive and the link flag
// clear. device_location, info and control:keepalive go as they came. None of them is a liveness instant.
func (e *Engine) Msg(m Message) {
	e.mu.Lock()
	defer e.unlock()
	if e.stopped || !e.kindLocked(m, KindMsg) {
		return
	}
	if m.Type == abortType {
		err := e.cfg.Car.Send([]byte(AbortJSON))
		if e.verbActive {
			e.holdCountLocked()
		}
		e.verbActive = false
		e.holdEndedLocked()
		if err != nil {
			e.failLocked(m.Type, err)
		}
		return
	}
	if err := e.cfg.Car.Send(m.Canonical); err != nil {
		e.failLocked(m.Type, err)
	}
}

// CarFrame is one frame from the car, in the car's order, with its D-Bus channel.
//   - A frame on a channel other than dcTcp and dcUdp is dropped and logged.
//   - An autopark:status is logged when its Summon fields differ from the session's previous one (statusLocked).
//   - Every autopark:cmd_result raises the highest timestamp seen (refuseLocked's floor).
//   - The cmd_result of an abort the engine injected is swallowed: one per injected abort.
//   - A car refusal (result false) does not clear verbActive (design §5.2 overrides Appendix A §2.6).
//   - Everything else goes to the phone, dcTcp as ChanTCP and dcUdp as ChanUDP. A phone link that cannot take a
//     frame ends the session.
func (e *Engine) CarFrame(channel string, data []byte) {
	head := readCarHead(data) // outside the lock: parsing is pure
	e.mu.Lock()
	defer e.unlock()
	if e.stopped {
		return
	}
	var ch byte
	switch channel {
	case carlink.ChannelTCP:
		ch = ChanTCP
	case carlink.ChannelUDP:
		ch = ChanUDP
	default:
		e.cfg.Logf("summon: car frame on channel %.16q dropped (%.64q, %d bytes)", channel, head.msgType, len(data))
		return
	}
	if head.msgType == statusType {
		e.statusLocked(head.status)
	}
	if head.msgType == cmdResultType {
		if head.hasTS && head.ts > e.highestTS {
			e.highestTS = head.ts
		}
		if head.cmdType == abortType && e.swallow > 0 {
			e.swallow--
			e.cfg.Logf("summon: the injected cmd_abort's cmd_result swallowed")
			return
		}
	}
	e.toPhoneLocked(ch, data, head.msgType)
}

// Tick is run every 10 ms by the D2 loop, and does nothing unless a verb is active. D2's trip: if no liveness instant
// came for more than TripAfter, counted from the later of the latest liveness instant and the hold's start (holdAt), it
// injects cmd_abort and logs why (tripWhyLocked); one more abort cmd_result will be swallowed, verbActive clears, and no HB is forwarded until the
// next forwarded verb. The session continues, and the phone learns nothing. A panic goes to Config.Fatal.
func (e *Engine) Tick() {
	defer e.recoverTo("Tick")
	e.mu.Lock()
	defer e.unlock()
	if e.stopped || !e.verbActive {
		return
	}
	now := e.cfg.Clock.BootMS()
	// A clock that ran backwards wraps the difference around, which trips: fail closed.
	idle := now - max(e.lastLive, e.holdAt)
	if idle <= tripMS {
		return
	}
	err := e.cfg.Car.Send([]byte(AbortJSON))
	e.swallow++
	e.verbActive = false
	e.tripped = true
	e.holdEndedLocked()
	e.cfg.Logf("summon: D2 trip: no liveness instant for %d ms, cmd_abort injected; %s", idle, e.tripWhyLocked(now))
	e.holdCountLocked()
	if err != nil {
		e.failLocked("the injected cmd_abort", err)
	}
}

// VerbActive reports whether a forwarded verb may be executing on the car.
func (e *Engine) VerbActive() bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.verbActive
}

// fresh is D2's freshness: the frame's tick is known and at most Fresh old.
func fresh(age time.Duration, ok bool) bool { return ok && age <= Fresh }

// liveLocked stamps a liveness instant: a fresh LIVE, HB or verb.
func (e *Engine) liveLocked(now uint64) {
	e.lastLive, e.liveSet = now, true
}

// arrival is one LIVE or HB frame's arrival, for the trip's log line.
type arrival struct {
	at    uint64 // BOOTTIME ms
	hb    bool   // an HB; else a LIVE
	stale bool   // its tick was unknown or older than Fresh
}

// arrivalLocked records a LIVE or HB arrival in the ring.
func (e *Engine) arrivalLocked(now uint64, hb, stale bool) {
	e.arrivals[e.arrNext] = arrival{at: now, hb: hb, stale: stale}
	e.arrNext = (e.arrNext + 1) % arrivalKeep
	e.arrCount = min(e.arrCount+1, arrivalKeep)
}

// tripWhyLocked says why no liveness instant came, to tell "the phone's frames stopped arriving" from "they arrived,
// stale": the latest LIVE's arrival and tick age, and the LIVE and HB arrivals of the last TripAfter with how many were
// stale. When every kept arrival is within TripAfter, the counts are lower bounds ("at least").
func (e *Engine) tripWhyLocked(now uint64) string {
	last := "no LIVE in the session"
	if e.liveSeen {
		tick := "unknown tick"
		if e.liveAgeOK {
			tick = fmt.Sprintf("tick age %d ms", e.liveAge.Milliseconds())
		}
		last = fmt.Sprintf("last LIVE arrived %d ms ago (%s)", now-e.liveAt, tick)
	}
	var lives, staleLives, hbs, staleHBs, in int
	for i := range e.arrCount {
		a := e.arrivals[i]
		if now-a.at > tripMS { // a clock that ran backwards wraps around: not counted
			continue
		}
		in++
		switch {
		case a.hb:
			hbs++
			if a.stale {
				staleHBs++
			}
		default:
			lives++
			if a.stale {
				staleLives++
			}
		}
	}
	bound := ""
	if in == arrivalKeep {
		bound = "at least "
	}
	return fmt.Sprintf("%s; in the last %d ms %s%d LIVE (%d stale) and %d HB (%d stale)", last, tripMS, bound, lives,
		staleLives, hbs, staleHBs)
}

// holdStats is one hold's HB and LIVE arrivals, for its end's log line. Only frames that arrive while the hold is
// active count.
type holdStats struct {
	hbIn, hbStale, hbTripped int    // HBs arrived; dropped as stale; dropped after a trip
	hbFwd                    series // HBs forwarded to the car
	liveIn                   int    // LIVEs arrived
	liveFresh                series // fresh LIVEs
}

// series counts events and keeps the longest gap between two consecutive ones.
type series struct {
	n       int
	last    uint64 // BOOTTIME ms of the latest event
	longest uint64 // ms
}

func (s *series) mark(now uint64) {
	if s.n > 0 && now >= s.last {
		s.longest = max(s.longest, now-s.last)
	}
	s.n++
	s.last = now
}

// gapText is the longest gap, or "none" with fewer than two events.
func (s series) gapText() string {
	if s.n < 2 {
		return "none"
	}
	return fmt.Sprintf("%d ms", s.longest)
}

// holdCountLocked logs, once per hold, how many re-sends of the active verb's type the hold took and how many of them
// were stale, and the hold's HB and LIVE arrivals (holdStats). The phone's release, D2's trip and H4 end a hold.
func (e *Engine) holdCountLocked() {
	h := e.hold
	e.cfg.Logf("summon: hold ended: %d re-sends (%d stale); "+
		"HB %d arrived, %d forwarded, %d dropped (%d stale, %d after a trip), longest gap between forwarded %s; "+
		"LIVE %d arrived, %d fresh, longest gap between fresh %s",
		e.resends, e.staleSends, h.hbIn, h.hbFwd.n, h.hbStale+h.hbTripped, h.hbStale, h.hbTripped, h.hbFwd.gapText(),
		h.liveIn, h.liveFresh.n, h.liveFresh.gapText())
	e.resends, e.staleSends, e.hold = 0, 0, holdStats{}
}

// refusal is the helper's cmd_result for a refused verb. Its keys are in Go's sorted order, as QtCarServer's are.
type refusal struct {
	CmdType   string `json:"cmd_type"`
	MsgType   string `json:"msg_type"`
	Reason    string `json:"reason"`
	Result    bool   `json:"result"`
	Timestamp int64  `json:"timestamp"`
}

// refuseLocked answers a refused verb on dcTcp. Its timestamp is never below a cmd_result timestamp seen in the
// session, because Tesla's app drops stale results while it waits for a command response.
func (e *Engine) refuseLocked(m Message, reason string) {
	ts := max(e.cfg.Now().UnixMilli(), e.highestTS)
	b, _ := json.Marshal(refusal{CmdType: m.Type, MsgType: cmdResultType, Reason: reason, Timestamp: ts}) // cannot fail
	e.toPhoneLocked(ChanTCP, b, cmdResultType)
}

// kindOf is the frame kind a msg_type travels in (schema.go's table), or 0 for none.
func kindOf(msgType string) Kind {
	if msgType == abortType {
		return KindMsg
	}
	return specs[msgType].frame
}

// kindLocked checks that m arrived in the frame kind its msg_type belongs to. Parse guarantees it; a mismatch is a
// bug upstream, and it ends the session rather than let a message past D2.
func (e *Engine) kindLocked(m Message, k Kind) bool {
	if kindOf(m.Type) == k {
		return true
	}
	e.endLocked(fmt.Sprintf("%.64q in frame kind %d", m.Type, k))
	return false
}

// toPhoneLocked hands a CAR frame to the phone. If the phone link cannot take it, the session ends: Tesla's app has no
// command-ack timer, so a frame is never dropped silently (plan call 6).
func (e *Engine) toPhoneLocked(ch byte, data []byte, msgType string) {
	if !e.cfg.Phone.Car(ch, data) {
		e.endLocked(fmt.Sprintf("the phone link cannot take %.64q (%d bytes)", msgType, len(data)))
	}
}

// linkLocked feeds one LIVE's tick age to the link flag (design §6.2).
//   - LinkSetRun ages in a row over LinkSetAge set it. An unknown tick counts as over.
//   - LinkClearFor of ages at or under LinkClearAge clear it, measured from the first LIVE of an unbroken run of
//     such ages. An age over LinkClearAge breaks the run, and the period restarts at the next good age.
func (e *Engine) linkLocked(age time.Duration, ok bool, now uint64) {
	if !ok || age > LinkSetAge {
		e.lateRun++
	} else {
		e.lateRun = 0
	}
	if ok && age <= LinkClearAge {
		if !e.goodRun {
			e.goodRun, e.goodSince = true, now
		}
	} else {
		e.goodRun = false
	}
	switch {
	case e.lateRun >= LinkSetRun:
		e.setLinkLocked(true)
	case e.goodRun && now-e.goodSince >= clearForMS:
		e.setLinkLocked(false)
	}
}

// holdEndedLocked: the engine forwarded or injected cmd_abort, so the hold ended. The link flag clears, and the next
// hold's LIVEs are counted afresh.
func (e *Engine) holdEndedLocked() {
	e.lateRun, e.goodRun = 0, false
	e.setLinkLocked(false)
}

// setLinkLocked calls Phone.SetLink when, and only when, the link flag changes.
func (e *Engine) setLinkLocked(degraded bool) {
	if e.link == degraded {
		return
	}
	e.link = degraded
	e.cfg.Phone.SetLink(degraded)
}

// carStatus is the car's Summon status, from an autopark:status frame. A field the frame lacks is "".
type carStatus struct {
	autopark, autoparkReason, smart, wait, abort, unavailable string
}

// statusLocked logs the car's Summon status when it differs from the session's previous one: one line per change,
// none for a repeat. Each value is quoted and cut at 48 bytes, since the car chose it.
func (e *Engine) statusLocked(st carStatus) {
	if e.hasStatus && st == e.status {
		return
	}
	e.status, e.hasStatus = st, true
	e.cfg.Logf("summon: car status autopark=%.48q reason=%.48q smart=%.48q wait=%.48q abort=%.48q unavailable=%.48q",
		st.autopark, st.autoparkReason, st.smart, st.wait, st.abort, st.unavailable)
}

// failLocked: a Car.Send failed (a verb, a heartbeat, a message, the phone's abort or the trip's). The session ends,
// and its marker is never cleared: every Send error outside H4 comes here.
func (e *Engine) failLocked(what string, err error) {
	e.sendFailed = true
	e.endLocked(fmt.Sprintf("sending %.64q to the car failed: %v", what, err))
}

// endLocked asks for the session's end, once. unlock calls Phone.End after it releases the lock; the capability's
// Teardown then runs Stop.
func (e *Engine) endLocked(why string) {
	e.cfg.Logf("summon: ending the session: %s", why)
	if e.endAsked {
		return
	}
	e.endAsked = true
	e.endNow = endReason
}

// unlock releases the lock, then delivers the Phone.End this critical section asked for.
func (e *Engine) unlock() {
	reason := e.endNow
	e.endNow = ""
	e.mu.Unlock()
	if reason != "" {
		e.cfg.Phone.End(reason)
	}
}

// recoverTo passes a panic to Config.Fatal. Stop and Tick defer it before they lock, so it runs after unlock.
func (e *Engine) recoverTo(where string) {
	if p := recover(); p != nil {
		e.cfg.Fatal(fmt.Errorf("summon: panic in %s: %v", where, p))
	}
}

// carHead is what the engine reads from a car frame. CarFrame uses cmdType and ts only for a cmd_result, and status
// only for an autopark:status.
type carHead struct {
	msgType string
	cmdType string
	ts      int64 // rounded up, so a refusal never goes below it
	hasTS   bool
	status  carStatus
}

// readCarHead parses a car frame. A frame that is not a JSON object has an empty head and is forwarded as it is.
func readCarHead(data []byte) carHead {
	var obj map[string]any
	if json.Unmarshal(data, &obj) != nil {
		return carHead{}
	}
	var h carHead
	h.msgType, _ = obj["msg_type"].(string)
	h.cmdType, _ = obj["cmd_type"].(string)
	if f, ok := obj["timestamp"].(float64); ok && f >= 0 && f <= maxCarTS {
		h.ts, h.hasTS = int64(math.Ceil(f)), true
	}
	if h.msgType == statusType {
		h.status = readCarStatus(obj)
	}
	return h
}

// readCarStatus reads an autopark:status frame's Summon fields. A string is taken as it is; any other value (a
// number, say) as its JSON text; a missing one is "".
func readCarStatus(obj map[string]any) carStatus {
	field := func(k string) string {
		v, ok := obj[k]
		if !ok {
			return ""
		}
		if str, ok := v.(string); ok {
			return str
		}
		b, _ := json.Marshal(v) // a decoded JSON value: cannot fail
		return string(b)
	}
	return carStatus{
		autopark:       field("autopark_state"),
		autoparkReason: field("autopark_state_reason"),
		smart:          field("smart_summon_state"),
		wait:           field("smart_summon_last_wait_reason"),
		abort:          field("smart_summon_last_abort_reason"),
		unavailable:    field("smart_summon_last_unavailable_reason"),
	}
}
