package summon

import (
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"airgapp/gateway/internal/carlink"
	"airgapp/gateway/internal/clock"
)

// The engine tests' car and phone. Every test seeds the car's first location, (carLat, carLon) in P at speed 0, as
// the car pushes it after H1, and presses from 10 m. The engine reads neither (D3 was dropped by owner ruling
// 2026-11-28); the logs tests check that no position leaks into a log line.
const (
	carLat = 52.52
	carLon = 13.405
	// tenMLat is 10.0 m north of the car: 0.00009° of latitude is 10.007 m (R = 6371 km).
	tenMLat = 52.52009

	findMeType  = "autopark:cmd_find_me"
	forwardType = "autopark:cmd_forward"
	reverseType = "autopark:cmd_reverse"

	wallMS = 1_700_000_000_000 // the rig's fixed wall clock, Unix ms
)

var verbTypes = []string{findMeType, forwardType, reverseType}

var errBus = errors.New("carlink: PeerCommsToCarserverData: emit failed")

// callLog records, in order, every call the engine makes on its Car, Phone and Marker.
type callLog struct {
	mu    sync.Mutex
	calls []string
}

func (l *callLog) add(s string) {
	l.mu.Lock()
	l.calls = append(l.calls, s)
	l.mu.Unlock()
}

func (l *callLog) get() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return slices.Clone(l.calls)
}

func (l *callLog) reset() {
	l.mu.Lock()
	l.calls = nil
	l.mu.Unlock()
}

// fakeCar is a recording Car.
type fakeCar struct {
	log *callLog

	mu                                 sync.Mutex
	sent                               []string
	connectErr, sendErr, disconnectErr error
	onSend                             func(data []byte) // runs inside Send, e.g. to panic
	onDisconnect                       func()            // runs inside Disconnect
}

func (c *fakeCar) Connect() error {
	c.log.add("car.connect")
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.connectErr
}

func (c *fakeCar) Send(data []byte) error {
	c.log.add("car.send " + string(data))
	c.mu.Lock()
	c.sent = append(c.sent, string(data))
	err, hook := c.sendErr, c.onSend
	c.mu.Unlock()
	if hook != nil {
		hook(data)
	}
	return err
}

func (c *fakeCar) Disconnect() error {
	c.log.add("car.disconnect")
	c.mu.Lock()
	err, hook := c.disconnectErr, c.onDisconnect
	c.mu.Unlock()
	if hook != nil {
		hook()
	}
	return err
}

func (c *fakeCar) setSendErr(err error) {
	c.mu.Lock()
	c.sendErr = err
	c.mu.Unlock()
}

func (c *fakeCar) setDisconnectErr(err error) {
	c.mu.Lock()
	c.disconnectErr = err
	c.mu.Unlock()
}

// sends is every Send's data, in order.
func (c *fakeCar) sends() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return slices.Clone(c.sent)
}

// count is how many Sends carried exactly s.
func (c *fakeCar) count(s string) int {
	n := 0
	for _, d := range c.sends() {
		if d == s {
			n++
		}
	}
	return n
}

func (c *fakeCar) reset() {
	c.mu.Lock()
	c.sent = nil
	c.mu.Unlock()
}

// phoneFrame is one CAR frame the phone link took.
type phoneFrame struct {
	ch   byte
	data string
}

// fakePhone is a recording Phone.
type fakePhone struct {
	log *callLog

	mu     sync.Mutex
	frames []phoneFrame // the frames the link took
	links  []bool
	ends   []string
	full   bool   // Car returns false
	onEnd  func() // runs inside End
}

func (p *fakePhone) Car(ch byte, data []byte) bool {
	p.log.add(fmt.Sprintf("phone.car %d %s", ch, data))
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.full {
		return false
	}
	p.frames = append(p.frames, phoneFrame{ch, string(data)})
	return true
}

func (p *fakePhone) SetLink(degraded bool) {
	p.log.add(fmt.Sprintf("phone.link %v", degraded))
	p.mu.Lock()
	p.links = append(p.links, degraded)
	p.mu.Unlock()
}

func (p *fakePhone) End(reason string) {
	p.log.add("phone.end " + reason)
	p.mu.Lock()
	p.ends = append(p.ends, reason)
	hook := p.onEnd
	p.mu.Unlock()
	if hook != nil {
		hook()
	}
}

func (p *fakePhone) setFull(full bool) {
	p.mu.Lock()
	p.full = full
	p.mu.Unlock()
}

func (p *fakePhone) got() []phoneFrame {
	p.mu.Lock()
	defer p.mu.Unlock()
	return slices.Clone(p.frames)
}

func (p *fakePhone) linkCalls() []bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return slices.Clone(p.links)
}

func (p *fakePhone) endReasons() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return slices.Clone(p.ends)
}

func (p *fakePhone) reset() {
	p.mu.Lock()
	p.frames, p.links, p.ends = nil, nil, nil
	p.mu.Unlock()
}

// fakeMarker is a recording Marker.
type fakeMarker struct {
	log *callLog

	mu               sync.Mutex
	setErr, clearErr error
}

func (m *fakeMarker) Set() error {
	m.log.add("marker.set")
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.setErr
}

func (m *fakeMarker) Clear() error {
	m.log.add("marker.clear")
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.clearErr
}

// rig is an Engine on recording fakes, a fake BOOTTIME clock and a settable wall clock.
type rig struct {
	e      *Engine
	clk    *clock.Fake
	log    *callLog
	car    *fakeCar
	phone  *fakePhone
	marker *fakeMarker
	wall   atomic.Int64 // Now, Unix ms

	mu     sync.Mutex
	fatals []error
	logs   []string
}

// newRig builds the rig without starting the engine.
func newRig(t *testing.T) *rig {
	t.Helper()
	r := &rig{clk: clock.NewFake(), log: &callLog{}}
	r.car = &fakeCar{log: r.log}
	r.phone = &fakePhone{log: r.log}
	r.marker = &fakeMarker{log: r.log}
	r.wall.Store(wallMS)
	r.e = NewEngine(Config{
		Car:    r.car,
		Phone:  r.phone,
		Marker: r.marker,
		Clock:  r.clk,
		Now:    func() time.Time { return time.UnixMilli(r.wall.Load()) },
		Logf: func(format string, args ...any) {
			r.mu.Lock()
			r.logs = append(r.logs, fmt.Sprintf(format, args...))
			r.mu.Unlock()
		},
		Fatal: func(err error) {
			r.mu.Lock()
			r.fatals = append(r.fatals, err)
			r.mu.Unlock()
		},
	})
	return r
}

// startRig starts the engine, feeds the car's first location as the car pushes it after H1 (52.52, 13.405, gear P,
// speed 0), and clears the recorders, so each test sees only its own calls.
func startRig(t *testing.T) *rig {
	t.Helper()
	r := newRig(t)
	if err := r.e.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	r.e.CarFrame(carlink.ChannelTCP, locationJSON(carLat, carLon, "P", 0))
	r.reset()
	return r
}

func (r *rig) reset() {
	r.log.reset()
	r.car.reset()
	r.phone.reset()
	r.mu.Lock()
	r.fatals, r.logs = nil, nil
	r.mu.Unlock()
}

// staleAge is the shortest tick age that is not fresh; the tests derive their timings from Fresh and TripAfter (and
// engine.go's tripMS), so they hold if the values change.
const staleAge = Fresh + time.Millisecond

// step advances BOOTTIME by ms and runs one D2 tick.
func (r *rig) step(ms uint64) {
	r.clk.Advance(ms)
	r.e.Tick()
}

func (r *rig) aborts() int { return r.car.count(AbortJSON) }

func (r *rig) fatalErrs() []error {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Clone(r.fatals)
}

func (r *rig) logLines() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Clone(r.logs)
}

// setLate sends LinkSetRun LIVE frames 110 ms late and checks that they set the link flag.
func (r *rig) setLate(t *testing.T) {
	t.Helper()
	for range 3 {
		r.e.Live(110*time.Millisecond, true)
	}
	if got := r.phone.linkCalls(); !slices.Equal(got, []bool{true}) {
		t.Fatalf("SetLink calls %v after three late LIVEs, want [true]", got)
	}
}

func parseMsg(t *testing.T, k Kind, body string) Message {
	t.Helper()
	m, err := Parse(k, []byte(body))
	if err != nil {
		t.Fatalf("Parse(%s): %v", body, err)
	}
	return m
}

// verbAt is Tesla's verb typ from a phone at (lat, lon); a find_me's goal is the phone's own position.
func verbAt(t *testing.T, typ string, lat, lon float64) Message {
	t.Helper()
	body := fmt.Sprintf(`{"msg_type":%q,"latitude":%v,"longitude":%v,"client_version":2,"created:timestamp":1700000000123}`,
		typ, lat, lon)
	if typ == findMeType {
		body = fmt.Sprintf(`{"msg_type":%q,"latitude":%v,"longitude":%v,"accuracy":5,"goal_latitude":%v,"goal_longitude":%v,"client_version":2,"summon_type":"find_me","created:timestamp":1700000000123}`,
			typ, lat, lon, lat, lon)
	}
	return parseMsg(t, KindVerb, body)
}

// verb10m is Tesla's verb typ from a phone 10 m north of the car.
func verb10m(t *testing.T, typ string) Message {
	t.Helper()
	return verbAt(t, typ, tenMLat, carLon)
}

func heartbeatMsg(t *testing.T) Message {
	t.Helper()
	return parseMsg(t, KindHeartbeat,
		`{"msg_type":"autopark:heartbeat_app_webrtc","timestamp":1700000000200,"car_timestamp":1700000000150,"created:timestamp":1700000000201}`)
}

func abortMsg(t *testing.T) Message {
	t.Helper()
	return parseMsg(t, KindMsg, `{"msg_type":"autopark:cmd_abort"}`)
}

