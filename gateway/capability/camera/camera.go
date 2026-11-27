// Package camera is the CAMERA capability (id 0x02) with TALK inside it:
// live single-camera and phone-tiled grid views from the car's multicast
// camera ports, and push-to-talk through the exterior speaker. Wire protocol:
// docs/CAMERA-PROTOCOL.md. Car access goes only through the camera root
// helper (camhelper); each window has its own helper session, so closing a
// window (or a late Teardown of an abandoned one) releases only what that
// window acquired.
package camera

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"sort"
	"strings"
	"sync"
	"time"

	"airgapp/gateway/camhelper/client"
	"airgapp/gateway/camhelper/proto"
	"airgapp/gateway/capability"
)

// Capability id and frame types.
const (
	ID byte = 0x02

	TSelect    byte = 0x20
	TMode      byte = 0x21
	TTalkStart byte = 0x22
	TTalkPCM   byte = 0x23
	TTalkStop  byte = 0x24

	TVideo     byte = 0xA0
	TStreams   byte = 0xA1
	TStatus    byte = 0xA2
	TTalkState byte = 0xA3
)

// Video flow control (bulk-lane bytes).
const (
	DropAbove   = 1 << 20   // start dropping above this
	ResumeBelow = 256 << 10 // resume (at the next IDR) below this
)

const (
	pcmRate       = 48000
	bytesPerMS    = pcmRate * 2 / 1000 // s16 mono
	talkQueueMax  = 45 * 1000 * bytesPerMS
	chunkMS       = 20
	chunkBytes    = chunkMS * bytesPerMS
	pcmLeadChunks = 5 // keep ~100 ms queued at the writer
)

// Views.
var (
	// The grid's four tiles, in stream-index order; the pillars are single views only (owner, 2026-11-27).
	gridViews = []string{"front", "rear", "left_repeater", "right_repeater"}
	// summon: the car's own Summon camera (Tesla's summon webcam) on 9895, only
	// while a SUMMON window is open (design §6.5).
	validViews = map[string]bool{"grid": true, "interior": true, "fascia": true, "summon": true, "left_pillar": true, "right_pillar": true}
	directPort = map[string]int{
		"rear": 9900, "left_repeater": 9897, "right_repeater": 9896, "left_pillar": 9893, "right_pillar": 9892,
	}
)

// isGridView reports whether v is one of the grid's tiles (a valid grid focus).
func isGridView(v string) bool {
	for _, g := range gridViews {
		if g == v {
			return true
		}
	}
	return false
}

func init() {
	for _, v := range gridViews {
		validViews[v] = true
	}
}

// Sources (verified on the car 2026-11-20).
const (
	frontDriving = 9898 // the dashcam front stream: front while driving or during SUMMON
	fasciaPort   = 9901 // the front bumper fisheye; needs sessionActive, which driving and SUMMON drop
	selPort      = 9895 // the selectable camera (GUI_selectableCameraRequest)
)

// Selectable values (GUI_selectableCameraRequest).
const (
	selInterior = 1
	selFront    = 3  // FrontFisheye: the windshield ultrawide, front while parked
	selBackup   = 9  // Backup: the summon camera in R
	selGrid     = 11 // Grid: the summon camera in every gear but D and R
)

// summonSel is Tesla's summon-webcam camera for the gear
// (WebcamCommandHandler::checkWebcamAllowed for a summon session, 2026.38.6):
// Backup in R, the front fisheye in D, Grid otherwise (P, N, unknown). Tesla
// falls back to the front fisheye when its cellular data limit is reached;
// airgapp has no such limit.
func summonSel(gear string) int {
	switch gear {
	case "R":
		return selBackup
	case "D":
		return selFront
	}
	return selGrid
}

// Helper is the part of the camhelper client CAMERA uses.
type Helper interface {
	Hold(context.Context) (proto.Status, error)
	Release(context.Context) error
	TalkArm(context.Context) (proto.Status, error)
	TalkDisarm(ctx context.Context, noEdge bool) error
	Gain(context.Context, bool) error
	Sel(context.Context, int) (proto.Status, error)
	Summon(ctx context.Context, on bool) (proto.Status, error)
	CaptureStart(context.Context, int) error
	CaptureStop(context.Context, int) error
	Status(context.Context) (proto.Status, error)
	WritePCM([]byte) error
	Events() <-chan proto.Event
	Done() <-chan struct{}
	Close() error
}

// DialFunc opens one helper session; onAU receives its access units.
type DialFunc func(onAU client.AUFunc) (Helper, error)

// SocketDialer dials the helper at path.
func SocketDialer(path string) DialFunc {
	return func(onAU client.AUFunc) (Helper, error) {
		c, err := client.Dial(path, onAU)
		if err != nil {
			return nil, err
		}
		return c, nil
	}
}

// Config holds the helper dialer and the timings (zero = car defaults).
type Config struct {
	Dial        DialFunc
	Tick        time.Duration // reconcile: 1 s
	StatusEvery time.Duration // STATUS refresh: 5 s
	CallTimeout time.Duration // per helper call: 2 s
	SlowTimeout time.Duration // hold, sel and talk_arm (several car writes each): 10 s
	SummonPoll  time.Duration // Host().SummonOpen/SummonLinkDegraded poll: 150 ms
	Redial      time.Duration // helper reconnect: 2 s
	Retry       time.Duration // hold / talk / selectable retry: 5 s
	Settle      time.Duration // 9895 is captured only this long after a selectable write: 1 s
	NoVideo     time.Duration // a capture with no AU for this long: no_video, restarted: 12 s
	Stale       time.Duration // streams_ok needs an AU this recent on every stream: 3 s
	TalkTail    time.Duration // gain back to -20 this long after the last queued audio: 1.5 s
	Chunk       time.Duration // PCM pacing chunk: 20 ms
	Log         *log.Logger
}

