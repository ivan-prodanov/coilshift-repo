package summon

import (
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"airgapp/gateway/internal/carlink"
)

// Owner ruling 2026-11-28: D3 (gate 8, the stale car position; gate 9, the 3 m floor) is gone, and the engine logs
// the car's own Summon status (autopark:status) as it changes.

// statusJSON is the car's autopark:status (QtCarServer's sorted keys), with the leash fields Tesla's frame carries.
func statusJSON(autopark, reason, smart, wait, abort, unavailable string) []byte {
	return fmt.Appendf(nil, `{"autopark_state":%q,"autopark_state_reason":%q,"msg_type":"autopark:status","smart_summon_last_abort_reason":%q,"smart_summon_last_unavailable_reason":%q,"smart_summon_last_wait_reason":%q,"smart_summon_leash_proximity_meters":0,"smart_summon_leash_travel_meters":0,"smart_summon_state":%q,"timestamp":1700000000000}`,
		autopark, reason, abort, unavailable, wait, smart)
}

// startRigNoLocation starts the engine without the car's first location: none has arrived yet.
func startRigNoLocation(t *testing.T) *rig {
	t.Helper()
	r := newRig(t)
	if err := r.e.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	r.reset()
	return r
}

// pressForwarded presses m with a fresh tick, then releases, and fails the test unless the verb went to the car with
// no answer from the helper.
func (r *rig) pressForwarded(t *testing.T, m Message) {
	t.Helper()
	r.car.reset()
	r.phone.reset()
	r.e.Verb(m, 20*time.Millisecond, true, 1)
	if got := r.car.sends(); !slices.Equal(got, []string{string(m.Canonical)}) {
		t.Fatalf("car got %q, want the verb %s forwarded", got, m.Type)
	}
	if got := r.phone.got(); len(got) != 0 {
		t.Fatalf("phone got %v for a forwarded verb", got)
	}
	r.e.Msg(abortMsg(t))
}

// goTo is Tesla's cmd_find_me in Go to Target: the phone at (lat, lon), the goal at (glat, glon).
func goTo(t *testing.T, lat, lon, glat, glon float64) Message {
	t.Helper()
	return parseMsg(t, KindVerb, fmt.Sprintf(
		`{"msg_type":"autopark:cmd_find_me","latitude":%v,"longitude":%v,"accuracy":5,"goal_latitude":%v,"goal_longitude":%v,"client_version":2,"summon_type":"pin_drop","created:timestamp":1700000000123}`,
		lat, lon, glat, glon))
}

func TestNoD3Refusals(t *testing.T) {
	verbs := func(t *testing.T) []Message {
		return []Message{
			verbAt(t, findMeType, carLat, carLon), // 0 m: Come to Me's goal is the phone
			verbAt(t, forwardType, carLat, carLon),
			verbAt(t, reverseType, carLat, carLon),
			goTo(t, tenMLat, carLon, carLat, carLon), // the goal at 0 m
			goTo(t, carLat, carLon, tenMLat, carLon), // the phone at 0 m
		}
	}
	states := []struct {
		name  string
		start func(t *testing.T) *rig
	}{
		{"the phone at the car's position", startRig},
		{"no car position", startRigNoLocation},
		{"an unreadable car position", func(t *testing.T) *rig {
			r := startRigNoLocation(t)
			r.e.CarFrame(carlink.ChannelTCP, []byte(`{"blinker_state":"off","heading":0,"latitude":null,"longitude":13.405,"msg_type":"vehicle_data:location","shift_state":"P","speed":0,"timestamp":1700000000000}`))
			return r
		}},
		{"a frozen car stream across long holds", func(t *testing.T) *rig {
			r := startRig(t)
			for range 3 { // well past the old StaleAfter, with no car frame
				r.e.Verb(verb10m(t, forwardType), 0, true, 1)
				for range 150 {
					r.clk.Advance(10)
					if r.clk.BootMS()%50 == 0 {
						r.e.Live(0, true)
					}
					r.e.Tick()
				}
				r.e.Msg(abortMsg(t))
			}
			return r
		}},
		{"moving in D with no location since", func(t *testing.T) *rig {
			r := startRig(t)
			r.e.CarFrame(carlink.ChannelTCP, locationJSON(carLat, carLon, "D", 1.5))
			r.e.Verb(verb10m(t, forwardType), 0, true, 1)
			for range 200 {
				r.clk.Advance(10)
				if r.clk.BootMS()%50 == 0 {
					r.e.Live(0, true)
				}
				r.e.Tick()
			}
			r.e.Msg(abortMsg(t))
			return r
		}},
	}
	for _, s := range states {
		t.Run(s.name, func(t *testing.T) {
			r := s.start(t)
			for _, m := range verbs(t) {
				r.pressForwarded(t, m)
			}
			for _, l := range r.logLines() {
				if strings.Contains(l, "refused") || strings.Contains(l, "D3") {
					t.Fatalf("log line %q", l)
				}
			}
		})
	}
}