func deviceLocationMsg(t *testing.T) Message {
	t.Helper()
	return parseMsg(t, KindMsg, fmt.Sprintf(
		`{"msg_type":"autopark:device_location","latitude":%v,"longitude":%v,"accuracy":5,"altitude":-1,"altitude_accuracy":-1,"goal_latitude":%v,"goal_longitude":%v,"summon_type":"find_me","created:timestamp":1700000000300}`,
		tenMLat, carLon, tenMLat, carLon))
}

func infoMsg(t *testing.T) Message {
	t.Helper()
	return parseMsg(t, KindMsg,
		`{"msg_type":"autopark:info","txid":"<none>","x_tesla_user_agent":"ua","model":"m","name":"n","tag":"t","public_key_hash":"h","created:timestamp":1700000000400}`)
}

func keepaliveMsg(t *testing.T) Message {
	t.Helper()
	return parseMsg(t, KindMsg, `{"msg_type":"control:keepalive","created:timestamp":1700000000500}`)
}

// resultJSON is the car's cmd_result (QtCarServer's sorted keys).
func resultJSON(cmdType string, ok bool, reason string, ts int64) []byte {
	return fmt.Appendf(nil, `{"cmd_type":%q,"msg_type":"autopark:cmd_result","reason":%q,"result":%t,"timestamp":%d}`,
		cmdType, reason, ok, ts)
}

// locationJSON is the car's vehicle_data:location.
func locationJSON(lat, lon float64, shift string, speed float64) []byte {
	return fmt.Appendf(nil, `{"blinker_state":"off","heading":0,"latitude":%v,"longitude":%v,"msg_type":"vehicle_data:location","shift_state":%q,"speed":%v,"timestamp":1700000000000}`,
		lat, lon, shift, speed)
}

// carEcho is the car's heartbeat echo, which goes on dcUdp.
var carEcho = []byte(`{"car_timestamp":1700000000250,"msg_type":"autopark:heartbeat_car_webrtc","timestamp":1700000000200}`)

// within runs f and fails the test if it has not returned within d (a deadlock).
func within(t *testing.T, d time.Duration, what string, f func()) {
	t.Helper()
	done := make(chan struct{})
	go func() {
		defer close(done)
		f()
	}()
	select {
	case <-done:
	case <-time.After(d):
		t.Fatalf("%s did not return within %v: deadlock", what, d)
	}
}

func TestStartSetsMarkerThenConnects(t *testing.T) {
	r := newRig(t)
	if err := r.e.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if got, want := r.log.get(), []string{"marker.set", "car.connect"}; !slices.Equal(got, want) {
		t.Fatalf("calls %q, want %q", got, want)
	}
}

func TestStartRefusedWhenMarkerFails(t *testing.T) {
	r := newRig(t)
	errSet := errors.New("marker: read-only file system")
	r.marker.setErr = errSet
	if err := r.e.Start(); !errors.Is(err, errSet) {
		t.Fatalf("Start = %v, want the marker's error", err)
	}
	if got := r.log.get(); !slices.Equal(got, []string{"marker.set"}) {
		t.Fatalf("calls %q: H1 must not go out without the marker", got)
	}
}

func TestStartRefusedWhenConnectFails(t *testing.T) {
	r := newRig(t)
	errH1 := errors.New("carlink: LocalToRemoteSignaling: emit failed")
	r.car.connectErr = errH1
	if err := r.e.Start(); !errors.Is(err, errH1) {
		t.Fatalf("Start = %v, want H1's error", err)
	}
	// H1 may have reached the car, so the marker stays for the finish hook.
	if got, want := r.log.get(), []string{"marker.set", "car.connect"}; !slices.Equal(got, want) {
		t.Fatalf("calls %q, want %q", got, want)
	}
}

func TestStartOnlyOnce(t *testing.T) {
	t.Run("a second Start", func(t *testing.T) {
		r := newRig(t)
		if err := r.e.Start(); err != nil {
			t.Fatalf("Start: %v", err)
		}
		r.log.reset()
		if err := r.e.Start(); err == nil {
			t.Fatal("a second Start succeeded")
		}
		if got := r.log.get(); len(got) != 0 {
			t.Fatalf("a second Start made calls %q: a second H1", got)
		}
	})
	t.Run("Start after Stop", func(t *testing.T) {
		r := newRig(t)
		r.e.Stop("close")
		r.log.reset()
		if err := r.e.Start(); err == nil {
			t.Fatal("Start after Stop succeeded")
		}
		if got := r.log.get(); len(got) != 0 {
			t.Fatalf("Start after Stop made calls %q: H1 with no H4 to follow", got)
		}
	})
}

func TestVerbForwardedWhenFresh(t *testing.T) {
	for _, typ := range verbTypes {
		t.Run(typ, func(t *testing.T) {
			r := startRig(t)
			m := verb10m(t, typ)
			r.e.Verb(m, 40*time.Millisecond, true, 7)
			if got := r.car.sends(); !slices.Equal(got, []string{string(m.Canonical)}) {
				t.Fatalf("car got %q, want the canonical verb %s", got, m.Canonical)
			}
			if !r.e.VerbActive() {
				t.Fatal("VerbActive is false after a forwarded verb")
			}
			if got := r.phone.got(); len(got) != 0 {
				t.Fatalf("phone got %v for a forwarded verb", got)
			}
		})
	}
}

func TestVerbSendFailureKeepsVerbActive(t *testing.T) {
	// A verb whose Send failed may still have reached the car, so H4 aborts it. The failed Send also keeps the marker
	// (TestStopKeepsMarkerAfterFailedSend).
	r := startRig(t)
	r.car.setSendErr(errBus)
	r.e.Verb(verb10m(t, forwardType), 0, true, 1)
	if !r.e.VerbActive() {
		t.Fatal("verbActive is false after a verb whose Send failed")
	}
	r.car.setSendErr(nil)
	r.log.reset()
	r.e.Stop("error")
	if got, want := r.log.get(), []string{"car.send " + AbortJSON, "car.disconnect"}; !slices.Equal(got, want) {
		t.Fatalf("H4 calls %q, want %q", got, want)
	}
}

// bootClock is a Clock that stays at its BOOTTIME ms.
type bootClock uint64

func (c bootClock) BootMS() uint64 { return uint64(c) }

// liveAgo feeds a fresh LIVE, then advances the clock by ms: the latest liveness instant is ms old.
func (r *rig) liveAgo(ms uint64) {
	r.e.Live(0, true)
	r.clk.Advance(ms)
}

// assertRefusal checks that f is the helper's refusal of cmdType with reason, on dcTcp.
func assertRefusal(t *testing.T, f phoneFrame, cmdType, reason string) {
	t.Helper()
	m := decode(t, []byte(f.data))
	if f.ch != 0 || m["msg_type"] != "autopark:cmd_result" || m["cmd_type"] != cmdType || m["reason"] != reason ||
		m["result"] != false {
		t.Fatalf("got channel %d %s, want a dcTcp refusal of %s with reason %s", f.ch, f.data, cmdType, reason)
	}
}

// refusalTS is the timestamp of the refusal in f.
func refusalTS(t *testing.T, f phoneFrame) string {
	t.Helper()
	ts, ok := decode(t, []byte(f.data))["timestamp"]
	if !ok {
		t.Fatalf("no timestamp in %s", f.data)
	}
	return fmt.Sprint(ts)
}

func TestStaleVerbForwarded(t *testing.T) {
	// Gate 7 ("fresh press") was dropped by owner ruling 2026-11-28: a stale verb goes to the car, given a liveness
	// instant at most TripAfter before it when it starts a hold.
	stale := []struct {
		name string
		age  time.Duration
		ok   bool
	}{
		{"just over Fresh", staleAge, true},
		{"unknown tick", 0, false},
	}
	for _, typ := range verbTypes {
		for _, c := range stale {
			t.Run(typ+"/"+c.name, func(t *testing.T) {
				r := startRig(t)
				r.liveAgo(tripMS) // exactly TripAfter ago still counts
				m := verb10m(t, typ)
				r.e.Verb(m, c.age, c.ok, 1)
				if got := r.car.sends(); !slices.Equal(got, []string{string(m.Canonical)}) {
					t.Fatalf("car got %q, want the verb", got)
				}
				if !r.e.VerbActive() {
					t.Fatal("a forwarded verb left verbActive false")
				}
				if got := r.phone.got(); len(got) != 0 {
					t.Fatalf("phone got %v: the helper answers no forwarded verb", got)
				}
			})
		}
	}
	t.Run("exactly Fresh is fresh, with no liveness before it", func(t *testing.T) {
		r := startRig(t)
		m := verb10m(t, forwardType)
		r.e.Verb(m, Fresh, true, 1)
		if got := r.car.sends(); !slices.Equal(got, []string{string(m.Canonical)}) {
			t.Fatalf("car got %q, want the verb", got)
		}
	})
}