func (c *Config) defaults() {
	if c.Dial == nil {
		c.Dial = SocketDialer(proto.DefaultSocket)
	}
	set := func(d *time.Duration, v time.Duration) {
		if *d == 0 {
			*d = v
		}
	}
	set(&c.Tick, time.Second)
	set(&c.StatusEvery, 5*time.Second)
	set(&c.CallTimeout, 2*time.Second)
	set(&c.SlowTimeout, 10*time.Second)
	set(&c.SummonPoll, 150*time.Millisecond)
	set(&c.Redial, 2*time.Second)
	set(&c.Retry, 5*time.Second)
	set(&c.Settle, time.Second)
	set(&c.NoVideo, 12*time.Second)
	set(&c.Stale, 3*time.Second)
	set(&c.TalkTail, 1500*time.Millisecond)
	set(&c.Chunk, chunkMS*time.Millisecond)
	if c.Log == nil {
		c.Log = log.Default()
	}
}

type camCap struct{ cfg Config }

// New returns CAMERA talking to the helper at its default socket.
func New() capability.Capability { return NewWithConfig(Config{}) }

// NewWithConfig returns CAMERA with a custom helper dialer or timings.
func NewWithConfig(cfg Config) capability.Capability {
	cfg.defaults()
	return &camCap{cfg: cfg}
}

func (*camCap) Spec() capability.Spec {
	return capability.Spec{
		Name: "CAMERA", ID: ID, LeaseMS: 20000,
		In:   map[byte]int{TSelect: 64, TMode: 32, TTalkStart: 64, TTalkPCM: 16384, TTalkStop: 16},
		Out:  map[byte]int{TVideo: 1 << 20, TStreams: 1024, TStatus: 1024, TTalkState: 256},
		Bulk: map[byte]bool{TVideo: true},
	}
}

func (*camCap) PreArm(context.Context, []byte) error { return nil }

type armParams struct {
	View string `json:"view"`
	Mode string `json:"mode"`
	Talk bool   `json:"talk"`
}

func strictJSON(b []byte, v any) error {
	if len(bytes.TrimSpace(b)) == 0 {
		return nil
	}
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return err
	}
	if dec.More() {
		return errors.New("trailing data")
	}
	return nil
}

func parseArm(raw json.RawMessage) (armParams, error) {
	p := armParams{View: "front", Mode: "full"}
	if err := strictJSON(raw, &p); err != nil {
		return p, fmt.Errorf("camera: ARM params: %w", err)
	}
	if !validViews[p.View] {
		return p, fmt.Errorf("camera: unknown view %q", p.View)
	}
	if p.Mode != "full" && p.Mode != "keyframe" {
		return p, fmt.Errorf("camera: unknown mode %q", p.Mode)
	}
	return p, nil
}

// Open never talks to the helper: it starts the window's goroutines and
// returns at once. Readiness is reported later with STREAMS/STATUS.
func (c *camCap) Open(_ context.Context, w capability.Window, params json.RawMessage) (capability.Handler, error) {
	p, err := parseArm(params)
	if err != nil {
		return nil, err
	}
	h := newHandler(c.cfg, w, p)
	h.start()
	return h, nil
}

// ---- handler ----

type cmdKind int

const (
	cmdSelect cmdKind = iota
	cmdMode
	cmdTalkStart
	cmdTalkStop
)

type cmd struct {
	kind        cmdKind
	view, focus string
	mode        string
}

type streamSpec struct {
	index  byte
	view   string
	port   int
	full   bool
	dewarp bool // the windshield ultrawide (9895 with selectable 3)
}

type handler struct {
	cfg  Config
	w    capability.Window
	talk bool // ARM had talk:true

	cmdMu   sync.Mutex
	cmdq    []cmd         // from Frame; never blocks the window's reader
	cmdC    chan struct{} // 1-buffered: commands are queued
	drained chan struct{} // pacer: the talk queue ran empty
	stop    chan struct{}
	once    sync.Once
	wg      sync.WaitGroup
	start0  time.Time

	vid *video
	tq  *talkQueue

	mu        sync.Mutex // guards the fields below (shared with Teardown and the pacer)
	stopped   bool
	h         Helper
	talkLive  bool // helper talk armed: the pacer feeds the writer
	routeOK   bool
	selCam    string // camera of the selectable hold in progress (for the session log)
	selStart  time.Time
	seenViews []string

	// controller-only state
	view, focus, mode string
	st                proto.Status
	haveStatus        bool
	nextDial          time.Time
	helperDown        bool
	held              bool
	holdReason        string
	nextHold          time.Time
	sel               int
	selReason         string
	nextSel           time.Time
	selSettle         time.Time   // 9895 is not captured before this (the APE may still send the previous camera)
	settle            *time.Timer // reconciles at selSettle
	settleC           <-chan time.Time
	streams           []streamSpec
	running           map[int]time.Time // capture port -> started
	capRetry          map[int]time.Time
	capFails          map[int]int
	noVideo           bool
	talkArmed         bool
	talkErr           string
	nextTalk          time.Time
	routeWasOK        bool
	msgActive         bool
	wantBoost         bool
	boostApplied      bool
	tail              *time.Timer
	tailC             <-chan time.Time
	tailPending       bool
	overflow          bool
	summon            bool // Host().SummonOpen()
	degraded          bool // Host().SummonLinkDegraded()
	summonDone        bool // talk disarmed for this summon
	summonMode        bool // the helper granted SUMMON mode (gates waived, the summon selectable allowed)
	summonMaybe       bool // a summon on timed out: the helper may have granted it (summon off is owed on close)
	nextSummon        time.Time
	knownGear         string // the last known gear: an unknown reading does not switch the summon camera
	summonErr         string // the last SUMMON-mode refusal (logged once per change)
	lastStatus        []byte
	lastStatusAt      time.Time
	lastTalk          talkStateJSON
	lastTalkAt        time.Time
	lastStreams       []byte
}

func newHandler(cfg Config, w capability.Window, p armParams) *handler {
	h := &handler{
		cfg: cfg, w: w, talk: p.Talk,
		cmdC: make(chan struct{}, 1), drained: make(chan struct{}, 1), stop: make(chan struct{}),
		start0: time.Now(),
		vid:    newVideo(w),
		tq:     &talkQueue{max: talkQueueMax},
		view:   p.View, focus: "front", mode: p.Mode,
		running: map[int]time.Time{}, capRetry: map[int]time.Time{}, capFails: map[int]int{},
	}
	h.vid.shut(selPort) // opened by its first capture start
	h.noteView()
	return h
}

