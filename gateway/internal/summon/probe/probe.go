// Package probe is `summonhelper probe`, the no-motion car check (design §8, check 4; plan call 10). It holds
// com.tesla.Webrtc for a set time, as the phone's session would, and reports how the car answers: which car frames
// arrive after H1, how many and how regularly (a frozen stream shows as a long gap), who calls, whether the heartbeats
// echo, whether authd's KeysUpdated reaches this uid, and, through Config.Roles, each -pub key's KeyInfo role.
//
// It originates no verb, abort or device location, and starts heartbeats only after the car's connect-edge push, so
// it never feeds a session another client left open. A heartbeat is the car's dead-man hold: each one re-arms the
// car's 10 s stop timer (design §8.5), so heartbeats fed into a session another client left without a disconnect
// could keep a Summon run going. The car pushes autopark:style only on the false-to-true edge of its connected state
// (Appendix A §1.2), so a style after H1 proves the probe's H1 opened a fresh session. Without one within StyleWait,
// the probe sends no heartbeat and no keepalive: it sends only the disconnect, whose H4 also ends a run another
// client left open, and returns ErrNoStyle.
//
// Its only messages are H1, Tesla's heartbeat_app_webrtc, the control:keepalive and H4's disconnect map, and the
// package has no code path for any other. It links carlink and the session marker, never the summon engine or the
// helper, and phone data reaches the Link only through phone.post, which takes a phoneMsg, not bytes, and encodes it
// itself. It keeps the helper's session marker as the helper does (plan call 12): it refuses while the marker exists,
// before and again after taking the name, sets it before H1, and clears it only after a confirmed disconnect, so a
// probe that dies after H1 leaves the marker for `summonhelper finish`.
package probe

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"text/tabwriter"
	"time"

	"airgapp/gateway/internal/carlink"
	"airgapp/gateway/internal/summon/marker"
)

// The probe's defaults (design §6.1: Tesla's 10 Hz heartbeat, a keepalive every 15 s) and its bounds.
const (
	DefaultDuration       = 120 * time.Second
	DefaultHBEvery        = 100 * time.Millisecond
	DefaultKeepaliveEvery = 15 * time.Second
	DefaultStyleWait      = 2 * time.Second  // how long after H1 the car's autopark:style may take
	CallerWait            = time.Second      // one CallerInfo lookup's bound
	RolesWait             = 10 * time.Second // Roles' bound; the report waits for it at most this long

	heartbeatType = "autopark:heartbeat_app_webrtc"
	echoType      = "autopark:heartbeat_car_webrtc"
	styleType     = "autopark:style"
	keepaliveJSON = `{"msg_type":"control:keepalive"}`

	maxTypes   = 64      // the msg_types listed one by one, the reserved ones included
	maxTypeLen = 64      // a longer msg_type is never listed
	maxCallers = 16      // the callers looked up; the senders of echoes come on top, up to as many again
	maxCarTS   = 1 << 53 // the largest car_timestamp echoed: JavaScript's largest exact integer
)

// reserved are the msg_types the car sends in a session. Each always gets its row, so frames of other types cannot
// crowd them out of the report.
var reserved = map[string]bool{
	echoType: true, styleType: true, "autopark:status": true, "vehicle_data:location": true,
	"autopark:cmd_result": true,
}

var (
	// ErrMarkerPresent refuses a probe while the helper's session marker exists: a session may still be open, and
	// the car would take a second H1 for no change, bringing no frames.
	ErrMarkerPresent = errors.New("probe: refusing: the session marker exists, so a session may still be open: " +
		"run `summonhelper finish` first")
	// ErrNoStyle: no autopark:style came within StyleWait of H1, so the car made no fresh session. Only H1 and the
	// disconnect were sent.
	ErrNoStyle = errors.New("probe: no autopark:style after H1: the car made no fresh session (a session may have " +
		"been open, or the car did not accept the helper); only H1 and the disconnect were sent")
	// ErrNoRunDir: the session marker's directory does not exist, so the marker cannot be set. Nothing was sent.
	ErrNoRunDir = errors.New("probe: cannot set the session marker")

	errUnhealthy = errors.New("probe: the car link turned unhealthy: the name was lost, or an emit failed")
)

