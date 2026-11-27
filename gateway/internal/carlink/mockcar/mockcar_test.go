package mockcar

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/godbus/dbus/v5"

	"airgapp/gateway/internal/carlink"
	"airgapp/gateway/internal/carlink/bustest"
)

var t0 = time.UnixMilli(1_700_000_000_000)

type rig struct {
	t   *testing.T
	fb  *carlink.FakeBus
	car *Car
	now time.Time

	mu     sync.Mutex
	frames []carlink.CarFrame
	sig    []map[string]any
}

func newRig(t *testing.T, mut ...func(*Config)) *rig {
	t.Helper()
	r := &rig{t: t, fb: carlink.NewFakeBus(), now: t0}
	if err := r.fb.Serve(carlink.Handlers{
		CarFrame:  func(f carlink.CarFrame) { r.mu.Lock(); r.frames = append(r.frames, f); r.mu.Unlock() },
		Signaling: func(b map[string]any) { r.mu.Lock(); r.sig = append(r.sig, b); r.mu.Unlock() },
	}); err != nil {
		t.Fatal(err)
	}
	cfg := Config{Port: FakePort(r.fb), Now: func() time.Time { return r.now }, Type9After: 50 * time.Millisecond}
	for _, m := range mut {
		m(&cfg)
	}
	r.car = New(cfg)
	return r
}

// step advances the fake clock by d and steps the car there.
func (r *rig) step(d time.Duration) {
	r.now = r.now.Add(d)
	r.car.Step(r.now)
}

func (r *rig) emit(member string, body map[string]any) {
	r.t.Helper()
	if err := r.fb.Emit(member, body); err != nil {
		r.t.Fatal(err)
	}
}

func (r *rig) up() {
	r.emit("LocalToRemoteSignaling", map[string]any{"data_channels_ready": true})
	r.car.Step(r.now)
}

func (r *rig) down() {
	r.emit("LocalToRemoteSignaling", map[string]any{
		"ice_state_change": true, "msg_type": "autopark:set_webrtc", "set_webrtc_value": false})
	r.car.Step(r.now)
}

func (r *rig) phone(data string) {
	r.t.Helper()
	r.emit("PeerCommsToCarserverData", map[string]any{"uid": carlink.PhoneUID, "data": data})
	r.car.Step(r.now)
}

func (r *rig) take() []carlink.CarFrame {
	r.mu.Lock()
	defer r.mu.Unlock()
	f := r.frames
	r.frames = nil
	return f
}

func (r *rig) violations() []string { return r.car.Violations() }

func decode(t *testing.T, f carlink.CarFrame) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(f.Data, &m); err != nil {
		t.Fatalf("frame %q: %v", f.Data, err)
	}
	return m
}

const findMe = `{"msg_type":"autopark:cmd_find_me","latitude":1,"longitude":2,"summon_type":"pin_drop"}`

func TestConnectPushesStyleStatusLocation(t *testing.T) {
	r := newRig(t)
	r.up()
	fr := r.take()
	want := []string{
		`{"autopark_style":"dead_man","msg_type":"autopark:style"}`,
		fmt.Sprintf(`{"autopark_state":"standby","autopark_state_reason":"","msg_type":"autopark:status",`+
			`"smart_summon_last_abort_reason":"","smart_summon_last_unavailable_reason":"",`+
			`"smart_summon_leash_proximity_meters":0,"smart_summon_leash_travel_meters":0,`+
			`"smart_summon_state":"initializing","timestamp":%d}`, t0.UnixMilli()),
		fmt.Sprintf(`{"blinker_state":"off","heading":0,"latitude":0,"longitude":0,"msg_type":"vehicle_data:location",`+
			`"shift_state":"P","speed":0,"timestamp":%d}`, t0.UnixMilli()),
	}
	if len(fr) != 3 {
		t.Fatalf("%d frames, want 3: %v", len(fr), fr)
	}
	for i, f := range fr {
		if f.Channel != carlink.ChannelTCP || f.UID != carlink.PhoneUID || string(f.Data) != want[i] {
			t.Fatalf("frame %d = %s %s %s\nwant dcTcp %s", i, f.Channel, f.UID, f.Data, want[i])
		}
	}
	if !r.car.Connected() || len(r.violations()) != 0 {
		t.Fatalf("connected %v, violations %v", r.car.Connected(), r.violations())
	}
}

