// Package mockcar is a mock of the car's QtCarServer for the com.tesla.Webrtc contract, following Tesla's flow
// (Appendix A §1.2 and §8): on data_channels_ready it pushes style, status and a location frame; it echoes heartbeats,
// answers verbs and aborts with a cmd_result under the firmware's single-value CAPI latch, samples location on change,
// and after a disconnect calls RemoteToLocalSignaling type 9 after Type9After. It records everything it receives and
// every rule the helper broke (Violations). The Car is transport-agnostic: a Port carries its calls and delivers the
// helper's signals, either on a FakeBus (FakePort) or on a real bus (DBusPort).
package mockcar

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"math"
	"sync"
	"time"

	"airgapp/gateway/internal/carlink"
)

// Port is the car's side of the com.tesla.Webrtc transport.
type Port interface {
	CallCarserverToPeer(channel string, data []byte) error
	CallRemoteToLocal(body map[string]any) error
	attach(c *Car) // New calls it, so the port can deliver the helper's signals to the Car
}

// Config is the Car's configuration.
type Config struct {
	Port        Port
	Now         func() time.Time // the car's epoch clock (timestamps); default time.Now
	SampleEvery time.Duration    // default 250 ms
	Type9After  time.Duration    // default 60 s
	Logf        func(format string, args ...any)
}

// Received is one input the Car got: a signal member (LocalToRemoteSignaling or PeerCommsToCarserverData), the
// msg_type it carried ("" if none), its data (the phone's frame, or the signal body as JSON) and when.
type Received struct {
	Member, MsgType string
	Data            []byte
	At              time.Time
}

// The latch durations of the firmware's CAPI_remoteAutopark (§8.2) and the CAPI values (a verb: 1, 2 or 6; abort: 3).
const (
	verbLatch  = 20 * time.Second
	abortLatch = 5 * time.Second
	capiFwd    = 1
	capiRev    = 2
	capiAbort  = 3
	capiFindMe = 6
)

// allowed is the helper's allowed msg_type set (§1.2 H2).
var allowed = map[string]bool{
	"autopark:cmd_find_me": true, "autopark:cmd_forward": true, "autopark:cmd_reverse": true,
	"autopark:cmd_abort": true, "autopark:device_location": true, "autopark:info": true,
	"autopark:heartbeat_app_webrtc": true, "control:keepalive": true,
}

const abortJSON = `{"msg_type":"autopark:cmd_abort"}`

type pose struct {
	lat, lon, heading, speed float64
	blinker, shift           string
}

type out struct {
	channel string
	data    []byte
	body    map[string]any // set: a RemoteToLocalSignaling call
}

// Car is the mock QtCarServer.
type Car struct {
	cfg Config

	mu         sync.Mutex
	connected  bool
	accepted   bool      // a verb was accepted since the last connect
	type9At    time.Time // zero: no timer
	echo       bool
	refuse     []string
	capi       int
	capiUntil  time.Time
	cur, sent  pose
	lastSample time.Time
	freeze     int // frames left before the freeze; -1: none
	got        []Received
	viol       []string

	outq     []out
	draining bool
}

// New returns a Car attached to cfg.Port.
func New(cfg Config) *Car {
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.SampleEvery <= 0 {
		cfg.SampleEvery = 250 * time.Millisecond
	}
	if cfg.Type9After <= 0 {
		cfg.Type9After = 60 * time.Second
	}
	if cfg.Logf == nil {
		cfg.Logf = func(string, ...any) {}
	}
	c := &Car{cfg: cfg, echo: true, freeze: -1, cur: pose{blinker: "off", shift: "P"}}
	cfg.Port.attach(c)
	return c
}

func (c *Car) ms() int64 { return c.cfg.Now().UnixMilli() }

// violate records a broken rule. c.mu is held.
func (c *Car) violate(format string, a ...any) {
	v := fmt.Sprintf(format, a...)
	c.viol = append(c.viol, v)
	c.cfg.Logf("mockcar: violation: %s", v)
}

// send queues one frame on channel. c.mu is held. It counts toward FreezeAfter.
func (c *Car) send(channel string, m map[string]any) {
	if c.freeze == 0 {
		return
	}
	data, err := json.Marshal(m) // sorted keys
	if err != nil {
		c.cfg.Logf("mockcar: not sending an unencodable frame: %v", err)
		return
	}
	if c.freeze > 0 {
		c.freeze--
	}
	c.outq = append(c.outq, out{channel: channel, data: data})
}