// Config is Run's configuration. Bus and MarkerPath are required. A zero Duration, HBEvery, KeepaliveEvery or
// StyleWait takes its default. A nil Now is time.Now, a nil Out discards the report, a nil Logf discards, and a nil
// Exit leaves the Link's os.Exit. A nil CallerInfo lists the callers without their uid and pid; a nil KeysUpdated or
// Roles is skipped.
type Config struct {
	Bus            carlink.Bus
	Duration       time.Duration    // -duration, default 120 s, from H1
	HBEvery        time.Duration    // 100 ms
	KeepaliveEvery time.Duration    // 15 s
	StyleWait      time.Duration    // 2 s: how long after H1 the car's autopark:style may take; tests shorten it
	Now            func() time.Time // the heartbeats' ms, and the report's times

	// CallerInfo is (*carlink.DBusBus).CallerInfo: a caller's uid, and its pid when the bus knows it.
	CallerInfo func(ctx context.Context, sender string) (uid, pid uint32, err error)
	// KeysUpdated is (*carlink.DBusBus).SubscribeKeysUpdated. nil: skipped.
	KeysUpdated func(f func()) error
	// Roles is -keyinfo's lines, "entry 1: role 2", never a key, a fingerprint or the VIN. nil: skipped.
	Roles func(ctx context.Context) ([]string, error)

	// MarkerPath is the helper's session marker. summonhelper passes its compiled-in default, never a typed path, so
	// an error may name its directory.
	MarkerPath string
	Out        io.Writer                        // the report, printed once, after the teardown
	Logf       func(format string, args ...any) // must never block: logq's in summonhelper
	Exit       func(code int)                   // carlink.WithExit, for tests; nil in production
}

// TypeStats is one msg_type's car frames after H1: how many, when the first arrived, and the largest gap between two
// of them or from the last one to the end of the run.
type TypeStats struct {
	Count         int
	First, MaxGap time.Duration
}

// Report is what the probe saw: counts, times and the callers' ids. It never holds a field value of a car frame:
// ByType lists a msg_type only in Tesla's form (lowercase letters and underscores, a colon, lowercase letters and
// underscores).
type Report struct {
	HeartbeatsSent, Echoes int
	ByType                 map[string]TypeStats // msg_type → stats; never a field value
	Callers                []string             // "uid=… pid=… sender=…", in the order they first called
	KeysUpdated            int
	Roles                  []string
}