func (h *handler) start() {
	h.wg.Add(1)
	go h.run()
	if h.talk {
		h.wg.Add(1)
		go h.pacer()
	}
}

// Frame is called on the window's reader goroutine, in order.
func (h *handler) Frame(t byte, body []byte) error {
	switch t {
	case TSelect:
		var s struct {
			View  string `json:"view"`
			Focus string `json:"focus"`
		}
		if err := strictJSON(body, &s); err != nil {
			return fmt.Errorf("camera: SELECT: %w", err)
		}
		if !validViews[s.View] {
			return fmt.Errorf("camera: SELECT unknown view %q", s.View)
		}
		if s.Focus != "" && (s.View != "grid" || !isGridView(s.Focus)) {
			return fmt.Errorf("camera: SELECT bad focus %q", s.Focus)
		}
		return h.send(cmd{kind: cmdSelect, view: s.View, focus: s.Focus})
	case TMode:
		var m struct {
			Mode string `json:"mode"`
		}
		if err := strictJSON(body, &m); err != nil {
			return fmt.Errorf("camera: MODE: %w", err)
		}
		if m.Mode != "full" && m.Mode != "keyframe" {
			return fmt.Errorf("camera: MODE %q", m.Mode)
		}
		return h.send(cmd{kind: cmdMode, mode: m.Mode})
	case TTalkStart, TTalkStop:
		if !h.talk {
			return errors.New("camera: TALK frame without talk in ARM")
		}
		var empty struct{}
		if err := strictJSON(body, &empty); err != nil {
			return fmt.Errorf("camera: TALK_START/STOP body: %w", err)
		}
		k := cmdTalkStart
		if t == TTalkStop {
			k = cmdTalkStop
		}
		return h.send(cmd{kind: k})
	case TTalkPCM:
		if !h.talk {
			return errors.New("camera: TALK_PCM without talk in ARM")
		}
		if len(body)%2 != 0 {
			return errors.New("camera: TALK_PCM odd length")
		}
		h.tq.push(body)
		return nil
	}
	return fmt.Errorf("camera: unexpected type 0x%02x", t)
}

// maxCmds bounds the commands queued while the controller waits on a slow
// helper call: more means the phone is flooding SELECT/MODE/TALK frames.
const maxCmds = 256

// send queues a command for the controller. It never blocks: a slow helper
// call (up to SlowTimeout) must not stall the window's reader.
func (h *handler) send(c cmd) error {
	h.cmdMu.Lock()
	if len(h.cmdq) >= maxCmds {
		h.cmdMu.Unlock()
		return errors.New("camera: too many queued commands")
	}
	h.cmdq = append(h.cmdq, c)
	h.cmdMu.Unlock()
	select {
	case h.cmdC <- struct{}{}:
	default:
	}
	return nil
}

func (h *handler) takeCmds() []cmd {
	h.cmdMu.Lock()
	defer h.cmdMu.Unlock()
	q := h.cmdq
	h.cmdq = nil
	return q
}

// Teardown is quick and exactly-once safe. It never waits on Done (an
// overrun Open is torn down before its window closes): it stops the
// goroutines, closes this window's helper session (the helper then releases
// what this session holds, in the §4.9 order) and waits at most 300 ms.
func (h *handler) Teardown(reason string) {
	h.once.Do(func() {
		close(h.stop)
		h.mu.Lock()
		h.stopped = true
		hc := h.h
		h.h = nil
		h.mu.Unlock()
		if hc != nil {
			hc.Close()
		}
		h.endSelLog()
		done := make(chan struct{})
		go func() { h.wg.Wait(); close(done) }()
		select {
		case <-done:
		case <-time.After(300 * time.Millisecond):
			h.cfg.Log.Printf("camera: teardown (%s): goroutines still finishing", reason)
		}
	})
}

func (h *handler) recoverGo(name string) {
	if r := recover(); r != nil {
		h.cfg.Log.Printf("camera: %s panic: %v", name, r)
		h.w.Close("error: camera " + name + " panic")
	}
}

// ---- controller ----

func (h *handler) run() {
	defer h.wg.Done()
	defer h.recoverGo("controller")
	defer h.closeHelper()
	tick := time.NewTicker(h.cfg.Tick)
	defer tick.Stop()
	poll := time.NewTicker(h.cfg.SummonPoll)
	defer poll.Stop()
	defer func() {
		if h.settle != nil {
			h.settle.Stop()
		}
	}()
	h.pollHost()
	h.reconcile()
	for {
		var evC <-chan proto.Event
		var doneC <-chan struct{}
		if hc := h.helper(); hc != nil {
			evC, doneC = hc.Events(), hc.Done()
		}
		select {
		case <-h.stop:
			return
		case <-h.w.Done():
			return
		case <-h.cmdC:
			for _, c := range h.takeCmds() {
				h.apply(c)
			}
		case <-poll.C:
			if !h.pollHost() {
				continue // nothing changed: no reconcile at 150 ms
			}
		case ev := <-evC:
			h.onEvent(ev)
		case <-doneC:
			h.helperLost()
		case <-h.drained:
			h.onDrained()
		case <-h.tailC:
			h.onTail()
		case <-h.settleC:
			h.settle, h.settleC = nil, nil // the settle is over: start 9895 now, not at the next tick
		case <-tick.C:
		}
		if h.isStopped() {
			return
		}
		h.reconcile()
	}
}

func (h *handler) isStopped() bool {
	select {
	case <-h.stop:
		return true
	default:
		return false
	}
}

func (h *handler) helper() Helper {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.h
}

func (h *handler) closeHelper() {
	h.mu.Lock()
	hc := h.h
	h.h = nil
	h.talkLive = false
	h.mu.Unlock()
	if hc != nil {
		hc.Close()
	}
	h.endSelLog()
}

func (h *handler) ctx() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), h.cfg.CallTimeout)
}

// slowCtx bounds hold, sel and talk_arm, which run several car writes.
func (h *handler) slowCtx() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), h.cfg.SlowTimeout)
}