func TestStaleVerbStartingAHoldNeedsLiveness(t *testing.T) {
	refused := func(t *testing.T, r *rig, typ string) {
		t.Helper()
		if got := r.car.sends(); len(got) != 0 {
			t.Fatalf("car got %q, want nothing", got)
		}
		if r.e.VerbActive() {
			t.Fatal("a refused verb set verbActive")
		}
		got := r.phone.got()
		if len(got) != 1 {
			t.Fatalf("phone got %v, want one refusal", got)
		}
		assertRefusal(t, got[0], typ, "action_invalid")
		want := fmt.Sprintf("summon: %s (press 1) refused: no fresh liveness in 800 ms", typ)
		if !slices.Contains(r.logLines(), want) {
			t.Fatalf("log lines %q, want %q", r.logLines(), want)
		}
	}
	for _, typ := range verbTypes {
		t.Run(typ+"/no liveness instant in the session", func(t *testing.T) {
			r := startRig(t) // the fake clock may read 0, as lastLive's zero value does: liveSet decides
			r.e.Verb(verb10m(t, typ), staleAge, true, 1)
			refused(t, r, typ)
		})
		t.Run(typ+"/the last one TripAfter + 1 ms before", func(t *testing.T) {
			r := startRig(t)
			r.liveAgo(tripMS + 1)
			r.e.Verb(verb10m(t, typ), 0, false, 1)
			refused(t, r, typ)
		})
	}
	t.Run("a session within TripAfter of boot has no liveness instant at BOOTTIME 0", func(t *testing.T) {
		log := &callLog{}
		car, phone := &fakeCar{log: log}, &fakePhone{log: log}
		e := NewEngine(Config{Car: car, Phone: phone, Marker: &fakeMarker{log: log}, Clock: bootClock(300)})
		if err := e.Start(); err != nil {
			t.Fatalf("Start: %v", err)
		}
		e.Verb(verb10m(t, forwardType), staleAge, true, 1)
		if got := car.sends(); len(got) != 0 {
			t.Fatalf("car got %q: lastLive's zero value counted as a liveness instant", got)
		}
		if got := phone.got(); len(got) != 1 {
			t.Fatalf("phone got %v, want one refusal", got)
		}
	})
	t.Run("a stale LIVE or HB is no liveness instant", func(t *testing.T) {
		r := startRig(t)
		r.e.Live(staleAge, true)
		r.e.Heartbeat(heartbeatMsg(t), staleAge, true, 1)
		r.car.reset()
		r.e.Verb(verb10m(t, forwardType), staleAge, true, 1)
		refused(t, r, forwardType)
	})
	t.Run("a fresh HB is one", func(t *testing.T) {
		r := startRig(t)
		r.e.Heartbeat(heartbeatMsg(t), 0, true, 1)
		r.clk.Advance(500)
		r.car.reset()
		m := verb10m(t, forwardType)
		r.e.Verb(m, staleAge, true, 1)
		if got := r.car.sends(); !slices.Equal(got, []string{string(m.Canonical)}) {
			t.Fatalf("car got %q, want the verb", got)
		}
	})
}

func TestAllStaleLinkStopsAfterTheFirstTrip(t *testing.T) {
	// The concern the stale-start rule closes: on a link where every frame is stale, the phone's re-sends every 250 ms
	// must not restart the car after each trip. The first stale press, 300 ms after the last fresh LIVE, is forwarded
	// and trips 500 ms later (TripAfter after the LIVE); after the trip no verb reaches the car, for 10 s of re-sends.
	r := startRig(t)
	r.liveAgo(300)
	first := verb10m(t, forwardType)
	r.e.Verb(first, staleAge, true, 1)
	for ms := uint64(10); ms <= 10000; ms += 10 {
		r.clk.Advance(10)
		if ms%50 == 0 {
			r.e.Live(staleAge, true)
		}
		if ms%100 == 0 {
			r.e.Heartbeat(heartbeatMsg(t), staleAge, true, 1)
		}
		if ms%250 == 0 {
			r.e.Verb(verb10m(t, forwardType), staleAge, true, 1)
		}
		r.e.Tick()
	}
	if got := r.car.count(string(first.Canonical)); got != 3 {
		// the first verb, then its re-sends at 250 and 500 ms, before the trip at 501 ms (its tick at 510)
		t.Fatalf("car got the verb %d times, want 3: only within the first hold", got)
	}
	if n := r.aborts(); n != 1 {
		t.Fatalf("%d aborts, want the one trip", n)
	}
	if got := r.car.sends(); got[len(got)-1] != AbortJSON {
		t.Fatalf("the car's last input is %s, want the trip's abort", got[len(got)-1])
	}
	refusals := 0
	for _, f := range r.phone.got() {
		assertRefusal(t, f, forwardType, "action_invalid")
		refusals++
	}
	if want := (10000 - 750) / 250; refusals != want+1 { // every re-send from 750 ms to 10000 ms
		t.Fatalf("%d refusals, want %d", refusals, want+1)
	}
}

func TestHealthyLinkOccasionalStaleVerbForwarded(t *testing.T) {
	// Fresh LIVEs every 50 ms: a stale re-send replaces the hold, and a stale press that starts a new hold after a
	// release goes through too.
	r := startRig(t)
	m := verb10m(t, forwardType)
	r.e.Verb(m, 0, true, 1)
	for ms := uint64(10); ms <= 3000; ms += 10 {
		r.clk.Advance(10)
		if ms%50 == 0 {
			r.e.Live(20*time.Millisecond, true)
		}
		if ms%250 == 0 {
			if ms%1000 == 0 {
				r.e.Verb(m, staleAge, true, 1) // an occasional late one
			} else {
				r.e.Verb(m, 30*time.Millisecond, true, 1)
			}
		}
		r.e.Tick()
	}
	if n := r.aborts(); n != 0 {
		t.Fatalf("%d aborts on a healthy link", n)
	}
	if got := r.car.count(string(m.Canonical)); got != 13 {
		t.Fatalf("car got the verb %d times, want 13", got)
	}
	r.e.Msg(abortMsg(t))
	r.clk.Advance(40)
	r.e.Verb(verb10m(t, reverseType), staleAge, true, 2) // a stale press 40 ms after a fresh LIVE
	if !r.e.VerbActive() {
		t.Fatal("the stale press was not forwarded")
	}
	if got := r.phone.got(); len(got) != 0 {
		t.Fatalf("phone got %v", got)
	}
}

func TestStaleVerbIsNotLiveness(t *testing.T) {
	t.Run("a stale press 300 ms after a fresh LIVE trips TripAfter + 1 ms after that LIVE", func(t *testing.T) {
		r := startRig(t)
		r.liveAgo(300)
		m := verb10m(t, forwardType)
		r.e.Verb(m, staleAge, true, 1)
		if got := r.car.sends(); !slices.Equal(got, []string{string(m.Canonical)}) {
			t.Fatalf("car got %q, want the stale verb", got)
		}
		r.e.Live(staleAge, true) // stale LIVEs are no liveness instant either
		r.e.Live(0, false)
		r.step(tripMS - 300) // 500 ms after the hold's start, TripAfter after the LIVE
		if n := r.aborts(); n != 0 {
			t.Fatalf("%d aborts TripAfter after the LIVE", n)
		}
		r.step(1)
		if n := r.aborts(); n != 1 {
			t.Fatalf("%d aborts 501 ms after the stale verb (TripAfter + 1 ms after the LIVE), want 1: a stale verb started a trip clock of its own", n)
		}
		if r.e.VerbActive() {
			t.Fatal("verbActive after the trip")
		}
	})
	t.Run("stale re-sends every 250 ms never keep the hold alive", func(t *testing.T) {
		for _, first := range []struct {
			name  string
			age   time.Duration
			tripB uint64 // the trip comes TripAfter + 1 ms after this many ms before the first verb
		}{{"a fresh first verb", 0, 0}, {"a stale first verb", staleAge, 100}} {
			t.Run(first.name, func(t *testing.T) {
				r := startRig(t)
				r.liveAgo(100)
				r.e.Verb(verb10m(t, forwardType), first.age, true, 1)
				for ms := uint64(10); ms <= tripMS-first.tripB; ms += 10 {
					r.clk.Advance(10)
					if ms%250 == 0 {
						r.e.Verb(verb10m(t, forwardType), staleAge, true, 1) // the phone's re-send while held
					}
					r.e.Tick()
				}
				if n := r.aborts(); n != 0 {
					t.Fatalf("%d aborts by TripAfter", n)
				}
				r.step(1)
				if n := r.aborts(); n != 1 {
					t.Fatalf("%d aborts at TripAfter + 1 ms, want 1: a stale re-send stamped liveness", n)
				}
			})
		}
	})
	t.Run("fresh re-sends keep the hold alive", func(t *testing.T) {
		r := startRig(t)
		r.e.Verb(verb10m(t, forwardType), 0, true, 1)
		for ms := uint64(10); ms <= 3000; ms += 10 {
			r.clk.Advance(10)
			if ms%250 == 0 {
				r.e.Verb(verb10m(t, forwardType), Fresh, true, 1)
			}
			r.e.Tick()
		}
		if n := r.aborts(); n != 0 {
			t.Fatalf("%d aborts while fresh re-sends arrived every 250 ms", n)
		}
	})
	t.Run("a stale verb that replaces an active one keeps the hold's trip clock", func(t *testing.T) {
		r := startRig(t)
		r.e.Verb(verb10m(t, forwardType), 0, true, 1)
		r.clk.Advance(300)
		r.e.Live(0, true) // the hold's latest liveness instant, at 300
		r.clk.Advance(400)
		r.e.Verb(verb10m(t, reverseType), staleAge, true, 2) // at 700
		r.step(tripMS - 400)                                 // TripAfter after the LIVE
		if n := r.aborts(); n != 0 {
			t.Fatalf("%d aborts TripAfter after the last liveness instant", n)
		}
		r.step(1)
		if n := r.aborts(); n != 1 {
			t.Fatalf("%d aborts TripAfter + 1 ms after the last liveness instant, want 1: the stale verb reset the clock", n)
		}
	})
	t.Run("a stale verb after a trip, and a fresh LIVE, clears tripped", func(t *testing.T) {
		r := startRig(t)
		r.e.Verb(verb10m(t, forwardType), 0, true, 1)
		r.step(tripMS + 1)
		hb := heartbeatMsg(t)
		r.car.reset()
		r.e.Heartbeat(hb, 0, true, 1) // fresh, so a liveness instant, but not forwarded after the trip
		if got := r.car.sends(); len(got) != 0 {
			t.Fatalf("car got %q after the trip", got)
		}
		m := verb10m(t, forwardType)
		r.e.Verb(m, staleAge, true, 2)
		r.e.Heartbeat(hb, 0, true, 2)
		if got, want := r.car.sends(), []string{string(m.Canonical), string(hb.Canonical)}; !slices.Equal(got, want) {
			t.Fatalf("car got %q, want the stale verb, then the fresh heartbeat", got)
		}
	})
}