// check validates cfg and fills in its defaults, touching nothing.
func (cfg Config) check() (Config, error) {
	switch {
	case cfg.Bus == nil:
		return cfg, errors.New("probe: no bus")
	case cfg.MarkerPath == "":
		return cfg, errors.New("probe: no session marker path")
	case cfg.Duration < 0 || cfg.HBEvery < 0 || cfg.KeepaliveEvery < 0 || cfg.StyleWait < 0:
		return cfg, errors.New("probe: a negative duration or period")
	}
	if cfg.Duration == 0 {
		cfg.Duration = DefaultDuration
	}
	if cfg.HBEvery == 0 {
		cfg.HBEvery = DefaultHBEvery
	}
	if cfg.KeepaliveEvery == 0 {
		cfg.KeepaliveEvery = DefaultKeepaliveEvery
	}
	if cfg.StyleWait == 0 {
		cfg.StyleWait = DefaultStyleWait
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.Out == nil {
		cfg.Out = io.Discard
	}
	if cfg.Logf == nil {
		cfg.Logf = func(string, ...any) {}
	}
	return cfg, nil
}

// carControl is the Link without Send: H1, the disconnect, the name and its health.
type carControl interface {
	Connect() error
	Disconnect() error
	Release() error
	OnUnhealthy(f func())
}

// phoneMsg is one of the probe's two phone messages: the keepalive, or a heartbeat with its timestamps.
type phoneMsg struct {
	heartbeat bool
	ms        int64 // a heartbeat's timestamp and created:timestamp
	carTS     int64 // a heartbeat's car_timestamp; 0: omitted
}

// phone is the probe's only way to send the car phone data. Its one method takes a phoneMsg, not bytes, and encodes
// it itself, so no code path can hand the car anything but the keepalive or a heartbeat.
type phone struct{ send func(data []byte) error }

// post encodes m, with sorted keys, and sends it.
func (ph phone) post(m phoneMsg) error {
	data := []byte(keepaliveJSON)
	if m.heartbeat {
		fields := map[string]any{"created:timestamp": m.ms, "msg_type": heartbeatType, "timestamp": m.ms}
		if m.carTS > 0 {
			fields["car_timestamp"] = m.carTS
		}
		var err error
		if data, err = json.Marshal(fields); err != nil {
			return fmt.Errorf("probe: encoding a heartbeat: %w", err)
		}
	}
	return ph.send(data)
}

// acquire takes com.tesla.Webrtc with DO_NOT_QUEUE (carlink.ErrNameTaken when another connection owns it), and hands
// the Link out split in two: carControl, which has no Send, and phone. It is the only place the Link's Send is
// reachable.
func acquire(cfg Config) (carControl, phone, error) {
	link := carlink.NewLink(cfg.Bus, carlink.WithExit(cfg.Exit), carlink.WithLogf(cfg.Logf))
	if err := link.Acquire(); err != nil {
		return nil, phone{}, err
	}
	return link, phone{send: link.Send}, nil
}

// Run is the probe, in the brief's order, as fix round 1 amended it:
//
//  1. while the session marker exists, it refuses, touching nothing;
//  2. it acquires com.tesla.Webrtc with DO_NOT_QUEUE, and refuses when another connection owns it; then it checks
//     the marker again, and refuses if one appeared;
//  3. it serves the car's calls, replying 1, and subscribes the KeysUpdated counter; car frames that arrive before
//     H1 are counted, as a sign that a session may have been open;
//  4. it sets the marker, then sends H1;
//  5. it waits for the car's autopark:style, at most StyleWait (or Duration, if shorter). Without it, it sends
//     nothing more and goes to step 7, and Run returns ErrNoStyle;
//  6. with it, one sender sends a keepalive and a heartbeat at once, then a heartbeat every HBEvery, carrying the last
//     echo's car_timestamp once there is one, and a keepalive every KeepaliveEvery, while Run counts the car frames
//     until Duration has passed since H1, ctx ends, the link turns unhealthy (the name was lost, or an emit failed) or
//     a send fails;
//  7. it stops the sender and waits for it, then sends the disconnect, which the Link confirms with a round trip,
//     clears the marker only if the disconnect was confirmed, and releases the name;
//  8. it prints the report to Out, once.
//
// Every exit after H1 was sent runs step 7, a failed H1 included: H1 may have reached the car. A run that ctx ended
// with a confirmed disconnect returns nil. Otherwise the error says why, and whether the marker stays for
// `summonhelper finish`. The report is returned (and printed) whenever H1 was sent.
func Run(ctx context.Context, cfg Config) (Report, error) {
	cfg, err := cfg.check()
	if err != nil {
		return Report{}, err
	}
	// 1. An open session is ended by `summonhelper finish` first.
	if marker.Present(cfg.MarkerPath) {
		return Report{}, ErrMarkerPresent
	}
	// The car calls a DBusBus drops are logged by size, on godbus's read goroutine: through Logf, which never blocks.
	if b, ok := cfg.Bus.(interface{ SetLogf(func(string, ...any)) }); ok {
		b.SetLogf(cfg.Logf)
	}

	// 2. The name, then the marker again: one that appeared since the check (a second probe, or a hand) is not this
	// probe's to clear. Once the probe holds the name, no engine can start a session.
	car, ph, err := acquire(cfg)
	if err != nil {
		if errors.Is(err, carlink.ErrNameTaken) {
			return Report{}, fmt.Errorf("probe: refusing: %w: stop summonhelper and webrtc-comms first", err)
		}
		return Report{}, fmt.Errorf("probe: requesting %s: %w", carlink.WebrtcName, err)
	}
	if marker.Present(cfg.MarkerPath) {
		return Report{}, errors.Join(ErrMarkerPresent, release(car))
	}

	// 3. The car's calls, and the KeysUpdated counter.
	p := &probe{cfg: cfg, wake: make(chan struct{}, 1), styled: make(chan struct{}), types: map[string]*typeState{}}
	if err := cfg.Bus.Serve(carlink.Handlers{CarFrame: p.carFrame, Reply: func() int32 { return 1 }}); err != nil {
		return Report{}, errors.Join(fmt.Errorf("probe: serving %s: %w", carlink.WebrtcPath, err), release(car))
	}
	unhealthy := make(chan struct{})
	car.OnUnhealthy(func() { close(unhealthy) })
	keysNote := p.watchKeys()
	if err := ctx.Err(); err != nil {
		return Report{}, errors.Join(fmt.Errorf("probe: stopped before H1: %w", err), release(car))
	}

	// 4. The marker, then H1.
	mk := marker.New(cfg.MarkerPath)
	if err := mk.Set(); err != nil {
		return Report{}, errors.Join(markerErr(cfg.MarkerPath, err), release(car))
	}
	finishLookups := p.startLookups()
	roles := p.startRoles(ctx)
	p.openWindow()
	var ended string
	var runErr error
	if err := car.Connect(); err != nil {
		ended, runErr = "H1 failed", fmt.Errorf("probe: H1: %w", err)
	} else {
		cfg.Logf("probe: H1 sent; waiting up to %v for the car's autopark:style", p.gate())
		// 5 and 6. hold returns once the sender, if it ran, has stopped.
		ended, runErr = p.hold(ctx, ph, unhealthy)
	}
	held := p.closeWindow()

	// 7. The disconnect, confirmed; the marker goes only after the confirmation; the name.
	s := summary{held: held, ended: ended, keysNote: keysNote}
	if errors.Is(runErr, ErrNoStyle) {
		s.noStyle = p.gate()
	}
	discErr := car.Disconnect()
	var clearErr error
	switch {
	case discErr != nil:
		s.disconnect = fmt.Sprintf("not confirmed (%v): the session marker stays; run `summonhelper finish` "+
			"before anything else", discErr)
		discErr = fmt.Errorf("probe: the disconnect was not confirmed, so the session marker stays: run "+
			"`summonhelper finish` before anything else: %w", discErr)
	default:
		if clearErr = pathless(mk.Clear()); clearErr != nil {
			s.disconnect = fmt.Sprintf("confirmed, but the session marker stays (%v)", clearErr)
			clearErr = fmt.Errorf("probe: the disconnect was confirmed, but the session marker stays: %w", clearErr)
		} else {
			s.disconnect = "confirmed; the session marker is cleared"
		}
	}
	relErr := car.Release()
	if relErr != nil {
		relErr = fmt.Errorf("probe: releasing %s: %w", carlink.WebrtcName, relErr)
	}
	cfg.Logf("probe: ended (%s) %v after H1; disconnect %s", ended, held.Round(time.Millisecond), s.disconnect)

	// 8. The report.
	p.report(&s, finishLookups())
	s.rep.Roles, s.rolesNote = waitRoles(roles)
	if _, err := cfg.Out.Write(s.text()); err != nil {
		cfg.Logf("probe: writing the report: %v", err)
	}
	return s.rep, errors.Join(runErr, discErr, clearErr, relErr)
}

// release gives the name back after a refusal before H1.
func release(car carControl) error {
	if err := car.Release(); err != nil {
		return fmt.Errorf("probe: releasing %s: %w", carlink.WebrtcName, err)
	}
	return nil
}

// markerErr is Set's failure. A missing directory gets fixed words that name it and say how to make it: the marker
// path is the helper's compiled-in default, never typed. Any other failure drops the path.
func markerErr(path string, err error) error {
	if errors.Is(err, fs.ErrNotExist) {
		dir := filepath.Dir(path)
		return fmt.Errorf("%w: %s is missing; the helper's run script creates it, or create it as root with "+
			"`install -d -o 1337 -g 4950 -m 2750 %s`", ErrNoRunDir, dir, dir)
	}
	return fmt.Errorf("probe: setting the session marker: %w", pathless(err))
}

// pathless is err without the path an *fs.PathError carries: the message stays short, and names no file.
func pathless(err error) error {
	var pe *fs.PathError
	if errors.As(err, &pe) {
		return fmt.Errorf("%s: %w", pe.Op, pe.Err)
	}
	return err
}

// The phases of a run, as the car-frame handler sees them.
const (
	beforeH1 = iota // served, H1 not yet sent: frames are counted as a sign of an open session
	inWindow        // from H1 to the end of the run: frames are counted by msg_type
	afterRun        // the run is over: frames are ignored
)

// probe is one run's state.
type probe struct {
	cfg    Config
	wake   chan struct{} // a new caller to look up
	styled chan struct{} // closed at the first autopark:style after H1

	hbSent atomic.Int64
	keys   atomic.Int64

	mu        sync.Mutex
	phase     int
	t0, end   time.Time
	types     map[string]*typeState
	others    int  // the rows of msg_types that are not reserved
	unlisted  int  // frames whose msg_type is not listed: absent, not in Tesla's form, or past maxTypes
	preH1     int  // frames before H1
	styleSeen bool // styled is closed
	echoes    int
	carTS     int64 // the last echo's car_timestamp; 0 while absent or not a usable integer
	callers   []*caller
	echoExtra int   // callers kept past maxCallers because they sent echoes
	keysSeen  int64 // KeysUpdated at the end of the run
}

type typeState struct {
	count  int
	first  time.Duration
	last   time.Time
	maxGap time.Duration
}

type caller struct {
	sender string
	asked  bool
	line   string
}

// carFrame is the CarFrame handler. It runs on the bus's read goroutine, so it only counts, under a lock that is
// never held across a bus call. Before H1 it counts the frame as one before H1. In the window it counts it by
// msg_type, keeps an echo's car_timestamp, opens the gate at the first autopark:style, and notes a new caller for the
// lookups.
func (p *probe) carFrame(f carlink.CarFrame) {
	mt, listed, carTS, echo := classify(f.Data)
	p.mu.Lock()
	defer p.mu.Unlock()
	switch p.phase {
	case beforeH1:
		p.preH1++
		return
	case afterRun:
		return
	}
	now := p.cfg.Now()
	if !listed || !p.count(mt, now) {
		p.unlisted++
	}
	if echo {
		p.echoes++
		p.carTS = carTS
	}
	if mt == styleType && !p.styleSeen {
		p.styleSeen = true
		close(p.styled)
	}
	p.noteCaller(f.Sender, echo)
}

// count adds one frame of the listable mt arriving at now, and says whether mt has a row. A reserved msg_type always
// gets one; any other only while the others' rows number under maxTypes-len(reserved). p.mu is held.
func (p *probe) count(mt string, now time.Time) bool {
	s := p.types[mt]
	switch {
	case s == nil && !reserved[mt] && p.others >= maxTypes-len(reserved):
		return false
	case s == nil:
		s = &typeState{first: now.Sub(p.t0)}
		p.types[mt] = s
		if !reserved[mt] {
			p.others++
		}
	default:
		if gap := now.Sub(s.last); gap > s.maxGap {
			s.maxGap = gap
		}
	}
	s.count++
	s.last = now
	return true
}

// noteCaller records a sender the first time it calls, and wakes the lookups. It keeps up to maxCallers callers, and
// on top of them up to maxCallers more that send echoes, so the car's own line stays even when other callers came
// first. p.mu is held.
func (p *probe) noteCaller(sender string, echo bool) {
	for _, c := range p.callers {
		if c.sender == sender {
			return
		}
	}
	switch {
	case len(p.callers)-p.echoExtra < maxCallers:
	case echo && p.echoExtra < maxCallers:
		p.echoExtra++
	default:
		return
	}
	p.callers = append(p.callers, &caller{sender: sender})
	select {
	case p.wake <- struct{}{}:
	default:
	}
}

// classify reads a car frame's msg_type. listed is true only when it has Tesla's form, ^[a-z_]+:[a-z_]+$, in at most
// maxTypeLen bytes: no other msg_type is ever printed, so none can carry a value. For an echo (heartbeat_car_webrtc)
// it also reads car_timestamp, 0 unless it is an integer above 0 and at most maxCarTS.
func classify(data []byte) (mt string, listed bool, carTS int64, echo bool) {
	var m map[string]json.RawMessage
	if json.Unmarshal(data, &m) != nil {
		return "", false, 0, false
	}
	if json.Unmarshal(m["msg_type"], &mt) != nil || !listable(mt) {
		return "", false, 0, false
	}
	if mt != echoType {
		return mt, true, 0, false
	}
	return mt, true, carTimestamp(m["car_timestamp"]), true
}

// listable reports whether mt matches ^[a-z_]+:[a-z_]+$ in at most maxTypeLen bytes.
func listable(mt string) bool {
	colon := strings.IndexByte(mt, ':')
	if len(mt) > maxTypeLen || colon <= 0 || colon == len(mt)-1 {
		return false
	}
	for i := 0; i < len(mt); i++ {
		if c := mt[i]; i != colon && (c < 'a' || c > 'z') && c != '_' {
			return false
		}
	}
	return true
}

// carTimestamp is the raw JSON value as QtCarServer's milliseconds: an integer literal above 0 and at most maxCarTS.
// Anything else (a fraction, an exponent, a sign, a string, a longer number) is 0, and the next heartbeat carries no
// car_timestamp. What a heartbeat carries is re-encoded from the integer, never the car's text.
func carTimestamp(raw json.RawMessage) int64 {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 || len(raw) > 16 { // maxCarTS has 16 digits
		return 0
	}
	n, err := strconv.ParseInt(string(raw), 10, 64)
	if err != nil || n <= 0 || n > maxCarTS {
		return 0
	}
	return n
}

// watchKeys subscribes the KeysUpdated counter. Its note says why there is no count: "" when it is watched.
func (p *probe) watchKeys() string {
	if p.cfg.KeysUpdated == nil {
		return "not watched"
	}
	if err := p.cfg.KeysUpdated(func() { p.keys.Add(1) }); err != nil {
		p.cfg.Logf("probe: KeysUpdated is not watched: %v", err)
		return fmt.Sprintf("not watched: %v", err)
	}
	return ""
}

// openWindow starts the count at H1.
func (p *probe) openWindow() {
	p.mu.Lock()
	p.t0, p.phase = p.cfg.Now(), inWindow
	p.mu.Unlock()
}

// closeWindow ends the run: later frames are not counted, and each type's largest gap takes in its tail, from its
// last frame to now. It returns how long the run held the session.
func (p *probe) closeWindow() time.Duration {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.end, p.phase = p.cfg.Now(), afterRun
	for _, s := range p.types {
		if tail := p.end.Sub(s.last); tail > s.maxGap {
			s.maxGap = tail
		}
	}
	p.keysSeen = p.keys.Load()
	return p.end.Sub(p.t0)
}

// gate is how long after H1 the probe waits for the car's autopark:style: StyleWait, or Duration if shorter.
func (p *probe) gate() time.Duration { return min(p.cfg.StyleWait, p.cfg.Duration) }

// hold runs the run after H1. It first waits for the car's autopark:style, at most gate(). Without it, it sends
// nothing and returns ErrNoStyle. With it, it runs the sender until Duration has passed since H1, ctx ends, the link
// turns unhealthy, or a send fails; then it stops the sender and waits for it to return, so nothing is sent once
// hold returns. It says how the run ended, with an error unless the duration ran out or ctx ended.
func (p *probe) hold(ctx context.Context, ph phone, unhealthy <-chan struct{}) (string, error) {
	dur := time.NewTimer(p.cfg.Duration)
	defer dur.Stop()
	wait := time.NewTimer(p.gate())
	defer wait.Stop()
	select {
	case <-p.styled:
		p.cfg.Logf("probe: the car's autopark:style arrived: a fresh session; sending heartbeats and keepalives")
	case <-wait.C:
		return fmt.Sprintf("no autopark:style within %v of H1", p.gate()), ErrNoStyle
	case <-ctx.Done():
		return "cancelled before the car's autopark:style", nil
	case <-unhealthy:
		return "the car link turned unhealthy", errUnhealthy
	}

	sctx, stop := context.WithCancel(context.Background())
	defer stop()
	sent := make(chan error, 1)
	go func() { sent <- p.sender(sctx, ph) }()
	var (
		ended   string
		err     error
		stopped bool
	)
	select {
	case <-dur.C:
		ended = "the duration ran out"
	case <-ctx.Done():
		ended = "cancelled"
	case <-unhealthy:
		ended, err = "the car link turned unhealthy", errUnhealthy
	case err = <-sent:
		ended, stopped = "a send failed", true
	}
	stop()
	if !stopped {
		if serr := <-sent; serr != nil {
			ended, err = "a send failed", serr
		}
	}
	return ended, err
}

// sender is the run's only sender: a keepalive and a heartbeat at once, then a heartbeat every HBEvery and a
// keepalive every KeepaliveEvery. It returns at the first failed send, or nil once ctx ends. It checks ctx before
// every send, the first two included, so after hold cancels it, only a send already under way can still complete.
func (p *probe) sender(ctx context.Context, ph phone) error {
	hb := time.NewTicker(p.cfg.HBEvery)
	defer hb.Stop()
	ka := time.NewTicker(p.cfg.KeepaliveEvery)
	defer ka.Stop()
	if ctx.Err() != nil {
		return nil
	}
	if err := p.keepalive(ph); err != nil {
		return err
	}
	if ctx.Err() != nil {
		return nil
	}
	if err := p.heartbeat(ph); err != nil {
		return err
	}
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-hb.C:
			if ctx.Err() != nil {
				return nil
			}
			if err := p.heartbeat(ph); err != nil {
				return err
			}
		case <-ka.C:
			if ctx.Err() != nil {
				return nil
			}
			if err := p.keepalive(ph); err != nil {
				return err
			}
		}
	}
}