// pollHost reads the SUMMON flags; it reports whether either changed.
func (h *handler) pollHost() bool {
	host := h.w.Host()
	if host == nil {
		return false
	}
	summon, degraded := host.SummonOpen(), host.SummonLinkDegraded()
	if summon == h.summon && degraded == h.degraded {
		return false
	}
	if summon != h.summon {
		h.cfg.Log.Printf("camera: SUMMON %s", map[bool]string{true: "open: talk off, the car's summon camera", false: "closed: resuming"}[summon])
		if summon {
			h.summonDone = false
			h.nextSummon, h.summonErr = time.Now(), ""
			h.knownGear = "" // this window's own readings only: an unknown first one is Grid, as Tesla's
			h.tq.reset()
			h.msgActive, h.wantBoost = false, false
			h.stopTail()
		} else {
			now := time.Now()
			h.nextHold, h.nextSel, h.nextTalk = now, now, now
			if h.talkErr == "summon" {
				h.talkErr = ""
			}
		}
	}
	h.summon, h.degraded = summon, degraded
	return true
}

// helperLost forgets everything the dead session held: the helper released
// it. The next reconcile redials and re-acquires.
func (h *handler) helperLost() {
	h.closeHelper()
	h.helperDown = true
	h.nextDial = time.Now().Add(h.cfg.Redial)
	h.held, h.sel, h.talkArmed, h.boostApplied = false, 0, false, false
	h.summonMode, h.summonMaybe = false, false // the helper ended it with the session
	h.knownGear = ""
	h.setRoute(false)
	h.running = map[int]time.Time{}
	h.vid.markAll()
}

// codeTimeout marks a helper call that ran out of time: retry later.
const codeTimeout = "timeout"

// failed handles a helper call error: a refusal code is returned; a timeout
// means "retry later" (the helper may still complete it; a late reply is
// dropped by request id and every op is idempotent); anything else (the
// session closed) drops the session.
func (h *handler) failed(err error) string {
	if err == nil {
		return ""
	}
	if code := client.Code(err); code != "" {
		return code
	}
	if errors.Is(err, context.DeadlineExceeded) {
		h.cfg.Log.Printf("camera: helper call timed out; retrying later")
		return codeTimeout
	}
	if !h.isStopped() {
		h.cfg.Log.Printf("camera: helper call failed: %v", err)
		h.helperLost()
	}
	return ""
}

func (h *handler) apply(c cmd) {
	switch c.kind {
	case cmdSelect:
		h.view = c.view
		if c.view == "grid" && c.focus != "" {
			h.focus = c.focus
		}
		h.noteView()
	case cmdMode:
		h.mode = c.mode
	case cmdTalkStart:
		h.msgActive, h.wantBoost, h.overflow = true, true, false
		h.stopTail()
	case cmdTalkStop:
		h.msgActive = false
		if h.tq.len() == 0 {
			h.startTail()
		}
	}
}

func (h *handler) onDrained() {
	if !h.msgActive && h.tq.len() == 0 {
		h.startTail()
	}
}

func (h *handler) startTail() {
	h.stopTail()
	h.tail = time.NewTimer(h.cfg.TalkTail)
	h.tailC = h.tail.C
	h.tailPending = true
}

func (h *handler) stopTail() {
	if h.tail != nil {
		h.tail.Stop()
	}
	h.tail, h.tailC, h.tailPending = nil, nil, false
}

func (h *handler) onTail() {
	h.tail, h.tailC, h.tailPending = nil, nil, false
	if !h.msgActive && h.tq.len() == 0 {
		h.wantBoost = false
	}
}

func (h *handler) onEvent(ev proto.Event) {
	now := time.Now()
	switch ev.Ev {
	case proto.EvStatus:
		if ev.Status != nil {
			h.st, h.haveStatus = *ev.Status, true
			if ev.Status.Talk {
				h.setRoute(ev.Status.Route == "ok")
			}
		}
	case proto.EvHoldEnd:
		h.held, h.holdReason, h.nextHold = false, ev.Reason, now.Add(h.cfg.Retry)
		h.talkEnded(ev.Reason)
	case proto.EvTalkEnd:
		h.talkEnded(ev.Reason)
	case proto.EvSelEnd:
		h.endSelLog()
		h.vid.shut(selPort) // nothing of the ended camera reaches a later 9895 stream
		h.sel, h.selReason, h.nextSel = 0, proto.ErrSelRefused, now.Add(h.selRetry())
	case proto.EvCapEnd:
		if ev.Port == selPort {
			h.vid.shut(selPort)
		} else {
			h.vid.clear(ev.Port) // the restarted capture starts a new GOP
		}
		delete(h.running, ev.Port)
		h.capFails[ev.Port]++
		h.capRetry[ev.Port] = now.Add(capBackoff(h.capFails[ev.Port]))
	case proto.EvRoute:
		h.setRoute(ev.Reason == "ok")
	case proto.EvSummonEnd:
		// The car's Summon session ended (its sel_end comes too): ask again,
		// it is refused until a new Summon session starts.
		h.summonMode, h.summonMaybe, h.nextSummon = false, false, now.Add(h.cfg.Tick)
	}
}

func (h *handler) talkEnded(reason string) {
	if h.talkArmed {
		h.talkArmed, h.boostApplied = false, false
		h.talkErr = reason
		h.nextTalk = time.Now().Add(h.cfg.Retry)
	}
	h.setRoute(false)
	h.mu.Lock()
	h.talkLive = false
	h.mu.Unlock()
}

func (h *handler) setRoute(ok bool) {
	h.mu.Lock()
	h.routeOK = ok
	h.mu.Unlock()
	if ok {
		h.routeWasOK = true
	}
}

func capBackoff(n int) time.Duration {
	d := time.Duration(n) * time.Second
	if d > 10*time.Second {
		d = 10 * time.Second
	}
	return d
}