func TestDeviceLocationForwarded(t *testing.T) {
	// device_location goes to the car as in Tesla's app, at any distance, with or without a car position.
	for _, start := range []func(t *testing.T) *rig{startRig, startRigNoLocation} {
		r := start(t)
		m := parseMsg(t, KindMsg, fmt.Sprintf(
			`{"msg_type":"autopark:device_location","latitude":%v,"longitude":%v,"accuracy":5,"altitude":-1,"altitude_accuracy":-1,"goal_latitude":%v,"goal_longitude":%v,"summon_type":"find_me","created:timestamp":1700000000300}`,
			carLat, carLon, carLat, carLon))
		r.e.Msg(m)
		if got := r.car.sends(); !slices.Equal(got, []string{string(m.Canonical)}) {
			t.Fatalf("car got %q, want the device_location", got)
		}
		if got := r.phone.got(); len(got) != 0 {
			t.Fatalf("phone got %v", got)
		}
	}
}

func TestRepressAfterCarRefusal(t *testing.T) {
	r := startRig(t)
	r.pressForwarded(t, verb10m(t, findMeType))
	r.e.CarFrame(carlink.ChannelTCP, resultJSON(findMeType, false, "plugged_in", wallMS+50))
	r.clk.Advance(2000) // the car stands still: no frame
	r.pressForwarded(t, verb10m(t, findMeType))
}

// statusLines is the rig's "car status" log lines.
func (r *rig) statusLines() []string {
	var out []string
	for _, l := range r.logLines() {
		if strings.HasPrefix(l, "summon: car status ") {
			out = append(out, l)
		}
	}
	return out
}

func TestCarStatusLogged(t *testing.T) {
	r := startRig(t)
	ready := statusJSON("ready", "", "paused", "", "", "")
	r.e.CarFrame(carlink.ChannelTCP, ready)
	r.e.CarFrame(carlink.ChannelTCP, ready) // a repeat: no line
	// The same Summon fields with another timestamp and leash: no line either.
	r.e.CarFrame(carlink.ChannelTCP, []byte(`{"autopark_state":"ready","autopark_state_reason":"","msg_type":"autopark:status","smart_summon_last_abort_reason":"","smart_summon_last_unavailable_reason":"","smart_summon_last_wait_reason":"","smart_summon_leash_proximity_meters":4,"smart_summon_leash_travel_meters":2,"smart_summon_state":"paused","timestamp":1700000009999}`))
	r.e.CarFrame(carlink.ChannelTCP, statusJSON("ready", "", "paused", "obstacle", "", ""))
	r.e.CarFrame(carlink.ChannelTCP, statusJSON("ready", "", "paused", "obstacle", "", ""))
	r.e.CarFrame(carlink.ChannelTCP, statusJSON("standby", "plugged_in", "unavailable", "obstacle", "user_aborted", "charging"))
	r.e.CarFrame(carlink.ChannelTCP, statusJSON("ready", "", "paused", "", "", "")) // back again: a change
	r.e.CarFrame("x", statusJSON("active", "", "driving", "", "", ""))              // a dropped frame: never logged
	r.e.CarFrame(carlink.ChannelUDP, carEcho)                                       // not a status
	want := []string{
		`summon: car status autopark="ready" reason="" smart="paused" wait="" abort="" unavailable=""`,
		`summon: car status autopark="ready" reason="" smart="paused" wait="obstacle" abort="" unavailable=""`,
		`summon: car status autopark="standby" reason="plugged_in" smart="unavailable" wait="obstacle" abort="user_aborted" unavailable="charging"`,
		`summon: car status autopark="ready" reason="" smart="paused" wait="" abort="" unavailable=""`,
	}
	if got := r.statusLines(); !slices.Equal(got, want) {
		t.Fatalf("status lines\n%q\nwant\n%q", got, want)
	}
	// Every status frame still reaches the phone as it came.
	if n := len(r.phone.got()); n != 8 {
		t.Fatalf("phone got %d frames, want the 7 statuses on dcTcp and the echo", n)
	}
}

func TestCarStatusLogShapes(t *testing.T) {
	t.Run("the first status of a session is logged, even with every field empty", func(t *testing.T) {
		r := startRig(t)
		r.e.CarFrame(carlink.ChannelTCP, []byte(`{"msg_type":"autopark:status"}`))
		r.e.CarFrame(carlink.ChannelTCP, []byte(`{"msg_type":"autopark:status","timestamp":5}`))
		want := []string{`summon: car status autopark="" reason="" smart="" wait="" abort="" unavailable=""`}
		if got := r.statusLines(); !slices.Equal(got, want) {
			t.Fatalf("status lines %q, want %q", got, want)
		}
	})
	t.Run("a non-string value is its JSON text; a long one is cut", func(t *testing.T) {
		r := startRig(t)
		long := strings.Repeat("w", 100)
		r.e.CarFrame(carlink.ChannelTCP, fmt.Appendf(nil,
			`{"autopark_state":3,"msg_type":"autopark:status","smart_summon_last_wait_reason":%q,"smart_summon_state":null}`, long))
		want := []string{fmt.Sprintf(`summon: car status autopark="3" reason="" smart="null" wait=%q abort="" unavailable=""`,
			strings.Repeat("w", 48))}
		if got := r.statusLines(); !slices.Equal(got, want) {
			t.Fatalf("status lines %q, want %q", got, want)
		}
	})
	t.Run("a status that is not a JSON object is not logged", func(t *testing.T) {
		r := startRig(t)
		r.e.CarFrame(carlink.ChannelTCP, []byte(`"autopark:status"`))
		if got := r.statusLines(); len(got) != 0 {
			t.Fatalf("status lines %q", got)
		}
	})
}