func TestHeartbeatEcho(t *testing.T) {
	r := newRig(t)
	r.up()
	r.take()
	r.now = r.now.Add(7 * time.Millisecond)
	r.phone(`{"msg_type":"autopark:heartbeat_app_webrtc","timestamp":123456}`)
	fr := r.take()
	want := fmt.Sprintf(`{"car_timestamp":%d,"msg_type":"autopark:heartbeat_car_webrtc","timestamp":123456}`, r.now.UnixMilli())
	if len(fr) != 1 || fr[0].Channel != carlink.ChannelUDP || string(fr[0].Data) != want {
		t.Fatalf("got %v, want one dcUdp %s", fr, want)
	}
	r.car.SetEcho(false)
	r.phone(`{"msg_type":"autopark:heartbeat_app_webrtc","timestamp":1}`)
	if fr := r.take(); len(fr) != 0 {
		t.Fatalf("echo off still answered: %v", fr)
	}
}

func TestVerbResultShape(t *testing.T) {
	r := newRig(t)
	r.up()
	r.take()
	r.phone(findMe)
	fr := r.take()
	want := fmt.Sprintf(`{"cmd_type":"autopark:cmd_find_me","msg_type":"autopark:cmd_result","reason":"","result":true,"timestamp":%d}`,
		t0.UnixMilli())
	if len(fr) != 1 || fr[0].Channel != carlink.ChannelTCP || string(fr[0].Data) != want {
		t.Fatalf("find_me: got %v, want %s", fr, want)
	}
	r.phone(`{"msg_type":"autopark:cmd_reverse","latitude":1,"longitude":2}`)
	if m := decode(t, r.take()[0]); m["cmd_type"] != "autopark:cmd_reverse" || m["result"] != true {
		t.Fatalf("reverse: %v", m)
	}
	r.car.RefuseNext("plugged_in")
	r.phone(`{"msg_type":"autopark:cmd_forward","latitude":1,"longitude":2}`)
	if m := decode(t, r.take()[0]); m["result"] != false || m["reason"] != "plugged_in" || m["cmd_type"] != "autopark:cmd_forward" {
		t.Fatalf("refusal: %v", m)
	}
	r.phone(`{"msg_type":"autopark:cmd_forward","latitude":1,"longitude":2}`)
	if m := decode(t, r.take()[0]); m["result"] != true {
		t.Fatalf("RefuseNext lasted more than one verb: %v", m)
	}
}

func TestAbortResultAndAlreadySet(t *testing.T) {
	r := newRig(t)
	r.up()
	r.take()
	r.phone(abortJSON)
	if m := decode(t, r.take()[0]); m["cmd_type"] != "autopark:cmd_abort" || m["result"] != true || m["reason"] != "" {
		t.Fatalf("first abort: %v", m)
	}
	r.step(4 * time.Second)
	r.take()
	r.phone(abortJSON)
	if m := decode(t, r.take()[0]); m["result"] != false || m["reason"] != "already_set" {
		t.Fatalf("second abort within 5 s: %v", m)
	}
	r.step(2 * time.Second) // 6 s after the first: the latch is gone
	r.take()
	r.phone(abortJSON)
	if m := decode(t, r.take()[0]); m["result"] != true {
		t.Fatalf("abort after the latch expired: %v", m)
	}
	// A repeated verb while latched is already_set.
	r.phone(findMe)
	r.take()
	r.phone(findMe)
	if m := decode(t, r.take()[0]); m["reason"] != "already_set" {
		t.Fatalf("repeated verb: %v", m)
	}
	if v := r.violations(); len(v) != 0 {
		t.Fatalf("violations %v", v)
	}
}