// flush delivers the queued outputs through the Port in order, from one goroutine at a time: a call made while
// another goroutine is delivering is queued and delivered by that one. Called without c.mu.
func (c *Car) flush() {
	c.mu.Lock()
	if c.draining {
		c.mu.Unlock()
		return
	}
	c.draining = true
	for len(c.outq) > 0 {
		o := c.outq[0]
		c.outq = c.outq[1:]
		c.mu.Unlock()
		var err error
		if o.body != nil {
			err = c.cfg.Port.CallRemoteToLocal(o.body)
		} else {
			err = c.cfg.Port.CallCarserverToPeer(o.channel, o.data)
		}
		if err != nil {
			c.cfg.Logf("mockcar: call failed: %v", err)
		}
		c.mu.Lock()
	}
	c.draining = false
	c.mu.Unlock()
}

// Signal is the helper's signal reaching the car: member is LocalToRemoteSignaling or PeerCommsToCarserverData.
func (c *Car) Signal(member string, body map[string]any) {
	c.mu.Lock()
	switch member {
	case carlink.SignalLocalToRemoteSignaling:
		c.signaling(body)
	case carlink.SignalPeerCommsToCarserverData:
		c.phoneFrame(body)
	default:
		c.violate("unknown signal %q", member)
	}
	c.mu.Unlock()
	c.flush()
}

func (c *Car) record(member, msgType string, data []byte) {
	c.got = append(c.got, Received{Member: member, MsgType: msgType, Data: data, At: c.cfg.Now()})
}

// signaling handles LocalToRemoteSignaling. c.mu is held.
func (c *Car) signaling(body map[string]any) {
	mt, _ := body["msg_type"].(string)
	raw, _ := json.Marshal(body)
	c.record(carlink.SignalLocalToRemoteSignaling, mt, raw)
	if _, ok := body["data_channels_ready"]; ok {
		c.connect()
	}
	if _, ok := body["ice_state_change"]; ok {
		if v, _ := body["set_webrtc_value"].(bool); !v {
			c.disconnect()
		}
	}
}

// connect is webRTCConnectedImpl's false-to-true edge (H1). c.mu is held.
func (c *Car) connect() {
	if c.connected {
		c.violate("connect without a disconnect")
		return
	}
	c.connected = true
	c.accepted = false
	c.type9At = time.Time{} // H1 stops the car's 60 s timer
	c.send(carlink.ChannelTCP, map[string]any{"autopark_style": "dead_man", "msg_type": "autopark:style"})
	c.send(carlink.ChannelTCP, map[string]any{
		"autopark_state": "standby", "autopark_state_reason": "", "msg_type": "autopark:status",
		"smart_summon_last_abort_reason": "", "smart_summon_last_unavailable_reason": "",
		"smart_summon_leash_proximity_meters": 0, "smart_summon_leash_travel_meters": 0,
		"smart_summon_state": "initializing", "timestamp": c.ms(),
	})
	c.sendLocation()
	c.lastSample = c.cfg.Now()
}

// disconnect is webRTCConnected(false). c.mu is held.
func (c *Car) disconnect() {
	if !c.connected {
		c.violate("disconnect without a connect")
		return
	}
	c.connected = false
	if c.accepted { // RemoteSummon::disconnected writes CAPI=3 after an accepted verb (H4)
		c.capi, c.capiUntil = capiAbort, c.cfg.Now().Add(abortLatch)
		c.accepted = false
	}
	c.type9At = c.cfg.Now().Add(c.cfg.Type9After)
}

func (p pose) finite() bool {
	for _, v := range []float64{p.lat, p.lon, p.heading, p.speed} {
		if math.IsNaN(v) || math.IsInf(v, 0) {
			return false
		}
	}
	return true
}

// sendLocation sends the current pose unless a value is not finite (heading invalid: withheld, §8.6).
func (c *Car) sendLocation() {
	p := c.cur
	if !p.finite() {
		return
	}
	c.sent = p
	c.send(carlink.ChannelTCP, map[string]any{
		"blinker_state": p.blinker, "heading": p.heading, "latitude": p.lat, "longitude": p.lon,
		"msg_type": "vehicle_data:location", "shift_state": p.shift, "speed": p.speed, "timestamp": c.ms(),
	})
}