func TestVerbWhileActiveForwarded(t *testing.T) {
	// Gate 6 ("one verb at a time") was dropped by owner ruling 2026-11-28: a verb while one is active replaces it.
	for _, typ := range verbTypes {
		t.Run(typ, func(t *testing.T) {
			r := startRig(t)
			first := verb10m(t, forwardType)
			r.e.Verb(first, 20*time.Millisecond, true, 1)
			r.clk.Advance(200)
			second := verb10m(t, typ)
			r.e.Verb(second, 20*time.Millisecond, true, 2)
			if got, want := r.car.sends(), []string{string(first.Canonical), string(second.Canonical)}; !slices.Equal(got, want) {
				t.Fatalf("car got %q, want both verbs", got)
			}
			if got := r.phone.got(); len(got) != 0 {
				t.Fatalf("phone got %v", got)
			}
			if !r.e.VerbActive() {
				t.Fatal("verbActive is false")
			}
			// The fresh second verb is a liveness instant: the trip comes TripAfter + 1 ms after it.
			r.step(tripMS)
			if n := r.aborts(); n != 0 {
				t.Fatalf("%d aborts TripAfter after the second verb", n)
			}
			r.step(1)
			if n := r.aborts(); n != 1 {
				t.Fatalf("%d aborts TripAfter + 1 ms after the second verb, want 1", n)
			}
		})
	}
}

// verbLines is the rig's verb log lines: forwarded, refused, and the hold's end.
func (r *rig) verbLines() []string {
	var out []string
	for _, l := range r.logLines() {
		if strings.Contains(l, "(press ") || strings.HasPrefix(l, "summon: hold ended") {
			out = append(out, l)
		}
	}
	return out
}

func TestVerbLogs(t *testing.T) {
	// A verb line only when a verb starts a hold or changes the verb type; re-sends of the same type are counted and
	// logged once, when the hold ends.
	t.Run("the phone's release", func(t *testing.T) {
		r := startRig(t)
		fwd, rev := verb10m(t, forwardType), verb10m(t, reverseType)
		r.e.Verb(fwd, 0, true, 1)
		for range 5 {
			r.clk.Advance(250)
			r.e.Verb(fwd, 20*time.Millisecond, true, 1)
		}
		r.clk.Advance(100)
		r.e.Verb(rev, 20*time.Millisecond, true, 2)
		r.clk.Advance(250)
		r.e.Verb(rev, staleAge, true, 2)
		r.clk.Advance(50)
		r.e.Live(0, true)
		r.e.Msg(abortMsg(t))
		r.e.Msg(abortMsg(t)) // no hold: no line
		want := []string{
			"summon: autopark:cmd_forward (press 1, 127 bytes) forwarded",
			"summon: autopark:cmd_reverse (press 2, 127 bytes) forwarded, replacing the active autopark:cmd_forward (forwarded 100 ms before)",
			"summon: hold ended: 6 re-sends (1 stale); HB 0 arrived, 0 forwarded, 0 dropped (0 stale, 0 after a trip), longest gap between forwarded none; LIVE 1 arrived, 1 fresh, longest gap between fresh none",
		}
		if got := r.verbLines(); !slices.Equal(got, want) {
			t.Fatalf("verb lines\n%q\nwant\n%q", got, want)
		}
	})
	t.Run("D2's trip, then H4", func(t *testing.T) {
		r := startRig(t)
		m := verb10m(t, findMeType)
		r.e.Verb(m, 0, true, 1)
		r.clk.Advance(250)
		r.e.Verb(m, staleAge, true, 1)
		r.step(tripMS)
		r.e.Live(0, true)
		r.e.Verb(m, staleAge, true, 2) // a new hold, stale but 0 ms after a fresh LIVE
		r.clk.Advance(100)
		r.e.Verb(m, 0, true, 2)
		r.e.Stop("close")
		want := []string{
			"summon: autopark:cmd_find_me (press 1, 213 bytes) forwarded",
			"summon: hold ended: 1 re-sends (1 stale); HB 0 arrived, 0 forwarded, 0 dropped (0 stale, 0 after a trip), longest gap between forwarded none; LIVE 0 arrived, 0 fresh, longest gap between fresh none",
			"summon: autopark:cmd_find_me (press 2, 213 bytes) forwarded, stale tick: not a liveness instant",
			"summon: hold ended: 1 re-sends (0 stale); HB 0 arrived, 0 forwarded, 0 dropped (0 stale, 0 after a trip), longest gap between forwarded none; LIVE 0 arrived, 0 fresh, longest gap between fresh none",
		}
		if got := r.verbLines(); !slices.Equal(got, want) {
			t.Fatalf("verb lines\n%q\nwant\n%q", got, want)
		}
	})
}

func TestQuickForwardReverseSwitch(t *testing.T) {
	// The phone switches between Forward and Reverse quickly, and re-sends the held one every 250 ms: every verb goes
	// to the car in order, with no abort in between, and the hold never trips while the verbs are fresh.
	r := startRig(t)
	var want []string
	seq := []string{forwardType, forwardType, reverseType, forwardType, reverseType, reverseType, forwardType}
	for i, typ := range seq {
		m := verb10m(t, typ)
		r.e.Verb(m, 30*time.Millisecond, true, uint32(i+1))
		want = append(want, string(m.Canonical))
		r.step(10)
		r.clk.Advance(40) // 50 ms between switches
		r.e.Tick()
	}
	if got := r.car.sends(); !slices.Equal(got, want) {
		t.Fatalf("car got %q, want %q", got, want)
	}
	if got := r.phone.got(); len(got) != 0 {
		t.Fatalf("phone got %v", got)
	}
	if !r.e.VerbActive() {
		t.Fatal("verbActive is false")
	}
}

func TestRefusalShape(t *testing.T) {
	r := startRig(t)
	r.e.Verb(verb10m(t, reverseType), staleAge, true, 1)
	got := r.phone.got()
	want := `{"cmd_type":"autopark:cmd_reverse","msg_type":"autopark:cmd_result","reason":"action_invalid","result":false,"timestamp":1700000000000}`
	if len(got) != 1 || got[0] != (phoneFrame{0, want}) {
		t.Fatalf("phone got %v, want %s on dcTcp", got, want)
	}
}

func TestRefusalTimestampNeverGoesBack(t *testing.T) {
	refuse := func(t *testing.T, r *rig) string {
		t.Helper()
		r.phone.reset()
		r.e.Verb(verb10m(t, forwardType), staleAge, true, 1)
		got := r.phone.got()
		if len(got) != 1 {
			t.Fatalf("phone got %v, want one refusal", got)
		}
		return refusalTS(t, got[0])
	}
	t.Run("a forwarded cmd_result", func(t *testing.T) {
		r := startRig(t)
		r.wall.Store(1000)
		r.e.CarFrame(carlink.ChannelTCP, resultJSON(abortType, true, "", 5000))
		if ts := refuse(t, r); ts != "5000" {
			t.Fatalf("timestamp %s with Now 1000 after a cmd_result at 5000, want 5000", ts)
		}
		r.wall.Store(9000)
		if ts := refuse(t, r); ts != "9000" {
			t.Fatalf("timestamp %s with Now 9000, want 9000", ts)
		}
	})
	t.Run("a swallowed cmd_result counts", func(t *testing.T) {
		r := startRig(t)
		r.wall.Store(1000)
		r.e.Verb(verb10m(t, forwardType), 0, true, 1)
		r.step(tripMS + 1)
		r.e.CarFrame(carlink.ChannelTCP, resultJSON(abortType, true, "", 7000))
		if ts := refuse(t, r); ts != "7000" {
			t.Fatalf("timestamp %s, want 7000", ts)
		}
	})
	t.Run("only a cmd_result counts; a fractional one rounds up; an absurd one is ignored", func(t *testing.T) {
		r := startRig(t)
		r.wall.Store(1000)
		r.e.CarFrame(carlink.ChannelTCP, []byte(`{"autopark_state":"standby","msg_type":"autopark:status","timestamp":8000}`))
		r.e.CarFrame(carlink.ChannelTCP, []byte(`{"cmd_type":"autopark:cmd_abort","msg_type":"autopark:cmd_result","reason":"","result":true,"timestamp":1e300}`))
		if ts := refuse(t, r); ts != "1000" {
			t.Fatalf("timestamp %s, want 1000", ts)
		}
		r.e.CarFrame(carlink.ChannelTCP, []byte(`{"cmd_type":"autopark:cmd_abort","msg_type":"autopark:cmd_result","reason":"","result":true,"timestamp":5000.25}`))
		if ts := refuse(t, r); ts != "5001" {
			t.Fatalf("timestamp %s after a cmd_result at 5000.25, want 5001", ts)
		}
	})
}

func TestTripAfter(t *testing.T) {
	r := startRig(t)
	r.e.Verb(verb10m(t, findMeType), 0, true, 1)
	r.step(tripMS)
	if n := r.aborts(); n != 0 {
		t.Fatalf("%d aborts after exactly TripAfter", n)
	}
	r.step(1)
	if n := r.aborts(); n != 1 {
		t.Fatalf("%d aborts after TripAfter + 1 ms, want 1", n)
	}
	if r.e.VerbActive() {
		t.Fatal("verbActive after the trip")
	}
	for range 50 {
		r.step(10)
	}
	if n := r.aborts(); n != 1 {
		t.Fatalf("%d aborts, want exactly one", n)
	}
	// The session continues and the phone learns nothing.
	if got := r.phone.got(); len(got) != 0 {
		t.Fatalf("phone got %v from a trip", got)
	}
	if got := r.phone.endReasons(); len(got) != 0 {
		t.Fatalf("the trip ended the session: %q", got)
	}
}