// heartbeat sends Tesla's heartbeat_app_webrtc, with the last echo's car_timestamp when there is one.
func (p *probe) heartbeat(ph phone) error {
	p.mu.Lock()
	carTS := p.carTS
	p.mu.Unlock()
	if err := ph.post(phoneMsg{heartbeat: true, ms: p.cfg.Now().UnixMilli(), carTS: carTS}); err != nil {
		return fmt.Errorf("probe: sending a heartbeat: %w", err)
	}
	p.hbSent.Add(1)
	return nil
}

func (p *probe) keepalive(ph phone) error {
	if err := ph.post(phoneMsg{}); err != nil {
		return fmt.Errorf("probe: sending a keepalive: %w", err)
	}
	return nil
}

// startLookups looks up each caller as it first calls, on a goroutine of its own: a bus call cannot run on the read
// goroutine that delivered the frame. The returned func, called once the window is closed, makes a last pass over
// the callers not yet asked and returns their lines in call order.
func (p *probe) startLookups() func() []string {
	stop, done := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(done)
		for {
			select {
			case <-p.wake:
				p.lookUp()
			case <-stop:
				p.lookUp()
				return
			}
		}
	}()
	return func() []string {
		close(stop)
		<-done
		p.mu.Lock()
		defer p.mu.Unlock()
		lines := make([]string, len(p.callers))
		for i, c := range p.callers {
			lines[i] = c.line
		}
		return lines
	}
}