// phoneFrame handles PeerCommsToCarserverData. c.mu is held.
func (c *Car) phoneFrame(body map[string]any) {
	uid, _ := body["uid"].(string)
	data, _ := body["data"].(string)
	var m map[string]any
	d := json.NewDecoder(bytes.NewReader([]byte(data)))
	d.UseNumber()
	_ = d.Decode(&m)
	mt, _ := m["msg_type"].(string)
	c.record(carlink.SignalPeerCommsToCarserverData, mt, []byte(data))
	if uid != carlink.PhoneUID {
		c.violate("uid %q, want %q", uid, carlink.PhoneUID)
	}
	if !c.connected {
		c.violate("data while disconnected: %s", mt)
		return
	}
	if !allowed[mt] {
		c.violate("msg_type %q is outside the allowed set", mt)
		return
	}
	now := c.cfg.Now()
	switch mt {
	case "autopark:heartbeat_app_webrtc":
		if c.echo {
			c.send(carlink.ChannelUDP, map[string]any{
				"car_timestamp": c.ms(), "msg_type": "autopark:heartbeat_car_webrtc", "timestamp": m["timestamp"],
			})
		}
	case "autopark:cmd_abort":
		if data != abortJSON {
			c.violate("abort data %q is not exactly %s", data, abortJSON)
		}
		c.command(mt, capiAbort, abortLatch, now, false)
	case "autopark:cmd_forward":
		c.command(mt, capiFwd, verbLatch, now, true)
	case "autopark:cmd_reverse":
		c.command(mt, capiRev, verbLatch, now, true)
	case "autopark:cmd_find_me":
		c.command(mt, capiFindMe, verbLatch, now, true)
	}
}

// command answers a verb or abort under the single CAPI value (§8.2). c.mu is held.
func (c *Car) command(mt string, value int, latch time.Duration, now time.Time, verb bool) {
	reason, ok := "", true
	if verb && len(c.refuse) > 0 {
		reason, ok = c.refuse[0], false
		c.refuse = c.refuse[1:]
	} else {
		cur := c.capi
		if !now.Before(c.capiUntil) {
			cur = 0
		}
		if cur == value {
			reason, ok = "already_set", false
		} else {
			c.capi, c.capiUntil = value, now.Add(latch)
			c.accepted = c.accepted || verb
		}
	}
	c.send(carlink.ChannelTCP, map[string]any{
		"cmd_type": mt, "msg_type": "autopark:cmd_result", "reason": reason, "result": ok, "timestamp": c.ms(),
	})
}

// Step advances the car to now: it delivers what the Port has queued (FakePort), then runs location sampling and the
// type 9 timer. Deterministic for unit tests; Run calls the same logic in real time.
func (c *Car) Step(now time.Time) {
	if d, ok := c.cfg.Port.(interface{ drain() }); ok {
		d.drain()
	}
	c.tick(now)
}

func (c *Car) tick(now time.Time) {
	c.mu.Lock()
	if c.connected && !now.Before(c.lastSample.Add(c.cfg.SampleEvery)) {
		c.lastSample = now
		if c.cur != c.sent {
			c.sendLocation()
		}
	}
	if !c.connected && !c.type9At.IsZero() && !now.Before(c.type9At) {
		c.type9At = time.Time{}
		c.outq = append(c.outq, out{body: map[string]any{"type": int32(9), "uid": carlink.PhoneUID, "session_id": ""}})
	}
	c.mu.Unlock()
	c.flush()
}

// Run drives sampling and the type 9 timer in real time until ctx ends.
func (c *Car) Run(ctx context.Context) {
	period := min(c.cfg.SampleEvery, 10*time.Millisecond)
	t := time.NewTicker(period)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			c.tick(c.cfg.Now())
		}
	}
}

// SetPose sets what the next location sample reports. A pose with a NaN or infinite value is never sent.
func (c *Car) SetPose(lat, lon, heading, speed float64, blinker, shift string) {
	c.mu.Lock()
	c.cur = pose{lat, lon, heading, speed, blinker, shift}
	c.mu.Unlock()
}

// RefuseNext makes the next verb answer result false with reason.
func (c *Car) RefuseNext(reason string) {
	c.mu.Lock()
	c.refuse = append(c.refuse, reason)
	c.mu.Unlock()
}

// SetEcho turns the heartbeat echo on or off (default on).
func (c *Car) SetEcho(on bool) {
	c.mu.Lock()
	c.echo = on
	c.mu.Unlock()
}

// FreezeAfter makes the car send no frame after n more (the research freeze); n = 0 freezes it now.
func (c *Car) FreezeAfter(n int) {
	c.mu.Lock()
	c.freeze = n
	c.mu.Unlock()
}

// Received returns a copy of everything received, in order.
func (c *Car) Received() []Received {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]Received(nil), c.got...)
}

// Connected reports whether the car sees the phone session as connected.
func (c *Car) Connected() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.connected
}

// Violations returns a copy of the broken rules, in order.
func (c *Car) Violations() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]string(nil), c.viol...)
}

// WaitFor polls Received (real time) until pred holds or timeout passes.
func (c *Car) WaitFor(pred func([]Received) bool, timeout time.Duration) bool {
	end := time.Now().Add(timeout)
	for {
		if pred(c.Received()) {
			return true
		}
		if time.Now().After(end) {
			return false
		}
		time.Sleep(time.Millisecond)
	}
}