func TestFreshLiveKeepsAlive(t *testing.T) {
	r := startRig(t)
	r.e.Verb(verb10m(t, findMeType), 0, true, 1)
	for ms := 10; ms <= 5000; ms += 10 {
		r.clk.Advance(10)
		if ms%50 == 0 {
			r.e.Live(Fresh, true) // F itself is fresh
		}
		r.e.Tick()
	}
	if n := r.aborts(); n != 0 {
		t.Fatalf("%d aborts while fresh LIVEs arrived every 50 ms", n)
	}
	if !r.e.VerbActive() {
		t.Fatal("verbActive cleared while fresh LIVEs arrived")
	}
}

func TestStaleLiveDoesNotKeepAlive(t *testing.T) {
	cases := []struct {
		name string
		age  time.Duration
		ok   bool
	}{
		{"Fresh + 10ms", Fresh + 10*time.Millisecond, true},
		{"just over Fresh", staleAge, true},
		{"expired tick", 0, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := startRig(t)
			r.e.Verb(verb10m(t, findMeType), 0, true, 1)
			for range tripMS / 50 { // LIVEs every 50 ms up to TripAfter
				r.clk.Advance(50)
				r.e.Live(c.age, c.ok)
				r.e.Tick()
			}
			r.step(tripMS % 50)
			if n := r.aborts(); n != 0 {
				t.Fatalf("%d aborts by TripAfter", n)
			}
			r.step(1)
			if n := r.aborts(); n != 1 {
				t.Fatalf("%d aborts at TripAfter + 1 ms, want 1: a stale LIVE stamped liveness", n)
			}
		})
	}
}

func TestFreshHeartbeatKeepsAlive(t *testing.T) {
	r := startRig(t)
	hb := heartbeatMsg(t)
	r.e.Verb(verb10m(t, findMeType), 0, true, 1)
	for ms := 10; ms <= 2000; ms += 10 {
		r.clk.Advance(10)
		if ms%100 == 0 {
			r.e.Heartbeat(hb, Fresh, true, 1)
		}
		r.e.Tick()
	}
	if n := r.aborts(); n != 0 {
		t.Fatalf("%d aborts while fresh heartbeats arrived every 100 ms", n)
	}
	if n := r.car.count(string(hb.Canonical)); n != 20 {
		t.Fatalf("%d heartbeats forwarded, want 20", n)
	}
}

func TestTripSwallowsItsAbortResult(t *testing.T) {
	r := startRig(t)
	r.e.Verb(verb10m(t, forwardType), 0, true, 1)
	r.step(tripMS + 1)
	if n := r.aborts(); n != 1 {
		t.Fatalf("%d aborts, want the trip's", n)
	}
	verbResult := resultJSON(forwardType, true, "", wallMS+10)
	r.e.CarFrame(carlink.ChannelTCP, verbResult)                                 // not an abort's: forwarded
	r.e.CarFrame(carlink.ChannelTCP, resultJSON(abortType, true, "", wallMS+20)) // the injected abort's: swallowed
	got := r.phone.got()
	if len(got) != 1 || got[0] != (phoneFrame{0, string(verbResult)}) {
		t.Fatalf("phone got %v, want only the verb's cmd_result", got)
	}
}

func TestSwallowOnlyInjectedAbortResult(t *testing.T) {
	r := startRig(t)
	v := verb10m(t, findMeType)
	r.e.Verb(v, 0, true, 1)
	r.step(tripMS + 1)   // the trip injects an abort
	r.e.Msg(abortMsg(t)) // then the phone's release sends its own
	if got, want := r.car.sends(), []string{string(v.Canonical), AbortJSON, AbortJSON}; !slices.Equal(got, want) {
		t.Fatalf("car got %q, want the verb, the injected abort and the phone's abort", got)
	}
	r.e.CarFrame(carlink.ChannelTCP, resultJSON(abortType, true, "", wallMS+10))
	second := resultJSON(abortType, false, "already_set", wallMS+20)
	r.e.CarFrame(carlink.ChannelTCP, second)
	got := r.phone.got()
	if len(got) != 1 || got[0] != (phoneFrame{0, string(second)}) {
		t.Fatalf("phone got %v, want only the second abort result", got)
	}
}

func TestNoHeartbeatAfterTripUntilNextVerb(t *testing.T) {
	r := startRig(t)
	hb := heartbeatMsg(t)
	r.e.Verb(verb10m(t, findMeType), 0, true, 1)
	r.step(tripMS + 1)
	r.e.Heartbeat(hb, 20*time.Millisecond, true, 1)
	r.e.Msg(abortMsg(t)) // the release does not end the trip's hold on heartbeats
	r.clk.Advance(100)
	r.e.Heartbeat(hb, 20*time.Millisecond, true, 1)
	if n := r.car.count(string(hb.Canonical)); n != 0 {
		t.Fatalf("%d heartbeats forwarded after the trip", n)
	}
	r.clk.Advance(500)
	next := verb10m(t, findMeType)
	r.e.Verb(next, 20*time.Millisecond, true, 2)
	if n := r.car.count(string(next.Canonical)); n != 2 {
		t.Fatalf("the next press went out %d times in all, want 2 (one per press)", n)
	}
	r.e.Heartbeat(hb, 20*time.Millisecond, true, 2)
	if n := r.car.count(string(hb.Canonical)); n != 1 {
		t.Fatalf("%d heartbeats forwarded after the next verb, want 1", n)
	}
}

func TestStaleHeartbeatNotForwarded(t *testing.T) {
	r := startRig(t)
	hb := heartbeatMsg(t)
	r.e.Verb(verb10m(t, findMeType), 0, true, 1)
	r.clk.Advance(200)
	r.e.Heartbeat(hb, staleAge, true, 1)
	r.clk.Advance(50)
	r.e.Heartbeat(hb, 0, false, 1)
	if n := r.car.count(string(hb.Canonical)); n != 0 {
		t.Fatalf("%d stale heartbeats forwarded", n)
	}
	r.step(tripMS - 250)
	if n := r.aborts(); n != 0 {
		t.Fatalf("%d aborts at TripAfter", n)
	}
	r.step(1)
	if n := r.aborts(); n != 1 {
		t.Fatalf("%d aborts at TripAfter + 1 ms, want 1: a stale heartbeat stamped liveness", n)
	}
}

func TestHeartbeatBeforeAnyVerbForwarded(t *testing.T) {
	r := startRig(t)
	hb := heartbeatMsg(t)
	r.e.Heartbeat(hb, 30*time.Millisecond, true, 1)
	if got := r.car.sends(); !slices.Equal(got, []string{string(hb.Canonical)}) {
		t.Fatalf("car got %q, want the heartbeat", got)
	}
	if r.e.VerbActive() {
		t.Fatal("a heartbeat set verbActive")
	}
}

func TestAbortAlwaysForwardedCanonical(t *testing.T) {
	// An abort that carried other bytes still goes out as exactly AbortJSON.
	odd := Message{Type: abortType, Canonical: []byte(`{"msg_type":"autopark:cmd_abort","x":1}`)}
	states := []struct {
		name  string
		setup func(t *testing.T, r *rig)
	}{
		{"no verb", func(*testing.T, *rig) {}},
		{"verb active", func(t *testing.T, r *rig) { r.e.Verb(verb10m(t, forwardType), 0, true, 1) }},
		{"tripped", func(t *testing.T, r *rig) {
			r.e.Verb(verb10m(t, forwardType), 0, true, 1)
			r.step(tripMS + 1)
		}},
	}
	for _, s := range states {
		t.Run(s.name, func(t *testing.T) {
			r := startRig(t)
			s.setup(t, r)
			r.car.reset()
			r.e.Msg(abortMsg(t))
			r.e.Msg(odd)
			if got, want := r.car.sends(), []string{AbortJSON, AbortJSON}; !slices.Equal(got, want) {
				t.Fatalf("car got %q, want two canonical aborts", got)
			}
			if r.e.VerbActive() {
				t.Fatal("verbActive after the phone's abort")
			}
		})
	}
}

func TestCarRefusalKeepsVerbActive(t *testing.T) {
	r := startRig(t)
	r.e.Verb(verb10m(t, findMeType), 0, true, 1)
	r.clk.Advance(50)
	refused := resultJSON(findMeType, false, "plugged_in", wallMS+50)
	r.e.CarFrame(carlink.ChannelTCP, refused)
	if got := r.phone.got(); len(got) != 1 || got[0] != (phoneFrame{0, string(refused)}) {
		t.Fatalf("phone got %v, want the car's refusal as it was", got)
	}
	if !r.e.VerbActive() {
		t.Fatal("the car's refusal cleared verbActive")
	}
	r.step(tripMS - 50) // TripAfter after the verb, the last liveness instant
	if n := r.aborts(); n != 0 {
		t.Fatalf("%d aborts at TripAfter", n)
	}
	r.step(1)
	if n := r.aborts(); n != 1 {
		t.Fatalf("%d aborts at TripAfter + 1 ms, want 1", n)
	}
}

func TestStopRunsH4Once(t *testing.T) {
	r := startRig(t)
	r.e.Verb(verb10m(t, forwardType), 0, true, 1)
	r.log.reset()
	r.e.Stop("close")
	if got, want := r.log.get(), []string{"car.send " + AbortJSON, "car.disconnect", "marker.clear"}; !slices.Equal(got, want) {
		t.Fatalf("H4 calls %q, want %q", got, want)
	}
	if r.e.VerbActive() {
		t.Fatal("verbActive after Stop")
	}
	r.log.reset()
	r.e.Stop("close")
	// Every input after Stop is ignored.
	for range 3 {
		r.e.Live(110*time.Millisecond, true)
	}
	r.e.Live(0, true)
	r.e.Verb(verb10m(t, reverseType), 0, true, 2)
	r.e.Verb(verb10m(t, reverseType), staleAge, true, 3)
	r.e.Heartbeat(heartbeatMsg(t), 0, true, 2)
	r.e.Msg(abortMsg(t))
	r.e.Msg(deviceLocationMsg(t))
	r.e.CarFrame(carlink.ChannelTCP, resultJSON(forwardType, true, "", wallMS))
	r.e.CarFrame(carlink.ChannelUDP, carEcho)
	r.step(1000)
	if got := r.log.get(); len(got) != 0 {
		t.Fatalf("calls after Stop: %q", got)
	}
}

