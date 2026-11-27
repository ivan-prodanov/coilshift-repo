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
	"airgapp/gateway/internal/guards"
)

// D2's timing values (design §5.2, §6.2) and the CAR frame's channel bytes.
const (
	Fresh        = 150 * time.Millisecond // F: a tick age at or under this is fresh
	TripAfter    = 300 * time.Millisecond // verbActive and no liveness instant for longer than this: the trip
	ChanTCP      = byte(0)                // CAR channel byte for dcTcp
	ChanUDP      = byte(1)                // CAR channel byte for dcUdp
	LinkSetAge   = 100 * time.Millisecond // LinkSetRun LIVE ages in a row over this set the link flag
	LinkSetRun   = 3
	LinkClearAge = 75 * time.Millisecond // LinkClearFor of LIVE ages at or under this clear it
	LinkClearFor = 2 * time.Second
)

// D3's values (design §5.3, plan call 2, Task 7's fix round 1).
const (
	FloorM = 3.0 // a verb whose phone position or goal is closer than this to the car is refused (haversine, metres)
	// StaleAfter: the car position goes stale after more than this much hold time, summed over holds, with no car
	// frame, or with no location frame after one that showed a speed other than 0 (in gear D or R at speed 0: this
	// hold's time alone). Only verbActive time counts, from the later of the frame and the verb's forwarding.
	StaleAfter = time.Second
)

const (
	tripMS     = uint64(TripAfter / time.Millisecond)
	clearForMS = uint64(LinkClearFor / time.Millisecond)
	staleMS    = uint64(StaleAfter / time.Millisecond)

	cmdResultType       = "autopark:cmd_result"
	locationType        = "vehicle_data:location"
	reasonActionInvalid = "action_invalid"
	reasonTooClose      = "location_params_invalid"  // D3: the phone position or the goal is within FloorM of the car
	reasonNoCarGPS      = "invalid_vehicle_location" // D3: no car position, or a stale one
	endReason           = "error"                    // Phone.End's reason: the window closes with BYE "error"

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
	Clock  clock.Clock                      // BOOTTIME ms: liveness, staleness, the trip
	Now    func() time.Time                 // wall clock: refusal timestamps
	Logf   func(format string, args ...any) // never blocks (Global Constraints)
	Fatal  func(err error)                  // a panic in Stop or Tick; the helper logs it and exits
}

// Engine is one Summon session between the phone and the car: the deadman D2 with its link flag (design §5.2,
// §6.2), the 3 m floor D3 with the car position's staleness (§5.3, plan call 2), refusals, and the session end H4
// (§6.1). It is pure: it starts no goroutines and reads time only from Config.Clock and Config.Now.
//
// One mutex guards it. Car.Send, Phone.Car and Phone.SetLink are called under it; the Link's emit deadline bounds
// a Send. Phone.End is called only after it is released. Logs carry message types and sizes, never positions.
type Engine struct {
	cfg Config

	mu         sync.Mutex
	started    bool   // Start ran
	stopped    bool   // Stop ran: every input is ignored
	verbActive bool   // a forwarded verb may be executing on the car
	tripped    bool   // D2 tripped: no HB is forwarded until the next forwarded verb
	lastLive   uint64 // BOOTTIME ms of the latest liveness instant
	swallow    int    // injected aborts whose cmd_result is not forwarded
	highestTS  int64  // the highest cmd_result timestamp seen in the session, Unix ms
	sendFailed bool   // a Car.Send failed in the session: H4 never clears the marker

	link      bool   // the link flag
	lateRun   int    // LIVE ages over LinkSetAge in a row, in this hold
	goodRun   bool   // every LIVE age since goodSince was at or under LinkClearAge
	goodSince uint64 // BOOTTIME ms of the first LIVE of that run

	// D3. The car position is the latest vehicle_data:location relayed in the session. Staleness runs only while
	// verbActive (staleLocked), and each window sums the quiet time of the holds since its frame (carryLocked).
	pos        carPos // the car position; pos.ok is false until a readable location frame arrives
	stale      bool   // the position went stale while verbActive; only a newer location frame clears it
	lastCarAt  uint64 // BOOTTIME ms of the latest car frame on dcTcp or dcUdp, of any kind
	lastLocAt  uint64 // BOOTTIME ms of the latest location frame
	verbAt     uint64 // BOOTTIME ms when the current verb was forwarded
	frameCarry uint64 // ms of earlier holds since the latest car frame
	locCarry   uint64 // ms of earlier holds since the latest location frame, if it shows a speed other than 0

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
	if fresh(age, ok) {
		e.lastLive = now
	}
	e.linkLocked(age, ok, now)
}