// reconcile drives the helper session toward the wanted state. Every helper
// call is bounded (CallTimeout, SlowTimeout for hold/sel/talk_arm); a timeout
// is retried later and never drops the session.
func (h *handler) reconcile() {
	defer h.emit()
	now := time.Now()
	hc := h.helper()
	if hc == nil {
		if now.Before(h.nextDial) {
			return
		}
		nc, err := h.cfg.Dial(h.vid.onAU)
		if err != nil {
			h.helperDown = true
			h.nextDial = now.Add(h.cfg.Redial)
			return
		}
		h.mu.Lock()
		if h.stopped {
			h.mu.Unlock()
			nc.Close()
			return
		}
		h.h = nc
		h.mu.Unlock()
		hc = nc
		h.helperDown = false
		h.nextHold, h.nextSel, h.nextTalk = now, now, now
		h.summonDone = false
		ctx, cancel := h.ctx()
		st, err := hc.Status(ctx)
		cancel()
		if err != nil {
			h.failed(err)
			if h.helper() == nil {
				return
			}
		} else {
			h.st, h.haveStatus = st, true
		}
	}

	if h.st.Gear != "" {
		h.knownGear = h.st.Gear
	}

	// 0. SUMMON (design §6.5, copying Tesla's summon webcam). While a window
	// is open: talk off (once; idempotent on the helper), the hold stays, and
	// SUMMON mode is asked of the helper until granted (it waives the gear,
	// Sentry and camera-access gates only while the car's Summon session is
	// live); the selectable then follows the gear (wantSel). When the window
	// closes: the selectable back to 0 first, then the mode off.
	if h.summon {
		if !h.summonDone {
			if !h.summonTalkOff(hc) {
				if h.helper() == nil {
					return
				}
			} else {
				h.summonDone = true
			}
		}
		// summon on only once talk is off (the helper refuses talk in SUMMON mode too).
		if h.summonDone && !h.summonMode && !now.Before(h.nextSummon) {
			ctx, cancel := h.ctx()
			st, err := hc.Summon(ctx, true)
			cancel()
			if st != (proto.Status{}) {
				h.st, h.haveStatus = st, true
			}
			if err == nil {
				h.summonMode, h.summonMaybe, h.summonErr = true, false, ""
				h.nextHold, h.nextSel = now, now
				h.cfg.Log.Printf("camera: SUMMON mode granted: the car's summon camera on 9895")
			} else {
				code := h.failed(err)
				if h.helper() == nil {
					return
				}
				// A timeout may still be granted on the helper: summon off is
				// owed if the window closes first. A refusal means no mode.
				h.summonMaybe = code == codeTimeout
				if code != h.summonErr {
					h.cfg.Log.Printf("camera: SUMMON mode refused (%s); asking again every %v", code, h.cfg.Tick)
					h.summonErr = code
				}
				h.nextSummon = now.Add(h.cfg.Tick)
			}
		}
	} else if h.summonMode || h.summonMaybe {
		if h.sel != 0 {
			h.stopCapture(hc, selPort)
			ctx, cancel := h.slowCtx()
			_, err := hc.Sel(ctx, 0)
			cancel()
			if code := h.failed(err); code == "" {
				h.endSelLog()
				h.sel = 0
				delete(h.running, selPort)
			} else {
				h.sel = -1 // unknown: release again on the next pass
			}
			if h.helper() == nil {
				return
			}
		}
		if h.sel == 0 {
			ctx, cancel := h.ctx()
			_, err := hc.Summon(ctx, false)
			cancel()
			if err == nil {
				h.summonMode, h.summonMaybe = false, false
				h.cfg.Log.Printf("camera: SUMMON mode off")
			} else if h.failed(err); h.helper() == nil {
				return
			}
		}
	}

	// 1. Hold WEBCAM_webcamSessionActive (the parked APE multicasts then),
	// also while SUMMON is open: Tesla's summon webcam holds it too.
	if !h.held && !now.Before(h.nextHold) {
		ctx, cancel := h.slowCtx()
		st, err := hc.Hold(ctx)
		cancel()
		if st != (proto.Status{}) {
			h.st, h.haveStatus = st, true
		}
		if err == nil {
			h.held, h.holdReason = true, ""
		} else {
			h.holdReason = h.failed(err)
			h.nextHold = now.Add(h.cfg.Retry)
			if h.helper() == nil {
				return
			}
		}
	}

	// 2. Selectable: interior's 1 while it is selected, otherwise front's 3
	// while parked and held; in SUMMON mode the summon camera for the gear, in
	// any gear (see wantSel). Every write is preceded by stopping the 9895
	// capture (its AUs are dropped from then on) and is released (0) before
	// another value is written. h.sel < 0 means unknown (a call timed out).
	want := h.wantSel()
	if h.sel == want {
		h.selReason = ""
	} else {
		if h.sel != 0 {
			h.stopCapture(hc, selPort)
			ctx, cancel := h.slowCtx()
			_, err := hc.Sel(ctx, 0)
			cancel()
			if code := h.failed(err); code == "" {
				h.endSelLog()
				h.sel = 0
				delete(h.running, selPort) // sel 0 also ends this session's 9895 capture on the helper
			} else {
				h.sel = -1 // unknown: release again on the next pass
			}
			if h.helper() == nil {
				return
			}
		}
		h.selReason = ""
		if want != 0 && h.sel == 0 {
			if h.st.Gear != "P" && h.haveStatus && !(h.summon && h.summonMode) {
				h.selReason = proto.ErrSelRefused
			} else if !now.Before(h.nextSel) {
				h.stopCapture(hc, selPort) // normally stopped already: never capture across a change
				if h.helper() == nil {
					return
				}
				ctx, cancel := h.slowCtx()
				st, err := hc.Sel(ctx, want)
				cancel()
				if st != (proto.Status{}) {
					h.st = st
				}
				switch code := h.failed(err); {
				case err == nil:
					h.sel = want
					h.beginSelLog(selName(want))
					h.startSettle()
				case code == codeTimeout:
					// It may still succeed on the helper: log the hold
					// conservatively and release it on the next pass.
					h.beginSelLog(selName(want))
					h.sel = -1
					h.nextSel = now.Add(h.cfg.Retry)
				default:
					h.selReason = proto.ErrSelRefused
					h.nextSel = now.Add(h.selRetry())
					if h.helper() == nil {
						return
					}
				}
			} else {
				h.selReason = proto.ErrSelRefused
			}
		}
	}

	// 3. Streams and captures. Captures are warm only while parked (gear P)
	// with the sessionActive hold: the five direct views, fascia (9901) and
	// the selectable 9895 (front's 3 by default) run, whatever is selected,
	// so a SELECT is answered from the GOP cache at once. While driving or
	// while SUMMON is open (the MCU also runs the UI and navigation) only the
	// viewed streams are captured; the 150 ms SUMMON poll or the status event
	// with the new gear reconciles at once, so the unwatched captures stop (or
	// warm again) without waiting for a tick. 9895 runs only during a known
	// selectable hold and starts only after its settle.
	h.setStreams(h.wantStreams())
	wantPorts := h.capturePorts(now)
	streamPort := map[int]string{}
	for _, s := range h.streams {
		streamPort[s.port] = s.view
	}
	for port := range h.running {
		if !wantPorts[port] {
			h.stopCapture(hc, port)
		}
	}
	h.noVideo = false
	ports := make([]int, 0, len(wantPorts))
	for p := range wantPorts {
		ports = append(ports, p)
	}
	sort.Ints(ports)
	for _, port := range ports {
		_, watched := streamPort[port]
		started, running := h.running[port]
		if running {
			last := h.vid.lastAU(port)
			if last.Before(started) {
				last = started
			}
			if now.Sub(last) >= h.cfg.NoVideo {
				h.noVideo = h.noVideo || watched
				if h.summon && watched {
					h.w.LogEvent(fmt.Sprintf("capture_dead_during_summon port=%d view=%s", port, streamPort[port]))
				}
				h.stopCapture(hc, port) // restart below on a later pass
				h.capFails[port]++
				h.capRetry[port] = now.Add(capBackoff(h.capFails[port]))
			}
			continue
		}
		if h.capFails[port] > 0 && watched {
			h.noVideo = true
		}
		if now.Before(h.capRetry[port]) {
			continue
		}
		if port == selPort {
			h.vid.open(selPort) // a new capture: an empty cache, and AUs flow again
		}
		ctx, cancel := h.ctx()
		err := hc.CaptureStart(ctx, port)
		cancel()
		if err == nil {
			h.running[port] = now
			continue
		}
		h.failed(err)
		if h.helper() == nil {
			return
		}
		h.capFails[port]++
		h.capRetry[port] = now.Add(capBackoff(h.capFails[port]))
	}
	for port := range h.capFails {
		if !h.vid.lastAU(port).IsZero() && now.Sub(h.vid.lastAU(port)) < h.cfg.Stale {
			delete(h.capFails, port)
		}
	}

	// 4. Talk (never while SUMMON is open).
	// Never while SUMMON is open or its mode may still be on: talk comes back
	// only after summon off was confirmed (sel 0, summon off, then talk).
	if h.talk && !h.summon && !h.summonMode && !h.summonMaybe && h.held && !h.talkArmed && !now.Before(h.nextTalk) {
		ctx, cancel := h.slowCtx()
		st, err := hc.TalkArm(ctx)
		cancel()
		if st != (proto.Status{}) {
			h.st = st
		}
		if err == nil {
			h.talkArmed, h.talkErr, h.boostApplied = true, "", false
			h.setRoute(st.Route == "ok")
			h.mu.Lock()
			h.talkLive = true
			h.mu.Unlock()
		} else {
			h.talkErr = h.failed(err)
			if h.talkErr == codeTimeout {
				h.talkErr = "" // still arming
			}
			h.nextTalk = now.Add(h.cfg.Retry)
			if h.helper() == nil {
				return
			}
		}
	}
	if h.talkArmed && h.wantBoost != h.boostApplied {
		ctx, cancel := h.ctx()
		err := hc.Gain(ctx, h.wantBoost)
		cancel()
		if err == nil {
			h.boostApplied = h.wantBoost
		} else if h.failed(err); h.helper() == nil {
			return
		}
	}
}