func TestStopWithoutVerbOnlyDisconnects(t *testing.T) {
	cases := []struct {
		name  string
		setup func(t *testing.T, r *rig)
	}{
		{"no verb", func(*testing.T, *rig) {}},
		{"after the phone's abort", func(t *testing.T, r *rig) {
			r.e.Verb(verb10m(t, forwardType), 0, true, 1)
			r.e.Msg(abortMsg(t))
		}},
		{"after a trip", func(t *testing.T, r *rig) {
			r.e.Verb(verb10m(t, forwardType), 0, true, 1)
			r.step(tripMS + 1)
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := startRig(t)
			c.setup(t, r)
			r.log.reset()
			r.e.Stop("close")
			if got, want := r.log.get(), []string{"car.disconnect", "marker.clear"}; !slices.Equal(got, want) {
				t.Fatalf("H4 calls %q, want %q", got, want)
			}
		})
	}
}

func TestStopKeepsMarkerWhenDisconnectFails(t *testing.T) {
	r := startRig(t)
	r.car.disconnectErr = errors.New("carlink: disconnect not confirmed")
	r.e.Stop("close")
	if got := r.log.get(); !slices.Equal(got, []string{"car.disconnect"}) {
		t.Fatalf("H4 calls %q, want the disconnect and no marker.clear", got)
	}
}

func TestStopKeepsMarkerWhenAbortFails(t *testing.T) {
	r := startRig(t)
	r.e.Verb(verb10m(t, forwardType), 0, true, 1)
	r.car.setSendErr(errBus)
	r.log.reset()
	r.e.Stop("close")
	if got, want := r.log.get(), []string{"car.send " + AbortJSON, "car.disconnect"}; !slices.Equal(got, want) {
		t.Fatalf("H4 calls %q, want %q: no marker.clear after a failed abort", got, want)
	}
}

func TestStopKeepsMarkerAfterFailedSend(t *testing.T) {
	// One Car.Send fails during the session; every later call succeeds, H4's Disconnect included. The marker still
	// stays, so the supervisor's takeover sends its own abort whatever the Car implementation latched.
	cases := []struct {
		name string
		fail func(t *testing.T, r *rig) // runs one input whose Send fails
		want []string                   // H4's calls
	}{
		{"the trip's abort", func(t *testing.T, r *rig) {
			r.e.Verb(verb10m(t, forwardType), 0, true, 1)
			r.car.setSendErr(errBus)
			r.step(tripMS + 1)
		}, []string{"car.disconnect"}},
		{"the phone's abort", func(t *testing.T, r *rig) {
			r.e.Verb(verb10m(t, forwardType), 0, true, 1)
			r.car.setSendErr(errBus)
			r.e.Msg(abortMsg(t))
		}, []string{"car.disconnect"}},
		{"a verb", func(t *testing.T, r *rig) {
			r.car.setSendErr(errBus)
			r.e.Verb(verb10m(t, forwardType), 0, true, 1)
		}, []string{"car.send " + AbortJSON, "car.disconnect"}},
		{"a heartbeat", func(t *testing.T, r *rig) {
			r.car.setSendErr(errBus)
			r.e.Heartbeat(heartbeatMsg(t), 0, true, 1)
		}, []string{"car.disconnect"}},
		{"a device_location", func(t *testing.T, r *rig) {
			r.car.setSendErr(errBus)
			r.e.Msg(deviceLocationMsg(t))
		}, []string{"car.disconnect"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := startRig(t)
			c.fail(t, r)
			if got := r.phone.endReasons(); len(got) != 1 {
				t.Fatalf("Phone.End calls %q: the Send did not fail", got)
			}
			r.car.setSendErr(nil) // from here every Send succeeds, and Disconnect returns nil
			r.log.reset()
			r.e.Stop("error")
			if got := r.log.get(); !slices.Equal(got, c.want) {
				t.Fatalf("H4 calls %q, want %q: no marker.clear after a failed Send", got, c.want)
			}
		})
	}
}

func TestStopPanicCallsFatal(t *testing.T) {
	r := startRig(t)
	r.e.Verb(verb10m(t, forwardType), 0, true, 1)
	r.car.onDisconnect = func() { panic("boom") }
	r.log.reset()
	func() {
		defer func() {
			if p := recover(); p != nil {
				t.Fatalf("Stop let the panic out: %v", p)
			}
		}()
		r.e.Stop("close")
	}()
	if errs := r.fatalErrs(); len(errs) != 1 || !strings.Contains(errs[0].Error(), "boom") {
		t.Fatalf("Fatal got %v, want one error carrying the panic", errs)
	}
	if got, want := r.log.get(), []string{"car.send " + AbortJSON, "car.disconnect"}; !slices.Equal(got, want) {
		t.Fatalf("calls %q, want %q: the marker stays", got, want)
	}
	within(t, 2*time.Second, "VerbActive after the panic", func() { r.e.VerbActive() })
	r.e.Stop("close")
	if len(r.log.get()) != 2 || len(r.fatalErrs()) != 1 {
		t.Fatalf("a second Stop ran H4 again: %q", r.log.get())
	}
}

func TestTripPanicCallsFatal(t *testing.T) {
	r := startRig(t)
	r.e.Verb(verb10m(t, forwardType), 0, true, 1)
	r.car.onSend = func([]byte) { panic("wedged") }
	r.clk.Advance(tripMS + 1)
	func() {
		defer func() {
			if p := recover(); p != nil {
				t.Fatalf("Tick let the panic out: %v", p)
			}
		}()
		r.e.Tick()
	}()
	if errs := r.fatalErrs(); len(errs) != 1 || !strings.Contains(errs[0].Error(), "wedged") {
		t.Fatalf("Fatal got %v, want one error carrying the panic", errs)
	}
	within(t, 2*time.Second, "VerbActive after the panic", func() { r.e.VerbActive() })
}

func TestCarSendErrorEndsSession(t *testing.T) {
	cases := []struct {
		name string
		run  func(t *testing.T, r *rig)
	}{
		{"verb", func(t *testing.T, r *rig) {
			r.car.setSendErr(errBus)
			r.e.Verb(verb10m(t, forwardType), 0, true, 1)
		}},
		{"heartbeat", func(t *testing.T, r *rig) {
			r.car.setSendErr(errBus)
			r.e.Heartbeat(heartbeatMsg(t), 0, true, 1)
		}},
		{"device_location", func(t *testing.T, r *rig) {
			r.car.setSendErr(errBus)
			r.e.Msg(deviceLocationMsg(t))
		}},
		{"the phone's abort", func(t *testing.T, r *rig) {
			r.car.setSendErr(errBus)
			r.e.Msg(abortMsg(t))
		}},
		{"the trip's abort", func(t *testing.T, r *rig) {
			r.e.Verb(verb10m(t, forwardType), 0, true, 1)
			r.car.setSendErr(errBus)
			r.step(tripMS + 1)
		}},
		{"two failures end it once", func(t *testing.T, r *rig) {
			r.car.setSendErr(errBus)
			r.e.Msg(keepaliveMsg(t))
			r.e.Msg(infoMsg(t))
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := startRig(t)
			c.run(t, r)
			if got := r.phone.endReasons(); !slices.Equal(got, []string{"error"}) {
				t.Fatalf("Phone.End calls %q, want one with reason error", got)
			}
		})
	}
}

func TestPhoneLinkFullEndsSession(t *testing.T) {
	t.Run("a car frame", func(t *testing.T) {
		r := startRig(t)
		r.phone.setFull(true)
		r.e.CarFrame(carlink.ChannelTCP, locationJSON(carLat, carLon, "P", 0))
		if got := r.phone.endReasons(); !slices.Equal(got, []string{"error"}) {
			t.Fatalf("Phone.End calls %q, want one with reason error", got)
		}
	})
	t.Run("a refusal", func(t *testing.T) {
		r := startRig(t)
		r.phone.setFull(true)
		r.e.Verb(verb10m(t, forwardType), staleAge, true, 1)
		if got := r.phone.endReasons(); !slices.Equal(got, []string{"error"}) {
			t.Fatalf("Phone.End calls %q, want one with reason error", got)
		}
	})
}

func TestPhoneEndCalledOutsideLock(t *testing.T) {
	cases := []struct {
		name  string
		setup func(t *testing.T, r *rig) func() // returns the call that ends the session
	}{
		{"car send error", func(t *testing.T, r *rig) func() {
			m := verb10m(t, forwardType)
			r.car.setSendErr(errBus)
			return func() { r.e.Verb(m, 0, true, 1) }
		}},
		{"trip send error", func(t *testing.T, r *rig) func() {
			r.e.Verb(verb10m(t, forwardType), 0, true, 1)
			r.car.setSendErr(errBus)
			r.clk.Advance(tripMS + 1)
			return r.e.Tick
		}},
		{"phone link full", func(t *testing.T, r *rig) func() {
			r.phone.setFull(true)
			return func() { r.e.CarFrame(carlink.ChannelUDP, carEcho) }
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := startRig(t)
			call := c.setup(t, r)
			r.phone.onEnd = func() { r.e.VerbActive() } // deadlocks if End runs under the engine lock
			within(t, 2*time.Second, "the call that ends the session", call)
			if got := r.phone.endReasons(); len(got) != 1 {
				t.Fatalf("Phone.End calls %q, want one", got)
			}
		})
	}
}