// Verb is a VERB frame: cmd_find_me, cmd_forward or cmd_reverse. D2's checks, then D3's:
//  1. while verbActive, it is refused with action_invalid;
//  2. unless it is fresh, it is refused with action_invalid;
//  3. D3 (floorLocked): with no car position, or a stale one, it is refused with invalid_vehicle_location; with its
//     phone position or its goal within FloorM of the car, with location_params_invalid;
//  4. otherwise m.Canonical goes to the car, verbActive is set, verbAt is stamped, tripped is cleared, and the
//     forwarding is a liveness instant.
//
// pressID is logged; no rule applies to it.
func (e *Engine) Verb(m Message, age time.Duration, ok bool, pressID uint32) {
	e.mu.Lock()
	defer e.unlock()
	if e.stopped || !e.kindLocked(m, KindVerb) {
		return
	}
	now := e.cfg.Clock.BootMS()
	if e.verbActive { // 1. one verb at a time
		e.refuseLocked(m, pressID, reasonActionInvalid)
		return
	}
	if !fresh(age, ok) { // 2. a late or unknown tick
		e.refuseLocked(m, pressID, reasonActionInvalid)
		return
	}
	if reason := e.floorLocked(m); reason != "" { // 3. D3, the 3 m floor
		e.refuseLocked(m, pressID, reason)
		return
	}
	// 4. Forward. verbActive is set even if Send fails: the verb may have reached the car, so H4 aborts it.
	err := e.cfg.Car.Send(m.Canonical)
	e.verbActive = true
	e.verbAt = now
	e.tripped = false
	e.lastLive = now
	e.cfg.Logf("summon: %s (press %d, %d bytes) forwarded", m.Type, pressID, len(m.Canonical))
	if err != nil {
		e.failLocked(m.Type, err)
	}
}

// Heartbeat is an HB frame (heartbeat_app_webrtc). A fresh one is a liveness instant, and it is forwarded unless D2
// tripped since the last forwarded verb. A stale one is neither forwarded nor a liveness instant.
func (e *Engine) Heartbeat(m Message, age time.Duration, ok bool, pressID uint32) {
	e.mu.Lock()
	defer e.unlock()
	if e.stopped || !e.kindLocked(m, KindHeartbeat) || !fresh(age, ok) {
		return
	}
	e.lastLive = e.cfg.Clock.BootMS()
	if e.tripped {
		return
	}
	if err := e.cfg.Car.Send(m.Canonical); err != nil {
		e.failLocked(m.Type, err)
	}
}