// summonTalkOff disarms talk for an open SUMMON window (talk never runs during
// SUMMON). The hold stays, so the helper fires its usual sessionActive edge,
// which un-routes the exterior stream. It reports whether the disarm went
// through (else it is retried).
func (h *handler) summonTalkOff(hc Helper) bool {
	h.mu.Lock()
	h.talkLive = false
	h.mu.Unlock()
	if !h.talk {
		return true
	}
	ctx, cancel := h.ctx()
	err := hc.TalkDisarm(ctx, false)
	cancel()
	h.talkArmed, h.boostApplied, h.talkErr = false, false, "summon"
	h.setRoute(false)
	if err != nil {
		h.failed(err)
		return false
	}
	return true
}

// MaxCaptures is the helper's per-session capture limit: the warm set (five
// direct views, fascia 9901 and the selectable 9895) fits.
const MaxCaptures = 8

// parkedWarm reports whether the warm set runs: parked (gear P) with the
// sessionActive hold and no SUMMON window.
func (h *handler) parkedWarm() bool {
	return h.held && !h.summon && h.st.Gear == "P" && !h.st.Driving
}

// wantSel is the selectable to hold: in SUMMON mode with the hold, the summon
// camera for the gear (summonSel), whatever view is shown, as Tesla's summon
// webcam holds it for the whole session; 0 while SUMMON is open without the
// mode, or closing; else 1 while interior is selected; else 3 (front, the
// windshield ultrawide) while the warm set runs, whatever view is shown, so
// front is answered from the warm 9895 capture; else 0 (driving, no hold).
func (h *handler) wantSel() int {
	switch {
	case h.summon || h.summonMode || h.summonMaybe:
		if h.summon && h.summonMode && h.held {
			return summonSel(h.knownGear)
		}
		return 0
	case h.view == "interior":
		return selInterior
	case h.parkedWarm():
		return selFront
	}
	return 0
}

// selName names a selectable camera for the session log.
func selName(v int) string {
	switch v {
	case selInterior:
		return "interior"
	case selFront:
		return "front"
	case selBackup:
		return "backup"
	case selGrid:
		return "grid"
	}
	return fmt.Sprintf("selectable_%d", v)
}