func TestLinkFlag(t *testing.T) {
	late := 110 * time.Millisecond
	t.Run("three late ages in a row set it once", func(t *testing.T) {
		r := startRig(t)
		r.e.Live(late, true)
		r.e.Live(late, true)
		if got := r.phone.linkCalls(); len(got) != 0 {
			t.Fatalf("SetLink %v after two late LIVEs", got)
		}
		r.e.Live(late, true)
		r.e.Live(late, true)
		if got := r.phone.linkCalls(); !slices.Equal(got, []bool{true}) {
			t.Fatalf("SetLink %v, want [true] once", got)
		}
	})
	t.Run("an age in between breaks the run", func(t *testing.T) {
		r := startRig(t)
		for _, age := range []time.Duration{late, 50 * time.Millisecond, late} {
			r.e.Live(age, true)
		}
		if got := r.phone.linkCalls(); len(got) != 0 {
			t.Fatalf("SetLink %v after 110, 50, 110", got)
		}
	})
	t.Run("100ms is not late", func(t *testing.T) {
		r := startRig(t)
		for range 3 {
			r.e.Live(100*time.Millisecond, true)
		}
		if got := r.phone.linkCalls(); len(got) != 0 {
			t.Fatalf("SetLink %v after three ages of 100 ms", got)
		}
	})
	t.Run("an unknown tick counts as late", func(t *testing.T) {
		r := startRig(t)
		r.e.Live(late, true)
		r.e.Live(0, false)
		r.e.Live(late, true)
		if got := r.phone.linkCalls(); !slices.Equal(got, []bool{true}) {
			t.Fatalf("SetLink %v, want [true]", got)
		}
	})
	t.Run("the phone's abort clears it", func(t *testing.T) {
		r := startRig(t)
		r.setLate(t)
		r.e.Msg(abortMsg(t))
		r.e.Msg(abortMsg(t)) // already clear: no call
		if got := r.phone.linkCalls(); !slices.Equal(got, []bool{true, false}) {
			t.Fatalf("SetLink %v, want [true false]", got)
		}
	})
	t.Run("a trip clears it", func(t *testing.T) {
		r := startRig(t)
		r.e.Verb(verb10m(t, forwardType), 0, true, 1)
		r.setLate(t) // late for the link, but fresh: liveness instants at the verb's own instant
		r.step(tripMS + 1)
		if r.aborts() != 1 {
			t.Fatal("no trip")
		}
		if got := r.phone.linkCalls(); !slices.Equal(got, []bool{true, false}) {
			t.Fatalf("SetLink %v, want [true false]", got)
		}
	})
	t.Run("a hold end restarts the count", func(t *testing.T) {
		r := startRig(t)
		r.e.Live(late, true)
		r.e.Live(late, true)
		r.e.Msg(abortMsg(t))
		r.e.Live(late, true)
		r.e.Live(late, true)
		if got := r.phone.linkCalls(); len(got) != 0 {
			t.Fatalf("SetLink %v: two late LIVEs before the release and two after are not three in a row of one hold", got)
		}
		r.e.Live(late, true)
		if got := r.phone.linkCalls(); !slices.Equal(got, []bool{true}) {
			t.Fatalf("SetLink %v, want [true]", got)
		}
	})
	t.Run("2 s of ages at or under 75 ms clear it", func(t *testing.T) {
		r := startRig(t)
		r.setLate(t)
		for range 40 { // the run starts at the first: 0 ... 1950 ms
			r.clk.Advance(50)
			r.e.Live(75*time.Millisecond, true)
		}
		if got := r.phone.linkCalls(); !slices.Equal(got, []bool{true}) {
			t.Fatalf("SetLink %v after 1950 ms of good ages", got)
		}
		r.clk.Advance(50)
		r.e.Live(10*time.Millisecond, true) // 2000 ms
		if got := r.phone.linkCalls(); !slices.Equal(got, []bool{true, false}) {
			t.Fatalf("SetLink %v after 2000 ms of good ages, want [true false]", got)
		}
	})
	t.Run("an age of 90 ms restarts the 2 s", func(t *testing.T) {
		r := startRig(t)
		r.setLate(t)
		for range 21 { // a run of 0 ... 1000 ms
			r.clk.Advance(50)
			r.e.Live(20*time.Millisecond, true)
		}
		r.clk.Advance(50)
		r.e.Live(90*time.Millisecond, true) // 1050 ms: the run breaks
		for range 40 {                      // a new run: 1100 ... 3050 ms
			r.clk.Advance(50)
			r.e.Live(20*time.Millisecond, true)
		}
		if got := r.phone.linkCalls(); !slices.Equal(got, []bool{true}) {
			t.Fatalf("SetLink %v: cleared within 2 s of the 90 ms age", got)
		}
		r.clk.Advance(50)
		r.e.Live(20*time.Millisecond, true) // 3100 ms: 2000 ms into the new run
		if got := r.phone.linkCalls(); !slices.Equal(got, []bool{true, false}) {
			t.Fatalf("SetLink %v, want [true false]", got)
		}
	})
	t.Run("Stop clears it", func(t *testing.T) {
		r := startRig(t)
		r.setLate(t)
		r.e.Stop("close")
		if got := r.phone.linkCalls(); !slices.Equal(got, []bool{true, false}) {
			t.Fatalf("SetLink %v, want [true false]", got)
		}
	})
}

func TestCarFrameChannels(t *testing.T) {
	r := startRig(t)
	result := resultJSON(forwardType, true, "", wallMS)
	r.e.CarFrame(carlink.ChannelTCP, result)
	r.e.CarFrame(carlink.ChannelUDP, carEcho)
	r.e.CarFrame("x", result)
	r.e.CarFrame("", carEcho)
	want := []phoneFrame{{0, string(result)}, {1, string(carEcho)}}
	if got := r.phone.got(); !slices.Equal(got, want) {
		t.Fatalf("phone got %v, want %v", got, want)
	}
	if got := r.phone.endReasons(); len(got) != 0 {
		t.Fatalf("an unknown channel ended the session: %q", got)
	}
	dropped := 0
	for _, l := range r.logLines() {
		if strings.Contains(l, "channel") {
			dropped++
		}
	}
	if dropped != 2 {
		t.Fatalf("%d log lines about a dropped channel, want 2: %q", dropped, r.logLines())
	}
}

func TestNonMotionMsgForwarded(t *testing.T) {
	r := startRig(t)
	r.e.Verb(verb10m(t, forwardType), 0, true, 1)
	r.step(tripMS + 1) // tripped: they still go out
	r.car.reset()
	msgs := []Message{deviceLocationMsg(t), infoMsg(t), keepaliveMsg(t)}
	for _, m := range msgs {
		r.e.Msg(m)
	}
	want := []string{string(msgs[0].Canonical), string(msgs[1].Canonical), string(msgs[2].Canonical)}
	if got := r.car.sends(); !slices.Equal(got, want) {
		t.Fatalf("car got %q, want %q", got, want)
	}
}

func TestNonMotionMsgIsNotLiveness(t *testing.T) {
	r := startRig(t)
	r.e.Verb(verb10m(t, findMeType), 0, true, 1)
	loc, ka := deviceLocationMsg(t), keepaliveMsg(t)
	for range tripMS / 50 { // every 50 ms up to TripAfter
		r.clk.Advance(50)
		r.e.Msg(loc)
		r.e.Msg(ka)
		r.e.Tick()
	}
	r.step(tripMS % 50)
	if n := r.aborts(); n != 0 {
		t.Fatalf("%d aborts by TripAfter", n)
	}
	r.step(1)
	if n := r.aborts(); n != 1 {
		t.Fatalf("%d aborts at TripAfter + 1 ms, want 1: a device_location or keepalive stamped liveness", n)
	}
	if r.e.VerbActive() {
		t.Fatal("verbActive after the trip")
	}
}

func TestWrongKindEndsSession(t *testing.T) {
	cases := []struct {
		name string
		run  func(t *testing.T, r *rig)
	}{
		{"a verb as MSG", func(t *testing.T, r *rig) { r.e.Msg(verb10m(t, forwardType)) }},
		{"a heartbeat as MSG", func(t *testing.T, r *rig) { r.e.Msg(heartbeatMsg(t)) }},
		{"a verb as HB", func(t *testing.T, r *rig) { r.e.Heartbeat(verb10m(t, findMeType), 0, true, 1) }},
		{"a heartbeat as VERB", func(t *testing.T, r *rig) { r.e.Verb(heartbeatMsg(t), 0, true, 1) }},
		{"a device_location as VERB", func(t *testing.T, r *rig) { r.e.Verb(deviceLocationMsg(t), 0, true, 1) }},
		{"an unknown type as MSG", func(t *testing.T, r *rig) {
			r.e.Msg(Message{Type: "autopark:cmd_pause", Canonical: []byte(`{"msg_type":"autopark:cmd_pause"}`)})
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := startRig(t)
			c.run(t, r)
			if got := r.car.sends(); len(got) != 0 {
				t.Fatalf("car got %q", got)
			}
			if r.e.VerbActive() {
				t.Fatal("verbActive")
			}
			if got := r.phone.endReasons(); !slices.Equal(got, []string{"error"}) {
				t.Fatalf("Phone.End calls %q, want one with reason error", got)
			}
		})
	}
}

func TestLogsCarryNoPositions(t *testing.T) {
	r := startRig(t)
	r.e.Verb(verb10m(t, findMeType), 0, true, 1)
	r.e.Verb(verb10m(t, forwardType), 0, true, 2)                                   // replaces the active one
	r.e.Heartbeat(heartbeatMsg(t), 0, true, 1)                                      // forwarded
	r.e.Msg(deviceLocationMsg(t))                                                   // forwarded
	r.e.CarFrame("x", locationJSON(carLat, carLon, "D", 1))                         // dropped and logged
	r.step(tripMS + 1)                                                              // the trip
	r.e.CarFrame(carlink.ChannelTCP, resultJSON(abortType, true, "", wallMS))       // swallowed
	r.e.Verb(verb10m(t, reverseType), staleAge, true, 3)                            // stale, no liveness: refused
	r.e.CarFrame(carlink.ChannelTCP, statusJSON("ready", "", "paused", "", "", "")) // logged
	r.e.CarFrame(carlink.ChannelTCP, locationJSON(carLat, carLon, "D", 1))          // forwarded
	r.e.Stop("close")
	lines := r.logLines()
	if len(lines) == 0 {
		t.Fatal("no log lines")
	}
	for _, l := range lines {
		for _, bad := range []string{"52.52", "13.40", "latitude", "longitude", "1700000000", "5YJ3"} {
			if strings.Contains(l, bad) {
				t.Fatalf("log line %q contains %q", l, bad)
			}
		}
	}
}