func TestReleaseThenRepressAccepted(t *testing.T) {
	r := newRig(t)
	r.up()
	r.take()
	r.phone(findMe)
	r.phone(abortJSON)
	r.phone(findMe)
	fr := r.take()
	if len(fr) != 3 {
		t.Fatalf("%d results, want 3", len(fr))
	}
	for i, f := range fr {
		if m := decode(t, f); m["result"] != true {
			t.Fatalf("result %d: %v", i, m)
		}
	}
}

func TestLocationOnChangeOnly(t *testing.T) {
	r := newRig(t)
	r.up()
	r.take()
	for i := 0; i < 8; i++ { // 2 s of no change
		r.step(250 * time.Millisecond)
	}
	if fr := r.take(); len(fr) != 0 {
		t.Fatalf("%d frames with no change: %v", len(fr), fr)
	}
	r.car.SetPose(0, 0, 0, 3, "off", "P") // speed only
	r.step(250 * time.Millisecond)
	fr := r.take()
	if len(fr) != 1 {
		t.Fatalf("%d frames after a speed change, want 1", len(fr))
	}
	if m := decode(t, fr[0]); m["msg_type"] != "vehicle_data:location" || m["speed"] != 3.0 || m["latitude"] != 0.0 {
		t.Fatalf("frame %v", m)
	}
	r.step(250 * time.Millisecond)
	if fr := r.take(); len(fr) != 0 {
		t.Fatalf("a repeat of the same pose sent %v", fr)
	}
	r.car.SetPose(0, 0, 0, 3, "left", "P") // blinker only
	r.step(250 * time.Millisecond)
	fr = r.take()
	if len(fr) != 1 || decode(t, fr[0])["blinker_state"] != "left" {
		t.Fatalf("blinker change: %v", fr)
	}
	r.car.SetPose(0, 0, 0, 3, "left", "D") // gear only
	r.step(250 * time.Millisecond)
	fr = r.take()
	if len(fr) != 1 || decode(t, fr[0])["shift_state"] != "D" {
		t.Fatalf("gear change: %v", fr)
	}
	// The status is never re-sent during motion.
	for _, f := range fr {
		if decode(t, f)["msg_type"] == "autopark:status" {
			t.Fatal("status re-sent")
		}
	}
}

func (r *rig) signals() []map[string]any {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]map[string]any(nil), r.sig...)
}

func TestDisconnectThenType9(t *testing.T) {
	r := newRig(t)
	r.up()
	r.down()
	if r.car.Connected() {
		t.Fatal("still connected")
	}
	r.take()
	r.step(49 * time.Millisecond)
	if n := len(r.signals()); n != 0 {
		t.Fatal("type 9 early")
	}
	r.step(time.Millisecond)
	want := []map[string]any{{"type": int32(9), "uid": "test_phone", "session_id": ""}}
	if got := r.signals(); !reflect.DeepEqual(got, want) {
		t.Fatalf("signaling %#v, want %#v", got, want)
	}
	r.step(time.Second) // fires once
	if n := len(r.signals()); n != 1 {
		t.Fatalf("type 9 fired %d times", n)
	}
}

func TestType9CancelledByReconnect(t *testing.T) {
	r := newRig(t) // Type9After 50 ms
	r.up()
	r.down() // first disconnect at +0
	r.step(20 * time.Millisecond)
	r.up()
	r.step(10 * time.Millisecond)
	r.down()                      // second disconnect at +30 ms: its type 9 is due at +80 ms
	r.step(20 * time.Millisecond) // +50 ms: the first deadline
	r.step(20 * time.Millisecond) // +70 ms
	if got := r.signals(); len(got) != 0 {
		t.Fatalf("type 9 fired at or after the first disconnect's deadline: %v", got)
	}
	r.step(9 * time.Millisecond) // +79 ms
	if got := r.signals(); len(got) != 0 {
		t.Fatalf("type 9 fired before the second deadline: %v", got)
	}
	r.step(time.Millisecond) // +80 ms
	if got := r.signals(); len(got) != 1 {
		t.Fatalf("%d type 9 at the second deadline, want 1", len(got))
	}
	r.step(time.Second)
	if got := r.signals(); len(got) != 1 {
		t.Fatalf("type 9 fired %d times", len(got))
	}
	// A reconnect alone, with no second disconnect, fires nothing.
	r2 := newRig(t)
	r2.up()
	r2.down()
	r2.step(20 * time.Millisecond)
	r2.up()
	r2.step(time.Second)
	if got := r2.signals(); len(got) != 0 {
		t.Fatalf("type 9 fired after a reconnect: %v", got)
	}
	if v := r.car.Violations(); len(v) != 0 {
		t.Fatalf("violations %v", v)
	}
}