// lookUp asks CallerInfo about each caller not yet asked, one at a time, without the lock.
func (p *probe) lookUp() {
	for {
		p.mu.Lock()
		var c *caller
		for _, x := range p.callers {
			if !x.asked {
				c, x.asked = x, true
				break
			}
		}
		p.mu.Unlock()
		if c == nil {
			return
		}
		line := p.callerLine(c.sender)
		p.mu.Lock()
		c.line = line
		p.mu.Unlock()
	}
}

func (p *probe) callerLine(sender string) string {
	if p.cfg.CallerInfo == nil {
		return fmt.Sprintf("uid=? pid=? sender=%s", sender)
	}
	ctx, cancel := context.WithTimeout(context.Background(), CallerWait)
	defer cancel()
	uid, pid, err := p.cfg.CallerInfo(ctx, sender)
	if err != nil {
		return fmt.Sprintf("uid=? pid=? sender=%s (lookup failed: %v)", sender, err)
	}
	return fmt.Sprintf("uid=%d pid=%d sender=%s", uid, pid, sender)
}

type rolesResult struct {
	lines []string
	err   error
}

// startRoles asks Roles on a goroutine of its own, so the session's timing never waits for authd. It gets RolesWait,
// and a cancelled run does not cut it short: the report still shows the roles.
func (p *probe) startRoles(ctx context.Context) <-chan rolesResult {
	if p.cfg.Roles == nil {
		return nil
	}
	ch := make(chan rolesResult, 1)
	rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), RolesWait)
	go func() {
		defer cancel()
		lines, err := p.cfg.Roles(rctx)
		ch <- rolesResult{lines, err}
	}()
	return ch
}