func TestConfigDefaults(t *testing.T) {
	// Now, Logf and Fatal may be nil: refusals use time.Now, logs are dropped, and a panic in Stop is raised again
	// rather than lost.
	log := &callLog{}
	car, phone := &fakeCar{log: log}, &fakePhone{log: log}
	e := NewEngine(Config{Car: car, Phone: phone, Marker: &fakeMarker{log: log}, Clock: clock.NewFake()})
	if err := e.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	before := time.Now().UnixMilli()
	e.Verb(verb10m(t, forwardType), staleAge, true, 1) // refused (no liveness yet), and logged
	after := time.Now().UnixMilli()
	got := phone.got()
	if len(got) != 1 {
		t.Fatalf("phone got %v, want one refusal", got)
	}
	ts, err := strconv.ParseInt(refusalTS(t, got[0]), 10, 64)
	if err != nil {
		t.Fatalf("refusal timestamp: %v", err)
	}
	if ts < before || ts > after {
		t.Fatalf("refusal timestamp %d outside the wall clock's [%d, %d]", ts, before, after)
	}
	e.CarFrame(carlink.ChannelTCP, statusJSON("ready", "", "paused", "", "", ""))
	car.onDisconnect = func() { panic("boom") }
	defer func() {
		if p := recover(); p == nil || !strings.Contains(fmt.Sprint(p), "boom") {
			t.Fatalf("Stop with a nil Fatal: recover() = %v, want the panic raised again", p)
		}
	}()
	e.Stop("close")
}

func TestConcurrentInputs(t *testing.T) {
	r := startRig(t)
	verbM, hbM, abortM, locM := verb10m(t, findMeType), heartbeatMsg(t), abortMsg(t), deviceLocationMsg(t)
	result := resultJSON(abortType, true, "", wallMS)
	stop := make(chan struct{})
	var wg sync.WaitGroup
	loop := func(f func()) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 3000 {
				select {
				case <-stop:
					return
				default:
					f()
				}
			}
		}()
	}
	loop(func() { r.e.Live(110*time.Millisecond, true) })
	loop(func() { r.e.Live(20*time.Millisecond, true) })
	loop(func() { r.e.Verb(verbM, 40*time.Millisecond, true, 1) })
	loop(func() { r.e.Heartbeat(hbM, 40*time.Millisecond, true, 1) })
	loop(func() { r.e.Msg(abortM) })
	loop(func() { r.e.Msg(locM) })
	loop(func() { r.e.CarFrame(carlink.ChannelTCP, result) })
	loop(func() {
		r.clk.Advance(1)
		r.e.Tick()
	})
	loop(func() { r.e.VerbActive() })
	time.Sleep(5 * time.Millisecond)
	r.e.Stop("close")
	close(stop)
	wg.Wait()
	// Nothing reached the car or the phone's CAR stream after H4's disconnect.
	calls := r.log.get()
	at := slices.Index(calls, "car.disconnect")
	if at < 0 {
		t.Fatal("no H4 disconnect")
	}
	for _, c := range calls[at+1:] {
		if c != "marker.clear" && c != "phone.link false" {
			t.Fatalf("after H4's disconnect: %q", c)
		}
	}
}

// tripLine is the rig's one D2 trip log line.
func (r *rig) tripLine(t *testing.T) string {
	t.Helper()
	var got []string
	for _, l := range r.logLines() {
		if strings.HasPrefix(l, "summon: D2 trip: ") {
			got = append(got, l)
		}
	}
	if len(got) != 1 {
		t.Fatalf("trip lines %q, want one", got)
	}
	return got[0]
}

func TestTripLogSaysWhy(t *testing.T) {
	const prefix = "summon: D2 trip: no liveness instant for 801 ms, cmd_abort injected; "
	t.Run("the phone's frames stopped arriving", func(t *testing.T) {
		r := startRig(t)
		r.e.Verb(verb10m(t, forwardType), 0, true, 1)
		r.clk.Advance(50)
		r.e.Live(20*time.Millisecond, true)
		r.e.Heartbeat(heartbeatMsg(t), 20*time.Millisecond, true, 1)
		r.step(tripMS + 1)
		want := prefix + "last LIVE arrived 801 ms ago (tick age 20 ms); in the last 800 ms 0 LIVE (0 stale) and 0 HB (0 stale)"
		if got := r.tripLine(t); got != want {
			t.Fatalf("trip line\n%q\nwant\n%q", got, want)
		}
	})
	t.Run("they arrived, stale", func(t *testing.T) {
		r := startRig(t)
		r.e.Live(staleAge, true) // before the hold, and outside the window at the trip
		r.clk.Advance(100)
		r.e.Verb(verb10m(t, forwardType), 0, true, 1)
		for ms := uint64(10); ms <= tripMS; ms += 10 {
			r.clk.Advance(10)
			if ms%50 == 0 {
				r.e.Live(staleAge, true)
			}
			if ms%100 == 0 {
				r.e.Heartbeat(heartbeatMsg(t), 0, false, 1)
			}
			r.e.Tick()
		}
		r.step(1)
		want := prefix + "last LIVE arrived 1 ms ago (tick age 501 ms); in the last 800 ms 16 LIVE (16 stale) and 8 HB (8 stale)"
		if got := r.tripLine(t); got != want {
			t.Fatalf("trip line\n%q\nwant\n%q", got, want)
		}
	})
	t.Run("an unknown tick", func(t *testing.T) {
		r := startRig(t)
		r.e.Verb(verb10m(t, forwardType), 0, true, 1)
		r.clk.Advance(100)
		r.e.Live(0, false)
		r.step(tripMS - 99) // 801 ms after the verb
		want := prefix + "last LIVE arrived 701 ms ago (unknown tick); in the last 800 ms 1 LIVE (1 stale) and 0 HB (0 stale)"
		if got := r.tripLine(t); got != want {
			t.Fatalf("trip line\n%q\nwant\n%q", got, want)
		}
	})
	t.Run("no LIVE in the session", func(t *testing.T) {
		r := startRig(t)
		r.e.Verb(verb10m(t, forwardType), 0, true, 1)
		r.clk.Advance(10)
		r.e.Heartbeat(heartbeatMsg(t), staleAge, true, 1)
		r.step(tripMS - 9)
		want := prefix + "no LIVE in the session; in the last 800 ms 0 LIVE (0 stale) and 1 HB (1 stale)"
		if got := r.tripLine(t); got != want {
			t.Fatalf("trip line\n%q\nwant\n%q", got, want)
		}
	})
	t.Run("a flood fills the ring: the counts are lower bounds", func(t *testing.T) {
		r := startRig(t)
		r.e.Verb(verb10m(t, forwardType), 0, true, 1)
		for range 200 {
			r.e.Live(staleAge, true)
		}
		r.step(tripMS + 1)
		want := prefix + "last LIVE arrived 801 ms ago (tick age 501 ms); in the last 800 ms 0 LIVE (0 stale) and 0 HB (0 stale)"
		if got := r.tripLine(t); got != want {
			t.Fatalf("trip line\n%q\nwant\n%q", got, want)
		}
		r.e.Verb(verb10m(t, forwardType), 0, true, 2)
		r.reset()
		r.clk.Advance(tripMS)
		for range 200 {
			r.e.Live(staleAge, true)
		}
		r.step(1)
		want = "summon: D2 trip: no liveness instant for 801 ms, cmd_abort injected; last LIVE arrived 1 ms ago (tick age 501 ms); in the last 800 ms at least 128 LIVE (128 stale) and 0 HB (0 stale)"
		if got := r.tripLine(t); got != want {
			t.Fatalf("trip line\n%q\nwant\n%q", got, want)
		}
	})
}

func TestHoldEndStats(t *testing.T) {
	r := startRig(t)
	hb := heartbeatMsg(t)
	r.e.Heartbeat(hb, 0, true, 1) // before the hold: not counted
	r.e.Live(0, true)
	r.e.Verb(verb10m(t, forwardType), 0, true, 1)
	base := r.clk.BootMS()
	at := func(ms uint64, f func()) { // ms after the verb
		r.clk.Advance(base + ms - r.clk.BootMS())
		f()
		r.e.Tick()
	}
	at(50, func() { r.e.Live(20*time.Millisecond, true) })
	at(100, func() { r.e.Heartbeat(hb, 20*time.Millisecond, true, 1) })
	at(150, func() { r.e.Live(staleAge, true) })
	at(200, func() { r.e.Heartbeat(hb, 20*time.Millisecond, true, 1) })
	at(300, func() { r.e.Heartbeat(hb, 0, false, 1) })
	at(400, func() { r.e.Live(0, false) })
	at(500, func() { r.e.Live(Fresh, true) })
	at(550, func() { r.e.Heartbeat(hb, staleAge, true, 1) })
	at(620, func() { r.e.Heartbeat(hb, Fresh, true, 1) })
	at(700, func() { r.e.Msg(abortMsg(t)) })
	r.e.Heartbeat(hb, 0, true, 1) // after the hold: not counted
	r.e.Live(0, true)
	want := "summon: hold ended: 0 re-sends (0 stale); " +
		"HB 5 arrived, 3 forwarded, 2 dropped (2 stale, 0 after a trip), longest gap between forwarded 420 ms; " +
		"LIVE 4 arrived, 2 fresh, longest gap between fresh 450 ms"
	var got []string
	for _, l := range r.logLines() {
		if strings.HasPrefix(l, "summon: hold ended") {
			got = append(got, l)
		}
	}
	if !slices.Equal(got, []string{want}) {
		t.Fatalf("hold-end lines\n%q\nwant\n%q", got, want)
	}
}