// selRetry is how soon a refused or ended selectable is asked for again: the
// usual Retry, but one tick in SUMMON mode, where a refusal means only that
// the gear changed under the request (the reply's status carries the new one).
func (h *handler) selRetry() time.Duration {
	if h.summon && h.summonMode {
		return h.cfg.Tick
	}
	return h.cfg.Retry
}

// startSettle runs after a selectable write: the APE can keep sending the
// previous camera on 9895 for up to about 1 s, so 9895 is captured only once
// Settle has passed, and a reconcile is scheduled for that moment.
func (h *handler) startSettle() {
	h.selSettle = time.Now().Add(h.cfg.Settle)
	delete(h.capFails, selPort) // a new camera: earlier failures do not count
	delete(h.capRetry, selPort)
	if h.settle != nil {
		h.settle.Stop()
	}
	h.settle = time.NewTimer(h.cfg.Settle)
	h.settleC = h.settle.C
}

// capturePorts is the set of ports to capture: the current streams, plus the
// warm set while parked and held (the five direct views, fascia's 9901, and
// 9895 while a selectable is known to be held). 9895 waits for its settle.
func (h *handler) capturePorts(now time.Time) map[int]bool {
	want := map[int]bool{}
	for _, s := range h.streams {
		want[s.port] = true
	}
	if h.parkedWarm() {
		for _, p := range directPort {
			want[p] = true
		}
		want[fasciaPort] = true
		if h.sel > 0 {
			want[selPort] = true
		}
	}
	if h.sel <= 0 || now.Before(h.selSettle) {
		delete(want, selPort)
	}
	return want
}

// stopCapture stops port's capture; on a timeout it stays in running so the
// next pass asks again (the helper may still be capturing it). 9895's AUs are
// dropped until its next capture start: the selectable may change meanwhile.
func (h *handler) stopCapture(hc Helper, port int) {
	if port == selPort {
		h.vid.shut(port)
	} else {
		h.vid.clear(port)
	}
	if _, ok := h.running[port]; !ok {
		return
	}
	ctx, cancel := h.ctx()
	err := hc.CaptureStop(ctx, port)
	cancel()
	if h.failed(err) == codeTimeout {
		return
	}
	delete(h.running, port)
}

// frontOnSel reports whether front is the windshield ultrawide (9895 with
// selectable 3): not while driving or during SUMMON, where 9895 carries the
// summon camera (or nothing) and front falls back to 9898.
func (h *handler) frontOnSel() bool { return !h.st.Driving && !h.summon }

// fasciaOff reports whether fascia has no stream: 9901 needs sessionActive,
// which driving drops; during SUMMON it is off too (the strip is the summon
// camera, and the MCU also runs the UI and navigation).
func (h *handler) fasciaOff() bool { return h.st.Driving || h.summon }

func (h *handler) portFor(view string) int {
	switch view {
	case "front":
		if !h.frontOnSel() {
			return frontDriving
		}
		return selPort
	case "interior", "summon":
		return selPort
	case "fascia":
		return fasciaPort
	}
	return directPort[view]
}

// stream returns view's stream spec, or false when its camera is not
// available: a 9895 view (front, interior, summon) needs its own selectable
// value to be held (a refusal is in STATUS), fascia is off while driving or
// during SUMMON. summon is the car's summon camera as it is (no dewarp, as
// Tesla's strip), only in SUMMON mode.
func (h *handler) stream(index int, view string, full bool) (streamSpec, bool) {
	s := streamSpec{index: byte(index), view: view, port: h.portFor(view), full: full}
	switch {
	case view == "fascia" && h.fasciaOff():
		return s, false
	case view == "summon":
		return s, h.summon && h.summonMode && h.sel > 0 && h.sel == h.wantSel()
	case s.port != selPort:
		return s, true
	case view == "interior":
		return s, h.sel == selInterior
	}
	s.dewarp = true
	return s, h.sel == selFront
}

func (h *handler) wantStreams() []streamSpec {
	full := h.mode == "full" && !h.degraded // SUMMON link degraded: keyframe-only everywhere
	if h.view == "grid" {
		out := make([]streamSpec, 0, len(gridViews))
		for i, v := range gridViews {
			if s, ok := h.stream(i, v, full && v == h.focus); ok {
				out = append(out, s)
			}
		}
		return out
	}
	if s, ok := h.stream(0, h.view, full); ok {
		return []streamSpec{s}
	}
	return nil // STATUS says why
}

type streamsJSON struct {
	View    string       `json:"view"`
	Focus   string       `json:"focus,omitempty"`
	Streams []streamJSON `json:"streams"`
}

type streamJSON struct {
	Index  int    `json:"index"`
	View   string `json:"view"`
	Full   bool   `json:"full"`
	Dewarp bool   `json:"dewarp,omitempty"` // the windshield ultrawide: the phone undistorts it
}

// setStreams installs the stream table when it changed (view, focus, mode or
// source): STREAMS first, then each new stream's catch-up from the warm
// capture's GOP cache.
func (h *handler) setStreams(specs []streamSpec) {
	sj := streamsJSON{View: h.view, Streams: []streamJSON{}}
	if h.view == "grid" {
		sj.Focus = h.focus
	}
	for _, s := range specs {
		sj.Streams = append(sj.Streams, streamJSON{Index: int(s.index), View: s.view, Full: s.full, Dewarp: s.dewarp})
	}
	body, _ := json.Marshal(sj)
	// The source port is part of the identity even though it is not on the
	// wire: front moving from 9895 to 9898 (driving) is a new stream.
	key := append([]byte(nil), body...)
	for _, s := range specs {
		key = append(key, fmt.Sprintf("|%d", s.port)...)
	}
	if bytes.Equal(key, h.lastStreams) {
		return
	}
	h.streams = specs
	h.vid.install(specs)
	if h.w.Send(TStreams, body) != nil {
		return // window closing
	}
	h.lastStreams = key
	h.vid.start()
}

// ---- STATUS and TALK_STATE ----