func TestLatchBoundaries(t *testing.T) {
	result := func(r *rig, data string) map[string]any {
		r.phone(data)
		return decode(t, r.take()[0])
	}
	r := newRig(t)
	r.up()
	r.take()
	result(r, findMe)
	r.step(20*time.Second - time.Millisecond)
	r.take()
	if m := result(r, findMe); m["reason"] != "already_set" {
		t.Fatalf("verb at +19.999 s: %v", m)
	}
	r.step(time.Millisecond)
	r.take()
	if m := result(r, findMe); m["result"] != true {
		t.Fatalf("verb at +20 s: %v", m)
	}
	// The abort latch: 5 s.
	r = newRig(t)
	r.up()
	r.take()
	result(r, abortJSON)
	r.step(5*time.Second - time.Millisecond)
	r.take()
	if m := result(r, abortJSON); m["reason"] != "already_set" {
		t.Fatalf("abort at +4.999 s: %v", m)
	}
	r.step(time.Millisecond)
	r.take()
	if m := result(r, abortJSON); m["result"] != true {
		t.Fatalf("abort at +5 s: %v", m)
	}
}

func TestSamplingCadence(t *testing.T) {
	r := newRig(t)
	r.up()
	r.take()
	r.car.SetPose(0, 0, 0, 5, "off", "P")
	r.step(100 * time.Millisecond)
	if fr := r.take(); len(fr) != 0 {
		t.Fatalf("a frame %v at +100 ms, before a sample is due", fr)
	}
	r.step(150 * time.Millisecond) // +250 ms
	if fr := r.take(); len(fr) != 1 {
		t.Fatalf("%d frames at +SampleEvery, want 1", len(fr))
	}
}

func TestDuplicateConnectRepushesNothing(t *testing.T) {
	r := newRig(t)
	r.up()
	r.take()
	r.up()
	if fr := r.take(); len(fr) != 0 {
		t.Fatalf("a duplicate data_channels_ready pushed %v", fr)
	}
	if v := r.violations(); len(v) != 1 {
		t.Fatalf("violations %v", v)
	}
}

func TestDisconnectAfterAcceptedVerbSetsAbort(t *testing.T) {
	r := newRig(t)
	r.up()
	r.take()
	r.phone(findMe) // accepted: CAPI 6
	r.down()
	r.step(time.Millisecond)
	r.up()
	r.take()
	r.phone(findMe) // CAPI is 3 (abort) now, not 6
	if m := decode(t, r.take()[0]); m["result"] != true {
		t.Fatalf("the same verb after a reconnect: %v", m)
	}
	// With no verb accepted, a disconnect leaves CAPI alone: the abort is not latched.
	r = newRig(t)
	r.up()
	r.down()
	r.up()
	r.take()
	r.phone(abortJSON)
	if m := decode(t, r.take()[0]); m["result"] != true {
		t.Fatalf("abort after a verbless disconnect: %v", m)
	}
}