// Msg is a MSG frame. cmd_abort always goes to the car as AbortJSON, and ends the hold: D3 checks the hold and carries
// its quiet time (if a verb was active), then verbActive and the link flag clear. device_location, info and
// control:keepalive go as they came; D3 never checks them. None of them is a liveness instant.
func (e *Engine) Msg(m Message) {
	e.mu.Lock()
	defer e.unlock()
	if e.stopped || !e.kindLocked(m, KindMsg) {
		return
	}
	if m.Type == abortType {
		err := e.cfg.Car.Send([]byte(AbortJSON))
		if e.verbActive { // D3 at the release, as at a tick: a release between two ticks must not escape
			now := e.cfg.Clock.BootMS()
			e.staleLocked(now)
			e.carryLocked(now)
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
//   - A frame on a channel other than dcTcp and dcUdp is dropped and logged. D3 does not see it.
//   - D3: every other frame stamps lastCarAt and resets the frame carry, and a vehicle_data:location becomes the car
//     position (locationLocked).
//   - Every autopark:cmd_result raises the highest timestamp seen.
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
	now := e.cfg.Clock.BootMS()
	e.lastCarAt, e.frameCarry = now, 0 // any frame, the swallowed one too: the car's stream is alive
	if head.msgType == locationType {
		e.locationLocked(head.pos, now)
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

// Tick is run every 10 ms by the D2 loop, and does nothing unless a verb is active. It first applies D3's staleness
// rules (staleLocked), then D2's trip: if no liveness instant came for more than TripAfter, it injects cmd_abort; D3
// carries the hold's quiet time, one more abort cmd_result will be swallowed, verbActive clears, and no HB is forwarded
// until the next forwarded verb. The session continues, and the phone learns nothing. A panic goes to Config.Fatal.
func (e *Engine) Tick() {
	defer e.recoverTo("Tick")
	e.mu.Lock()
	defer e.unlock()
	if e.stopped || !e.verbActive {
		return
	}
	now := e.cfg.Clock.BootMS()
	e.staleLocked(now)
	// A clock that ran backwards wraps the difference around, which trips: fail closed.
	idle := now - e.lastLive
	if idle <= tripMS {
		return
	}
	err := e.cfg.Car.Send([]byte(AbortJSON))
	e.swallow++
	e.carryLocked(now) // D3: the trip ends the hold; staleLocked already checked it at now
	e.verbActive = false
	e.tripped = true
	e.holdEndedLocked()
	e.cfg.Logf("summon: D2 trip: no liveness instant for %d ms, cmd_abort injected", idle)
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
func (e *Engine) refuseLocked(m Message, pressID uint32, reason string) {
	ts := max(e.cfg.Now().UnixMilli(), e.highestTS)
	b, _ := json.Marshal(refusal{CmdType: m.Type, MsgType: cmdResultType, Reason: reason, Timestamp: ts}) // cannot fail
	e.cfg.Logf("summon: %s (press %d) refused: %s", m.Type, pressID, reason)
	e.toPhoneLocked(ChanTCP, b, cmdResultType)
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

// carPos is the car position as D3 knows it, from a vehicle_data:location frame.
type carPos struct {
	ok       bool    // the frame was readable
	lat, lon float64 // degrees
	speed    float64
	shift    string // shift_state: Invalid, P, R, N, D or SNA
}

// rolling reports whether the location showed a speed other than 0 (the firmware's speed may be signed).
func (p carPos) rolling() bool { return p.speed != 0 }

// inDriveGear reports whether the location showed gear D or R.
func (p carPos) inDriveGear() bool { return p.shift == "D" || p.shift == "R" }

// floorLocked is D3 at a verb (design §5.3): the reason to refuse it, or "" to forward it.
//   - No car position, or a stale one: invalid_vehicle_location.
//   - Its phone position or its goal (goalOf) outside WGS84's ranges: location_params_invalid (R28). Such values could
//     wrap onto the car's own position in the car's encoding, and Tesla's app never presses without a fix.
//   - Its phone position or its goal under FloorM from the car: location_params_invalid. The test is !(d >= FloorM),
//     so a NaN distance is refused too (R27); since R28 only an absurd car position can still make one.
func (e *Engine) floorLocked(m Message) string {
	if !e.pos.ok || e.stale {
		return reasonNoCarGPS
	}
	glat, glon := goalOf(m)
	if !latLonInRange(m.Lat, m.Lon) || !latLonInRange(glat, glon) {
		return reasonTooClose
	}
	phone := guards.DistanceM(e.pos.lat, e.pos.lon, m.Lat, m.Lon)
	goal := guards.DistanceM(e.pos.lat, e.pos.lon, glat, glon)
	if !(phone >= FloorM) || !(goal >= FloorM) {
		return reasonTooClose
	}
	return ""
}

// latLonInRange reports whether a position is within WGS84's ranges, the edges included. NaN is not.
func latLonInRange(lat, lon float64) bool {
	return lat >= -90 && lat <= 90 && lon >= -180 && lon <= 180
}

// staleLocked is D3's staleness. It runs only while verbActive: in Tick, and at the phone's release before verbActive
// clears. A window's quiet time is this hold's, measured from the later of its frame and verbAt, summed with the
// earlier holds' since that frame (the carries, which carryLocked adds when a hold ends). Time between holds never
// counts, since this car sends nothing between holds. The position goes stale when
//   - no car frame of any kind came for more than StaleAfter of holds; or
//   - the last location showed a speed other than 0, and no location frame came for more than StaleAfter of holds; or
//   - the last location showed gear D or R at speed 0, and no location frame came for more than StaleAfter of this
//     hold alone (plan call 2's remaining case: the car resends its location only on a change).
//
// A stale position stays stale until a newer location frame (locationLocked). A clock that ran backwards wraps a
// difference around, and the sums saturate, so it makes the position stale: fail closed.
func (e *Engine) staleLocked(now uint64) {
	if !e.pos.ok || e.stale {
		return
	}
	if quiet := satAdd(e.frameCarry, now-max(e.lastCarAt, e.verbAt)); quiet > staleMS {
		e.stale = true
		e.cfg.Logf("summon: D3: the car position is stale: no car frame for %d ms of hold time", quiet)
		return
	}
	quiet := now - max(e.lastLocAt, e.verbAt)
	switch {
	case e.pos.rolling():
		quiet = satAdd(e.locCarry, quiet)
	case e.pos.inDriveGear(): // this hold alone
	default:
		return
	}
	if quiet > staleMS {
		e.stale = true
		e.cfg.Logf("summon: D3: the car position is stale: no location for %d ms of hold time after a moving one",
			quiet)
	}
}

// carryLocked: the phone's release or D2's trip ended a hold, and staleLocked has checked it at now. It adds the hold's
// quiet time to the frame carry, and to the location carry while the last location shows a speed other than 0. Gear
// D or R at speed 0 keeps a per-hold window.
func (e *Engine) carryLocked(now uint64) {
	e.frameCarry = satAdd(e.frameCarry, now-max(e.lastCarAt, e.verbAt))
	if e.pos.rolling() {
		e.locCarry = satAdd(e.locCarry, now-max(e.lastLocAt, e.verbAt))
	}
}

// satAdd is a + b, saturating at the largest uint64: a wrapped sum could bring a clock that ran backwards back under
// StaleAfter.
func satAdd(a, b uint64) uint64 {
	if s := a + b; s >= a {
		return s
	}
	return math.MaxUint64
}

// locationLocked takes a vehicle_data:location frame as the car position, which makes a stale position current and
// resets the location carry. The car sends one only when something changed, so a frame the engine cannot read leaves no
// car position: verbs are refused until a readable one arrives.
func (e *Engine) locationLocked(p carPos, now uint64) {
	switch {
	case !p.ok:
		e.cfg.Logf("summon: D3: a location frame without a readable position: no car position")
	case e.stale:
		e.cfg.Logf("summon: D3: a location frame: the car position is current again")
	}
	e.pos, e.stale, e.lastLocAt, e.locCarry = p, false, now, 0
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

// carHead is what the engine reads from a car frame. CarFrame uses cmdType and ts only for a cmd_result, and pos only
// for a vehicle_data:location.
type carHead struct {
	msgType string
	cmdType string
	ts      int64 // rounded up, so a refusal never goes below it
	hasTS   bool
	pos     carPos
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
	if h.msgType == locationType {
		h.pos = readCarPos(obj)
	}
	return h
}

// readCarPos reads a location frame. It is readable when latitude, longitude and speed are numbers and shift_state is
// a string, as the car always sends them (Appendix A §8.6).
func readCarPos(obj map[string]any) carPos {
	lat, okLat := obj["latitude"].(float64)
	lon, okLon := obj["longitude"].(float64)
	speed, okSpeed := obj["speed"].(float64)
	shift, okShift := obj["shift_state"].(string)
	if !okLat || !okLon || !okSpeed || !okShift {
		return carPos{}
	}
	return carPos{ok: true, lat: lat, lon: lon, speed: speed, shift: shift}
}