type statusJSON struct {
	Gear      string `json:"gear"`
	Driving   bool   `json:"driving"`
	Sentry    string `json:"sentry"`
	View      string `json:"view"`
	StreamsOK bool   `json:"streams_ok"`
	Route     string `json:"route"`
	Talk      string `json:"talk"`
	GainDB    *int   `json:"gain_db"`
	Reason    string `json:"reason"`
}

type talkStateJSON struct {
	State    string `json:"state"`
	QueuedMS int    `json:"queued_ms"`
	Reason   string `json:"reason"`
}

func (h *handler) routeState() string {
	if !h.talk || !h.talkArmed {
		return "off"
	}
	h.mu.Lock()
	ok := h.routeOK
	h.mu.Unlock()
	if ok {
		return "ok"
	}
	return "down"
}

func (h *handler) talkState() talkStateJSON {
	overflowed := h.tq.overflowed() // before the length: an overflow implies a full queue
	ts := talkStateJSON{State: "off", QueuedMS: h.tq.len() / bytesPerMS}
	if !h.talk {
		return ts
	}
	route := h.routeState()
	switch {
	case !h.talkArmed && h.talkErr != "":
		ts.State, ts.Reason = "error", h.talkErr
		if h.helperDown {
			ts.Reason = "helper_down"
		}
	case !h.talkArmed:
		ts.State = "arming"
		if h.helperDown {
			ts.Reason = "helper_down"
		}
	case route != "ok":
		ts.State = "arming"
		if h.routeWasOK {
			ts.Reason = "route_lost"
		}
	case ts.QueuedMS > 0 || h.msgActive || h.tailPending:
		ts.State = "playing"
	default:
		ts.State = "ready"
	}
	if overflowed || h.overflow {
		h.overflow = true
		if ts.Reason == "" {
			ts.Reason = "talk_overflow"
		}
	}
	return ts
}

// viewRefused reports a shown camera that is refused: interior, or front
// (single or the grid's tile) while it would be on 9895, whose selectable was
// refused; fascia while driving or during SUMMON. A refused default hold
// behind another view is not reported: that view does not need it.
func (h *handler) viewRefused() bool {
	switch h.view {
	case "fascia":
		return h.fasciaOff()
	case "interior":
		return h.selReason != ""
	case "front", "grid":
		return h.selReason != "" && h.frontOnSel()
	case "summon":
		return !(h.summon && h.summonMode) || h.selReason != ""
	}
	return false
}

func (h *handler) streamsOK() bool {
	if len(h.streams) == 0 {
		return false
	}
	now := time.Now()
	for _, s := range h.streams {
		last := h.vid.lastAU(s.port)
		if last.IsZero() || now.Sub(last) > h.cfg.Stale {
			return false
		}
	}
	return true
}

func (h *handler) reason(ts talkStateJSON) string {
	switch {
	case h.summon:
		return "summon"
	case h.helperDown:
		return "helper_down"
	case strings.HasPrefix(h.holdReason, "gate_"):
		return h.holdReason
	case h.viewRefused():
		return proto.ErrSelRefused
	case h.noVideo:
		return "no_video"
	case ts.Reason == "route_lost", ts.Reason == "talk_overflow":
		return ts.Reason
	case ts.State == "error" && ts.Reason != "":
		return ts.Reason
	}
	return ""
}

// emit sends STATUS (on change and every StatusEvery) and TALK_STATE (on
// change; queued_ms at most every 500 ms).
func (h *handler) emit() {
	now := time.Now()
	ts := h.talkState()
	st := statusJSON{
		Gear: h.st.Gear, Driving: h.st.Driving, Sentry: h.st.Sentry, View: h.view,
		StreamsOK: h.streamsOK(), Route: h.routeState(), Talk: ts.State, Reason: h.reason(ts),
	}
	if h.talkArmed && h.st.Talk {
		st.GainDB = h.st.GainDB
	}
	body, _ := json.Marshal(st)
	if !bytes.Equal(body, h.lastStatus) || now.Sub(h.lastStatusAt) >= h.cfg.StatusEvery {
		if h.w.Send(TStatus, body) == nil {
			h.lastStatus, h.lastStatusAt = body, now
		}
	}
	if h.talk {
		changed := ts.State != h.lastTalk.State || ts.Reason != h.lastTalk.Reason
		if changed || (ts.QueuedMS != h.lastTalk.QueuedMS && now.Sub(h.lastTalkAt) >= 500*time.Millisecond) || h.lastTalkAt.IsZero() {
			b, _ := json.Marshal(ts)
			if h.w.Send(TTalkState, b) == nil {
				h.lastTalk, h.lastTalkAt = ts, now
			}
		}
	}
}

// ---- session log ----

func (h *handler) noteView() {
	h.mu.Lock()
	seen := false
	for _, v := range h.seenViews {
		seen = seen || v == h.view
	}
	if !seen {
		h.seenViews = append(h.seenViews, h.view)
	}
	d := "view=" + h.view + " seen=" + strings.Join(h.seenViews, ",")
	h.mu.Unlock()
	if h.talk {
		d += " talk=1"
	}
	h.w.SetDetail(d)
}

// beginSelLog starts the session-log record of a selectable hold, named by
// its camera ("front", "interior"): selectable 3 is held whatever view is
// shown, so the view would not say what was diverted.
func (h *handler) beginSelLog(camera string) {
	h.mu.Lock()
	if h.selCam != camera { // a hold already logged (e.g. after a timed-out sel) keeps its start
		h.selCam, h.selStart = camera, time.Now()
	}
	h.mu.Unlock()
}

// endSelLog records one selectable hold (camera, start, end as ms since the
// window opened) exactly once.
func (h *handler) endSelLog() {
	h.mu.Lock()
	camera, start := h.selCam, h.selStart
	h.selCam = ""
	h.mu.Unlock()
	if camera == "" {
		return
	}
	ms := func(t time.Time) int64 { return t.Sub(h.start0).Milliseconds() }
	h.w.LogEvent(fmt.Sprintf("%s hold start=%dms end=%dms (dashcam wide slot diverted)", camera, ms(start), ms(time.Now())))
}