func TestNonFinitePoseNeverSent(t *testing.T) {
	for _, bad := range []float64{math.NaN(), math.Inf(1), math.Inf(-1)} {
		for field := 0; field < 4; field++ {
			v := [4]float64{}
			v[field] = bad
			r := newRig(t)
			r.car.SetPose(v[0], v[1], v[2], v[3], "off", "P")
			r.car.FreezeAfter(3)
			r.up()
			fr := r.take()
			if len(fr) != 2 || decode(t, fr[0])["msg_type"] != "autopark:style" || decode(t, fr[1])["msg_type"] != "autopark:status" {
				t.Fatalf("pose %v: push %v, want style and status only", v, fr)
			}
			r.step(250 * time.Millisecond)
			if fr := r.take(); len(fr) != 0 {
				t.Fatalf("pose %v: sampling sent %v", v, fr)
			}
			// No freeze count was spent on the withheld frames: exactly one frame is still allowed.
			r.phone(`{"msg_type":"autopark:heartbeat_app_webrtc","timestamp":1}`)
			if fr := r.take(); len(fr) != 1 {
				t.Fatalf("pose %v: %d frames left in the freeze budget, want 1", v, len(fr))
			}
			r.car.FreezeAfter(-1)
			// Back to a valid pose: the frame goes out, and the two frames the freeze allowed were not spent on a location.
			r.car.SetPose(1, 1, 1, 1, "off", "P")
			r.step(250 * time.Millisecond)
			if fr := r.take(); len(fr) != 1 {
				t.Fatalf("pose %v: recovery sent %d frames", v, len(fr))
			}
		}
	}
}

func TestViolations(t *testing.T) {
	cases := []struct {
		name string
		do   func(r *rig)
		want string
	}{
		{"msg_type outside the set", func(r *rig) { r.up(); r.phone(`{"msg_type":"autopark:cmd_pause"}`) }, "outside the allowed set"},
		{"abort not exact", func(r *rig) { r.up(); r.phone(`{"msg_type":"autopark:cmd_abort","x":1}`) }, "not exactly"},
		{"wrong uid", func(r *rig) {
			r.up()
			r.emit("PeerCommsToCarserverData", map[string]any{"uid": "someone", "data": abortJSON})
			r.car.Step(r.now)
		}, "uid"},
		{"data while disconnected", func(r *rig) { r.phone(abortJSON) }, "while disconnected"},
		{"data after disconnect", func(r *rig) { r.up(); r.down(); r.phone(abortJSON) }, "while disconnected"},
		{"second connect", func(r *rig) { r.up(); r.up() }, "connect without a disconnect"},
		{"disconnect without connect", func(r *rig) { r.down() }, "disconnect without a connect"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := newRig(t)
			c.do(r)
			v := r.violations()
			if len(v) != 1 || !strings.Contains(v[0], c.want) {
				t.Fatalf("violations %v, want one containing %q", v, c.want)
			}
		})
	}
	t.Run("clean session", func(t *testing.T) {
		r := newRig(t)
		r.up()
		r.phone(abortJSON)
		r.phone(`{"msg_type":"control:keepalive"}`)
		r.phone(`{"msg_type":"autopark:device_location","latitude":1}`)
		r.phone(`{"msg_type":"autopark:info"}`)
		r.down()
		if v := r.violations(); len(v) != 0 {
			t.Fatalf("violations %v", v)
		}
		// device_location, info and keepalive are recorded and get no reply.
		got := r.car.Received()
		if len(got) != 6 || got[2].MsgType != "control:keepalive" || got[3].MsgType != "autopark:device_location" {
			t.Fatalf("received %v", got)
		}
		if fr := r.take(); len(fr) != 4 { // style, status, location, abort result
			t.Fatalf("%d frames, want 4", len(fr))
		}
	})
}

func TestFreezeAfter(t *testing.T) {
	r := newRig(t)
	r.up()
	r.take()
	r.car.FreezeAfter(2)
	for i := 0; i < 4; i++ {
		r.phone(`{"msg_type":"autopark:heartbeat_app_webrtc","timestamp":1}`)
	}
	if fr := r.take(); len(fr) != 2 {
		t.Fatalf("%d frames after FreezeAfter(2), want 2", len(fr))
	}
	r.car.SetPose(1, 1, 1, 1, "off", "D")
	r.step(250 * time.Millisecond)
	if fr := r.take(); len(fr) != 0 {
		t.Fatalf("frozen car sent %v", fr)
	}
}