// waitRoles waits at most RolesWait for Roles. Its note says why there are no roles: "" when they came.
func waitRoles(ch <-chan rolesResult) ([]string, string) {
	if ch == nil {
		return nil, "not checked"
	}
	t := time.NewTimer(RolesWait)
	defer t.Stop()
	select {
	case r := <-ch:
		if r.err != nil {
			return nil, fmt.Sprintf("failed: %v", r.err)
		}
		return r.lines, ""
	case <-t.C:
		return nil, fmt.Sprintf("no answer within %v", RolesWait)
	}
}

// report fills s with the run's Report and its uncounted frames, the callers' lines included. The window is closed.
func (p *probe) report(s *summary, callers []string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	s.unlisted, s.preH1 = p.unlisted, p.preH1
	s.rep = Report{
		HeartbeatsSent: int(p.hbSent.Load()),
		Echoes:         p.echoes,
		ByType:         make(map[string]TypeStats, len(p.types)),
		Callers:        callers,
		KeysUpdated:    int(p.keysSeen),
	}
	for mt, t := range p.types {
		s.rep.ByType[mt] = TypeStats{Count: t.count, First: t.first, MaxGap: t.maxGap}
	}
}

// summary is the printed report: the Report, and what the run knows about itself.
type summary struct {
	rep                 Report
	held                time.Duration
	ended               string
	noStyle             time.Duration // > 0: no autopark:style came within this long of H1
	preH1, unlisted     int           // frames before H1, and frames with an unlisted msg_type
	keysNote, rolesNote string        // "" when KeysUpdated was watched, and when Roles answered
	disconnect          string
}