func TestRunSamplesInRealTime(t *testing.T) {
	r := newRig(t, func(c *Config) { c.Now = time.Now; c.SampleEvery = 20 * time.Millisecond })
	ctx, cancel := contextWithCancel(t)
	done := make(chan struct{})
	go func() { r.car.Run(ctx); close(done) }()
	r.emit("LocalToRemoteSignaling", map[string]any{"data_channels_ready": true})
	waitFrames := func(n int) bool {
		end := time.Now().Add(3 * time.Second)
		for time.Now().Before(end) {
			r.mu.Lock()
			k := len(r.frames)
			r.mu.Unlock()
			if k >= n {
				return true
			}
			time.Sleep(time.Millisecond)
		}
		return false
	}
	if !waitFrames(3) {
		t.Fatal("no connect frames")
	}
	r.car.SetPose(1, 2, 3, 4, "off", "D")
	if !waitFrames(4) { // the changed pose
		t.Fatal("no sampled frame")
	}
	cancel()
	<-done
}

// bus tests

func TestDBusPortIgnoresNonOwner(t *testing.T) {
	bus := bustest.Start(t)
	owner := bus.Conn(t)
	if rep, err := owner.RequestName(carlink.WebrtcName, dbus.NameFlagDoNotQueue); err != nil || rep != dbus.RequestNameReplyPrimaryOwner {
		t.Fatalf("RequestName: %v %v", rep, err)
	}
	car := New(Config{Port: DBusPort(bus.Conn(t))})
	forger := bus.Conn(t)
	emit := func(c *dbus.Conn, v string) {
		t.Helper()
		err := c.Emit(carlink.WebrtcPath, carlink.WebrtcIface+"."+carlink.SignalLocalToRemoteSignaling,
			map[string]dbus.Variant{"data_channels_ready": dbus.MakeVariant(true), "msg_type": dbus.MakeVariant(v)})
		if err != nil {
			t.Fatal(err)
		}
	}
	emit(forger, "forged")
	emit(owner, "real")
	if !car.WaitFor(func(r []Received) bool { return len(r) > 0 }, 5*time.Second) {
		t.Fatal("the owner's signal never arrived")
	}
	got := car.Received()
	if len(got) != 1 || got[0].MsgType != "real" {
		t.Fatalf("received %v, want only the owner's signal", got)
	}
}

func TestDBusPortRoundTrip(t *testing.T) {
	bus := bustest.Start(t)
	hb, err := carlink.DialBus(bus.Addr())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { hb.Close() })
	var mu sync.Mutex
	var frames []carlink.CarFrame
	if err := hb.Serve(carlink.Handlers{CarFrame: func(f carlink.CarFrame) {
		mu.Lock()
		frames = append(frames, f)
		mu.Unlock()
	}}); err != nil {
		t.Fatal(err)
	}
	link := carlink.NewLink(hb)
	if err := link.Acquire(); err != nil {
		t.Fatal(err)
	}
	car := New(Config{Port: DBusPort(bus.Conn(t))})
	if err := link.Connect(); err != nil {
		t.Fatal(err)
	}
	if !car.WaitFor(func(r []Received) bool { return len(r) >= 1 }, 5*time.Second) {
		t.Fatal("no connect")
	}
	end := time.Now().Add(5 * time.Second)
	for {
		mu.Lock()
		n := len(frames)
		mu.Unlock()
		if n >= 3 || time.Now().After(end) {
			break
		}
		time.Sleep(time.Millisecond)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(frames) != 3 {
		t.Fatalf("%d frames, want 3", len(frames))
	}
	for i, mt := range []string{"autopark:style", "autopark:status", "vehicle_data:location"} {
		var m map[string]any
		if err := json.Unmarshal(frames[i].Data, &m); err != nil || m["msg_type"] != mt || frames[i].Channel != carlink.ChannelTCP {
			t.Fatalf("frame %d = %s on %s, want %s on dcTcp", i, frames[i].Data, frames[i].Channel, mt)
		}
	}
}

func contextWithCancel(t *testing.T) (context.Context, context.CancelFunc) {
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	return ctx, cancel
}