// text is the report for Out. It holds msg_types in Tesla's form, counts, times, the callers' ids, the KeysUpdated
// count and the roles' lines, never a value from a car frame.
func (s summary) text() []byte {
	var b bytes.Buffer
	ms := func(d time.Duration) time.Duration { return d.Round(time.Millisecond) }
	fmt.Fprintf(&b, "summonhelper probe: held the session %v after H1; ended: %s\n", ms(s.held), s.ended)
	if s.noStyle > 0 {
		fmt.Fprintf(&b, "no autopark:style within %v of H1: the car made no fresh session (a session may have been "+
			"open, or the car did not accept the helper); only H1 and the disconnect were sent\n", s.noStyle)
	}
	if s.preH1 > 0 {
		fmt.Fprintf(&b, "%d car frame(s) before H1: a session may have been open\n", s.preH1)
	}
	switch {
	case len(s.rep.ByType) == 0 && s.unlisted == 0:
		b.WriteString("car frames after H1: none\n")
	case len(s.rep.ByType) == 0:
		b.WriteString("car frames after H1: none with a listed msg_type\n")
	default:
		types := make([]string, 0, len(s.rep.ByType))
		for mt := range s.rep.ByType {
			types = append(types, mt)
		}
		sort.Slice(types, func(i, j int) bool {
			a, c := s.rep.ByType[types[i]], s.rep.ByType[types[j]]
			if a.First != c.First {
				return a.First < c.First
			}
			return types[i] < types[j]
		})
		b.WriteString("car frames after H1, by msg_type (the largest gap includes the tail to the end):\n")
		tw := tabwriter.NewWriter(&b, 0, 4, 2, ' ', 0)
		fmt.Fprintln(tw, "  msg_type\tcount\tfirst\tlargest gap")
		for _, mt := range types {
			st := s.rep.ByType[mt]
			fmt.Fprintf(tw, "  %q\t%d\t%v\t%v\n", mt, st.Count, ms(st.First), ms(st.MaxGap))
		}
		tw.Flush()
	}
	if s.unlisted > 0 {
		fmt.Fprintf(&b, "  and %d frame(s) with an unlisted msg_type\n", s.unlisted)
	}
	fmt.Fprintf(&b, "heartbeats: %d sent, %d echoed\n", s.rep.HeartbeatsSent, s.rep.Echoes)
	if len(s.rep.Callers) == 0 {
		b.WriteString("callers: none\n")
	} else {
		b.WriteString("callers:\n")
		for _, c := range s.rep.Callers {
			fmt.Fprintf(&b, "  %s\n", c)
		}
	}
	if s.keysNote != "" {
		fmt.Fprintf(&b, "keys updated: %s\n", s.keysNote)
	} else {
		fmt.Fprintf(&b, "keys updated: %d\n", s.rep.KeysUpdated)
	}
	switch {
	case s.rolesNote != "":
		fmt.Fprintf(&b, "roles: %s\n", s.rolesNote)
	case len(s.rep.Roles) == 0:
		b.WriteString("roles: none\n")
	default:
		b.WriteString("roles:\n")
		for _, r := range s.rep.Roles {
			fmt.Fprintf(&b, "  %s\n", r)
		}
	}
	fmt.Fprintf(&b, "disconnect: %s\n", s.disconnect)
	return b.Bytes()
}
